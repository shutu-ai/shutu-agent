//go:build windows

package jobs

import (
	"context"
	"testing"
	"time"
)

// TestRunCommandLineBoundedKillsWindowsDescendants proves the cancellation
// path closes the shell's child process as well as cmd.exe. Without the Job
// Object, ping keeps the inherited output pipes open after the shell is killed
// and this bounded runner cannot reach quiescence promptly.
func TestRunCommandLineBoundedKillsWindowsDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	outcome, err := runCommandLineBounded("ping -n 30 127.0.0.1 >NUL", t.TempDir())(ctx)
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if outcome.Status != StatusKilled {
		t.Fatalf("outcome = %+v, want killed", outcome)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("descendant survived cancellation for %v", elapsed)
	}
}
