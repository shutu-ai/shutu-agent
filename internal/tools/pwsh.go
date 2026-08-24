// pwsh.go — the dsh-aligned pwsh tool (a port of dsh tool-pwsh + pwsh-local):
// each call runs ONE PowerShell command in a FRESH process
// (`pwsh -NoLogo -NoProfile -NonInteractive -Command <line>`), so no state
// (cwd, variables, functions) persists between calls — the model passes
// workdir instead of cd. The command string is one argv element, so there is
// no intermediate shell and no shell-quoting layer to escape (the bash -c
// string domain has no equivalent here); native Windows paths (C:\...) pass
// through unchanged.
//
// The dsh contract is mirrored call-for-call: a UTF-8 encoding preamble rides
// ahead of every command (PowerShell 5.1 writes the OEM code page otherwise),
// credential-shaped parent environment entries are scrubbed, model-friendly
// overrides (NO_COLOR/PAGER/GIT_PAGER) are appended, non-zero exits are
// reported as [exit code: N] markers on a NORMAL result (never an error), a
// timeout kills the process tree and reports [timed out after Nms], empty
// output renders as (no output), and run_in_background registers a
// jobs.Registry job (kind pwsh) observed with the job_* tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/jobs"
)

// pwshToolName is the model-facing tool name (dsh: pwsh). The registry
// whitelist mirrors it via config.terminalToolNames.
const pwshToolName = "pwsh"

// dsh pwsh-local defaults: 120s per-call default, 600s cap. The registry's
// per-tool deadline (tools.timeout) is the outer bound; whichever fires first
// is reported as the timeout.
const (
	pwshDefaultTimeoutMS = 120_000
	pwshMaxTimeoutMS     = 600_000
	// pwshJobOutputLimit bounds a background job's stored output (dsh's
	// maxOutputBytes default); the registry applies the same cap to
	// foreground results via Policy.OutputLimit.
	pwshJobOutputLimit = 64 * 1024
)

// pwshEncodingPreamble pins UTF-8 output encoding for every command (dsh
// ENCODING_PREAMBLE): the subprocess output is decoded as UTF-8, but Windows
// PowerShell 5.1 (the fallback executable) writes the console/OEM code page
// by default, which garbles non-ASCII output; pwsh 7 defaults to UTF-8 and is
// unaffected. It rides on line 1 after "; " so PowerShell error line numbers
// stay accurate.
const pwshEncodingPreamble = "[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [System.Text.UTF8Encoding]::new($false); "

// pwshEnvOverrides are the model-friendly environment overrides (dsh
// ENV_OVERRIDES): disable colors and pagers that would garble tool output.
// An override already present in the parent environment is kept (dsh
// ordering: the parent's value wins over the override).
var pwshEnvOverrides = []string{"NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat"}

// PwshOpts configures the pwsh tool (the composition root supplies them).
type PwshOpts struct {
	// Workdir is the default working directory of every call (the session
	// workspace). Empty means the agent process's own working directory.
	Workdir string
	// WorkdirFunc, when set, is evaluated for every call and takes precedence
	// over Workdir. It is the session-header cwd bridge used by Web sessions.
	WorkdirFunc func() string
	// Jobs is the background-job registry used by run_in_background; nil
	// means the parameter is not advertised and is rejected.
	Jobs jobs.Registry
	// Owner returns the current session id, the authorization boundary for
	// background jobs. nil means jobs are unowned.
	Owner func() string
}

// PwshTool runs one PowerShell command in a fresh process per call.
type PwshTool struct {
	workdir     string
	workdirFunc func() string
	jobs        jobs.Registry
	owner       func() string
	background  bool // run_in_background advertised and accepted
}

// NewPwsh returns a PwshTool bound to the composition's defaults. Background
// execution is available exactly when a usable jobs registry is supplied.
func NewPwsh(opts PwshOpts) PwshTool {
	return PwshTool{
		workdir:     opts.Workdir,
		workdirFunc: opts.WorkdirFunc,
		jobs:        opts.Jobs,
		owner:       opts.Owner,
		background:  registryPresent(opts.Jobs),
	}
}

// registryPresent reports whether r holds a usable registry. An interface
// wrapping a typed nil pointer (e.g. a nil *jobs.Local assigned to the
// interface) is as absent as a nil interface — the composition root's `a.jobs
// != nil` check alone would otherwise slip a nil-backed interface through.
func registryPresent(r jobs.Registry) bool {
	if r == nil {
		return false
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return !v.IsNil()
	}
	return true
}

func (PwshTool) Name() string { return pwshToolName }

// Description is the model-facing summary (dsh tool-pwsh description, minus
// the sandbox/escalation sentences shutu does not mount for pwsh).
func (PwshTool) Description() string {
	return "Execute a PowerShell command (`pwsh -Command`) and return its stdout/stderr. " +
		"Each call runs in a fresh pwsh process: no state (cwd, variables, functions) persists between calls — " +
		"pass `workdir` instead of using `cd`. Paths use native Windows form (`C:\\...`); read environment " +
		"variables with `$env:NAME`. Non-zero exits are reported as `[exit code: N]`. " +
		"Credential-shaped environment variables are scrubbed before execution. " +
		"On Windows a force-killed command settles as `[exit code: 1]` without a signal marker — " +
		"treat it as an interruption, not a command failure. " +
		"Long output is truncated to its tail; the full output is saved to a file whose path is reported when available. " +
		"Set `run_in_background: true` for long-running commands: the call returns a job id immediately; " +
		"observe with `job_status` or `job_read`, await with `job_wait`, stop with `job_cancel`."
}

func (t PwshTool) Schema() map[string]any {
	properties := map[string]any{
		"command": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "The PowerShell command to execute.",
		},
		"description": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"; \"Get-Process\" → \"List running processes\".",
		},
		"timeoutMs": map[string]any{
			"type":        "number",
			"minimum":     1,
			"description": "Timeout in milliseconds. The tool applies its default (120000) and cap (600000) and kills the command on expiry; the deployment's tools.timeout bound also applies.",
		},
		"workdir": map[string]any{
			"type":        "string",
			"description": "Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.",
		},
	}
	if t.background {
		properties["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Run in the background and return a job id immediately (observe with job_status or job_read, await with job_wait, stop with job_cancel). No timeout applies.",
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"command", "description"},
		"additionalProperties": false,
	}
}

// Execute validates the args and runs the command in a fresh process. A
// non-zero exit, a timeout and a killed process are NORMAL results carrying
// markers (the model decides how to react); only infrastructure failures
// (validation, spawn, interruption) are errors — the dsh render contract.
func (t PwshTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command         string `json:"command"`
		Description     string `json:"description"`
		TimeoutMS       *int64 `json:"timeoutMs"`
		Workdir         string `json:"workdir"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("pwsh: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("pwsh: command is required")
	}
	if strings.TrimSpace(a.Description) == "" {
		return "", fmt.Errorf("pwsh: description is required")
	}
	if a.TimeoutMS != nil && *a.TimeoutMS <= 0 {
		return "", fmt.Errorf("pwsh: timeoutMs must be a positive number")
	}
	base := t.workdir
	if t.workdirFunc != nil {
		base = t.workdirFunc()
	}
	workdir := resolveWorkdir(a.Workdir, base)
	if a.RunInBackground {
		if !t.background {
			return "", fmt.Errorf("pwsh: run_in_background is unavailable (jobs disabled)")
		}
		return t.startBackground(ctx, a.Command, workdir)
	}
	return t.runForeground(ctx, a.Command, workdir, a.TimeoutMS)
}

// resolveWorkdir resolves the model workdir: an explicit absolute path is
// used verbatim, an explicit relative one is resolved against the session
// workspace, and an absent one falls back to the workspace (dsh
// resolveWorkdir).
func resolveWorkdir(requested, base string) string {
	if requested == "" {
		return base
	}
	if base != "" && !filepath.IsAbs(requested) {
		return filepath.Join(base, requested)
	}
	return requested
}

// resolvePwshPath finds the PowerShell executable: pwsh (PowerShell 7) first,
// then Windows PowerShell 5.1 on Windows, so a host without pwsh on PATH
// still works (dsh candidatePwshPaths).
func resolvePwshPath() (string, error) {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("powershell.exe"); err == nil {
			return p, nil
		}
	}
	return "", errors.New("pwsh: no pwsh executable found on PATH (install PowerShell 7)")
}

// effectiveTimeout clamps the model timeout to [1ms, pwshMaxTimeoutMS],
// defaulting to pwshDefaultTimeoutMS when absent (dsh clampTimeout). The
// registry's per-tool deadline is folded in by the caller.
func effectiveTimeout(requested *int64) time.Duration {
	ms := int64(pwshDefaultTimeoutMS)
	if requested != nil {
		ms = *requested
	}
	if ms > pwshMaxTimeoutMS {
		ms = pwshMaxTimeoutMS
	}
	if ms < 1 {
		ms = 1
	}
	return time.Duration(ms) * time.Millisecond
}

// pwshCommand assembles the fresh-process invocation: the command string is
// ONE argv element after the UTF-8 preamble, so no quoting layer exists.
func pwshCommand(pwsh, command, workdir string) *exec.Cmd {
	cmd := exec.Command(pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", pwshEncodingPreamble+command)
	cmd.Dir = workdir
	cmd.Env = pwshEnv()
	prepareProcessGroup(cmd)
	return cmd
}

// pwshEnv returns the scrubbed parent environment plus the model-friendly
// overrides; a parent value for an override name is kept (dsh env ordering:
// ENV_OVERRIDES first, the caller's env wins).
func pwshEnv() []string {
	env := scrubEnv(os.Environ())
	for _, ov := range pwshEnvOverrides {
		name, _, _ := strings.Cut(ov, "=")
		if !hasEnvName(env, name) {
			env = append(env, ov)
		}
	}
	return env
}

func hasEnvName(env []string, name string) bool {
	for _, kv := range env {
		n, _, ok := strings.Cut(kv, "=")
		if ok && n == name {
			return true
		}
	}
	return false
}

// runForeground runs one command in a fresh process and renders the result.
// The registry's per-tool deadline and the model's timeoutMs share one
// effective timeout: whichever fires first kills the process tree and is
// reported as [timed out after Nms] on a normal result; only an upstream
// cancellation (user stop) surfaces as an error. Output goes to temp files
// (not pipes), so a grandchild that lingers after the kill can never keep
// Wait blocked.
func (t PwshTool) runForeground(ctx context.Context, command, workdir string, timeoutMS *int64) (string, error) {
	pwsh, err := resolvePwshPath()
	if err != nil {
		return "", err
	}
	timeout := effectiveTimeout(timeoutMS)
	if d, ok := ctx.Deadline(); ok {
		if rem := time.Until(d); rem < timeout {
			timeout = rem
		}
	}
	if timeout <= 0 {
		timeout = time.Millisecond
	}

	cmd := pwshCommand(pwsh, command, workdir)
	outFile, errFile, cleanup, err := pwshOutputFiles()
	if err != nil {
		return "", fmt.Errorf("pwsh: %w", err)
	}
	defer cleanup()
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("pwsh: start: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waitDone:
	case <-timer.C:
		timedOut = true
		killTree(cmd)
		waitErr = <-waitDone
	case <-ctx.Done():
		killTree(cmd)
		waitErr = <-waitDone
		if ctx.Err() == context.DeadlineExceeded {
			// The registry's per-tool deadline is the deployment timeout; it
			// is reported with the marker, never as an error (dsh: the
			// executor's timeout is a normal result).
			timedOut = true
		} else {
			return "", fmt.Errorf("pwsh: interrupted: %w", waitErr)
		}
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}

	stdout, readErr := os.ReadFile(outPathOf(outFile))
	if readErr != nil {
		return "", fmt.Errorf("pwsh: read stdout: %w", readErr)
	}
	stderr, readErr := os.ReadFile(outPathOf(errFile))
	if readErr != nil {
		return "", fmt.Errorf("pwsh: read stderr: %w", readErr)
	}
	// A non-zero exit is reported, never errored (dsh render): only a Wait
	// failure that is not an exit (infrastructure) is an error.
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return "", fmt.Errorf("pwsh: %w", waitErr)
		}
	}
	exitCode := 0
	signal := ""
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			if sig, ok := exitSignalName(ee); ok {
				signal = sig
			}
		}
	}
	return formatPwshOutput(string(stdout), string(stderr), exitCode, signal, timedOut, timeout.Milliseconds()), nil
}

// startBackground registers the command as a jobs.Registry job (kind pwsh)
// and returns the registry-issued id in the dsh acknowledgement text. The
// registry's owner fence and cancellation apply; the job's stored output is
// bounded to pwshJobOutputLimit.
func (t PwshTool) startBackground(ctx context.Context, command, workdir string) (string, error) {
	pwsh, err := resolvePwshPath()
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	var live *exec.Cmd
	id, err := t.jobs.Start(ctx, jobs.JobStart{
		Kind:             jobs.Kind(pwshToolName),
		Label:            command,
		OwnerSession:     t.ownerSession(),
		OutputLimitBytes: pwshJobOutputLimit,
		Run: func(jctx context.Context) (jobs.JobOutcome, error) {
			return runPwshJob(jctx, pwsh, command, workdir, &mu, &live)
		},
		Cancel: func(reason string) error {
			mu.Lock()
			c := live
			mu.Unlock()
			if c != nil {
				killTree(c)
			}
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("pwsh: %w", err)
	}
	return fmt.Sprintf("started background job %s; observe with job_status or job_read, await with job_wait, stop with job_cancel", id), nil
}

// ownerSession returns the current session id (the job authorization
// boundary); "" when no owner provider is installed (unowned job).
func (t PwshTool) ownerSession() string {
	if t.owner != nil {
		return t.owner()
	}
	return ""
}

// runPwshJob is the background job body: it runs the fresh-process command
// under the registry-owned context (cancelled by job_cancel / Close) and
// settles the job from the outcome — killed when the job context was
// cancelled, otherwise completed with the exit code as detail (a nonzero
// command exit is reported, not failed; dsh processOutcome).
func runPwshJob(ctx context.Context, pwsh, command, workdir string, mu *sync.Mutex, live **exec.Cmd) (jobs.JobOutcome, error) {
	cmd := pwshCommand(pwsh, command, workdir)
	mu.Lock()
	*live = cmd
	mu.Unlock()
	defer func() {
		mu.Lock()
		*live = nil
		mu.Unlock()
	}()

	outFile, errFile, cleanup, err := pwshOutputFiles()
	if err != nil {
		return jobs.JobOutcome{}, fmt.Errorf("pwsh: %w", err)
	}
	defer cleanup()
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		return jobs.JobOutcome{}, fmt.Errorf("pwsh: start: %w", err)
	}
	stop := monitorCtx(ctx, cmd)
	waitErr := cmd.Wait()
	stop()

	stdout, readErr := os.ReadFile(outPathOf(outFile))
	if readErr != nil {
		return jobs.JobOutcome{}, fmt.Errorf("pwsh: read stdout: %w", readErr)
	}
	stderr, readErr := os.ReadFile(outPathOf(errFile))
	if readErr != nil {
		return jobs.JobOutcome{}, fmt.Errorf("pwsh: read stderr: %w", readErr)
	}
	body := formatPwshOutput(string(stdout), string(stderr), 0, "", false, 0)
	if ctx.Err() != nil {
		return jobs.JobOutcome{Status: jobs.StatusKilled, Detail: "killed", Output: body}, nil
	}
	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return jobs.JobOutcome{}, fmt.Errorf("pwsh: %w", waitErr)
		}
	}
	return jobs.JobOutcome{
		Status: jobs.StatusCompleted,
		Detail: fmt.Sprintf("exit code: %d", exitCode),
		Output: formatPwshOutput(string(stdout), string(stderr), exitCode, "", false, 0),
	}, nil
}

// pwshOutputFiles creates the stdout and stderr capture files (temp files,
// not pipes — the run_command rationale: Wait never blocks on a lingering
// grandchild). The returned cleanup closes and removes both.
func pwshOutputFiles() (outFile, errFile *os.File, cleanup func(), err error) {
	outFile, err = os.CreateTemp("", "pa-pwsh-out-*.txt")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create stdout file: %w", err)
	}
	errFile, err = os.CreateTemp("", "pa-pwsh-err-*.txt")
	if err != nil {
		outFile.Close()
		os.Remove(outFile.Name())
		return nil, nil, nil, fmt.Errorf("create stderr file: %w", err)
	}
	return outFile, errFile, func() {
		outFile.Close()
		errFile.Close()
		os.Remove(outFile.Name())
		os.Remove(errFile.Name())
	}, nil
}

// outPathOf returns the capture-file path (the *os.File is still open when
// the result is read; os.ReadFile on a path we hold open works — the
// run_command pattern).
func outPathOf(f *os.File) string { return f.Name() }

// formatPwshOutput renders the model-facing result (dsh renderPwshResult):
// stdout, a marked [stderr] section, then the exit-status markers — a clean
// exit (0, no signal) produces no marker and empty output renders as
// (no output). The timed-out marker comes first because the exit marker
// anchors the tail.
func formatPwshOutput(stdout, stderr string, exitCode int, signal string, timedOut bool, timeoutMS int64) string {
	body := stdout
	if stderr != "" {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[stderr]\n" + stderr
	}
	if body == "" {
		body = "(no output)"
	}
	var markers []string
	if timedOut {
		markers = append(markers, fmt.Sprintf("[timed out after %dms]", timeoutMS))
	}
	if signal != "" {
		markers = append(markers, fmt.Sprintf("[killed by signal: %s]", signal))
	} else if exitCode != 0 {
		markers = append(markers, fmt.Sprintf("[exit code: %d]", exitCode))
	}
	if len(markers) == 0 {
		return body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + strings.Join(markers, "\n")
}
