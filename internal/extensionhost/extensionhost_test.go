package extensionhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

type captureLLM struct {
	requests []llm.ChatRequest
}

func (l *captureLLM) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	l.requests = append(l.requests, request)
	return emptyStreamReader{}, nil
}

type emptyStreamReader struct{}

func (emptyStreamReader) Next() (llm.StreamEvent, error) { return llm.StreamEvent{}, io.EOF }

func TestHostLifecycleContextToolsAndRestart(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "demo", extension.RestartOnFailure, 2)
	pidPath := filepath.Join(dir, "extension.pid")
	manifest := readManifestForTest(t, manifestPath)
	manifest.Transport.Env = append(manifest.Transport.Env, "EXTENSION_PID_FILE="+pidPath)
	rewriteTestManifest(t, manifestPath, manifest)
	registry := tools.New()
	registry.SetPolicy(tools.Policy{Timeout: 5 * time.Second})
	events := make(chan Event, 32)
	host := New(Config{
		Workspace: `C:\workspace`, StartupTimeout: 5 * time.Second, HealthTimeout: time.Second,
		ContextTimeout: 2 * time.Second, ShutdownTimeout: time.Second,
		GlobalContextChars: 160, MaxContributionChars: 80,
		Sources:      []Source{{ManifestPath: manifestPath, Grants: []string{"session.id", "session.turn", "session.step", "workspace.path", "user.input"}}},
		Registry:     registry,
		AllowedTools: map[string]struct{}{PublicToolName("demo", "record"): {}},
		Observer:     func(event Event) { events <- event },
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
		SessionID: "session-1", TurnID: "turn:1", StepID: "step:1",
	})
	contributions, err := host.ProvideContext(ctx, "find deployment risk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 1 || !strings.Contains(contributions[0].Content, "find deployment risk") {
		select {
		case event := <-events:
			t.Logf("provider event: %#v", event)
		default:
		}
		t.Fatalf("context contributions = %#v", contributions)
	}
	messages := host.ContextInjector().InjectWithError
	if messages == nil {
		t.Fatal("context injector is unavailable")
	}
	injected, err := messages(ctx, "find deployment risk")
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) != 1 || injected[0].SourcePlugin != "demo" || injected[0].Role != llm.RoleUser {
		t.Fatalf("injected messages = %#v", injected)
	}

	publicName := PublicToolName("demo", "record")
	if _, ok := registry.Registration(publicName); !ok {
		t.Fatalf("extension tool %q was not registered", publicName)
	}
	result, err := registry.Execute(ctx, publicName, []byte(`{"text":"saved"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "saved" {
		t.Fatalf("tool result = %q", result.Output)
	}
	sensitive := host.SensitiveTools()
	if len(sensitive) != 1 || sensitive[0] != publicName {
		t.Fatalf("sensitive tools = %#v", sensitive)
	}
	if err := host.Restart(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Execute(ctx, publicName, []byte(`{"text":"after-restart"}`)); err != nil {
		t.Fatal(err)
	}
	if err := host.Restart(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := host.Restart(context.Background(), "demo"); !errors.Is(err, ErrRestartExhausted) {
		t.Fatalf("third restart error = %v", err)
	}
	if health, err := host.Health(context.Background(), "demo"); err != nil || !health.Ready {
		t.Fatalf("health after restarts = %#v, %v", health, err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(pidPath); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed extension did not remove its pid marker on shutdown: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHostContextTimeoutAndCancellationFailSoft(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "slow", extension.RestartNever, 0)
	registry := tools.New()
	host := New(Config{
		StartupTimeout: 5 * time.Second, ContextTimeout: 20 * time.Millisecond,
		Sources:  []Source{{ManifestPath: manifestPath, Grants: []string{"user.input"}}},
		Registry: registry,
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if contributions, err := host.ProvideContext(context.Background(), "slow", nil); err != nil || len(contributions) != 0 {
		t.Fatalf("timeout result = %#v, %v", contributions, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.ProvideContext(cancelled, "cancelled", nil); err != nil {
		t.Fatalf("optional provider cancellation must fail soft: %v", err)
	}
}

func TestHostContextBudgetAndDeduplication(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "budget", extension.RestartNever, 0)
	host := New(Config{
		StartupTimeout: 5 * time.Second, ContextTimeout: time.Second,
		GlobalContextChars: 120, MaxContributionChars: 80,
		Sources:  []Source{{ManifestPath: manifestPath, Grants: []string{"user.input"}}},
		Registry: tools.New(),
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	contributions, err := host.ProvideContext(context.Background(), "large:abcdefghij", nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, contribution := range contributions {
		total += len(contribution.Content)
	}
	if total != 120 || len(contributions) != 2 || !strings.Contains(contributions[0].Content, "truncated by agent budget") {
		t.Fatalf("budgeted contributions = %#v, total=%d", contributions, total)
	}
	contributions, err = host.ProvideContext(context.Background(), "duplicate:same", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 1 {
		t.Fatalf("duplicate contributions = %#v", contributions)
	}
}

func TestExtensionProcessCrashFailsSoftAndRecovers(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "crash", extension.RestartOnFailure, 3)
	host := New(Config{
		StartupTimeout: 5 * time.Second, ContextTimeout: time.Second,
		Sources:  []Source{{ManifestPath: manifestPath, Grants: []string{"user.input"}}},
		Registry: tools.New(),
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if contributions, err := host.ProvideContext(context.Background(), "crash", nil); err != nil || len(contributions) != 0 {
		t.Fatalf("crashing provider result = %#v, %v", contributions, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		health, err := host.Health(context.Background(), "crash")
		if err == nil && health.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("extension did not recover: health=%#v, err=%v", health, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExtensionContextReachesModelRequest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "full", extension.RestartNever, 0)
	host := New(Config{
		StartupTimeout: 5 * time.Second, ContextTimeout: time.Second,
		Sources:  []Source{{ManifestPath: manifestPath, Grants: []string{"session.id", "session.turn", "session.step", "user.input"}}},
		Registry: tools.New(),
	})
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	model := &captureLLM{}
	log := session.New()
	agentLoop := loop.New(loop.Config{
		LLM: model, Log: log, Tools: tools.New(), Prompt: prompt.New("system"),
		RuntimeSessionID: "session-model", RuntimeAgentID: "session-model",
		PreStep: []loop.PreStepInjector{host.ContextInjector()},
	})
	if err := agentLoop.Run(context.Background(), "model question"); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model calls = %d", len(model.requests))
	}
	found := false
	for _, message := range model.requests[0].Messages {
		if strings.Contains(message.Text(), "Extension evidence for model question") {
			found = true
		}
	}
	if !found {
		t.Fatalf("model request did not contain extension context: %#v", model.requests[0].Messages)
	}
	web := host.WebContributions()
	if len(web) != 1 || web[0].Route != "/extensions/full/" || web[0].ServiceURL == "" {
		t.Fatalf("web contributions = %#v", web)
	}
	response, err := http.Get(web[0].ServiceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "independent extension") {
		t.Fatalf("extension web response = %d/%s", response.StatusCode, string(body))
	}
}

func TestHostRejectsProtocolMismatch(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestManifest(t, dir, "mismatch", extension.RestartNever, 0)
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("mismatch"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := readManifestForTest(t, manifestPath)
	manifest.Transport.Env = append(manifest.Transport.Env, "EXTENSION_TEST_MODE=mismatch")
	rewriteTestManifest(t, manifestPath, manifest)
	host := New(Config{StartupTimeout: 2 * time.Second, Sources: []Source{{ManifestPath: manifestPath, Required: true, Grants: []string{"user.input"}}}, Registry: tools.New()})
	err := host.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not satisfy required API") {
		t.Fatalf("protocol mismatch error = %v", err)
	}
}

func TestHostStartupFailureIsClassified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extension.yaml")
	manifest := readManifestForTest(t, writeTestManifest(t, dir, "missing", extension.RestartNever, 0))
	manifest.Transport.Command = "definitely-missing-shutu-extension-command"
	rewriteTestManifest(t, path, manifest)
	host := New(Config{StartupTimeout: time.Second, Sources: []Source{{ManifestPath: path, Required: true}}, Registry: tools.New()})
	if err := host.Start(context.Background()); err == nil {
		t.Fatal("missing executable must fail")
	}
}

func writeTestManifest(t *testing.T, dir, id string, restart extension.RestartPolicy, maxRestarts int) string {
	t.Helper()
	mode := "normal"
	if id == "slow" {
		mode = "slow"
	}
	if id == "full" {
		mode = "full"
	}
	manifest := extension.Manifest{
		ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Version: "0.1.0", ExtensionAPI: "1.0",
		Capabilities: extension.Capabilities{Tools: true, ContextProvider: true, Lifecycle: true, Health: true, Web: mode == "full"},
		Transport: extension.Transport{
			Type: "stdio", Command: os.Args[0],
			Args: []string{"-test.run=^TestExtensionHostHelperProcess$", "--"},
			Env:  []string{"GO_WANT_EXTENSION_PROCESS=1", "EXTENSION_TEST_MODE=" + mode},
		},
		Tools: extension.ToolsContribution{Definitions: []extension.ToolDefinition{{
			Name: "record", Description: "record input", Risk: extension.ToolRiskWrite,
			InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}},
			OutputSchema: map[string]any{"type": "string"},
		}}},
		ContextProvider: extension.ContextProviderConfig{Enabled: true, Strategy: extension.ContextBeforeEveryModelCall},
		Web:             extension.WebContribution{Enabled: mode == "full", Route: "/extensions/full/", Title: "Full"},
		Health:          extension.HealthConfig{Enabled: true, TimeoutMS: 1000},
		Lifecycle:       extension.LifecycleConfig{Enabled: true, StartupTimeoutMS: 3000, RestartPolicy: restart, MaxRestarts: maxRestarts},
		Permissions:     []extension.Permission{{Name: "session.id"}, {Name: "session.turn"}, {Name: "session.step"}, {Name: "workspace.path"}, {Name: "user.input", Required: true}},
	}
	path := filepath.Join(dir, "extension.yaml")
	rewriteTestManifest(t, path, manifest)
	return path
}

func readManifestForTest(t *testing.T, path string) extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := extension.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func rewriteTestManifest(t *testing.T, path string, manifest extension.Manifest) {
	t.Helper()
	var output strings.Builder
	if err := extension.WriteManifest(&output, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(output.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionHostHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_EXTENSION_PROCESS") != "1" {
		return
	}
	mode := os.Getenv("EXTENSION_TEST_MODE")
	if pidPath := os.Getenv("EXTENSION_PID_FILE"); pidPath != "" {
		if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
			os.Exit(1)
		}
		defer func() { _ = os.Remove(pidPath) }()
	}
	manifest := extension.Manifest{
		ID: "helper", Name: "Helper", Version: "0.1.0", ExtensionAPI: "1.0",
		RequiredAgentAPI: "1.0", Capabilities: extension.Capabilities{Tools: true, ContextProvider: true, Lifecycle: true, Health: true},
	}
	if mode == "mismatch" {
		manifest.RequiredAgentAPI = "2.0"
	}
	var webURL string
	var webServer *http.Server
	if mode == "full" {
		listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
		if listenErr != nil {
			os.Exit(1)
		}
		webURL = "http://" + listener.Addr().String() + "/"
		webServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "independent extension page")
		})}
		go func() { _ = webServer.Serve(listener) }()
		defer func() { _ = webServer.Close() }()
		manifest.Capabilities.Web = true
	}
	server := extension.NewServer(extension.ServerCallbacks{
		Manifest:   manifest,
		WebBaseURL: func() string { return webURL },
		Health: func(context.Context) (extension.HealthResult, error) {
			return extension.HealthResult{Ready: true, Status: "ready"}, nil
		},
		ProvideContext: func(ctx context.Context, request extension.ContextRequest) (extension.ContextResult, error) {
			if mode == "slow" {
				select {
				case <-ctx.Done():
					return extension.ContextResult{}, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
			if strings.HasPrefix(request.UserInput, "large:") {
				return extension.ContextResult{Contributions: []extension.ContextContribution{
					{Source: "high", Content: strings.Repeat("A", 160), Priority: 20, Truncatable: true},
					{Source: "low", Content: strings.Repeat("B", 100), Priority: 10, Truncatable: true},
				}}, nil
			}
			if request.UserInput == "crash" {
				os.Exit(2)
			}
			if strings.HasPrefix(request.UserInput, "duplicate:") {
				return extension.ContextResult{Contributions: []extension.ContextContribution{
					{Source: "same", Content: "same evidence", Truncatable: true},
					{Source: "same", Content: "same evidence", Truncatable: true},
				}}, nil
			}
			return extension.ContextResult{Contributions: []extension.ContextContribution{{
				Source: "demo", Content: fmt.Sprintf("Extension evidence for %s", request.UserInput), Priority: 10, EstimatedTokens: 8, Truncatable: true,
			}}}, nil
		},
		CallTool: func(_ context.Context, request extension.ToolCallRequest) (extension.ToolCallResult, error) {
			return extension.ToolCallResult{Value: request.Arguments["text"]}, nil
		},
	})
	if err := server.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
