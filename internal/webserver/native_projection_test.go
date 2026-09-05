package webserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/meter"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

func TestNativeProjectionBaselineIncludesMountedDSHKeys(t *testing.T) {
	values := newNativeProjectionCursor().projectionBlock("", -1).Values
	want := []string{
		"title", "todos", "plan", "tokenUsage", "contextPressure", "contextBreakdown",
		"goal", "permissions", "subagent", "subagentTiming", "sessionListMetadata", "sessionStats",
		"completionLedger",
	}
	for _, key := range want {
		if _, ok := values[key]; !ok {
			t.Fatalf("projection baseline is missing DSH key %q: %#v", key, values)
		}
	}
	if values["title"] != nil || values["goal"] != nil || values["todos"] != nil {
		t.Fatalf("empty projection sent non-null optional values: %#v", values)
	}
	plan, ok := values["plan"].(map[string]any)
	if !ok || plan["active"] != false || plan["pending"] != false {
		t.Fatalf("initial plan projection = %#v", values["plan"])
	}
	usage, ok := values["tokenUsage"].(map[string]any)
	if !ok || usage["uncachedInputTokens"] != int64(0) || usage["outputTokens"] != int64(0) {
		t.Fatalf("initial token usage projection = %#v", values["tokenUsage"])
	}
}

func TestNativeProjectionCompletionLedgerMatchesMeterReplay(t *testing.T) {
	cursor := newNativeProjectionCursor()
	assistant := session.Event{
		Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"answer","reasoning":"think","toolCalls":[{"id":"call-1","name":"read","arguments":"{}"}]}`),
	}
	toolResult := session.Event{
		Seq: 2, Type: session.EventToolResult, At: time.UnixMilli(1002), Version: session.EventVersion,
		Data: json.RawMessage(`{"callId":"call-1","name":"read","output":"ok"}`),
	}
	cursor.project("completion-ledger", assistant)
	projected := cursor.project("completion-ledger", toolResult)
	values := cursor.projectionBlock("", 2).Values
	got, ok := values["completionLedger"].(map[string]any)
	if !ok {
		t.Fatalf("completion ledger projection = %#v", values["completionLedger"])
	}
	wantProjection := meter.ProjectCompletion([]session.Event{assistant, toolResult})
	if got["assistantTokens"] != int64(wantProjection.AssistantTokens) ||
		got["reasoningTokens"] != int64(wantProjection.ReasoningTokens) ||
		got["toolCallTokens"] != int64(wantProjection.ToolCallTokens) ||
		got["toolResultTokens"] != int64(wantProjection.ToolResultTokens) ||
		got["attachmentBytes"] != wantProjection.AttachmentBytes {
		t.Fatalf("native completion ledger = %#v, want %+v", got, wantProjection)
	}
	if projected.Type != session.EventToolResult {
		t.Fatalf("tool result projection type = %q", projected.Type)
	}
}

func TestNativeProjectionNormalizesLegacyInboxInsertedNull(t *testing.T) {
	cursor := newNativeProjectionCursor()
	projected := cursor.project("legacy-inbox", session.Event{
		Seq: 1, Type: session.EventAgentInboxSpliced, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"target":"next-step","start":0,"removedCount":1,"inserted":null,"outcome":"canceled"}`),
	})
	var data map[string]any
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	inserted, ok := data["inserted"].([]any)
	if !ok || len(inserted) != 0 {
		t.Fatalf("legacy inbox inserted = %#v, want []", data["inserted"])
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ValidateWireEvent(encoded); err != nil {
		t.Fatalf("normalized inbox event rejected: %v\n%s", err, encoded)
	}
}

func TestNativeProjectionPreservesSkillInvocationSource(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:       llm.RoleUser,
		Content:    []llm.ContentBlock{llm.Text("<skill_content name=\"review-bash\">body</skill_content>")},
		SourceKind: "skill-invocation",
		SourceName: "review-bash",
		SourceForm: "instructions",
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("skill-source", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			Form string `json:"form"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "skill-invocation" ||
		data.Source.Name != "review-bash" || data.Source.Form != "instructions" {
		t.Fatalf("native skill source = %+v, want skill-invocation/review-bash/instructions", data.Source)
	}
}

func TestNativeProjectionPreservesRuntimeSnapshotSections(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:         llm.RoleUser,
		Content:      []llm.ContentBlock{llm.Text("Current runtime context.")},
		SourceKind:   "plugin",
		SourcePlugin: "@shutu-ai/system-prompt",
		SourceForm:   "snapshot",
		SourceSections: []llm.ContextSnapshotSection{{
			Name: "workspace", Text: "Working directory: D:/work",
		}},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("runtime-snapshot", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind     string `json:"kind"`
			Plugin   string `json:"plugin"`
			Form     string `json:"form"`
			Sections []struct {
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"sections"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "plugin" ||
		data.Source.Plugin != "@shutu-ai/system-prompt" || data.Source.Form != "snapshot" ||
		len(data.Source.Sections) != 1 || data.Source.Sections[0].Name != "workspace" ||
		data.Source.Sections[0].Text != "Working directory: D:/work" {
		t.Fatalf("native runtime source = %+v, want DSH snapshot sections", data.Source)
	}
}

func TestNativeProjectionPreservesAgentInstructionBaselineSource(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:                   llm.RoleUser,
		Content:                []llm.ContentBlock{llm.Text("<system-reminder>\nWorkspace rules\n</system-reminder>")},
		SourceKind:             "agent-instructions",
		SourceForm:             "instructions",
		SourceBaseline:         true,
		SourceBaselineIdentity: `{"projectRoot":"."}`,
		SourceChanges: []map[string]any{{
			"action": "set", "scope": ".", "path": "AGENTS.md",
			"digest": "digest-1",
		}},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("instruction-baseline", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind             string              `json:"kind"`
			Form             string              `json:"form"`
			Baseline         bool                `json:"baseline"`
			BaselineIdentity string              `json:"baselineIdentity"`
			Changes          []map[string]string `json:"changes"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "agent-instructions" ||
		data.Source.Form != "instructions" || !data.Source.Baseline ||
		data.Source.BaselineIdentity != `{"projectRoot":"."}` || len(data.Source.Changes) != 1 ||
		data.Source.Changes[0]["path"] != "AGENTS.md" {
		t.Fatalf("native instruction source = %+v, want complete DSH baseline", data.Source)
	}
}

func TestNativeProjectionPreservesNoticeSummary(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:          llm.RoleUser,
		Content:       []llm.ContentBlock{llm.Text("bash test [completed]")},
		SourceKind:    "plugin",
		SourcePlugin:  "tool-jobs",
		SourceForm:    "notice",
		SourceSummary: "bash test [completed]",
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("job-notice", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind    string `json:"kind"`
			Plugin  string `json:"plugin"`
			Form    string `json:"form"`
			Summary string `json:"summary"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "plugin" || data.Source.Plugin != "tool-jobs" ||
		data.Source.Form != "notice" || data.Source.Summary != "bash test [completed]" {
		t.Fatalf("native notice source = %+v, want DSH summary", data.Source)
	}
}

func TestNativeProjectionPreservesSubagentSettledNotice(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:                  llm.RoleUser,
		Content:               []llm.ContentBlock{llm.Text("Background subagent child-1 finished and will do no further work unless you send it more.")},
		SourceKind:            "subagent-settled",
		SourceForm:            "notice",
		SourceSummary:         "Background subagent child-1 finished and will do no further work unless you send it more.",
		SourceSenderSessionID: "child-1",
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("subagent-notice", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind            string `json:"kind"`
			Form            string `json:"form"`
			Summary         string `json:"summary"`
			SenderSessionID string `json:"senderSessionId"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "subagent-settled" ||
		data.Source.Form != "notice" || data.Source.SenderSessionID != "child-1" ||
		data.Source.Summary != "Background subagent child-1 finished and will do no further work unless you send it more." {
		t.Fatalf("native subagent source = %+v, want DSH settlement notice", data.Source)
	}
}

func TestNativeProjectionPreservesSubagentReportRelay(t *testing.T) {
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:                  llm.RoleUser,
		Content:               []llm.ContentBlock{llm.Text("Background subagent child-1 reported:\nselected findings")},
		SourceKind:            "subagent-report",
		SourceForm:            "relay",
		SourceSenderSessionID: "child-1",
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("subagent-report", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind            string `json:"kind"`
			Form            string `json:"form"`
			SenderSessionID string `json:"senderSessionId"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "subagent-report" ||
		data.Source.Form != "relay" || data.Source.SenderSessionID != "child-1" {
		t.Fatalf("native subagent report source = %+v, want DSH relay", data.Source)
	}
}

func TestNativeProjectionPreservesSessionReferenceRecall(t *testing.T) {
	references := []map[string]any{{
		"sessionId": "source", "label": "Source", "capturedThroughSeq": 7,
		"compacted": true, "originalMessages": 2, "retainedMessages": 1,
		"omittedMessages": 1, "omittedBytes": 12, "truncated": true, "inputIndex": 0,
	}}
	payload := session.NewUserMessageAt(1, 1, 1, llm.Message{
		Role:             llm.RoleUser,
		Content:          []llm.ContentBlock{llm.Text(`[{"sessionId":"source","label":"Source"}]`)},
		SourceKind:       "session-reference",
		SourceForm:       "recall",
		SourceReferences: references,
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	projected := cursor.project("session-recall", session.Event{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: raw,
	})
	var data struct {
		Source *struct {
			Kind       string           `json:"kind"`
			Form       string           `json:"form"`
			References []map[string]any `json:"references"`
		} `json:"source"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "session-reference" || data.Source.Form != "recall" ||
		len(data.Source.References) != 1 || data.Source.References[0]["label"] != "Source" {
		t.Fatalf("native session recall source = %+v", data.Source)
	}
}

func TestNativeSurfaceSnapshotUsesSharedProjection(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer"}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":1,"end":2}}`)},
	}
	cursor := newNativeProjectionCursor()
	for _, event := range events {
		cursor.project("shared-surface", event)
	}
	canonical, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	value := cursor.surfaceSnapshot()
	nodes, ok := value["nodes"].([]any)
	if !ok || len(nodes) != len(canonical.Surface) || nodes[0] != uint64(3) {
		t.Fatalf("native surface snapshot = %#v, canonical surface = %#v", value, canonical.Surface)
	}
}

func TestNativeControlValuesUseSharedProjection(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventPlanMode, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"active":true,"pending":false}`)},
		{Seq: 2, Type: session.EventTodoWrite, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"todos":[{"id":"todo-1","content":"ship","status":"in_progress"}]}`)},
	}
	canonical, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	for _, event := range events {
		cursor.project("shared-controls", event)
	}
	values := cursor.projectionBlock("", 2).Values
	plan, ok := values["plan"].(map[string]any)
	if !ok || plan["active"] != canonical.PlanMode.Active || plan["pending"] != canonical.PlanMode.Pending {
		t.Fatalf("native plan=%#v canonical=%#v", values["plan"], canonical.PlanMode)
	}
	todos, ok := values["todos"].([]any)
	if !ok || len(todos) != 1 || todos[0].(map[string]any)["content"] != "ship" || todos[0].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("native todos=%#v canonical=%#v", values["todos"], canonical.Todos)
	}
}

func TestNativeTodoPlanValuesUseSharedProjection(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventPlanCreate, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","title":"ship native UI"}`)},
		{Seq: 2, Type: session.EventPlanStatus, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","status":"in-progress"}`)},
	}
	canonical, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	cursor := newNativeProjectionCursor()
	for _, event := range events {
		cursor.project("shared-plan-todo", event)
	}
	todos, ok := cursor.projectionBlock("", 2).Values["todos"].([]any)
	if !ok || len(todos) != 1 {
		t.Fatalf("native plan todo=%#v canonical plans=%#v", cursor.projectionBlock("", 2).Values["todos"], canonical.Plans)
	}
	item := todos[0].(map[string]any)
	if item["content"] != "ship native UI" || item["status"] != "in_progress" {
		t.Fatalf("native plan todo item=%#v canonical plans=%#v", item, canonical.Plans)
	}
}

func TestNativeProjectionReplaysSharedCoreFixture(t *testing.T) {
	events, err := contractfixture.CoreTurnEvents()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	cursor := newNativeProjectionCursor()
	for _, record := range events {
		event := session.Event{Seq: record.Seq, Type: record.Type, At: time.UnixMilli(record.Time), Version: session.EventVersion, Data: record.Data}
		projected := cursor.project("fixture-session", event)
		encoded, err := json.Marshal(projected)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.ValidateWireEvent(encoded); err != nil {
			t.Fatalf("projected %s rejected: %v\n%s", event.Type, err, encoded)
		}
	}
}

func TestNativeProjectionClampsNegativeCoordinatesAtWireBoundary(t *testing.T) {
	cursor := newNativeProjectionCursor()
	projected := cursor.project("negative-coordinates", session.Event{
		Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{"turn":-1,"step":-1,"text":"reply"}`),
	})
	var data struct {
		Turn    int `json:"turn"`
		Step    int `json:"step"`
		Message struct {
			Content []any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Turn != 0 || data.Step != 0 || len(data.Message.Content) == 0 {
		t.Fatalf("negative assistant coordinates = %+v", data)
	}

	chunk := cursor.project("negative-coordinates", session.Event{
		Seq: 2, Type: session.EventAssistantChunk, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"turn":-1,"step":-1,"text":"chunk"}`),
	})
	var chunkData struct {
		Turn int `json:"turn"`
		Step int `json:"step"`
	}
	if err := json.Unmarshal(chunk.Data, &chunkData); err != nil {
		t.Fatal(err)
	}
	if chunkData.Turn != 0 || chunkData.Step != 0 {
		t.Fatalf("negative assistant chunk coordinates = %+v", chunkData)
	}
}

func TestNativeProjectionKeepsImplicitTurnStartAlignedWithToolEvents(t *testing.T) {
	cursor := newNativeProjectionCursor()
	turnStart := cursor.project("turn-alignment", session.Event{
		Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{}`),
	})
	toolCall := cursor.project("turn-alignment", session.Event{
		Seq: 2, Type: session.EventToolCall, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","name":"read","arguments":"{}"}`),
	})
	toolResult := cursor.project("turn-alignment", session.Event{
		Seq: 3, Type: session.EventToolResult, At: time.UnixMilli(1002), Version: session.EventVersion,
		Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","output":"ok"}`),
	})
	for _, projected := range []nativeSessionEvent{turnStart, toolCall, toolResult} {
		var data map[string]any
		if err := json.Unmarshal(projected.Data, &data); err != nil {
			t.Fatalf("decode %s: %v", projected.Type, err)
		}
		if got, ok := data["turn"].(float64); !ok || got != 1 {
			t.Fatalf("%s turn = %#v, want 1", projected.Type, data["turn"])
		}
	}
}

func TestNativeProjectionConvertsRequestStartToDSHRequestHeader(t *testing.T) {
	cursor := newNativeProjectionCursor()
	projected := cursor.project("request-header", session.Event{
		Seq: 7, Type: session.EventLLMRequestStart, At: time.UnixMilli(1007), Version: session.EventVersion,
		Data: json.RawMessage(`{"requestId":"turn:1:step:1","provider":"deepseek-official","model":"deepseek-v4-flash","reasoningEffort":"high","maxTokens":321,"temperature":0.25,"stop":["<END>"],"messages":[{"role":"system","text":"Initial System Prompt"},{"role":"user","text":"hi"}],"tools":[{"name":"read","description":"Read files","parameters":{"type":"object"}}]}`),
	})
	if projected.Type != "request/header" {
		t.Fatalf("projected request type = %q, want request/header", projected.Type)
	}
	var data struct {
		Header struct {
			Config map[string]any   `json:"config"`
			System string           `json:"system"`
			Tools  []map[string]any `json:"tools"`
		} `json:"header"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(projected.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Reason != "initial" || data.Header.System != "Initial System Prompt" {
		t.Fatalf("request header prompt = %+v", data)
	}
	if data.Header.Config["provider"] != "deepseek-official" || data.Header.Config["model"] != "deepseek-v4-flash" || data.Header.Config["reasoningEffort"] != "high" {
		t.Fatalf("request header config = %+v", data.Header.Config)
	}
	if data.Header.Config["maxTokens"] != float64(321) || data.Header.Config["temperature"] != 0.25 {
		t.Fatalf("request header generation controls = %+v", data.Header.Config)
	}
	stop, ok := data.Header.Config["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "<END>" {
		t.Fatalf("request header stop = %#v", data.Header.Config["stop"])
	}
	if len(data.Header.Tools) != 1 || data.Header.Tools[0]["name"] != "read" {
		t.Fatalf("request header tools = %+v", data.Header.Tools)
	}
	updated := cursor.project("request-header", session.Event{
		Seq: 8, Type: session.EventLLMRequestStart, At: time.UnixMilli(1008), Version: session.EventVersion,
		Data: json.RawMessage(`{"provider":"deepseek-official","model":"deepseek-v4-flash","messages":[{"role":"system","text":"Updated Prompt"}],"tools":[]}`),
	})
	var updatedData struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(updated.Data, &updatedData); err != nil {
		t.Fatal(err)
	}
	if updatedData.Reason != "update" {
		t.Fatalf("subsequent request header reason = %q, want update", updatedData.Reason)
	}
}

func TestNativeProjectionSeedsContextMeterCapacity(t *testing.T) {
	cursor := newNativeProjectionCursor()
	cursor.setContextWindow(128000)
	cursor.project("context-meter", session.Event{
		Seq: 1, Type: session.EventLLMRequestEnd, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"usage":{"inputTokens":100,"cachedInputTokens":40,"cacheWriteTokens":5}}`),
	})
	values := cursor.projectionBlock("", 1).Values
	pressure, ok := values["contextPressure"].(map[string]any)
	if !ok {
		t.Fatalf("context pressure projection = %#v", values["contextPressure"])
	}
	if pressure["contextWindow"] != 128000 || pressure["pressureTokens"] != int64(145) {
		t.Fatalf("context meter pressure = %#v, want window=128000 pressure=145", pressure)
	}
}
