package deepseek

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
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// newTestClient starts a fake DeepSeek endpoint and returns a Client pointed
// at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key"})
}

func sse(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

func TestStreamText(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})

	reader, err := c.Stream(context.Background(), llm.ChatRequest{
		Model: "request-selected-model", MaxTokens: 123, Stop: []string{"END"},
		Temperature: func() *float64 { v := 0.2; return &v }(),
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}},
		},
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
	if text.String() != "Hello" {
		t.Fatalf("text = %q, want Hello", text.String())
	}
	if finish.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", finish.FinishReason)
	}
	if len(finish.ToolCalls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", finish.ToolCalls)
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream flag = %v, want true", gotBody["stream"])
	}
	streamOptions, ok := gotBody["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage=true", gotBody["stream_options"])
	}
	if gotBody["model"] != "request-selected-model" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(123) || gotBody["temperature"] != 0.2 {
		t.Fatalf("generation controls = %#v, want max_tokens=123 temperature=0.2", gotBody)
	}
	if stops, ok := gotBody["stop"].([]any); !ok || len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop = %#v, want [END]", gotBody["stop"])
	}
}

func TestOfficialRequestCarriesAttributionIdentity(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, APIKey: "test-key", UserID: "anonymous-test"})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{SessionID: "session-1", Purpose: "compaction"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("next: %v", nextErr)
		}
	}
	if got.Get("User-Agent") != llm.AttributionUserAgent {
		t.Fatalf("user-agent = %q, want %q", got.Get("User-Agent"), llm.AttributionUserAgent)
	}
	if got.Get("X-Shutu-User-Id") != "anonymous-test" {
		t.Fatalf("user id = %q", got.Get("X-Shutu-User-Id"))
	}
	if got.Get("X-Shutu-Session-Id") != "session-1" {
		t.Fatalf("session id = %q", got.Get("X-Shutu-Session-Id"))
	}
	if got.Get("X-Shutu-Compact") != "1" {
		t.Fatalf("compact = %q", got.Get("X-Shutu-Compact"))
	}
}

func TestCompatibleRouteOmitsDeepSeekIdentityHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{ProviderName: "openai", BaseURL: srv.URL, APIKey: "test-key", UserID: "anonymous-test"})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{SessionID: "session-1", Purpose: "compaction"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("next: %v", nextErr)
		}
	}
	if got.Get("User-Agent") != llm.AttributionUserAgent {
		t.Fatalf("user-agent = %q, want %q", got.Get("User-Agent"), llm.AttributionUserAgent)
	}
	for _, name := range []string{"X-Shutu-User-Id", "X-Shutu-Session-Id", "X-Shutu-Compact"} {
		if value := got.Get(name); value != "" {
			t.Fatalf("%s = %q, want omitted for compatible route", name, value)
		}
	}
}

func TestStreamPreservesEveryDeltaInOneChunk(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"choices":[{"delta":{"reasoning_content":"think","content":"answer"},"finish_reason":null},{"delta":{"content":" tail"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []llm.StreamEvent
	for {
		event, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		got = append(got, event)
	}
	if len(got) != 4 || got[0].Kind != llm.StreamReasoningDelta || got[0].Text != "think" ||
		got[1].Kind != llm.StreamTextDelta || got[1].Text != "answer" ||
		got[2].Kind != llm.StreamTextDelta || got[2].Text != " tail" ||
		got[3].Kind != llm.StreamFinish {
		t.Fatalf("events = %+v, want reasoning + both text deltas + finish", got)
	}
}

func TestStreamUsageAcceptsCacheHitCompatibilityField(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_cache_hit_tokens":3}}`,
			"[DONE]",
		)))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var finish llm.StreamEvent
	for {
		event, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if event.Kind == llm.StreamFinish {
			finish = event
		}
	}
	if finish.Usage.InputTokens != 7 || finish.Usage.CacheReadTokens != 3 || finish.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %+v, want disjoint input=7 cacheRead=3 total=12", finish.Usage)
	}
}

// TestStreamReasoningEffort verifies the dsh 思考强度 wire: a "high" effort
// travels as reasoning_effort, "off" and "" omit the field entirely.
func TestStreamReasoningEffort(t *testing.T) {
	probe := func(effort string) map[string]any {
		var gotBody map[string]any
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
		})
		reader, err := c.Stream(context.Background(), llm.ChatRequest{ReasoningEffort: effort})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for {
			if _, err := reader.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("next: %v", err)
			}
		}
		return gotBody
	}

	if body := probe("high"); body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	for _, eff := range []string{"", "off"} {
		body := probe(eff)
		if _, present := body["reasoning_effort"]; present {
			t.Fatalf("%q effort must omit reasoning_effort, got %v", eff, body["reasoning_effort"])
		}
	}
}

func TestStreamReasoningEffortValidatesAndPublishesThinkingMode(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := reader.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if gotBody["thinking"] == nil || gotBody["reasoning_effort"] != "low" {
		t.Fatalf("thinking wire = %#v, want enabled + low", gotBody)
	}
	thinking := gotBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v, want enabled", thinking)
	}

	if _, err := c.Stream(context.Background(), llm.ChatRequest{ReasoningEffort: "bogus"}); err == nil {
		t.Fatal("unsupported reasoning effort was accepted")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("failure = %+v (typed=%v), want UNSUPPORTED_REASONING_EFFORT", failure, ok)
	}
}

func TestStreamDeploymentDisabledThinkingRejectsEnablement(t *testing.T) {
	c := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "test-key", Thinking: "disabled"})
	if _, err := c.Stream(context.Background(), llm.ChatRequest{ReasoningEffort: "high"}); err == nil {
		t.Fatal("deployment-disabled thinking accepted high effort")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("failure = %+v (typed=%v), want UNSUPPORTED_REASONING_EFFORT", failure, ok)
	}
}

func TestStreamToolCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":\"UTC\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		)))
	})

	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var finish llm.StreamEvent
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind == llm.StreamFinish {
			finish = ev
		}
	}
	if len(finish.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", finish.ToolCalls)
	}
	call := finish.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "get_time" {
		t.Fatalf("call = %+v", call)
	}
	if call.Arguments != `{"tz":"UTC"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
	if finish.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", finish.FinishReason)
	}
}

func TestStreamSendsToolsAndToolMessage(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})

	_, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("sys")}},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_time", Arguments: "{}"}}},
			{Role: llm.RoleTool, ToolCallID: "c1", Content: []llm.ContentBlock{llm.Text("out")}},
		},
		Tools: []llm.ToolSchema{{Name: "get_time", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", gotBody["tools"])
	}
}

func TestStreamToolResultImagesUseTextToolMessageAndUserImageMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.png")
	if err := os.WriteFile(path, []byte("image-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "test-key", SupportsImages: true})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{Messages: []llm.Message{{
		Role: llm.RoleTool, ToolCallID: "call-1",
		Content: []llm.ContentBlock{{Kind: llm.BlockImage, Image: llm.ImageRef{Path: path, MediaType: "image/png", Bytes: 11}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := reader.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v, want tool + following user image message", gotBody["messages"])
	}
	tool := messages[0].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call-1" || tool["content"] != "(see attached image)" {
		t.Fatalf("tool message = %#v", tool)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("image message = %#v", user)
	}
	parts := user["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != toolResultImageMarker {
		t.Fatalf("image parts = %#v", parts)
	}
}

func TestStreamHTTPError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	})
	if _, err := c.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error on 401")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "AUTH" {
		t.Fatalf("401 failure = %+v (typed=%v), want AUTH", failure, ok)
	}
}

func TestStreamEmptyResponseIsTypedFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	event, err := reader.Next()
	if err != nil {
		t.Fatalf("empty response next: %v", err)
	}
	if event.Kind != llm.StreamFinish || event.Failure == nil || event.Failure.Code != "EMPTY_RESPONSE" {
		t.Fatalf("empty response event = %+v, want EMPTY_RESPONSE finish", event)
	}
}

func TestStreamTruncatedMissingDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n"))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, err = reader.Next() // content delta
	if err != nil {
		t.Fatalf("first next: %v", err)
	}
	_, err = reader.Next() // EOF without [DONE]
	if err == nil {
		t.Fatal("expected truncated-stream error")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "STREAM_CLOSED" {
		t.Fatalf("truncated stream failure = %+v (typed=%v), want STREAM_CLOSED", failure, ok)
	}
}

func TestStreamRejectsUnterminatedSSETail(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}"))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err == nil {
		t.Fatal("unterminated SSE tail was accepted")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "STREAM_CLOSED" {
		t.Fatalf("failure = %+v (typed=%v), want STREAM_CLOSED", failure, ok)
	}
}

func TestStreamAcceptsUTF8BOMBeforeFirstSSEField(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("\xef\xbb\xbf" + sse(
			`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := reader.Next()
	if err != nil || event.Kind != llm.StreamTextDelta || event.Text != "ok" {
		t.Fatalf("first event = %+v, err=%v", event, err)
	}
}

func TestStreamIdleWatchdogReturnsTimeout(t *testing.T) {
	c := New(Config{APIKey: "test-key", StreamIdleTimeout: 20 * time.Millisecond})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	c.baseURL = srv.URL
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err == nil {
		t.Fatal("idle stream unexpectedly returned")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "TIMEOUT" {
		t.Fatalf("failure = %+v (typed=%v), want TIMEOUT", failure, ok)
	}
}

func TestStreamCancellationReturnsAborted(t *testing.T) {
	c := New(Config{APIKey: "test-key", StreamIdleTimeout: time.Hour})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	c.baseURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := c.Stream(ctx, llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.Next(); err == nil {
		t.Fatal("cancelled stream unexpectedly returned")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "ABORTED" {
		t.Fatalf("failure = %+v (typed=%v), want ABORTED", failure, ok)
	}
}

func TestStreamMalformedPayload(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {not json}` + "\n\n"))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, err := reader.Next(); err == nil {
		t.Fatal("expected malformed payload error")
	} else if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "MALFORMED_RESPONSE" {
		t.Fatalf("malformed payload failure = %+v (typed=%v), want MALFORMED_RESPONSE", failure, ok)
	}
}

// newRetryClient returns a Client with fast, zero-delay retries for tests.
func newRetryClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	c := newTestClient(t, handler)
	c.maxRetries = 2
	c.backoff = func(int) time.Duration { return 0 }
	return c
}

// TestStreamRetries429ThenSucceeds verifies the 429→200 backoff retry path
// (dispatch-m2 §5; acceptance requires an httptest 429→200 case).
func TestStreamRetries429ThenSucceeds(t *testing.T) {
	var calls int
	c := newRetryClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})

	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text strings.Builder
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
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q, want ok", text.String())
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (429 then 200)", calls)
	}
}

// TestStreamDoesNotRetry4xx verifies auth/4xx errors are returned immediately
// without retry (dispatch-m2 §5).
func TestStreamDoesNotRetry4xx(t *testing.T) {
	var calls int
	c := newRetryClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error on 401")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", calls)
	}
}

// TestStreamRetries5xxExhausted verifies 5xx is retried maxRetries times and
// then the last error is returned.
func TestStreamRetries5xxExhausted(t *testing.T) {
	var calls int
	c := newRetryClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (initial + 2 retries)", calls)
	}
}

// TestStreamRetryAbortsOnCancellation verifies a cancelled context aborts the
// backoff wait instead of sleeping out the full delay.
func TestStreamRetryAbortsOnCancellation(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		cancel() // cancel right after the first 429 so the backoff aborts
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.maxRetries = 5
	c.backoff = func(int) time.Duration { return time.Hour } // would hang without cancellation
	if _, err := c.Stream(ctx, llm.ChatRequest{}); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (retry aborted by cancellation)", calls)
	}
}

// TestID returns the stable provider id (M8-2, dispatch-m8-2 §3).
func TestID(t *testing.T) {
	if got := New(Config{}).ID(); got != "deepseek-official" {
		t.Fatalf("ID = %q, want deepseek", got)
	}
}

// TestAvailable verifies the cheap local availability check (dispatch-m8-2 §3):
// key present + base URL parseable, with no network call. A missing key or an
// unparseable base URL makes the provider unavailable.
func TestAvailable(t *testing.T) {
	// Key present, valid base URL (explicit and defaulted).
	if !New(Config{APIKey: "k"}).Available() {
		t.Error("key + default base URL must be available")
	}
	if !New(Config{APIKey: "k", BaseURL: "https://api.deepseek.com"}).Available() {
		t.Error("key + explicit base URL must be available")
	}

	// Missing key → unavailable regardless of base URL.
	if New(Config{}).Available() {
		t.Error("empty key must be unavailable")
	}
	if New(Config{BaseURL: "https://api.deepseek.com"}).Available() {
		t.Error("empty key with a base URL must be unavailable")
	}

	// Invalid / unparseable base URL → unavailable even with a key.
	for _, bad := range []string{":", "://", "not a url", "http://"} {
		if New(Config{APIKey: "k", BaseURL: bad}).Available() {
			t.Errorf("base URL %q must be unavailable", bad)
		}
	}
}

func TestCloseWipesCredentialAndMakesClientUnavailable(t *testing.T) {
	c := New(Config{APIKey: "secret"})
	if !c.Available() {
		t.Fatal("client should be available before Close")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.Available() {
		t.Fatal("client remained available after credential wipe")
	}
	if got := c.keySnapshot(); got != "" {
		t.Fatalf("credential after Close = %q", got)
	}
}

func TestCredentialProviderIsResolvedPerStream(t *testing.T) {
	var current string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+current {
			t.Errorf("authorization = %q, want Bearer %s", got, current)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	defer server.Close()
	current = "first"
	client := New(Config{
		BaseURL: server.URL,
		APIKey:  "bootstrap",
		CredentialProvider: func(context.Context) (string, error) {
			return current, nil
		},
	})
	for _, want := range []string{"first", "rotated"} {
		current = want
		reader, err := client.Stream(context.Background(), llm.ChatRequest{})
		if err != nil {
			t.Fatalf("stream with %s: %v", want, err)
		}
		for {
			_, err = reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("read with %s: %v", want, err)
			}
		}
	}
}

type trackingCredentialLease struct {
	value    string
	released chan struct{}
	once     sync.Once
}

func (l *trackingCredentialLease) Value() string { return l.value }
func (l *trackingCredentialLease) Release() {
	l.once.Do(func() { close(l.released) })
}

func TestCredentialLeaseIsReleasedWhenStreamTerminates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{"content":"ok"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	}))
	defer server.Close()
	lease := &trackingCredentialLease{value: "lease-key", released: make(chan struct{})}
	client := New(Config{
		BaseURL: server.URL,
		APIKey:  "bootstrap",
		CredentialLeaseProvider: func(context.Context) (llm.CredentialLease, error) {
			return lease, nil
		},
	})
	reader, err := client.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-lease.released:
	case <-time.After(time.Second):
		t.Fatal("credential lease was not released after stream termination")
	}
}

// TestToWireMessageReasoning verifies the M8 wire contract: an assistant
// message whose content carries a reasoning block serializes the joined
// reasoning text on the OpenAI-compatible reasoning_content field, and
// non-assistant messages never carry it (dispatch-m8 §3).
func TestToWireMessageReasoning(t *testing.T) {
	asst := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockReasoning, Text: "step by step, "},
			{Kind: llm.BlockReasoning, Text: "then answer"},
			llm.Text("Here is the answer."),
		},
	}
	w, err := toWireMessage(asst)
	if err != nil {
		t.Fatalf("toWireMessage: %v", err)
	}
	if w.ReasoningContent != "step by step, then answer" {
		t.Fatalf("reasoning_content = %q, want the joined reasoning text", w.ReasoningContent)
	}
	if w.Content != "Here is the answer." {
		t.Fatalf("content = %q, want the text blocks only (no reasoning)", w.Content)
	}

	// A user message with a reasoning block must NOT carry reasoning_content.
	usr := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Kind: llm.BlockReasoning, Text: "x"}, llm.Text("hi")},
	}
	wu, err := toWireMessage(usr)
	if err != nil {
		t.Fatalf("toWireMessage: %v", err)
	}
	if wu.ReasoningContent != "" {
		t.Fatalf("user message must not carry reasoning_content, got %q", wu.ReasoningContent)
	}
	if wu.Content != "hi" {
		t.Fatalf("user content = %q, want hi", wu.Content)
	}
}

// TestToWireMessageReasoningOnTheWire verifies reasoning_content actually lands
// in the request body sent to the endpoint (httptest asserting the JSON body,
// dispatch-m8 §6).
func TestToWireMessageReasoningOnTheWire(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	_, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, Text: "think hard"},
				llm.Text("answer"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["reasoning_content"] != "think hard" {
		t.Fatalf("wire message = %+v, want reasoning_content=think hard", first)
	}
	if first["content"] != "answer" {
		t.Fatalf("wire message content = %+v, want answer", first)
	}
}

// TestStreamReasoningDelta verifies the SSE reader surfaces reasoning_content
// deltas as StreamReasoningDelta and accumulates them onto StreamFinish.Reasoning
// (dispatch-m8 §3/§6).
func TestStreamReasoningDelta(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"reasoning_content":"Let me"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"reasoning_content":" think"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"Done."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var reasoning, text strings.Builder
	var finish llm.StreamEvent
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		switch ev.Kind {
		case llm.StreamReasoningDelta:
			reasoning.WriteString(ev.Text)
		case llm.StreamTextDelta:
			text.WriteString(ev.Text)
		case llm.StreamFinish:
			finish = ev
		}
	}
	if reasoning.String() != "Let me think" {
		t.Fatalf("reasoning deltas = %q, want %q", reasoning.String(), "Let me think")
	}
	if text.String() != "Done." {
		t.Fatalf("text deltas = %q, want Done.", text.String())
	}
	if finish.Reasoning != "Let me think" {
		t.Fatalf("finish.Reasoning = %q, want the accumulated reasoning", finish.Reasoning)
	}
	if finish.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", finish.FinishReason)
	}
}

// TestStreamNoReasoningRegression verifies a stream without any
// reasoning_content behaves exactly as before: only text deltas and a finish
// with empty Reasoning (M8 regression guard).
func TestStreamNoReasoningRegression(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(
			`{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		)))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{})
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
	if text.String() != "ok" {
		t.Fatalf("text = %q, want ok", text.String())
	}
	if finish.Reasoning != "" {
		t.Fatalf("finish.Reasoning = %q, want empty without reasoning deltas", finish.Reasoning)
	}
}
