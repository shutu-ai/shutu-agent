package webserver

// DSH native session projection. The persisted Shutu log deliberately keeps
// its own compact event payloads; the native client consumes DSH's message and
// stream envelopes. This adapter is the single conversion point used by both
// history replay and live mux delivery.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
)

type nativeProjectionCursor struct {
	turn      int
	step      int
	provider  string
	model     string
	requestID string
	surface   []uint64
}

func newNativeProjectionCursor() *nativeProjectionCursor {
	return &nativeProjectionCursor{turn: -1}
}

func (c *nativeProjectionCursor) project(sessionID string, ev session.Event) nativeSessionEvent {
	data := nativeJSONObject(ev.Data)
	projectedType := ev.Type
	projectedData := data
	ignorable := false
	var surfaceOp any
	var sourceEventSeqs []uint64

	switch ev.Type {
	case session.EventTurnStart:
		c.turn = nativeEventInt(data, "turn", c.turn+1)
		c.step = 0
		projectedData = map[string]any{"turn": c.turn}
	case session.EventTurnEnd:
		turn := nativeEventInt(data, "turn", c.turn)
		projectedData = map[string]any{
			"turn":   turn,
			"reason": nativeTurnEndReason(data),
		}
	case session.EventStepStart:
		c.step = nativeEventInt(data, "step", c.step)
		projectedData = map[string]any{"turn": c.turn, "step": c.step}
	case session.EventStepEnd:
		step := nativeEventInt(data, "step", c.step)
		projectedData = map[string]any{"turn": c.turn, "step": step}
	case session.EventLLMRequestStart:
		c.provider = nativeEventString(data, "provider")
		c.model = nativeEventString(data, "model")
		c.requestID = nativeEventString(data, "requestId", "request_id")
		projectedType = "request/context"
		projectedData = map[string]any{
			"provider": c.provider,
			"model":    c.model,
		}
		if contextWindow := nativeEventNumber(data, "contextWindow", "context_window"); contextWindow > 0 {
			projectedData["contextWindow"] = contextWindow
		}
	case session.EventLLMRequestEnd:
		// The DSH client has no required request-end event. Keep the diagnostic
		// row replayable without making an unknown event a reconstruction error.
		ignorable = true
	case session.EventAssistantChunk, session.EventAssistantReasoning:
		chunkType := "text-delta"
		if ev.Type == session.EventAssistantReasoning {
			chunkType = "reasoning-delta"
		}
		text := nativeEventString(data, "text")
		projectedType = session.EventAssistantChunk
		projectedData = map[string]any{
			"turn": c.turn,
			"step": c.step,
			"chunk": map[string]any{
				"type":  chunkType,
				"index": 0,
				"text":  text,
			},
		}
	case session.EventAssistantMessage:
		projectedData = c.assistantMessageData(sessionID, ev.Seq, data)
	case session.EventUserMessage:
		projectedData = nativeUserMessageData(sessionID, ev.Seq, data)
	case session.EventToolCall, "tool/start":
		projectedType = session.EventToolCall
		projectedData = map[string]any{
			"turn":      nativeEventInt(data, "turn", c.turn),
			"step":      nativeEventInt(data, "step", c.step),
			"callId":    nativeEventString(data, "callId", "call_id"),
			"name":      nativeEventString(data, "name"),
			"arguments": nativeEventString(data, "arguments", "args", "tool_args"),
		}
	case session.EventToolResult, "tool/error":
		projectedType = session.EventToolResult
		projectedData = c.toolResultData(sessionID, ev.Seq, data, ev.Type == "tool/error")
	case session.EventLLMRetry:
		projectedData = c.retryData(sessionID, ev.Seq, data)
	case session.EventCompactionStart, session.EventCompactionSummary,
		session.EventCompactionEnd, session.EventCompactionPrune:
		ignorable = true
	}
	if projectedType == session.EventUserMessage || projectedType == session.EventAssistantMessage || projectedType == session.EventToolResult {
		surfaceOp, sourceEventSeqs = c.projectSurface(ev.Seq, data)
	}
	if !nativeDSHEventType(projectedType) {
		ignorable = true
	}

	result := nativeSessionEvent{
		Seq:  ev.Seq,
		Type: projectedType,
		Time: ev.At.UnixMilli(),
		Data: nativeJSONBytes(projectedData),
	}
	if ignorable {
		result.Ignorable = true
	}
	if projectedType == session.EventUserMessage || projectedType == session.EventAssistantMessage || projectedType == session.EventToolResult {
		result.SurfaceOp = surfaceOp
		result.SourceEventSeqs = sourceEventSeqs
	}
	return result
}

func nativeUserMessageData(sessionID string, seq uint64, data map[string]any) map[string]any {
	content := nativeMessageContent(nativeEventValue(data, "content"))
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": nativeEventString(data, "text")}}
	}
	source := nativeEventObject(data, "source")
	if source == nil {
		source = map[string]any{"kind": "user"}
	}
	if nativeEventString(source, "kind") == "" {
		source["kind"] = "user"
	}
	return map[string]any{
		"id":      nativeMessageID(sessionID, seq),
		"role":    "user",
		"content": content,
		"source":  source,
	}
}

func (c *nativeProjectionCursor) assistantMessageData(sessionID string, seq uint64, data map[string]any) map[string]any {
	content := make([]any, 0, 2)
	if reasoning := nativeEventString(data, "reasoning"); reasoning != "" {
		content = append(content, map[string]any{"type": "reasoning", "text": reasoning})
	}
	if text := nativeEventString(data, "text"); text != "" || len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range nativeEventArray(data, "toolCalls", "tool_calls") {
		content = append(content, map[string]any{
			"type":      "tool-call",
			"id":        nativeEventString(call, "id"),
			"name":      nativeEventString(call, "name"),
			"arguments": nativeEventString(call, "arguments", "args"),
		})
	}
	message := map[string]any{
		"id":      nativeMessageID(sessionID, seq),
		"role":    "assistant",
		"content": content,
		"source": map[string]any{
			"kind":     "model",
			"provider": c.provider,
			"model":    c.model,
		},
	}
	if message["source"].(map[string]any)["provider"] == "" {
		message["source"].(map[string]any)["provider"] = "shutu"
	}
	if message["source"].(map[string]any)["model"] == "" {
		message["source"].(map[string]any)["model"] = "unknown"
	}
	result := map[string]any{
		"turn":    nativeEventInt(data, "turn", c.turn),
		"step":    nativeEventInt(data, "step", c.step),
		"message": message,
	}
	if usage := nativeEventValue(data, "usage"); usage != nil {
		result["usage"] = usage
	}
	if nativeEventBool(data, "interrupted") {
		result["interrupted"] = true
	}
	return result
}

func (c *nativeProjectionCursor) toolResultData(sessionID string, seq uint64, data map[string]any, forcedError bool) map[string]any {
	callID := nativeEventString(data, "callId", "call_id")
	if message := nativeEventObject(data, "message"); message != nil {
		if source := nativeEventObject(message, "source"); source != nil && callID == "" {
			callID = nativeEventString(source, "callId", "call_id")
		}
	}
	if callID == "" {
		callID = fmt.Sprintf("call:%d", seq)
	}
	name := nativeEventString(data, "name", "toolName", "tool_name")
	output := nativeEventString(data, "output", "toolOutput", "tool_output")
	if output == "" {
		output = nativeEventString(data, "error")
	}
	errorData := nativeEventObject(data, "error")
	isError := forcedError || errorData != nil || nativeEventBool(data, "isError", "is_error")
	blocks := nativeToolResultBlocks(data, output, isError)
	message := map[string]any{
		"id":      nativeMessageID(sessionID, seq),
		"role":    "user",
		"content": []any{map[string]any{"type": "tool-result", "toolCallId": callID, "content": blocks, "isError": isError}},
		"source":  map[string]any{"kind": "tool", "callId": callID},
	}
	result := map[string]any{
		"turn":    nativeEventInt(data, "turn", c.turn),
		"step":    nativeEventInt(data, "step", c.step),
		"message": message,
		"callId":  callID,
		"name":    name,
	}
	if errorData != nil {
		result["error"] = errorData
	} else if forcedError {
		result["error"] = map[string]any{"name": "ToolError", "code": "TOOL_ERROR"}
	}
	return result
}

func nativeToolResultBlocks(data map[string]any, fallback string, isError bool) []any {
	if message := nativeEventObject(data, "message"); message != nil {
		if raw := nativeEventValue(message, "content"); raw != nil {
			if blocks := nativeMessageContent(raw); len(blocks) > 0 {
				return blocks
			}
		}
	}
	if raw := nativeEventValue(data, "content"); raw != nil {
		if blocks := nativeMessageContent(raw); len(blocks) > 0 {
			return blocks
		}
	}
	if fallback == "" && isError {
		fallback = "Tool execution failed"
	}
	return []any{map[string]any{"type": "text", "text": fallback}}
}

func (c *nativeProjectionCursor) retryData(sessionID string, seq uint64, data map[string]any) map[string]any {
	retry := nativeEventInt(data, "retry", nativeEventInt(data, "attempt", 1))
	result := map[string]any{
		"retryId":    fmt.Sprintf("%s:retry:%d:%d", sessionID, nativeEventInt(data, "turn", c.turn), retry),
		"turn":       nativeEventInt(data, "turn", c.turn),
		"step":       nativeEventInt(data, "step", c.step),
		"provider":   nativeEventString(data, "provider"),
		"mode":       nativeEventString(data, "mode"),
		"policyKey":  nativeEventString(data, "policyKey", "policy_key"),
		"retry":      retry,
		"maxRetries": nativeEventInt(data, "maxRetries", 0),
		"delayMs":    nativeEventInt(data, "delayMs", 0),
	}
	if failure := nativeEventObject(data, "failure"); failure != nil {
		result["failure"] = failure
	} else if message := nativeEventString(data, "error"); message != "" {
		result["failure"] = map[string]any{"code": "UNKNOWN", "message": message}
	}
	return result
}

func nativeMessageContent(raw any) []any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ := nativeEventString(object, "type", "kind")
		switch typ {
		case "text", "reasoning":
			result = append(result, map[string]any{"type": typ, "text": nativeEventString(object, "text")})
		case "image":
			image := nativeEventObject(object, "image")
			if image == nil {
				image = object
			}
			result = append(result, map[string]any{
				"type": "image",
				"attachment": map[string]any{
					"attachmentId": nativeEventString(image, "id", "attachmentId", "attachment_id"),
					"mediaType":    nativeEventString(image, "mediaType", "media_type"),
					"bytes":        nativeEventNumber(image, "bytes"),
					"width":        nativeEventNumber(image, "width"),
					"height":       nativeEventNumber(image, "height"),
				},
			})
		case "tool-call":
			result = append(result, map[string]any{
				"type": "tool-call", "id": nativeEventString(object, "id", "callId"),
				"name": nativeEventString(object, "name"), "arguments": nativeEventString(object, "arguments", "args"),
			})
		}
	}
	return result
}

func nativeTurnEndReason(data map[string]any) map[string]any {
	status := strings.ToLower(nativeEventString(data, "status", "state"))
	switch status {
	case "cancelled", "canceled", "aborted":
		return map[string]any{"kind": "aborted", "reason": map[string]any{"kind": "legacy"}}
	case "blocked":
		return map[string]any{"kind": "blocked"}
	case "max-tokens", "max_tokens":
		return map[string]any{"kind": "max-tokens"}
	case "interrupted":
		return map[string]any{"kind": "interrupted"}
	case "failed", "error":
		message := nativeEventString(data, "error", "message")
		return map[string]any{"kind": "error", "error": map[string]any{"message": message, "code": "UNKNOWN"}}
	default:
		return map[string]any{"kind": "completed"}
	}
}

func nativeMessageID(sessionID string, seq uint64) string {
	return fmt.Sprintf("shutu:%s:message:%d", sessionID, seq)
}

func nativeJSONObject(raw json.RawMessage) map[string]any {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return map[string]any{}
	}
	return value
}

func nativeJSONBytes(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func nativeEventValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	for actual, value := range object {
		for _, key := range keys {
			if strings.EqualFold(actual, key) {
				return value
			}
		}
	}
	return nil
}

func nativeEventObject(object map[string]any, keys ...string) map[string]any {
	value, _ := nativeEventValue(object, keys...).(map[string]any)
	return value
}

func nativeEventArray(object map[string]any, keys ...string) []map[string]any {
	value, _ := nativeEventValue(object, keys...).([]any)
	result := make([]map[string]any, 0, len(value))
	for _, item := range value {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func nativeEventString(object map[string]any, keys ...string) string {
	value, _ := nativeEventValue(object, keys...).(string)
	return value
}

func nativeEventNumber(object map[string]any, keys ...string) int {
	value := nativeEventValue(object, keys...)
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	}
	return 0
}

func nativeEventInt(object map[string]any, key string, fallback int) int {
	value := nativeEventValue(object, key)
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	}
	return fallback
}

func nativeEventBool(object map[string]any, keys ...string) bool {
	value, _ := nativeEventValue(object, keys...).(bool)
	return value
}

func nativeSourceEventSeqs(data map[string]any) []uint64 {
	value, _ := nativeEventValue(data, "sourceEventSeqs", "source_event_seqs").([]any)
	result := make([]uint64, 0, len(value))
	for _, item := range value {
		if number, ok := item.(float64); ok && number >= 0 {
			result = append(result, uint64(number))
		}
	}
	return result
}

func nativeSurfaceOp(data map[string]any) any {
	if value := nativeEventValue(data, "surfaceOp", "surface_op"); value != nil {
		return value
	}
	return nil
}

func (c *nativeProjectionCursor) projectSurface(seq uint64, data map[string]any) (any, []uint64) {
	raw := nativeEventValue(data, "surfaceOp", "surface_op")
	if raw == nil {
		c.surface = append(c.surface, seq)
		return "append", nativeSourceEventSeqs(data)
	}
	replace, ok := raw.(map[string]any)
	if !ok || nativeEventString(replace, "op") != "replace" {
		c.surface = append(c.surface, seq)
		return "append", nativeSourceEventSeqs(data)
	}
	start := nativeEventInt(replace, "start", -1)
	end := nativeEventInt(replace, "end", -1)
	first := -1
	last := -1
	for index, node := range c.surface {
		if node < uint64(start) || node > uint64(end) {
			continue
		}
		if first == -1 {
			first = index
		}
		last = index
	}
	if first == -1 || last == -1 {
		// Preserve an already-normalized operation for a truncated page. The
		// caller may be projecting a window that does not include its sources;
		// the full replay path supplies those sources before this branch.
		return raw, nativeSourceEventSeqs(data)
	}
	shadowed := append([]uint64(nil), c.surface[first:last+1]...)
	next := make([]uint64, 0, len(c.surface)-len(shadowed)+1)
	next = append(next, c.surface[:first]...)
	next = append(next, seq)
	next = append(next, c.surface[last+1:]...)
	c.surface = next
	return map[string]any{"op": "replace", "start": shadowed[0], "end": shadowed[len(shadowed)-1]}, shadowed
}

func nativeDSHEventType(typ string) bool {
	switch typ {
	case "assistant/chunk", "assistant/message", "compaction/end", "compaction/prune", "compaction/start",
		"compaction/summary", "feedback/record", "goal/change", "llm/retry", "permission/preset", "plan/mode",
		"request/context", "request/header", "schedule/change", "session/end-seed", "session/title",
		"session/title-llm-request", "step/end", "step/start", "subagent/descriptor", "todo/write", "tool/call",
		"tool/result", "turn/end", "turn/start", "user/message":
		return true
	default:
		return false
	}
}
