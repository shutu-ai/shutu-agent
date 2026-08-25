package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/jobs"
)

// runCommandName is the single execution-class tool (design.md §5 / D10 落地).
// It is registered only when tools.run_command.enabled is true (main.go), so
// the model does not even see it by default.
const runCommandName = "bash"

// sensitiveEnvTokens are the credential-shaped substrings removed from the
// environment before every run_command (mirrors dsh's scrubbedParentEnv, with
// API added): the child process must never implicitly inherit the agent's
// keys, secrets, tokens, passwords, or API credentials.
var sensitiveEnvTokens = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}

// RunCommand executes a single shell command line in a fixed working
// directory. It is deliberately not an interactive shell: on Windows the
// command runs through "cmd /C <line>", elsewhere "/bin/sh -c <line>".
type RunCommand struct {
	Workdir     string // fixed working directory; empty means the agent's own cwd
	WorkdirFunc func() string
	JobsFunc    func() jobs.Registry
	OwnerFunc   func() string
	DshEnvFunc  ManagedEnvFunc
	Background  bool
}

// NewRunCommand returns a RunCommand bound to a fixed working directory.
func NewRunCommand(workdir string) RunCommand { return RunCommand{Workdir: workdir} }

// NewRunCommandForWorkdir resolves the working directory for each invocation,
// allowing one registry to serve multiple session workspaces.
func NewRunCommandForWorkdir(workdir func() string) RunCommand {
	return RunCommand{WorkdirFunc: workdir}
}

// NewRunCommandForWorkdirAndJobs wires the dsh background-job surface without
// creating an import cycle with cmd/pa's composition root. The providers are
// evaluated per call because jobs are registered after the base tools.
func NewRunCommandForWorkdirAndJobs(workdir func() string, jobsFunc func() jobs.Registry, ownerFunc func() string, background bool) RunCommand {
	return RunCommand{WorkdirFunc: workdir, JobsFunc: jobsFunc, OwnerFunc: ownerFunc, Background: background}
}

func (RunCommand) Name() string { return runCommandName }

func (RunCommand) Description() string {
	return "Execute a bash command and return stdout/stderr. Each call runs in a fresh non-interactive shell; " +
		"non-zero exits are reported as [exit code: N]. Credential-shaped environment variables are scrubbed. " +
		"Pass workdir instead of using cd; use run_in_background for long-running commands."
}

func (t RunCommand) Schema() map[string]any {
	properties := map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "the single command line to execute",
		},
		"description": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "Clear, concise description of what this command does in active voice.",
		},
		"timeoutMs": map[string]any{
			"type":        "number",
			"minimum":     1,
			"description": "Timeout in milliseconds; timeout is reported as a normal result.",
		},
		"workdir": map[string]any{
			"type":        "string",
			"description": "Working directory; relative paths resolve against the session workspace.",
		},
	}
	if t.Background {
		properties["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Run in the background; observe with job_output or stop with job_kill.",
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"command", "description"},
		"additionalProperties": false,
	}
}

// Execute runs the command with a scrubbed environment and a fixed working
// directory. Output (stdout+stderr merged) is captured to a temp file rather
// than a pipe, so a grandchild that lingers after we kill the direct child can
// never keep Wait blocked. A non-zero exit code is reported inline as
// "[exit code: N]" plus the output (a normal result, so the model sees what
// the command printed); a context cancellation or timeout is reported as an
// error (the Execute pipeline maps it to tool/error).
func (t RunCommand) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command         string `json:"command"`
		Description     string `json:"description"`
		TimeoutMS       *int64 `json:"timeoutMs"`
		Workdir         string `json:"workdir"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("run_command: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("run_command: empty command")
	}
	if strings.TrimSpace(a.Description) == "" {
		return "", fmt.Errorf("run_command: description is required")
	}
	if a.TimeoutMS != nil && *a.TimeoutMS <= 0 {
		return "", fmt.Errorf("run_command: timeoutMs must be a positive number")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("run_command: cancelled: %w", err)
	}
	workdir := t.Workdir
	if t.WorkdirFunc != nil {
		workdir = t.WorkdirFunc()
	}
	if a.Workdir != "" {
		if !filepath.IsAbs(a.Workdir) && workdir != "" {
			workdir = filepath.Join(workdir, a.Workdir)
		} else {
			workdir = a.Workdir
		}
	}
	if a.RunInBackground {
		if !t.Background || t.JobsFunc == nil || t.JobsFunc() == nil {
			return "", fmt.Errorf("run_command: run_in_background is unavailable (jobs disabled)")
		}
		return t.startBackground(ctx, a.Command, workdir)
	}
	return t.runForeground(ctx, a.Command, workdir, a.TimeoutMS)
}

func (t RunCommand) runForeground(ctx context.Context, command, workdir string, timeoutMS *int64) (string, error) {
	startedAt := time.Now()
	execCtx := ctx
	timedOut := false
	requested := int64(0)
	if timeoutMS == nil {
		requested = DefaultRunCommandTimeout.Milliseconds()
		timeoutMS = &requested
	}
	if timeoutMS != nil {
		requested = *timeoutMS
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(requested)*time.Millisecond)
		defer cancel()
	}
	cmd := newCommand(command, workdir, bashEnv(t.DshEnvFunc))
	outFile, err := os.CreateTemp("", "pa-run-stdout-*.txt")
	if err != nil {
		return "", fmt.Errorf("run_command: create output file: %w", err)
	}
	outPath := outFile.Name()
	errFile, err := os.CreateTemp("", "pa-run-stderr-*.txt")
	if err != nil {
		outFile.Close()
		os.Remove(outPath)
		return "", fmt.Errorf("run_command: create error file: %w", err)
	}
	errPath := errFile.Name()
	defer outFile.Close()
	defer errFile.Close()
	defer os.Remove(outPath)
	defer os.Remove(errPath)
	cmd.Stdout = outFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("run_command: start: %w", err)
	}
	// Interrupt the process when the context is done (Ctrl+C or the Execute
	// deadline): the direct child is killed, and its output file (not a pipe)
	// lets Wait return immediately.
	stop := monitorCtx(execCtx, cmd)
	waitErr := cmd.Wait()
	stop()

	out, readErr := os.ReadFile(outPath)
	if readErr != nil {
		return "", fmt.Errorf("run_command: read output: %w", readErr)
	}
	errOut, readErr := os.ReadFile(errPath)
	if readErr != nil {
		return "", fmt.Errorf("run_command: read stderr: %w", readErr)
	}
	out = []byte(strings.ToValidUTF8(string(out), "\uFFFD"))
	errOut = []byte(strings.ToValidUTF8(string(errOut), "\uFFFD"))
	if waitErr != nil {
		if ctx.Err() == context.Canceled {
			return "", fmt.Errorf("run_command: interrupted: %w", waitErr)
		}
		if execCtx.Err() == context.DeadlineExceeded {
			timedOut = true
			if requested == 0 {
				requested = time.Since(startedAt).Milliseconds()
				if requested < 1 {
					requested = 1
				}
			}
		}
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return formatBashOutput(out, errOut, ee.ExitCode(), timedOut, requested), nil
		}
		return "", fmt.Errorf("run_command: %w", waitErr)
	}
	if execCtx.Err() == context.DeadlineExceeded {
		timedOut = true
		if requested == 0 {
			requested = time.Since(startedAt).Milliseconds()
			if requested < 1 {
				requested = 1
			}
		}
	}
	return formatBashOutput(out, errOut, 0, timedOut, requested), nil
}

func (t RunCommand) startBackground(ctx context.Context, command, workdir string) (string, error) {
	provider := t.JobsFunc()
	var mu sync.Mutex
	var live *exec.Cmd
	var capture capturePaths
	id, err := provider.Start(ctx, jobs.JobStart{
		Kind: jobs.Kind(runCommandName), Label: command,
		OwnerSession: t.ownerSession(), OutputLimitBytes: 64 * 1024,
		ReadOutput: capture.Read,
		Run: func(jctx context.Context) (outcome jobs.JobOutcome, runErr error) {
			streamOutput := ""
			defer func() { capture.Finish(streamOutput) }()
			cmd := newCommand(command, workdir, bashEnv(t.DshEnvFunc))
			mu.Lock()
			live = cmd
			mu.Unlock()
			defer func() { mu.Lock(); live = nil; mu.Unlock() }()
			outFile, err := os.CreateTemp("", "pa-bash-job-stdout-*.txt")
			if err != nil {
				return jobs.JobOutcome{}, err
			}
			path := outFile.Name()
			errFile, err := os.CreateTemp("", "pa-bash-job-stderr-*.txt")
			if err != nil {
				outFile.Close()
				os.Remove(path)
				return jobs.JobOutcome{}, err
			}
			errPath := errFile.Name()
			capture.Set(path, errPath)
			defer outFile.Close()
			defer errFile.Close()
			defer os.Remove(path)
			defer os.Remove(errPath)
			cmd.Stdout, cmd.Stderr = outFile, errFile
			if err := cmd.Start(); err != nil {
				return jobs.JobOutcome{}, err
			}
			stop := monitorCtx(jctx, cmd)
			waitErr := cmd.Wait()
			stop()
			out, err := os.ReadFile(path)
			if err != nil {
				return jobs.JobOutcome{}, err
			}
			errOut, err := os.ReadFile(errPath)
			if err != nil {
				return jobs.JobOutcome{}, err
			}
			streamOutput = formatShellStreams(string(out), string(errOut))
			if jctx.Err() != nil {
				return jobs.JobOutcome{Status: jobs.StatusKilled, Detail: "killed", Output: formatBashOutput(out, errOut, 0, false, 0)}, nil
			}
			code := 0
			if ee, ok := waitErr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			return jobs.JobOutcome{Status: jobs.StatusCompleted, Detail: fmt.Sprintf("exit code: %d", code), Output: formatBashOutput(out, errOut, code, false, 0)}, nil
		},
		Cancel: func(string) error {
			mu.Lock()
			defer mu.Unlock()
			if live != nil {
				killTree(live)
			}
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("run_command: %w", err)
	}
	return fmt.Sprintf("started background job %s; observe with job_output or job_kill", id), nil
}

func (t RunCommand) ownerSession() string {
	if t.OwnerFunc != nil {
		return t.OwnerFunc()
	}
	return ""
}

// newCommand assembles a single-line, non-interactive shell invocation.
func newCommand(command, workdir string, env []string) *exec.Cmd {
	var argv []string
	if runtime.GOOS == "windows" {
		argv = []string{"cmd.exe", "/C", "chcp 65001 >nul & " + command}
	} else {
		argv = []string{"/bin/sh", "-c", command}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = env
	prepareProcessGroup(cmd)
	return cmd
}

// monitorCtx terminates the command's process tree when ctx is done. The
// returned stop func must be called once Wait returns so the watcher goroutine
// does not leak.
func monitorCtx(ctx context.Context, cmd *exec.Cmd) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree(cmd)
		case <-done:
		}
	}()
	return func() { close(done) }
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
		if isSensitiveEnvName(name) || isManagedDshEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isManagedDshEnvName(name string) bool {
	return strings.HasPrefix(strings.ToUpper(name), "DSH_")
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

// capturePaths exposes consuming output from a pair of append-only process
// output files while the process is running and retains the unread stream tail
// after settlement.
type capturePaths struct {
	mu             sync.Mutex
	stdout         string
	stderr         string
	stdoutOffset   int
	stderrOffset   int
	finished       string
	finishedOffset int
	active         bool
}

func (c *capturePaths) Set(stdout, stderr string) {
	c.mu.Lock()
	c.stdout, c.stderr = stdout, stderr
	c.stdoutOffset = 0
	c.stderrOffset = 0
	c.finished = ""
	c.finishedOffset = 0
	c.active = true
	c.mu.Unlock()
}

func (c *capturePaths) Finish(output string) {
	c.mu.Lock()
	c.stdout, c.stderr = "", ""
	c.finished = output
	c.finishedOffset = 0
	c.active = false
	c.mu.Unlock()
}

func (c *capturePaths) Read() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return consumeDelta(c.finished, &c.finishedOffset), nil
	}
	stdout, stderr := c.stdout, c.stderr
	if stdout == "" && stderr == "" {
		return "", nil
	}
	out, err := os.ReadFile(stdout)
	if err != nil {
		return "", err
	}
	errOut, err := os.ReadFile(stderr)
	if err != nil {
		return "", err
	}
	stdoutDelta := consumeDelta(string(out), &c.stdoutOffset)
	stderrDelta := consumeDelta(string(errOut), &c.stderrOffset)
	return formatShellStreams(stdoutDelta, stderrDelta), nil
}

func consumeDelta(output string, offset *int) string {
	if *offset < 0 || *offset > len(output) {
		*offset = 0
	}
	delta := output[*offset:]
	*offset = len(output)
	return delta
}

func formatBashOutput(stdout, stderr []byte, code int, timedOut bool, timeoutMS int64) string {
	text := formatShellStreams(string(stdout), string(stderr))
	if text == "" {
		text = "(no output)"
	}
	if timedOut {
		text += fmt.Sprintf("\n[timed out after %dms]", timeoutMS)
	}
	if code != 0 {
		text += fmt.Sprintf("\n[exit code: %d]", code)
	}
	return text
}

func formatShellStreams(stdout, stderr string) string {
	body := stdout
	if stderr != "" {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[stderr]\n" + stderr
	}
	return body
}
