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

	res, err := p.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: utf8Command(), Cwd: testCwd(t)})
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

	res, err := p.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: "echo hello", Cwd: testCwd(t)})
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

	res, err := p.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: failCommand(), Cwd: testCwd(t)})
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
		Mode:    SandboxFullAccess,
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
	// Windows timer/process startup jitter can make the measured child lifetime
	// slightly shorter than the requested deadline. TimedOut plus the outer
	// prompt-return bound above are the semantic contract; do not turn scheduler
	// jitter into a flaky lower-bound assertion.
	if res.Duration <= 0 {
		t.Fatalf("Duration = %v, want a positive run duration", res.Duration)
	}
}

// TestRunTruncated covers the output quota: both streams are capped at
// MaxOutput bytes and Truncated is set.
func TestRunTruncated(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()

	const maxOut = 1024
	res, err := p.Run(context.Background(), RunRequest{
		Mode:      SandboxFullAccess,
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
		Mode: SandboxFullAccess,
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

	res, err := p.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: printCwdCommand()})
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

func TestLocalProviderCloseStopsActiveProcess(t *testing.T) {
	p := NewLocalProvider()
	local := p.(*localProvider)
	result := make(chan error, 1)
	go func() {
		_, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxFullAccess, Code: longSleep(), Cwd: testCwd(t), Timeout: 30 * time.Second,
		})
		result <- err
	}()

	// Wait for the child to cross the provider's active publication barrier.
	// A fixed sleep is flaky under a loaded test process: Close can otherwise
	// legitimately win before Run publishes the command and the test would
	// assert the wrong lifecycle branch.
	deadline := time.Now().Add(5 * time.Second)
	for {
		local.mu.Lock()
		active := len(local.active)
		local.mu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not cross the active publication barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not quiesce the active process")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("closed process run returned transport error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active process run did not settle after Close")
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

	res, err := e.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: "echo hello", Cwd: testCwd(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello" {
		t.Fatalf("Stdout = %q, want %q", got, "hello")
	}
}

func TestEngineDefaultWorkspaceModeFailsClosedWithoutEnforcingBackend(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	cap := e.prov.(capabilityReporter).Capabilities()
	if cap.StrongIsolation {
		t.Skip("host provides the enforcing workspace backend")
	}
	if _, err := e.Run(context.Background(), RunRequest{Code: "echo must-not-run", Cwd: testCwd(t)}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("default workspace run error = %v, want ErrSandboxUnavailable", err)
	}
}

// TestEngineWithProvider covers the Engine seam delegating to an explicit
// Provider.
func TestEngineWithProvider(t *testing.T) {
	e := NewEngine(NewLocalProvider())
	defer e.Close()

	res, err := e.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: "echo hi", Cwd: testCwd(t)})
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

func TestEngineFailsClosedWhenIsolationIsRequired(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	cap := e.prov.(capabilityReporter).Capabilities()
	if cap.StrongIsolation {
		if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", RequireStrongIsolation: true}); err != nil {
			t.Fatalf("strong isolation with advertised capability: %v", err)
		}
	} else if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", RequireStrongIsolation: true}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("strong isolation error = %v, want ErrSandboxUnavailable", err)
	}
	if cap.NetworkIsolation {
		if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", RequireNetworkIsolation: true}); err != nil {
			t.Fatalf("network isolation with advertised capability: %v", err)
		}
	} else if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", RequireNetworkIsolation: true}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("network isolation error = %v, want ErrSandboxUnavailable", err)
	}
	if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", AllowNetwork: true}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("network access error = %v, want ErrSandboxUnavailable", err)
	}
	if cap.StrongIsolation {
		root := t.TempDir()
		cwd := filepath.Join(root, "cwd")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", Mode: SandboxReadOnly, Root: root, Cwd: cwd}); err != nil {
			t.Fatalf("readonly mode with advertised capability: %v", err)
		}
	} else if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", Mode: SandboxReadOnly}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("readonly mode error = %v, want ErrSandboxUnavailable", err)
	}
	if _, err := e.Run(context.Background(), RunRequest{Code: "echo hi", Mode: SandboxFullAccess}); err != nil {
		t.Fatalf("explicit full-access mode: %v", err)
	}
}

func TestBwrapNetworkIsolationWhenAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces are a Linux backend")
	}
	p := NewLocalProvider()
	defer p.Close()
	cap := p.(capabilityReporter).Capabilities()
	if !cap.NetworkIsolation {
		t.Skip("no usable bubblewrap network namespace on this host")
	}
	result, err := p.Run(context.Background(), RunRequest{
		Mode:                    SandboxWorkspaceWrite,
		RequireNetworkIsolation: true,
		Cwd:                     t.TempDir(),
		Code:                    "test ! -e /sys/class/net/eth0 && test ! -e /sys/class/net/enp0s3",
	})
	if err != nil {
		t.Fatalf("network-isolated run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("network namespace did not hide host interfaces: %+v", result)
	}
}

func TestSeatbeltProfileMatchesFileEffectPolicy(t *testing.T) {
	readOnly := seatbeltProfile(SandboxReadOnly, "/workspace")
	if !strings.Contains(readOnly, "(deny file-write*)") {
		t.Fatalf("read-only Seatbelt profile = %q, missing write deny", readOnly)
	}
	if strings.Contains(readOnly, `(subpath "/workspace")`) {
		t.Fatalf("read-only Seatbelt profile granted workspace writes: %q", readOnly)
	}

	workspaceRoot := filepath.Clean(`/workspace/with "quote`)
	workspace := seatbeltProfile(SandboxWorkspaceWrite, workspaceRoot)
	if !strings.Contains(workspace, `(subpath `+sbplString(workspaceRoot)+`)`) {
		t.Fatalf("workspace-write Seatbelt profile did not quote workspace root: %q", workspace)
	}
	if !strings.Contains(workspace, `(subpath `+sbplString(filepath.Clean("/tmp"))+`)`) {
		t.Fatalf("workspace-write Seatbelt profile omitted /tmp: %q", workspace)
	}

	argv := seatbeltCommand("sandbox-exec", "/workspace", SandboxReadOnly, []string{"/bin/sh", "-c", "echo ok"})
	wantPrefix := []string{"sandbox-exec", "-p"}
	if len(argv) < len(wantPrefix)+2 || argv[0] != wantPrefix[0] || argv[1] != wantPrefix[1] || argv[len(argv)-3] != "/bin/sh" {
		t.Fatalf("Seatbelt command = %#v", argv)
	}
}

func TestSeatbeltEnforcesReadOnlyWhenAvailable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is a macOS backend")
	}
	p := NewLocalProvider()
	defer p.Close()
	provider := p.(*localProvider)
	if provider.seatbelt == "" {
		t.Skip("no usable sandbox-exec backend on this host")
	}
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside.txt")
	t.Cleanup(func() { _ = os.Remove(outside) })
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly,
		Root: root,
		Cwd:  root,
		Code: "echo denied > " + shellQuote(outside),
	})
	if err != nil {
		t.Fatalf("Seatbelt read-only run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("read-only command unexpectedly succeeded: %+v", result)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only command created %s: %v", outside, err)
	}
}

func TestLocalProviderFullAccessIsExplicit(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxFullAccess,
		Root: root,
		Cwd:  cwd,
		Code: "echo allowed",
	})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "allowed" {
		t.Fatalf("full-access run = %+v, err=%v", result, err)
	}
}

func TestReadOnlyModeAcceptsCanonicalAndLegacySpellings(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()
	if runtime.GOOS != "linux" || !p.(capabilityReporter).Capabilities().StrongIsolation {
		t.Skip("canonical read-only execution requires the enforcing Linux backend")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []SandboxMode{SandboxReadOnly, SandboxReadOnlyLegacy} {
		result, err := p.Run(context.Background(), RunRequest{Mode: mode, Root: root, Cwd: cwd, Code: "echo ok"})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("mode %q result=%+v err=%v", mode, result, err)
		}
	}
}

func TestBwrapEnforcesReadOnlyWhenAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is a Linux backend")
	}
	p := NewLocalProvider()
	defer p.Close()
	cap := p.(capabilityReporter).Capabilities()
	if !cap.StrongIsolation {
		t.Skip("no usable bubblewrap/user namespace on this host")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	res, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly,
		Root: root,
		Cwd:  cwd,
		Code: "echo denied > " + shellQuote(outside),
	})
	if err != nil {
		t.Fatalf("read-only run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("read-only command unexpectedly succeeded: %+v", res)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only command created %s: %v", outside, err)
	}
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func TestLocalProviderRejectsCwdOutsidePolicyRootBeforeCreatingIt(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()
	root := t.TempDir()
	escaping := filepath.Join(t.TempDir(), "outside")
	if _, err := p.Run(context.Background(), RunRequest{Mode: SandboxFullAccess, Code: "echo no", Root: root, Cwd: escaping}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("escape error = %v, want ErrInvalidRequest", err)
	}
	if _, err := os.Stat(escaping); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaping cwd was created: %v", err)
	}
}

func TestLocalProviderRejectsSymlinkedCwdOutsidePolicyRoot(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxFullAccess, Code: "echo no", Root: root, Cwd: link,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("symlink escape error = %v, want ErrInvalidRequest", err)
	}
}

func TestLocalProviderRejectsMissingCwdBelowEscapingSymlink(t *testing.T) {
	p := NewLocalProvider()
	defer p.Close()
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	escaping := filepath.Join(link, "not-yet-created")
	if _, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxFullAccess, Code: "echo no", Root: root, Cwd: escaping,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing symlink escape error = %v, want ErrInvalidRequest", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "not-yet-created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaping cwd was created through symlink: %v", err)
	}
}

func TestControlledShellEnforcesHardFileLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hard limits are not the Windows enforcement backend")
	}
	p := newLocalProvider()
	defer p.Close()
	if !hasSandboxMode(p.Capabilities(), SandboxWorkspaceWrite) {
		t.Skip("no enforcing controlled-shell backend on this host")
	}
	cwd := t.TempDir()
	result, err := p.Run(context.Background(), RunRequest{
		Mode:             SandboxWorkspaceWrite,
		Root:             cwd,
		Cwd:              cwd,
		Code:             "ulimit -f unlimited 2>/dev/null; dd if=/dev/zero of=resource-limit.bin bs=8192 count=1 >/dev/null 2>&1",
		MaxMemoryBytes:   64 * 1024 * 1024,
		MaxFileSizeBytes: 4096,
		MaxProcesses:     128,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("file-size-limited command unexpectedly succeeded: %+v", result)
	}
	info, statErr := os.Stat(filepath.Join(cwd, "resource-limit.bin"))
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat limited file: %v", statErr)
	}
	if statErr == nil && info.Size() > 4096 {
		t.Fatalf("file-size limit was not enforced: size=%d", info.Size())
	}
}

func TestControlledShellEnforcesHardMemoryLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hard limits are not the Windows enforcement backend")
	}
	p := newLocalProvider()
	defer p.Close()
	if !hasSandboxMode(p.Capabilities(), SandboxWorkspaceWrite) {
		t.Skip("no enforcing controlled-shell backend on this host")
	}
	cwd := t.TempDir()
	result, err := p.Run(context.Background(), RunRequest{
		Mode:           SandboxWorkspaceWrite,
		Root:           cwd,
		Cwd:            cwd,
		Code:           `dd if=/dev/zero of=/dev/null bs=64M count=1 >/dev/null 2>&1`,
		MaxMemoryBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("memory-limited command unexpectedly succeeded: %+v", result)
	}
}

func TestControlledShellBlocksBoundedForkBomb(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process ceilings are not the Windows enforcement backend")
	}
	p := newLocalProvider()
	defer p.Close()
	if !hasSandboxMode(p.Capabilities(), SandboxWorkspaceWrite) {
		t.Skip("no enforcing controlled-shell backend on this host")
	}
	cwd := t.TempDir()
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxWorkspaceWrite,
		Root: cwd,
		Cwd:  cwd,
		Code: `
			recurse() {
				if [ "$1" -le 0 ]; then sleep 5; return; fi
				recurse "$(("$1" - 1))" & recurse "$(("$1" - 1))" &
				wait
			}
			recurse 4
			echo must-not-run
		`,
		MaxMemoryBytes:   64 * 1024 * 1024,
		MaxFileSizeBytes: 64 * 1024 * 1024,
		MaxProcesses:     2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 || strings.Contains(result.Stdout, "must-not-run") {
		t.Fatalf("process-limited fork fixture was not fail closed: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(cwd, "cwd")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat cwd: %v", err)
	}
}

func hasSandboxMode(capabilities SandboxCapabilities, want SandboxMode) bool {
	for _, mode := range capabilities.SupportedModes {
		if mode == want {
			return true
		}
	}
	return false
}

func TestScrubEnvRemovesCredentialShapedNamesOnly(t *testing.T) {
	got := scrubEnv([]string{
		"PATH=/usr/bin",
		"DSH_API_KEY=secret",
		"access_token=secret",
		"MY_PASSWORD=secret",
		"LANG=C",
		"MALFORMED",
	})
	if strings.Join(got, "\x00") != "PATH=/usr/bin\x00LANG=C" {
		t.Fatalf("scrubbed env = %#v, want only non-credential entries", got)
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
