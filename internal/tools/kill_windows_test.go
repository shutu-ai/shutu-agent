//go:build windows

package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunForegroundKillsWindowsDescendants covers the direct run_command
// cancellation seam. The child ping inherits stdout/stderr; a direct-child
// kill would leave those handles open and make the runner wait on its capture
// goroutines until ping's long timeout expires.
func TestRunForegroundKillsWindowsDescendants(t *testing.T) {
	runner := NewRunCommand(t.TempDir())
	// This is the force-kill containment test; graceful escalation does not
	// need another 3s wait here.
	runner.SettingsFunc = func() ShellSettings { return ShellSettings{GraceMS: 0} }
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := runner.runForeground(ctx, "ping -n 30 127.0.0.1 >NUL", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("cancelled command: %v", err)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("cancelled command output = %q, want timeout marker", out)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("descendant survived cancellation for %v: %v", elapsed, err)
	}
}
