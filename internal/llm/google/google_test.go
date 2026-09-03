package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// newTestProvider starts a fake Gemini endpoint and returns a provider pointed
// at it.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *googleProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key"})
}

// sseData renders one SSE data payload.
func sseData(payload string) string { return "data: " + payload }

// TestID returns the stable provider id.
func TestID(t *testing.T) {
	if got := New(Config{}).ID(); got != "google" {
		t.Fatalf("ID = %q, want google", got)
	}
	if got := New(Config{ID: "custom-google"}).ID(); got != "custom-google" {
		t.Fatalf("custom ID = %q, want custom-google", got)
	}
}

// TestAvailable verifies the cheap local availability check: key + parseable
// base URL, no network.
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

// TestStreamTextAndThinking verifies the core Gemini wire: text parts become
// StreamTextDelta, thought:true parts become StreamReasoningDelta, and the
// finishReason maps to stop. It also asserts the request carried the
// x-goog-api-key header and the streamGenerateContent endpoint.
func TestStreamTextAndThinking(t *testing.T) {
	var gotBody string
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/request-selected-model:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("unexpected URL %s", r.URL.String())
		}
		gotKey = r.Header.Get("x-goog-api-key")
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseData(`{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking A"}]}}]}`) + "\n\n"))
		w.Write([]byte(sseData(`{"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}`) + "\n\n"))
		w.Write([]byte(sseData(`{"candidates":[{"content":{"parts":[{"text":"world"}]},"finishReason":"STOP"}]}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "gemini-2.5-flash"})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model:       "request-selected-model",
		MaxTokens:   123,
		Temperature: func() *float64 { v := 0.2; return &v }(),
		Stop:        []string{"END"},
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("deepseek style system")}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}},
		},
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
	if reasoning != "thinking A" {
		t.Errorf("reasoning = %q, want %q", reasoning, "thinking A")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if gotKey != "k" {
		t.Errorf("x-goog-api-key = %q, want k", gotKey)
	}
	if !strings.Contains(gotBody, `"systemInstruction"`) || !strings.Contains(gotBody, `"deepseek style system"`) {
		t.Errorf("request body missing systemInstruction: %s", gotBody)
	}
	for _, field := range []string{`"maxOutputTokens":123`, `"temperature":0.2`, `"stopSequences":["END"]`} {
		if !strings.Contains(gotBody, field) {
			t.Errorf("request body missing %s: %s", field, gotBody)
		}
	}
}

// TestStreamFunctionCall verifies Gemini functionCall parts surface as llm
// tool calls (with parsed arguments) and the finish falls back to tool_calls.
func TestStreamFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseData(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Hangzhou"}}}]},"finishReason":"STOP"}]}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{Messages: []llm.Message{}})
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
	if calls[0].Name != "get_weather" {
		t.Errorf("call name = %q", calls[0].Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("args not json: %v", err)
	}
	if args["city"] != "Hangzhou" {
		t.Errorf("args = %v, want city=Hangzhou", args)
	}
}

func TestStreamTruncatedBeforeFinishReasonFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sseData(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatalf("first delta: %v", err)
	}
	_, err = reader.Next()
	if err == nil {
		t.Fatal("truncated stream unexpectedly completed")
	}
	failure, ok := llm.FailureFacts(err)
	if !ok || failure.Code != "STREAM_CLOSED" {
		t.Fatalf("truncated stream error = %v facts=%+v ok=%v", err, failure, ok)
	}
}

// TestStreamProviderError verifies a non-2xx error is surfaced (message parsed
// from the {"error":{"message":...}} envelope).
func TestStreamProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"API key not valid"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if _, err := p.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "API key not valid") {
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
		_, _ = w.Write([]byte(sseData(`{"candidates":[{"finishReason":"STOP"}]}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k", ModelCatalog: []llm.ModelInfo{{ID: "model", DefaultMaxTokens: 777}}})
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
	config, ok := got["generationConfig"].(map[string]any)
	if !ok || config["maxOutputTokens"] != float64(777) {
		t.Fatalf("generationConfig = %#v, want maxOutputTokens 777", got["generationConfig"])
	}
}

func TestStreamUsesReferenceRouteDefaultMaxOutputTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sseData(`{"candidates":[{"finishReason":"STOP"}]}`) + "\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
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
	config, ok := got["generationConfig"].(map[string]any)
	if !ok || config["maxOutputTokens"] != float64(32768) {
		t.Fatalf("generationConfig = %#v, want maxOutputTokens 32768", got["generationConfig"])
	}
}
