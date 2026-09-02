// Package llm defines the LLM adapter contract and the streaming event
// protocol shared by every provider. SSE streaming is a hard requirement from
// day one (D6): Stream returns an incremental reader, never a whole-response
// blob.
package llm

import (
	"context"
	"errors"
	"fmt"
)

// Failure is the provider-neutral, durable failure fact carried by a model
// request.  Keeping the code separate from the human message lets the loop
// and protocol projections preserve the same error class across retries,
// cancellation and cold replay.
type Failure struct {
	Message              string `json:"message"`
	Code                 string `json:"code"`
	Status               int    `json:"status,omitempty"`
	ProviderRetryAfterMS int64  `json:"providerRetryAfterMs,omitempty"`
	RequestID            string `json:"requestId,omitempty"`
}

// FailureError wraps a provider or runtime error without losing its stable
// failure facts.  It is intentionally compatible with errors.Is/errors.As.
type FailureError struct {
	Facts Failure
	Cause error
}

func (e *FailureError) Error() string {
	if e == nil {
		return ""
	}
	if e.Facts.Message != "" {
		return RedactDiagnostic(e.Facts.Message)
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("%s failure", e.Facts.Code)
}

func (e *FailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// FailureFacts returns the durable facts when err carries them.
func FailureFacts(err error) (Failure, bool) {
	var typed *FailureError
	if err == nil || !errors.As(err, &typed) || typed == nil {
		return Failure{}, false
	}
	return redactFailure(typed.Facts), true
}

// NewFailureError constructs a failure that remains inspectable after the
// request has crossed provider and loop boundaries.
func NewFailureError(message, code string, cause error) error {
	return &FailureError{Facts: redactFailure(Failure{Message: message, Code: code}), Cause: cause}
}

// NewFailureFactsError preserves the complete provider-neutral failure facts
// (status, retry-after and request id included) across loop recovery seams.
func NewFailureFactsError(facts Failure, cause error) error {
	return &FailureError{Facts: redactFailure(facts), Cause: cause}
}

func redactFailure(failure Failure) Failure {
	failure.Message = RedactDiagnostic(failure.Message)
	return failure
}

// TokenUsage is provider-neutral token accounting attached to a completed stream.
type TokenUsage struct {
	InputTokens     int `json:"inputTokens,omitempty"`
	OutputTokens    int `json:"outputTokens,omitempty"`
	TotalTokens     int `json:"totalTokens,omitempty"`
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
	// CacheReadTokens and CacheWriteTokens are disjoint prompt accounting
	// buckets. InputTokens is the uncached prompt bucket when these fields are
	// populated. CachedInputTokens is retained as a legacy aggregate for old
	// persisted events and adapters; new provider code should use the explicit
	// buckets so read and write traffic cannot be conflated.
	CacheReadTokens   int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens  int `json:"cacheWriteTokens,omitempty"`
	CachedInputTokens int `json:"cachedInputTokens,omitempty"`
}

func (u TokenUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.ReasoningTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.CachedInputTokens == 0
}

type RetryEvent struct {
	RetryID    string
	Attempt    int
	MaxRetries int
	DelayMS    int64
	Error      string
	Mode       string
	PolicyKey  string
	Failure    *Failure
}

// RetryObserver receives request-level retry lifecycle transitions while a
// provider is still inside Stream. The observer is carried by the request
// context so adapters can publish a durable scheduled transition before a
// cancellable backoff and a started transition immediately before the next
// wire attempt. Implementations must be synchronous: returning an error
// aborts the retry rather than silently losing the lifecycle record.
type RetryObserver interface {
	RetryScheduled(context.Context, ChatRequest, RetryEvent) error
	RetryStarted(context.Context, ChatRequest, RetryEvent) error
}

type retryObserverContextKey struct{}

// WithRetryObserver binds an observer to one request context. It is intended
// for the agent loop; direct provider callers remain single-attempt and have
// no durable session projection.
func WithRetryObserver(ctx context.Context, observer RetryObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, retryObserverContextKey{}, observer)
}

func RetryObserverFromContext(ctx context.Context) (RetryObserver, bool) {
	observer, ok := ctx.Value(retryObserverContextKey{}).(RetryObserver)
	return observer, ok && observer != nil
}

// Role is a provider-neutral conversation role, mirroring the OpenAI wire
// vocabulary used by the DeepSeek chat completions API.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolSchema declares one callable tool to the model.
type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema of the arguments
}

// ChatRequest is a single model request: conversation history plus the tools
// the model may call on this step.
type ChatRequest struct {
	// Provider selects the provider to route to; empty means the default
	// (decided by the composition root). The loop never sets it — it calls the
	// injected provider's Stream with the default — so this is reserved for
	// future explicit routing (M8-2b / tool-layer direct provider calls,
	// dispatch-m8-2 §2).
	Provider string
	Model    string
	// MaxTokens is an optional per-request output budget. Zero leaves the
	// provider default in control; a positive value is serialized using the
	// exact field required by the selected wire protocol.
	MaxTokens int
	// Temperature is optional so an explicit zero remains distinguishable from
	// an omitted setting. Providers that do not support it may ignore it only
	// when their protocol contract says so.
	Temperature *float64
	// Stop contains provider-neutral stop sequences. Empty means omitted.
	Stop []string
	// ReasoningEffort is the selected thinking effort ("off" | "low" | "high" |
	// "max"; dsh ModelSelect 思考强度 对齐). Empty keeps the provider default.
	// Each protocol adapter serializes the value using its own wire shape.
	ReasoningEffort string
	// ReasoningBudgetTokens optionally supplies a provider-specific thinking
	// budget. Zero uses the adapter/reference default for the selected effort.
	ReasoningBudgetTokens int
	// SessionID is the durable harness session identity associated with this
	// provider request. Providers may use it for request attribution; it is not
	// part of the model-visible prompt.
	SessionID string
	// Purpose identifies an internal request variant such as "compaction".
	// Empty means a normal user-facing model request.
	Purpose  string
	Messages []Message
	Tools    []ToolSchema
}

// StreamEventKind discriminates StreamEvent values.
type StreamEventKind int

const (
	// StreamTextDelta carries an incremental piece of assistant text.
	StreamTextDelta StreamEventKind = iota
	// StreamReasoningDelta carries an incremental piece of the assistant's
	// reasoning text (M8: DeepSeek streams reasoning_content deltas in
	// parallel with content deltas).
	StreamReasoningDelta
	// StreamFinish marks the end of the stream with the final finish reason,
	// the complete accumulated tool calls (arguments already joined), and the
	// accumulated reasoning text.
	StreamFinish
)

// StreamEvent is one element read from a model stream.
type StreamEvent struct {
	Kind         StreamEventKind
	Text         string     // StreamTextDelta / StreamReasoningDelta
	Reasoning    string     // StreamFinish: accumulated reasoning text
	FinishReason string     // StreamFinish: stop | tool_calls | ...
	ToolCalls    []ToolCall // StreamFinish: complete calls in model order
	Usage        TokenUsage // StreamFinish: provider usage, when available
	Failure      *Failure   // StreamFinish: provider-normalized stream failure
}

// StreamReader yields StreamEvents until io.EOF.
type StreamReader interface {
	Next() (StreamEvent, error)
}

// RetryInfo is optionally implemented by a reader returned from the shared retry wrapper.
type RetryInfo interface {
	Attempts() int
	RetryEvents() []RetryEvent
}

// LLM is the adapter interface every provider implements.
type LLM interface {
	// Stream starts a chat request and returns an incremental reader. The
	// returned reader must honor ctx cancellation.
	Stream(ctx context.Context, req ChatRequest) (StreamReader, error)
}
