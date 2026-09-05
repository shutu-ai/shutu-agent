package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// The native protocol is deliberately fail-closed for an event that could
// change reconstruction. Informational extensions must carry ignorable:true.
var (
	ErrUnknownRequiredEvent = errors.New("session: unknown required event")
	ErrMalformedWireEvent   = errors.New("session: malformed wire event")
	ErrUnsupportedEvent     = errors.New("session: unsupported legacy event")
)

// ValidateEventVocabulary rejects event encodings that this runtime no longer
// has a lossless migration for. The check is shared by live appends and cold
// replay so an obsolete request header cannot enter the log through one path
// and fail only after restart.
func ValidateEventVocabulary(typ string, raw json.RawMessage) error {
	switch typ {
	case "request/header-delta":
		return fmt.Errorf("%w: request/header-delta", ErrUnsupportedEvent)
	case "mode/set":
		return fmt.Errorf("%w: mode/set", ErrUnsupportedEvent)
	case EventRequestHeader:
		var data struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &data); err == nil && data.Reason == "fallback" {
			return fmt.Errorf("%w: request/header reason %q", ErrUnsupportedEvent, data.Reason)
		}
	case EventSessionTitle:
		var data struct {
			Title       string `json:"title"`
			MessageSeqs []any  `json:"messageSeqs"`
			Source      struct {
				Kind     string `json:"kind"`
				Provider string `json:"provider"`
			} `json:"source"`
		}
		if err := json.Unmarshal(raw, &data); err != nil || strings.TrimSpace(data.Title) == "" {
			return fmt.Errorf("%w: session/title requires a non-empty title", ErrMalformedWireEvent)
		}
		switch data.Source.Kind {
		case "fallback", "user":
			return nil
		case "provider":
			if strings.TrimSpace(data.Source.Provider) != "" {
				return nil
			}
			return fmt.Errorf("%w: session/title provider source is missing provider", ErrMalformedWireEvent)
		default:
			return fmt.Errorf("%w: session/title has invalid source", ErrMalformedWireEvent)
		}
	}
	return nil
}

// validateLogEventVocabulary is the stricter counterpart used by the durable
// event log.  ValidateEventVocabulary intentionally remains usable for the
// native wire envelope, where an extension may be accepted when it carries
// ignorable:true.  A durable log has no such flag in its Append API: letting
// an unknown type in would create a replay event that current projections do
// not understand and would silently advance the durable cursor.  Known
// internal/legacy log-only events are listed here explicitly; new events must
// extend this vocabulary before they can be persisted.
func validateLogEventVocabulary(typ string, raw json.RawMessage) error {
	if err := ValidateEventVocabulary(typ, raw); err != nil {
		return err
	}
	if !knownLogEventType(typ) {
		return fmt.Errorf("%w: %s", ErrUnknownRequiredEvent, typ)
	}
	return nil
}

// ValidateDurableEvent is the public admission boundary for append-only
// stores.  Native wire validation may allow an explicitly ignorable extension,
// but a durable event has no equivalent escape hatch: every persisted type
// must be known to the replay/projection vocabulary.
func ValidateDurableEvent(typ string, raw json.RawMessage) error {
	return validateLogEventVocabulary(typ, raw)
}

func knownLogEventType(typ string) bool {
	switch typ {
	case EventTurnStart, EventTurnEnd, EventStepStart, EventStepEnd,
		EventRequestHeader, EventRequestContext, EventSteeringMessage, EventTodoWrite,
		EventLLMRequestEnd, EventLLMRetry, EventLLMRetryStarted,
		EventUserMessage, EventAssistantChunk, EventAssistantReasoning, EventAssistantMessage,
		EventSessionTitle, EventToolCall, EventToolResult, EventFeedbackRecord,
		EventWebCommandResult, EventWebSearchLLMRequest, EventCommandRun, EventCommandDone,
		EventJobStart, EventJobStatus, EventJobDone,
		EventTerminalStart, EventTerminalStop, EventEvalRun, EventRalphRun,
		EventWorkflowRun, EventWorkflowStart, EventWorkflowPhase, EventWorkflowLog,
		EventWorkflowAgentStart, EventWorkflowAgentEnd, EventWorkflowEnd,
		EventToolWorkflowRunStart, EventToolWorkflowAgentStart,
		EventToolWorkflowAgentEnd, EventToolWorkflowRunEnd,
		EventSubagentStart, EventSubagentEnd, EventSubagentReport,
		EventGoalRoundStart, EventGoalRoundEnd,
		EventCompactionStart, EventCompactionSummary, EventCompactionEnd, EventCompactionPrune,
		EventSkillCatalog, EventSkillLoad,
		EventScheduleCreate, EventScheduleList, EventScheduleDelete, EventScheduleFire,
		EventPlanCreate, EventPlanUpdate, EventPlanDelete, EventPlanStatus, EventPlanList, EventPlanMode,
		EventSpillWrite, EventSpillRecall, EventSpillList, EventSpillDelete,
		EventInteractRequest, EventInteractResolve, EventInteractCancel, EventInteractDeny, EventInteractStatus,
		EventApprovalAsked, EventApprovalDecided, EventApprovalPolicy, EventPermissionPreset, EventSandboxMode,
		EventAgentInboxSpliced, EventTeamSnapshot, EventTeamMember, EventTeamTask,
		EventTeamMessageQueued, EventTeamMessageDelivered,
		EventCodeRun, EventCodeDispatchStart, EventCodeDispatch,
		EventMcpList, EventMcpCall, EventFsRead, EventFsWrite, EventFsList,
		EventScheduleChange,
		// These literals are retained for replay of pre-alignment logs. New
		// code emits the canonical aliases above.
		"tool/start", "tool/error", "llm/request_start":
		return true
	default:
		return false
	}
}

// ValidateWireEvent validates the DSH session-event envelope emitted by the
// native/Web projection. It is intentionally independent of session.Log's
// legacy JSONL record format (whose timestamp is an RFC3339 string).
func ValidateWireEvent(raw []byte) error {
	var envelope struct {
		Type      string          `json:"type"`
		Seq       *uint64         `json:"seq"`
		Time      *int64          `json:"time"`
		Data      json.RawMessage `json:"data"`
		Ignorable *bool           `json:"ignorable"`
		SurfaceOp json.RawMessage `json:"surfaceOp"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: envelope JSON: %v", ErrMalformedWireEvent, err)
	}
	if envelope.Type == "" || envelope.Seq == nil || envelope.Time == nil || envelope.Data == nil {
		return fmt.Errorf("%w: type, seq, time and data are required", ErrMalformedWireEvent)
	}
	if err := ValidateEventVocabulary(envelope.Type, envelope.Data); err != nil {
		return err
	}
	if !knownWireEventType(envelope.Type) {
		if envelope.Ignorable == nil || !*envelope.Ignorable {
			return fmt.Errorf("%w: %s", ErrUnknownRequiredEvent, envelope.Type)
		}
		return nil
	}
	if envelope.Type == EventUserMessage || envelope.Type == EventAssistantMessage || envelope.Type == EventToolResult {
		if envelope.SurfaceOp == nil {
			return fmt.Errorf("%w: %s surfaceOp is required", ErrMalformedWireEvent, envelope.Type)
		}
	}
	if err := validateWireEventData(envelope.Type, envelope.Data); err != nil {
		return err
	}
	return nil
}

func knownWireEventType(typ string) bool {
	switch typ {
	case EventTurnStart, EventTurnEnd, EventStepStart, EventStepEnd,
		EventUserMessage, EventAssistantChunk, EventAssistantMessage,
		EventToolCall, EventToolResult, EventRequestHeader, EventRequestContext, EventTodoWrite,
		EventLLMRetry, EventLLMRetryStarted:
		return true
	case EventApprovalAsked, EventApprovalDecided, EventApprovalPolicy, EventPermissionPreset, EventSandboxMode:
		return true
	case EventAgentInboxSpliced:
		return true
	case EventCommandRun, EventCommandDone:
		return true
	case EventFeedbackRecord, EventPlanCreate, EventPlanUpdate, EventPlanDelete,
		EventPlanStatus, EventPlanList, EventPlanMode:
		return true
	case EventSessionTitle:
		return true
	default:
		return false
	}
}

func validateWireEventData(typ string, raw json.RawMessage) error {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("%w: %s data must be an object", ErrMalformedWireEvent, typ)
	}
	require := func(fields ...string) error {
		for _, field := range fields {
			if _, ok := object[field]; !ok {
				return fmt.Errorf("%w: %s data missing %q", ErrMalformedWireEvent, typ, field)
			}
		}
		return nil
	}
	if typ == EventTurnStart || typ == EventTurnEnd || typ == EventStepStart || typ == EventStepEnd ||
		typ == EventAssistantChunk || typ == EventAssistantMessage || typ == EventToolCall || typ == EventToolResult {
		if err := require("turn"); err != nil {
			return err
		}
	}
	if typ == EventStepStart || typ == EventStepEnd || typ == EventAssistantChunk || typ == EventAssistantMessage || typ == EventToolCall || typ == EventToolResult {
		if err := require("step"); err != nil {
			return err
		}
	}
	switch typ {
	case EventAgentInboxSpliced:
		if err := require("target", "start", "inserted"); err != nil {
			return err
		}
	case EventTurnEnd:
		if err := require("turn", "reason"); err != nil {
			return err
		}
	case EventUserMessage:
		if err := require("role", "content", "source"); err != nil {
			return err
		}
	case EventAssistantChunk:
		if err := require("chunk"); err != nil {
			return err
		}
	case EventAssistantMessage:
		if err := require("message"); err != nil {
			return err
		}
	case EventToolCall:
		if err := require("callId", "name", "arguments"); err != nil {
			return err
		}
	case EventToolResult:
		if err := require("message"); err != nil {
			return err
		}
	case EventRequestHeader:
		if err := require("header", "reason"); err != nil {
			return err
		}
	case EventRequestContext:
		if err := require("provider", "model"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["provider"]) || !wireNonEmptyString(object["model"]) {
			return fmt.Errorf("%w: %s provider and model must be non-empty", ErrMalformedWireEvent, typ)
		}
		if value, present := object["contextWindow"]; present && !wirePositiveInt(value) {
			return fmt.Errorf("%w: %s contextWindow must be a positive integer", ErrMalformedWireEvent, typ)
		}
	case EventTodoWrite:
		if err := require("todos"); err != nil {
			return err
		}
		if todos, ok := object["todos"].([]any); !ok || todos == nil {
			return fmt.Errorf("%w: %s todos must be an array", ErrMalformedWireEvent, typ)
		}
	case EventFeedbackRecord:
		if err := require("text"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["text"]) {
			return fmt.Errorf("%w: %s text must be non-empty", ErrMalformedWireEvent, typ)
		}
	case EventPlanCreate:
		if err := require("scope", "id", "title"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["scope"]) || !wireNonEmptyString(object["id"]) || !wireNonEmptyString(object["title"]) {
			return fmt.Errorf("%w: %s scope, id and title must be non-empty", ErrMalformedWireEvent, typ)
		}
	case EventPlanUpdate, EventPlanDelete:
		if err := require("scope", "id"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["scope"]) || !wireNonEmptyString(object["id"]) {
			return fmt.Errorf("%w: %s scope and id must be non-empty", ErrMalformedWireEvent, typ)
		}
	case EventPlanStatus:
		if err := require("scope", "id", "status"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["scope"]) || !wireNonEmptyString(object["id"]) || !wireNonEmptyString(object["status"]) {
			return fmt.Errorf("%w: %s scope, id and status must be non-empty", ErrMalformedWireEvent, typ)
		}
	case EventPlanList:
		if err := require("count"); err != nil {
			return err
		}
		if !wireNonNegativeInt(object["count"]) {
			return fmt.Errorf("%w: %s count must be a non-negative integer", ErrMalformedWireEvent, typ)
		}
	case EventPlanMode:
		if err := require("active"); err != nil {
			return err
		}
		if _, ok := object["active"].(bool); !ok {
			return fmt.Errorf("%w: %s active must be boolean", ErrMalformedWireEvent, typ)
		}
	case EventLLMRetry:
		if err := require("retryId", "turn", "step", "provider", "mode", "policyKey", "retry", "delayMs", "failure"); err != nil {
			return err
		}
		if err := validateWireRetryData(object); err != nil {
			return err
		}
	case EventLLMRetryStarted:
		if err := require("retryId", "turn", "step", "retry"); err != nil {
			return err
		}
		if !wireNonEmptyString(object["retryId"]) || !wirePositiveInt(object["turn"]) ||
			!wirePositiveInt(object["step"]) || !wirePositiveInt(object["retry"]) {
			return fmt.Errorf("%w: %s retry-started has invalid retryId/turn/step/retry", ErrMalformedWireEvent, typ)
		}
	case EventApprovalAsked:
		if err := require("id", "toolName"); err != nil {
			return err
		}
	case EventApprovalDecided:
		if err := require("id", "outcome"); err != nil {
			return err
		}
	case EventApprovalPolicy:
		if err := require("policy"); err != nil {
			return err
		}
	case EventPermissionPreset:
		if err := require("preset"); err != nil {
			return err
		}
	case EventSandboxMode:
		if err := require("mode"); err != nil {
			return err
		}
	case EventCommandRun:
		if err := require("commandId", "name", "source"); err != nil {
			return err
		}
	case EventCommandDone:
		if err := require("commandId", "kind"); err != nil {
			return err
		}
	}
	return nil
}

func validateWireRetryData(object map[string]any) error {
	if !wireNonEmptyString(object["retryId"]) || !wirePositiveInt(object["turn"]) ||
		!wirePositiveInt(object["step"]) || !wireNonEmptyString(object["provider"]) ||
		!wireNonEmptyString(object["policyKey"]) || !wirePositiveInt(object["retry"]) {
		return fmt.Errorf("%w: llm/retry has invalid identity or coordinates", ErrMalformedWireEvent)
	}
	mode, _ := object["mode"].(string)
	if mode != "normal" && mode != "always" {
		return fmt.Errorf("%w: llm/retry mode must be normal or always", ErrMalformedWireEvent)
	}
	if !wireNonNegativeInt(object["delayMs"]) || object["delayMs"].(float64) > 2147483647 {
		return fmt.Errorf("%w: llm/retry delayMs is outside 0..2147483647", ErrMalformedWireEvent)
	}
	failure, ok := object["failure"].(map[string]any)
	if !ok || !wireNonEmptyString(failure["message"]) || !wireNonEmptyString(failure["code"]) {
		return fmt.Errorf("%w: llm/retry failure requires non-empty message and code", ErrMalformedWireEvent)
	}
	if status, present := failure["status"]; present && !wireStatus(status) {
		return fmt.Errorf("%w: llm/retry failure status must be an integer in 100..599", ErrMalformedWireEvent)
	}
	if retryAfter, present := failure["providerRetryAfterMs"]; present && !wirePositiveNumber(retryAfter) {
		return fmt.Errorf("%w: llm/retry failure providerRetryAfterMs must be positive", ErrMalformedWireEvent)
	}
	if requestID, present := failure["requestId"]; present && !wireNonEmptyString(requestID) {
		return fmt.Errorf("%w: llm/retry failure requestId must be non-empty", ErrMalformedWireEvent)
	}
	if mode == "normal" {
		max, ok := object["maxRetries"]
		if !ok || !wirePositiveInt(max) || object["retry"].(float64) > max.(float64) {
			return fmt.Errorf("%w: llm/retry normal mode requires maxRetries >= retry", ErrMalformedWireEvent)
		}
	} else if _, ok := object["maxRetries"]; ok {
		return fmt.Errorf("%w: llm/retry always mode must omit maxRetries", ErrMalformedWireEvent)
	}
	return nil
}

func wirePositiveInt(value any) bool {
	n, ok := value.(float64)
	return ok && n == math.Trunc(n) && n > 0 && n <= float64(maxInt())
}

func wireNonNegativeInt(value any) bool {
	n, ok := value.(float64)
	return ok && n == math.Trunc(n) && n >= 0 && n <= float64(math.MaxInt64)
}

func wireNonEmptyString(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

func wireStatus(value any) bool {
	n, ok := value.(float64)
	return ok && n == math.Trunc(n) && n >= 100 && n <= 599
}

func wirePositiveNumber(value any) bool {
	n, ok := value.(float64)
	return ok && !math.IsInf(n, 0) && !math.IsNaN(n) && n > 0
}
