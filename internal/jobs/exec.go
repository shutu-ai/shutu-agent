// exec.go — the job_* tools' external-command execution body (dispatch-m5a-2
// §2): job_start runs a command line through a single non-interactive shell
// invocation, exactly like tools.run_command but kept inside this package so
// the jobs seam never imports the tools package. Output goes to a temp file
// (not a pipe) so a lingering grandchild can never keep Wait blocked; context
// cancellation (Kill/Close) terminates the process tree and settles the job.
package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// sensitiveEnvTokens are the credential-shaped substrings removed from the
// environment before every job command (mirrors tools.run_command's scrub):
// the child process must never implicitly inherit the agent's keys, secrets,
// tokens, passwords, or API credentials.
var sensitiveEnvTokens = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}

// runCommandLine is the JobStart.Run body for a job_start command: it runs the
// line, captures merged output, and settles the job from the outcome. Context
// cancellation (Kill/Close) terminates the process tree and settles the job as
// killed; a non-zero exit settles it as failed with the exit code.
func runCommandLine(command string) func(ctx context.Context) (JobOutcome, error) {
	return func(ctx context.Context) (JobOutcome, error) {
		cmd := newShellCommand(command, "", scrubbedEnv())
		outFile, err := os.CreateTemp("", "pa-job-*.txt")
		if err != nil {
			return JobOutcome{}, fmt.Errorf("job: create output file: %w", err)
		}
		outPath := outFile.Name()
		defer outFile.Close()
		defer os.Remove(outPath)
		cmd.Stdout = outFile
		cmd.Stderr = outFile

		if err := cmd.Start(); err != nil {
			return JobOutcome{}, fmt.Errorf("job: start: %w", err)
		}
		// Terminate the process tree when the job context is done (Kill/Close).
		stop := monitorContext(ctx, cmd)
		waitErr := cmd.Wait()
		stop()

		out, readErr := os.ReadFile(outPath)
		if readErr != nil {
			return JobOutcome{}, fmt.Errorf("job: read output: %w", readErr)
		}
		out = []byte(strings.ToValidUTF8(string(out), "�"))
		if waitErr != nil {
			if ctx.Err() != nil {
				return JobOutcome{Status: StatusKilled, Detail: "cancelled"}, nil
			}
			if ee, ok := waitErr.(*exec.ExitError); ok {
				return JobOutcome{Status: StatusFailed, Detail: fmt.Sprintf("exit code: %d", ee.ExitCode()), Output: string(out)}, nil
			}
			return JobOutcome{}, fmt.Errorf("job: %w", waitErr)
		}
		return JobOutcome{Status: StatusCompleted, Detail: "exit code: 0", Output: string(out)}, nil
	}
}

// newShellCommand assembles a single-line, non-interactive shell invocation.
func newShellCommand(command, workdir string, env []string) *exec.Cmd {
	var argv []string
	if runtime.GOOS == "windows" {
		argv = []string{"cmd.exe", "/C", "chcp 65001 >nul & " + command}
	} else {
		argv = []string{"/bin/sh", "-c", command}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = env
	prepareJobProcessGroup(cmd)
	return cmd
}

// monitorContext terminates the command's process tree when ctx is done. The
// returned stop func must be called once Wait returns so the watcher goroutine
// does not leak.
func monitorContext(ctx context.Context, cmd *exec.Cmd) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killJobTree(cmd)
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
