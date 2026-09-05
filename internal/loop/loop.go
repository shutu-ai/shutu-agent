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
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	llmretry "github.com/shutu-ai/shutu-agent/internal/llm/retry"
	"github.com/shutu-ai/shutu-agent/internal/observability"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/timecontext"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

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
	Name   string // informational (logging/config); not a registration key
	Inject func(ctx context.Context, userText string) []llm.Message
	// InjectWithError is the durable variant for injectors whose preparation
	// facts must be committed before the model request. When set it takes
	// precedence over Inject and its error fail-stops the step; Inject remains
	// the compatibility fail-open path for observational injectors.
	InjectWithError func(ctx context.Context, userText string) ([]llm.Message, error)
	OncePerTurn     bool // run only for the first step of a turn
	Deduplicate     bool // do not append an identical visible context message twice
	// Unbounded marks injectors whose producer owns a complete wire contract.
	// The skill catalog is one instance: DSH publishes every entry and bounds
	// each description, so a generic 4k cut silently hides later skills.
	Unbounded bool
}

// PreStepDecision is the authoritative result of the agent/pre-step
// waterfall. A rejected or empty first proposal closes the durable turn
// without spending a model step, matching DSH's claimed-work semantics.
type PreStepDecision struct {
	Kind     string // "reject" or "enter"
	Messages []llm.Message
}

// PreStepPayload describes the messages removed from the Agent inbox for a
// proposed step. Hooks may replace the messages or reject the proposal, but
// model-visible messages are persisted by Loop before the request is built.
type PreStepPayload struct {
	Turn     int
	Step     int
	UserText string
	Messages []llm.Message
}

// PreStepNext is the downstream continuation of a pre-step waterfall.
type PreStepNext func(context.Context, PreStepPayload) (PreStepDecision, error)

// PreStepHook is an around-middleware extension point. A hook that wants to
// preserve downstream contributions must call next.
type PreStepHook func(context.Context, PreStepPayload, PreStepNext) (PreStepDecision, error)

// RequestPayload is the frozen model request boundary. Hooks may replace the
// request configuration, while the durable request/header records the final
// request that was actually attempted.
type RequestPayload struct {
	Turn    int
	Step    int
	Signal  context.Context
	Request llm.ChatRequest
}

type RequestNext func(context.Context, RequestPayload) (llm.ChatRequest, error)

// RequestHook wraps agent/request. It is deliberately separate from the LLM
// provider so providers remain transport adapters rather than policy owners.
type RequestHook func(context.Context, RequestPayload, RequestNext) (llm.ChatRequest, error)

// RequestErrorPayload is the provider-neutral request failure seam.
type RequestErrorPayload struct {
	Turn     int
	Step     int
	Provider string
	Error    error
}

// RequestErrorHook returns true when it owns one retry of the failed request.
type RequestErrorHook func(context.Context, RequestErrorPayload, func(context.Context, RequestErrorPayload) (bool, error)) (bool, error)

// TurnStoppingPayload is emitted after step/end when a model response did not
// request tools. A turn-stopping hook may claim another same-turn step by
// returning Stop=false and supplying the next-step messages. This is the
// runtime seam used by Agent inboxes for follow-ups and steering; ordinary
// user follow-ups remain separate turns.
type TurnStoppingPayload struct {
	Turn int
	Step int
}

type TurnStoppingDecision struct {
	Stop     bool
	Messages []llm.Message
}

type TurnStoppingNext func(context.Context, TurnStoppingPayload) (TurnStoppingDecision, error)

// TurnStoppingHook wraps the decision made after a completed model step.
// Hooks are composed in registration order and may call next to preserve the
// downstream decision.
type TurnStoppingHook func(context.Context, TurnStoppingPayload, TurnStoppingNext) (TurnStoppingDecision, error)

// Loop drives one conversation turn against the session log.
type Loop struct {
	llm llm.LLM
	// resolveRequestLLM selects the transport and retry policy after request
	// middleware has produced the final provider/model route. Keeping this
	// optional preserves the lightweight single-LLM contract for embedders,
	// while the application wiring can make a request-hook route change real
	// rather than metadata-only.
	resolveRequestLLM      func(context.Context, llm.ChatRequest) (llm.LLM, error)
	log                    *session.Log
	tools                  *tools.Registry
	toolSpecs              func() []llm.ToolSchema // per-session model-facing tool surface (dsh presentation mode)
	prompt                 *prompt.Builder
	model                  string
	provider               string
	effort                 string
	reasoningBudgetTokens  int
	maxTokens              int
	temperature            *float64
	stop                   []string
	contextWindow          int
	runtimeContext         func(context.Context, string) []llm.Message // dsh-style durable runtime snapshot
	timeContext            *timecontext.Service
	preStep                []PreStepInjector // additional injectors, in registration order
	preStepHooks           []PreStepHook
	requestHooks           []RequestHook
	requestErrorHooks      []RequestErrorHook
	turnStoppingHooks      []TurnStoppingHook
	continueOnCancel       func(context.Context) ([]llm.Message, bool, error)
	onText                 func(string)                          // optional sink for streamed assistant text (REPL)
	onError                func(error)                           // optional sink for stream errors (REPL)
	onUsage                func(llm.ChatRequest, llm.TokenUsage) // detached usage/telemetry sink
	metrics                *observability.Metrics
	tracer                 *observability.Tracer
	recoverContextOverflow func(context.Context) bool // one forced compaction retry
	runtimeSessionID       string
	runtimeAgentID         string
	runtimeEmit            func(string, any) error
	maxParallelToolCalls   int
	maxParallelToolCallsFn func() int
}

// Config wires the loop's dependencies. All fields are required except the
// optional hooks.
type Config struct {
	LLM    llm.LLM
	Log    *session.Log
	Tools  *tools.Registry
	Prompt *prompt.Builder
	Model  string
	// MaxTokens, Temperature and Stop are optional per-request generation
	// controls. They are carried to the selected provider on every step.
	MaxTokens   int
	Temperature *float64
	Stop        []string
	// ContextWindow is the resolved route capacity, when known. It is logged
	// as the independent reference-compatible request/context projection.
	ContextWindow int
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
	// ReasoningBudgetTokens is an optional provider thinking budget. Zero lets
	// the provider use its reference default.
	ReasoningBudgetTokens int
	// RuntimeContext supplies the dsh-style current runtime snapshot. It is
	// projected after the current user message and deduplicated against the
	// visible session surface.
	RuntimeContext func(context.Context, string) []llm.Message
	// TimeContext, when non-nil, appends the DSH-compatible durable clock
	// reading after every other pre-step contribution. Errors fail the step
	// before provider I/O; nil leaves the optional provider disabled.
	TimeContext *timecontext.Service
	// PreStep registers additional pre-step context injectors. Injectors run in
	// registration order, with returned context bounded to maxInjectorChars;
	// OncePerTurn and Deduplicate control their cadence. A panicking injector is
	// skipped (fail-open).
	PreStep []PreStepInjector
	// PreStepHooks are DSH-compatible around-middleware hooks. They run for a
	// claimed external message batch before the first model step.
	PreStepHooks []PreStepHook
	// RequestHooks wrap the final request configuration before provider I/O.
	RequestHooks []RequestHook
	// ResolveRequestLLM resolves the transport after RequestHooks have run.
	// The returned LLM also owns the retry policy for this exact route. A nil
	// resolver retains the configured LLM for compatibility callers.
	ResolveRequestLLM func(context.Context, llm.ChatRequest) (llm.LLM, error)
	// RequestErrorHooks may own one bounded retry for a provider-neutral error.
	RequestErrorHooks []RequestErrorHook
	// TurnStoppingHooks run after step/end when the model has no tool calls.
	// Returning Stop=false claims a same-turn next step; Messages are appended
	// to the next step as user/context messages and may be empty when the hook
	// intentionally re-requests from the existing surface.
	TurnStoppingHooks []TurnStoppingHook
	// ContinueOnCancel may claim a steering batch after the active provider
	// step was interrupted. A successful claim resumes the same durable turn;
	// returning false preserves ordinary external cancellation semantics.
	ContinueOnCancel func(context.Context) ([]llm.Message, bool, error)
	// OnText, if set, is called with each streamed assistant text delta.
	OnText func(string)
	// OnError, if set, is called when a step's stream fails after start.
	OnError func(error)
	// OnUsage, if set, receives successful provider usage together with the
	// exact request that produced it. The callback runs after the stream has
	// finished and before the durable request/end marker.
	OnUsage func(llm.ChatRequest, llm.TokenUsage)
	// Metrics is an optional non-durable aggregate. It is observation-only and
	// must never be used as a control-flow dependency.
	Metrics *observability.Metrics
	// Tracer is an optional bounded in-process span recorder. Span finalization
	// is best-effort and never changes the loop result.
	Tracer *observability.Tracer
	// RecoverContextOverflow is called once when the provider rejects a request
	// because its context window is full. Returning true retries the same step
	// after the callback has compacted the append-only session surface.
	RecoverContextOverflow func(context.Context) bool
	// RuntimeSessionID and RuntimeEmit bind tool callbacks to the session-owned
	// runtime that created this loop. They are optional for legacy callers.
	RuntimeSessionID string
	RuntimeAgentID   string
	RuntimeEmit      func(string, any) error
	// MaxParallelToolCalls bounds the rolling pool for explicitly
	// concurrency-safe tools. Zero uses dsh's default of ten.
	MaxParallelToolCalls int
	// MaxParallelToolCallsFunc, when set, is consulted for every tool batch so
	// a committed settings change reaches an already-constructed Agent loop.
	// A non-positive result falls back to MaxParallelToolCalls/default rules.
	MaxParallelToolCallsFunc func() int
}

// New returns a Loop.
func New(cfg Config) *Loop {
	return &Loop{
		llm:                    cfg.LLM,
		resolveRequestLLM:      cfg.ResolveRequestLLM,
		log:                    cfg.Log,
		tools:                  cfg.Tools,
		toolSpecs:              cfg.ToolSpecs,
		prompt:                 cfg.Prompt,
		model:                  cfg.Model,
		provider:               cfg.Provider,
		effort:                 cfg.ReasoningEffort,
		reasoningBudgetTokens:  cfg.ReasoningBudgetTokens,
		maxTokens:              cfg.MaxTokens,
		temperature:            cfg.Temperature,
		stop:                   append([]string(nil), cfg.Stop...),
		contextWindow:          cfg.ContextWindow,
		runtimeContext:         cfg.RuntimeContext,
		timeContext:            cfg.TimeContext,
		preStep:                append([]PreStepInjector(nil), cfg.PreStep...),
		preStepHooks:           append([]PreStepHook(nil), cfg.PreStepHooks...),
		requestHooks:           append([]RequestHook(nil), cfg.RequestHooks...),
		requestErrorHooks:      append([]RequestErrorHook(nil), cfg.RequestErrorHooks...),
		turnStoppingHooks:      append([]TurnStoppingHook(nil), cfg.TurnStoppingHooks...),
		continueOnCancel:       cfg.ContinueOnCancel,
		onText:                 cfg.OnText,
		onError:                cfg.OnError,
		onUsage:                cfg.OnUsage,
		metrics:                cfg.Metrics,
		tracer:                 cfg.Tracer,
		recoverContextOverflow: cfg.RecoverContextOverflow,
		runtimeSessionID:       cfg.RuntimeSessionID,
		runtimeAgentID:         cfg.RuntimeAgentID,
		runtimeEmit:            cfg.RuntimeEmit,
		maxParallelToolCalls:   maxParallelToolCalls(cfg.MaxParallelToolCalls),
		maxParallelToolCallsFn: cfg.MaxParallelToolCallsFunc,
	}
}

func (l *Loop) effectiveMaxParallelToolCalls() int {
	if l == nil {
		return 1
	}
	if l.maxParallelToolCallsFn != nil {
		return maxParallelToolCalls(l.maxParallelToolCallsFn())
	}
	return l.maxParallelToolCalls
}

func maxParallelToolCalls(value int) int {
	if value <= 0 {
		return 10
	}
	return value
}

// AddTurnStoppingHook appends a same-turn continuation hook. It is primarily
// used by long-lived Agent drivers that must claim inbox work after a model
// step has emitted no tool calls.
func (l *Loop) AddTurnStoppingHook(hook TurnStoppingHook) {
	if l == nil || hook == nil {
		return
	}
	l.turnStoppingHooks = append(l.turnStoppingHooks, hook)
}

// AddPreStepHook appends an Agent-owned pre-step middleware after hooks
// configured at loop construction. It is used by runtime bridges that need to
// claim addressed inbox work at each proposed continuation step.
func (l *Loop) AddPreStepHook(hook PreStepHook) {
	if l == nil || hook == nil {
		return
	}
	l.preStepHooks = append(l.preStepHooks, hook)
}

// PrependPreStepHook installs an Agent-owned pre-step middleware before hooks
// configured at loop construction. Inbox claims are deliberately placed at
// this edge: the Harness claims addressed work before compaction/skill
// projection, so those projections observe the exact message batch that will
// enter the proposed step.
func (l *Loop) PrependPreStepHook(hook PreStepHook) {
	if l == nil || hook == nil {
		return
	}
	l.preStepHooks = append([]PreStepHook{hook}, l.preStepHooks...)
}

// SetContinueOnCancel installs the optional same-turn steering continuation
// seam used by an Agent-owned driver. It must be configured before Run.
func (l *Loop) SetContinueOnCancel(continueOnCancel func(context.Context) ([]llm.Message, bool, error)) {
	if l == nil {
		return
	}
	l.continueOnCancel = continueOnCancel
}

// Run executes one turn for the given user input. It appends user/message,
// then runs steps until the model stops requesting tools. There is no
// arbitrary fixed step count; context, model/token budgets, and provider
// errors remain the termination mechanisms, matching DSH's turn boundary.
// The supplied context cancels the current step (design.md §4).
func (l *Loop) Run(ctx context.Context, userText string) (runErr error) {
	return l.RunMessages(ctx, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(userText)}}})
}

// RunMessages executes one turn from an ordered batch of user/context
// messages. It is the bridge used by the Agent inbox so multiple claimed
// messages are preserved as separate durable events instead of being joined
// into one lossy string.
func (l *Loop) RunMessages(ctx context.Context, inputs []llm.Message) (runErr error) {
	if len(inputs) == 0 {
		return errors.New("loop: at least one input message is required")
	}
	if l.runtimeSessionID != "" || l.runtimeEmit != nil {
		ctx = runtimectx.With(ctx, runtimectx.Runtime{
			SessionID: l.runtimeSessionID,
			Emit:      l.runtimeEmit,
			Trace: runtimectx.Correlation{
				AgentID:   l.runtimeAgentID,
				SessionID: l.runtimeSessionID,
			},
		})
	}
	turnNumber := l.log.NextTurn()
	if l.metrics != nil {
		l.metrics.Turn()
	}
	turnStatus := "completed"
	finalFinishReason := ""
	// DSH treats an output-token ceiling as sticky for the whole turn: once any
	// step hits it, a later tool-driven step that finishes normally must not
	// downgrade the durable turn outcome.
	turnMaxTokens := false
	if _, err := l.log.Append(session.EventTurnStart, session.NewTurnStartAt(turnNumber)); err != nil {
		return err
	}
	defer func() {
		status := turnStatus
		if runErr != nil {
			status = "failed"
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				status = "cancelled"
			}
		}
		var endPayload any
		if failure, ok := llm.FailureFacts(runErr); ok && status == "failed" {
			endPayload = session.NewTurnEndAtFailure(turnNumber, status, failure)
		} else {
			endPayload = session.NewTurnEndAt(turnNumber, status, errorText(runErr))
		}
		if _, err := l.log.Append(session.EventTurnEnd, endPayload); err != nil && runErr == nil {
			runErr = err
		}
	}()
	for _, input := range inputs {
		if input.Role != "" && input.Role != llm.RoleUser {
			return fmt.Errorf("loop: input message role %q is not user", input.Role)
		}
		if strings.TrimSpace(input.Text()) == "" && len(input.Content) == 0 {
			return errors.New("loop: input message is empty")
		}
	}
	userText := inputs[0].Text()
	var nextStepMessages []llm.Message
	var toolResultBoundary runtimectx.ToolResultBoundary
	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			if l.continueOnCancel != nil {
				messages, continued, continueErr := l.continueOnCancel(ctx)
				if continueErr != nil {
					return continueErr
				}
				if continued {
					ctx = context.WithoutCancel(ctx)
					nextStepMessages = messages
					continue
				}
			}
			return fmt.Errorf("loop: cancelled: %w", err)
		}
		stepCtx := runtimectx.WithCorrelation(ctx, runtimectx.Correlation{
			AgentID:   l.runtimeAgentID,
			SessionID: l.runtimeSessionID,
			TurnID:    fmt.Sprintf("turn:%d", turnNumber),
			StepID:    fmt.Sprintf("step:%d", step+1),
		})
		if toolResultBoundary.Present() {
			stepCtx = runtimectx.WithToolResultsComplete(stepCtx, toolResultBoundary)
		}
		var entered []llm.Message
		if step > 0 && nextStepMessages != nil {
			entered = cloneMessages(nextStepMessages)
			nextStepMessages = nil
		}
		if step == 0 {
			entered = cloneMessages(inputs)
		}
		// The reference agent/pre-step waterfall runs for every proposed step,
		// including model/tool continuations. Later steps may legitimately have
		// no newly claimed user message: the existing durable history is still
		// the proposal, and a hook or injector may add context before the next
		// request. Applying the waterfall only to step one silently skipped
		// per-step context, plan-mode and cancellation-boundary hooks.
		if len(l.preStepHooks) > 0 {
			decision, err := l.runPreStep(stepCtx, turnNumber, step+1, userText, entered)
			if err != nil {
				return err
			}
			if decision.Kind != "enter" || (step == 0 && len(decision.Messages) == 0) {
				// A pre-step refusal is a blocked turn, not a generic failed or
				// rejected request. This is the durable reason used by the
				// reference Agent when policy/inbox work is claimed but no step
				// may be entered.
				turnStatus = "blocked"
				return nil
			}
			entered = decision.Messages
		}
		// dsh runs pre-step projection before deriving the request history. The
		// resulting messages are committed by step after step/start, preserving
		// the exact durable lifecycle order.
		contextMessages, err := l.collectInjectors(stepCtx, step, userText)
		if err != nil {
			return err
		}
		if l.timeContext != nil {
			requestMessages := append(append([]llm.Message(nil), entered...), contextMessages...)
			reading, err := l.timeContext.Reading(l.log.Events(), turnNumber, step+1, requestMessages)
			if err != nil {
				return err
			}
			if reading != nil {
				contextMessages = append(contextMessages, *reading)
			}
		}
		if len(contextMessages) > 0 {
			entered = append(append([]llm.Message(nil), entered...), contextMessages...)
		}
		var nextBoundary runtimectx.ToolResultBoundary
		done, err := l.step(stepCtx, turnNumber, step+1, entered, &finalFinishReason, &nextBoundary)
		if err != nil {
			if l.continueOnCancel != nil && errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				messages, continued, continueErr := l.continueOnCancel(stepCtx)
				if continueErr != nil {
					return continueErr
				}
				if continued {
					ctx = context.WithoutCancel(ctx)
					nextStepMessages = messages
					continue
				}
			}
			return err
		}
		toolResultBoundary = nextBoundary
		switch finalFinishReason {
		case "length", "max_tokens":
			turnMaxTokens = true
		}
		if done {
			if err := ctx.Err(); err != nil {
				if l.continueOnCancel != nil {
					messages, continued, continueErr := l.continueOnCancel(ctx)
					if continueErr != nil {
						return continueErr
					}
					if continued {
						ctx = context.WithoutCancel(ctx)
						nextStepMessages = messages
						continue
					}
				}
				return fmt.Errorf("loop: cancelled: %w", err)
			}
			if turnMaxTokens {
				turnStatus = "max-tokens"
			} else if finalFinishReason == "content_filter" || finalFinishReason == "refusal" {
				turnStatus = "refusal"
			}
			decision, err := l.runTurnStopping(stepCtx, turnNumber, step+1)
			if err != nil {
				return err
			}
			if decision.Stop {
				return nil
			}
			nextStepMessages = decision.Messages
		}
	}
}

// MaxTokens reports the effective output budget for diagnostics and contract
// tests. The field remains private so request middleware cannot mutate a loop
// after admission.
func (l *Loop) MaxTokens() int {
	if l == nil {
		return 0
	}
	return l.maxTokens
}

// ContextWindow reports the effective input capacity for diagnostics and
// contract tests. Zero means the provider default remains in control.
func (l *Loop) ContextWindow() int {
	if l == nil {
		return 0
	}
	return l.contextWindow
}

func (l *Loop) runPreStep(ctx context.Context, turn, step int, userText string, messages []llm.Message) (PreStepDecision, error) {
	payload := PreStepPayload{Turn: turn, Step: step, UserText: userText, Messages: cloneMessages(messages)}
	base := func(_ context.Context, p PreStepPayload) (PreStepDecision, error) {
		return PreStepDecision{Kind: "enter", Messages: cloneMessages(p.Messages)}, nil
	}
	next := PreStepNext(base)
	for i := len(l.preStepHooks) - 1; i >= 0; i-- {
		hook := l.preStepHooks[i]
		if hook == nil {
			continue
		}
		downstream := next
		next = func(h PreStepHook, downstream PreStepNext) PreStepNext {
			return func(hctx context.Context, p PreStepPayload) (PreStepDecision, error) {
				return h(hctx, p, downstream)
			}
		}(hook, downstream)
	}
	decision, err := next(ctx, payload)
	if err != nil {
		return PreStepDecision{}, err
	}
	if decision.Kind == "" {
		decision.Kind = "enter"
	}
	if decision.Kind != "enter" && decision.Kind != "reject" {
		return PreStepDecision{}, fmt.Errorf("loop: invalid pre-step decision %q", decision.Kind)
	}
	return decision, nil
}

func (l *Loop) applyRequestHooks(ctx context.Context, turn, step int, request llm.ChatRequest) (llm.ChatRequest, error) {
	payload := RequestPayload{Turn: turn, Step: step, Signal: ctx, Request: request}
	base := func(_ context.Context, p RequestPayload) (llm.ChatRequest, error) {
		return p.Request, nil
	}
	next := RequestNext(base)
	for i := len(l.requestHooks) - 1; i >= 0; i-- {
		hook := l.requestHooks[i]
		if hook == nil {
			continue
		}
		downstream := next
		next = func(h RequestHook, downstream RequestNext) RequestNext {
			return func(hctx context.Context, p RequestPayload) (llm.ChatRequest, error) {
				return h(hctx, p, downstream)
			}
		}(hook, downstream)
	}
	return next(ctx, payload)
}

func (l *Loop) handleRequestError(ctx context.Context, payload RequestErrorPayload) (bool, error) {
	base := func(context.Context, RequestErrorPayload) (bool, error) { return false, nil }
	next := base
	for i := len(l.requestErrorHooks) - 1; i >= 0; i-- {
		hook := l.requestErrorHooks[i]
		if hook == nil {
			continue
		}
		downstream := next
		next = func(h RequestErrorHook, downstream func(context.Context, RequestErrorPayload) (bool, error)) func(context.Context, RequestErrorPayload) (bool, error) {
			return func(hctx context.Context, p RequestErrorPayload) (bool, error) {
				return h(hctx, p, downstream)
			}
		}(hook, downstream)
	}
	return next(ctx, payload)
}

func (l *Loop) runTurnStopping(ctx context.Context, turn, step int) (TurnStoppingDecision, error) {
	base := func(_ context.Context, _ TurnStoppingPayload) (TurnStoppingDecision, error) {
		return TurnStoppingDecision{Stop: true}, nil
	}
	next := TurnStoppingNext(base)
	for i := len(l.turnStoppingHooks) - 1; i >= 0; i-- {
		hook := l.turnStoppingHooks[i]
		if hook == nil {
			continue
		}
		downstream := next
		next = func(h TurnStoppingHook, downstream TurnStoppingNext) TurnStoppingNext {
			return func(hctx context.Context, p TurnStoppingPayload) (TurnStoppingDecision, error) {
				return h(hctx, p, downstream)
			}
		}(hook, downstream)
	}
	decision, err := next(ctx, TurnStoppingPayload{Turn: turn, Step: step})
	if err != nil {
		return TurnStoppingDecision{}, err
	}
	if !decision.Stop && decision.Messages == nil && len(l.turnStoppingHooks) == 0 {
		return TurnStoppingDecision{Stop: true}, nil
	}
	return decision, nil
}

func (l *Loop) collectInjectors(ctx context.Context, step int, userText string) ([]llm.Message, error) {
	var out []llm.Message
	for _, inj := range l.effectiveInjectors() {
		if err := ctx.Err(); err != nil || (inj.OncePerTurn && step > 0) {
			continue
		}
		messages, err := l.safeInject(inj, ctx, userText)
		if err != nil {
			return nil, fmt.Errorf("loop: pre-step %s: %w", inj.Name, err)
		}
		for _, message := range messages {
			if inj.Deduplicate && (l.visibleMessageExists(message) || containsMessage(out, message)) {
				continue
			}
			// Preserve producer-owned source first; only injectors that still
			// return legacy unattributed context get the transport fallback.
			if message.SourceKind == "" {
				if sourceKind, sourcePlugin := legacyContextSource(inj.Name); sourceKind != "" {
					message.SourceKind, message.SourcePlugin = sourceKind, sourcePlugin
				}
			}
			out = append(out, message)
		}
	}
	return out, nil
}

func legacyContextSource(injectorName string) (kind, plugin string) {
	switch injectorName {
	case "runtime-context":
		return "plugin", "@shutu-ai/system-prompt"
	case "skill":
		return "skill-catalog", ""
	default:
		return "", ""
	}
}

func cloneMessages(in []llm.Message) []llm.Message {
	out := make([]llm.Message, len(in))
	copy(out, in)
	for i := range out {
		out[i].Content = append([]llm.ContentBlock(nil), out[i].Content...)
		out[i].ToolCalls = append([]llm.ToolCall(nil), out[i].ToolCalls...)
	}
	return out
}

func containsMessage(messages []llm.Message, want llm.Message) bool {
	for _, message := range messages {
		if message.Text() == want.Text() && len(message.Content) == len(want.Content) {
			return true
		}
	}
	return false
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
	runtimeInserted := false
	for _, inj := range l.preStep {
		if inj.Name == "compaction" {
			out = append(out, inj)
			continue
		}
		if inj.Name == "agent-instructions" && l.runtimeContext != nil && !runtimeInserted {
			out = append(out, inj)
			out = append(out, runtime)
			runtimeInserted = true
			continue
		}
		if l.runtimeContext != nil && !runtimeInserted && len(out) == 0 {
			out = append(out, runtime)
			runtimeInserted = true
		}
		out = append(out, inj)
	}
	if l.runtimeContext != nil && !runtimeInserted && len(out) == 0 {
		out = append(out, runtime)
		runtimeInserted = true
	} else if l.runtimeContext != nil && !runtimeInserted && len(out) > 0 && out[0].Name == "compaction" {
		out = append([]PreStepInjector{out[0], runtime}, out[1:]...)
		runtimeInserted = true
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
	var payload any
	if message.SourceKind != "" {
		// Producers own their complete durable source. The injector name is
		// transport wiring, not a reason to collapse plugin, skill, recall, or
		// instruction provenance into one generic shape.
		payload = session.NewContextMessageFromLLM(message)
	} else {
		payload = session.NewUserMessageWithBlocks(message.Text(), message.Content)
	}
	_, err := l.log.Append(session.EventUserMessage,
		payload)
	return err
}

func (l *Loop) visibleMessageExists(message llm.Message) bool {
	want := message.Text()
	if want == "" {
		return false
	}
	events := l.log.Events()
	shadowed := make(map[uint64]bool)
	for _, event := range events {
		if event.Type != session.EventUserMessage {
			continue
		}
		if sourceSeqs, ok := session.EventSourceEventSeqs(event); ok {
			for _, seq := range sourceSeqs {
				shadowed[seq] = true
			}
			continue
		}
		if replacement, ok := session.SurfaceReplacement(event); ok {
			for seq := uint64(replacement.Start); seq <= uint64(replacement.End); seq++ {
				shadowed[seq] = true
			}
		}
	}
	// Stable pre-step snapshots only need to know whether an equivalent user
	// text is still visible. Re-deriving the whole model surface here is
	// unnecessarily expensive for restored long sessions (especially after
	// repeated compaction replacements), so inspect the append-only user rows
	// directly. Source attribution prevents ordinary user text from suppressing
	// a producer-owned snapshot; shadowed rows do not suppress republication.
	for _, event := range events {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Text    string `json:"text"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Source *durableMessageSource `json:"source"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		// NewContextMessageFromLLM persists Text and rich Content as the same
		// projection for compatibility. Compare one or the other, never both.
		text := data.Text
		if len(data.Content) > 0 {
			text = ""
			for _, block := range data.Content {
				text += block.Text
			}
		}
		if text != want {
			continue
		}
		if message.SourceKind != "" {
			// A recurring payload is not necessarily a live context: DSH's
			// replacement state is producer-owned, so A→B→A must publish the
			// final A even though an older A row has the same text. Compare the
			// complete durable source, not just kind/plugin/body.
			if data.Source == nil || !data.Source.equal(message) {
				continue
			}
		} else if data.Source != nil && data.Source.Kind != "user" {
			continue
		}
		if !shadowed[event.Seq] {
			return true
		}
	}
	return false
}

// durableMessageSource mirrors the producer-owned user/message source. It is
// local to the loop's visibility fast path; session owns the durable schema.
type durableMessageSource struct {
	Kind             string `json:"kind"`
	Form             string `json:"form,omitempty"`
	Update           bool   `json:"update,omitempty"`
	Plugin           string `json:"plugin,omitempty"`
	Name             string `json:"name,omitempty"`
	Entries          any    `json:"entries,omitempty"`
	References       any    `json:"references,omitempty"`
	Sections         any    `json:"sections,omitempty"`
	Summary          string `json:"summary,omitempty"`
	SenderSessionID  string `json:"senderSessionId,omitempty"`
	Baseline         bool   `json:"baseline,omitempty"`
	BaselineIdentity string `json:"baselineIdentity,omitempty"`
	Changes          any    `json:"changes,omitempty"`
	TeamID           string `json:"teamId,omitempty"`
	MessageID        string `json:"messageId,omitempty"`
	SenderID         string `json:"senderId,omitempty"`
	SenderName       string `json:"senderName,omitempty"`
}

func (s *durableMessageSource) equal(message llm.Message) bool {
	if s == nil {
		return false
	}
	candidate := durableMessageSource{
		Kind: message.SourceKind, Form: message.SourceForm, Update: message.SourceUpdate,
		Plugin: message.SourcePlugin, Name: message.SourceName, Entries: optionalJSONValue(message.SourceEntries),
		References: optionalJSONValue(message.SourceReferences), Sections: optionalJSONValue(message.SourceSections),
		Summary: message.SourceSummary, SenderSessionID: message.SourceSenderSessionID,
		Baseline: message.SourceBaseline, BaselineIdentity: message.SourceBaselineIdentity,
		Changes: optionalJSONValue(message.SourceChanges), TeamID: message.SourceTeamID,
		MessageID: message.SourceMessageID, SenderID: message.SourceSenderID,
		SenderName: message.SourceSenderName,
	}
	left, leftErr := json.Marshal(*s)
	right, rightErr := json.Marshal(candidate)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func optionalJSONValue(value any) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map:
		if rv.IsNil() {
			return nil
		}
	}
	return value
}

// safeInject calls one injector and bounds its contribution, containing a
// panic so a throwing injector is skipped (fail-open) instead of aborting the
// turn.

func (l *Loop) safeInject(inj PreStepInjector, ctx context.Context, userText string) (msgs []llm.Message, err error) {
	if inj.InjectWithError != nil {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("injector panic: %v", r)
				msgs = nil
			}
		}()
		msgs, err = inj.InjectWithError(ctx, userText)
		if err != nil {
			return nil, err
		}
		if inj.Unbounded {
			return msgs, nil
		}
		return truncateInjectorContext(msgs), nil
	}
	if inj.Inject == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			msgs = nil
			err = nil
		}
	}()
	msgs = inj.Inject(ctx, userText)
	if inj.Unbounded {
		return msgs, nil
	}
	return truncateInjectorContext(msgs), nil
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
	if failure, ok := llm.FailureFacts(err); ok && failure.Code == "CONTEXT_WINDOW_EXCEEDED" {
		return true
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

// normalizeFinishFailure mirrors the reference adapter boundary: only the
// known successful finish reasons are accepted as a completed model step;
// provider additions such as content_filter must not silently become a clean
// answer. The provider-neutral loop owns this fallback for every adapter.
func normalizeFinishFailure(reason string) (llm.Failure, bool) {
	switch reason {
	case "", "stop", "tool_calls", "length", "max_tokens":
		return llm.Failure{}, false
	case "content_filter":
		return llm.Failure{Message: "model response was blocked by the content filter", Code: "CONTENT_FILTER"}, true
	case "refusal":
		return llm.Failure{Message: "model refused to answer", Code: "REFUSAL"}, true
	default:
		code := strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(reason))
		return llm.Failure{Message: "model returned unsupported finish reason: " + reason, Code: code}, true
	}
}

// normalizeRequestError gives every adapter the same durable failure shape.
// Adapters may already return FailureError; transport-specific errors are
// classified here so a provider swap cannot silently change turn/error
// semantics. Cancellation is deliberately left untouched so it remains an
// abort and is never offered to request recovery.
func normalizeRequestError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := llm.FailureFacts(err); ok || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := err.Error()
	lower := strings.ToLower(message)
	code := "UNKNOWN"
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "api key") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication"):
		code, message = "AUTH", "API key is invalid"
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		code = "RATE_LIMIT"
	case strings.Contains(lower, "400") || strings.Contains(lower, "invalid request") || strings.Contains(lower, "bad request"):
		code = "INVALID_REQUEST"
	case strings.Contains(lower, "500") || strings.Contains(lower, "502") || strings.Contains(lower, "503") || strings.Contains(lower, "504") || strings.Contains(lower, "server error") || strings.Contains(lower, "service unavailable"):
		code = "SERVER"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "temporarily unavailable"):
		if strings.Contains(lower, "timeout") {
			code = "TIMEOUT"
		} else {
			code = "TRANSPORT"
		}
	}
	return llm.NewFailureError(llm.RedactDiagnostic(message), code, err)
}

func retryInfoOf(err error) (llm.RetryInfo, bool) {
	if err == nil {
		return nil, false
	}
	var info llm.RetryInfo
	if errors.As(err, &info) && info != nil {
		return info, true
	}
	return nil, false
}

func retryInfoReader(reader llm.StreamReader) (llm.RetryInfo, bool) {
	if reader == nil {
		return nil, false
	}
	info, ok := reader.(llm.RetryInfo)
	return info, ok && info != nil
}

// sessionRetryObserver projects provider-wrapper retries at the point they
// actually happen. The provider calls RetryScheduled before its backoff and
// RetryStarted immediately before the next wire attempt, which preserves the
// reference distinction between a scheduled retry and one whose wait
// completed. observed lets the legacy RetryInfo fallback avoid duplicating
// events for providers that have not adopted the observer seam.
type sessionRetryObserver struct {
	log      *session.Log
	turn     int
	step     int
	provider string
	model    string
	observed bool
}

func (o *sessionRetryObserver) RetryScheduled(ctx context.Context, _ llm.ChatRequest, retry llm.RetryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.observed = true
	_, err := o.log.Append(session.EventLLMRetry, session.NewLLMRetryAt(o.turn, o.step, o.provider, o.model, retry))
	return err
}

func (o *sessionRetryObserver) RetryStarted(ctx context.Context, _ llm.ChatRequest, retry llm.RetryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := o.log.Append(session.EventLLMRetryStarted, session.NewLLMRetryStarted(retry, o.turn, o.step))
	return err
}

// step performs one model request and its tool executions. It returns
// (true, nil) when the turn is complete (no tool calls requested). Pre-step
// context has already been persisted, so the request contains the system
// prompt followed by the durable derived history.
func (l *Loop) step(ctx context.Context, turnNumber, stepNumber int, entered []llm.Message, finalFinishReason *string, toolResultBoundary *runtimectx.ToolResultBoundary) (done bool, stepErr error) {
	if toolResultBoundary != nil {
		*toolResultBoundary = runtimectx.ToolResultBoundary{}
	}
	if l.metrics != nil {
		l.metrics.Step()
	}
	var stepSpan *observability.Span
	if l.tracer != nil {
		correlation, _ := runtimectx.CorrelationOf(ctx)
		stepSpan = l.tracer.Start(correlation, "agent.step", correlation.TurnID)
		defer func() { l.tracer.End(stepSpan, stepErr) }()
	}
	if _, err := l.log.Append(session.EventStepStart, session.NewStepStartAt(turnNumber, stepNumber)); err != nil {
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
		if _, err := l.log.Append(session.EventStepEnd, session.NewStepEndAt(turnNumber, stepNumber, status, errorText(stepErr))); err != nil && stepErr == nil {
			stepErr = err
			done = false
		}
	}()
	for index, input := range entered {
		if input.Role != "" && input.Role != llm.RoleUser {
			return false, fmt.Errorf("loop: entered message role %q is not user", input.Role)
		}
		if strings.TrimSpace(input.Text()) == "" && len(input.Content) == 0 {
			continue
		}
		if input.Persisted {
			continue
		}
		payload := session.NewUserMessageAt(turnNumber, stepNumber, index, input)
		if _, err := l.log.Append(session.EventUserMessage, payload); err != nil {
			return false, err
		}
	}
	history, err := l.projectedHistory()
	if err != nil {
		return false, err
	}
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
	requestCtx := runtimectx.WithCorrelation(ctx, runtimectx.Correlation{
		AgentID:   l.runtimeAgentID,
		SessionID: l.runtimeSessionID,
		TurnID:    fmt.Sprintf("turn:%d", turnNumber),
		StepID:    fmt.Sprintf("step:%d", stepNumber),
		RequestID: requestID,
	})
	request := llm.ChatRequest{Provider: l.provider, Model: l.model, MaxTokens: l.maxTokens, Temperature: l.temperature, Stop: append([]string(nil), l.stop...), ReasoningEffort: l.effort, ReasoningBudgetTokens: l.reasoningBudgetTokens, SessionID: l.runtimeSessionID, Messages: messages, Tools: specs}
	request, err = l.applyRequestHooks(requestCtx, turnNumber, stepNumber, request)
	if err != nil {
		return false, err
	}
	streamLLM := l.llm
	if l.resolveRequestLLM != nil {
		streamLLM, err = l.resolveRequestLLM(requestCtx, request)
		if err != nil {
			return false, err
		}
		if streamLLM == nil {
			return false, errors.New("loop: request resolver returned nil LLM")
		}
	}
	if request.Provider != "" {
		if err := l.appendRequestContext(request.Provider, request.Model, l.contextWindow); err != nil {
			return false, err
		}
		if _, err := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "initial")); err != nil {
			return false, err
		}
	}
	requestAttempts := 1
	var retryObserver *sessionRetryObserver
	if request.Provider != "" {
		retryObserver = &sessionRetryObserver{
			log: l.log, turn: turnNumber, step: stepNumber,
			provider: request.Provider, model: request.Model,
		}
		requestCtx = llm.WithRetryObserver(requestCtx, retryObserver)
	}
	streamRequest := func(streamCtx context.Context) (llm.StreamReader, error) {
		var requestSpan *observability.Span
		if l.tracer != nil {
			correlation, _ := runtimectx.CorrelationOf(streamCtx)
			parentID := ""
			if stepSpan != nil {
				parentID = stepSpan.ID
			}
			requestSpan = l.tracer.Start(correlation, "llm.request", parentID)
		}
		reader, streamErr := streamLLM.Stream(streamCtx, request)
		if requestSpan != nil {
			l.tracer.End(requestSpan, streamErr)
		}
		if l.metrics != nil {
			l.metrics.Request(streamErr)
		}
		return reader, streamErr
	}
	// Provider retry is deliberately executed here, at the durable request
	// boundary, rather than hidden inside Provider.Stream. This keeps each
	// attempt observable and lets cancellation/approval hooks see the same
	// request-error lifecycle as the reference harness. The standalone
	// retry.WrapProvider API remains available for compatibility callers.
	retryPolicy, hasRetryPolicy := streamLLM.(llm.RetryPolicyProvider)
	retryConfig := llmretry.Config{}
	if hasRetryPolicy {
		policy := retryPolicy.RetryPolicy()
		retryConfig = llmretry.Config{
			Mode: policy.Mode, MaxRetries: policy.MaxRetries, MaxRetriesSet: true,
			InitialBackoff: time.Duration(policy.InitialDelayMS) * time.Millisecond,
			MaxBackoff:     time.Duration(policy.MaxDelayMS) * time.Millisecond,
			JitterRatio:    policy.JitterRatio,
			RetryableCodes: append([]string(nil), policy.RetryableCodes...),
		}
	}
	retryID := ""
	scheduleProviderRetry := func(retryCtx context.Context, failed error, retryAttempt int) (bool, error) {
		if !hasRetryPolicy || request.Provider == "" {
			return false, nil
		}
		if retryID == "" {
			retryID = llmretry.NewRetryID()
		}
		event, ok := llmretry.RetryEventFor(retryConfig, retryID, retryAttempt, failed)
		if !ok {
			return false, nil
		}
		if _, err := l.log.Append(session.EventLLMRetry, session.NewLLMRetryAt(turnNumber, stepNumber, request.Provider, request.Model, event)); err != nil {
			return false, err
		}
		if err := llmretry.WaitFor(retryCtx, time.Duration(event.DelayMS)*time.Millisecond); err != nil {
			return false, err
		}
		if _, err := l.log.Append(session.EventLLMRetryStarted, session.NewLLMRetryStarted(event, turnNumber, stepNumber)); err != nil {
			return false, err
		}
		return true, nil
	}
	reader, err := streamRequest(requestCtx)
	if err != nil && l.recoverContextOverflow != nil && isContextOverflowError(err) && l.recoverContextOverflow(requestCtx) {
		// Compaction appends a surface replacement marker. Rebuild history so the
		// retry sees the folded checkpoint rather than the rejected surface.
		history, err = l.projectedHistory()
		if err != nil {
			return false, err
		}
		messages = messages[:0]
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(l.prompt.Build())}})
		messages = append(messages, history...)
		if request.Provider != "" {
			request.Messages = messages
			if _, startErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); startErr != nil {
				return false, startErr
			}
		}
		request.Messages = messages
		requestAttempts++
		reader, err = streamRequest(requestCtx)
	}
	providerRetryAttempt := 0
	for err != nil {
		// The reference always policy is a downstream recovery fallback: give
		// application request-error hooks the first chance to own the retry,
		// then schedule the provider's unbounded retry only when they decline.
		if retryConfig.Mode == "always" && len(l.requestErrorHooks) > 0 {
			retry, hookErr := l.handleRequestError(requestCtx, RequestErrorPayload{
				Turn: turnNumber, Step: stepNumber, Provider: request.Provider, Error: err,
			})
			if hookErr != nil {
				return false, hookErr
			}
			if retry {
				if cancelErr := ctx.Err(); cancelErr != nil {
					return false, fmt.Errorf("loop: cancelled: %w", cancelErr)
				}
				if request.Provider != "" {
					if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
						return false, appendErr
					}
				}
				requestAttempts++
				reader, err = streamRequest(requestCtx)
				if err != nil && ctx.Err() != nil {
					return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
				}
				continue
			}
		}
		providerRetryAttempt++
		retry, retryErr := scheduleProviderRetry(ctx, err, providerRetryAttempt)
		if retryErr != nil {
			if ctx.Err() != nil {
				return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
			}
			return false, retryErr
		}
		if !retry {
			break
		}
		if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
			return false, appendErr
		}
		requestAttempts++
		reader, err = streamRequest(requestCtx)
		if err != nil && ctx.Err() != nil {
			return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
		}
	}
	if err != nil && ctx.Err() != nil {
		return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
	}
	if err != nil {
		err = normalizeRequestError(err)
	}
	if err != nil && len(l.requestErrorHooks) > 0 {
		// A recovery listener is allowed to retry every failed request. The
		// reference agent re-enters this waterfall after each failed attempt;
		// one boolean decision must not accidentally turn that into a one-shot
		// retry budget. Provider adapters still own their own bounded transport
		// retry policy, while this loop handles explicit application recovery.
		for err != nil {
			retry, hookErr := l.handleRequestError(requestCtx, RequestErrorPayload{
				Turn: turnNumber, Step: stepNumber, Provider: request.Provider, Error: err,
			})
			if hookErr != nil {
				return false, hookErr
			}
			if !retry {
				break
			}
			// Recovery is asynchronous and may cancel the owning turn while the
			// hook is deciding. Cancellation wins over a stale retry action;
			// never issue a second provider request after the signal is closed.
			if cancelErr := ctx.Err(); cancelErr != nil {
				return false, fmt.Errorf("loop: cancelled: %w", cancelErr)
			}
			if request.Provider != "" {
				if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
					return false, appendErr
				}
			}
			requestAttempts++
			reader, err = streamRequest(requestCtx)
			if err != nil && ctx.Err() != nil {
				return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
			}
			if err != nil {
				err = normalizeRequestError(err)
			}
		}
	}
	if err != nil {
		attempts := requestAttempts
		if request.Provider != "" {
			if info, ok := retryInfoOf(err); ok {
				attempts = requestAttempts - 1 + info.Attempts()
				if retryObserver == nil || !retryObserver.observed {
					for _, retryEvent := range info.RetryEvents() {
						if _, appendErr := l.log.Append(session.EventLLMRetry, session.NewLLMRetryAt(turnNumber, stepNumber, request.Provider, request.Model, retryEvent)); appendErr != nil {
							return false, appendErr
						}
					}
				}
			}
			if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", err.Error(), llm.TokenUsage{}, attempts)); appendErr != nil {
				return false, errors.Join(err, appendErr)
			}
		}
		return false, err
	}
	attempts := requestAttempts
	if info, ok := retryInfoReader(reader); ok {
		attempts = requestAttempts - 1 + info.Attempts()
		if request.Provider != "" && (retryObserver == nil || !retryObserver.observed) {
			for _, retryEvent := range info.RetryEvents() {
				if _, appendErr := l.log.Append(session.EventLLMRetry, session.NewLLMRetryAt(turnNumber, stepNumber, request.Provider, request.Model, retryEvent)); appendErr != nil {
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
	var streamFailure *llm.Failure
	var pendingChunk strings.Builder
	var pendingChunkKind string
	var pendingChunkAt time.Time
	// A live provider stream is known even when it emits no content. Keep a
	// non-nil empty slice so the final assistant boundary preserves DSH's
	// explicit-empty provenance distinction.
	sourceEventSeqs := make([]uint64, 0)
	flushPendingChunk := func() error {
		if pendingChunk.Len() == 0 {
			return nil
		}
		var payload any
		if pendingChunkKind == session.EventAssistantReasoning {
			payload = session.NewAssistantReasoningAt(turnNumber, stepNumber, pendingChunk.String())
		} else {
			payload = session.NewAssistantChunkAt(turnNumber, stepNumber, pendingChunk.String())
		}
		event, err := l.log.Append(pendingChunkKind, payload)
		if err != nil {
			return err
		}
		sourceEventSeqs = append(sourceEventSeqs, event.Seq)
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

streamRetry:
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
					session.NewInterruptedAssistantMessageAtWithSources(turnNumber, stepNumber, text.String(), calls, reasoning, sourceEventSeqs)); aerr != nil {
					return false, aerr
				}
			}
			if l.onError != nil {
				l.onError(err)
			}
			if request.Provider != "" {
				if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", err.Error(), usage, attempts)); appendErr != nil {
					return false, errors.Join(err, appendErr)
				}
			}
			if ctx.Err() != nil {
				return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
			}
			return false, normalizeRequestError(err)
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
			streamFailure = ev.Failure
		}
	}
	if err := flushPendingChunk(); err != nil {
		return false, err
	}

	failure, failedFinish := normalizeFinishFailure(finishReason)
	if streamFailure != nil {
		failure = *streamFailure
		failedFinish = true
	}
	if failedFinish {
		requestErr := llm.NewFailureFactsError(failure, nil)
		for {
			// Always-mode recovery delegates to application hooks before its
			// provider fallback, including terminal stream failures.
			if retryConfig.Mode == "always" && len(l.requestErrorHooks) > 0 {
				retry, hookErr := l.handleRequestError(requestCtx, RequestErrorPayload{
					Turn: turnNumber, Step: stepNumber, Provider: request.Provider, Error: requestErr,
				})
				if hookErr != nil {
					return false, hookErr
				}
				if retry {
					if cancelErr := ctx.Err(); cancelErr != nil {
						return false, fmt.Errorf("loop: cancelled: %w", cancelErr)
					}
					if request.Provider != "" {
						if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", failure.Message, usage, attempts)); appendErr != nil {
							return false, appendErr
						}
						if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
							return false, appendErr
						}
					}
					attempts++
					reader, err = streamRequest(requestCtx)
					if err != nil {
						if ctx.Err() != nil {
							return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
						}
						requestErr = normalizeRequestError(err)
						if updated, ok := llm.FailureFacts(requestErr); ok {
							failure = updated
						}
						continue
					}
					text.Reset()
					reasoning = ""
					calls = nil
					finishReason = ""
					usage = llm.TokenUsage{}
					streamFailure = nil
					pendingChunk.Reset()
					pendingChunkKind = ""
					pendingChunkAt = time.Time{}
					sourceEventSeqs = make([]uint64, 0)
					goto streamRetry
				}
			}
			// A provider can report a retryable failure in its terminal stream
			// finish (EMPTY_RESPONSE, refusal/server protocol failures, etc.),
			// not only while opening the stream. Route that case through the same
			// durable retry boundary before consulting application recovery hooks.
			providerRetryAttempt++
			if retry, retryErr := scheduleProviderRetry(ctx, requestErr, providerRetryAttempt); retryErr != nil {
				if ctx.Err() != nil {
					return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
				}
				return false, retryErr
			} else if retry {
				if request.Provider != "" {
					if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", failure.Message, usage, attempts)); appendErr != nil {
						return false, appendErr
					}
					if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
						return false, appendErr
					}
				}
				attempts++
				reader, err = streamRequest(requestCtx)
				if err != nil {
					if ctx.Err() != nil {
						return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
					}
					requestErr = normalizeRequestError(err)
					if updated, ok := llm.FailureFacts(requestErr); ok {
						failure = updated
					}
					continue
				}
				// A retry is a fresh provider attempt for the same step; no
				// failed finish or its chunks may leak into the next result.
				text.Reset()
				reasoning = ""
				calls = nil
				finishReason = ""
				usage = llm.TokenUsage{}
				streamFailure = nil
				pendingChunk.Reset()
				pendingChunkKind = ""
				pendingChunkAt = time.Time{}
				sourceEventSeqs = make([]uint64, 0)
				goto streamRetry
			}
			if len(l.requestErrorHooks) == 0 {
				break
			}
			if len(l.requestErrorHooks) > 0 {
				retry, hookErr := l.handleRequestError(requestCtx, RequestErrorPayload{
					Turn: turnNumber, Step: stepNumber, Provider: request.Provider, Error: requestErr,
				})
				if hookErr != nil {
					return false, hookErr
				}
				if !retry {
					break
				}
				if cancelErr := ctx.Err(); cancelErr != nil {
					return false, fmt.Errorf("loop: cancelled: %w", cancelErr)
				}
				if request.Provider != "" {
					if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", failure.Message, usage, attempts)); appendErr != nil {
						return false, appendErr
					}
					if _, appendErr := l.log.Append(session.EventRequestHeader, session.NewRequestHeader(requestID, request, "update")); appendErr != nil {
						return false, appendErr
					}
				}
				attempts++
				reader, err = streamRequest(requestCtx)
				if err != nil {
					if ctx.Err() != nil {
						return false, fmt.Errorf("loop: cancelled: %w", ctx.Err())
					}
					requestErr = normalizeRequestError(err)
					if updated, ok := llm.FailureFacts(requestErr); ok {
						failure = updated
					}
					continue
				}
				// A retry is a fresh provider attempt for the same step; no
				// failed finish or its chunks may leak into the next result.
				text.Reset()
				reasoning = ""
				calls = nil
				finishReason = ""
				usage = llm.TokenUsage{}
				streamFailure = nil
				pendingChunk.Reset()
				pendingChunkKind = ""
				pendingChunkAt = time.Time{}
				sourceEventSeqs = make([]uint64, 0)
				goto streamRetry
			}
		}
		if request.Provider != "" {
			if _, appendErr := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "error", failure.Message, usage, attempts)); appendErr != nil {
				return false, appendErr
			}
		}
		return false, requestErr
	}

	if _, err := l.log.Append(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsageAndSources(turnNumber, stepNumber, text.String(), calls, finishReason, reasoning, usage, sourceEventSeqs)); err != nil {
		return false, err
	}
	if finalFinishReason != nil {
		// Preserve the provider step ending even when tool calls follow. The
		// turn-level sticky max-tokens latch depends on this fact; a concluding
		// tool can still replace it with its terminal marker below.
		*finalFinishReason = finishReason
	}
	if l.onUsage != nil {
		l.onUsage(request, usage)
	}
	if l.metrics != nil {
		l.metrics.Usage(usage)
	}
	if request.Provider != "" {
		if _, err := l.log.Append(session.EventLLMRequestEnd, session.NewLLMRequestEndWithUsageDetail(requestID, request.Provider, request.Model, l.effort, "completed", "", usage, attempts)); err != nil {
			return false, err
		}
	}
	if len(calls) == 0 {
		if finalFinishReason != nil {
			*finalFinishReason = finishReason
		}
		return true, nil
	}
	parentSpanID := ""
	if stepSpan != nil {
		parentSpanID = stepSpan.ID
	}
	concludesTurn, boundary, err := l.executeCalls(ctx, turnNumber, stepNumber, calls, parentSpanID)
	if err != nil {
		return false, err
	}
	if toolResultBoundary != nil {
		*toolResultBoundary = boundary
	}
	if concludesTurn {
		// A successful tool may own the terminal boundary. All calls in the
		// already-submitted batch have still been committed; no extra model step
		// is opened after that barrier.
		if finalFinishReason != nil {
			*finalFinishReason = "tool_concludes_turn"
		}
		return true, nil
	}
	return false, nil
}

// projectedHistory is the loop's model-visible history boundary. The loop
// must use the same validated projection as Web, ACP, and session-query so a
// replacement, image block, or malformed durable event cannot be interpreted
// differently by the request path than by another surface.
func (l *Loop) projectedHistory() ([]llm.Message, error) {
	if l == nil || l.log == nil {
		return nil, errors.New("loop: session projection unavailable")
	}
	snapshot, err := projection.BuildWithImageResolver(l.log.Events(), l.log.ImageResolver())
	if err != nil {
		return nil, fmt.Errorf("loop history projection: %w", err)
	}
	return snapshot.History, nil
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

func (l *Loop) executeCalls(ctx context.Context, turnNumber, stepNumber int, calls []llm.ToolCall, parentSpanID string) (bool, runtimectx.ToolResultBoundary, error) {
	concludesTurn := false
	var boundary runtimectx.ToolResultBoundary
	for next := 0; next < len(calls); {
		if err := ctx.Err(); err != nil {
			if logErr := l.appendAbortedCalls(turnNumber, stepNumber, calls[next:]); logErr != nil {
				return false, boundary, logErr
			}
			return false, boundary, fmt.Errorf("loop: cancelled: %w", err)
		}

		parsed, parseErr := tools.ParseArguments([]byte(calls[next].Arguments))
		if parseErr != nil || !l.tools.IsConcurrencySafe(calls[next].Name, parsed) || l.effectiveMaxParallelToolCalls() <= 1 {
			prepareArgs := any(parsed)
			if parseErr != nil {
				prepareArgs = []byte(calls[next].Arguments)
			}
			outcome, err := l.startToolCall(ctx, turnNumber, stepNumber, calls[next], prepareArgs, parentSpanID)
			if err != nil {
				return false, boundary, err
			}
			resultSeq, err := l.commitToolOutcome(turnNumber, stepNumber, outcome)
			if err != nil {
				return false, boundary, err
			}
			boundary.Sequence = resultSeq
			concludesTurn = concludesTurn || outcome.res.ConcludesTurn
			next++
			continue
		}
		groupConcludes, groupBoundary, err := l.executeParallelGroup(ctx, turnNumber, stepNumber, calls, &next, parentSpanID)
		if err != nil {
			return false, boundary, err
		}
		if groupBoundary.Sequence > boundary.Sequence {
			boundary.Sequence = groupBoundary.Sequence
		}
		concludesTurn = concludesTurn || groupConcludes
	}
	return concludesTurn, boundary, nil
}

type settledToolCall struct {
	index   int
	outcome toolCallOutcome
}

// executeParallelGroup keeps the next unstarted call as a live barrier. This
// is important when a completed tool replaces or disables a later tool: dsh
// reclassifies that later call before starting it, rather than using a stale
// group classification.
func (l *Loop) executeParallelGroup(ctx context.Context, turnNumber, stepNumber int, calls []llm.ToolCall, next *int, parentSpanID string) (bool, runtimectx.ToolResultBoundary, error) {
	start := *next
	committed := start
	started := start
	ready := make(map[int]toolCallOutcome)
	running := make(map[int]struct{})
	concludesTurn := false
	var boundary runtimectx.ToolResultBoundary
	settled := make(chan settledToolCall, len(calls)-start)

	commitReady := func() error {
		for committed < started {
			outcome, ok := ready[committed]
			if !ok {
				return nil
			}
			resultSeq, err := l.commitToolOutcome(turnNumber, stepNumber, outcome)
			if err != nil {
				return err
			}
			boundary.Sequence = resultSeq
			concludesTurn = concludesTurn || outcome.res.ConcludesTurn
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
		for ctx.Err() == nil && *next < len(calls) && len(running)+len(ready) < l.effectiveMaxParallelToolCalls() {
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
				// Parallel calls still carry the same durable execution identity as
				// serial calls. In particular, runtime event sinks and nested
				// providers must see the addressed session plus turn/step/call
				// correlation rather than a bare parent context.
				callCtx := runtimectx.WithCorrelation(ctx, runtimectx.Correlation{
					AgentID: l.runtimeAgentID, SessionID: l.runtimeSessionID,
					TurnID: fmt.Sprintf("turn:%d", turnNumber), StepID: fmt.Sprintf("step:%d", stepNumber),
					CallID: modelCall.ID,
				})
				var toolSpan *observability.Span
				if l.tracer != nil {
					correlation, _ := runtimectx.CorrelationOf(callCtx)
					toolSpan = l.tracer.Start(correlation, "tool."+modelCall.Name, parentSpanID)
				}
				result, execErr := l.tools.ExecutePrepared(callCtx, execution)
				if toolSpan != nil {
					l.tracer.End(toolSpan, toolMetricError(result, execErr))
				}
				if l.metrics != nil {
					l.metrics.Tool(toolMetricError(result, execErr))
				}
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
			return false, boundary, err
		}
		if err := commitReady(); err != nil {
			drain()
			return false, boundary, err
		}
		if ctx.Err() != nil {
			drain()
			if err := commitReady(); err != nil {
				return false, boundary, err
			}
			if err := l.appendAbortedCalls(turnNumber, stepNumber, calls[*next:]); err != nil {
				return false, boundary, err
			}
			return false, boundary, fmt.Errorf("loop: cancelled: %w", ctx.Err())
		}
		if len(running) == 0 {
			return concludesTurn, boundary, nil
		}
		settledCall := <-settled
		delete(running, settledCall.index)
		ready[settledCall.index] = settledCall.outcome
	}
}

func (l *Loop) startToolCall(ctx context.Context, turnNumber, stepNumber int, call llm.ToolCall, parsed any, parentSpanID string) (toolCallOutcome, error) {
	callEvent, err := l.log.Append(session.EventToolCall,
		session.NewToolCall(turnNumber, stepNumber, call.ID, call.Name, call.Arguments))
	if err != nil {
		return toolCallOutcome{}, err
	}
	callCtx := runtimectx.WithCorrelation(ctx, runtimectx.Correlation{
		AgentID:   l.runtimeAgentID,
		SessionID: l.runtimeSessionID,
		TurnID:    fmt.Sprintf("turn:%d", turnNumber),
		StepID:    fmt.Sprintf("step:%d", stepNumber),
		CallID:    call.ID,
	})
	prepared, err := l.tools.Prepare(callCtx, call.ID, call.Name, parsed)
	if err != nil {
		if l.metrics != nil {
			l.metrics.Tool(err)
		}
		return toolCallOutcome{call: call, callSeq: callEvent.Seq, err: err}, nil
	}
	var toolSpan *observability.Span
	if l.tracer != nil {
		correlation, _ := runtimectx.CorrelationOf(callCtx)
		toolSpan = l.tracer.Start(correlation, "tool."+call.Name, parentSpanID)
	}
	res, err := l.tools.ExecutePrepared(callCtx, prepared)
	if toolSpan != nil {
		l.tracer.End(toolSpan, toolMetricError(res, err))
	}
	if l.metrics != nil {
		l.metrics.Tool(toolMetricError(res, err))
	}
	return toolCallOutcome{call: call, callSeq: callEvent.Seq, res: res, err: err}, nil
}

// toolMetricError preserves the distinction between transport/executor errors
// and a normalized structured ToolResult failure. Both are failed tool calls
// for observability, even though only the former travels through Go's error
// return. Keeping this decision in one helper also makes serial and parallel
// execution report the same counters.
func toolMetricError(result tools.ToolResult, err error) error {
	if err != nil {
		return err
	}
	if !result.IsError {
		return nil
	}
	if result.Error != nil {
		return &tools.ExecutionError{Info: *result.Error, Message: result.Output}
	}
	return errors.New("structured tool result reported an error")
}

func (l *Loop) commitToolOutcome(turnNumber, stepNumber int, outcome toolCallOutcome) (uint64, error) {
	if outcome.err != nil {
		info := tools.ErrorInfoOf(outcome.err)
		result, err := l.log.Append(session.EventToolResult,
			session.NewToolErrorAtCodeWithSource(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.err.Error(), info.Code, outcome.callSeq))
		if err != nil {
			return 0, err
		}
		return result.Seq, l.appendToolAdditionalContexts(outcome.res)
	}
	var spill *session.SpillRef
	if outcome.res.SpillPath != "" {
		spill = &session.SpillRef{
			Locator: outcome.res.SpillPath, Bytes: outcome.res.SpillBytes,
			RetrievalHint: "Retrieve the full tool output from this locator when the preview is insufficient.",
			Source:        &session.SpillSource{ToolName: outcome.call.Name, CallID: outcome.call.ID, Label: outcome.call.Name},
		}
	}
	var payload any
	if outcome.res.IsError {
		code := "TOOL_RESULT_ERROR"
		if outcome.res.Error != nil && outcome.res.Error.Code != "" {
			code = outcome.res.Error.Code
		}
		if len(outcome.res.Content) > 0 {
			payload = session.NewToolErrorResultWithContentAtCodeWithSourceMeta(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, outcome.res.Content, code, outcome.callSeq, outcome.res.Meta)
		} else {
			payload = session.NewToolErrorResultAtCodeWithSourceMeta(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, spill, code, outcome.callSeq, outcome.res.Meta)
		}
	} else {
		payload = session.NewToolResultAtWithSourceMeta(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, spill, outcome.callSeq, outcome.res.Meta)
		if len(outcome.res.Content) > 0 {
			payload = session.NewToolResultWithContentAtSourceMeta(turnNumber, stepNumber, outcome.call.ID, outcome.call.Name, outcome.res.Output, outcome.res.Content, outcome.callSeq, outcome.res.Meta)
		}
	}
	result, err := l.log.Append(session.EventToolResult, payload)
	if err != nil {
		return 0, err
	}
	// additionalContexts are deliberately appended only after the tool result.
	// This is the observable boundary used by the reference loop: nested Code
	// Mode/plugin context is neither visible while a tool is in flight nor
	// lost when the outer result is an error.
	return result.Seq, l.appendToolAdditionalContexts(outcome.res)
}

func (l *Loop) appendToolAdditionalContexts(result tools.ToolResult) error {
	contexts := make([]llm.Message, 0, len(result.AdditionalContextMessages)+len(result.AdditionalContexts))
	for _, message := range result.AdditionalContextMessages {
		message.Role = llm.RoleUser
		message.Content = append([]llm.ContentBlock(nil), message.Content...)
		if message.SourceKind == "" {
			message.SourceKind = "plugin"
		}
		contexts = append(contexts, message)
	}
	// Keep the old string representation observable for compatibility callers.
	// New producers should send rich messages so source attribution and content
	// blocks survive the boundary.
	for _, text := range result.AdditionalContexts {
		if text == "" {
			continue
		}
		contexts = append(contexts, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(text)}, SourceKind: "plugin"})
	}
	for _, message := range contexts {
		if len(message.Content) == 0 && message.Text() == "" {
			continue
		}
		if _, err := l.log.Append(session.EventUserMessage,
			session.NewContextMessageFromLLM(message)); err != nil {
			return err
		}
	}
	return nil
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

// appendRequestContext records the reference request/context last-wins slot
// only when the resolved route or advertised capacity changes. It is kept
// separate from request/header because context pressure uses the former while
// request reconstruction uses the latter.
func (l *Loop) appendRequestContext(provider, model string, contextWindow int) error {
	if l.log == nil || provider == "" || model == "" {
		return nil
	}
	previous := l.log.Events()
	for i := len(previous) - 1; i >= 0; i-- {
		if previous[i].Type != session.EventRequestContext {
			continue
		}
		var data struct {
			Provider      string `json:"provider"`
			Model         string `json:"model"`
			ContextWindow int    `json:"contextWindow,omitempty"`
		}
		if json.Unmarshal(previous[i].Data, &data) == nil && data.Provider == provider && data.Model == model && data.ContextWindow == contextWindow {
			return nil
		}
		break
	}
	_, err := l.log.Append(session.EventRequestContext, session.NewRequestContext(provider, model, contextWindow))
	return err
}
