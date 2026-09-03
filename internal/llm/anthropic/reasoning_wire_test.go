package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

func TestStreamReasoningEffortSerializesThinking(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sseEvents(
			sseEventLine("message_start", `{"type":"message_start","message":{"id":"m1"}}`),
			sseEventLine("message_stop", `{"type":"message_stop"}`),
		))
	}))
	defer srv.Close()
	reasoning := true
	p := New(Config{BaseURL: srv.URL, APIKey: "k", ModelCatalog: []llm.ModelInfo{{ID: "claude-sonnet-4-5", Reasoning: &reasoning}}})
	temperature := 0.2
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model: "claude-sonnet-4-5", ReasoningEffort: "high", ReasoningBudgetTokens: 2048,
		Temperature: &temperature, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, reader)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	if _, ok := body["temperature"]; ok {
		t.Fatalf("temperature must be omitted with extended thinking: %#v", body)
	}
}
