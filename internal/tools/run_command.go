package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
	Workdir string // fixed working directory; empty means the agent's own cwd
}

// NewRunCommand returns a RunCommand bound to a fixed working directory.
func NewRunCommand(workdir string) RunCommand { return RunCommand{Workdir: workdir} }

func (RunCommand) Name() string { return runCommandName }

func (RunCommand) Description() string {
	return "execute a single shell command line in a fixed working directory; " +
		"no interactive shell; a non-zero exit is reported as [exit code: N]; " +
		"credential-shaped environment variables are scrubbed before execution"
}

func (RunCommand) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "the single command line to execute",
			},
		},
		"required":             []string{"command"},
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
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("run_command: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("run_command: empty command")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("run_command: cancelled: %w", err)
	}
	cmd := newCommand(a.Command, t.Workdir, scrubbedEnv())
	outFile, err := os.CreateTemp("", "pa-run-*.txt")
	if err != nil {
		return "", fmt.Errorf("run_command: create output file: %w", err)
	}
	outPath := outFile.Name()
	defer outFile.Close()
	defer os.Remove(outPath)
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("run_command: start: %w", err)
	}
	// Interrupt the process when the context is done (Ctrl+C or the Execute
	// deadline): the direct child is killed, and its output file (not a pipe)
	// lets Wait return immediately.
	stop := monitorCtx(ctx, cmd)
	waitErr := cmd.Wait()
	stop()

	out, readErr := os.ReadFile(outPath)
	if readErr != nil {
		return "", fmt.Errorf("run_command: read output: %w", readErr)
	}
	out = []byte(strings.ToValidUTF8(string(out), "�"))
	if waitErr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("run_command: interrupted: %w", waitErr)
		}
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return formatExit(ee.ExitCode(), out), nil
		}
		return "", fmt.Errorf("run_command: %w", waitErr)
	}
	return string(out), nil
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

// formatExit renders a non-zero exit as a leading marker plus the output.
func formatExit(code int, out []byte) string {
	text := strings.TrimRight(string(out), "\n")
	return fmt.Sprintf("[exit code: %d]\n%s", code, text)
}
