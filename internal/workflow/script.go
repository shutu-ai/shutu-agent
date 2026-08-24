package workflow

import "context"

// ScriptRequest is the dsh-shaped workflow request. Meta and Args are plain
// data; Script is the model-authored JavaScript body.
type ScriptRequest struct {
	Meta            map[string]any
	Script          string
	Args            any
	ParentSessionID string
}

// AgentRequest is the host-side projection of a JavaScript agent() call.
// Provider/model are optional routing hints; the composition root decides
// whether the selected subagent backend supports them.
type AgentRequest struct {
	Prompt   string
	Label    string
	Phase    string
	Provider string
	Model    string
	Schema   map[string]any
}

// AgentResult is the JSON-safe result returned to a workflow script.
type AgentResult struct {
	ID         string
	Output     string
	StopReason string
	Structured any
}

// AgentStart is the host capability exposed to the external Node runtime.
type AgentStart func(context.Context, AgentRequest) (AgentResult, error)

// ScriptEvent is an observe-only workflow lifecycle event. The event type is
// intentionally a string so external providers can preserve dsh vocabulary.
type ScriptEvent struct {
	Type string
	Data any
}

// ScriptRunner executes one JavaScript workflow and returns only JSON-safe
// data. Implementations must settle when ctx is cancelled.
type ScriptRunner interface {
	RunScript(context.Context, ScriptRequest, AgentStart, func(ScriptEvent)) (ScriptResult, error)
}

// ScriptResult is the terminal result of a JavaScript workflow.
type ScriptResult struct {
	Value         any
	StopReason    string
	Error         string
	AgentsStarted int
}
