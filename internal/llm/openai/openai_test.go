package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

func sse(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

// TestNewID verifies the stable provider id (dispatch-m8-2 §4). The default
// base_url/model (https://api.openai.com/v1 / gpt-4o-mini) are applied inside
// New and asserted at the config layer (dispatch-m8-2 §7); the wire behavior
// with an explicit config is covered by TestStreamTextAndToolCallsRegression.
func TestNewID(t *testing.T) {
	if got := New(Config{APIKey: "k"}).ID(); got != "openai" {
		t.Fatalf("ID = %q, want openai", got)
	}
}

// TestStreamTextAndToolCallsRegression verifies the delegation regression: the
// openai provider streams text and tool calls through the shared
// OpenAI-compatible SSE client exactly like deepseek (dispatch-m8-2 §4/§7 —
// httptest fake OpenAI-compatible SSE service).
func TestStreamTextAndToolCallsRegression(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if body, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(body, &gotBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":\"UTC\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"It is "},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"noon"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		)))
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "openai-key", Model: "gpt-4o-mini"})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("time")}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var text strings.Builder
	var finish llm.StreamEvent
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind == llm.StreamTextDelta {
			text.WriteString(ev.Text)
		}
		if ev.Kind == llm.StreamFinish {
			finish = ev
		}
	}

	if text.String() != "It is noon" {
		t.Fatalf("text = %q, want %q", text.String(), "It is noon")
	}
	if len(finish.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", finish.ToolCalls)
	}
	if finish.ToolCalls[0].Name != "get_time" || finish.ToolCalls[0].Arguments != `{"tz":"UTC"}` {
		t.Fatalf("tool call = %+v", finish.ToolCalls[0])
	}
	if finish.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", finish.FinishReason)
	}
	// The wire hit the OpenAI-compatible /chat/completions endpoint with the
	// configured key and model.
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer openai-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v", gotBody["model"])
	}
}

// TestAvailable verifies the cheap local availability check (dispatch-m8-2 §4):
// API key present + base URL parseable, no network. A missing key or an
// unparseable base URL makes the provider unavailable.
func TestAvailable(t *testing.T) {
	if !New(Config{APIKey: "k"}).Available() {
		t.Error("key + default base URL must be available")
	}
	if !New(Config{APIKey: "k", BaseURL: "https://api.openai.com/v1"}).Available() {
		t.Error("key + explicit base URL must be available")
	}
	if New(Config{}).Available() {
		t.Error("empty key must be unavailable")
	}
	if New(Config{BaseURL: "https://api.openai.com/v1"}).Available() {
		t.Error("empty key with a base URL must be unavailable")
	}
	for _, bad := range []string{":", "://", "not a url", "http://"} {
		if New(Config{APIKey: "k", BaseURL: bad}).Available() {
			t.Errorf("base URL %q must be unavailable", bad)
		}
	}
}

func TestHTTPFailureUsesOpenAIProviderLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "k", DisableRetry: true})
	_, err := p.Stream(context.Background(), llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}}})
	if err == nil {
		t.Fatal("expected HTTP failure")
	}
	facts, ok := llm.FailureFacts(err)
	if !ok || facts.Code != "AUTH" {
		t.Fatalf("failure facts = %+v, ok=%v", facts, ok)
	}
	if !strings.HasPrefix(facts.Message, "openai:") {
		t.Fatalf("failure message = %q, want openai label", facts.Message)
	}
}

// TestStreamImageRequestThroughOpenAI verifies the M8-3b image path goes
// through the openai provider end to end (dispatch-m8-3b §7: openai 委托
// deepseek 后由 deepseek 测试覆盖，openai 补一个带图走通): with SupportsImages=true an
// image request serializes as a parts array with the image_url data URL.
func TestStreamImageRequestThroughOpenAI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(path, []byte("pngbytes"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "k", SupportsImages: true})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Bytes: 8, Path: path}},
		}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want 1 image_url part", msgs[0])
	}
	img, _ := content[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("part type = %v", img["type"])
	}
	iu, _ := img["image_url"].(map[string]any)
	if url, _ := iu["url"].(string); !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("data URL = %q, want the data:image/png;base64, prefix", url)
	}
}

func TestStreamUsesExplicitModelDefaultMaxTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
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
	if got["max_tokens"] != float64(777) {
		t.Fatalf("max_tokens = %#v, want 777", got["max_tokens"])
	}
}
