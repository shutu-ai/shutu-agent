package contractfixture_test

// This matrix is intentionally assembled outside the individual providers.
// The release gate needs one consumer-facing proof that capability failures
// are stable, fail closed, and do not accidentally execute the side effect
// they rejected. The detailed lifecycle tests remain in their owning packages.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/code"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/persistence"
	"github.com/shutu-ai/shutu-agent/internal/profile"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/web"
)

func TestNegativeCapabilityMatrix(t *testing.T) {
	t.Run("tool admission and schema are fail closed", func(t *testing.T) {
		registry := tools.New()
		tool := &negativeMatrixTool{}
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}

		if _, err := registry.Execute(context.Background(), "missing", []byte(`{}`)); tools.ErrorInfoOf(err).Code != tools.CodeUnknownTool {
			t.Fatalf("unknown tool = %+v, want %s", tools.ErrorInfoOf(err), tools.CodeUnknownTool)
		}
		if _, err := registry.Execute(context.Background(), tool.Name(), []byte(`{}`)); tools.ErrorInfoOf(err).Code != tools.CodeToolDenied {
			t.Fatalf("disabled tool = %+v, want %s", tools.ErrorInfoOf(err), tools.CodeToolDenied)
		}
		registry.SetPolicy(tools.Policy{Enabled: []string{tool.Name()}})
		if _, err := registry.Execute(context.Background(), tool.Name(), []byte(`{"value":7}`)); tools.ErrorInfoOf(err).Code != tools.CodeInvalidArgs {
			t.Fatalf("bad schema = %+v, want %s", tools.ErrorInfoOf(err), tools.CodeInvalidArgs)
		}
		if tool.called.Load() {
			t.Fatal("rejected tool calls executed the tool body")
		}
	})

	t.Run("sandbox capability mismatch is rejected before provider", func(t *testing.T) {
		provider := &negativeMatrixSandbox{}
		engine := code.NewEngine(provider)
		defer engine.Close()
		if _, err := engine.Run(context.Background(), code.RunRequest{Code: "must-not-run"}); !errors.Is(err, code.ErrSandboxUnavailable) {
			t.Fatalf("workspace request = %v, want ErrSandboxUnavailable", err)
		}
		if provider.called.Load() {
			t.Fatal("sandbox provider ran despite an unadvertised workspace mode")
		}
	})

	t.Run("optional profile is explicit unsupported", func(t *testing.T) {
		registry := profile.Local()
		if err := registry.Use(profile.IDSandboxesE2B); !errors.Is(err, profile.ErrProfileUnsupported) {
			t.Fatalf("e2b profile = %v, want ErrProfileUnsupported", err)
		}
	})

	t.Run("unknown event does not advance durable sequence", func(t *testing.T) {
		log := session.New()
		if _, err := log.Append("future/required-event", map[string]any{}); !errors.Is(err, session.ErrUnknownRequiredEvent) {
			t.Fatalf("unknown event = %v, want ErrUnknownRequiredEvent", err)
		}
		if got := log.NextSeq(); got != 1 {
			t.Fatalf("sequence after rejected event = %d, want 1", got)
		}
	})

	t.Run("expired approval is terminal and owner scoped", func(t *testing.T) {
		engine := interact.NewEngine(nil)
		defer engine.Close()
		controller := any(engine).(interact.ExpiryController)
		controller.SetRequestTTL(time.Nanosecond)
		requester := any(engine).(interact.SessionRequester)
		request, err := requester.RequestForSession(context.Background(), "owner-a", "approve", "write", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
		items, err := engine.List(context.Background())
		if err != nil || len(items) != 1 || items[0].Status != interact.StatusExpired {
			t.Fatalf("expired approval = %+v, err=%v", items, err)
		}
		resolver := any(engine).(interact.SessionResolver)
		if _, err := resolver.ResolveForSession(context.Background(), "owner-b", request.ID, interact.StatusApproved); !errors.Is(err, interact.ErrWrongSession) {
			t.Fatalf("cross-owner resolve = %v, want ErrWrongSession", err)
		}
		if _, err := engine.Resolve(context.Background(), request.ID, interact.StatusApproved); !errors.Is(err, interact.ErrAlreadyResolved) {
			t.Fatalf("resolve after expiry = %v, want ErrAlreadyResolved", err)
		}
	})

	t.Run("network loss is a provider error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close()
		provider := web.NewHttpFetchProvider(web.FetchLimits{TimeoutMs: 500})
		if _, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: url}); !errors.Is(err, web.ErrProvider) {
			t.Fatalf("closed endpoint = %v, want ErrProvider", err)
		}
	})
}

// TestNegativeCrossProcessOracles keeps the release matrix honest about
// process boundaries. Package-local tests can prove a provider error or a
// worker callback, but they cannot prove that the public child process settles
// without inventing a durable result after it loses its external dependency.
func TestNegativeCrossProcessOracles(t *testing.T) {
	t.Run("worker death preserves only the committed prefix", func(t *testing.T) {
		root := t.TempDir()
		jsonl, err := persistence.OpenJSONL(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := jsonl.Create(context.Background(), persistence.Header{ID: "worker-death"}, nil); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(root, "worker.ready")
		cmd := negativeHelperCommand(t, "worker", root, ready, "")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker helper: %v", err)
		}
		if err := waitForNegativeHelperFile(ready); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("worker helper unexpectedly exited successfully after abnormal worker death")
		}

		loaded, err := jsonl.Load(context.Background(), "worker-death")
		if err != nil {
			t.Fatalf("load post-death session: %v", err)
		}
		if len(loaded.Events) != 5 {
			t.Fatalf("post-death recovered event count = %d, want committed prefix plus one interrupted step/end pair: %#v", len(loaded.Events), loaded.Events)
		}
		if err := session.ValidateLifecycle(loaded.Events); err != nil {
			t.Fatalf("committed crash prefix became unreplayable: %v", err)
		}
		if loaded.Events[3].Type != session.EventStepEnd || loaded.Events[4].Type != session.EventTurnEnd {
			t.Fatalf("post-death recovery did not close the open lifecycle: %#v", loaded.Events)
		}
		for _, event := range loaded.Events {
			if event.Type == session.EventToolResult {
				t.Fatal("worker death invented a tool/result after no result was committed")
			}
		}
	})

	t.Run("network loss settles in a child without durable mutation", func(t *testing.T) {
		root := t.TempDir()
		jsonl, err := persistence.OpenJSONL(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := jsonl.Create(context.Background(), persistence.Header{ID: "network-loss"}, nil); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close()
		status := filepath.Join(root, "network.status")
		cmd := negativeHelperCommand(t, "network", root, status, url)
		if err := cmd.Run(); err != nil {
			t.Fatalf("network helper: %v", err)
		}
		encoded, err := os.ReadFile(status)
		if err != nil {
			t.Fatalf("read network helper status: %v", err)
		}
		if string(encoded) != "provider-error" {
			t.Fatalf("network helper status = %q, want provider-error", encoded)
		}
		loaded, err := jsonl.Load(context.Background(), "network-loss")
		if err != nil {
			t.Fatalf("load post-network-loss session: %v", err)
		}
		if len(loaded.Events) != 0 {
			t.Fatalf("network loss mutated durable session: %#v", loaded.Events)
		}
	})
}

func negativeHelperCommand(t *testing.T, mode, root, status, url string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNegativeCrossProcessHelper$")
	cmd.Env = append(os.Environ(),
		"SHUTU_NEGATIVE_HELPER="+mode,
		"SHUTU_NEGATIVE_ROOT="+root,
		"SHUTU_NEGATIVE_STATUS="+status,
		"SHUTU_NEGATIVE_URL="+url,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}

func waitForNegativeHelperFile(path string) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("negative helper did not publish readiness before deadline")
}

// TestNegativeCrossProcessHelper is launched only by
// TestNegativeCrossProcessOracles. Keeping the helper in this package makes
// the test exercise the same compiled public process boundary as the release
// binary's child fixtures.
func TestNegativeCrossProcessHelper(t *testing.T) {
	mode := os.Getenv("SHUTU_NEGATIVE_HELPER")
	if mode == "" {
		t.Skip("negative cross-process helper")
	}
	root := os.Getenv("SHUTU_NEGATIVE_ROOT")
	status := os.Getenv("SHUTU_NEGATIVE_STATUS")
	switch mode {
	case "worker":
		jsonl, err := persistence.OpenJSONL(root)
		if err != nil {
			t.Fatal(err)
		}
		events := []session.Event{
			{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1})},
			{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(2).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1, "step": 1})},
			{Seq: 3, Type: session.EventToolCall, At: time.UnixMilli(3).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1, "step": 1, "callId": "worker-call", "name": "negative", "arguments": "{}"})},
		}
		if err := jsonl.Append(context.Background(), "worker-death", events); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(status, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Model an abrupt worker death without relying on TerminateProcess
		// permissions of the host test runner. The committed prefix is all the
		// parent may recover; no terminal result has been manufactured.
		os.Exit(23)
	case "network":
		provider := web.NewHttpFetchProvider(web.FetchLimits{TimeoutMs: 500})
		_, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: os.Getenv("SHUTU_NEGATIVE_URL")})
		result := "unexpected-success"
		if errors.Is(err, web.ErrProvider) {
			result = "provider-error"
		}
		if writeErr := os.WriteFile(status, []byte(result), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	default:
		t.Fatalf("unknown negative helper mode %q", mode)
	}
}

type negativeMatrixTool struct{ called atomic.Bool }

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func (t *negativeMatrixTool) Name() string        { return "negative_matrix" }
func (t *negativeMatrixTool) Description() string { return "negative matrix fixture" }
func (t *negativeMatrixTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
		"required": []string{"value"},
	}
}
func (t *negativeMatrixTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}
func (t *negativeMatrixTool) Execute(context.Context, any) (string, error) {
	t.called.Store(true)
	return "unexpected", nil
}

type negativeMatrixSandbox struct{ called atomic.Bool }

func (*negativeMatrixSandbox) Name() string { return "negative-matrix-sandbox" }
func (p *negativeMatrixSandbox) Capabilities() code.SandboxCapabilities {
	return code.SandboxCapabilities{Available: true, SupportedModes: []code.SandboxMode{code.SandboxFullAccess}}
}
func (p *negativeMatrixSandbox) Run(context.Context, code.RunRequest) (code.Result, error) {
	p.called.Store(true)
	return code.Result{}, nil
}
func (*negativeMatrixSandbox) Close() error { return nil }
