package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestNativeWebSearchSettingsDriveProviderRequest(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "dynamic-search-key" {
			t.Errorf("provider API key = %q", r.Header.Get("x-api-key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"web_search_tool_result","content":[]}]}`))
	}))
	t.Cleanup(server.Close)

	a := &app{
		cfg: config.Config{
			Web: config.WebConfig{
				Enabled:  config.Bool(true),
				DeepSeek: config.DeepSeekWebConfig{BaseURL: server.URL, Model: "config-model", APIVersion: "2023-06-01", MaxTokens: 4096, MaxUses: 5},
			},
		},
		reg: tools.New(),
		log: session.New(),
	}
	t.Setenv("WEB_SEARCH_RUNTIME_KEY", "dynamic-search-key")
	a.applyNativeWebSearchSettings(map[string]any{
		"apiKeyEnv": "WEB_SEARCH_RUNTIME_KEY", "baseURL": server.URL,
		"model": "deepseek-v4-pro", "apiVersion": "2024-02-01",
		"maxTokens": 2048, "maxUses": 2,
	})
	if err := a.registerWeb(); err != nil {
		t.Fatalf("registerWeb: %v", err)
	}
	a.reg.SetPolicy(tools.Policy{Enabled: []string{"web_search"}, OutputLimit: 0})
	res, err := a.reg.Execute(context.Background(), "web_search", json.RawMessage(`{"queries":["dynamic settings"]}`))
	if err != nil || res.IsError {
		t.Fatalf("web search failed: result=%+v err=%v", res, err)
	}
	select {
	case body := <-requests:
		if body["model"] != "deepseek-v4-pro" {
			t.Fatalf("request model = %#v", body["model"])
		}
		if body["max_tokens"] != float64(2048) {
			t.Fatalf("request max_tokens = %#v", body["max_tokens"])
		}
		toolsPayload, ok := body["tools"].([]any)
		if !ok || len(toolsPayload) != 1 {
			t.Fatalf("request tools = %#v", body["tools"])
		}
		tool, _ := toolsPayload[0].(map[string]any)
		if tool["max_uses"] != float64(2) {
			t.Fatalf("request max_uses = %#v", tool["max_uses"])
		}
	case <-time.After(time.Second):
		t.Fatal("provider request did not reach endpoint")
	}

	events := a.log.Events()
	if len(events) != 1 || events[0].Type != session.EventWebSearchLLMRequest {
		t.Fatalf("search request events = %#v, want one %s", events, session.EventWebSearchLLMRequest)
	}
	if history := a.log.DeriveHistory(); len(history) != 0 {
		t.Fatalf("log-only search request entered model history: %#v", history)
	}
}
