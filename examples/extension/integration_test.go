package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/extensionhost"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

type integrationModel struct {
	mu        sync.Mutex
	toolCalls int
	requests  int
}

func (m *integrationModel) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	m.mu.Lock()
	index := m.requests
	m.requests++
	toolCalls := m.toolCalls
	m.mu.Unlock()
	if index < toolCalls {
		return newIntegrationReader([]llm.StreamEvent{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: fmt.Sprintf("call-%d", index+1), Name: "ext__demo__echo", Arguments: `{"text":"demo"}`,
			}},
		}}), nil
	}
	return newIntegrationReader([]llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "done"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}), nil
}

type integrationReader struct {
	events []llm.StreamEvent
	index  int
}

func newIntegrationReader(events []llm.StreamEvent) *integrationReader {
	return &integrationReader{events: events}
}

func (r *integrationReader) Next() (llm.StreamEvent, error) {
	if r.index >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := r.events[r.index]
	r.index++
	return event, nil
}

type integrationObserver struct {
	mu     sync.Mutex
	events []extensionhost.Event
}

func (o *integrationObserver) Observe(event extensionhost.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *integrationObserver) hasEventType(eventType string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, event := range o.events {
		if event.EventType == eventType && event.Delivered && !event.Queued {
			return true
		}
	}
	return false
}

func (o *integrationObserver) snapshot() []extensionhost.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]extensionhost.Event(nil), o.events...)
}

func (o *integrationObserver) waitForEventType(t *testing.T, eventType string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if o.hasEventType(eventType) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("extension did not observe %q; observed=%#v", eventType, o.snapshot())
}

func buildDemoExtension(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "demo-extension.exe")
	executable, err := os.Executable()
	if err == nil {
		binary = filepath.Join(filepath.Dir(executable), "demo-extension-integration.exe")
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build demo extension: %v\n%s", err, output)
	}
	return binary
}

func startDemoHost(t *testing.T, binary string, strategy extension.ContextStrategy, eventDelayMS int) (*extensionhost.Host, *tools.Registry, *integrationObserver) {
	t.Helper()
	manifestData, err := os.ReadFile("extension.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := extension.ParseManifest(manifestData)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Transport.Command = binary
	manifest.Transport.Env = append(manifest.Transport.Env, fmt.Sprintf("DEMO_EXTENSION_EVENT_DELAY_MS=%d", eventDelayMS))
	manifest.ContextProvider.Strategy = strategy
	manifestPath := filepath.Join(t.TempDir(), "extension.yaml")
	var rendered strings.Builder
	if err := extension.WriteManifest(&rendered, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(rendered.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tools.New()
	observer := &integrationObserver{}
	host := extensionhost.New(extensionhost.Config{
		Registry: registry, EventQueueSize: 256, EventTimeout: time.Second,
		GlobalContextChars: 4000, MaxContributionChars: 2000,
		GlobalContextTokens: 1000, MaxContributionTokens: 500,
		Sources: []extensionhost.Source{{
			ManifestPath: manifestPath,
			Grants:       []string{"session.id", "session.turn", "session.step", "workspace.path", "user.input"},
		}},
		Observer: observer.Observe,
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host, registry, observer
}

func runDemoTurn(t *testing.T, host *extensionhost.Host, registry *tools.Registry, sessionID string, toolCalls int) {
	t.Helper()
	publicTool := "ext__demo__echo"
	registry.SetPolicy(tools.Policy{Timeout: 5 * time.Second, Enabled: []string{publicTool}})
	model := &integrationModel{toolCalls: toolCalls}
	log := session.New()
	log.SetObserver(func(event session.Event) {
		host.PublishSessionEvent(sessionID, event)
	})
	agent := loop.New(loop.Config{
		LLM: model, Log: log, Tools: registry, Prompt: prompt.New("integration"),
		RuntimeSessionID: sessionID, RuntimeAgentID: sessionID,
		PreStep: []loop.PreStepInjector{host.ContextInjector()},
	})
	ctx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
		SessionID: sessionID, TurnID: "turn:1",
	})
	if err := agent.Run(ctx, "demo integration probe"); err != nil {
		t.Fatalf("agent loop: %v", err)
	}
}

func countDeliveredEventTypes(observer *integrationObserver, eventType string) int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	count := 0
	for _, event := range observer.events {
		if event.EventType == eventType && event.Delivered && !event.Queued {
			count++
		}
	}
	return count
}

func waitForDeliveredEventCount(t *testing.T, observer *integrationObserver, eventType string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countDeliveredEventTypes(observer, eventType) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("delivered %s events = %d, want >= %d", eventType, countDeliveredEventTypes(observer, eventType), want)
}

func TestDemoExtensionRuntimeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test builds and runs the independent demo extension")
	}
	binary := buildDemoExtension(t)

	t.Run("navigation events tool and context", func(t *testing.T) {
		host, registry, observer := startDemoHost(t, binary, extension.ContextBeforeEveryModelCall, 0)
		contributions := host.WebContributions()
		if len(contributions) != 1 || contributions[0].ExtensionID != "demo" ||
			contributions[0].Route != "/extensions/demo/" || !contributions[0].Ready {
			t.Fatalf("web contributions = %#v", contributions)
		}
		if _, ok := registry.Registration("ext__demo__echo"); !ok {
			t.Fatal("demo echo tool was not registered")
		}
		host.PublishSessionStarted("demo-session")
		runDemoTurn(t, host, registry, "demo-session", 1)
		for _, eventType := range []string{
			extension.EventSessionStarted, extension.EventToolStarted, extension.EventToolCompleted,
			extension.EventContextRequested, extension.EventContextInjected,
		} {
			observer.waitForEventType(t, eventType)
		}
		if got := countDeliveredEventTypes(observer, extension.EventContextInjected); got != 2 {
			t.Fatalf("before_every_model_call injections = %d, want 2", got)
		}

		store, err := store.OpenSQLite(filepath.Join(t.TempDir(), "integration.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		server, err := webserver.New(store, "integration-token", "")
		if err != nil {
			t.Fatal(err)
		}
		routes := make([]webserver.ExtensionRoute, 0, len(contributions))
		for _, contribution := range contributions {
			routes = append(routes, webserver.ExtensionRoute{
				ExtensionID: contribution.ExtensionID, Title: contribution.Title, Route: contribution.Route,
				NavigationEnabled: contribution.NavigationEnabled, NavigationGroup: contribution.NavigationGroup,
				Order: contribution.Order, Ready: contribution.Ready, ServiceURL: contribution.ServiceURL,
			})
		}
		server.SetExtensionRoutes(routes)
		portal := httptest.NewServer(server.Handler())
		defer portal.Close()

		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, portal.URL+"/api/extensions", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer integration-token")
		response, err := portal.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var inventory struct {
			Extensions []struct {
				ExtensionID string `json:"extensionId"`
				Route       string `json:"route"`
				Ready       bool   `json:"ready"`
			} `json:"extensions"`
		}
		if err := json.NewDecoder(response.Body).Decode(&inventory); err != nil {
			t.Fatal(err)
		}
		if len(inventory.Extensions) != 1 || inventory.Extensions[0].ExtensionID != "demo" ||
			!inventory.Extensions[0].Ready || inventory.Extensions[0].Route != "/extensions/demo/" {
			t.Fatalf("portal inventory = %#v", inventory.Extensions)
		}

		request, err = http.NewRequestWithContext(context.Background(), http.MethodGet, portal.URL+"/extensions/demo/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer integration-token")
		response, err = portal.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		page, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || !strings.Contains(string(page), "Demo Extension") {
			t.Fatalf("proxied demo page = %d/%s", response.StatusCode, page)
		}
	})

	t.Run("strategy matrix", func(t *testing.T) {
		tests := []struct {
			name       string
			strategy   extension.ContextStrategy
			wantInject int
			manual     bool
		}{
			{name: "once per turn", strategy: extension.ContextOncePerTurn, wantInject: 1},
			{name: "after tool result", strategy: extension.ContextAfterToolResult, wantInject: 1},
			{name: "manual", strategy: extension.ContextManual, manual: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				host, registry, observer := startDemoHost(t, binary, test.strategy, 0)
				runDemoTurn(t, host, registry, "strategy-"+strings.ReplaceAll(string(test.strategy), "_", "-"), 1)
				got := countDeliveredEventTypes(observer, extension.EventContextInjected)
				if test.manual {
					if got != 0 {
						t.Fatalf("manual automatic injections = %d, want 0", got)
					}
					contributions, err := host.RefreshContext(context.Background(), "manual refresh")
					if err != nil || len(contributions) != 1 {
						t.Fatalf("manual refresh = %#v, %v", contributions, err)
					}
					waitForDeliveredEventCount(t, observer, extension.EventContextInjected, 1)
					return
				}
				waitForDeliveredEventCount(t, observer, extension.EventContextInjected, test.wantInject)
				if got := countDeliveredEventTypes(observer, extension.EventContextInjected); got != test.wantInject {
					t.Fatalf("%s injections = %d, want %d", test.name, got, test.wantInject)
				}
			})
		}
	})

	t.Run("slow subscriber restart and removal", func(t *testing.T) {
		host, _, observer := startDemoHost(t, binary, extension.ContextOncePerTurn, 20)
		started := time.Now()
		for i := 0; i < 20; i++ {
			host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("publish blocked for %s", elapsed)
		}
		waitForDeliveredEventCount(t, observer, extension.EventTurnStarted, 20)
		if err := host.Restart(context.Background(), "demo"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if observer.hasEventType(extension.EventExtensionRestarted) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !observer.hasEventType(extension.EventExtensionRestarted) {
			t.Fatalf("extension restart event was not delivered; observed=%#v", observer.snapshot())
		}
		contributions := host.WebContributions()
		if len(contributions) != 1 || !contributions[0].Ready {
			t.Fatalf("post-restart contributions = %#v", contributions)
		}
		restartCtx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
			SessionID: "post-restart-context", TurnID: "turn:1", StepID: "step:1",
		})
		restored, err := host.ProvideContext(restartCtx, "post-restart context", nil)
		if err != nil || len(restored) != 1 || !strings.Contains(restored[0].Content, "post-restart context") {
			t.Fatalf("post-restart context = %#v, %v", restored, err)
		}
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
		observer.waitForEventType(t, extension.EventExtensionStopped)
		if got := host.WebContributions(); len(got) != 0 {
			t.Fatalf("post-close contributions = %#v", got)
		}
		removedCtx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
			SessionID: "post-close-context", TurnID: "turn:1", StepID: "step:1",
		})
		if removed, err := host.ProvideContext(removedCtx, "post-close context", nil); err != nil || len(removed) != 0 {
			t.Fatalf("post-close context = %#v, %v", removed, err)
		}
	})
}
