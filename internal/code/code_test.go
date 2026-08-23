package code

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunUTF8Output preserves Unicode output across the subprocess boundary.
func TestRunUTF8Output(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	res, err := p.Run(context.Background(), RunRequest{Code: utf8Command(), Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "你好，世界" {
		t.Fatalf("Stdout = %q, want UTF-8 Chinese text", got)
	}
}

// TestRunSuccess covers the happy path: a command that exits 0 and prints to
// stdout, with no timeout or truncation markers and a positive duration.
func TestRunSuccess(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	res, err := p.Run(context.Background(), RunRequest{Code: "echo hello", Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello" {
		t.Fatalf("Stdout = %q, want %q", got, "hello")
	}
	if res.Stderr != "" {
		t.Fatalf("Stderr = %q, want empty", res.Stderr)
	}
	if res.TimedOut || res.Truncated {
		t.Fatalf("TimedOut=%v Truncated=%v, want false/false", res.TimedOut, res.Truncated)
	}
	if res.Duration <= 0 {
		t.Fatalf("Duration = %v, want > 0", res.Duration)
	}
}

// TestRunFailure covers a non-zero exit: the exit code and stderr are reported
// as a normal Result (nil error).
func TestRunFailure(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	res, err := p.Run(context.Background(), RunRequest{Code: failCommand(), Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
	if got := strings.TrimSpace(res.Stderr); !strings.Contains(got, "oops") {
		t.Fatalf("Stderr = %q, want to contain %q", got, "oops")
	}
	if res.TimedOut {
		t.Fatalf("TimedOut = true, want false")
	}
}

// TestRunTimeout covers the hard-kill path: a run that outlives the timeout is
// killed promptly, marked TimedOut, and returns as a normal Result. The
// elapsed bound proves the direct child was actually killed (Wait did not
// block on a lingering grandchild).
func TestRunTimeout(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	start := time.Now()
	res, err := p.Run(context.Background(), RunRequest{
		Code:    longSleep(),
		Cwd:     testCwd(t),
		Timeout: 300 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false, want true (stdout=%q stderr=%q)", res.Stdout, res.Stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run did not return promptly after the hard kill: %v", elapsed)
	}
	if res.Duration < 200*time.Millisecond {
		t.Fatalf("Duration = %v, want ~timeout (300ms)", res.Duration)
	}
}

// TestRunTruncated covers the output quota: both streams are capped at
// MaxOutput bytes and Truncated is set.
func TestRunTruncated(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	const maxOut = 1024
	res, err := p.Run(context.Background(), RunRequest{
		Code:      bigOutput(70000),
		Cwd:       testCwd(t),
		MaxOutput: maxOut,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(res.Stdout) != maxOut {
		t.Fatalf("len(Stdout) = %d, want %d", len(res.Stdout), maxOut)
	}
	if len(res.Stderr) != maxOut {
		t.Fatalf("len(Stderr) = %d, want %d", len(res.Stderr), maxOut)
	}
}

// TestRunCwd covers the sandbox cwd: an explicit (not yet existing) working
// directory is created and honored — the command's pwd lands in it.
func TestRunCwd(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	cwd := testCwd(t)
	res, err := p.Run(context.Background(), RunRequest{
		Code: printCwdCommand(),
		Cwd:  cwd,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	got := strings.TrimSpace(res.Stdout)
	if !samePath(got, cwd) {
		t.Fatalf("cwd = %q, want %q", got, cwd)
	}
}

// TestRunDefaultCwd covers the default sandbox cwd (<cwd base>/.sandbox): it
// is created on demand and the command's pwd lands in it. The process cwd is
// pointed at a fresh temp dir so the default sandbox is created there rather
// than inside the repo, and restored afterward.
func TestRunDefaultCwd(t *testing.T) {
	base := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	want := filepath.Join(base, sandboxDirName)
	// Remove the sandbox dir before the framework removes its parent; retry
	// because a just-exited child can briefly hold the dir handle on Windows.
	t.Cleanup(func() {
		for i := 0; i < 5; i++ {
			if err := os.RemoveAll(want); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("cleanup: could not remove %s", want)
	})

	p := NewLocalProvider()
	defer p.Close()

	res, err := p.Run(context.Background(), RunRequest{Code: printCwdCommand()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stdout=%q)", res.ExitCode, res.Stdout)
	}
	got := strings.TrimSpace(res.Stdout)
	if !samePath(got, want) {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("default sandbox dir was not created: %v", err)
	}
}

// TestCloseIdempotent covers the seam's close contract: Close is idempotent
// and rejects further Runs with ErrProviderClosed.
func TestCloseIdempotent(t *testing.T) {
	p := NewLocalProvider()
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.Run(context.Background(), RunRequest{Code: "echo hi"}); !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("Run after Close: err = %v, want ErrProviderClosed", err)
	}
}

// TestRunUnsupportedLang covers validation: only "sh" is supported in v1.
func TestRunUnsupportedLang(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	if _, err := p.Run(context.Background(), RunRequest{Lang: "python", Code: "print(1)"}); !errors.Is(err, ErrUnsupportedLang) {
		t.Fatalf("err = %v, want ErrUnsupportedLang", err)
	}
}

// TestRunEmptyCode covers validation: a blank command is rejected.
func TestRunEmptyCode(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	if _, err := p.Run(context.Background(), RunRequest{Code: "  "}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// TestEngineDefaultProvider covers the Engine seam with no explicit Provider:
// the local subprocess sandbox is selected by default.
func TestEngineDefaultProvider(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	res, err := e.Run(context.Background(), RunRequest{Code: "echo hello", Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello" {
		t.Fatalf("Stdout = %q, want %q", got, "hello")
	}
}

// TestEngineWithProvider covers the Engine seam delegating to an explicit
// Provider.
func TestEngineWithProvider(t *testing.T) {
	e := NewEngine(NewLocalProvider())
	defer e.Close()

	res, err := e.Run(context.Background(), RunRequest{Code: "echo hi", Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hi" {
		t.Fatalf("Stdout = %q, want %q", got, "hi")
	}
}

// TestEngineClosed covers the Engine close contract: Close is idempotent and
// further Runs are rejected with ErrEngineClosed.
func TestEngineClosed(t *testing.T) {
	e := NewEngine(nil)
	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi"}); !errors.Is(err, ErrEngineClosed) {
		t.Fatalf("Run after Close: err = %v, want ErrEngineClosed", err)
	}
}

// testCwd returns an isolated sandbox cwd under a fresh temp dir (created on
// demand by the provider). Tests use an explicit Cwd so the default sandbox
// dir (<cwd base>/.sandbox) is not created inside the repo — only
// TestRunDefaultCwd exercises the default path, under a temp process cwd.
func testCwd(t *testing.T) string {
	return filepath.Join(t.TempDir(), "sandbox")
}

func utf8Command() string {
	if runtime.GOOS == "windows" {
		return "echo 你好，世界"
	}
	return "printf '你好，世界\\n'"
}

// failCommand returns a one-line command that exits 3 with "oops" on stderr.
func failCommand() string {
	if runtime.GOOS == "windows" {
		return "echo oops 1>&2 & exit 3"
	}
	return "echo oops 1>&2; exit 3"
}

// printCwdCommand returns a one-line command that prints the current working
// directory.
func printCwdCommand() string {
	if runtime.GOOS == "windows" {
		return "cd"
	}
	return "pwd"
}

// longSleep returns a command that blocks far longer than any test timeout.
// The Windows branch is a cmd-internal loop (no grandchild process), so
// killing the direct child leaves nothing lingering to hold the sandbox cwd
// and the test framework can clean up its temp dir.
func longSleep() string {
	if runtime.GOOS == "windows" {
		return "for /L %i in (1,1,1000000000) do @rem"
	}
	return "sleep 30"
}

// bigOutput returns a command that writes big bytes to stdout and the same to
// stderr (70k each for the default test). The Windows branch deliberately
// carries no embedded double quotes: exec.Cmd re-quotes argv for cmd /C, and
// a code string containing double quotes makes cmd echo instead of execute
// (the documented single-command-line boundary — see the package comment).
func bigOutput(big int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(
			"powershell -NoProfile -NonInteractive -Command ('x'*%d);[Console]::Error.Write('e'*%d)",
			big, big)
	}
	return fmt.Sprintf("yes x | head -c %d; yes e | head -c %d 1>&2", big, big)
}

// samePath compares two filesystem paths, ignoring case on Windows.
func samePath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}
