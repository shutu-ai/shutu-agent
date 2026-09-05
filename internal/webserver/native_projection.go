package webserver

// DSH native session projection. The persisted Shutu log deliberately keeps
// its own compact event payloads; the native client consumes DSH's message and
// stream envelopes. This adapter is the single conversion point used by both
// history replay and live mux delivery.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/shutu-ai/shutu-agent/internal/meter"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

// Version 2 invalidates checkpoints written by the old session.list path,
// which could fold only the bounded tail while labelling the result with the
// complete session revision. Those rows are structurally valid but
// semantically incomplete and must never be reused after upgrade.
const nativeProjectionCacheVersion = 2

type nativeProjectionCachePayload struct {
	Block   nativeProjectionBlock `json:"block"`
	Surface map[string]any        `json:"surface"`
}

// nativeProjectionCheckpointEvent bounds write-behind traffic to events that
// can materially change the durable conversation/state projection. Streaming
// chunks still reach the live client immediately; they do not force one
// SQLite write per token. A later history request remains the authoritative
// repair path when a checkpoint intentionally lags a non-checkpoint event.
func nativeProjectionCheckpointEvent(eventType string) bool {
	switch eventType {
	case session.EventTurnEnd, session.EventUserMessage, session.EventAssistantMessage,
		session.EventToolResult, session.EventLLMRequestEnd, session.EventCompactionEnd,
		session.EventPlanCreate, session.EventPlanUpdate, session.EventPlanStatus,
		session.EventPlanDelete, session.EventPlanMode, session.EventSessionTitle, "todo/write",
		"permission/preset", "sandbox/mode":
		return true
	default:
		return false
	}
}

// nativeCachedProjection reads only an exact-revision checkpoint. A stale
// row is deliberately ignored and a schema mismatch is invalidated; the
// durable event log remains the authority and the caller will rebuild it.
func (s *Server) nativeCachedProjection(ctx context.Context, sessionID string, revision uint64) (*nativeProjectionCachePayload, bool) {
	cache, ok := s.store.(store.SessionProjectionCacheStore)
	if !ok || revision == 0 {
		return nil, false
	}
	row, err := cache.GetProjectionCache(ctx, sessionID)
	if err != nil {
		return nil, false
	}
	if row.SessionID == "" {
		return nil, false
	}
	if row.Version != nativeProjectionCacheVersion || row.Revision != revision {
		if row.Version != nativeProjectionCacheVersion {
			_ = cache.DeleteProjectionCache(context.Background(), sessionID)
		}
		return nil, false
	}
	var payload nativeProjectionCachePayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil || payload.Block.Values == nil {
		_ = cache.DeleteProjectionCache(context.Background(), sessionID)
		return nil, false
	}
	return &payload, true
}

// saveNativeProjection is fail-soft by design. A lost cache write costs a
// longer replay on the next read but must never turn a committed session or
// Web request into a failure.
func (s *Server) saveNativeProjection(ctx context.Context, sessionID string, revision int64, block nativeProjectionBlock, surface map[string]any) {
	cache, ok := s.store.(store.SessionProjectionCacheStore)
	if !ok || revision < 0 {
		return
	}
	payload, err := json.Marshal(nativeProjectionCachePayload{Block: block, Surface: surface})
	if err != nil {
		return
	}
	_ = cache.PutProjectionCache(ctx, store.ProjectionCacheRow{
		SessionID: sessionID, Version: nativeProjectionCacheVersion, Revision: uint64(revision), Payload: payload,
	})
}

type nativeProjectionCursor struct {
	turn              int
	step              int
	provider          string
	model             string
	requestID         string
	requestHeaderSeen bool
	surface           []uint64
	surfaceGeneration int
	values            map[string]any
	changed           map[string]any
	usage             nativeProjectionTokenUsage
	usageSeen         bool
	todos             []map[string]any
	todosSeen         bool
	stats             nativeProjectionSessionStats
	goal              map[string]any
	subagent          nativeProjectionSubagentState
	context           nativeProjectionContextBreakdown
	completion        meter.CompletionProjection
	list              nativeProjectionSessionListMetadata
	canonical         *projection.Cursor
}

func newNativeProjectionCursor() *nativeProjectionCursor {
	canonical, _ := projection.NewCursor(nil)
	values := make(map[string]any)
	stats := &nativeProjectionSessionStats{}
	values["sessionStats"] = nativeSessionStatsValue(stats)
	values["tokenUsage"] = map[string]any{
		"uncachedInputTokens": int64(0), "outputTokens": int64(0),
		"cacheReadTokens": int64(0), "cacheWriteTokens": int64(0),
	}
	values["contextPressure"] = map[string]any{}
	values["completionLedger"] = map[string]any{
		"assistantTokens": int64(0), "reasoningTokens": int64(0),
		"toolCallTokens": int64(0), "toolResultTokens": int64(0),
		"attachmentBytes": int64(0),
	}
	values["goal"] = nil
	values["plan"] = map[string]any{"active": false, "pending": false}
	return &nativeProjectionCursor{
		turn: -1, values: values, changed: make(map[string]any), stats: *stats,
		list: session.NewSessionListMetadata(), canonical: canonical,
	}
}

// setContextWindow seeds the native context-meter projection with the
// effective route capacity. DSH keeps this denominator independent from the
// provider usage sample, so history replay and live mux delivery must both
// receive it before folding the session log.
func (c *nativeProjectionCursor) setContextWindow(window int) {
	if window <= 0 {
		return
	}
	context := nativeProjectionMap(c.values["contextPressure"])
	context["contextWindow"] = window
	c.setProjectionValue("contextPressure", context)
}

func (c *nativeProjectionCursor) project(sessionID string, ev session.Event) nativeSessionEvent {
	data := nativeJSONObject(ev.Data)
	canonicalAccepted := false
	if c.canonical != nil {
		// The native adapter still owns DSH wire normalization, but the shared
		// projection owns the durable surface list used by reconnect snapshots.
		// A bounded/truncated page may begin mid-stream or contain a legacy
		// alias; keep the adapter fallback for that transport-only case.
		if nativeSurfaceOp(data) != nil && ev.Type != session.EventUserMessage {
			// Current durable compaction markers are user/message events. Older
			// native fixtures may carry a replacement marker on an assistant
			// row; that adapter-only form is intentionally not admitted into the
			// canonical model projection.
			c.canonical = nil
		} else if err := c.canonical.Append(ev); err != nil {
			c.canonical = nil
		} else {
			canonicalAccepted = true
		}
	}
	c.changed = make(map[string]any)
	c.foldProjection(ev, data)
	if canonicalAccepted {
		// Keep live control frames on the same canonical state used by the
		// reconnect checkpoint. The native adapter still decides wire shape,
		// but it no longer decides the value for these shared fields.
		c.syncCanonicalControlChanges(ev)
	}
	projectedType := ev.Type
	projectedData := data
	ignorable := false
	var surfaceOp any
	var sourceEventSeqs []uint64

	switch ev.Type {
	case session.EventTurnStart:
		// The cursor starts at -1 as an internal "no active turn" sentinel,
		// while DSH's wire contract is one-based.  Using c.turn+1 directly
		// would therefore expose the first turn as 0 and make the following
		// tool events appear to belong to a different turn in the live UI.
		nextTurn := c.turn + 1
		if c.turn < 0 {
			nextTurn = 1
		}
		c.turn = nativeNonNegative(nativeEventInt(data, "turn", nextTurn))
		c.step = 0
		projectedData = map[string]any{"turn": c.turn}
	case session.EventTurnEnd:
		turn := nativeNonNegative(nativeEventInt(data, "turn", c.turn))
		projectedData = map[string]any{
			"turn":   turn,
			"reason": nativeTurnEndReason(data),
		}
	case session.EventStepStart:
		c.step = nativeNonNegative(nativeEventInt(data, "step", c.step))
		projectedData = map[string]any{"turn": nativeNonNegative(c.turn), "step": c.step}
	case session.EventStepEnd:
		step := nativeNonNegative(nativeEventInt(data, "step", c.step))
		projectedData = map[string]any{"turn": nativeNonNegative(c.turn), "step": step}
	case session.EventRequestHeader, "llm/request_start":
		c.provider = nativeEventString(data, "provider")
		c.model = nativeEventString(data, "model")
		c.requestID = nativeEventString(data, "requestId", "request_id")
		reason := "initial"
		if c.requestHeaderSeen {
			reason = "update"
		}
		if explicit := nativeEventString(data, "reason"); explicit != "" {
			reason = explicit
		}
		header := nativeEventObject(data, "header")
		if header == nil {
			header = nativeLLMRequestHeader(data)
		}
		projectedType = "request/header"
		projectedData = map[string]any{
			"header": header,
			"reason": reason,
		}
		c.requestHeaderSeen = true
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
		if text == "" {
			// The canonical replay fixture and SDK/provider adapters may carry
			// the chunk envelope under data.chunk.  Accept both wire forms so
			// native history does not silently erase a real delta.
			text = nativeEventString(nativeEventObject(data, "chunk"), "text")
		}
		projectedType = session.EventAssistantChunk
		projectedData = map[string]any{
			"turn": nativeNonNegative(c.turn),
			"step": nativeNonNegative(c.step),
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
			"turn":      nativeNonNegative(nativeEventInt(data, "turn", c.turn)),
			"step":      nativeNonNegative(nativeEventInt(data, "step", c.step)),
			"callId":    nativeEventString(data, "callId", "call_id"),
			"name":      nativeEventString(data, "name"),
			"arguments": nativeEventString(data, "arguments", "args", "tool_args"),
		}
	case session.EventToolResult, "tool/error":
		projectedType = session.EventToolResult
		projectedData = c.toolResultData(sessionID, ev.Seq, data, ev.Type == "tool/error")
	case session.EventLLMRetry:
		projectedData = c.retryData(sessionID, ev.Seq, data)
	case session.EventLLMRetryStarted:
		projectedData = map[string]any{
			"retryId": nativeEventString(data, "retryId", "retry_id"),
			"turn":    nativeNonNegative(nativeEventInt(data, "turn", c.turn)),
			"step":    nativeNonNegative(nativeEventInt(data, "step", c.step)),
			"retry":   nativeEventInt(data, "retry", nativeEventInt(data, "attempt", 1)),
		}
	case session.EventCompactionStart, session.EventCompactionSummary,
		session.EventCompactionEnd, session.EventCompactionPrune:
		ignorable = true
	case session.EventAgentInboxSpliced:
		// Older durable removal splices can carry Go's nil-slice null. DSH's
		// browser replay spreads `inserted`, so normalize the legacy wire form
		// without rewriting the durable log.
		if projectedData["inserted"] == nil {
			projectedData["inserted"] = []any{}
		}
	case session.EventCommandRun, session.EventCommandDone:
		// Command lifecycle facts are already in the canonical dsh shape and
		// remain outside model history.
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

// nativeProjectionBlock is the DSH history-tail projection contract. The
// values are deliberately derived from the same ordered source log as the
// events, so a reconnect can atomically replace both the conversation page
// and its UI state.
type nativeProjectionBlock struct {
	AsOfSeq int64          `json:"asOfSeq"`
	Values  map[string]any `json:"values"`
}

type nativeProjectionTokenUsage struct {
	UncachedInputTokens int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
}

type nativeProjectionSessionStats struct {
	Turns        int64
	Steps        int64
	LLMMs        int64
	ToolMs       int64
	TTFTMs       int64
	TTFTSteps    int64
	DecodeMs     int64
	DecodeTokens int64
	LastTurn     int
	HasLastTurn  bool
	OpenStep     *nativeProjectionOpenStep
	PendingTools map[string]int64
}

type nativeProjectionOpenStep struct {
	Turn          int
	Step          int
	StartTime     int64
	FirstTokenAt  int64
	HasFirstToken bool
}

// nativeProjectionSubagentState mirrors the two DSH projections maintained by
// the subagent package. A Shutu child log currently starts with
// subagent/start, while the adapter also accepts the canonical
// subagent/descriptor event for native sessions.
type nativeProjectionSubagentState struct {
	descriptorSeen      bool
	pendingTurnStart    int64
	hasPendingTurnStart bool
	activeSince         int64
	activeThrough       int64
	active              bool
	settledMs           int64
}

type nativeProjectionContextBreakdown struct {
	systemTokens  int64
	toolsTokens   int64
	messageTokens int64
	claim         *nativeProjectionContextClaim
}

type nativeProjectionContextClaim struct {
	start  uint64
	end    uint64
	tokens int64
}

type nativeProjectionSessionListMetadata = session.SessionListMetadata

func (c *nativeProjectionCursor) foldProjection(ev session.Event, data map[string]any) {
	c.foldSessionStats(ev, data)
	c.foldSubagent(ev, data)
	c.foldContextBreakdown(ev, data)
	c.foldSessionListMetadata(ev, data)
	c.foldCompletion(ev)
	switch ev.Type {
	case session.EventSessionTitle:
		c.setProjectionValue("title", nativeEventString(data, "title", "text"))
	case "todo/write":
		if todos, ok := nativeTodoItems(nativeEventValue(data, "items", "todos")); ok {
			c.todos = todos
			c.todosSeen = true
			c.setProjectionValue("todos", nativeTodoValues(c.todos))
		}
	case session.EventPlanCreate:
		switch nativeEventString(data, "scope") {
		case "goal":
			c.createGoalProjection(ev, data)
		case "todo":
			c.upsertTodo(data)
		}
	case session.EventPlanUpdate:
		switch nativeEventString(data, "scope") {
		case "goal":
			c.updateGoalProjection(ev, data)
		case "todo":
			c.upsertTodo(data)
		}
	case session.EventPlanStatus:
		switch nativeEventString(data, "scope") {
		case "goal":
			c.updateGoalStatus(ev, data)
		case "todo":
			c.updateTodoStatus(nativeEventString(data, "id"), nativeEventString(data, "status"))
		}
	case session.EventPlanDelete:
		switch nativeEventString(data, "scope") {
		case "goal":
			c.setProjectionValue("goal", nil)
			c.goal = nil
		case "todo":
			c.deleteTodo(nativeEventString(data, "id"))
		}
	case session.EventGoalRoundStart:
		c.updateGoalRounds(ev, data)
	case session.EventGoalRoundEnd:
		c.updateGoalStatus(ev, data)
	case "permission/preset":
		c.setProjectionValue("permissions", nativePermissionProjection(nativeEventString(data, "currentValue", "current", "permission", "preset")))
	case "sandbox/mode":
		mode := nativeEventString(data, "mode")
		permission := "standard"
		if mode == "read-only" || mode == "readonly" {
			permission = "readonly"
		} else if mode == "danger-full-access" {
			permission = "full"
		}
		c.setProjectionValue("permissions", nativePermissionProjection(permission))
	case session.EventPlanMode:
		c.setProjectionValue("plan", map[string]any{
			"active":  nativeEventBool(data, "active"),
			"pending": nativeEventBool(data, "pending"),
		})
	case session.EventRequestHeader, "llm/request_start":
		if contextWindow := nativeEventInt64(data, "contextWindow", "context_window"); contextWindow > 0 {
			context := nativeProjectionMap(c.values["contextPressure"])
			context["contextWindow"] = contextWindow
			c.setProjectionValue("contextPressure", context)
		}
	case session.EventLLMRequestEnd:
		if usage := nativeEventObject(data, "usage"); usage != nil {
			c.addUsage(usage)
		}
	case session.EventAssistantMessage:
		// Some providers only attach usage to assistant/message. It is a
		// fallback; request_end remains authoritative when both are present.
		if !c.usageSeen {
			if usage := nativeEventObject(data, "usage"); usage != nil {
				c.addUsage(usage)
			}
		}
	}
}

// foldCompletion keeps the native state projection on the same committed
// output ledger as the Go meter. The cursor is replayed from the ordered log,
// so adding one event at a time is equivalent to ProjectCompletion over the
// complete history and does not count transient assistant chunks.
func (c *nativeProjectionCursor) foldCompletion(ev session.Event) {
	if ev.Type != session.EventAssistantMessage && ev.Type != session.EventToolResult {
		return
	}
	delta := meter.ProjectCompletion([]session.Event{ev})
	c.completion.AssistantTokens += delta.AssistantTokens
	c.completion.ReasoningTokens += delta.ReasoningTokens
	c.completion.ToolCallTokens += delta.ToolCallTokens
	c.completion.ToolResultTokens += delta.ToolResultTokens
	c.completion.AttachmentBytes += delta.AttachmentBytes
	c.setProjectionValue("completionLedger", map[string]any{
		"assistantTokens":  int64(c.completion.AssistantTokens),
		"reasoningTokens":  int64(c.completion.ReasoningTokens),
		"toolCallTokens":   int64(c.completion.ToolCallTokens),
		"toolResultTokens": int64(c.completion.ToolResultTokens),
		"attachmentBytes":  c.completion.AttachmentBytes,
	})
}

func (c *nativeProjectionCursor) foldSessionListMetadata(ev session.Event, data map[string]any) {
	_ = data
	c.list.Apply(ev)
}

func nativeSessionListMetadataValue(metadata *nativeProjectionSessionListMetadata) map[string]any {
	value := map[string]any{"blank": metadata.Blank}
	if metadata.LastPromptAt == nil {
		value["lastPromptAt"] = nil
	} else {
		value["lastPromptAt"] = *metadata.LastPromptAt
	}
	return value
}

// nativeSessionListMetadataFromValues decodes the small list hint from a
// durable projection checkpoint. It is deliberately strict: if an older or
// malformed checkpoint does not carry the hint, callers keep their local
// fallback instead of inventing a blank/non-blank state.
func nativeSessionListMetadataFromValues(values map[string]any) (nativeProjectionSessionListMetadata, bool) {
	raw, ok := values["sessionListMetadata"].(map[string]any)
	if !ok {
		return nativeProjectionSessionListMetadata{}, false
	}
	blank, ok := raw["blank"].(bool)
	if !ok {
		return nativeProjectionSessionListMetadata{}, false
	}
	metadata := nativeProjectionSessionListMetadata{Blank: blank}
	if value, present := raw["lastPromptAt"]; present && value != nil {
		switch number := value.(type) {
		case int64:
			metadata.LastPromptAt = &number
		case float64:
			converted := int64(number)
			if float64(converted) != number {
				return nativeProjectionSessionListMetadata{}, false
			}
			metadata.LastPromptAt = &converted
		default:
			return nativeProjectionSessionListMetadata{}, false
		}
	}
	return metadata, true
}

func (c *nativeProjectionCursor) foldContextBreakdown(ev session.Event, data map[string]any) {
	breakdown := &c.context
	switch ev.Type {
	case session.EventRequestHeader, "llm/request_start":
		header := data
		if nested := nativeEventObject(data, "header"); nested != nil {
			header = nested
		}
		if ev.Type == "llm/request_start" {
			header = nativeLLMRequestHeader(data)
		}
		breakdown.systemTokens = 0
		breakdown.toolsTokens = 0
		if system := nativeEventString(header, "system", "systemPrompt", "system_prompt"); system != "" {
			breakdown.systemTokens = nativeEstimateTextTokens(system) + 4
		} else if _, exists := header["system"]; exists {
			breakdown.systemTokens = 4
		}
		if tools := nativeEventValue(header, "tools"); tools != nil {
			if items, ok := tools.([]any); !ok || len(items) > 0 {
				breakdown.toolsTokens = nativeEstimateJSONTokens(tools) + 4
			}
		}
		c.setProjectionValue("contextBreakdown", nativeContextBreakdownValue(breakdown))
		return
	case session.EventCompactionSummary, session.EventCompactionPrune:
		if claim := nativeContextBreakdownClaim(data); claim != nil {
			breakdown.claim = claim
		}
		return
	}

	if !nativeContextSurfaceEvent(ev.Type) {
		breakdown.claim = nil
		return
	}
	tokens := nativeEstimateSurfaceEventTokens(ev.Type, data)
	if op, ok := nativeContextReplaceOp(data); ok {
		if claim := breakdown.claim; claim != nil && claim.start == op.start && claim.end == op.end {
			breakdown.messageTokens += tokens - claim.tokens
		}
		// A replacement without an adjacent shadow-price claim is neutral in
		// DSH's bounded fold: the old range cannot be priced safely.
	} else {
		breakdown.messageTokens += tokens
	}
	breakdown.claim = nil
	c.setProjectionValue("contextBreakdown", nativeContextBreakdownValue(breakdown))
}

func nativeContextBreakdownValue(breakdown *nativeProjectionContextBreakdown) map[string]any {
	return map[string]any{
		"systemTokens":  breakdown.systemTokens,
		"toolsTokens":   breakdown.toolsTokens,
		"messageTokens": breakdown.messageTokens,
	}
}

func nativeContextSurfaceEvent(eventType string) bool {
	switch eventType {
	case session.EventUserMessage, session.EventAssistantMessage, session.EventToolResult:
		return true
	default:
		return false
	}
}

func nativeEstimateSurfaceEventTokens(eventType string, data map[string]any) int64 {
	content := nativeSurfaceContent(data)
	if eventType == session.EventToolResult {
		content = nativeToolResultBlocks(data, nativeEventString(data, "output", "toolOutput", "tool_output", "error"), nativeEventBool(data, "isError", "is_error"))
	}
	return nativeEstimateContentTokens(content) + 4
}

func nativeSurfaceContent(data map[string]any) []any {
	if raw := nativeEventValue(data, "content"); raw != nil {
		if content := nativeMessageContent(raw); len(content) > 0 {
			return content
		}
	}
	content := make([]any, 0, 2)
	if reasoning := nativeEventString(data, "reasoning"); reasoning != "" {
		content = append(content, map[string]any{"type": "reasoning", "text": reasoning})
	}
	if text := nativeEventString(data, "text"); text != "" || len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range nativeEventArray(data, "toolCalls", "tool_calls") {
		content = append(content, map[string]any{
			"type": "tool-call", "id": nativeEventString(call, "id", "callId"),
			"name": nativeEventString(call, "name"), "arguments": nativeEventString(call, "arguments", "args"),
		})
	}
	return content
}

func nativeEstimateContentTokens(content []any) int64 {
	var tokens int64
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			tokens += nativeEstimateJSONTokens(raw) + 4
			continue
		}
		switch nativeEventString(block, "type", "kind") {
		case "text", "reasoning":
			tokens += nativeEstimateTextTokens(nativeEventString(block, "text")) + 4
		case "tool-call":
			tokens += nativeEstimateTextTokens(nativeEventString(block, "name"))
			tokens += nativeEstimateTextTokens(nativeEventString(block, "arguments", "args")) + 4
		case "tool-result":
			if nested := nativeEventValue(block, "content"); nested != nil {
				tokens += nativeEstimateContentTokens(nativeMessageContent(nested)) + 4
			} else {
				tokens += nativeEstimateJSONTokens(block) + 4
			}
		default:
			tokens += nativeEstimateJSONTokens(block) + 4
		}
	}
	return tokens
}

func nativeEstimateTextTokens(value string) int64 {
	length := int64(len(utf16.Encode([]rune(value))))
	if length == 0 {
		return 0
	}
	return (length + 3) / 4
}

func nativeEstimateJSONTokens(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 {
		return 0
	}
	return (int64(len(utf16.Encode([]rune(string(encoded)))) + 3)) / 4
}

func nativeContextBreakdownClaim(data map[string]any) *nativeProjectionContextClaim {
	rangeValue := nativeEventObject(data, "shadowedRange", "shadowed_range")
	if rangeValue == nil {
		return nil
	}
	start, startOK := nativeNonnegativeEventUint64(rangeValue, "start")
	end, endOK := nativeNonnegativeEventUint64(rangeValue, "end")
	tokens, tokensOK := nativeNonnegativeEventInt64(data, "shadowedTokenCount", "shadowed_token_count")
	if !startOK || !endOK || !tokensOK || end < start {
		return nil
	}
	return &nativeProjectionContextClaim{start: start, end: end, tokens: tokens}
}

func nativeContextReplaceOp(data map[string]any) (nativeProjectionContextClaim, bool) {
	op := nativeEventObject(data, "surfaceOp", "surface_op")
	if op == nil || nativeEventString(op, "op") != "replace" {
		return nativeProjectionContextClaim{}, false
	}
	start, startOK := nativeNonnegativeEventUint64(op, "start")
	end, endOK := nativeNonnegativeEventUint64(op, "end")
	return nativeProjectionContextClaim{start: start, end: end}, startOK && endOK && end >= start
}

func (c *nativeProjectionCursor) foldSubagent(ev session.Event, data map[string]any) {
	state := &c.subagent
	now := ev.At.UnixMilli()
	if now < 0 {
		now = 0
	}

	switch ev.Type {
	case "subagent/descriptor", session.EventSubagentStart:
		identity, valid := nativeSubagentIdentity(data, ev.Type == session.EventSubagentStart, ev.Seq)
		if valid {
			c.setProjectionValue("subagent", identity)
		} else {
			// DSH uses a null sentinel for malformed or unsupported
			// descriptors so consumers can distinguish it from no projection.
			c.setProjectionValue("subagent", nil)
		}
		state.descriptorSeen = true
		state.settledMs = 0
		state.active = false
		state.activeSince = 0
		state.activeThrough = 0
		if state.hasPendingTurnStart {
			state.active = true
			state.activeSince = state.pendingTurnStart
			state.activeThrough = now
			state.hasPendingTurnStart = false
		}
		c.setProjectionValue("subagentTiming", nativeSubagentTimingValue(state))
		return
	case session.EventTurnStart:
		if !state.descriptorSeen {
			state.pendingTurnStart = now
			state.hasPendingTurnStart = true
			return
		}
		state.active = true
		state.activeSince = now
		state.activeThrough = now
		c.setProjectionValue("subagentTiming", nativeSubagentTimingValue(state))
	case session.EventTurnEnd:
		if !state.descriptorSeen {
			state.hasPendingTurnStart = false
			return
		}
		if state.active {
			if now >= state.activeSince {
				state.settledMs += now - state.activeSince
			}
			state.active = false
			state.activeSince = 0
			state.activeThrough = 0
		}
		c.setProjectionValue("subagentTiming", nativeSubagentTimingValue(state))
	default:
		if !state.descriptorSeen || !state.active {
			return
		}
		state.activeThrough = now
		c.setProjectionValue("subagentTiming", nativeSubagentTimingValue(state))
	}
}

func nativeSubagentIdentity(data map[string]any, legacyStart bool, seq uint64) (map[string]any, bool) {
	mode := nativeEventString(data, "mode")
	if legacyStart && mode == "" {
		if nativeEventBool(data, "continuable") {
			mode = "continuable"
		} else {
			mode = "one-shot"
		}
	}
	if mode != "one-shot" && mode != "continuable" {
		return nil, false
	}
	if !legacyStart {
		if !nativeSubagentDescriptorValid(data, mode) {
			return nil, false
		}
	}
	identity := map[string]any{"mode": mode, "seq": seq}
	label := nativeEventString(data, "label")
	if mode == "continuable" {
		if strings.TrimSpace(label) == "" {
			return nil, false
		}
		identity["label"] = label
	} else if label != "" {
		identity["label"] = label
	}
	return identity, true
}

func nativeSubagentDescriptorValid(data map[string]any, mode string) bool {
	version, ok := data["version"]
	if !ok || !nativeSubagentVersionTwo(version) {
		return false
	}
	if _, ok := data["provider"].(string); !ok {
		return false
	}
	allowed := map[string]bool{"version": true, "mode": true, "provider": true, "label": true}
	if mode == "continuable" {
		allowed["agentProvider"] = true
		allowed["agentModel"] = true
		allowed["persona"] = true
		allowed["toolFilter"] = true
	}
	for key := range data {
		if !allowed[key] {
			return false
		}
	}
	if label, exists := data["label"]; exists {
		if _, ok := label.(string); !ok {
			return false
		}
	} else if mode == "continuable" {
		return false
	}
	for _, key := range []string{"agentProvider", "agentModel", "persona"} {
		if value, exists := data[key]; exists {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	if raw, exists := data["toolFilter"]; exists && !nativeSubagentToolFilterValid(raw) {
		return false
	}
	return true
}

func nativeSubagentVersionTwo(value any) bool {
	switch number := value.(type) {
	case float64:
		return number == 2
	case json.Number:
		parsed, err := number.Float64()
		return err == nil && parsed == 2
	case int:
		return number == 2
	case int64:
		return number == 2
	case uint64:
		return number == 2
	default:
		return false
	}
}

func nativeSubagentToolFilterValid(value any) bool {
	filter, ok := value.(map[string]any)
	if !ok || len(filter) == 0 {
		return false
	}
	hasList := false
	for key, raw := range filter {
		if key != "allow" && key != "deny" {
			return false
		}
		items, ok := raw.([]any)
		if !ok {
			return false
		}
		hasList = true
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
	}
	return hasList
}

func nativeSubagentTimingValue(state *nativeProjectionSubagentState) map[string]any {
	value := map[string]any{"settledMs": state.settledMs}
	if state.active {
		value["active"] = map[string]any{
			"since":   state.activeSince,
			"through": state.activeThrough,
		}
	}
	return value
}

func (c *nativeProjectionCursor) createGoalProjection(ev session.Event, data map[string]any) {
	if nativeEventString(data, "id") == "" {
		return
	}
	detail := nativeEventObject(data, "detail")
	objective := nativeEventString(detail, "objective")
	status := nativeEventString(detail, "status")
	revision := nativeEventInt(data, "revision", nativeEventInt(detail, "revision", 1))
	if revision < 1 {
		revision = 1
	}
	maxRounds := nativeEventInt(data, "maxRounds", nativeEventInt(detail, "maxRounds", 0))
	roundsStarted := nativeEventInt(data, "roundsStarted", nativeEventInt(detail, "roundsStarted", 0))
	createdAt := ev.At.UnixMilli()
	if createdAt < 0 {
		createdAt = 0
	}
	c.goal = map[string]any{
		"goal": map[string]any{
			"id": nativeEventString(data, "id"), "revision": revision, "objective": objective,
			"phase": nativeGoalPhase(status), "maxGoalRounds": maxRounds,
		},
		"roundsStarted": roundsStarted, "createdAt": createdAt, "updatedAt": createdAt,
	}
	c.setProjectionValue("goal", c.goal)
}

func (c *nativeProjectionCursor) updateGoalProjection(ev session.Event, data map[string]any) {
	if c.goal == nil || nativeEventString(data, "scope") != "goal" || nativeEventString(data, "id") != nativeGoalID(c.goal) {
		return
	}
	goal := nativeProjectionMap(c.goal["goal"])
	if title := nativeEventString(data, "objective"); title != "" {
		goal["objective"] = title
	}
	if title := nativeEventString(data, "title"); title != "" {
		// The DSH GoalSnapshot has no title field; title is intentionally not
		// copied into the wire projection.
		_ = title
	}
	revision := nativeEventInt(data, "revision", nativeEventInt(goal, "revision", 1)+1)
	goal["revision"] = revision
	c.goal["goal"] = goal
	c.goal["updatedAt"] = ev.At.UnixMilli()
	c.setProjectionValue("goal", c.goal)
}

func (c *nativeProjectionCursor) updateGoalStatus(ev session.Event, data map[string]any) {
	if c.goal == nil {
		return
	}
	id := nativeEventString(data, "id", "goalId", "goal_id")
	if id != "" && id != nativeGoalID(c.goal) {
		return
	}
	goal := nativeProjectionMap(c.goal["goal"])
	if status := nativeEventString(data, "status", "phase"); status != "" {
		goal["phase"] = nativeGoalPhase(status)
		if nativeGoalPhase(status) == "blocked" {
			if reason := nativeEventString(data, "reason", "error"); reason != "" {
				goal["blockedReason"] = map[string]any{"code": "blocked", "message": reason}
			}
		} else {
			delete(goal, "blockedReason")
		}
	}
	c.goal["goal"] = goal
	c.goal["updatedAt"] = ev.At.UnixMilli()
	c.setProjectionValue("goal", c.goal)
}

func (c *nativeProjectionCursor) updateGoalRounds(ev session.Event, data map[string]any) {
	if c.goal == nil || nativeEventString(data, "goalId", "goal_id") != nativeGoalID(c.goal) {
		return
	}
	round := nativeEventInt(data, "round", 0)
	if round > nativeEventInt(c.goal, "roundsStarted", 0) {
		c.goal["roundsStarted"] = round
		c.goal["updatedAt"] = ev.At.UnixMilli()
		c.setProjectionValue("goal", c.goal)
	}
}

func nativeGoalID(value map[string]any) string {
	goal := nativeProjectionMap(value["goal"])
	return nativeEventString(goal, "id")
}

func nativeGoalPhase(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "complete", "completed", "success":
		return "complete"
	case "blocked":
		return "blocked"
	case "paused", "cancelled", "canceled", "aborted":
		return "paused"
	default:
		return "active"
	}
}

func (c *nativeProjectionCursor) foldSessionStats(ev session.Event, data map[string]any) {
	stats := &c.stats
	turn := nativeNonNegative(nativeEventInt(data, "turn", c.turn))
	step := nativeNonNegative(nativeEventInt(data, "step", c.step))
	now := ev.At.UnixMilli()
	switch ev.Type {
	case session.EventStepStart:
		stats.OpenStep = &nativeProjectionOpenStep{Turn: turn, Step: step, StartTime: now}
	case session.EventAssistantChunk, session.EventAssistantReasoning:
		open := stats.OpenStep
		if open != nil && open.Turn == turn && open.Step == step && !open.HasFirstToken && nativeEventString(data, "text") != "" {
			open.FirstTokenAt = now
			open.HasFirstToken = true
		}
	case session.EventAssistantMessage:
		open := stats.OpenStep
		if open != nil && open.Turn == turn && open.Step == step {
			if now >= open.StartTime {
				stats.LLMMs += now - open.StartTime
			}
			if open.HasFirstToken {
				if open.FirstTokenAt >= open.StartTime {
					stats.TTFTMs += open.FirstTokenAt - open.StartTime
				}
				stats.TTFTSteps++
				if usage := nativeEventObject(data, "usage"); usage != nil {
					if output, ok := nativeNonnegativeEventInt64(usage, "outputTokens", "output_tokens"); ok {
						stats.DecodeTokens += output
						if now >= open.FirstTokenAt {
							stats.DecodeMs += now - open.FirstTokenAt
						}
					}
				}
			}
			stats.OpenStep = nil
		}
	case session.EventToolCall, "tool/start":
		callID := nativeEventString(data, "callId", "call_id")
		if callID != "" {
			if stats.PendingTools == nil {
				stats.PendingTools = make(map[string]int64)
			}
			stats.PendingTools[callID] = now
		}
	case session.EventToolResult, "tool/error":
		callID := nativeEventString(data, "callId", "call_id")
		if callID == "" {
			if message := nativeEventObject(data, "message"); message != nil {
				if source := nativeEventObject(message, "source"); source != nil {
					callID = nativeEventString(source, "callId", "call_id")
				}
			}
		}
		if started, ok := stats.PendingTools[callID]; ok {
			if now >= started {
				stats.ToolMs += now - started
			}
			delete(stats.PendingTools, callID)
		}
	case session.EventStepEnd:
		if !stats.HasLastTurn || stats.LastTurn != turn {
			stats.Turns++
		}
		stats.Steps++
		stats.LastTurn = turn
		stats.HasLastTurn = true
		stats.OpenStep = nil
	case session.EventTurnEnd:
		stats.PendingTools = nil
	}
	next := nativeSessionStatsValue(stats)
	if !nativeSessionStatsEqual(c.values["sessionStats"], next) {
		c.setProjectionValue("sessionStats", next)
	}
}

func nativeSessionStatsValue(stats *nativeProjectionSessionStats) map[string]any {
	return map[string]any{
		"turns":        stats.Turns,
		"steps":        stats.Steps,
		"llmMs":        stats.LLMMs,
		"toolMs":       stats.ToolMs,
		"ttftMs":       stats.TTFTMs,
		"ttftSteps":    stats.TTFTSteps,
		"decodeMs":     stats.DecodeMs,
		"decodeTokens": stats.DecodeTokens,
	}
}

func nativeSessionStatsEqual(previous any, next map[string]any) bool {
	object, ok := previous.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range next {
		if nativeEventInt64(object, key) != nativeEventInt64(next, key) {
			return false
		}
		if nativeEventValue(object, key) == nil && value != nil {
			return false
		}
	}
	return true
}

func (c *nativeProjectionCursor) setProjectionValue(key string, value any) {
	stored := nativeProjectionValueCopy(value)
	c.values[key] = stored
	c.changed[key] = nativeProjectionValueCopy(stored)
}

func (c *nativeProjectionCursor) addUsage(usage map[string]any) {
	input := nativeEventInt64(usage, "inputTokens", "input_tokens")
	cached := nativeEventInt64(usage, "cachedInputTokens", "cached_input_tokens", "cacheReadTokens", "cache_read_tokens")
	uncached := nativeEventInt64(usage, "uncachedInputTokens", "uncached_input_tokens")
	if uncached == 0 && input > cached {
		uncached = input - cached
	}
	c.usage.UncachedInputTokens += maxInt64(0, uncached)
	c.usage.OutputTokens += maxInt64(0, nativeEventInt64(usage, "outputTokens", "output_tokens"))
	c.usage.CacheReadTokens += maxInt64(0, cached)
	c.usage.CacheWriteTokens += maxInt64(0, nativeEventInt64(usage, "cacheWriteTokens", "cache_write_tokens"))
	c.usageSeen = true
	c.setProjectionValue("tokenUsage", map[string]any{
		"uncachedInputTokens": c.usage.UncachedInputTokens,
		"outputTokens":        c.usage.OutputTokens,
		"cacheReadTokens":     c.usage.CacheReadTokens,
		"cacheWriteTokens":    c.usage.CacheWriteTokens,
	})
	context := nativeProjectionMap(c.values["contextPressure"])
	pressure := input + cached + nativeEventInt64(usage, "cacheWriteTokens", "cache_write_tokens")
	if pressure > 0 {
		context["pressureTokens"] = pressure
	}
	c.setProjectionValue("contextPressure", context)
}

func (c *nativeProjectionCursor) upsertTodo(data map[string]any) {
	id := nativeEventString(data, "id")
	content := nativeEventString(data, "content", "title", "objective", "text")
	if detail := nativeEventObject(data, "detail"); detail != nil {
		if content == "" {
			content = nativeEventString(detail, "content", "title", "objective", "text")
		}
	}
	if id == "" {
		id = content
	}
	for index := range c.todos {
		if nativeEventString(c.todos[index], "id") == id {
			if content != "" {
				c.todos[index]["content"] = content
			}
			c.todosSeen = true
			c.setProjectionValue("todos", nativeTodoValues(c.todos))
			return
		}
	}
	c.todos = append(c.todos, map[string]any{
		"id": id, "content": content, "status": nativeTodoStatus(nativeEventString(data, "status")),
	})
	c.todosSeen = true
	c.setProjectionValue("todos", nativeTodoValues(c.todos))
}

func (c *nativeProjectionCursor) updateTodoStatus(id, status string) {
	for index := range c.todos {
		if nativeEventString(c.todos[index], "id") == id {
			c.todos[index]["status"] = nativeTodoStatus(status)
			c.setProjectionValue("todos", nativeTodoValues(c.todos))
			return
		}
	}
}

func (c *nativeProjectionCursor) deleteTodo(id string) {
	for index := range c.todos {
		if nativeEventString(c.todos[index], "id") == id {
			c.todos = append(c.todos[:index], c.todos[index+1:]...)
			c.setProjectionValue("todos", nativeTodoValues(c.todos))
			return
		}
	}
}

func (c *nativeProjectionCursor) projectionChanges() map[string]any {
	changes := make(map[string]any, len(c.changed))
	for key, value := range c.changed {
		changes[key] = value
	}
	return changes
}

func (c *nativeProjectionCursor) projectionBlock(title string, lastSeq int64, permission ...string) nativeProjectionBlock {
	values := make(map[string]any, len(c.values)+1)
	for key, value := range c.values {
		values[key] = nativeProjectionValueCopy(value)
	}
	if title == "" {
		values["title"] = nil
	} else {
		values["title"] = title
	}
	if !c.todosSeen {
		values["todos"] = nil
	}
	if _, ok := values["sessionStats"]; !ok {
		values["sessionStats"] = nativeSessionStatsValue(&c.stats)
	}
	if _, ok := values["subagent"]; !ok {
		values["subagent"] = nil
	}
	if _, ok := values["subagentTiming"]; !ok {
		values["subagentTiming"] = nativeSubagentTimingValue(&c.subagent)
	}
	if _, ok := values["contextBreakdown"]; !ok {
		values["contextBreakdown"] = nativeContextBreakdownValue(&c.context)
	}
	if _, ok := values["sessionListMetadata"]; !ok {
		values["sessionListMetadata"] = nativeSessionListMetadataValue(&c.list)
	}
	values["permissions"] = nativePermissionProjection(firstNonEmpty(permission...))
	if c.canonical != nil {
		snapshot := c.canonical.Snapshot()
		if snapshot.Permission != "" {
			values["permissions"] = nativePermissionProjection(snapshot.Permission)
		}
		values["plan"] = map[string]any{
			"active":  snapshot.PlanMode.Active,
			"pending": snapshot.PlanMode.Pending,
		}
		values["goal"] = canonicalGoalValue(snapshot)
		values["todos"] = canonicalTodosValue(snapshot)
		values["sessionListMetadata"] = nativeSessionListMetadataValue(&snapshot.SessionList)
	}
	return nativeProjectionBlock{AsOfSeq: lastSeq, Values: values}
}

func (c *nativeProjectionCursor) syncCanonicalControlChanges(ev session.Event) {
	snapshot := c.canonical.Snapshot()
	switch ev.Type {
	case "permission/preset", "sandbox/mode":
		c.changed["permissions"] = nativePermissionProjection(snapshot.Permission)
	case session.EventPlanMode:
		c.changed["plan"] = map[string]any{
			"active":  snapshot.PlanMode.Active,
			"pending": snapshot.PlanMode.Pending,
		}
	case session.EventGoalRoundStart, session.EventGoalRoundEnd:
		c.changed["goal"] = canonicalGoalValue(snapshot)
	case session.EventTodoWrite:
		c.changed["todos"] = canonicalTodosValue(snapshot)
	case session.EventPlanCreate, session.EventPlanUpdate, session.EventPlanStatus, session.EventPlanDelete:
		scope := nativeEventString(nativeJSONObject(ev.Data), "scope")
		if scope == "goal" {
			c.changed["goal"] = canonicalGoalValue(snapshot)
		}
		if scope == "todo" {
			c.changed["todos"] = canonicalTodosValue(snapshot)
		}
	}
}

// canonicalGoalValue converts the durable shared goal entity into the compact
// DSH GoalView shape. Activation remains a runtime concern; all durable goal
// fields and lifecycle timestamps come from the canonical projection.
func canonicalGoalValue(snapshot projection.Snapshot) any {
	var goal *projection.Entity
	for index := len(snapshot.Goals) - 1; index >= 0; index-- {
		candidate := &snapshot.Goals[index]
		if candidate.Status == "done" || candidate.Status == "cancelled" || candidate.Status == "canceled" {
			continue
		}
		goal = candidate
		break
	}
	if goal == nil {
		return nil
	}
	data := goal.Data
	revision := projectionInt(data, "revision")
	if revision < 1 {
		revision = 1
	}
	maxRounds := projectionInt(data, "maxRounds", "maxGoalRounds")
	goalValue := map[string]any{
		"id":            goal.ID,
		"revision":      revision,
		"objective":     nativeEventString(data, "objective"),
		"phase":         nativeGoalPhase(goal.Status),
		"roundsStarted": projectionInt(data, "roundsStarted"),
		"maxGoalRounds": maxRounds,
	}
	if reason := nativeEventString(data, "reason", "blockedReason"); reason != "" && goalValue["phase"] == "blocked" {
		goalValue["blockedReason"] = map[string]any{"code": "blocked", "message": reason}
	}
	createdAt := goal.CreatedAt
	if createdAt == 0 {
		createdAt = goal.UpdatedAt
	}
	updatedAt := goal.UpdatedAt
	if updatedAt == 0 {
		updatedAt = createdAt
	}
	return map[string]any{
		"goal":          goalValue,
		"roundsStarted": projectionInt(data, "roundsStarted"),
		"createdAt":     createdAt,
		"updatedAt":     updatedAt,
	}
}

func projectionInt(data map[string]any, keys ...string) int {
	for _, key := range keys {
		if number, ok := data[key].(float64); ok && number >= 0 && number == float64(int(number)) {
			return int(number)
		}
		if number, ok := data[key].(int); ok {
			return number
		}
	}
	return 0
}

func canonicalTodosValue(snapshot projection.Snapshot) any {
	if snapshot.Todos != nil {
		if raw, ok := snapshot.Todos["todos"]; ok {
			if items, valid := nativeTodoItems(raw); valid {
				return nativeTodoValues(items)
			}
		}
	}
	items := make([]map[string]any, 0)
	for _, entity := range snapshot.PlanTodos {
		item := nativeProjectionMap(entity.Data)
		item["id"] = entity.ID
		item["content"] = nativeEventString(item, "content", "title", "objective", "text")
		if entity.Status != "" {
			item["status"] = entity.Status
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil
	}
	return nativeTodoValues(items)
}

func nativeProjectionValueCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for key, item := range typed {
			copy[key] = nativeProjectionValueCopy(item)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for index, item := range typed {
			copy[index] = nativeProjectionValueCopy(item)
		}
		return copy
	default:
		return value
	}
}

func nativePermissionProjection(permission string) map[string]any {
	permission = strings.ToLower(strings.TrimSpace(permission))
	if permission != "readonly" && permission != "full" {
		permission = "standard"
	}
	return map[string]any{
		"options": []any{
			map[string]any{"value": "readonly", "name": "Read-only"},
			map[string]any{"value": "standard", "name": "Standard"},
			map[string]any{"value": "full", "name": "Full access"},
		},
		"currentValue": permission,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nativeProjectionMap(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		clone := make(map[string]any, len(object)+1)
		for key, item := range object {
			clone[key] = item
		}
		return clone
	}
	return make(map[string]any)
}

func nativeTodoItems(raw any) ([]map[string]any, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	result := make([]map[string]any, 0, len(items))
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := nativeEventString(object, "id")
		if id == "" {
			id = fmt.Sprintf("todo:%d", index)
		}
		result = append(result, map[string]any{
			"id":      id,
			"content": nativeEventString(object, "content", "title", "objective", "text"),
			"status":  nativeTodoStatus(nativeEventString(object, "status", "state")),
		})
	}
	return result, true
}

func nativeTodoValues(items []map[string]any) []any {
	result := make([]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{
			"content": nativeEventString(item, "content"),
			"status":  nativeTodoStatus(nativeEventString(item, "status")),
		})
	}
	return result
}

func nativeTodoStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "success":
		return "completed"
	case "in_progress", "in-progress", "active", "running":
		return "in_progress"
	default:
		return "pending"
	}
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

// nativeLLMRequestHeader converts Shutu's private request-start detail into
// the DSH request/header event consumed by Trajectory. DSH deliberately keeps
// the model-visible system prompt and complete tool catalog on this event so
// the first trajectory cell can be rendered as SYSTEM: Initial System Prompt,
// followed by the actual user/message cell.
func nativeLLMRequestHeader(data map[string]any) map[string]any {
	system := ""
	for _, message := range nativeEventArray(data, "messages") {
		if nativeEventString(message, "role") != "system" {
			continue
		}
		if text := nativeEventString(message, "text"); text != "" {
			if system != "" {
				system += "\n"
			}
			system += text
		}
	}
	tools := nativeEventValue(data, "tools")
	if tools == nil {
		tools = []any{}
	}
	config := map[string]any{
		"provider": nativeEventString(data, "provider"),
		"model":    nativeEventString(data, "model"),
	}
	if effort := nativeEventString(data, "reasoningEffort", "reasoning_effort"); effort != "" {
		config["reasoningEffort"] = effort
	}
	if maxTokens := nativeEventInt(data, "maxTokens", 0); maxTokens > 0 {
		config["maxTokens"] = maxTokens
	}
	if temperature, ok := nativeEventFloat64(data, "temperature"); ok {
		config["temperature"] = temperature
	}
	if stop := nativeEventStrings(data, "stop"); len(stop) != 0 {
		config["stop"] = stop
	}
	return map[string]any{
		"config": config,
		"system": system,
		"tools":  tools,
	}
}

func (c *nativeProjectionCursor) assistantMessageData(sessionID string, seq uint64, data map[string]any) map[string]any {
	content := nativeMessageContent(nativeEventValue(data, "content"))
	messageData := nativeEventObject(data, "message")
	if len(content) == 0 && messageData != nil {
		content = nativeMessageContent(nativeEventValue(messageData, "content"))
	}
	if len(content) == 0 {
		content = make([]any, 0, 2)
		reasoning := nativeEventString(data, "reasoning")
		if reasoning == "" && messageData != nil {
			reasoning = nativeEventString(messageData, "reasoning")
		}
		if reasoning != "" {
			content = append(content, map[string]any{"type": "reasoning", "text": reasoning})
		}
		text := nativeEventString(data, "text")
		if text == "" && messageData != nil {
			text = nativeEventString(messageData, "text")
		}
		if text != "" || len(content) == 0 {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		calls := nativeEventArray(data, "toolCalls", "tool_calls")
		if len(calls) == 0 && messageData != nil {
			calls = nativeEventArray(messageData, "toolCalls", "tool_calls")
		}
		for _, call := range calls {
			content = append(content, map[string]any{
				"type":      "tool-call",
				"id":        nativeEventString(call, "id", "callId"),
				"name":      nativeEventString(call, "name"),
				"arguments": nativeEventString(call, "arguments", "args"),
			})
		}
	}
	provider := nativeEventString(data, "provider")
	model := nativeEventString(data, "model")
	if messageData != nil {
		if source := nativeEventObject(messageData, "source"); source != nil {
			if provider == "" {
				provider = nativeEventString(source, "provider")
			}
			if model == "" {
				model = nativeEventString(source, "model")
			}
		}
	}
	message := map[string]any{
		"id":      nativeMessageID(sessionID, seq),
		"role":    "assistant",
		"content": content,
		"source": map[string]any{
			"kind":     "model",
			"provider": firstNonEmpty(provider, c.provider),
			"model":    firstNonEmpty(model, c.model),
		},
	}
	if message["source"].(map[string]any)["provider"] == "" {
		message["source"].(map[string]any)["provider"] = "shutu"
	}
	if message["source"].(map[string]any)["model"] == "" {
		message["source"].(map[string]any)["model"] = "unknown"
	}
	result := map[string]any{
		"turn":    nativeNonNegative(nativeEventInt(data, "turn", c.turn)),
		"step":    nativeNonNegative(nativeEventInt(data, "step", c.step)),
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
		"turn":    nativeNonNegative(nativeEventInt(data, "turn", c.turn)),
		"step":    nativeNonNegative(nativeEventInt(data, "step", c.step)),
		"message": message,
		"callId":  callID,
		"name":    name,
	}
	if errorData != nil {
		result["error"] = errorData
	} else if forcedError || nativeEventString(data, "error") != "" {
		result["error"] = map[string]any{"name": "ToolError", "code": "TOOL_ERROR"}
	}
	if meta := nativeEventValue(data, "meta"); meta != nil {
		result["meta"] = meta
	}
	if spill := nativeEventValue(data, "spill"); spill != nil {
		result["spill"] = spill
	}
	if code := nativeEventString(data, "code"); code != "" {
		result["code"] = code
	}
	return result
}

func nativeToolResultBlocks(data map[string]any, fallback string, isError bool) []any {
	if message := nativeEventObject(data, "message"); message != nil {
		if raw := nativeEventValue(message, "content"); raw != nil {
			if blocks := nativeMessageContent(raw); len(blocks) > 0 {
				// Canonical Harness ToolResultMessage carries one outer
				// tool-result block. The tool/result event projection already
				// supplies that outer block, so expose only its nested content
				// here to avoid double wrapping during replay.
				if len(blocks) == 1 {
					if block, ok := blocks[0].(map[string]any); ok && block["type"] == "tool-result" {
						if nested := nativeMessageContent(block["content"]); len(nested) > 0 {
							return nested
						}
					}
				}
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
	turn := nativeNonNegative(nativeEventInt(data, "turn", c.turn))
	retryID := nativeEventString(data, "retryId", "retry_id")
	if retryID == "" {
		retryID = fmt.Sprintf("%s:retry:%d:%d", sessionID, turn, retry)
	}
	result := map[string]any{
		"retryId":    retryID,
		"turn":       turn,
		"step":       nativeNonNegative(nativeEventInt(data, "step", c.step)),
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
			image := nativeEventObject(object, "attachment", "image")
			if image == nil {
				image = object
			}
			attachment := map[string]any{
				"attachmentId": nativeEventString(image, "attachmentId", "attachment_id", "id"),
				"mediaType":    nativeEventString(image, "mediaType", "media_type"),
				"bytes":        nativeEventNumber(image, "bytes"),
				"width":        nativeEventNumber(image, "width"),
				"height":       nativeEventNumber(image, "height"),
			}
			if name := nativeEventString(image, "name"); name != "" {
				attachment["name"] = name
			}
			result = append(result, map[string]any{
				"type":       "image",
				"attachment": attachment,
			})
		case "tool-call":
			result = append(result, map[string]any{
				"type": "tool-call", "id": nativeEventString(object, "id", "callId"),
				"name": nativeEventString(object, "name"), "arguments": nativeEventString(object, "arguments", "args"),
			})
		case "tool-result":
			content := nativeMessageContent(nativeEventValue(object, "content"))
			if content == nil {
				content = []any{}
			}
			result = append(result, map[string]any{
				"type": "tool-result", "toolCallId": nativeEventString(object, "toolCallId", "tool_call_id"),
				"content": content, "isError": nativeEventBool(object, "isError", "is_error"),
			})
		default:
			// ContentBlockMap is extension-friendly. Preserve an unknown block
			// verbatim so native plugins can render it at the client boundary.
			result = append(result, nativeProjectionValueCopy(object))
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

func nativeEventStrings(object map[string]any, keys ...string) []string {
	value := nativeEventValue(object, keys...)
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func nativeEventFloat64(object map[string]any, keys ...string) (float64, bool) {
	value := nativeEventValue(object, keys...)
	switch number := value.(type) {
	case float64:
		return number, true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	}
	return 0, false
}

func nativeNonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func nativeEventNumber(object map[string]any, keys ...string) int {
	return int(nativeEventInt64(object, keys...))
}

func nativeEventInt64(object map[string]any, keys ...string) int64 {
	value := nativeEventValue(object, keys...)
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	case int:
		return int64(number)
	case int64:
		return number
	case uint64:
		return int64(number)
	}
	return 0
}

func nativeNonnegativeEventInt64(object map[string]any, keys ...string) (int64, bool) {
	value := nativeEventValue(object, keys...)
	switch number := value.(type) {
	case float64:
		if number < 0 {
			return 0, false
		}
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil && parsed >= 0
	case int:
		return int64(number), number >= 0
	case int64:
		return number, number >= 0
	case uint64:
		return int64(number), number <= uint64(^uint64(0)>>1)
	default:
		return 0, false
	}
}

func nativeNonnegativeEventUint64(object map[string]any, keys ...string) (uint64, bool) {
	value := nativeEventValue(object, keys...)
	switch number := value.(type) {
	case float64:
		if number < 0 || number != float64(uint64(number)) {
			return 0, false
		}
		return uint64(number), true
	case json.Number:
		parsed, err := strconv.ParseUint(string(number), 10, 64)
		return parsed, err == nil
	case int:
		return uint64(number), number >= 0
	case int64:
		return uint64(number), number >= 0
	case uint64:
		return number, true
	default:
		return 0, false
	}
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
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
	c.surfaceGeneration++
	return map[string]any{"op": "replace", "start": shadowed[0], "end": shadowed[len(shadowed)-1]}, shadowed
}

func (c *nativeProjectionCursor) surfaceSnapshot() map[string]any {
	if c.canonical != nil {
		snapshot := c.canonical.Snapshot()
		nodes := make([]any, 0, len(snapshot.Surface))
		for _, entry := range snapshot.Surface {
			nodes = append(nodes, entry.Seq)
		}
		return map[string]any{
			"nodes":             nodes,
			"replaceGeneration": c.surfaceGeneration,
		}
	}
	nodes := make([]any, 0, len(c.surface))
	for _, seq := range c.surface {
		nodes = append(nodes, seq)
	}
	return map[string]any{
		"nodes":             nodes,
		"replaceGeneration": c.surfaceGeneration,
	}
}

func nativeDSHEventType(typ string) bool {
	switch typ {
	case "assistant/chunk", "assistant/message", "command/run", "command/done", "compaction/end", "compaction/prune", "compaction/start",
		"compaction/summary", "feedback/record", "goal/change", "llm/retry", "llm/retry-started", "permission/preset", "sandbox/mode", "approval/asked", "approval/decided", "approval/policy", "plan/mode",
		"request/context", "request/header", "schedule/change", "session/end-seed", session.EventSessionTitle,
		"session/title-llm-request", "step/end", "step/start", "subagent/descriptor", "todo/write", "tool/call",
		"tool/result", "tool/code-dispatch-start", "tool/code-dispatch", "turn/end", "turn/start", "user/message":
		return true
	default:
		return false
	}
}
