// exec.go — the job_* tools' external-command execution body (dispatch-m5a-2
// §2): job_start runs a command line through a single non-interactive shell
// invocation, exactly like tools.run_command but kept inside this package so
// the jobs seam never imports the tools package. Output is drained through
// bounded pipes; context cancellation (Kill/Close) terminates the process tree
// and settles the job.
package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const defaultJobOutputLimit = 64 * 1024

// sensitiveEnvTokens are the credential-shaped substrings removed from the
// environment before every job command (mirrors tools.run_command's scrub):
// the child process must never implicitly inherit the agent's keys, secrets,
// tokens, passwords, or API credentials.
var sensitiveEnvTokens = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}

// runCommandLine is retained as a compatibility wrapper for package-local
// embedders; production JobStart wiring uses the bounded implementation below.
func runCommandLine(command, workdir string) func(ctx context.Context) (JobOutcome, error) {
	/*
			return func(ctx context.Context) (JobOutcome, error) {
				cmd := newShellCommand(command, workdir, scrubbedEnv())
				outFile, err := os.CreateTemp("", "sta-job-*.txt")
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

	*/
	return runCommandLineBounded(command, workdir)
}

// runCommandLineBounded is the production JobStart executor. Both streams are
// drained concurrently into one synchronized bounded capture, so noisy
// commands cannot block on pipes or grow a temporary file without limit.
func runCommandLineBounded(command, workdir string) func(ctx context.Context) (JobOutcome, error) {
	return func(ctx context.Context) (JobOutcome, error) {
		cmd := newShellCommand(command, workdir, scrubbedEnv())
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return JobOutcome{}, fmt.Errorf("job: create stdout pipe: %w", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return JobOutcome{}, fmt.Errorf("job: create stderr pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return JobOutcome{}, fmt.Errorf("job: start: %w", err)
		}
		attachJobProcessGroup(cmd)
		defer releaseJobProcessGroup(cmd)
		capture := &boundedOutput{limit: defaultJobOutputLimit}
		copyDone := make(chan error, 2)
		go func() { _, copyErr := io.Copy(capture, stdout); copyDone <- copyErr }()
		go func() { _, copyErr := io.Copy(capture, stderr); copyDone <- copyErr }()
		stop := monitorContext(ctx, cmd)
		waitErr := cmd.Wait()
		stop()
		copyErr1 := <-copyDone
		copyErr2 := <-copyDone
		if copyErr1 != nil && ctx.Err() == nil {
			return JobOutcome{}, fmt.Errorf("job: read stdout: %w", copyErr1)
		}
		if copyErr2 != nil && ctx.Err() == nil {
			return JobOutcome{}, fmt.Errorf("job: read stderr: %w", copyErr2)
		}
		out := []byte(strings.ToValidUTF8(capture.String(), "?"))
		if capture.truncated {
			out = append(out, []byte("\n[output truncated]")...)
		}
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

type boundedOutput struct {
	mu sync.Mutex
	bytes.Buffer
	limit     int
	truncated bool
}

func (o *boundedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.limit <= o.Len() {
		o.truncated = true
		return len(p), nil
	}
	remaining := o.limit - o.Len()
	if len(p) > remaining {
		if _, err := o.Buffer.Write(p[:remaining]); err != nil {
			return 0, err
		}
		o.truncated = true
		return len(p), nil
	}
	return o.Buffer.Write(p)
}

func (o *boundedOutput) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			written, writeErr := o.Write(buf[:n])
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
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			killJobTree(cmd)
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
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
