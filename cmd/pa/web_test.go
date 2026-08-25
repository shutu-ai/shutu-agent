package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeWebApp builds a minimal app for web wiring tests: only the fields
// registerWeb touches (cfg.Web, reg, log) are set.
func makeWebApp(webEnabled bool) *app {
	return &app{
		cfg: config.Config{
			Web: config.WebConfig{Enabled: config.Bool(webEnabled)},
		},
		reg: tools.New(),
		log: session.New(),
	}
}

// TestRegisterWebDisabledRegistersNothing verifies the D10 gate: with
// web.enabled=false the composition root creates no Engine and registers no
// web_* tool (dispatch-m7-2 §6 / 自测: enabled=false 不注册) — even with a
// DEEPSEEK_API_KEY present.
func TestRegisterWebDisabledRegistersNothing(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	a := makeWebApp(false)
	if err := a.registerWeb(); err != nil {
		t.Fatalf("registerWeb: %v", err)
	}
	if a.web != nil {
		t.Fatal("web Engine must be nil when web.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		switch spec.Name {
		case "web_search", "web_fetch":
			t.Fatalf("%s registered while web disabled", spec.Name)
		}
	}
}

// TestRegisterWebEnabledRegistersAndValidates verifies the enabled path: the
// Engine is created, the two web_* tools are registered, D7 rejects bad
// arguments at the Execute gate, and a valid web_search call with no
// DEEPSEEK_API_KEY returns the readable no-provider error (env-key gating
// without touching the network).
func TestRegisterWebEnabledRegistersAndValidates(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "") // force the search provider to stay unregistered
	a := makeWebApp(true)
	a.reg.SetPolicy(tools.Policy{Enabled: []string{"web_search", "web_fetch"}, Timeout: 0, OutputLimit: 0})
	if err := a.registerWeb(); err != nil {
		t.Fatalf("registerWeb: %v", err)
	}
	if a.web == nil {
		t.Fatal("web Engine must be created when web.enabled=true")
	}
	found := map[string]bool{}
	for _, s := range a.reg.Specs() {
		found[s.Name] = true
	}
	for _, name := range []string{"web_search", "web_fetch"} {
		if !found[name] {
			t.Fatalf("%s not registered when web.enabled=true", name)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct{ name, args string }{
		{"web_search", `{}`},                                // missing required queries
		{"web_search", `{"queries":[]}`},                    // minItems: 1
		{"web_search", `{"queries":["a","b","c","d","e"]}`}, // maxItems: 4 (default)
		{"web_search", `{"queries":["a"],"extra":1}`},       // additional properties rejected
		{"web_search", `{"queries":[1]}`},                   // items must be strings
		{"web_fetch", `{}`},                                 // missing required url
		{"web_fetch", `{"url":1}`},                          // url must be a string
		{"web_fetch", `{"url":"https://x","extra":1}`},      // additional properties rejected
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid web_search with no key: the tool is wired, and the env-key
	// gating produces the readable no-provider error (no network).
	if res, err := a.reg.Execute(context.Background(), "web_search", json.RawMessage(`{"queries":["golang"]}`)); err != nil || !res.IsError {
		t.Fatalf("web_search without DEEPSEEK_API_KEY must return a structured error: result=%+v err=%v", res, err)
	} else if !strings.Contains(res.Output, "no search provider") {
		t.Errorf("output = %q, want the no-provider hint", res.Output)
	}
	// A failed call logs nothing at the tool layer (D3: web/search-request
	// only fires inside a provider OnRequest; the tool layer emits no web
	// event, and there was no provider to dispatch through).
	if n := len(a.log.Events()); n != 0 {
		t.Fatalf("failed web_search logged %d events, want none", n)
	}
}
