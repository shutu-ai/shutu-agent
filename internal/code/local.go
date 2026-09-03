package code

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/pathsecure"
)

// Defaults applied when the matching RunRequest field is zero.
const (
	// defaultTimeout bounds a run when RunRequest.Timeout is zero (30s).
	defaultTimeout = 30 * time.Second
	// defaultMaxOutput is the per-stream output cap when RunRequest.MaxOutput
	// is zero (64KiB).
	defaultMaxOutput = 64 * 1024
	// Controlled shell defaults are deliberately finite and inherited by the
	// complete child tree. They are a containment floor, not a claim that the
	// host has a globally isolated cgroup.
	defaultMaxMemoryBytes   = 512 * 1024 * 1024
	defaultMaxFileSizeBytes = 64 * 1024 * 1024
	defaultMaxProcesses     = 256
	// langSh is the only supported language; an empty Lang defaults to it.
	langSh = "sh"
	// sandboxDirName is the default sandbox subdirectory under the cwd base
	// when RunRequest.Cwd is empty.
	sandboxDirName = ".sandbox"
)

// sensitiveEnvTokens are the credential-shaped substrings removed from the
// environment before every sandbox run (mirrors internal/tools/run_command.go):
// the child process must never implicitly inherit the agent's keys, secrets,
// tokens, passwords, or API credentials. This is the concrete v1 form of the
// "default no network" boundary — no network credentials are injected.
var sensitiveEnvTokens = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}

// localProvider is the default sandbox backend (ADR 决策 M6e): a local
// subprocess sandbox built on os/exec + context, with no third-party
// dependencies. It enforces the controlled-isolation semantics documented in
// the package comment: a separate child process, a hard-kill timeout, per-
// stream output quotas, and an isolated sandbox cwd created on demand. Output
// is continuously drained from pipes into bounded captures so a noisy child
// cannot block on a full pipe or materialize unbounded output on disk.
type localProvider struct {
	mu        sync.Mutex
	closed    bool
	active    map[*exec.Cmd]chan struct{}
	closeDone chan struct{}
	// bwrap is set only after a bounded functional probe succeeds. A path
	// found by LookPath alone is not sufficient: user namespaces may be
	// disabled even when the executable is installed.
	bwrap string
	// bwrapNetwork is true only when the backend also passed a network
	// namespace probe. A filesystem sandbox without this bit must not claim
	// that it enforces the no-network policy.
	bwrapNetwork bool
	// prlimit is the Linux util-linux executor that installs hard rlimits
	// before exec. It is required for controlled bwrap modes so memory,
	// file-size, and process ceilings are kernel-enforced rather than left to
	// shell-builtin behavior.
	prlimit string
	// seatbelt is the macOS file-effect backend. It intentionally does not
	// advertise network isolation: the profile below mirrors DSH's file-only
	// sandbox seam and leaves that policy unsupported on this backend.
	seatbelt string
	// windowsACL is set only after a real restricted-token/file-write probe.
	windowsACL bool
	// aclWorkspaceLocks serializes workspaces that mutate ACLs. Windows ACL
	// state is transactional per path; concurrent runs on the same workspace
	// would otherwise race grant/cleanup.
	aclWorkspaceLocks   map[string]*sync.Mutex
	aclWorkspaceLocksMu sync.Mutex
	// diagnosticArgv is a test-only top-level argv override. It keeps native
	// Windows API regressions independent of cmd.exe/PowerShell spawning.
	diagnosticArgv []string
}

// boundedCapture drains a subprocess stream while retaining only its prefix.
// Write must report the full input length even after the quota is reached: the
// reader must keep draining so the child cannot deadlock on a full pipe.
type boundedCapture struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	if c.limit <= c.Len() {
		c.truncated = true
		return len(p), nil
	}
	remaining := c.limit - c.Len()
	if len(p) > remaining {
		if _, err := c.Buffer.Write(p[:remaining]); err != nil {
			return 0, err
		}
		c.truncated = true
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// ReadFrom prevents io.Copy from selecting bytes.Buffer.ReadFrom, which would
// bypass Write and defeat the quota. It deliberately keeps reading after the
// quota is reached so the subprocess remains unblocked.
func (c *boundedCapture) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			written, writeErr := c.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

// NewLocalProvider returns the default local subprocess sandbox Provider.
func NewLocalProvider() Provider { return newLocalProvider() }

func newLocalProvider() *localProvider {
	bwrap, network := detectBwrap()
	prlimit := ""
	if bwrap != "" {
		if found, err := exec.LookPath("prlimit"); err == nil {
			prlimit = found
		} else {
			// A file-only bwrap backend is useful for accidental-write
			// containment, but A3.1's controlled-shell contract also requires
			// resource ceilings. Do not advertise a partial backend as strong.
			bwrap, network = "", false
		}
	}
	seatbelt := ""
	if bwrap == "" {
		seatbelt = detectSeatbelt()
	}
	return &localProvider{
		bwrap: bwrap, bwrapNetwork: network, prlimit: prlimit, seatbelt: seatbelt,
		windowsACL:        windowsACLBackendAvailable(),
		active:            make(map[*exec.Cmd]chan struct{}),
		aclWorkspaceLocks: make(map[string]*sync.Mutex),
		closeDone:         make(chan struct{}),
	}
}

func (p *localProvider) aclWorkspaceLock(path string) *sync.Mutex {
	p.aclWorkspaceLocksMu.Lock()
	defer p.aclWorkspaceLocksMu.Unlock()
	if p.aclWorkspaceLocks == nil {
		p.aclWorkspaceLocks = make(map[string]*sync.Mutex)
	}
	if lock := p.aclWorkspaceLocks[path]; lock != nil {
		return lock
	}
	lock := new(sync.Mutex)
	p.aclWorkspaceLocks[path] = lock
	return lock
}

// Name identifies the provider in the registry ("local").
func (p *localProvider) Name() string { return "local" }

// Capabilities is deliberately honest. On Linux, bwrap is advertised only
// after a functional probe; on macOS, sandbox-exec and on Windows the ACL
// restricted-token path are advertised only after their real file-effect
// probes pass. These supply the file-effect boundary for read-only/workspace-
// write modes. Other hosts retain the historical local process boundary and
// must fail closed for stronger requirements.
func (p *localProvider) Capabilities() SandboxCapabilities {
	// danger-full-access is an explicit escape hatch, not the default. It is
	// still a separate advertised mode so the engine can distinguish a caller
	// that deliberately requested host authority from an accidental widening
	// of the workspace-write policy.
	// A plain subprocess is not an enforcing workspace sandbox. Keep the
	// provider available for an explicit danger-full-access request, but do not
	// advertise the default workspace-write mode without a proven backend.
	cap := SandboxCapabilities{
		Available: true, Backend: "none", IsolationLevel: IsolationNone,
		SupportedModes: []SandboxMode{SandboxFullAccess},
	}
	switch {
	case p.bwrap != "":
		cap.Backend = "bubblewrap"
		cap.IsolationLevel = IsolationStrong
		cap.StrongIsolation = p.prlimit != ""
		cap.SupportedModes = []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxFullAccess}
		cap.NetworkIsolation = p.bwrapNetwork
	case p.seatbelt != "":
		cap.Backend = "seatbelt"
		cap.IsolationLevel = IsolationContainment
		cap.SupportedModes = []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxFullAccess}
	case p.windowsACL:
		cap.Backend = "windows-acl"
		cap.IsolationLevel = IsolationContainment
		cap.SupportedModes = []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxFullAccess}
	}
	return cap
}

// Run executes req as a child process under the controlled-isolation
// semantics documented in the package comment. Defaults are applied first
// (Lang "sh", 30s timeout, 64KiB per-stream cap, <cwd base>/.sandbox dir).
// A non-zero exit code and a timeout are normal outcomes returned as
// (Result, nil); the error return signals the run did not happen.
func (p *localProvider) Run(ctx context.Context, req RunRequest) (Result, error) {
	if err := p.checkOpen(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	lang := req.Lang
	if lang == "" {
		lang = langSh
	}
	if lang != langSh {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedLang, req.Lang)
	}
	if strings.TrimSpace(req.Code) == "" {
		return Result{}, fmt.Errorf("%w: empty code", ErrInvalidRequest)
	}
	mode := normalizeSandboxMode(req.Mode)
	if mode == "" {
		mode = SandboxWorkspaceWrite
	}
	if mode != SandboxReadOnly && mode != SandboxWorkspaceWrite && mode != SandboxFullAccess {
		return Result{}, fmt.Errorf("%w: local provider cannot enforce sandbox mode %q", ErrSandboxUnavailable, mode)
	}
	if req.RequireStrongIsolation && (!p.Capabilities().StrongIsolation || mode == SandboxFullAccess) {
		return Result{}, fmt.Errorf("%w: local provider cannot enforce strong isolation for mode %q", ErrSandboxUnavailable, mode)
	}
	if req.RequireNetworkIsolation && (p.bwrap == "" || !p.bwrapNetwork || mode == SandboxFullAccess) {
		return Result{}, fmt.Errorf("%w: local provider cannot enforce network isolation for mode %q", ErrSandboxUnavailable, mode)
	}
	if mode == SandboxReadOnly && p.bwrap == "" && p.seatbelt == "" && !p.windowsACL {
		return Result{}, fmt.Errorf("%w: local provider has no enforcing read-only backend", ErrSandboxUnavailable)
	}
	if p.bwrap == "" && p.seatbelt == "" && !p.windowsACL && mode != SandboxFullAccess {
		return Result{}, fmt.Errorf("%w: local provider has no enforcing backend for mode %q", ErrSandboxUnavailable, mode)
	}
	if req.AllowNetwork && mode != SandboxFullAccess {
		return Result{}, fmt.Errorf("%w: local provider cannot enforce requested network access policy", ErrSandboxUnavailable)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}
	limits, err := resolveShellResourceLimits(req, mode)
	if err != nil {
		return Result{}, err
	}

	cwd := req.Cwd
	if cwd == "" {
		base, err := os.Getwd()
		if err != nil {
			return Result{}, fmt.Errorf("code: resolve cwd base: %w", err)
		}
		cwd = filepath.Join(base, sandboxDirName)
	}
	if req.Root != "" {
		root, err := canonicalPolicyPath(req.Root)
		if err != nil {
			return Result{}, fmt.Errorf("code: resolve sandbox root: %w", err)
		}
		absCwd, err := canonicalPolicyPath(cwd)
		if err != nil {
			return Result{}, fmt.Errorf("code: resolve sandbox cwd: %w", err)
		}
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(absCwd))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return Result{}, fmt.Errorf("%w: cwd %q escapes root %q", ErrInvalidRequest, cwd, req.Root)
		}
	}
	if mode == SandboxReadOnly {
		// Creating the working directory would itself violate the read-only
		// contract. A caller requesting readonly must point at an existing
		// directory; bubblewrap then remounts the complete visible tree RO.
		info, err := os.Stat(cwd)
		if err != nil {
			return Result{}, fmt.Errorf("%w: readonly cwd %s is unavailable: %v", ErrSandboxUnavailable, cwd, err)
		}
		if !info.IsDir() {
			return Result{}, fmt.Errorf("%w: readonly cwd %s is not a directory", ErrSandboxUnavailable, cwd)
		}
	} else if err := os.MkdirAll(cwd, 0o755); err != nil {
		return Result{}, fmt.Errorf("code: create sandbox dir %s: %w", cwd, err)
	}

	workspaceRoot := req.Root
	if workspaceRoot == "" {
		workspaceRoot = cwd
	}
	workspaceRoot, err = canonicalPolicyPath(workspaceRoot)
	if err != nil {
		return Result{}, fmt.Errorf("code: resolve sandbox workspace root: %w", err)
	}
	return p.exec(ctx, req.Code, cwd, workspaceRoot, mode, timeout, maxOut, limits)
}

type shellResourceLimits struct {
	memoryBytes   int64
	fileSizeBytes int64
	processes     int
}

func resolveShellResourceLimits(req RunRequest, mode SandboxMode) (shellResourceLimits, error) {
	if mode == SandboxFullAccess {
		return shellResourceLimits{}, nil
	}
	limits := shellResourceLimits{
		memoryBytes:   defaultMaxMemoryBytes,
		fileSizeBytes: defaultMaxFileSizeBytes,
		processes:     defaultMaxProcesses,
	}
	if req.MaxMemoryBytes != 0 {
		if req.MaxMemoryBytes < 16*1024*1024 {
			return shellResourceLimits{}, fmt.Errorf("%w: MaxMemoryBytes must be at least 16777216", ErrInvalidRequest)
		}
		limits.memoryBytes = req.MaxMemoryBytes
	}
	if req.MaxFileSizeBytes != 0 {
		if req.MaxFileSizeBytes < 4096 {
			return shellResourceLimits{}, fmt.Errorf("%w: MaxFileSizeBytes must be at least 4096", ErrInvalidRequest)
		}
		limits.fileSizeBytes = req.MaxFileSizeBytes
	}
	if req.MaxProcesses != 0 {
		if req.MaxProcesses < 1 {
			return shellResourceLimits{}, fmt.Errorf("%w: MaxProcesses must be positive", ErrInvalidRequest)
		}
		limits.processes = req.MaxProcesses
	}
	return limits, nil
}

// canonicalPolicyPath resolves the existing portion of a policy path before
// doing containment checks. A lexical Rel check is insufficient: an in-root
// symlink can otherwise point Cwd (or a future child of Cwd) outside Root.
// Missing trailing components are appended to the resolved existing parent,
// which preserves the reference sandbox contract for a not-yet-created cwd.
func canonicalPolicyPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	candidate := abs
	var suffix []string
	for {
		if _, err := os.Lstat(candidate); err == nil {
			resolved, err := pathsecure.ResolveExisting(candidate)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}

// exec runs the code as a single non-interactive shell line (cmd /C on
// Windows, /bin/sh -c elsewhere) with a scrubbed environment in the sandbox
// cwd. The run context carries the timeout; when it expires the owned process
// tree is hard-killed and TimedOut is set. Output is continuously drained and
// retained only up to the per-stream quota.
func (p *localProvider) exec(ctx context.Context, code, cwd, workspaceRoot string, mode SandboxMode, timeout time.Duration, maxOut int, limits shellResourceLimits) (Result, error) {
	argv := p.diagnosticArgv
	if argv == nil && mode != SandboxFullAccess {
		switch {
		case p.bwrap != "":
			// prlimit installs kernel rlimits before exec; bwrap and all of
			// its descendants inherit them without shell-builtin quirks.
			argv = prlimitCommand(
				p.prlimit,
				limits,
				bwrapCommand(p.bwrap, workspaceRoot, mode, p.bwrapNetwork, shellCommand(code)),
			)
		case p.seatbelt != "":
			argv = shellCommandWithLimitsArgv(
				seatbeltCommand(p.seatbelt, workspaceRoot, mode, shellCommand(code)),
				limits,
			)
		default:
			argv = shellCommandWithLimits(code, limits)
		}
	}
	if argv == nil {
		argv = shellCommand(code)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = scrubbedEnv()
	prepareProcessTree(cmd)
	var aclRun windowsACLHandle
	if p.windowsACL && mode != SandboxFullAccess {
		aclLock := p.aclWorkspaceLock(workspaceRoot)
		aclLock.Lock()
		defer aclLock.Unlock()
		var err error
		aclRun, err = prepareWindowsACLRun(mode, workspaceRoot)
		if err != nil {
			return Result{}, fmt.Errorf("code: prepare Windows ACL sandbox: %w", err)
		}
		defer func() { _ = aclRun.close() }()
		if err := aclRun.configure(cmd); err != nil {
			return Result{}, fmt.Errorf("code: configure Windows ACL sandbox: %w", err)
		}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("code: create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("code: create stderr pipe: %w", err)
	}

	var tree processTree
	var treeReady atomic.Bool
	var closeTreeOnce sync.Once
	closeTree := func() {
		closeTreeOnce.Do(func() { _ = tree.Close() })
	}
	// CommandContext normally kills only the direct process. Once the owned
	// process tree has been attached, cancellation must close that tree too.
	// Before attachment, killing the direct process is the only safe fallback.
	cmd.Cancel = func() error {
		if treeReady.Load() {
			closeTree()
			return nil
		}
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}

	stdoutCapture := &boundedCapture{limit: maxOut}
	stderrCapture := &boundedCapture{limit: maxOut}
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)
	startCapture := func() {
		go func() {
			_, copyErr := io.Copy(stdoutCapture, stdoutPipe)
			stdoutDone <- copyErr
		}()
		go func() {
			_, copyErr := io.Copy(stderrCapture, stderrPipe)
			stderrDone <- copyErr
		}()
	}

	start := time.Now()
	// Start and publication are serialized with Close. This prevents Close
	// from returning while a checked-but-not-yet-started command escapes the
	// provider's active set.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		// Close may win the narrow checkOpen -> publication race. The caller's
		// run has then been abandoned by provider lifecycle, not rejected as a
		// malformed request; settle it as a normal abort outcome. A Run started
		// after Close still returns ErrProviderClosed from checkOpen above.
		if ctx.Err() != nil {
			return Result{Duration: runDuration(start)}, ctx.Err()
		}
		return Result{Duration: runDuration(start)}, nil
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		if runCtx.Err() == context.DeadlineExceeded {
			// Under heavy host contention the setup phase can consume the full
			// budget before CreateProcess succeeds. That is still the caller's
			// timeout outcome, not a startup failure that should bypass the
			// sandbox result contract.
			return Result{Duration: runDuration(start), TimedOut: true}, nil
		}
		if ctx.Err() != nil {
			return Result{Duration: runDuration(start)}, ctx.Err()
		}
		return Result{Duration: runDuration(start)}, fmt.Errorf("code: start: %w", err)
	}
	// Start draining before attaching the tree. This keeps cleanup bounded even
	// if tree setup fails after a child has already begun writing.
	startCapture()
	// Full-access runs receive zero resource limits above, so this stays zero
	// there. Controlled modes get an explicit kernel-enforced concurrent
	// process ceiling on Windows; POSIX shells inherit the ulimit spelling.
	tree, err = attachProcessTree(cmd, processTreeLimits{
		maxProcesses: limits.processes,
		memoryBytes:  limits.memoryBytes,
	})
	if err != nil {
		p.mu.Unlock()
		_ = cmd.Cancel()
		_ = cmd.Wait()
		<-stdoutDone
		<-stderrDone
		return Result{Duration: runDuration(start)}, fmt.Errorf("code: attach process tree: %w", err)
	}
	treeReady.Store(true)
	done := make(chan struct{})
	if p.active == nil {
		p.active = make(map[*exec.Cmd]chan struct{})
	}
	p.active[cmd] = done
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.active, cmd)
		close(done)
		p.mu.Unlock()
	}()
	waitErr := cmd.Wait()
	stdoutCopyErr := <-stdoutDone
	stderrCopyErr := <-stderrDone
	duration := runDuration(start)

	timedOut := runCtx.Err() == context.DeadlineExceeded

	closeTree()
	if aclRun != nil {
		if err := aclRun.close(); err != nil {
			return Result{Duration: duration, TimedOut: timedOut}, fmt.Errorf("code: cleanup Windows ACL sandbox: %w", err)
		}
	}
	// os/exec closes StdoutPipe as part of Wait. On Windows a copy goroutine
	// that was already draining the fully-settled stream can observe that
	// close as "file already closed" even though the command exited normally;
	// do not turn that ordinary reap race into a transport failure.
	if stdoutCopyErr != nil && !expectedPipeClose(stdoutCopyErr) && ctx.Err() == nil && runCtx.Err() == nil {
		return Result{Duration: duration, TimedOut: timedOut}, fmt.Errorf("code: read stdout: %w", stdoutCopyErr)
	}
	if stderrCopyErr != nil && !expectedPipeClose(stderrCopyErr) && ctx.Err() == nil && runCtx.Err() == nil {
		return Result{Duration: duration, TimedOut: timedOut}, fmt.Errorf("code: read stderr: %w", stderrCopyErr)
	}
	// Every subprocess result crosses the model/session boundary as UTF-8. On
	// Windows shellCommand selects code page 65001; this final guard also keeps
	// malformed third-party bytes from becoming invalid Go strings.
	stdout := []byte(strings.ToValidUTF8(stdoutCapture.String(), "?"))
	stderr := []byte(strings.ToValidUTF8(stderrCapture.String(), "?"))
	truncated := stdoutCapture.truncated || stderrCapture.truncated

	res := Result{
		ExitCode:  0,
		Stdout:    string(stdout),
		Stderr:    string(stderr),
		TimedOut:  timedOut,
		Truncated: truncated,
		Duration:  duration,
	}

	if waitErr != nil {
		if timedOut {
			// The run was hard-killed by the timeout: a normal sandbox outcome.
			if ee, ok := waitErr.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
			}
			return res, nil
		}
		// Not a sandbox timeout: distinguish a caller-cancelled/expired context
		// from a plain non-zero exit.
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if ee, ok := waitErr.(*exec.ExitError); ok {
			// Non-zero exit: a normal sandbox outcome reported in Result.
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("code: %w", waitErr)
	}
	return res, nil
}

func expectedPipeClose(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "file already closed")
}

func runDuration(start time.Time) time.Duration {
	duration := time.Since(start)
	if duration <= 0 {
		return time.Nanosecond
	}
	return duration
}

// detectBwrap returns a usable bubblewrap executable and whether its network
// namespace is usable. Some hosts permit user namespaces but deny network
// namespaces; in that case the file boundary remains available while network
// isolation stays fail-closed.
func detectBwrap() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	runner, err := exec.LookPath("bwrap")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runner,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
		"--", "true",
	)
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return "", false
	}
	networkCtx, networkCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer networkCancel()
	networkProbe := exec.CommandContext(networkCtx, runner,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-net",
		"--die-with-parent",
		"--", "true",
	)
	networkOK := networkProbe.Run() == nil && networkCtx.Err() == nil
	return runner, networkOK
}

// detectSeatbelt returns a usable macOS sandbox-exec executable. Seatbelt is
// the reference local backend's file-effect fence on Darwin. The probe is
// deliberately the same deny-write profile used for real calls, so merely
// finding sandbox-exec cannot make the controlled profile appear available.
func detectSeatbelt() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	runner, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	profile := seatbeltProfile(SandboxReadOnly, "")
	cmd := exec.CommandContext(ctx, runner, "-p", profile, "--", "true")
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return ""
	}
	return runner
}

// bwrapCommand expresses DSH's file-effect vocabulary using bubblewrap's
// mount profile. The complete host tree is visible read-only; workspace-write
// remounts only the selected workspace and gives /tmp an ephemeral tmpfs.
// When the probe proved that network namespaces are available, the same
// profile also unshares the network namespace, making the no-network policy an
// enforced backend property rather than an environment convention.
func bwrapCommand(runner, workspaceRoot string, mode SandboxMode, networkIsolation bool, argv []string) []string {
	wrapped := []string{
		runner,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if networkIsolation {
		wrapped = append(wrapped, "--unshare-net")
		// A new network namespace does not automatically remount the host's
		// sysfs snapshot. Cover /sys/class/net so the enforced namespace and
		// the observable interface inventory agree.
		wrapped = append(wrapped, "--tmpfs", "/sys/class/net")
	}
	if mode == SandboxWorkspaceWrite {
		wrapped = append(wrapped, "--tmpfs", "/tmp", "--bind", workspaceRoot, workspaceRoot)
	}
	wrapped = append(wrapped, "--")
	wrapped = append(wrapped, argv...)
	return wrapped
}

// seatbeltProfile expresses the same file-effect vocabulary as the DSH local
// provider's Seatbelt profile. Seatbelt's allow-default baseline is retained
// because this seam promises file effects only; network/process visibility are
// not claimed as isolated capabilities here.
func seatbeltProfile(mode SandboxMode, workspaceRoot string) string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if mode == SandboxWorkspaceWrite {
		roots := []string{workspaceRoot, "/tmp"}
		if temp := filepath.Clean(os.TempDir()); temp != "/tmp" {
			roots = append(roots, temp)
		}
		seen := make(map[string]struct{}, len(roots))
		for _, root := range roots {
			root = filepath.Clean(root)
			if root == "." || root == "" {
				continue
			}
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			forms = append(forms, `(allow file-write* (subpath `+sbplString(root)+`))`)
		}
	}
	return strings.Join(forms, " ")
}

func sbplString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func seatbeltCommand(runner, workspaceRoot string, mode SandboxMode, argv []string) []string {
	wrapped := []string{runner, "-p", seatbeltProfile(mode, workspaceRoot), "--"}
	return append(wrapped, argv...)
}

// shellCommand returns the argv of a single non-interactive shell line for
// language "sh": cmd /C on Windows, /bin/sh -c elsewhere.
func shellCommand(code string) []string {
	if runtime.GOOS == "windows" {
		// cmd.exe otherwise uses the active OEM code page (CP936 on many
		// Chinese Windows installations), while tool results are UTF-8.
		return []string{"cmd.exe", "/C", "chcp 65001 >nul & " + code}
	}
	return []string{"/bin/sh", "-c", code}
}

// shellCommandWithLimits puts hard POSIX resource limits in the shell that
// owns the user command. Hard limits are inherited across fork/exec, so a
// command cannot remove them with a later ulimit call and descendants remain
// covered. Linux bwrap uses the stronger prlimit exec boundary; Windows uses
// its Job Object memory/process boundary instead.
func shellCommandWithLimits(code string, limits shellResourceLimits) []string {
	return shellCommandWithLimitsArgv(shellCommand(code), limits)
}

func shellCommandWithLimitsArgv(command []string, limits shellResourceLimits) []string {
	if runtime.GOOS == "windows" || (limits.memoryBytes <= 0 && limits.fileSizeBytes <= 0 && limits.processes <= 0) {
		return command
	}
	parts := make([]string, 0, 4)
	if limits.memoryBytes > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -v %d", limits.memoryBytes/1024))
	}
	if limits.fileSizeBytes > 0 {
		// POSIX ulimit -f is expressed in 512-byte blocks.
		parts = append(parts, fmt.Sprintf("ulimit -f %d", (limits.fileSizeBytes+511)/512))
	}
	if limits.processes > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -u %d", limits.processes))
	}
	// Use a hard-limit setting and fail before the sandbox command if a
	// platform shell cannot install one of the requested controls. Quote every
	// argument so arbitrary sandbox argv remains one exact exec boundary.
	for i := range parts {
		// Set both soft and hard ceilings. The hard ceiling is what prevents
		// model-authored shell code from raising the soft value later.
		parts[i] = strings.Replace(parts[i], "ulimit -", "ulimit -S -H -", 1) + " || exit 125"
	}
	quoted := make([]string, len(command))
	for i, argument := range command {
		quoted[i] = shellSingleQuote(argument)
	}
	parts = append(parts, "exec "+strings.Join(quoted, " "))
	return []string{"/bin/sh", "-c", strings.Join(parts, "; ") + ";"}
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func prlimitCommand(runner string, limits shellResourceLimits, command []string) []string {
	argv := []string{
		runner,
		"--as=" + strconv.FormatInt(limits.memoryBytes, 10),
		"--fsize=" + strconv.FormatInt((limits.fileSizeBytes+511)/512, 10),
		"--nproc=" + strconv.Itoa(limits.processes),
		"--",
	}
	return append(argv, command...)
}

// scrubbedEnv returns the parent environment minus credential-shaped entries.
func scrubbedEnv() []string {
	return scrubEnv(os.Environ())
}

// scrubEnv removes every "NAME=value" entry whose NAME contains a
// credential-shaped token, case-insensitively, and keeps the rest.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if isSensitiveEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, tok := range sensitiveEnvTokens {
		if strings.Contains(upper, tok) {
			return true
		}
	}
	return false
}

// checkOpen rejects operations on a closed provider.
func (p *localProvider) checkOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	return nil
}

// Close marks the provider closed so no further runs are accepted. It is
// idempotent and releases nothing else (no goroutines live here).
func (p *localProvider) Close() error {
	p.mu.Lock()
	if p.closed {
		closeDone := p.closeDone
		p.mu.Unlock()
		<-closeDone
		return nil
	}
	p.closed = true
	active := make([]*exec.Cmd, 0, len(p.active))
	done := make([]chan struct{}, 0, len(p.active))
	for cmd, finished := range p.active {
		active = append(active, cmd)
		done = append(done, finished)
	}
	p.mu.Unlock()
	for _, cmd := range active {
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		} else if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	for _, finished := range done {
		<-finished
	}
	close(p.closeDone)
	return nil
}
