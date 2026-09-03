package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/llm/anthropic"
	"github.com/shutu-ai/shutu-agent/internal/llm/deepseek"
	"github.com/shutu-ai/shutu-agent/internal/llm/google"
	"github.com/shutu-ai/shutu-agent/internal/llm/openai"
	"github.com/shutu-ai/shutu-agent/internal/llm/openairesponses"
)

// TestEveryProviderRejectsUnsupportedRequestInputWithoutNetwork pins the stable A7.3
// modality boundary at the provider seam. Audio and merge-extensible blocks may
// be preserved durably, but no claimed provider may serialize them, read
// attachments, consume credentials, or cross the network with them.
func TestEveryProviderRejectsUnsupportedRequestInputWithoutNetwork(t *testing.T) {
	message := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind: "audio", Raw: json.RawMessage(`{"type":"audio","data":"aGk=","mimeType":"audio/wav"}`),
		}},
	}
	request := llm.ChatRequest{Model: "unused", Messages: []llm.Message{message}}
	var providerRequests atomic.Int32
	var lastAuthorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		lastAuthorization.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()
	providers := []struct {
		name string
		run  func() error
	}{
		{"deepseek", func() error {
			_, err := deepseek.New(deepseek.Config{ProviderName: "deepseek-official", BaseURL: server.URL, APIKey: "sentinel-unused"}).Stream(context.Background(), request)
			return err
		}},
		{"openai", func() error {
			_, err := openai.New(openai.Config{BaseURL: server.URL, APIKey: "sentinel-unused"}).Stream(context.Background(), request)
			return err
		}},
		{"anthropic", func() error {
			_, err := anthropic.New(anthropic.Config{BaseURL: server.URL, APIKey: "sentinel-unused"}).Stream(context.Background(), request)
			return err
		}},
		{"google", func() error {
			_, err := google.New(google.Config{BaseURL: server.URL, APIKey: "sentinel-unused"}).Stream(context.Background(), request)
			return err
		}},
		{"openai-responses", func() error {
			_, err := openairesponses.New(openairesponses.Config{BaseURL: server.URL, APIKey: "sentinel-unused"}).Stream(context.Background(), request)
			return err
		}},
	}
	for _, provider := range providers {
		provider := provider
		t.Run(provider.name, func(t *testing.T) {
			err := provider.run()
			facts, ok := llm.FailureFacts(err)
			if !ok || facts.Code != "UNSUPPORTED_INPUT_CONTENT" {
				t.Fatalf("%s audio request error = %v, want UNSUPPORTED_INPUT_CONTENT", provider.name, err)
			}
			if got := providerRequests.Load(); got != 0 {
				t.Fatalf("%s unsupported input made %d provider requests, want zero", provider.name, got)
			}
			if auth, ok := lastAuthorization.Load().(string); ok && strings.Contains(auth, "sentinel-unused") {
				t.Fatalf("%s unsupported input leaked the credential in a request", provider.name)
			}
		})
	}
	if got := providerRequests.Load(); got != 0 {
		t.Fatalf("unsupported input crossed the provider network boundary %d times, want zero", got)
	}
}
