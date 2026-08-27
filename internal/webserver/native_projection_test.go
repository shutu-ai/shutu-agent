package webserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func TestNativeProjectionBaselineIncludesMountedDSHKeys(t *testing.T) {
	values := newNativeProjectionCursor().projectionBlock("", -1).Values
	want := []string{
		"title", "todos", "plan", "tokenUsage", "contextPressure", "contextBreakdown",
		"goal", "permissions", "subagent", "subagentTiming", "sessionListMetadata", "sessionStats",
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
		Data: json.RawMessage(`{"requestId":"turn:1:step:1","provider":"deepseek-official","model":"deepseek-v4-flash","reasoningEffort":"high","messages":[{"role":"system","text":"Initial System Prompt"},{"role":"user","text":"hi"}],"tools":[{"name":"read","description":"Read files","parameters":{"type":"object"}}]}`),
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
