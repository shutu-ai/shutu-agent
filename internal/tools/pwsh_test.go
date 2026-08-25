package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/jobs"
)

// requirePwsh skips the test when no PowerShell executable resolves (the
// host-accommodation dsh's pwsh suites use).
func requirePwsh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pwsh"); err != nil {
		if _, err2 := exec.LookPath("powershell.exe"); err2 != nil {
			t.Skip("no pwsh or powershell.exe on PATH")
		}
	}
}

func execPwsh(t *testing.T, tool PwshTool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), b)
}

// schemaJSON renders the tool schema for key-presence assertions.
func schemaJSON(t *testing.T, tool PwshTool) string {
	t.Helper()
	b, err := json.Marshal(tool.Schema())
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

// TestFormatPwshOutput pins the dsh render contract without needing pwsh:
// stdout body, a marked [stderr] section, (no output), and the exit-status
// markers — a clean exit produces no marker and the timed-out marker comes
// first.
func TestFormatPwshOutput(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string
		stderr    string
		exitCode  int
		signal    string
		timedOut  bool
		timeoutMS int64
		want      string
	}{
		{name: "clean", stdout: "hi\n", want: "hi\n"},
		{name: "stderr-section", stdout: "out\n", stderr: "err\n", want: "out\n[stderr]\nerr\n"},
		{name: "stderr-only", stderr: "err\n", want: "[stderr]\nerr\n"},
		{name: "no-output", want: "(no output)"},
		{name: "exit-marker", stdout: "x", exitCode: 3, want: "x\n[exit code: 3]"},
		{name: "timeout-marker", stdout: "x", exitCode: 1, timedOut: true, timeoutMS: 300, want: "x\n[timed out after 300ms]\n[exit code: 1]"},
		{name: "signal-marker", stdout: "x", signal: "SIGKILL", want: "x\n[killed by signal: SIGKILL]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPwshOutput(tc.stdout, tc.stderr, tc.exitCode, tc.signal, tc.timedOut, tc.timeoutMS)
			if got != tc.want {
				t.Fatalf("formatPwshOutput = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPwshSchema verifies the dsh parameter surface: command and description
// are required, and run_in_background is advertised exactly when a jobs
// registry backs the tool.
func TestPwshSchema(t *testing.T) {
	plain := schemaJSON(t, NewPwsh(PwshOpts{}))
	for _, key := range []string{`"command"`, `"description"`, `"timeoutMs"`, `"workdir"`} {
		if !strings.Contains(plain, key) {
			t.Fatalf("plain schema %s lacks %s", plain, key)
		}
	}
	if strings.Contains(plain, "run_in_background") {
		t.Fatalf("plain schema must not advertise run_in_background: %s", plain)
	}
	if !strings.Contains(schemaJSON(t, NewPwsh(PwshOpts{Jobs: jobs.NewLocal(jobs.LocalOpts{})})), "run_in_background") {
		t.Fatal("schema with a jobs registry must advertise run_in_background")
	}
}

// TestPwshValidation verifies the pre-spawn argument checks (the registry's
// D7 schema gate rejects the same cases before Execute).
func TestPwshValidation(t *testing.T) {
	tool := NewPwsh(PwshOpts{})
	for _, args := range []map[string]any{
		{"command": ""},
		{},
		{"command": "echo hi", "description": ""},
		{"command": "echo hi", "description": "d", "timeoutMs": -1},
	} {
		if _, err := execPwsh(t, tool, args); err == nil {
			t.Fatalf("pwsh with args %v must error", args)
		}
	}
}

// TestPwshFreshProcessNoStatePersists verifies the dsh core contract: each
// call runs in a fresh pwsh process, so environment state set by one call is
// invisible to the next.
func TestPwshFreshProcessNoStatePersists(t *testing.T) {
	requirePwsh(t)
	tool := NewPwsh(PwshOpts{})
	if _, err := execPwsh(t, tool, map[string]any{
		"command":     `$env:PA_PWSH_STATE = "leak"`,
		"description": "set a variable",
	}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	out, err := execPwsh(t, tool, map[string]any{
		"command":     `if ($env:PA_PWSH_STATE) { $env:PA_PWSH_STATE } else { "fresh" }`,
		"description": "read the variable",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "fresh") || strings.Contains(out, "leak") {
		t.Fatalf("output = %q, want a fresh process with no carried state", out)
	}
}

// sameDir reports whether got and want name the same directory. PowerShell's
// Get-Location normalizes paths to their 8.3 short form under the user
// profile, so string equality would flake; os.SameFile compares the real
// files.
func sameDir(t *testing.T, got, want string) bool {
	t.Helper()
	gi, err1 := os.Stat(got)
	wi, err2 := os.Stat(want)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(gi, wi)
}

// TestPwshWorkdir verifies workdir resolution: an explicit absolute path is
// used verbatim, a relative one is resolved against the default working
// directory, and an absent one falls back to the default.
func TestPwshWorkdir(t *testing.T) {
	requirePwsh(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tool := NewPwsh(PwshOpts{Workdir: dir})
	out, err := execPwsh(t, tool, map[string]any{
		"command":     `(Get-Location).Path`,
		"description": "show cwd",
		"workdir":     sub,
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !sameDir(t, strings.TrimSpace(out), sub) {
		t.Fatalf("absolute workdir output = %q, want %q", out, sub)
	}
	out, err = execPwsh(t, tool, map[string]any{
		"command":     `(Get-Location).Path`,
		"description": "show cwd",
		"workdir":     "sub",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !sameDir(t, strings.TrimSpace(out), sub) {
		t.Fatalf("relative workdir output = %q, want %q", out, sub)
	}
	out, err = execPwsh(t, tool, map[string]any{
		"command":     `(Get-Location).Path`,
		"description": "show cwd",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !sameDir(t, strings.TrimSpace(out), dir) {
		t.Fatalf("default workdir output = %q, want %q", out, dir)
	}
}

// TestPwshExitCodeMarker verifies a non-zero exit is a NORMAL result carrying
// the [exit code: N] marker (never an error — the model decides how to
// react).
func TestPwshExitCodeMarker(t *testing.T) {
	requirePwsh(t)
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     "exit 3",
		"description": "fail with code 3",
	})
	if err != nil {
		t.Fatalf("non-zero exit must not be a hard error: %v", err)
	}
	if !strings.Contains(out, "[exit code: 3]") {
		t.Fatalf("output = %q, want the exit-code marker", out)
	}
	if !strings.Contains(out, "(no output)") {
		t.Fatalf("output = %q, want the (no output) body", out)
	}
}

// TestPwshStderrSection verifies stderr is rendered as a marked section after
// stdout (dsh render).
func TestPwshStderrSection(t *testing.T) {
	requirePwsh(t)
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     "Write-Output out-text; Write-Error err-text",
		"description": "write both streams",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "out-text") || !strings.Contains(out, "[stderr]") || !strings.Contains(out, "err-text") {
		t.Fatalf("output = %q, want stdout plus a [stderr] section", out)
	}
}

// TestPwshNoOutput verifies the (no output) body for an empty result.
func TestPwshNoOutput(t *testing.T) {
	requirePwsh(t)
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     `Write-Output $null`,
		"description": "print nothing",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "(no output)") {
		t.Fatalf("output = %q, want (no output)", out)
	}
}

// TestPwshUTF8Output verifies the encoding preamble pins UTF-8 output on any
// PowerShell generation (5.1 writes the OEM code page without it).
func TestPwshUTF8Output(t *testing.T) {
	requirePwsh(t)
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     `Write-Output "中文输出测试"`,
		"description": "print UTF-8 text",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "中文输出测试") {
		t.Fatalf("output = %q, want the UTF-8 text intact", out)
	}
}

// TestPwshScrubbedEnvAndOverrides verifies credential-shaped parent
// environment entries are never inherited, while the model-friendly
// overrides are present.
func TestPwshScrubbedEnvAndOverrides(t *testing.T) {
	requirePwsh(t)
	t.Setenv("PA_TEST_SECRET_TOKEN", "leakme-123")
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     `Write-Output "seen=$env:PA_TEST_SECRET_TOKEN"`,
		"description": "read a secret-shaped variable",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if strings.Contains(out, "leakme-123") {
		t.Fatalf("secret leaked into child output: %q", out)
	}
	out, err = execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     `Write-Output "nocolor=$env:NO_COLOR"`,
		"description": "read the NO_COLOR override",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "nocolor=1") {
		t.Fatalf("output = %q, want the NO_COLOR override applied", out)
	}
}

// TestPwshTimeout verifies the model timeoutMs kills the process and reports
// [timed out after Nms] as a NORMAL result, promptly.
func TestPwshTimeout(t *testing.T) {
	requirePwsh(t)
	start := time.Now()
	out, err := execPwsh(t, NewPwsh(PwshOpts{}), map[string]any{
		"command":     "Start-Sleep -Seconds 10",
		"description": "sleep",
		"timeoutMs":   300,
	})
	if err != nil {
		t.Fatalf("a timeout must be a normal result, not an error: %v", err)
	}
	if !strings.Contains(out, "[timed out after 300ms]") {
		t.Fatalf("output = %q, want the timeout marker", out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want a prompt kill", elapsed)
	}
}

// TestPwshTimeoutAtRegistryBound verifies the registry's per-tool deadline is
// the outer bound and is reported with the same timeout marker (the exact
// millisecond value is the deadline remaining at spawn, so only the marker
// shape is asserted).
func TestPwshTimeoutAtRegistryBound(t *testing.T) {
	requirePwsh(t)
	r := New()
	if err := r.Register(NewPwsh(PwshOpts{})); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"pwsh"}, Timeout: 300 * time.Millisecond})
	start := time.Now()
	res, err := r.Execute(context.Background(), "pwsh", json.RawMessage(`{"command":"Start-Sleep -Seconds 10","description":"sleep"}`))
	if err != nil {
		t.Fatalf("a registry-bound timeout must be a normal result, not an error: %v", err)
	}
	if !res.IsError || res.Error == nil || res.Error.Code != "TOOL_TIMEOUT" {
		t.Fatalf("result = %+v, want structured timeout", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want a prompt kill", elapsed)
	}
}

// TestPwshCancelled interrupts an executing command via context cancellation
// (the user-stop path): the process tree is killed and Execute returns an
// interruption error promptly.
func TestPwshCancelled(t *testing.T) {
	requirePwsh(t)
	tool := NewPwsh(PwshOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	b, err := json.Marshal(map[string]any{
		"command":     "Start-Sleep -Seconds 30",
		"description": "sleep",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var execErr error
	done := make(chan struct{})
	go func() {
		_, execErr = tool.Execute(ctx, b)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond) // let the command start
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executing command was not interrupted by cancellation")
	}
	if execErr == nil || !strings.Contains(execErr.Error(), "interrupted") {
		t.Fatalf("err = %v, want an interruption error", execErr)
	}
}

// TestPwshBackgroundJob verifies run_in_background registers a jobs.Registry
// job (kind pwsh): the acknowledgement carries the job id, job_output observes
// completion, and job_kill kills a
// live job.
func TestPwshBackgroundJob(t *testing.T) {
	requirePwsh(t)
	reg := jobs.NewLocal(jobs.LocalOpts{})
	defer reg.Close()
	tool := NewPwsh(PwshOpts{Jobs: reg, Owner: func() string { return "s-1" }})

	out, err := execPwsh(t, tool, map[string]any{
		"command":           "Write-Output bg-ok",
		"description":       "background echo",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "started background job pwsh-1") {
		t.Fatalf("ack = %q, want the job id", out)
	}
	snap, err := reg.Wait(context.Background(), "pwsh-1", "s-1", 10*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if snap.Status != jobs.StatusCompleted {
		t.Fatalf("status = %s, want completed", snap.Status)
	}
	got, _, err := reg.Read(context.Background(), "pwsh-1", "s-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, "bg-ok") {
		t.Fatalf("job output = %q, want the echo output", got)
	}

	out, err = execPwsh(t, tool, map[string]any{
		"command":           "Start-Sleep -Seconds 30",
		"description":       "background sleep",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "pwsh-2") {
		t.Fatalf("ack = %q, want the second job id", out)
	}
	if _, err := reg.Kill(context.Background(), "pwsh-2", "s-1", "test kill"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	snap, err = reg.Wait(context.Background(), "pwsh-2", "s-1", 10*time.Second)
	if err != nil {
		t.Fatalf("wait after kill: %v", err)
	}
	if snap.Status != jobs.StatusKilled {
		t.Fatalf("status = %s, want killed", snap.Status)
	}
}

// TestPwshBackgroundUnavailable verifies run_in_background is rejected (and
// not advertised) without a jobs registry.
func TestPwshBackgroundUnavailable(t *testing.T) {
	tool := NewPwsh(PwshOpts{})
	if _, err := execPwsh(t, tool, map[string]any{
		"command":           "Write-Output x",
		"description":       "bg",
		"run_in_background": true,
	}); err == nil || !strings.Contains(err.Error(), "run_in_background") {
		t.Fatalf("err = %v, want the run_in_background rejection", err)
	}
}

// TestPwshRegisteredAndExecutes verifies the composed gate: once registered
// and whitelisted, pwsh executes through the registry like any tool.
func TestPwshRegisteredAndExecutes(t *testing.T) {
	requirePwsh(t)
	r := New()
	if err := r.Register(NewPwsh(PwshOpts{})); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"pwsh"}, Timeout: time.Minute})
	res, err := r.Execute(context.Background(), "pwsh", json.RawMessage(`{"command":"Write-Output hi","description":"say hi"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("output = %q, want the echo output", res.Output)
	}
}
