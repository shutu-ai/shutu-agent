//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package code

import (
	"bufio"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestProcessTreeKillsBoundedForkBombGroup exercises the unsafe branch of a
// fork bomb without creating an unbounded process explosion. Four nested
// binary forks are enough to prove that descendants created after the direct
// child entered its own process group are owned by the runtime; an infinite
// fork bomb would add no extra ownership information and would endanger the
// test host.
func TestProcessTreeKillsBoundedForkBombGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("process-group fork fixture is skipped in -short mode")
	}
	script := `
	recurse() {
		if [ "$1" -le 0 ]; then
			sleep 30
			return
		fi
		recurse "$(("$1" - 1))" &
		recurse "$(("$1" - 1))" &
		wait
	}
	echo ready
	recurse 4
	exec sleep 30
	`
	cmd := exec.Command("/bin/sh", "-c", script)
	prepareProcessTree(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bounded fork-bomb fixture: %v", err)
	}

	tree, err := attachProcessTree(cmd, processTreeLimits{maxProcesses: 1})
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("attach process group: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		_ = tree.Close()
		_ = cmd.Wait()
		t.Fatal("bounded fork-bomb descendants did not start")
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("get fixture process group: %v", err)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("fixture did not enter an owned process group")
	}
	if err := tree.Close(); err != nil {
		t.Fatalf("close process group: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait for killed fixture: %v", err)
		}
	}

	// Killed descendants are reparented and may retain a brief zombie edge.
	// The ownership oracle is bounded, not a polling-free claim.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(-pgid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		// A live-but-reparented descendant can briefly make the group check
		// succeed. That still means teardown has not reached quiescence, so it
		// is retryable exactly like an EPERM probe.
		if err != nil && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("owned process group still addressable after kill: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned process group remained live after kill: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
