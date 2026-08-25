package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
)

func TestDefaultPolicyUsesDshRunCommandTimeout(t *testing.T) {
	p := DefaultPolicy()
	if p.Timeout != DefaultTimeout {
		t.Fatalf("ordinary tool timeout = %s, want %s", p.Timeout, DefaultTimeout)
	}
	if p.RunCommand.Timeout != DefaultRunCommandTimeout {
		t.Fatalf("run_command timeout = %s, want %s", p.RunCommand.Timeout, DefaultRunCommandTimeout)
	}
}

// blockUntilCtxDone is a tool whose Execute waits on the context; the Execute
// pipeline's deadline or a caller cancellation is what unblocks it. It stands
// in for a slow command (dispatch-m3: sleep 工具被掐断).
type blockUntilCtxDone struct{ name string }

func (b blockUntilCtxDone) Name() string        { return b.name }
func (b blockUntilCtxDone) Description() string { return "blocks until the context is done" }
func (b blockUntilCtxDone) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (b blockUntilCtxDone) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestDefaultPolicyIsReadOnlyWhitelist(t *testing.T) {
	p := DefaultPolicy()
	if !p.Allows("get_time") || !p.Allows("read") {
		t.Fatalf("default whitelist must contain the read-only tools: %v", p.Enabled)
	}
	if p.Allows("bash") {
		t.Fatal("run_command must not be whitelisted by default")
	}
	if p.Timeout != DefaultTimeout {
		t.Fatalf("default timeout = %v, want %v", p.Timeout, DefaultTimeout)
	}
	if p.OutputLimit != DefaultOutputLimit {
		t.Fatalf("default output limit = %d, want %d", p.OutputLimit, DefaultOutputLimit)
	}
}

// TestExecuteNotEnabledToolRejected is the whitelist gate: a registered tool
// that is not whitelisted must be refused at Execute (未启用 ⇒ 拒绝执行), and
// the tool body must never run.
func TestExecuteNotEnabledToolRejected(t *testing.T) {
	r := New()
	r.Register(GetTime{})
	r.SetPolicy(Policy{Enabled: []string{"read"}}) // get_time not enabled

	_, err := r.Execute(context.Background(), "get_time", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("get_time must be rejected when not whitelisted")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("error = %v, want 'not enabled'", err)
	}
}

// TestExecuteEmptyWhitelistRejectsAll verifies whitelist semantics: an empty
// enabled list allows nothing, it is never an allow-all.
func TestExecuteEmptyWhitelistRejectsAll(t *testing.T) {
	r := New()
	r.Register(GetTime{})
	r.SetPolicy(Policy{})
	if _, err := r.Execute(context.Background(), "get_time", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty whitelist must reject every tool")
	}
}

// TestExecuteTimeout is the per-tool deadline: a tool that blocks past the
// configured timeout is interrupted and the error mentions the timeout, which
// the loop logs as tool/error (dispatch-m3).
func TestExecuteTimeout(t *testing.T) {
	r := New()
	r.Register(blockUntilCtxDone{name: "slow_tool"})
	r.SetPolicy(Policy{
		Enabled:     []string{"slow_tool"},
		Timeout:     100 * time.Millisecond,
		OutputLimit: DefaultOutputLimit,
	})

	start := time.Now()
	_, err := r.Execute(context.Background(), "slow_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout mention", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want immediate interruption", elapsed)
	}
}

// TestExecuteNoDeadlineWhenPolicyZero verifies an explicit zero timeout means
// "no deadline" (the tool's own context governs), so tests and a deliberate
// configuration choice can disable the pipeline deadline.
func TestExecuteNoDeadlineWhenPolicyZero(t *testing.T) {
	done := make(chan struct{})
	r := New()
	r.Register(&waiterTool{done: done})
	r.SetPolicy(Policy{Enabled: []string{"waiter"}})

	resCh := make(chan ToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := r.Execute(context.Background(), "waiter", json.RawMessage(`{}`))
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res
	}()

	// No pipeline deadline: Execute must still be blocked after 100ms.
	select {
	case res := <-resCh:
		t.Fatalf("execute returned early with no deadline: %+v", res)
	case err := <-errCh:
		t.Fatalf("execute errored early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	select {
	case res := <-resCh:
		if res.Output != "done" {
			t.Fatalf("output = %q, want done", res.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execute did not return after the tool finished")
	}
}

// waiterTool returns only after done is closed (or the context ends).
type waiterTool struct{ done chan struct{} }

func (w *waiterTool) Name() string        { return "waiter" }
func (w *waiterTool) Description() string { return "waits" }
func (w *waiterTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (w *waiterTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.done:
		return "done", nil
	}
}

func TestPolicyFromConfigMapsAndDefaults(t *testing.T) {
	cfg := config.ToolsConfig{
		Enabled:     []string{"get_time", "read", "bash"},
		Timeout:     config.Duration{Duration: 7 * time.Second},
		OutputLimit: 1024,
		RunCommand: config.RunCommandConfig{
			Enabled: true,
			Timeout: config.Duration{Duration: time.Minute},
			Workdir: `C:\work`,
		},
	}
	p := PolicyFromConfig(cfg, "data")
	if !p.Allows("bash") {
		t.Fatal("run_command must be whitelisted")
	}
	if p.Timeout != 7*time.Second {
		t.Fatalf("timeout = %v", p.Timeout)
	}
	if p.OutputLimit != 1024 {
		t.Fatalf("output limit = %d", p.OutputLimit)
	}
	if !p.RunCommand.Enabled || p.RunCommand.Timeout != time.Minute || p.RunCommand.Workdir != `C:\work` {
		t.Fatalf("run command policy = %+v", p.RunCommand)
	}
	if p.SpillDir != filepath.Join("data", "spill") {
		t.Fatalf("spill dir = %q, want %q", p.SpillDir, filepath.Join("data", "spill"))
	}
}
