package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// fakeAccess implements TerminalAccess for tests. It does not enforce an
// owner fence — owner checks live in the composed root's GetActive, not here.
type fakeAccess struct {
	sess  *Session
	owner string
}

func (f *fakeAccess) Owner() string { return f.owner }

func (f *fakeAccess) GetActive() (*Session, error) {
	if f.sess == nil {
		return nil, fmt.Errorf("%w", ErrNoActive)
	}
	return f.sess, nil
}

func (f *fakeAccess) Start(opts SessionOpts) (*Session, error) {
	if f.sess != nil {
		return nil, fmt.Errorf("already active")
	}
	f.sess, _ = NewSession(opts)
	return f.sess, nil
}

func (f *fakeAccess) Stop() error { f.sess = nil; return nil }

func newTestTools(t *testing.T) (tools *TerminalTools, acc *fakeAccess, events *[]string) {
	events = &[]string{}
	acc = &fakeAccess{owner: "s-1"}
	tools = NewTerminalTools(acc, func(typ string, data any) {
		*events = append(*events, typ)
	})
	return tools, acc, events
}

func execTool(t *testing.T, tool tools.Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), b)
}

func testSessionOpts() SessionOpts {
	return SessionOpts{IdleMS: 100, TimeoutMS: 2000}
}

func containsEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

// TestPwshFirstUseStartsSessionAndRunsCommand verifies the dsh flow: the
// first pwsh call starts the persistent shell implicitly and runs the
// command, and terminal/start is emitted.
func TestPwshFirstUseStartsSessionAndRunsCommand(t *testing.T) {
	tt, acc, evts := newTestTools(t)
	if acc.sess != nil {
		t.Fatal("no session must exist before the first pwsh call")
	}

	out, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "hi")
	}
	if acc.sess == nil {
		t.Fatal("first pwsh call must leave an active session")
	}
	if !containsEvent(*evts, session.EventTerminalStart) {
		t.Fatalf("events = %v, want %q", *evts, session.EventTerminalStart)
	}
}

// TestPwshReusesSession verifies a second call runs in the same session
// (persistent shell) without starting a new one.
func TestPwshReusesSession(t *testing.T) {
	tt, acc, evts := newTestTools(t)
	if _, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo one"}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	first := acc.sess

	out, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo two"})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "two") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "two")
	}
	if acc.sess != first {
		t.Fatal("the second pwsh call must reuse the same session")
	}
	if count := countEvents(*evts, session.EventTerminalStart); count != 1 {
		t.Fatalf("terminal/start emitted %d times, want 1", count)
	}
}

func countEvents(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// TestPwshRejectsEmptyCommand verifies an empty command errors before any
// session is created.
func TestPwshRejectsEmptyCommand(t *testing.T) {
	tt, acc, _ := newTestTools(t)
	for _, args := range []map[string]any{{"command": ""}, {}} {
		if _, err := execTool(t, tt.Pwsh(), args); err == nil {
			t.Fatalf("pwsh with args %v must error", args)
		}
	}
	if acc.sess != nil {
		t.Fatal("no session may be created for an empty command")
	}
}

// TestPwshAfterExitStartsFresh verifies a call after the shell exited starts
// a fresh session (the exited session is replaced on next use).
func TestPwshAfterExitStartsFresh(t *testing.T) {
	tt, acc, _ := newTestTools(t)
	if _, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo start"}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	first := acc.sess

	// Send exit through the session directly (the tool has no exit verb).
	if err := first.Signal("stop"); err != nil {
		t.Logf("signal stop: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	acc.sess = nil

	out, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo fresh"})
	if err != nil {
		t.Fatalf("pwsh after exit: %v", err)
	}
	if !strings.Contains(out, "fresh") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "fresh")
	}
	if acc.sess == nil || acc.sess == first {
		t.Fatal("a fresh session must be started after the old one closed")
	}
}

// TestPwshTruncatesViewport verifies oversized viewport output is truncated
// with the marker.
func TestPwshTruncatesViewport(t *testing.T) {
	tt, _, _ := newTestTools(t)
	if _, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo " + strings.Repeat("x", 9000)}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	// The first call drains its own viewport, so drive a second large output.
	out, err := execTool(t, tt.Pwsh(), map[string]any{"command": "echo " + strings.Repeat("y", 9000)})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "[terminal output truncated]") {
		t.Fatalf("output = %q, want truncation marker", out)
	}
}
