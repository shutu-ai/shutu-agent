// Package loop implements the agent loop (design.md §4): a turn is 0..N
// steps, each step being one model request plus the tool calls it initiates.
// The loop is strictly serial and synchronous (D5) and only appends to the
// session log (D1/D3). No product feature may change this structure.
package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// maxSteps bounds the number of tool-call steps in one turn, so a misbehaving
// model cannot loop forever.
const maxSteps = 10

// maxInjectorChars bounds the total context text a single pre-step injector may
// contribute to the first request of a turn (ADR 2026-08-18-m5-agent-core.md
// 总体决策: pre_step.max_chars_per_injector, default 4000). Over-budget context
// is truncated UTF-8-safely (fail-open: it can never block the answer).
const maxInjectorChars = 4000

// PreStepInjector is one registered pre-step context injector (ADR 2026-08-18
// -m5-agent-core.md 总体决策: the unified pre-step injection extension point that
// supersedes the single M4b Recall hook). Inject is called once per turn —
// after user/message is appended, before the first step's model request — and
// returns extra context messages injected into that first request only.
// tool-call follow-up steps never re-carry the injected context.
type PreStepInjector struct {
	Name   string // informational (logging/config); not a registration key
	Inject func(ctx context.Context, userText string) []llm.Message
}

// Loop drives one conversation turn against the session log.
type Loop struct {
	llm       llm.LLM
	log       *session.Log
	tools     *tools.Registry
	toolSpecs func() []llm.ToolSchema // per-session model-facing tool surface (dsh presentation mode)
	prompt    *prompt.Builder
	model     string
	effort    string
	recall    func(context.Context, string) []llm.Message // M4b hook, kept as the first injector
	preStep   []PreStepInjector                           // additional injectors, in registration order
	onText    func(string)                                // optional sink for streamed assistant text (REPL)
	onError   func(error)                                 // optional sink for stream errors (REPL)
}

// Config wires the loop's dependencies. All fields are required except the
// optional hooks.
type Config struct {
	LLM    llm.LLM
	Log    *session.Log
	Tools  *tools.Registry
	Prompt *prompt.Builder
	Model  string
	// ToolSpecs, when set, is the session's model-facing tool surface (dsh
	// presentation mode: standard = native tools minus run_code, PTC = only
	// run_code, minimal = the fixed seam). It is called on every step; when
	// nil the loop sends every registered tool schema.
	ToolSpecs func() []llm.ToolSchema
	// ReasoningEffort is the thinking-effort default applied to every model
	// request of this loop (dsh 思考强度; "" keeps the provider default).
	ReasoningEffort string
	// Recall, if set, is the proactive knowledge recall extension point
	// (design.md §8, D4: new features hang on extension points). It is the
	// first pre-step injector ("recall", ADR 2026-08-18-m5-agent-core.md 总体
	// 决策): called once at the start of each turn — after user/message is
	// appended, before the first step's model request — and returns extra
	// context messages injected into that first request only. The recall
	// orchestration (query, KB.Recall, fail-open, kb/recall logging) lives
	// entirely in cmd/pa; the loop just injects what it returns. Kept for
	// backward compatibility with M4b; when both Recall and PreStep are set,
	// Recall runs first and PreStep follows. The turn/step structure is
	// unchanged.
	Recall func(ctx context.Context, userText string) []llm.Message
	// PreStep registers additional pre-step context injectors beyond Recall
	// (ADR 2026-08-18-m5-agent-core.md 总体决策). Each injector runs once per
	// turn, in registration order after Recall, with its returned context
	// bounded to maxInjectorChars; a panicking injector is skipped (fail-open).
	PreStep []PreStepInjector
	// OnText, if set, is called with each streamed assistant text delta.
	OnText func(string)
	// OnError, if set, is called when a step's stream fails after start.
	OnError func(error)
}

// New returns a Loop.
func New(cfg Config) *Loop {
	return &Loop{
		llm:       cfg.LLM,
		log:       cfg.Log,
		tools:     cfg.Tools,
		toolSpecs: cfg.ToolSpecs,
		prompt:    cfg.Prompt,
		model:     cfg.Model,
		effort:    cfg.ReasoningEffort,
		recall:    cfg.Recall,
		preStep:   append([]PreStepInjector(nil), cfg.PreStep...),
		onText:    cfg.OnText,
		onError:   cfg.OnError,
	}
}

// Run executes one turn for the given user input. It appends user/message,
// then runs steps until the model stops requesting tools or maxSteps is hit.
// The supplied context cancels the current step (design.md §4).
func (l *Loop) Run(ctx context.Context, userText string) error {
	if _, err := l.log.Append(session.EventUserMessage, session.NewUserMessage(userText)); err != nil {
		return err
	}
	// The pre-step context is collected once per turn and applied to the first
	// request only (the step === 1 gate below is unchanged): Recall (M4b) runs
	// first as the "recall" injector, then the registered PreStep injectors in
	// order. Each injector's contribution is bounded to maxInjectorChars and a
	// panicking injector is skipped (fail-open), so a misbehaving injector can
	// never block the answer or blow up the first request.
	var contextMsgs []llm.Message
	for _, inj := range l.effectiveInjectors() {
		contextMsgs = append(contextMsgs, l.safeInject(inj, ctx, userText)...)
	}
	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("loop: cancelled: %w", err)
		}
		done, err := l.step(ctx, contextMsgs)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		contextMsgs = nil // only the turn's first request carries the pre-step context
	}
	return fmt.Errorf("loop: exceeded %d steps per turn", maxSteps)
}

// effectiveInjectors returns the ordered pre-step injector list for one turn:
// the M4b Recall hook first (as "recall", kept for backward compatibility),
// then the registered PreStep injectors in order. Building it per turn lets the
// composition root swap the Recall hook between turns. A nil Recall hook
// contributes no injector.
func (l *Loop) effectiveInjectors() []PreStepInjector {
	var out []PreStepInjector
	if l.recall != nil {
		out = append(out, PreStepInjector{Name: "recall", Inject: l.recall})
	}
	out = append(out, l.preStep...)
	return out
}

// safeInject calls one injector and bounds its contribution, containing a
// panic so a throwing injector is skipped (fail-open) instead of aborting the
// turn.
func (l *Loop) safeInject(inj PreStepInjector, ctx context.Context, userText string) (msgs []llm.Message) {
	if inj.Inject == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			msgs = nil
		}
	}()
	return truncateInjectorContext(inj.Inject(ctx, userText))
}

// truncateInjectorContext bounds the total context text one injector may
// contribute to maxInjectorChars runes (config pre_step.max_chars_per_injector,
// default 4000): messages are kept in registration order until the budget is
// exhausted, the message that overflows is truncated UTF-8-safely, and the rest
// are dropped. Over-budget context never blocks the answer (fail-open). The
// injector messages are plain text this milestone, so Text()/SetText() give the
// exact old string semantics.
func truncateInjectorContext(msgs []llm.Message) []llm.Message {
	budget := maxInjectorChars
	keep := len(msgs)
	for i := range msgs {
		n := utf8.RuneCountInString(msgs[i].Text())
		if n > budget {
			if budget <= 0 {
				keep = i
				break
			}
			msgs[i].SetText(truncateRunes(msgs[i].Text(), budget))
			keep = i + 1
			break
		}
		budget -= n
	}
	return msgs[:keep]
}

// truncateRunes shortens s to at most max runes, never splitting a UTF-8
// sequence (mirrors internal/jobs/local.go's truncateUTF8, but counts runes).
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) == 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// step performs one model request and its tool executions. It returns
// (true, nil) when the turn is complete (no tool calls requested). contextMsgs
// are prepended to the request (after the system prompt, before the derived
// history).
func (l *Loop) step(ctx context.Context, contextMsgs []llm.Message) (bool, error) {
	history := l.log.DeriveHistory()
	specs := l.tools.Specs()
	if l.toolSpecs != nil {
		// The session's presentation mode owns the model-facing surface: the
		// wire tools array must match the mode exactly, never the full
		// registry (dsh assembly: native | code | both).
		specs = l.toolSpecs()
	}
	messages := make([]llm.Message, 0, len(history)+1+len(contextMsgs))
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(l.prompt.Build())}})
	messages = append(messages, contextMsgs...)
	messages = append(messages, history...)

	reader, err := l.llm.Stream(ctx, llm.ChatRequest{Model: l.model, ReasoningEffort: l.effort, Messages: messages, Tools: specs})
	if err != nil {
		return false, err
	}

	var text strings.Builder
	var reasoning string
	var calls []llm.ToolCall
	var finishReason string
	for {
		ev, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if l.onError != nil {
				l.onError(err)
			}
			return false, err
		}
		switch ev.Kind {
		case llm.StreamTextDelta:
			text.WriteString(ev.Text)
			if l.onText != nil {
				l.onText(ev.Text)
			}
			if _, err := l.log.Append(session.EventAssistantChunk, session.NewAssistantChunk(ev.Text)); err != nil {
				return false, err
			}
		case llm.StreamReasoningDelta:
			// M8: reasoning deltas are logged as they arrive (dsh order:
			// thinking before tool calls) so the UI can render the chain in
			// place; DeriveHistory folds these rows away in favor of the
			// joined reasoning on the closing assistant/message.
			reasoning += ev.Text
			if _, err := l.log.Append(session.EventAssistantReasoning, session.NewAssistantReasoning(ev.Text)); err != nil {
				return false, err
			}
		case llm.StreamFinish:
			calls = ev.ToolCalls
			finishReason = ev.FinishReason
			reasoning = ev.Reasoning // accumulated by the reader (M8)
		}
	}

	if _, err := l.log.Append(session.EventAssistantMessage, session.NewAssistantMessage(text.String(), calls, finishReason, reasoning)); err != nil {
		return false, err
	}
	if len(calls) == 0 {
		return true, nil
	}
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("loop: cancelled: %w", err)
		}
		// tool/start: the call is dispatched — the UI shows the running row
		// (dsh) while the tool executes; DeriveHistory folds this row away.
		if _, aerr := l.log.Append(session.EventToolStart, session.NewToolStart(call.ID, call.Name, call.Arguments)); aerr != nil {
			return false, aerr
		}
		res, err := l.tools.Execute(ctx, call.Name, []byte(call.Arguments))
		if err != nil {
			if _, aerr := l.log.Append(session.EventToolError, session.NewToolError(call.ID, call.Name, err.Error())); aerr != nil {
				return false, aerr
			}
		} else {
			var spill *session.SpillRef
			if res.SpillPath != "" {
				spill = &session.SpillRef{Locator: res.SpillPath, Bytes: res.SpillBytes}
			}
			if _, aerr := l.log.Append(session.EventToolResult, session.NewToolResult(call.ID, call.Name, res.Output, spill)); aerr != nil {
				return false, aerr
			}
		}
	}
	return false, nil
}
