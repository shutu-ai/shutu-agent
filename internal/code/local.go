package code

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Defaults applied when the matching RunRequest field is zero.
const (
	// defaultTimeout bounds a run when RunRequest.Timeout is zero (30s).
	defaultTimeout = 30 * time.Second
	// defaultMaxOutput is the per-stream output cap when RunRequest.MaxOutput
	// is zero (64KiB).
	defaultMaxOutput = 64 * 1024
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
// is captured to temp files rather than pipes so a grandchild that outlives
// the direct child (e.g. ping spawned by cmd.exe) can never keep Wait blocked
// (same pattern as internal/tools/run_command.go).
type localProvider struct {
	mu     sync.Mutex
	closed bool
}

// NewLocalProvider returns the default local subprocess sandbox Provider.
func NewLocalProvider() Provider { return newLocalProvider() }

func newLocalProvider() *localProvider { return &localProvider{} }

// Name identifies the provider in the registry ("local").
func (p *localProvider) Name() string { return "local" }

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

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxOut := req.MaxOutput
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}

	cwd := req.Cwd
	if cwd == "" {
		base, err := os.Getwd()
		if err != nil {
			return Result{}, fmt.Errorf("code: resolve cwd base: %w", err)
		}
		cwd = filepath.Join(base, sandboxDirName)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return Result{}, fmt.Errorf("code: create sandbox dir %s: %w", cwd, err)
	}

	return p.exec(ctx, req.Code, cwd, timeout, maxOut)
}

// exec runs the code as a single non-interactive shell line (cmd /C on
// Windows, /bin/sh -c elsewhere) with a scrubbed environment in the sandbox
// cwd. The run context carries the timeout; when it expires the direct child
// is hard-killed (exec.CommandContext) and TimedOut is set. Output goes to two
// temp files and is read back after Wait, then capped per stream.
func (p *localProvider) exec(ctx context.Context, code, cwd string, timeout time.Duration, maxOut int) (Result, error) {
	argv := shellCommand(code)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = scrubbedEnv()

	outFile, err := os.CreateTemp("", "pa-code-out-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("code: create stdout file: %w", err)
	}
	defer outFile.Close()
	defer os.Remove(outFile.Name())

	errFile, err := os.CreateTemp("", "pa-code-err-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("code: create stderr file: %w", err)
	}
	defer errFile.Close()
	defer os.Remove(errFile.Name())

	cmd.Stdout = outFile
	cmd.Stderr = errFile

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Duration: time.Since(start)}, fmt.Errorf("code: start: %w", err)
	}
	waitErr := cmd.Wait()
	duration := time.Since(start)

	timedOut := runCtx.Err() == context.DeadlineExceeded

	stdout, readErr := os.ReadFile(outFile.Name())
	if readErr != nil {
		return Result{Duration: duration, TimedOut: timedOut}, fmt.Errorf("code: read stdout: %w", readErr)
	}
	stderr, readErr := os.ReadFile(errFile.Name())
	if readErr != nil {
		return Result{Duration: duration, TimedOut: timedOut}, fmt.Errorf("code: read stderr: %w", readErr)
	}
	// Every subprocess result crosses the model/session boundary as UTF-8. On
	// Windows shellCommand selects code page 65001; this final guard also keeps
	// malformed third-party bytes from becoming invalid Go strings.
	stdout = []byte(strings.ToValidUTF8(string(stdout), "�"))
	stderr = []byte(strings.ToValidUTF8(string(stderr), "�"))

	truncated := false
	if len(stdout) > maxOut {
		stdout = stdout[:maxOut]
		truncated = true
	}
	if len(stderr) > maxOut {
		stderr = stderr[:maxOut]
		truncated = true
	}

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
	defer p.mu.Unlock()
	p.closed = true
	return nil
}
