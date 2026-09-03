package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

func TestStreamReasoningEffortSerializesThinkingConfig(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sseData(`{"candidates":[{"finishReason":"STOP"}]}`)+"\n\n")
	}))
	defer srv.Close()
	reasoning := true
	p := New(Config{BaseURL: srv.URL, APIKey: "k", ModelCatalog: []llm.ModelInfo{{ID: "gemini-2.5-flash", Reasoning: &reasoning}}})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model: "gemini-2.5-flash", ReasoningEffort: "high", ReasoningBudgetTokens: 2048,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	generation, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig = %#v", body["generationConfig"])
	}
	thinking, ok := generation["thinkingConfig"].(map[string]any)
	if !ok || thinking["includeThoughts"] != true || thinking["thinkingBudget"] != float64(2048) {
		t.Fatalf("thinkingConfig = %#v", generation["thinkingConfig"])
	}
}
