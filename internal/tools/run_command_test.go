package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sleepCommand returns a command that sleeps about `seconds` seconds, portable
// across Windows (cmd /C) and POSIX (sh -c).
func sleepCommand(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul", seconds+1)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

func TestScrubEnvRemovesCredentialShapedEntries(t *testing.T) {
	env := []string{
		"DEEPSEEK_API_KEY=sk-secret",
		"MY_SECRET_TOKEN=leak",
		"POSTGRES_PASSWORD=hunter2",
		"OPENAI_KEY=abc",
		"HOME=/home/x",
		"PATH=/usr/bin",
		"TMP=/tmp",
	}
	got := scrubEnv(env)
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		if isSensitiveEnvName(name) {
			t.Fatalf("credential-shaped name %q survived the scrub: %v", name, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("scrubbed env = %v, want 3 kept entries", got)
	}
}

// TestRunCommandNotRegisteredByDefault verifies run_command is not available
// out of the box: it is neither registered nor whitelisted, so Execute rejects
// it as unknown (dispatch-m3: 默认关闭).
func TestRunCommandNotRegisteredByDefault(t *testing.T) {
	r := New() // default policy: read-only whitelist, no run_command registered
	if _, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"echo hi"}`)); err == nil {
		t.Fatal("run_command must not execute by default")
	}
	for _, spec := range r.Specs() {
		if spec.Name == "bash" {
			t.Fatal("run_command must not be advertised to the model by default")
		}
	}
}

// TestRunCommandRegisteredAndExecutes verifies that once enabled and
// registered, run_command executes a single line and returns its output.
func TestRunCommandRegisteredAndExecutes(t *testing.T) {
	r := New()
	r.Register(NewRunCommand(""))
	r.SetPolicy(Policy{
		Enabled: []string{"bash"},
		Timeout: time.Hour,
		RunCommand: RunCommandPolicy{
			Enabled: true,
		},
	})
	res, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("output = %q, want echo output", res.Output)
	}
}

// TestRunCommandNonZeroExit reports the exit code inline as a normal result,
// so the model sees the output together with the marker.
func TestRunCommandNonZeroExit(t *testing.T) {
	r := New()
	r.Register(NewRunCommand(""))
	r.SetPolicy(Policy{
		Enabled: []string{"bash"},
		Timeout: time.Hour,
		RunCommand: RunCommandPolicy{
			Enabled: true,
		},
	})
	res, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"exit 3"}`))
	if err != nil {
		t.Fatalf("non-zero exit must not be a hard error: %v", err)
	}
	if !strings.HasPrefix(res.Output, "[exit code: 3]") {
		t.Fatalf("output = %q, want exit-code marker", res.Output)
	}
}

// TestRunCommandScrubbedEnv verifies 环境清除: a credential-shaped parent env
// variable is never inherited by the child command.
func TestRunCommandScrubbedEnv(t *testing.T) {
	t.Setenv("PA_TEST_SECRET_TOKEN", "leakme-123")
	var echo string
	if runtime.GOOS == "windows" {
		echo = "echo %PA_TEST_SECRET_TOKEN%"
	} else {
		echo = "echo $PA_TEST_SECRET_TOKEN"
	}
	r := New()
	r.Register(NewRunCommand(""))
	r.SetPolicy(Policy{
		Enabled: []string{"bash"},
		Timeout: time.Hour,
		RunCommand: RunCommandPolicy{
			Enabled: true,
		},
	})
	res, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"`+echo+`"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(res.Output, "leakme-123") {
		t.Fatalf("secret leaked into child output: %q", res.Output)
	}
}

// TestRunCommandTimeout verifies the run_command-specific deadline override:
// a long command is killed at the override and the pipeline reports the
// timeout (the loop logs tool/error).
func TestRunCommandTimeout(t *testing.T) {
	r := New()
	r.Register(NewRunCommand(""))
	r.SetPolicy(Policy{
		Enabled: []string{"bash"},
		Timeout: 0, // global deadline disabled; the override below governs
		RunCommand: RunCommandPolicy{
			Enabled: true,
			Timeout: 300 * time.Millisecond,
		},
	})
	start := time.Now()
	_, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"`+sleepCommand(10)+`"}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout mention", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want prompt kill", elapsed)
	}
}

// TestRunCommandCancelled interrupts an executing command via context
// cancellation (Ctrl+C path) — the process is killed through
// exec.CommandContext and Execute returns promptly (dispatch-m3: 取消中断执行中命令).
func TestRunCommandCancelled(t *testing.T) {
	r := New()
	r.Register(NewRunCommand(""))
	r.SetPolicy(Policy{
		Enabled: []string{"bash"},
		Timeout: time.Hour,
		RunCommand: RunCommandPolicy{
			Enabled: true,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	args := json.RawMessage(`{"command":"` + sleepCommand(30) + `"}`)

	var err error
	done := make(chan struct{})
	go func() {
		_, err = r.Execute(ctx, "bash", args)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond) // let the command start
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executing command was not interrupted by cancellation")
	}
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want interruption error", err)
	}
}
