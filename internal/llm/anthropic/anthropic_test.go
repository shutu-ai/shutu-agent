package anthropic

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

// newTestProvider starts a fake Anthropic Messages endpoint and returns a
// provider pointed at it.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *anthropicProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key"})
}

// sseEventLine renders one SSE event ("event: <name>\ndata: <data>").
func sseEventLine(name, data string) string {
	return "event: " + name + "\ndata: " + data
}

// sseEvents renders a complete SSE stream from per-event strings.
func sseEvents(events ...string) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString(e)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// drain consumes the reader until io.EOF, failing the test on any error.
func drain(t *testing.T, reader llm.StreamReader) {
	t.Helper()
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}
}

// TestID returns the stable provider id (dispatch-m8-2b §2.3).
func TestID(t *testing.T) {
	if got := New(Config{}).ID(); got != "anthropic" {
		t.Fatalf("ID = %q, want anthropic", got)
	}
}

// TestAvailable verifies the cheap local availability check (dispatch-m8-2b
// §2.3 / §4.5): key present + base URL parseable, no network. A missing key or
// an unparseable base URL makes the provider unavailable.
func TestAvailable(t *testing.T) {
	if !New(Config{APIKey: "k"}).Available() {
		t.Error("key + default base URL must be available")
	}
	if !New(Config{APIKey: "k", BaseURL: "https://api.anthropic.com/v1"}).Available() {
		t.Error("key + explicit base URL must be available")
	}
	if New(Config{}).Available() {
		t.Error("empty key must be unavailable")
	}
	if New(Config{BaseURL: "https://api.anthropic.com/v1"}).Available() {
		t.Error("empty key with a base URL must be unavailable")
	}
	for _, bad := range []string{":", "://", "not a url", "http://"} {
		if New(Config{APIKey: "k", BaseURL: bad}).Available() {
			t.Errorf("base URL %q must be unavailable", bad)
		}
	}
}

// TestStreamRequestWire verifies the full request serialization against the
// contract (dispatch-m8-2b §2.1 / §4.1): POST /v1/messages, the Anthropic
// headers, and the body — model/max_tokens/stream, system extracted from
// RoleSystem, and the messages array (user → text block, assistant →
// thinking+text+tool_use in order with parsed input, tool result → tool_result
// merged into a user message), plus the tools wire (name/description/
// input_schema).
func TestStreamRequestWire(t *testing.T) {
	var gotPath string
	var gotAPIKey, gotVersion, gotContentType, gotAccept string
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("content-type")
		gotAccept = r.Header.Get("accept")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`),
			sseEventLine("message_stop", `{"type":"message_stop"}`),
		)))
	})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Model:       "request-selected-model",
		MaxTokens:   123,
		Temperature: func() *float64 { v := 0.2; return &v }(),
		Stop:        []string{"END"},
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("You are helpful.")}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("what time")}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, Text: "think step by step"},
				llm.Text("let me check"),
			}, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_time", Arguments: `{"tz":"UTC"}`}}},
			{Role: llm.RoleTool, ToolCallID: "c1", Content: []llm.ContentBlock{llm.Text("12:00")}},
		},
		Tools: []llm.ToolSchema{{Name: "get_time", Description: "get the time", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drain(t, reader)

	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	if gotAccept != "application/json" {
		t.Errorf("accept = %q", gotAccept)
	}
	if gotBody["model"] != "request-selected-model" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(123) {
		t.Errorf("max_tokens = %v", gotBody["max_tokens"])
	}
	if gotBody["temperature"] != float64(0.2) {
		t.Errorf("temperature = %v", gotBody["temperature"])
	}
	stops, _ := gotBody["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v", gotBody["stop_sequences"])
	}
	if gotBody["stream"] != true {
		t.Errorf("stream = %v", gotBody["stream"])
	}
	if gotBody["system"] != "You are helpful." {
		t.Errorf("system = %v, want the RoleSystem text", gotBody["system"])
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want 3 (user / assistant / tool-result user)", gotBody["messages"])
	}

	// msg[0] user → single text block.
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("msg0 role = %v", m0["role"])
	}
	c0 := m0["content"].([]any)
	if len(c0) != 1 {
		t.Fatalf("msg0 content = %v, want 1 text block", m0["content"])
	}
	b0 := c0[0].(map[string]any)
	if b0["type"] != "text" || b0["text"] != "what time" {
		t.Errorf("msg0 block = %v, want text block", b0)
	}

	// msg[1] assistant → thinking, text, tool_use in order (dsh 范式).
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "assistant" {
		t.Errorf("msg1 role = %v", m1["role"])
	}
	c1 := m1["content"].([]any)
	if len(c1) != 3 {
		t.Fatalf("msg1 content = %v, want thinking/text/tool_use (3 blocks)", m1["content"])
	}
	th := c1[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "think step by step" {
		t.Errorf("msg1 block0 = %v, want thinking block", th)
	}
	tx := c1[1].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "let me check" {
		t.Errorf("msg1 block1 = %v, want text block", tx)
	}
	tu := c1[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "c1" || tu["name"] != "get_time" {
		t.Errorf("msg1 block2 = %v, want tool_use c1/get_time", tu)
	}
	if input := tu["input"].(map[string]any); input["tz"] != "UTC" {
		t.Errorf("tool_use input = %v, want the parsed arguments {tz:UTC}", tu["input"])
	}

	// msg[2] user → tool_result block merged (dispatch-m8-2b §2.1 rule 4).
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "user" {
		t.Errorf("msg2 role = %v", m2["role"])
	}
	c2 := m2["content"].([]any)
	if len(c2) != 1 {
		t.Fatalf("msg2 content = %v, want 1 tool_result block", m2["content"])
	}
	tr := c2[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" || tr["content"] != "12:00" {
		t.Errorf("msg2 block = %v, want tool_result c1 12:00", tr)
	}

	// tools wire: name/description/input_schema.
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", gotBody["tools"])
	}
	wt := tools[0].(map[string]any)
	if wt["name"] != "get_time" || wt["description"] != "get the time" {
		t.Errorf("tool = %v", wt)
	}
	if _, ok := wt["input_schema"]; !ok {
		t.Errorf("tool missing input_schema: %v", wt)
	}
}

// TestStreamAssistantReasoningRoundTrip is the 回传往返 case (dispatch-m8-2b
// §4.6): an assistant message carrying a reasoning block serializes as a
// thinking block before the text block, and the tool_use block keeps its place
// after them — the order the wire must preserve when replaying reasoning
// across providers.
func TestStreamAssistantReasoningRoundTrip(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})

	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, Text: "reason one"},
				{Kind: llm.BlockReasoning, Text: " then two"},
				llm.Text("answer"),
			}, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_time", Arguments: `{"tz":"UTC"}`}}},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want 1", gotBody["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("content = %v, want thinking/thinking/text/tool_use (4 blocks)", msgs[0])
	}
	wantTypes := []string{"thinking", "thinking", "text", "tool_use"}
	for i, want := range wantTypes {
		if got := content[i].(map[string]any)["type"]; got != want {
			t.Errorf("block %d type = %v, want %s", i, got, want)
		}
	}
	if th0 := content[0].(map[string]any)["thinking"]; th0 != "reason one" {
		t.Errorf("thinking block 0 = %v, want 'reason one'", th0)
	}
}

// TestStreamConsecutiveToolResultsMerge verifies consecutive tool results
// merge into a single user message of tool_result blocks (dispatch-m8-2b §2.1
// rule 4: 同一轮多个 tool result 追加到同一条 user 消息).
func TestStreamConsecutiveToolResultsMerge(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})

	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("run both")}},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "get_time", Arguments: `{"tz":"UTC"}`},
				{ID: "c2", Name: "get_time", Arguments: `{"tz":"Asia/Shanghai"}`},
			}},
			{Role: llm.RoleTool, ToolCallID: "c1", Content: []llm.ContentBlock{llm.Text("12:00")}},
			{Role: llm.RoleTool, ToolCallID: "c2", Content: []llm.ContentBlock{llm.Text("20:00")}},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want 3 (user / assistant / merged tool-result user)", gotBody["messages"])
	}
	content, _ := msgs[2].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("merged tool-result content = %v, want 2 tool_result blocks", msgs[2])
	}
	for i, id := range []string{"c1", "c2"} {
		tr := content[i].(map[string]any)
		if tr["type"] != "tool_result" || tr["tool_use_id"] != id {
			t.Errorf("merged block %d = %v, want tool_result %s", i, tr, id)
		}
	}
}

// TestStreamEmptyUserContentPlaceholder verifies the empty-content rule
// (dispatch-m8-2b §2.1 rule 5): a user message with no content blocks is sent
// as the "(no output)" placeholder.
func TestStreamEmptyUserContentPlaceholder(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})

	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want 1", gotBody["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want the placeholder block", msgs[0])
	}
	b := content[0].(map[string]any)
	if b["type"] != "text" || b["text"] != "(no output)" {
		t.Errorf("placeholder block = %v", b)
	}
}

// TestStreamToolUseArgumentsFallback verifies a tool call whose arguments are
// not valid JSON still serialize their input as {"_raw": <raw>} instead of
// being dropped (dispatch-m8-2b §2.1 rule 3).
func TestStreamToolUseArgumentsFallback(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})

	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "borked", Arguments: "not json"}}},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	tu := content[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "c1" {
		t.Fatalf("tool_use block = %v", tu)
	}
	input, _ := tu["input"].(map[string]any)
	if input["_raw"] != "not json" {
		t.Errorf("tool_use input = %v, want the _raw fallback", tu["input"])
	}
}

// TestStreamHTTPError verifies a non-2xx response with an error JSON body
// surfaces the server message (dispatch-m8-2b §2.3 / §4.4).
func TestStreamHTTPError(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad request body"}}`))
	})
	_, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "bad request body") {
		t.Errorf("err = %q, want the server message", err)
	}
	if !strings.Contains(err.Error(), "anthropic: provider error") {
		t.Errorf("err = %q, want the provider-error wrapper", err)
	}
}

// TestStreamHTTPErrorFlatMessage verifies the flat {"message":...} error shape
// (dispatch-m8-2b §2.3).
func TestStreamHTTPErrorFlatMessage(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"rate limited"}`))
	})
	_, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want the flat server message", err)
	}
}

// TestStreamRedirectBlocked verifies the no-follow redirect policy
// (dispatch-m8-2b §2.3): any 3xx is blocked and reported, never treated as
// success.
func TestStreamRedirectBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/messages", http.StatusMovedPermanently)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	_, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "redirect blocked") {
		t.Fatalf("err = %v, want the redirect-blocked error", err)
	}
}

func TestStreamUsesExplicitModelDefaultMaxTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvents(sseEventLine("message_start", `{"type":"message_start","message":{"id":"m"}}`), sseEventLine("message_stop", `{"type":"message_stop"}`))))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "test-key", ModelCatalog: []llm.ModelInfo{{ID: "model", DefaultMaxTokens: 777}}})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{Model: "model", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, reader)
	if got["max_tokens"] != float64(777) {
		t.Fatalf("max_tokens = %#v, want 777", got["max_tokens"])
	}
}

func TestStreamUsesReferenceRouteDefaultMaxTokens(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvents(sseEventLine("message_start", `{"type":"message_start","message":{"id":"m"}}`), sseEventLine("message_stop", `{"type":"message_stop"}`))))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "test-key"})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{Model: "unlisted", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, reader)
	if got["max_tokens"] != float64(32768) {
		t.Fatalf("max_tokens = %#v, want reference route default 32768", got["max_tokens"])
	}
}
