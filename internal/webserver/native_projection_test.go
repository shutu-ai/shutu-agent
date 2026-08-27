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
