// Package runtimectx carries session-owned execution identity through tool
// contexts without creating a dependency cycle with the tool registry.
package runtimectx

import "context"

type toolResultsKey struct{}

// ToolResultBoundary identifies the durable tool-result batch that has just
// committed. The zero value means "not at a tool-result boundary".
type ToolResultBoundary struct {
	Sequence uint64
}

func (boundary ToolResultBoundary) Present() bool {
	return boundary.Sequence != 0
}

type key struct{}

// Correlation carries the stable execution identities visible to tools,
// adapters and observability sinks. Empty fields are valid at boundaries where
// that identity has not been allocated yet; callers must not reconstruct these
// values by parsing human-facing event text.
type Correlation struct {
	AgentID      string
	SessionID    string
	TurnID       string
	StepID       string
	RequestID    string
	CallID       string
	GenerationID string
}

// Runtime is the per-run identity and durable event sink.
type Runtime struct {
	SessionID string
	Emit      func(string, any) error
	Trace     Correlation
}

func With(ctx context.Context, runtime Runtime) context.Context {
	return context.WithValue(ctx, key{}, runtime)
}

func Get(ctx context.Context) (Runtime, bool) {
	if ctx == nil {
		return Runtime{}, false
	}
	runtime, ok := ctx.Value(key{}).(Runtime)
	return runtime, ok
}

func SessionID(ctx context.Context) string {
	runtime, _ := Get(ctx)
	return runtime.SessionID
}

// WithCorrelation returns a child context carrying the supplied execution
// identities while preserving the session-owned event sink.
func WithCorrelation(ctx context.Context, correlation Correlation) context.Context {
	runtime, _ := Get(ctx)
	previous := runtime.Trace
	if correlation.AgentID == "" {
		correlation.AgentID = previous.AgentID
	}
	if correlation.SessionID == "" {
		correlation.SessionID = previous.SessionID
		if correlation.SessionID == "" {
			correlation.SessionID = runtime.SessionID
		}
	}
	if correlation.TurnID == "" {
		correlation.TurnID = previous.TurnID
	}
	if correlation.StepID == "" {
		correlation.StepID = previous.StepID
	}
	if correlation.RequestID == "" {
		correlation.RequestID = previous.RequestID
	}
	if correlation.CallID == "" {
		correlation.CallID = previous.CallID
	}
	if correlation.GenerationID == "" {
		correlation.GenerationID = previous.GenerationID
	}
	runtime.SessionID = correlation.SessionID
	runtime.Trace = correlation
	return With(ctx, runtime)
}

// CorrelationOf returns the current execution identities, if any.
func CorrelationOf(ctx context.Context) (Correlation, bool) {
	runtime, ok := Get(ctx)
	if !ok {
		return Correlation{}, false
	}
	correlation := runtime.Trace
	if correlation.SessionID == "" {
		correlation.SessionID = runtime.SessionID
	}
	return correlation, true
}

// WithToolResultsComplete marks the boundary after a step's tool calls have
// committed and another model request is about to run. Context strategies use
// this authoritative loop signal instead of inferring it from step numbers.
func WithToolResultsComplete(ctx context.Context, boundary ToolResultBoundary) context.Context {
	return context.WithValue(ctx, toolResultsKey{}, boundary)
}

// ToolResultsComplete returns the post-tool, pre-next-model-call boundary.
func ToolResultsComplete(ctx context.Context) (ToolResultBoundary, bool) {
	boundary, _ := ctx.Value(toolResultsKey{}).(ToolResultBoundary)
	return boundary, boundary.Present()
}

func Emit(ctx context.Context, typ string, data any) error {
	runtime, ok := Get(ctx)
	if !ok || runtime.Emit == nil {
		return nil
	}
	return runtime.Emit(typ, data)
}
