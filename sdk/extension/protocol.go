package extension

import "encoding/json"

// JSON-RPC 2.0 method names for Extension Protocol v1. Methods may be carried
// over newline-delimited stdio JSON or an HTTP endpoint that accepts one RPC
// request and returns one RPC response.
const (
	MethodInitialize     = "initialize"
	MethodShutdown       = "shutdown"
	MethodHealth         = "health"
	MethodProvideContext = "context/provide"
	MethodCallTool       = "tool/call"
	MethodEvent          = "event"
)

// EventVersion is independent of the Agent's durable vocabulary: it is the
// wire contract shown to external processes.
const EventVersion = 1

// Stable observational event types. Payloads contain identifiers, counts and
// outcomes only; message bodies, tool arguments and tool output never cross.
const (
	EventTurnStarted        = "turn.started"
	EventTurnCompleted      = "turn.completed"
	EventStepStarted        = "step.started"
	EventStepCompleted      = "step.completed"
	EventToolStarted        = "tool.started"
	EventToolCompleted      = "tool.completed"
	EventToolFailed         = "tool.failed"
	EventContextRequested   = "context.requested"
	EventContextInjected    = "context.injected"
	EventExtensionStarted   = "extension.started"
	EventExtensionRestarted = "extension.restarted"
	EventExtensionStopped   = "extension.stopped"
)

var SupportedEventTypes = []string{
	EventTurnStarted, EventTurnCompleted,
	EventStepStarted, EventStepCompleted,
	EventToolStarted, EventToolCompleted, EventToolFailed,
	EventContextRequested, EventContextInjected,
	EventExtensionStarted, EventExtensionRestarted, EventExtensionStopped,
}

func ValidEventType(eventType string) bool {
	for _, supported := range SupportedEventTypes {
		if eventType == supported {
			return true
		}
	}
	return false
}

type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e RPCError) Error() string { return e.Message }

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type InitializeRequest struct {
	ProtocolVersion       string       `json:"protocolVersion"`
	AgentAPIVersion       string       `json:"agentApiVersion"`
	AgentName             string       `json:"agentName"`
	AgentVersion          string       `json:"agentVersion"`
	GrantedPermissions    []string     `json:"grantedPermissions,omitempty"`
	SupportedCapabilities Capabilities `json:"supportedCapabilities"`
	SupportedEventTypes   []string     `json:"supportedEventTypes,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion     string           `json:"protocolVersion"`
	ExtensionAPIVersion string           `json:"extensionApiVersion"`
	Capabilities        Capabilities     `json:"capabilities"`
	Tools               []ToolDefinition `json:"tools,omitempty"`
	WebBaseURL          string           `json:"webBaseUrl,omitempty"`
	Restartable         bool             `json:"restartable,omitempty"`
}

type HealthResult struct {
	Ready  bool   `json:"ready"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type ContextRequest struct {
	SessionID string            `json:"sessionId,omitempty"`
	TurnID    string            `json:"turnId,omitempty"`
	StepID    string            `json:"stepId,omitempty"`
	Step      int               `json:"step,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	UserInput string            `json:"userInput,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type ContextContribution struct {
	Source          string            `json:"source"`
	Content         string            `json:"content"`
	Priority        int               `json:"priority,omitempty"`
	EstimatedTokens int               `json:"estimatedTokens,omitempty"`
	Truncatable     bool              `json:"truncatable,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type ContextResult struct {
	Contributions []ContextContribution `json:"contributions,omitempty"`
}

type ToolCallRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	CallID    string         `json:"callId,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
}

type ToolCallResult struct {
	// Value is lossless JSON. A string is rendered directly; other values are
	// canonical JSON, then pass through the Agent's normal output cap and audit.
	Value any    `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type Event struct {
	Type       string         `json:"type"`
	Version    int            `json:"version"`
	EventID    string         `json:"eventId,omitempty"`
	SessionID  string         `json:"sessionId,omitempty"`
	TurnID     string         `json:"turnId,omitempty"`
	StepID     string         `json:"stepId,omitempty"`
	Step       int            `json:"step,omitempty"`
	OccurredAt string         `json:"occurredAt,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}
