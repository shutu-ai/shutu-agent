package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

func TestStreamReasoningEffortSerializesReasoningConfig(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sseResponseEvent("response.completed", `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)+"\n\n")
	}))
	defer srv.Close()
	catalogReasoning := true
	p := New(Config{BaseURL: srv.URL, APIKey: "k", ModelCatalog: []llm.ModelInfo{{ID: "gpt-5", Reasoning: &catalogReasoning}}})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model: "gpt-5", ReasoningEffort: "high",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}
	include, ok := body["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", body["include"])
	}
}
