// Package loop implements the agent loop (design.md §4): a turn is 0..N
// steps, each step being one model request plus the tool calls it initiates.
// The loop is strictly serial and synchronous (D5) and only appends to the
// session log (D1/D3). No product feature may change this structure.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
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
// contribute to one step (ADR 2026-08-18-m5-agent-core.md
// 总体决策: pre_step.max_chars_per_injector, default 4000). Over-budget context
// is truncated UTF-8-safely (fail-open: it can never block the answer).
const maxInjectorChars = 4000

// Streamed deltas are durable fidelity records, but persisting every provider
// token makes a dense response spend most of its time crossing the store and
// WebSocket boundaries. Aggregate adjacent deltas for at most one frame-sized
// window while keeping the final assistant/message authoritative and keeping
// the interactive onText callback immediate.
const (
	streamChunkFlushInterval = 50 * time.Millisecond
	streamChunkMaxBytes      = 8 * 1024
)

// PreStepInjector is one registered pre-step context injector.
// -m5-agent-core.md 总体决策: the unified pre-step injection extension point that
// Inject is called at each step after
// user/message is appended, and its returned context is persisted before the
// model request. OncePerTurn is used by side-effectful turn hooks such as
// compaction and scheduling; Deduplicate is used by stable snapshots.
type PreStepInjector struct {
	Name        string // informational (logging/config); not a registration key
	Inject      func(ctx context.Context, userText string) []llm.Message
	OncePerTurn bool // run only for the first step of a turn
	Deduplicate bool // do not append an identical visible context message twice
}

// Loop drives one conversation turn against the session log.
type Loop struct {
	llm                    llm.LLM
	log                    *session.Log
	tools                  *tools.Registry
	toolSpecs              func() []llm.ToolSchema // per-session model-facing tool surface (dsh presentation mode)
	prompt                 *prompt.Builder
	model                  string
	provider               string
	effort                 string
	runtimeContext         func(context.Context, string) []llm.Message // dsh-style durable runtime snapshot
	preStep                []PreStepInjector                           // additional injectors, in registration order
	onText                 func(string)                                // optional sink for streamed assistant text (REPL)
	onError                func(error)                                 // optional sink for stream errors (REPL)
	recoverContextOverflow func(context.Context) bool                  // one forced compaction retry
	maxParallelToolCalls   int
}

// Config wires the loop's dependencies. All fields are required except the
// optional hooks.
type Config struct {
	LLM    llm.LLM
	Log    *session.Log
	Tools  *tools.Registry
	Prompt *prompt.Builder
	Model  string
	// Provider is persisted in request metadata when set; empty preserves the
	// legacy in-memory/test behavior.
	Provider string
	// ToolSpecs, when set, is the session's model-facing tool surface (dsh
	// presentation mode: standard = native tools minus run_code, PTC = only
	// run_code, minimal = the fixed seam). It is called on every step; when
	// nil the loop sends every registered tool schema.
	ToolSpecs func() []llm.ToolSchema
	// ReasoningEffort is the thinking-effort default applied to every model
	// request of this loop (dsh 思考强度; "" keeps the provider default).
	ReasoningEffort string
	// RuntimeContext supplies the dsh-style current runtime snapshot. It is
	// projected after the current user message and deduplicated against the
	// visible session surface.
	RuntimeContext func(context.Context, string) []llm.Message
	// PreStep registers additional pre-step context injectors. Injectors run in
	// registration order, with returned context bounded to maxInjectorChars;
	// OncePerTurn and Deduplicate control their cadence. A panicking injector is
	// skipped (fail-open).
	PreStep []PreStepInjector
	// OnText, if set, is called with each streamed assistant text delta.
	OnText func(string)
	// OnError, if set, is called when a step's stream fails after start.
	OnError func(error)
	// RecoverContextOverflow is called once when the provider rejects a request
	// because its context window is full. Returning true retries the same step
	// after the callback has compacted the append-only session surface.
	RecoverContextOverflow func(context.Context) bool
	// MaxParallelToolCalls bounds the rolling pool for explicitly
	// concurrency-safe tools. Zero uses dsh's default of ten.
	MaxParallelToolCalls int
}

// New returns a Loop.
func New(cfg Config) *Loop {
	return &Loop{
		llm:                    cfg.LLM,
		log:                    cfg.Log,
		tools:                  cfg.Tools,
		toolSpecs:              cfg.ToolSpecs,
		prompt:                 cfg.Prompt,
		model:                  cfg.Model,
		provider:               cfg.Provider,
		effort:                 cfg.ReasoningEffort,
		runtimeContext:         cfg.RuntimeContext,
		preStep:                append([]PreStepInjector(nil), cfg.PreStep...),
		onText:                 cfg.OnText,
		onError:                cfg.OnError,
		recoverContextOverflow: cfg.RecoverContextOverflow,
		maxParallelToolCalls:   maxParallelToolCalls(cfg.MaxParallelToolCalls),
	}
}

func maxParallelToolCalls(value int) int {
	if value <= 0 {
		return 10
	}
	return value
}

// Run executes one turn for the given user input. It appends user/message,
// then runs steps until the model stops requesting tools or maxSteps is hit.
// The supplied context cancels the current step (design.md §4).
func (l *Loop) Run(ctx context.Context, userText string) (runErr error) {
	turnNumber := l.log.NextTurn()
	if _, err := l.log.Append(session.EventTurnStart, session.NewTurnStart()); err != nil {
		return err
	}
	defer func() {
		status := "completed"
		if runErr != nil {
			status = "failed"
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				status = "cancelled"
			}
		}
		if _, err := l.log.Append(session.EventTurnEnd, session.NewTurnEnd(status, errorText(runErr))); err != nil && runErr == nil {
			runErr = err
		}
	}()
	if _, err := l.log.Append(session.EventUserMessage, session.NewUserMessage(userText)); err != nil {
		return err
	}
	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("loop: cancelled: %w", err)
		}
		// dsh runs pre-step projection for every step and persists each returned
		// user context message before deriving the request history. Injectors
		// that are turn-scoped opt out after step one; stable snapshots opt out
		// when the same visible message already exists.
		for _, inj := range l.effectiveInjectors() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("loop: cancelled: %w", err)
			}
			if inj.OncePerTurn && step > 0 {
				continue
			}
			for _, message := range l.safeInject(inj, ctx, userText) {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("loop: cancelled: %w", err)
				}
				if inj.Deduplicate && l.visibleMessageExists(message) {
					continue
				}
				if err := l.appendContextMessage(inj.Name, message); err != nil {
					return err
				}
			}
		}
		done, err := l.step(ctx, turnNumber, step+1)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("loop: exceeded %d steps per turn", maxSteps)
}

// effectiveInjectors returns the ordered pre-step injector list for one turn.
func (l *Loop) effectiveInjectors() []PreStepInjector {
	// dsh's normal path is user → system-prompt context → skill catalog →
	// other plugin context. Pressure compaction remains the first hook when it
	// is registered, so its pressure estimate does not include the snapshot it
	// may itself cause to be replaced.
	runtime := PreStepInjector{
		Name:        "runtime-context",
		Inject:      l.runtimeContext,
		Deduplicate: true,
	}
	var out []PreStepInjector
	for _, inj := range l.preStep {
		if inj.Name == "compaction" {
			out = append(out, inj)
			continue
		}
		if l.runtimeContext != nil && len(out) == 0 {
			out = append(out, runtime)
		}
		out = append(out, inj)
	}
	if l.runtimeContext != nil && len(out) == 0 {
		out = append(out, runtime)
	} else if l.runtimeContext != nil && len(out) > 0 && out[0].Name == "compaction" {
		out = append([]PreStepInjector{out[0], runtime}, out[1:]...)
	}
	return out
}

// appendContextMessage persists one pre-step user context message. Context is
// represented by the normal user/message event, as in dsh, so DeriveHistory is
// the source of truth for the initial request and every follow-up request.
func (l *Loop) appendContextMessage(injectorName string, message llm.Message) error {
	if strings.TrimSpace(message.Text()) == "" && len(message.Content) == 0 {
		return nil
	}
	sourceKind, sourcePlugin := contextSource(injectorName)
	var payload any
	if sourceKind != "" {
		payload = session.NewContextMessage(message.Text(), message.Content, sourceKind, sourcePlugin)
	} else {
		payload = session.NewUserMessageWithBlocks(message.Text(), message.Content)
	}
	_, err := l.log.Append(session.EventUserMessage,
		payload)
	return err
}

func contextSource(injectorName string) (kind, plugin string) {
	switch injectorName {
	case "runtime-context":
		return "plugin", "@deepseek-ai/dsh-system-prompt"
	case "skill":
		return "skill-catalog", ""
	default:
		return "", ""
	}
}

func (l *Loop) visibleMessageExists(message llm.Message) bool {
	want := message.Text()
	if want == "" {
		return false
	}
	// Stable pre-step snapshots only need to know whether an equivalent user
	// text was already persisted. Re-deriving the whole model surface here is
	// unnecessarily expensive for restored long sessions (especially after
	// repeated compaction replacements), so inspect the append-only user rows
	// directly. This preserves the old text equality semantics without making
	// cancellation wait for a full history fold.
	for _, event := range l.log.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Text == want {
			return true
		}
	}
	return false
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

func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	markers := []string{
		"context length", "context window", "context_length", "maximum context",
		"max context", "too many tokens", "token limit", "prompt is too long",
		"request too large",
	}
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// step performs one model request and its tool executions. It returns
// (true, nil) when the turn is complete (no tool calls requested). Pre-step
// context has already been persisted, so the request contains the system
// prompt followed by the durable derived history.
func (l *Loop) step(ctx context.Context, turnNumber, stepNumber int) (done bool, stepErr error) {
	if _, err := l.log.Append(session.EventStepStart, session.NewStepStart(stepNumber)); err != nil {
		return false, err
	}
	defer func() {
		status := "completed"
		if stepErr != nil {
			status = "failed"
			if errors.Is(stepErr, context.Canceled) || errors.Is(stepErr, context.DeadlineExceeded) {
				status = "cancelled"
			}
		}
		if _, err := l.log.Append(session.EventStepEnd, session.NewStepEnd(stepNumber, status, errorText(stepErr))); err != nil && stepErr == nil {
			stepErr = err
			done = false
		}
	}()
	history := l.log.DeriveHistory()
	specs := l.tools.Specs()
	if l.toolSpecs != nil {
		// The session's presentation mode owns the model-facing surface: the
		// wire tools array must match the mode exactly, never the full
		// registry (dsh assembly: native | code | both).
		specs = l.toolSpecs()
	}
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(l.prompt.Build())}})
	messages = append(messages, history...)

	requestID := fmt.Sprintf("turn:%d:step:%d", turnNumber, stepNumber)
	request := llm.ChatRequest{Provider: l.provider, Model: l.model, ReasoningEffort: l.effort, Messages: messages, Tools: specs}
	if l.provider != "" {
		if _, err := l.log.Append(session.EventLLMRequestStart, session.NewLLMRequestStartDetail(requestID, request)); err != nil {
			return false, err
		}
	}
	reader, err := l.llm.Stream(ctx, request)
	if err != nil && l.recoverContextOverflow != nil && isContextOverflowError(err) && l.recoverContextOverflow(ctx) {
		// Compaction appends a surface replacement marker. Rebuild history so the
		// retry sees the folded checkpoint rather than the rejected surface.
		history = l.log.DeriveHistory()
		messages = messages[:0]
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(l.prompt.Build())}})
		messages = append(messages, history...)
		if l.provider != "" {
			request.Messages = messages
			if _, startErr := l.log.Append(session.EventLLMRequestStart, session.NewLLMRequestStartDetail(requestID, request)); startErr != nil {
				return false, startErr
			}
		}
		request.Messages = messages
		reader, err = l.llm.Stream(ctx, request)
	}
	if err != nil {
		attempts := 1
		if l.provider != "" {
			if info, ok := err.(llm.RetryInfo); ok {
				attempts = info.Attempts()
				for _, retryEvent := range info.RetryEvents() {
					if _, appendErr := l.log.Append(session.EventLLMRetry, session.NewLLMRetry(l.provider, l.model, retryEvent)); appendErr != nil {
						return false, appendErr
					}
				}
			}
			_, _ = l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, l.provider, l.model, l.effort, "error", err.Error(), llm.TokenUsage{}, attempts))
		}
		return false, err
	}
	attempts := 1
	if info, ok := reader.(llm.RetryInfo); ok {
		attempts = info.Attempts()
		if l.provider != "" {
			for _, retryEvent := range info.RetryEvents() {
				if _, appendErr := l.log.Append(session.EventLLMRetry, session.NewLLMRetry(l.provider, l.model, retryEvent)); appendErr != nil {
					return false, appendErr
				}
			}
		}
	}

	var text strings.Builder
	var reasoning string
	var calls []llm.ToolCall
	var finishReason string
	var usage llm.TokenUsage
	var pendingChunk strings.Builder
	var pendingChunkKind string
	var pendingChunkAt time.Time
	flushPendingChunk := func() error {
		if pendingChunk.Len() == 0 {
			return nil
		}
		var payload any
		if pendingChunkKind == session.EventAssistantReasoning {
			payload = session.NewAssistantReasoning(pendingChunk.String())
		} else {
			payload = session.NewAssistantChunk(pendingChunk.String())
		}
		if _, err := l.log.Append(pendingChunkKind, payload); err != nil {
			return err
		}
		pendingChunk.Reset()
		pendingChunkKind = ""
		pendingChunkAt = time.Time{}
		return nil
	}
	queueStreamChunk := func(kind, value string) error {
		if value == "" {
			return nil
		}
		if pendingChunkKind != "" && pendingChunkKind != kind {
			if err := flushPendingChunk(); err != nil {
				return err
			}
		}
		if pendingChunk.Len() == 0 {
			pendingChunkAt = time.Now()
			pendingChunkKind = kind
		}
		pendingChunk.WriteString(value)
		if pendingChunk.Len() >= streamChunkMaxBytes || time.Since(pendingChunkAt) >= streamChunkFlushInterval {
			return flushPendingChunk()
		}
		return nil
	}
	for {
		ev, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if err := flushPendingChunk(); err != nil {
					return false, err
				}
				break
			}
			// Keep a durable assistant anchor when a stream was interrupted
			// after producing content. Without this row DeriveHistory would
			// discard the already logged chunk/reasoning rows on resume.
			if err := flushPendingChunk(); err != nil {
				return false, err
			}
			if text.Len() > 0 || reasoning != "" {
				if _, aerr := l.log.Append(session.EventAssistantMessage,
					session.NewInterruptedAssistantMessage(text.String(), calls, reasoning)); aerr != nil {
					return false, aerr
				}
			}
			if l.onError != nil {
				l.onError(err)
			}
			if l.provider != "" {
				_, _ = l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, l.provider, l.model, l.effort, "error", err.Error(), usage, attempts))
			}
			return false, err
		}
		switch ev.Kind {
		case llm.StreamTextDelta:
			text.WriteString(ev.Text)
			if l.onText != nil {
				l.onText(ev.Text)
			}
			if err := queueStreamChunk(session.EventAssistantChunk, ev.Text); err != nil {
				return false, err
			}
		case llm.StreamReasoningDelta:
			// M8: reasoning deltas are logged as they arrive (dsh order:
			// thinking before tool calls) so the UI can render the chain in
			// place; DeriveHistory folds these rows away in favor of the
			// joined reasoning on the closing assistant/message.
			reasoning += ev.Text
			if err := queueStreamChunk(session.EventAssistantReasoning, ev.Text); err != nil {
				return false, err
			}
		case llm.StreamFinish:
			calls = ev.ToolCalls
			finishReason = ev.FinishReason
			reasoning = ev.Reasoning // accumulated by the reader (M8)
			usage = ev.Usage
		}
	}
	if err := flushPendingChunk(); err != nil {
		return false, err
	}

	if _, err := l.log.Append(session.EventAssistantMessage, session.NewAssistantMessageWithUsage(text.String(), calls, finishReason, reasoning, usage)); err != nil {
		return false, err
	}
	if l.provider != "" {
		if _, err := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, l.provider, l.model, l.effort, "completed", "", usage, attempts)); err != nil {
			return false, err
		}
	}
	if len(calls) == 0 {
		return true, nil
	}
	if err := l.executeCalls(ctx, turnNumber, stepNumber, calls); err != nil {
		return false, err
	}
	return false, nil
}

type toolCallOutcome struct {
	call    llm.ToolCall
	callSeq uint64
	res     tools.ToolResult
	err     error
}

// executeCalls implements dsh's exclusive/parallel barrier semantics. Policy
// and approval preparation is ordered, safe bodies use a bounded rolling pool,
// and all results are committed in model order.
func (l *Loop) executeCalls(ctx context.Context, turnNumber, stepNumber int, calls []llm.ToolCall) error {
	for next := 0; next < len(calls); {
		if err := ctx.Err(); err != nil {
			if logErr := l.appendAbortedCalls(turnNumber, stepNumber, calls[next:]); logErr != nil {
				return logErr
			}
			return fmt.Errorf("loop: cancelled: %w", err)
		}

		parsed, parseErr := tools.ParseArguments([]byte(calls[next].Arguments))
		if parseErr != nil || !l.tools.IsConcurrencySafe(calls[next].Name, parsed) || l.maxParallelToolCalls <= 1 {
			prepareArgs := any(parsed)
			if parseErr != nil {
				prepareArgs = []byte(calls[next].Arguments)
			}
			outcome, err := l.startToolCall(ctx, turnNumber, stepNumber, calls[next], prepareArgs)
			if err != nil {
				return err
			}
			if err := l.commitToolOutcome(turnNumber, stepNumber, outcome); err != nil {
				return err
			}
			next++
			continue
		}
		if err := l.executeParallelGroup(ctx, turnNumber, stepNumber, calls, &next); err != nil {
			return err
		}
	}
	return nil
}

type settledToolCall struct {
	index   int
	outcome toolCallOutcome
}

// executeParallelGroup keeps the next unstarted call as a live barrier. This
// is important when a completed tool replaces or disables a later tool: dsh
// reclassifies that later call before starting it, rather than using a stale
// group classification.
func (l *Loop) executeParallelGroup(ctx context.Context, turnNumber, stepNumber int, calls []llm.ToolCall, next *int) error {
	start := *next
	committed := start
	started := start
	ready := make(map[int]toolCallOutcome)
	running := make(map[int]struct{})
	settled := make(chan settledToolCall, len(calls)-start)

	commitReady := func() error {
		for committed < started {
			outcome, ok := ready[committed]
			if !ok {
				return nil
			}
			if err := l.commitToolOutcome(turnNumber, stepNumber, outcome); err != nil {
				return err
			}
			delete(ready, committed)
			committed++
		}
		return nil
	}

	drain := func() {
		for len(running) > 0 {
			settledCall := <-settled
			delete(running, settledCall.index)
			ready[settledCall.index] = settledCall.outcome
		}
	}

	fill := func() error {
		for ctx.Err() == nil && *next < len(calls) && len(running)+len(ready) < l.maxParallelToolCalls {
			call := calls[*next]
			parsed, parseErr := tools.ParseArguments([]byte(call.Arguments))
			if parseErr != nil || !l.tools.IsConcurrencySafe(call.Name, parsed) {
				break
			}
			idx := *next
			callEvent, err := l.log.Append(session.EventToolCall,
				session.NewToolCall(turnNumber, stepNumber, call.ID, call.Name, call.Arguments))
			if err != nil {
				return err
			}
			(*next)++
			started++
			prepared, err := l.tools.Prepare(ctx, call.ID, call.Name, parsed)
			if err != nil {
				ready[idx] = toolCallOutcome{call: call, callSeq: callEvent.Seq, err: err}
				if err := commitReady(); err != nil {
					return err
				}
				continue
			}
			running[idx] = struct{}{}
			go func(index int, modelCall llm.ToolCall, execution *tools.PreparedExecution, sourceSeq uint64) {
				result, execErr := l.tools.ExecutePrepared(ctx, execution)
				settled <- settledToolCall{index: index, outcome: toolCallOutcome{call: modelCall, callSeq: sourceSeq, res: result, err: execErr}}
			}(idx, call, prepared, callEvent.Seq)
			if err := commitReady(); err != nil {
				drain()
				return err
			}
		}
		return nil
	}

	for {
		if err := fill(); err != nil {
			drain()
			return err
		}
		if err := commitReady(); err != nil {
			drain()
			return err
		}
		if ctx.Err() != nil {
			drain()
			if err := commitReady(); err != nil {
				return err
			}
			if err := l.appendAbortedCalls(turnNumber, stepNumber, calls[*next:]); err != nil {
				return err
			}
			return fmt.Errorf("loop: cancelled: %w", ctx.Err())
		}
		if len(running) == 0 {
			return nil
		}
		settledCall := <-settled
		delete(running, settledCall.index)
		ready[settledCall.index] = settledCall.outcome
	}
}

func (l *Loop) startToolCall(ctx context.Context, turnNumber, stepNumber int, call llm.ToolCall, parsed any) (toolCallOutcome, error) {
	callEvent, err := l.log.Append(session.EventToolCall,
		session.NewToolCall(turnNumber, stepNumber, call.ID, call.Name, call.Arguments))
	if err != nil {
		return toolCallOutcome{}, err
	}
	prepared, err := l.tools.Prepare(ctx, call.ID, call.Name, parsed)
	if err != nil {
		return toolCallOutcome{call: call, callSeq: callEvent.Seq, err: err}, nil
	}
	res, err := l.tools.ExecutePrepared(ctx, prepared)
	return toolCallOutcome{call: call, callSeq: callEvent.Seq, res: res, err: err}, nil
}

func (l *Loop) commitToolOutcome(turnNumber, stepNumber int, outcome toolCallOutcome) error {
	if outcome.err != nil {
		info := tools.ErrorInfoOf(outcome.err)
		_, err := l.log.Append(session.EventToolResult,
			session.NewToolErrorAtCodeWithSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.err.Error(), info.Code, outcome.callSeq))
		return err
	}
	var spill *session.SpillRef
	if outcome.res.SpillPath != "" {
		spill = &session.SpillRef{Locator: outcome.res.SpillPath, Bytes: outcome.res.SpillBytes}
	}
	var payload any
	if outcome.res.IsError {
		code := "TOOL_RESULT_ERROR"
		if outcome.res.Error != nil && outcome.res.Error.Code != "" {
			code = outcome.res.Error.Code
		}
		if len(outcome.res.Content) > 0 {
			payload = session.NewToolErrorResultWithContentAtCodeWithSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, outcome.res.Content, code, outcome.callSeq)
		} else {
			payload = session.NewToolErrorResultAtCodeWithSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, spill, code, outcome.callSeq)
		}
	} else {
		payload = session.NewToolResultAtWithSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, spill, outcome.callSeq)
		if len(outcome.res.Content) > 0 {
			payload = session.NewToolResultWithContentAtSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, outcome.res.Content, outcome.callSeq)
		}
	}
	_, err := l.log.Append(session.EventToolResult, payload)
	return err
}

func (l *Loop) appendAbortedCalls(turnNumber, stepNumber int, calls []llm.ToolCall) error {
	for _, call := range calls {
		callEvent, err := l.log.Append(session.EventToolCall,
			session.NewToolCall(turnNumber, stepNumber, call.ID, call.Name, call.Arguments))
		if err != nil {
			return err
		}
		if _, err := l.log.Append(session.EventToolResult,
			session.NewAbortedToolResultAtWithSource(turnNumber, stepNumber, call.ID, call.Name, callEvent.Seq)); err != nil {
			return err
		}
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
