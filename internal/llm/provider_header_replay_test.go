package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/llm/anthropic"
	"github.com/shutu-ai/shutu-agent/internal/llm/deepseek"
	"github.com/shutu-ai/shutu-agent/internal/llm/google"
	"github.com/shutu-ai/shutu-agent/internal/llm/openai"
	"github.com/shutu-ai/shutu-agent/internal/llm/openairesponses"
)

// TestEveryProviderHeaderReplay is the shared external-HTTP boundary replay
// for all claimed production provider protocols. It fixes the real method,
// endpoint, auth scheme, stable application attribution, DeepSeek-only
// identity headers, stream content type, and terminal provider stream event.
func TestEveryProviderHeaderReplay(t *testing.T) {
	const (
		apiKey      = "provider-header-key"
		model       = "provider-header-model"
		sessionID   = "provider-header-session"
		anonymousID = "provider-header-user"
	)
	request := llm.ChatRequest{
		Model: model, MaxTokens: 123, SessionID: sessionID, Purpose: "compaction",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("header replay")}},
		},
	}

	cases := []struct {
		name           string
		path           string
		newProvider    func(baseURL string) llm.LLM
		authHeader     string
		authValue      string
		identityHeader bool
		compactHeader  bool
	}{
		{
			name: "deepseek official", path: "/chat/completions",
			newProvider: func(baseURL string) llm.LLM {
				return deepseek.New(deepseek.Config{
					ProviderName: "deepseek-official", BaseURL: baseURL, APIKey: apiKey,
					UserID: anonymousID,
				})
			},
			authHeader: "Authorization", authValue: "Bearer " + apiKey,
			identityHeader: true, compactHeader: true,
		},
		{
			name: "openai compatible", path: "/chat/completions",
			newProvider: func(baseURL string) llm.LLM {
				return openai.New(openai.Config{BaseURL: baseURL, APIKey: apiKey})
			},
			authHeader: "Authorization", authValue: "Bearer " + apiKey,
		},
		{
			name: "anthropic", path: "/messages",
			newProvider: func(baseURL string) llm.LLM {
				return anthropic.New(anthropic.Config{BaseURL: baseURL, APIKey: apiKey})
			},
			authHeader: "X-Api-Key", authValue: apiKey,
		},
		{
			name: "gemini", path: "/models/" + model + ":streamGenerateContent",
			newProvider: func(baseURL string) llm.LLM {
				return google.New(google.Config{BaseURL: baseURL, APIKey: apiKey})
			},
			authHeader: "X-Goog-Api-Key", authValue: apiKey,
		},
		{
			name: "openai responses", path: "/responses",
			newProvider: func(baseURL string) llm.LLM {
				return openairesponses.New(openairesponses.Config{BaseURL: baseURL, APIKey: apiKey})
			},
			authHeader: "Authorization", authValue: "Bearer " + apiKey,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sseEvent := func(name, data string) string {
				return "event: " + name + "\ndata: " + data + "\n\n"
			}
			requests := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clone := r.Clone(r.Context())
				select {
				case requests <- clone:
				default:
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch {
				case r.URL.Path == "/chat/completions":
					_, _ = w.Write([]byte(strings.Join([]string{
						`data: {"choices":[{"delta":{"content":"header replay"},"finish_reason":null}]}`,
						`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
						`data: [DONE]`,
						"",
					}, "\n\n")))
				case r.URL.Path == "/messages":
					_, _ = w.Write([]byte(
						sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_header"}}`) +
							sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
							sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"header replay"}}`) +
							sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
							sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`) +
							sseEvent("message_stop", `{"type":"message_stop"}`),
					))
				case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
					_, _ = w.Write([]byte("data: " + `{"candidates":[{"content":{"parts":[{"text":"header replay"}]},"finishReason":"STOP"}]}` + "\n\n"))
				case r.URL.Path == "/responses":
					_, _ = w.Write([]byte(
						sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"header replay"}`) +
							sseEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_header","status":"completed"}}`),
					))
				default:
					t.Errorf("unexpected provider endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			reader, err := tc.newProvider(server.URL).Stream(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			var finish llm.StreamEvent
			for {
				event, nextErr := reader.Next()
				if errors.Is(nextErr, io.EOF) {
					break
				}
				if nextErr != nil {
					t.Fatal(nextErr)
				}
				if event.Kind == llm.StreamTextDelta {
					text.WriteString(event.Text)
				}
				if event.Kind == llm.StreamFinish {
					finish = event
				}
			}
			if text.String() != "header replay" || finish.FinishReason != "stop" {
				t.Fatalf("provider replay output=%q finish=%#v", text.String(), finish)
			}

			select {
			case got := <-requests:
				if got.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", got.Method)
				}
				wantURL, err := url.Parse(tc.path)
				if err != nil {
					t.Fatal(err)
				}
				if got.URL.Path != wantURL.Path {
					t.Fatalf("endpoint path = %s, want %s", got.URL.Path, wantURL.Path)
				}
				if tc.name == "gemini" && got.URL.Query().Get("alt") != "sse" {
					t.Fatalf("gemini alt = %q, want sse", got.URL.Query().Get("alt"))
				}
				if got.Header.Get(tc.authHeader) != tc.authValue {
					t.Fatalf("%s = %q, want %q", tc.authHeader, got.Header.Get(tc.authHeader), tc.authValue)
				}
				if got.Header.Get("User-Agent") != llm.AttributionUserAgent {
					t.Fatalf("User-Agent = %q, want %q", got.Header.Get("User-Agent"), llm.AttributionUserAgent)
				}
				identityNames := []string{"X-Shutu-User-Id", "X-Shutu-Session-Id"}
				if tc.identityHeader {
					for _, name := range identityNames {
						if got.Header.Get(name) == "" {
							t.Fatalf("%s is missing on official DeepSeek route", name)
						}
					}
					if got.Header.Get("X-Shutu-User-Id") != anonymousID {
						t.Fatalf("anonymous user id = %q", got.Header.Get("X-Shutu-User-Id"))
					}
					if got.Header.Get("X-Shutu-Session-Id") != sessionID {
						t.Fatalf("session id = %q", got.Header.Get("X-Shutu-Session-Id"))
					}
				} else {
					for _, name := range append(identityNames, "X-Shutu-Compact") {
						if value := got.Header.Get(name); value != "" {
							t.Fatalf("%s = %q, want omitted outside official DeepSeek", name, value)
						}
					}
				}
				if tc.compactHeader {
					if got.Header.Get("X-Shutu-Compact") != "1" {
						t.Fatalf("compaction header = %q, want 1", got.Header.Get("X-Shutu-Compact"))
					}
				}
			case <-context.Background().Done():
				t.Fatal("provider request was not observed")
			}
		})
	}
}
