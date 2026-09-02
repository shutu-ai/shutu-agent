package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

// newTestProvider starts a fake Responses endpoint and returns a provider
// pointed at it.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *openaiResponsesProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key"})
}

// sseResponseEvent renders one Responses SSE event ("event: <name>\ndata: <data>").
func sseResponseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data
}

// TestID returns the stable provider id.
func TestID(t *testing.T) {
	if got := New(Config{}).ID(); got != "openai-responses" {
		t.Fatalf("ID = %q, want openai-responses", got)
	}
	if got := New(Config{ID: "xai"}).ID(); got != "xai" {
		t.Fatalf("custom ID = %q, want xai", got)
	}
}

// TestAvailable verifies the cheap local availability check.
func TestAvailable(t *testing.T) {
	if !New(Config{APIKey: "k"}).Available() {
		t.Error("key + default base URL must be available")
	}
	if New(Config{}).Available() {
		t.Error("empty key must be unavailable")
	}
	for _, bad := range []string{":", "://", "not a url"} {
		if New(Config{APIKey: "k", BaseURL: bad}).Available() {
			t.Errorf("base URL %q must be unavailable", bad)
		}
	}
}

// TestStreamTextAndReasoning verifies the core Responses wire: reasoning_text
// deltas become StreamReasoningDelta, output_text deltas become
// StreamTextDelta, and response.completed maps status completed → stop. It also
// asserts the request went to {base}/responses with the Bearer header.
func TestStreamTextAndReasoning(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotModel string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if raw, err := io.ReadAll(r.Body); err == nil && json.Unmarshal(raw, &gotBody) == nil {
			gotModel, _ = gotBody["model"].(string)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseResponseEvent("response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","delta":"think step 1"}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hello "}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"world"}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.completed", `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model: "request-selected-model", MaxTokens: 123,
		Temperature: func() *float64 { v := 0.2; return &v }(), Stop: []string{"END"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text, reasoning, finish string
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		switch ev.Kind {
		case llm.StreamTextDelta:
			text += ev.Text
		case llm.StreamReasoningDelta:
			reasoning += ev.Text
		case llm.StreamFinish:
			finish = ev.FinishReason
		}
	}
	if text != "Hello world" {
		t.Errorf("text = %q, want %q", text, "Hello world")
	}
	if reasoning != "think step 1" {
		t.Errorf("reasoning = %q, want %q", reasoning, "think step 1")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if gotPath != "/responses" {
		t.Errorf("path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q, want Bearer k", gotAuth)
	}
	if gotModel != "request-selected-model" {
		t.Errorf("model = %q, want request-selected-model", gotModel)
	}
	if gotBody["max_output_tokens"] != float64(123) || gotBody["temperature"] != float64(0.2) {
		t.Errorf("generation controls = max_output_tokens:%v temperature:%v", gotBody["max_output_tokens"], gotBody["temperature"])
	}
	stops, _ := gotBody["stop"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop = %v", gotBody["stop"])
	}
}

// TestStreamFunctionCall verifies function_call_arguments deltas accumulate
// into a completed llm.ToolCall (id = the Responses call_id) when the stream
// ends without an explicit completed event (dsh 范式: the tool call is carried
// by the finish event).
func TestStreamFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseResponseEvent("response.output_item.added", `{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather"}}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item":{"id":"fc_1"},"delta":"{\"city\":\""}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item":{"id":"fc_1"},"delta":"Hangzhou\"}"}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.output_item.done", `{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Hangzhou\"}"}}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.completed", `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var calls []llm.ToolCall
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind == llm.StreamFinish {
			calls = ev.ToolCalls
		}
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" {
		t.Errorf("call id = %q, want call_1", calls[0].ID)
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("call name = %q", calls[0].Name)
	}
	if calls[0].Arguments != `{"city":"Hangzhou"}` {
		t.Errorf("arguments = %q", calls[0].Arguments)
	}
}

// TestStreamIncompleteStopReason maps response.incomplete → length (pi-ai
// mapStopReason 范式).
func TestStreamIncompleteStopReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseResponseEvent("response.incomplete", `{"type":"response.incomplete","reason":"max_output_tokens"}`) + "\n\n"))
		w.Write([]byte(sseResponseEvent("response.completed", `{"type":"response.completed","response":{"id":"r1","status":"incomplete"}}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var finish string
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind == llm.StreamFinish {
			finish = ev.FinishReason
		}
	}
	if finish != "length" {
		t.Errorf("finish = %q, want length", finish)
	}
}

// TestStreamProviderError verifies a non-2xx error surfaces the message.
func TestStreamProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if _, err := p.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("error = %v", err)
	}
}

// TestStreamImageFailClosed verifies an image request errors on a model that
// does not declare image input (dispatch-m8-3b §3).
func TestStreamImageFailClosed(t *testing.T) {
	p := New(Config{APIKey: "k"}) // SupportsImages=false
	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{Path: "x.png"}},
		}}},
	})
	if err == nil {
		t.Fatal("expected image fail-closed error")
	}
}

func TestStreamUsesExplicitModelDefaultMaxOutputTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sseResponseEvent("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "test-key", ModelCatalog: []llm.ModelInfo{{ID: "model", DefaultMaxTokens: 777}}})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{Model: "model", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
	}
	if got["max_output_tokens"] != float64(777) {
		t.Fatalf("max_output_tokens = %#v, want 777", got["max_output_tokens"])
	}
}

func TestStreamUsesReferenceRouteDefaultMaxOutputTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sseResponseEvent("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "test-key"})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{Model: "unlisted", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
	}
	if got["max_output_tokens"] != float64(32768) {
		t.Fatalf("max_output_tokens = %#v, want reference route default 32768", got["max_output_tokens"])
	}
}
