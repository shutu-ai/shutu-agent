package webserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

func nativeResponse(t *testing.T, recBody []byte) nativeRPCResponse {
	t.Helper()
	var response nativeRPCResponse
	if err := json.Unmarshal(recBody, &response); err != nil {
		t.Fatalf("decode native response: %v; body=%s", err, recBody)
	}
	return response
}

func TestNativeRPCSessionHistoryAndPrompt(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-session", []session.Event{{
		Seq: 0, Type: session.EventUserMessage, At: time.UnixMilli(1234), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"hello native"}`),
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"list-1","method":"session.list","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || response.RPCID != "list-1" {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].SessionID != "native-session" || !list.Items[0].Blank || list.Items[0].Projections == nil {
		t.Fatalf("session.list items = %+v", list.Items)
	}
	metadata, ok := list.Items[0].Projections.Values["sessionListMetadata"].(map[string]any)
	if !ok || metadata["blank"] != true || metadata["lastPromptAt"] != float64(1234) {
		t.Fatalf("session.list projection metadata = %#v", list.Items[0].Projections.Values["sessionListMetadata"])
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"history-1","method":"session.history","payload":{"sessionId":"native-session"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	var history struct {
		Header      nativeSessionHeader   `json:"header"`
		Events      []nativeHistoryEntry  `json:"events"`
		Projections nativeProjectionBlock `json:"projections"`
	}
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Header.Version != 0 || history.Header.ID != "native-session" || history.Header.CreatedAt == 0 || len(history.Events) != 1 || history.Events[0].Event.Time != 1234 || history.Events[0].Event.Type != session.EventUserMessage {
		t.Fatalf("session.history events = %+v", history.Events)
	}
	if history.Projections.AsOfSeq != 0 {
		t.Fatalf("history projection asOfSeq = %d, want 0", history.Projections.AsOfSeq)
	}
	var userData struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(history.Events[0].Event.Data, &userData); err != nil {
		t.Fatal(err)
	}
	if userData.ID == "" || userData.Role != "user" || userData.Source.Kind != "user" || len(userData.Content) != 1 || userData.Content[0].Text != "hello native" {
		t.Fatalf("native user message = %+v", userData)
	}

	var gotSession, gotText string
	srv.SetMessageHandler(func(_ context.Context, sessionID, text string, _ []llm.ImageRef) error {
		gotSession, gotText = sessionID, text
		return nil
	})
	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"prompt-1","method":"session.prompt","payload":{"sessionId":"native-session","mode":"queue","content":[{"type":"text","text":"send me"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || gotSession != "native-session" || gotText != "send me" {
		t.Fatalf("session.prompt response=%+v callback=(%q,%q)", response, gotSession, gotText)
	}
}

func TestNativeHistoryReturnsDSHProjectionBaseline(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-projections", []session.Event{
		{Seq: 1, Type: session.EventPlanCreate, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","title":"ship native UI"}`)},
		{Seq: 2, Type: session.EventPlanStatus, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","status":"in-progress"}`)},
		{Seq: 3, Type: session.EventPlanMode, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"active":true,"pending":false}`)},
		{Seq: 4, Type: session.EventLLMRequestStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"provider":"deepseek","model":"reasoner","contextWindow":128000}`)},
		{Seq: 5, Type: session.EventLLMRequestEnd, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"usage":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":40,"cacheWriteTokens":5}}`)},
	})
	if err := st.SetSessionTitle(context.Background(), "native-projections", "Native UI parity", session.TitleSourceUser); err != nil {
		t.Fatalf("set title: %v", err)
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"projection-1","method":"session.history","payload":{"sessionId":"native-projections"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Projections.AsOfSeq != 5 {
		t.Fatalf("projection asOfSeq = %d, want 5", history.Projections.AsOfSeq)
	}
	values := history.Projections.Values
	if values["title"] != "Native UI parity" {
		t.Fatalf("projection title = %#v", values["title"])
	}
	var todos []map[string]any
	encoded, _ = json.Marshal(values["todos"])
	if err := json.Unmarshal(encoded, &todos); err != nil {
		t.Fatal(err)
	}
	if len(todos) != 1 || todos[0]["content"] != "ship native UI" || todos[0]["status"] != "in_progress" {
		t.Fatalf("projection todos = %#v", todos)
	}
	var usage map[string]int64
	encoded, _ = json.Marshal(values["tokenUsage"])
	if err := json.Unmarshal(encoded, &usage); err != nil {
		t.Fatal(err)
	}
	if usage["uncachedInputTokens"] != 60 || usage["outputTokens"] != 20 || usage["cacheReadTokens"] != 40 || usage["cacheWriteTokens"] != 5 {
		t.Fatalf("projection token usage = %#v", usage)
	}
	var contextPressure map[string]int64
	encoded, _ = json.Marshal(values["contextPressure"])
	if err := json.Unmarshal(encoded, &contextPressure); err != nil {
		t.Fatal(err)
	}
	if contextPressure["contextWindow"] != 128000 || contextPressure["pressureTokens"] != 100 {
		t.Fatalf("projection context pressure = %#v", contextPressure)
	}
	var plan map[string]bool
	encoded, _ = json.Marshal(values["plan"])
	if err := json.Unmarshal(encoded, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan["active"] || plan["pending"] {
		t.Fatalf("projection plan = %#v", plan)
	}
}

func TestNativeHistoryReturnsGoalAndPermissionProjection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-goal", []session.Event{
		{Seq: 1, Type: session.EventPlanCreate, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","title":"Release","detail":{"objective":"ship the release","status":"pending","revision":1,"maxRounds":3}}`)},
		{Seq: 2, Type: session.EventGoalRoundStart, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"goalId":"goal-1","round":1}`)},
		{Seq: 3, Type: session.EventPlanStatus, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","status":"blocked","reason":"waiting for approval"}`)},
		{Seq: 4, Type: session.EventPlanUpdate, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","objective":"ship the verified release","revision":2}`)},
	})
	if err := st.SetSessionConfig(context.Background(), "native-goal", store.SessionConfig{Permission: "readonly"}); err != nil {
		t.Fatalf("set session permission: %v", err)
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"goal-1","method":"session.history","payload":{"sessionId":"native-goal"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	var goal map[string]any
	encoded, _ = json.Marshal(history.Projections.Values["goal"])
	if err := json.Unmarshal(encoded, &goal); err != nil {
		t.Fatal(err)
	}
	var goalSnapshot map[string]any
	encoded, _ = json.Marshal(goal["goal"])
	if err := json.Unmarshal(encoded, &goalSnapshot); err != nil {
		t.Fatal(err)
	}
	if goalSnapshot["id"] != "goal-1" || goalSnapshot["objective"] != "ship the verified release" || goalSnapshot["phase"] != "blocked" || goalSnapshot["maxGoalRounds"] != float64(3) {
		t.Fatalf("goal snapshot = %#v", goalSnapshot)
	}
	if goal["roundsStarted"] != float64(1) || goal["createdAt"] != float64(1001) || goal["updatedAt"] != float64(1004) {
		t.Fatalf("goal projection metadata = %#v", goal)
	}
	var reason map[string]any
	encoded, _ = json.Marshal(goalSnapshot["blockedReason"])
	if err := json.Unmarshal(encoded, &reason); err != nil {
		t.Fatal(err)
	}
	if reason["code"] != "blocked" || reason["message"] != "waiting for approval" {
		t.Fatalf("goal blocked reason = %#v", reason)
	}
	var permissions map[string]any
	encoded, _ = json.Marshal(history.Projections.Values["permissions"])
	if err := json.Unmarshal(encoded, &permissions); err != nil {
		t.Fatal(err)
	}
	if permissions["currentValue"] != "readonly" {
		t.Fatalf("permissions projection = %#v", permissions)
	}
}

func TestNativeHistoryReturnsDSHHeaderLineageAndSurfaceSnapshot(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-lineage", []session.Event{
		{Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"id":"child","provider":"spawn","parentSession":"root","label":"research","depth":2}`)},
		{Seq: 2, Type: session.EventTurnStart, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"question"}`)},
		{Seq: 4, Type: session.EventAssistantMessage, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer"}`)},
		{Seq: 5, Type: session.EventAssistantMessage, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":3,"end":4}}`)},
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"lineage","method":"session.history","payload":{"sessionId":"native-lineage"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Header  nativeSessionHeader `json:"header"`
		Surface map[string]any      `json:"surface"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Header.ParentSessionID != "root" || history.Header.Origin != "subagent" || history.Header.DelegationDepth != 2 {
		t.Fatalf("session header lineage = %+v", history.Header)
	}
	if history.Surface["replaceGeneration"] != float64(1) {
		t.Fatalf("surface generation = %#v", history.Surface)
	}
	nodes, ok := history.Surface["nodes"].([]any)
	if !ok || len(nodes) != 1 || nodes[0] != float64(5) {
		t.Fatalf("surface nodes = %#v", history.Surface["nodes"])
	}
}

func TestNativeProjectionPreservesDSHContentBlocks(t *testing.T) {
	cursor := newNativeProjectionCursor()
	event := cursor.project("native-content", session.Event{
		Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{"content":[{"type":"text","text":"**bold**\n\n    return 1"},{"type":"reasoning","text":"think first"},{"type":"image","attachment":{"attachmentId":"att-1","mediaType":"image/png","bytes":12,"width":2,"height":3,"name":"screen.png"}},{"type":"tool-result","toolCallId":"call-1","content":[{"type":"text","text":"done"}],"isError":true},{"type":"vendor-card","value":{"ok":true}}]}`),
	})
	var projected struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(event.Data, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Message.Content) != 5 {
		t.Fatalf("projected content = %#v", projected.Message.Content)
	}
	if projected.Message.Content[0]["text"] != "**bold**\n\n    return 1" || projected.Message.Content[1]["type"] != "reasoning" {
		t.Fatalf("markdown/reasoning blocks = %#v", projected.Message.Content[:2])
	}
	image, ok := projected.Message.Content[2]["attachment"].(map[string]any)
	if !ok || image["attachmentId"] != "att-1" || image["name"] != "screen.png" {
		t.Fatalf("image attachment block = %#v", projected.Message.Content[2])
	}
	if projected.Message.Content[3]["toolCallId"] != "call-1" || projected.Message.Content[3]["isError"] != true {
		t.Fatalf("tool result block = %#v", projected.Message.Content[3])
	}
	if projected.Message.Content[4]["type"] != "vendor-card" {
		t.Fatalf("extension block = %#v", projected.Message.Content[4])
	}
}

func TestNativeProjectionFoldsSessionStatsFromLifecycleEvents(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(900), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"step":1}`)},
		{Seq: 3, Type: session.EventAssistantChunk, At: time.UnixMilli(1800), Version: session.EventVersion, Data: json.RawMessage(`{"text":"a"}`)},
		{Seq: 4, Type: session.EventAssistantMessage, At: time.UnixMilli(4800), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","usage":{"outputTokens":60}}`)},
		{Seq: 5, Type: session.EventToolCall, At: time.UnixMilli(5000), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","name":"read"}`)},
		{Seq: 6, Type: session.EventToolResult, At: time.UnixMilli(6500), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","output":"ok"}`)},
		{Seq: 7, Type: session.EventStepEnd, At: time.UnixMilli(6600), Version: session.EventVersion, Data: json.RawMessage(`{"step":1}`)},
		{Seq: 8, Type: session.EventTurnEnd, At: time.UnixMilli(6700), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
	}
	cursor := newNativeProjectionCursor()
	for _, ev := range events {
		cursor.project("stats", ev)
	}
	var stats map[string]int64
	encoded, _ := json.Marshal(cursor.projectionBlock("", 8).Values["sessionStats"])
	if err := json.Unmarshal(encoded, &stats); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"turns": 1, "steps": 1, "llmMs": 3800, "toolMs": 1500,
		"ttftMs": 800, "ttftSteps": 1, "decodeMs": 3000, "decodeTokens": 60,
	}
	for key, value := range want {
		if stats[key] != value {
			t.Fatalf("sessionStats[%s] = %d, want %d (all=%#v)", key, stats[key], value, stats)
		}
	}
}

func TestNativeProjectionFoldsDSHSubagentIdentityAndTiming(t *testing.T) {
	tests := []struct {
		name        string
		events      []session.Event
		wantMode    string
		wantLabel   string
		wantSeq     uint64
		wantSettled int64
		wantActive  bool
	}{
		{
			name: "canonical descriptor includes pending turn",
			events: []session.Event{
				{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(100), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
				{Seq: 2, Type: "subagent/descriptor", At: time.UnixMilli(200), Version: session.EventVersion, Data: json.RawMessage(`{"version":2,"mode":"continuable","provider":"spawn","label":"research"}`)},
				{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(500), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
			},
			wantMode: "continuable", wantLabel: "research", wantSeq: 2, wantSettled: 400,
		},
		{
			name: "legacy start defaults to one shot",
			events: []session.Event{
				{Seq: 7, Type: session.EventSubagentStart, At: time.UnixMilli(700), Version: session.EventVersion, Data: json.RawMessage(`{"id":"child-1","label":"quick task"}`)},
			},
			wantMode: "one-shot", wantLabel: "quick task", wantSeq: 7,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cursor := newNativeProjectionCursor()
			for _, ev := range test.events {
				cursor.project("subagent", ev)
			}
			values := cursor.projectionBlock("", int64(test.wantSeq)).Values
			identity, ok := values["subagent"].(map[string]any)
			if !ok {
				t.Fatalf("subagent projection = %#v", values["subagent"])
			}
			if identity["mode"] != test.wantMode || identity["label"] != test.wantLabel || identity["seq"] != test.wantSeq {
				t.Fatalf("subagent identity = %#v", identity)
			}
			timing, ok := values["subagentTiming"].(map[string]any)
			if !ok || timing["settledMs"] != test.wantSettled {
				t.Fatalf("subagent timing = %#v", values["subagentTiming"])
			}
			_, active := timing["active"]
			if active != test.wantActive {
				t.Fatalf("subagent active = %v, want %v", active, test.wantActive)
			}
		})
	}
}

func TestNativeProjectionFoldsDSHContextBreakdown(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: "request/header", At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"header":{"system":"12345678","tools":[{"name":"read"}]}}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"hello"}`)},
		{Seq: 3, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"ok"}`)},
		{Seq: 4, Type: session.EventCompactionSummary, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"shadowedRange":{"start":2,"end":3},"shadowedTokenCount":19}`)},
		{Seq: 5, Type: session.EventAssistantMessage, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"text":"new","surfaceOp":{"op":"replace","start":2,"end":3}}`)},
	}
	cursor := newNativeProjectionCursor()
	for _, ev := range events {
		cursor.project("context", ev)
	}
	value, ok := cursor.projectionBlock("", 5).Values["contextBreakdown"].(map[string]any)
	if !ok {
		t.Fatalf("contextBreakdown projection = %#v", cursor.projectionBlock("", 5).Values["contextBreakdown"])
	}
	if value["systemTokens"] != int64(6) || value["toolsTokens"] != nativeEstimateJSONTokens([]any{map[string]any{"name": "read"}})+4 {
		t.Fatalf("contextBreakdown request envelope = %#v", value)
	}
	if value["messageTokens"] != int64(9) {
		t.Fatalf("contextBreakdown replacement total = %#v, want 9", value)
	}
}

func TestNativeProjectionRejectsMalformedDSHSubagentDescriptor(t *testing.T) {
	cursor := newNativeProjectionCursor()
	cursor.project("subagent", session.Event{
		Seq: 1, Type: "subagent/descriptor", At: time.UnixMilli(100), Version: session.EventVersion,
		Data: json.RawMessage(`{"version":1,"mode":"continuable","label":"research"}`),
	})
	values := cursor.projectionBlock("", 1).Values
	if values["subagent"] != nil {
		t.Fatalf("malformed subagent projection = %#v, want nil", values["subagent"])
	}
	timing, ok := values["subagentTiming"].(map[string]any)
	if !ok || timing["settledMs"] != int64(0) {
		t.Fatalf("malformed subagent timing = %#v", values["subagentTiming"])
	}
}

func TestNativeProjectionIncludesImageLimitsWhenAttachmentsEnabled(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	seedSession(t, st, "native-image-limits", nil)

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"image-limits","method":"session.history","payload":{"sessionId":"native-image-limits"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	limits, ok := history.Projections.Values["imageLimits"].(map[string]any)
	if !ok {
		t.Fatalf("history imageLimits = %#v", history.Projections.Values["imageLimits"])
	}
	if limits["maxImageBytes"] != float64(maxWebImageBytes) || limits["maxImagesPerMessage"] != float64(20) || limits["maxImageDimension"] != float64(2000) {
		t.Fatalf("history imageLimits = %#v", limits)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"image-list","method":"session.list","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Projections == nil {
		t.Fatalf("session.list image limits items = %+v", list.Items)
	}
	if _, ok := list.Items[0].Projections.Values["imageLimits"].(map[string]any); !ok {
		t.Fatalf("session.list imageLimits = %#v", list.Items[0].Projections.Values["imageLimits"])
	}
}

func TestNativeSessionAttachmentRequiresReferenceAndReturnsDSHData(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	data := []byte("native-image")
	ref, err := att.SaveImage("image/png", data, maxWebImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	eventData, err := json.Marshal(map[string]any{
		"content": []any{map[string]any{
			"type": "image", "attachment": map[string]any{"attachmentId": ref.ID, "mediaType": ref.MediaType, "bytes": len(data)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, st, "native-attachment", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: eventData,
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.attachment", "tok", `{"type":"client-request","rpcId":"attachment-1","method":"session.attachment","payload":{"sessionId":"native-attachment","attachmentId":"`+ref.ID+`"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.attachment response = %+v", response)
	}
	var value struct {
		Attachment map[string]any `json:"attachment"`
		Data       string         `json:"data"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.Attachment["attachmentId"] != ref.ID || value.Attachment["mediaType"] != "image/png" || value.Attachment["width"] != float64(1) || value.Attachment["height"] != float64(1) {
		t.Fatalf("attachment ref = %#v", value.Attachment)
	}
	if value.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("attachment data = %q", value.Data)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.attachment", "tok", `{"type":"client-request","rpcId":"attachment-2","method":"session.attachment","payload":{"sessionId":"native-attachment","attachmentId":"not-referenced"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "attachment-error" {
		t.Fatalf("unreferenced attachment response = %+v", response)
	}
}

func TestNativeSessionModelsUsesStandardSessionMethod(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-models", nil)
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.models", "tok", `{"type":"client-request","rpcId":"models-1","method":"session.models","payload":{"sessionId":"native-models"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.models response = %+v", response)
	}
	var value map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["current"].(map[string]any); !ok || value["routable"] != false {
		t.Fatalf("session.models value = %#v", value)
	}
}

func TestNativeAgentPresetsListSelectAndLock(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any { return map[string]any{"mode": "code"} })

	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("presets-list", "agentPreset.list", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("agentPreset.list response = %+v", response)
	}
	var roster struct {
		Presets     []map[string]any `json:"presets"`
		Authorable  bool             `json:"authorable"`
		HasDocument bool             `json:"hasDocument"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.Presets) != 3 || roster.Authorable || roster.HasDocument {
		t.Fatalf("agent preset roster = %+v", roster)
	}
	for _, preset := range roster.Presets {
		if preset["id"] == "code" && preset["isDefault"] != true {
			t.Fatalf("code preset default flag = %#v", preset["isDefault"])
		}
	}

	if err := st.CreateSession(context.Background(), "blank-preset", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionConfig(context.Background(), "blank-preset", store.SessionConfig{Provider: "openai", Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	response = call("preset-select", "agentPreset.select", map[string]any{"sessionId": "blank-preset", "agentPreset": "minimal"})
	if !response.Result.OK {
		t.Fatalf("agentPreset.select response = %+v", response)
	}
	config, err := st.GetSessionConfig(context.Background(), "blank-preset")
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentPreset != "minimal" || config.Provider != "openai" || config.Model != "gpt-4o" {
		t.Fatalf("selected session config = %+v", config)
	}

	seedSession(t, st, "locked-preset", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"already used"}`),
	}})
	response = call("preset-locked", "agentPreset.select", map[string]any{"sessionId": "locked-preset", "agentPreset": "code"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "agent-preset-locked" {
		t.Fatalf("locked agent preset response = %+v", response)
	}
	response = call("preset-invalid", "agentPreset.select", map[string]any{"sessionId": "blank-preset", "agentPreset": "unknown"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "agent-preset-invalid" {
		t.Fatalf("invalid agent preset response = %+v", response)
	}
}

func TestNativeSessionSelectModelPersistsAndProjectsSelection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"provider": "deepseek-official", "model": "deepseek-chat"}
	})
	if err := st.CreateSession(context.Background(), "select-model", time.Now()); err != nil {
		t.Fatal(err)
	}
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("select-model", "session.selectModel", map[string]any{
		"sessionId": "select-model", "provider": "openai", "model": "gpt-4o", "reasoningEffort": "high",
	})
	if !response.Result.OK {
		t.Fatalf("session.selectModel response = %+v", response)
	}
	var selected struct {
		Selected map[string]any `json:"selected"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Selected["provider"] != "openai" || selected.Selected["model"] != "gpt-4o" || selected.Selected["reasoningEffort"] != "high" {
		t.Fatalf("selected model = %#v", selected.Selected)
	}
	config, err := st.GetSessionConfig(context.Background(), "select-model")
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider != "openai" || config.Model != "gpt-4o" || config.ReasoningEffort != "high" {
		t.Fatalf("persisted model config = %+v", config)
	}

	response = call("models-after-select", "session.models", map[string]any{"sessionId": "select-model"})
	if !response.Result.OK {
		t.Fatalf("session.models response = %+v", response)
	}
	var models struct {
		Current map[string]any `json:"current"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &models); err != nil {
		t.Fatal(err)
	}
	if models.Current["provider"] != "openai" || models.Current["model"] != "gpt-4o" || models.Current["reasoningEffort"] != "high" {
		t.Fatalf("projected model selection = %#v", models.Current)
	}

	response = call("select-missing", "session.selectModel", map[string]any{
		"sessionId": "missing", "provider": "openai", "model": "gpt-4o",
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "not-found" {
		code, message := "", ""
		if response.Result.Error != nil {
			code, message = response.Result.Error.Code, response.Result.Error.Message
		}
		t.Fatalf("missing session response = %+v (%s: %s)", response, code, message)
	}
	response = call("select-invalid", "session.selectModel", map[string]any{"sessionId": "select-model", "provider": "openai"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("invalid selection response = %+v", response)
	}
}

func TestNativeSessionCreatePersistsAgentPreset(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetSessionManager(func(ctx context.Context, action, requestedID string) (string, error) {
		if action != "new" {
			t.Fatalf("session action = %q, want new", action)
		}
		id := requestedID
		if id == "" {
			id = "created-preset"
		}
		if err := st.CreateSession(ctx, id, time.Now()); err != nil {
			return "", err
		}
		return id, nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok", `{"type":"client-request","rpcId":"create-preset","method":"session.create","payload":{"sessionId":"created-preset","agentPreset":"code"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.create response = %+v", response)
	}
	config, err := st.GetSessionConfig(context.Background(), "created-preset")
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentPreset != "code" {
		t.Fatalf("created session config = %+v", config)
	}
}

func TestNativeHostDirectoryBrowseAndCreate(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	root := t.TempDir()
	srv.SetDefaultWorkdir(root)
	for _, name := range []string{"alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("directory-list", "host.listDirectory", map[string]any{"path": root})
	if !response.Result.OK {
		t.Fatalf("host.listDirectory response = %+v", response)
	}
	var listing workspaceDirectoryListing
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != filepath.Clean(root) || listing.Truncated || len(listing.Crumbs) == 0 {
		t.Fatalf("directory listing metadata = %+v", listing)
	}
	names := make(map[string]workspaceDirectoryEntry, len(listing.Entries))
	for _, entry := range listing.Entries {
		names[entry.Name] = entry
	}
	if names["alpha"].Path != filepath.Join(root, "alpha") || !names[".hidden"].Hidden {
		t.Fatalf("directory entries = %+v", names)
	}
	if _, ok := names["file.txt"]; ok {
		t.Fatalf("file was returned as directory: %+v", names)
	}

	response = call("directory-default", "host.listDirectory", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("default host.listDirectory response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != filepath.Clean(root) {
		t.Fatalf("default directory path = %q, want %q", listing.Path, filepath.Clean(root))
	}

	response = call("directory-create", "host.createDirectory", map[string]any{"path": root, "name": "created"})
	if !response.Result.OK {
		t.Fatalf("host.createDirectory response = %+v", response)
	}
	var created struct {
		Path string `json:"path"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &created); err != nil {
		t.Fatal(err)
	}
	if created.Path != filepath.Join(root, "created") {
		t.Fatalf("created path = %q", created.Path)
	}

	response = call("directory-exists", "host.createDirectory", map[string]any{"path": root, "name": "created"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-exists" {
		t.Fatalf("duplicate directory response = %+v", response)
	}
	response = call("directory-invalid-name", "host.createDirectory", map[string]any{"path": root, "name": "../escape"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-create-failed" {
		t.Fatalf("invalid directory name response = %+v", response)
	}
	response = call("directory-relative", "host.listDirectory", map[string]any{"path": "relative"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-unreadable" {
		t.Fatalf("relative directory response = %+v", response)
	}
}

func TestNativeHostPickDirectoryUnavailableOutsideWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native picker is intentionally interactive on Windows")
	}
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/host.pickDirectory", "tok", `{"type":"client-request","rpcId":"pick","method":"host.pickDirectory","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-picker-unavailable" {
		t.Fatalf("host.pickDirectory response = %+v", response)
	}
}

func TestNativeSessionHistorySeedsProjectionBeforeSelectingMessagePage(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-paged", []session.Event{
		{Seq: 0, Type: session.EventTurnStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"first"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"one"}`)},
		{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 4, Type: session.EventTurnStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"turn":2}`)},
		{Seq: 5, Type: session.EventUserMessage, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"text":"second"}`)},
		{Seq: 6, Type: session.EventAssistantMessage, At: time.UnixMilli(1006), Version: session.EventVersion, Data: json.RawMessage(`{"text":"two"}`)},
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"history-page","method":"session.history","payload":{"sessionId":"native-paged","maxMessages":1}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Events  []nativeHistoryEntry `json:"events"`
		HasMore bool                 `json:"hasMore"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 || history.Events[0].Event.Seq != 6 || !history.HasMore {
		t.Fatalf("paged history = %+v", history)
	}
	var assistant struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(history.Events[0].Event.Data, &assistant); err != nil {
		t.Fatal(err)
	}
	if assistant.Turn != 2 {
		t.Fatalf("page projection turn = %d, want 2", assistant.Turn)
	}
}

func TestNativeHistoryPageBoundsSkipsReplacementAndKeepsMessageSources(t *testing.T) {
	input := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","sourceEventSeqs":[1,2]}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":1,"end":2}}`)},
	}
	cursor := newNativeProjectionCursor()
	projected := make([]nativeSessionEvent, 0, len(input))
	for _, event := range input {
		projected = append(projected, cursor.project("paged", event))
	}

	start, end := nativeHistoryPageBounds(projected, nil, 1)
	if start != 0 || end != len(projected) {
		t.Fatalf("replacement page bounds = (%d,%d), want (0,%d)", start, end, len(projected))
	}

	start, end = nativeHistoryPageBounds(projected, uint64Ptr(3), 1)
	if start != 0 || end != 2 {
		t.Fatalf("source-group page bounds = (%d,%d), want (0,2)", start, end)
	}
}

func uint64Ptr(value uint64) *uint64 {
	return &value
}

func TestNativeProjectionUsesOneDSHShapeForReplayAndLive(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"hello"}`)},
		{Seq: 3, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","toolCalls":[{"ID":"c1","Name":"read","Arguments":"{}"}]}`)},
		{Seq: 4, Type: session.EventToolResult, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"turn":0,"step":0,"callId":"c1","name":"read","output":"ok"}`)},
	}
	replayCursor := newNativeProjectionCursor()
	liveCursor := newNativeProjectionCursor()
	for _, ev := range events {
		replayed := replayCursor.project("s1", ev)
		live := liveCursor.project("s1", ev)
		if string(replayed.Data) != string(live.Data) || replayed.Type != live.Type || replayed.SurfaceOp != live.SurfaceOp {
			t.Fatalf("replay/live projection differs for seq %d: replay=%s live=%s", ev.Seq, replayed.Data, live.Data)
		}
	}
	assistant := replayCursor.project("s1", events[2])
	var assistantData struct {
		Turn    int `json:"turn"`
		Step    int `json:"step"`
		Message struct {
			ID      string `json:"id"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(assistant.Data, &assistantData); err != nil {
		t.Fatal(err)
	}
	if assistantData.Turn != 0 || assistantData.Step != 0 || assistantData.Message.ID == "" || len(assistantData.Message.Content) != 2 {
		t.Fatalf("native assistant projection = %+v", assistantData)
	}
}

func TestNativeProjectionFoldsSurfaceReplacementAndDiagnostics(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer"}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":1,"end":2}}`)},
		{Seq: 4, Type: session.EventLLMRetry, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"attempt":2,"maxRetries":3,"delayMs":25,"error":"temporary"}`)},
		{Seq: 5, Type: session.EventJobStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"jobId":"j1"}`)},
	}
	cursor := newNativeProjectionCursor()
	projected := make([]nativeSessionEvent, 0, len(events))
	for _, ev := range events {
		projected = append(projected, cursor.project("s1", ev))
	}
	if got := projected[2].SurfaceOp; got == nil {
		t.Fatal("compaction projection is missing surfaceOp")
	}
	var surface struct {
		Op    string `json:"op"`
		Start uint64 `json:"start"`
		End   uint64 `json:"end"`
	}
	if err := json.Unmarshal(nativeJSONBytes(projected[2].SurfaceOp), &surface); err != nil {
		t.Fatal(err)
	}
	if surface.Op != "replace" || surface.Start != 1 || surface.End != 2 || len(projected[2].SourceEventSeqs) != 2 {
		t.Fatalf("surface projection = %+v, sources=%v", surface, projected[2].SourceEventSeqs)
	}
	var retry struct {
		RetryID string `json:"retryId"`
		Retry   int    `json:"retry"`
		Failure struct {
			Message string `json:"message"`
		} `json:"failure"`
	}
	if err := json.Unmarshal(projected[3].Data, &retry); err != nil {
		t.Fatal(err)
	}
	if retry.RetryID == "" || retry.Retry != 2 || retry.Failure.Message != "temporary" {
		t.Fatalf("retry projection = %+v", retry)
	}
	if !projected[4].Ignorable {
		t.Fatal("non-DSH event must be marked ignorable")
	}
}

func TestNativeRPCRejectsMethodMismatchAndAuth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"bad-1","method":"host.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("method mismatch response = %+v", response)
	}
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "", `{"type":"client-request","rpcId":"bad-2","method":"session.list","payload":{}}`); rec.Code != 401 {
		t.Fatalf("missing auth status = %d", rec.Code)
	}
}

func TestNativeSettingsDescribeAndMutate(t *testing.T) {
	srv, _ := newTestServer(t, "tok")

	rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.describe", "tok", `{"type":"client-request","rpcId":"settings-1","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe response = %+v", response)
	}
	var described struct {
		Namespaces []map[string]any `json:"namespaces"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatal(err)
	}
	if len(described.Namespaces) != 1 || described.Namespaces[0]["ns"] != nativeSettingsOnboarding {
		t.Fatalf("settings namespaces = %+v", described.Namespaces)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/settings.mutate", "tok", `{"type":"client-request","rpcId":"settings-2","method":"settings.mutate","payload":{"ns":"ui-onboarding","ops":[{"op":"set","path":["welcomeNoticeVersion"],"value":"2026-08-13.1"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.mutate response = %+v", response)
	}
	var view struct {
		NS       string         `json:"ns"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view.NS != nativeSettingsOnboarding || view.Value["welcomeNoticeVersion"] != "2026-08-13.1" || view.Revision != 1 {
		t.Fatalf("settings view = %+v", view)
	}
}

func TestNativeLLMCatalogUsesSanitizedConfig(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{
			{
				"id": "deepseek-official", "name": "DeepSeek", "available": true,
				"configured": true, "env_var": "DEEPSEEK_API_KEY", "candidates": []string{"deepseek-v4-flash"},
			},
		}}
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/llm.providers", "tok", `{"type":"client-request","rpcId":"llm-1","method":"llm.providers","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.providers response = %+v", response)
	}
	var providers struct {
		Providers []map[string]any `json:"providers"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0]["provider"] != "deepseek-official" {
		t.Fatalf("llm providers = %+v", providers.Providers)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/llm.models", "tok", `{"type":"client-request","rpcId":"llm-2","method":"llm.models","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.models response = %+v", response)
	}
	var models struct {
		Groups []map[string]any `json:"groups"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Groups) != 1 {
		t.Fatalf("llm model groups = %+v", models.Groups)
	}
}

func TestNativeMuxWebSocketSendsSubscriptionBaseline(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-ws", nil)
	var emit func(session.Event)
	registered := make(chan struct{})
	srv.SetEventSource(func(_ string, callback func(session.Event)) func() {
		emit = callback
		close(registered)
		return func() {}
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUta2V5\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	frame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Payload nativeSubscribedFrame `json:"payload"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.Type != "session/subscribed" || envelope.Payload.SessionID != "native-ws" || envelope.Payload.LastSeq != -1 {
		t.Fatalf("subscription frame = %s", frame)
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("mux did not register an event callback")
	}
	emit(session.Event{Seq: 1, Type: session.EventPlanMode, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{"active":true}`)})
	eventFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	projectionFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var eventEnvelope struct {
		Payload nativeSessionEventFrame `json:"payload"`
	}
	if err := json.Unmarshal(eventFrame, &eventEnvelope); err != nil {
		t.Fatal(err)
	}
	if eventEnvelope.Payload.Type != "session/event" || eventEnvelope.Payload.Event.Seq != 1 {
		t.Fatalf("live event frame = %s", eventFrame)
	}
	var projectionEnvelope struct {
		Payload nativeProjectionFrame `json:"payload"`
	}
	if err := json.Unmarshal(projectionFrame, &projectionEnvelope); err != nil {
		t.Fatal(err)
	}
	if projectionEnvelope.Payload.Type != "session/projection" || projectionEnvelope.Payload.Key != "plan" || projectionEnvelope.Payload.Seq != 1 {
		t.Fatalf("live projection frame = %s", projectionFrame)
	}
}

func readNativeTextFrame(reader *bufio.Reader) ([]byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first&0x0f != 1 {
		return nil, fmt.Errorf("opcode = %d", first&0x0f)
	}
	second, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	length := int(second & 0x7f)
	if length == 126 {
		var bytesLength [2]byte
		if _, err := io.ReadFull(reader, bytesLength[:]); err != nil {
			return nil, err
		}
		length = int(bytesLength[0])<<8 | int(bytesLength[1])
	} else if length == 127 {
		return nil, fmt.Errorf("unexpected large frame")
	}
	if second&0x80 != 0 {
		return nil, fmt.Errorf("server frame is masked")
	}
	payload := make([]byte, length)
	_, err = io.ReadFull(reader, payload)
	return payload, err
}
