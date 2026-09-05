package main

// dsh-compatible persistent terminal tools. These sessions are model-owned
// and owner-fenced; the existing /term session remains a separate human REPL
// seam for backward compatibility.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/pathsecure"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/terminal"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

const minimalPersistentShellType = "minimal-persistent-shell"

// persistentShellTool is the minimal-preset projection of DSH's
// tool-bash-persistent/tool-pwsh-persistent. One owner gets one long-lived
// interactive process; the model sees only the command argument.
type persistentShellTool struct {
	app         *app
	name        string
	shell       string
	description string
}

func (t persistentShellTool) Name() string        { return t.name }
func (t persistentShellTool) Description() string { return t.description }

func (persistentShellTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"command": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "The shell command to run.",
		},
	}, []string{"command"})
}

func (persistentShellTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}

// CancellationAware is explicit: context cancellation resets the persistent
// terminal and closes its owned process tree after interrupting foreground work.
func (persistentShellTool) CancellationAware() bool { return true }

func (t persistentShellTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := tools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", fmt.Errorf("%s: command must be a non-empty string", t.name)
	}
	rec, err := t.app.minimalPersistentShell(ctx, t.name, t.shell)
	if err != nil {
		return "", err
	}
	rec.mu.Lock()
	result, writeErr := rec.sess.WriteContext(ctx, persistentShellCommand(t.shell, in.Command), true)
	rec.mu.Unlock()
	if writeErr != nil {
		stopErr := t.app.removeModelTerminal(ctx, rec)
		if stopErr != nil {
			return "", fmt.Errorf("%s: %w; terminal reset: %v", t.name, writeErr, stopErr)
		}
		return "", fmt.Errorf("%s: %w", t.name, writeErr)
	}
	if result.Status.Kind == "exited" {
		if err := t.app.removeModelTerminal(ctx, rec); err != nil {
			return "", fmt.Errorf("%s: persist terminal stop: %w", t.name, err)
		}
		return persistentShellExitResult(result.Viewport, result.Status.ExitCode), nil
	}
	if result.Wait == terminal.WaitTimeout {
		if err := t.app.removeModelTerminal(ctx, rec); err != nil {
			return "", fmt.Errorf("%s: persist terminal stop: %w", t.name, err)
		}
		return "Your command timed out. Below is partial output:\n" + result.Viewport +
			"\nThe persistent " + t.name + " shell was reset; the next call starts from the workspace with a fresh current directory and environment.", nil
	}
	return persistentShellResult(result.Viewport), nil
}

func persistentShellCommand(shell, command string) string {
	nonceBytes := make([]byte, 8)
	_, _ = rand.Read(nonceBytes)
	nonce := fmt.Sprintf("%x-%d", nonceBytes, time.Now().UnixNano())
	start := "__SHUTU_PERSISTENT_START_" + nonce + "__"
	end := "__SHUTU_PERSISTENT_END_" + nonce + ":"
	if isPowerShell(shell) {
		body := strings.ReplaceAll(command, "`", "``")
		body = strings.ReplaceAll(body, "\"", "`\"")
		body = strings.ReplaceAll(body, "$", "`$")
		body = strings.ReplaceAll(body, "\r", "")
		body = strings.ReplaceAll(body, "\n", "`n")
		return fmt.Sprintf("Write-Output '%s'; $LASTEXITCODE = $null; $__ok = $false; try { Invoke-Expression \"%s\"; $__ok = $? } catch { $__ok = $false }; $__s = if ($null -ne $LASTEXITCODE) { [int]$LASTEXITCODE } elseif ($__ok) { 0 } else { 1 }; Write-Output ('%s' + $__s)", start, body, end)
	}
	quoted := strings.ReplaceAll(command, "\\", "\\\\")
	quoted = strings.ReplaceAll(quoted, "'", "\\'")
	quoted = strings.ReplaceAll(quoted, "\r", "\\r")
	quoted = strings.ReplaceAll(quoted, "\n", "\\n")
	return fmt.Sprintf("printf '%%s\\n' '%s'; eval -- $'%s'; __shutu_status=$?; printf '%%s%%s\\n' '%s' \"$__shutu_status\"", start, quoted, end)
}

func persistentShellResult(viewport string) string {
	start := strings.LastIndex(viewport, "__SHUTU_PERSISTENT_START_")
	if start < 0 {
		return viewport
	}
	lineEnd := strings.Index(viewport[start:], "\n")
	if lineEnd < 0 {
		return ""
	}
	contentStart := start + lineEnd + 1
	endRel := strings.Index(viewport[contentStart:], "__SHUTU_PERSISTENT_END_")
	if endRel < 0 {
		return strings.TrimSpace(viewport[contentStart:])
	}
	end := contentStart + endRel
	body := strings.TrimRight(viewport[contentStart:end], "\r\n")
	statusLine := viewport[end:]
	if newline := strings.Index(statusLine, "\n"); newline >= 0 {
		status := strings.TrimSpace(statusLine[:newline])
		if colon := strings.LastIndex(status, ":"); colon >= 0 {
			code := strings.TrimSpace(status[colon+1:])
			if code != "" && code != "0" {
				body += "\n[exit code: " + code + "]"
			}
		}
	}
	return body
}

func persistentShellExitResult(viewport string, code int) string {
	body := persistentShellResult(viewport)
	status := fmt.Sprintf("[shell exited: code %d]", code)
	if body == "" {
		body = status
	} else {
		body += "\n" + status
	}
	return body + "\nThe persistent shell was reset; the next call starts from the workspace with a fresh current directory and environment."
}

func isPowerShell(shell string) bool {
	base := strings.ToLower(filepath.Base(shell))
	return base == "pwsh" || base == "pwsh.exe" || base == "powershell" || base == "powershell.exe"
}

func (a *app) registerMinimalPersistentShell() error {
	shell := a.cfg.Terminal.Shell
	if runtime.GOOS == "windows" {
		if shell == "" {
			shell = "pwsh"
			if _, err := exec.LookPath(shell); err != nil {
				shell = "powershell.exe"
			}
		}
	} else if shell == "" {
		shell = "/bin/bash"
	}
	name := "bash"
	description := "Run commands in a persistent bash shell. State, including the current directory and exported environment variables, persists across calls for this agent."
	if runtime.GOOS == "windows" {
		name = "pwsh"
		description = "Run commands in a persistent PowerShell shell. State, including the current directory and exported environment variables, persists across calls for this agent."
	}
	a.modelTermMu.Lock()
	if a.modelTerms == nil {
		a.modelTerms = make(map[string]*modelTerminalRecord)
	}
	a.modelTermMu.Unlock()
	return a.reg.Register(persistentShellTool{app: a, name: name, shell: shell, description: description})
}

func (a *app) minimalPersistentShell(ctx context.Context, name, shell string) (*modelTerminalRecord, error) {
	owner := a.terminalOwner(ctx)
	cwd := a.sessionCWDFor(owner)
	a.modelTermMu.Lock()
	defer a.modelTermMu.Unlock()
	for _, rec := range a.modelTerms {
		if rec.owner == owner && rec.typ == minimalPersistentShellType && !rec.closing {
			return rec, nil
		}
	}
	sess, err := terminal.NewSession(terminal.SessionOpts{
		Shell: shell, Args: append([]string(nil), a.cfg.Terminal.Args...), Workdir: cwd,
		IdleMS: persistentShellIdleMS(a.cfg.Terminal.ReadIdleMS), TimeoutMS: a.cfg.Terminal.ReadTimeoutMS,
		ScrollbackMaxBytes: a.cfg.Terminal.ScrollbackMaxBytes, ScrollbackLines: a.cfg.Terminal.ScrollbackLines,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: start persistent shell: %w", name, err)
	}
	if err := a.appendModelTerminalEvent(ctx, session.EventTerminalStart, session.NewTerminalStart(sess.ID(), owner)); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("%s: persist terminal start: %w", name, err)
	}
	rec := &modelTerminalRecord{owner: owner, typ: minimalPersistentShellType, cwd: cwd, sess: sess}
	a.modelTerms[sess.ID()] = rec
	return rec, nil
}

func persistentShellIdleMS(configured int) int {
	// Windows PowerShell emits its startup banner asynchronously when stdin is
	// a redirected pipe. A 1.5-second quiet window gives the first command the
	// same readiness margin as DSH's terminal backend, even when a deployment
	// uses a very small interactive-terminal idle setting.
	if configured < 1500 {
		return 1500
	}
	return configured
}

func (a *app) removeModelTerminal(ctx context.Context, rec *modelTerminalRecord) error {
	return a.stopModelTerminal(ctx, rec, "shell_reset")
}

func (a *app) stopModelTerminal(ctx context.Context, rec *modelTerminalRecord, reason string) error {
	if rec == nil {
		return nil
	}
	a.modelTermMu.Lock()
	if current, ok := a.modelTerms[rec.sess.ID()]; ok && current == rec {
		delete(a.modelTerms, rec.sess.ID())
	}
	if rec.stopRecorded {
		a.modelTermMu.Unlock()
		return nil
	}
	rec.stopRecorded = true
	rec.closing = true
	a.modelTermMu.Unlock()
	rec.mu.Lock()
	closeErr := rec.sess.Close()
	rec.mu.Unlock()
	// The process has already been closed above. Keep the terminal lifecycle
	// auditable even when a command timeout/exit resets the minimal shell.
	appendErr := a.appendModelTerminalEvent(ctx, session.EventTerminalStop, session.NewTerminalStop(rec.sess.ID(), reason))
	if closeErr != nil && appendErr != nil {
		return fmt.Errorf("close: %v; persist stop: %w", closeErr, appendErr)
	}
	if closeErr != nil {
		return closeErr
	}
	return appendErr
}

type modelTerminalRecord struct {
	owner        string
	name         string
	typ          string
	cwd          string
	sess         *terminal.Session
	mu           sync.Mutex
	closing      bool
	stopRecorded bool
}

type modelTerminalTool struct {
	app  *app
	kind string
}

func (t modelTerminalTool) Name() string { return t.kind }

// CancellationAware only covers terminal_send. Other persistent terminal
// operations are non-blocking or own lifecycle receipts that must not be
// abandoned because the HTTP/Agent call context ended.
func (t modelTerminalTool) CancellationAware() bool { return t.kind == "terminal_send" }

func (t modelTerminalTool) Description() string {
	switch t.kind {
	case "terminal_open":
		return "open a persistent shell terminal session"
	case "terminal_list":
		return "list persistent terminal sessions owned by this agent"
	case "terminal_read":
		return "read scrollback from a persistent terminal session"
	case "terminal_send":
		return "send text to a persistent terminal session"
	case "terminal_signal":
		return "send a signal to a persistent terminal session"
	case "terminal_close":
		return "close a persistent terminal session"
	default:
		return "persistent terminal operation"
	}
}

func (t modelTerminalTool) Schema() map[string]any {
	stringProp := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	switch t.kind {
	case "terminal_open":
		return objectSchema(map[string]any{
			"type": stringProp("terminal backend type; use shell"),
			"name": stringProp("optional human-readable session name"),
			"cwd":  stringProp("optional working directory"),
		}, []string{"type"})
	case "terminal_list":
		return objectSchema(nil, nil)
	case "terminal_read":
		return objectSchema(map[string]any{
			"sessionId": stringProp("terminal session id"),
			"offset":    map[string]any{"type": "integer", "minimum": 0},
			"count":     map[string]any{"type": "integer", "minimum": 0},
		}, []string{"sessionId"})
	case "terminal_send":
		return objectSchema(map[string]any{
			"sessionId":         stringProp("terminal session id"),
			"text":              stringProp("text to write to the terminal"),
			"submit":            map[string]any{"type": "boolean"},
			"run_in_background": map[string]any{"type": "boolean"},
		}, []string{"sessionId", "text"})
	case "terminal_signal":
		return objectSchema(map[string]any{
			"sessionId": stringProp("terminal session id"),
			"signal": map[string]any{
				"type": "string",
				"enum": []string{"SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP", "SIGHUP"},
			},
		}, []string{"sessionId", "signal"})
	case "terminal_close":
		return objectSchema(map[string]any{"sessionId": stringProp("terminal session id")}, []string{"sessionId"})
	default:
		return objectSchema(nil, nil)
	}
}

func (t modelTerminalTool) OutputSchema() map[string]any {
	status := map[string]any{
		"oneOf": []any{
			objectSchema(map[string]any{"kind": map[string]any{"const": "running"}}, []string{"kind"}),
			objectSchema(map[string]any{
				"kind":     map[string]any{"const": "exited"},
				"exitCode": map[string]any{"type": []string{"integer", "null"}},
				"signal":   map[string]any{"type": []string{"string", "null"}},
			}, []string{"kind", "exitCode", "signal"}),
		},
	}
	snapshot := objectSchema(map[string]any{
		"sessionId": stringSchema(),
		"name":      map[string]any{"type": []string{"string", "null"}},
		"type":      stringSchema(),
		"pid":       map[string]any{"type": "integer"},
		"status":    status,
	}, []string{"sessionId", "type", "status"})
	switch t.kind {
	case "terminal_open":
		return objectSchema(map[string]any{
			"sessionId": stringSchema(), "name": map[string]any{"type": []string{"string", "null"}},
			"type": stringSchema(), "pid": map[string]any{"type": "integer"}, "status": status,
			"motd": stringSchema(),
		}, []string{"sessionId", "type", "status", "motd"})
	case "terminal_list":
		return map[string]any{"type": "array", "items": snapshot}
	case "terminal_read":
		return objectSchema(map[string]any{
			"text": stringSchema(), "totalLines": map[string]any{"type": "integer"},
			"lineBegin": map[string]any{"type": "integer"}, "lineEnd": map[string]any{"type": "integer"},
			"truncated": map[string]any{"type": "boolean"},
		}, []string{"text", "totalLines", "lineBegin", "lineEnd", "truncated"})
	case "terminal_send":
		return map[string]any{"oneOf": []any{
			objectSchema(map[string]any{"kind": map[string]any{"const": "background"}, "jobId": stringSchema()}, []string{"kind", "jobId"}),
			objectSchema(map[string]any{
				"kind": map[string]any{"const": "foreground"}, "viewport": stringSchema(),
				"waitReason":    map[string]any{"type": "string", "enum": []string{"stdin_read", "inferred_idle", "timeout", "session_exit"}},
				"sessionStatus": status, "truncated": map[string]any{"type": "boolean"},
			}, []string{"kind", "viewport", "waitReason", "sessionStatus", "truncated"}),
		}}
	case "terminal_signal":
		return objectSchema(map[string]any{"delivered": map[string]any{"const": true}, "targetPgid": map[string]any{"type": "integer"}}, []string{"delivered", "targetPgid"})
	case "terminal_close":
		return objectSchema(map[string]any{"sessionId": stringSchema(), "outcome": map[string]any{"type": "string", "enum": []string{"closed", "already-closing"}}}, []string{"sessionId", "outcome"})
	default:
		return objectSchema(nil, nil)
	}
}

func (t modelTerminalTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(result.Value)
	return string(b), err
}

func (t modelTerminalTool) ExecuteResult(ctx context.Context, args any) (tools.ToolResult, error) {
	value, err := t.app.executeModelTerminal(ctx, t.kind, args)
	if err != nil {
		return tools.ToolResult{}, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Value: value, Output: string(b)}, nil
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }

func (a *app) registerModelTerminalTools() error {
	if a.modelTerms == nil {
		a.modelTerms = make(map[string]*modelTerminalRecord)
	}
	for _, name := range []string{"terminal_open", "terminal_list", "terminal_read", "terminal_send", "terminal_signal", "terminal_close"} {
		if err := a.reg.Register(modelTerminalTool{app: a, kind: name}); err != nil {
			return fmt.Errorf("sta: register %s: %w", name, err)
		}
	}
	return nil
}

func (a *app) currentModelTerminal(id string) (*modelTerminalRecord, error) {
	return a.currentModelTerminalFor(a.currentID, id)
}

func (a *app) currentModelTerminalFor(owner, id string) (*modelTerminalRecord, error) {
	a.modelTermMu.Lock()
	rec, ok := a.modelTerms[id]
	if ok && (rec == nil || rec.owner != owner || rec.closing) {
		ok = false
	}
	a.modelTermMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal session %q not found", id)
	}
	return rec, nil
}

func (a *app) terminalOwner(ctx context.Context) string {
	if id := runtimectx.SessionID(ctx); id != "" {
		return id
	}
	if a.agentRegistry != nil {
		return ""
	}
	return a.currentID
}

func (a *app) executeModelTerminal(ctx context.Context, kind string, args any) (any, error) {
	owner := a.terminalOwner(ctx)
	switch kind {
	case "terminal_open":
		var in struct{ Type, Name, Cwd string }
		if err := tools.DecodeArgs(args, &in); err != nil {
			return nil, err
		}
		if in.Type != "shell" {
			return nil, fmt.Errorf("terminal_open: unsupported type %q", in.Type)
		}
		cwd, err := a.resolveModelTerminalCWD(owner, in.Cwd)
		if err != nil {
			return nil, err
		}
		sess, err := terminal.NewSession(terminal.SessionOpts{Shell: a.cfg.Terminal.Shell, Args: a.cfg.Terminal.Args, Workdir: cwd, IdleMS: persistentShellIdleMS(a.cfg.Terminal.ReadIdleMS), TimeoutMS: a.cfg.Terminal.ReadTimeoutMS, ScrollbackMaxBytes: a.cfg.Terminal.ScrollbackMaxBytes, ScrollbackLines: a.cfg.Terminal.ScrollbackLines})
		if err != nil {
			return nil, err
		}
		if err := a.appendModelTerminalEvent(ctx, session.EventTerminalStart, session.NewTerminalStart(sess.ID(), owner)); err != nil {
			_ = sess.Close()
			return nil, fmt.Errorf("terminal_open: persist terminal start: %w", err)
		}
		rec := &modelTerminalRecord{owner: owner, name: in.Name, typ: in.Type, cwd: cwd, sess: sess}
		a.modelTermMu.Lock()
		a.modelTerms[sess.ID()] = rec
		a.modelTermMu.Unlock()
		return a.modelTerminalOpenValue(rec), nil
	case "terminal_list":
		if _, err := tools.ParseArguments(args); err != nil {
			return nil, err
		}
		a.modelTermMu.Lock()
		ids := make([]string, 0, len(a.modelTerms))
		for id, rec := range a.modelTerms {
			if rec.owner == owner && !rec.closing {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, a.modelTerminalSnapshot(a.modelTerms[id]))
		}
		a.modelTermMu.Unlock()
		return out, nil
	case "terminal_read":
		var in struct {
			SessionID     string `json:"sessionId"`
			Offset, Count int
		}
		if err := tools.DecodeArgs(args, &in); err != nil {
			return nil, err
		}
		if in.Count == 0 {
			in.Count = 500
		}
		rec, err := a.currentModelTerminalFor(owner, in.SessionID)
		if err != nil {
			return nil, err
		}
		rec.mu.Lock()
		text, total, lineBegin, lineEnd, truncated := rec.sess.ReadWindow(in.Offset, in.Count)
		rec.mu.Unlock()
		return map[string]any{"text": text, "totalLines": total, "lineBegin": lineBegin, "lineEnd": lineEnd, "truncated": truncated}, nil
	case "terminal_send":
		var in struct {
			SessionID, Text string
			Submit          *bool
			RunInBackground bool `json:"run_in_background"`
		}
		if err := tools.DecodeArgs(args, &in); err != nil {
			return nil, err
		}
		rec, err := a.currentModelTerminalFor(owner, in.SessionID)
		if err != nil {
			return nil, err
		}
		submit := true
		if in.Submit != nil {
			submit = *in.Submit
		}
		if in.RunInBackground {
			if a.jobs == nil {
				return nil, fmt.Errorf("terminal_send: jobs are disabled")
			}
			id, err := a.jobs.Start(ctx, jobs.JobStart{Kind: jobs.Kind("pty-send"), Label: in.SessionID + ": terminal input", OwnerSession: owner, OutputLimitBytes: 256 * 1024,
				Correlation: jobs.CorrelationFromContext(ctx),
				Run: func(jobCtx context.Context) (jobs.JobOutcome, error) {
					rec.mu.Lock()
					defer rec.mu.Unlock()
					res, err := rec.sess.WriteContext(jobCtx, in.Text, submit)
					if err != nil {
						return jobs.JobOutcome{}, err
					}
					return jobs.JobOutcome{Status: jobs.StatusCompleted, Output: res.Viewport}, nil
				},
				Cancel: func(string) error { return rec.sess.Signal("interrupt") },
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"kind": "background", "jobId": id}, nil
		}
		rec.mu.Lock()
		res, err := rec.sess.WriteContext(ctx, in.Text, submit)
		rec.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "foreground", "viewport": res.Viewport, "waitReason": modelWaitReason(res.Wait), "sessionStatus": modelSessionStatus(res.Status), "truncated": res.Truncated}, nil
	case "terminal_signal":
		var in struct{ SessionID, Signal string }
		if err := tools.DecodeArgs(args, &in); err != nil {
			return nil, err
		}
		rec, err := a.currentModelTerminalFor(owner, in.SessionID)
		if err != nil {
			return nil, err
		}
		rec.mu.Lock()
		switch in.Signal {
		case "SIGINT":
			err = rec.sess.Signal("interrupt")
		case "SIGTERM", "SIGHUP":
			rec.mu.Unlock()
			if err := a.stopModelTerminal(ctx, rec, "signal"); err != nil {
				return nil, fmt.Errorf("terminal_signal: %w", err)
			}
			return map[string]any{"delivered": true, "targetPgid": rec.sess.PID()}, nil
		case "SIGKILL":
			rec.mu.Unlock()
			return nil, fmt.Errorf("terminal_signal: shell-targeted SIGKILL is rejected; use terminal_close")
		default:
			err = fmt.Errorf("terminal_signal: unsupported signal %q", in.Signal)
		}
		rec.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return map[string]any{"delivered": true, "targetPgid": rec.sess.PID()}, nil
	case "terminal_close":
		var in struct {
			SessionID string `json:"sessionId"`
		}
		if err := tools.DecodeArgs(args, &in); err != nil {
			return nil, err
		}
		a.modelTermMu.Lock()
		rec, ok := a.modelTerms[in.SessionID]
		if !ok || rec.owner != owner {
			a.modelTermMu.Unlock()
			return nil, fmt.Errorf("terminal session %q not found", in.SessionID)
		}
		if rec.closing {
			a.modelTermMu.Unlock()
			return map[string]any{"sessionId": in.SessionID, "outcome": "already-closing"}, nil
		}
		a.modelTermMu.Unlock()
		if err := a.stopModelTerminal(ctx, rec, "tool_close"); err != nil {
			return nil, fmt.Errorf("terminal_close: %w", err)
		}
		return map[string]any{"sessionId": in.SessionID, "outcome": "closed"}, nil
	default:
		return nil, fmt.Errorf("unknown terminal tool %q", kind)
	}
}

// resolveModelTerminalCWD makes the session workspace an actual authority
// boundary for model-owned persistent terminals. A lexical relative-path
// check is not enough: an in-workspace symlink can otherwise redirect the
// shell outside the session's workspace.
func (a *app) resolveModelTerminalCWD(owner, requested string) (string, error) {
	root := strings.TrimSpace(a.sessionCWDFor(owner))
	if root == "" {
		return "", fmt.Errorf("terminal_open: session workspace is unavailable")
	}
	root, err := pathsecure.ResolveExisting(root)
	if err != nil {
		return "", fmt.Errorf("terminal_open: resolve workspace: %w", err)
	}
	cwd := strings.TrimSpace(requested)
	if cwd == "" {
		cwd = root
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	cwd, err = pathsecure.ResolveExisting(cwd)
	if err != nil {
		return "", fmt.Errorf("terminal_open: resolve cwd: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("terminal_open: cwd is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("terminal_open: cwd is not a directory")
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("terminal_open: cwd %q escapes session workspace", requested)
	}
	return filepath.Clean(cwd), nil
}

// appendModelTerminalEvent uses the addressed Agent sink when present and
// only falls back to the legacy current-session log for direct CLI/tests.
// Unlike a best-effort observer, a failed lifecycle append rejects the tool
// operation so a terminal cannot become invisible to durable replay.
func (a *app) appendModelTerminalEvent(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok {
		if runtime.Emit == nil {
			return fmt.Errorf("terminal: durable runtime sink is unavailable")
		}
		return runtime.Emit(typ, data)
	}
	log := a.runtimeLog(ctx)
	if log == nil {
		return fmt.Errorf("terminal: session log is unavailable")
	}
	_, err := log.Append(typ, data)
	return err
}

// recoverTerminalClaims releases terminal/start claims left open by a prior
// process before their owner can materialize. The receipt is the authority
// for whether a terminal is addressable; this cold pass cannot resurrect or
// inspect the old process, so it appends exactly one `process_restart` stop
// edge and never touches terminals already live in this process.
func (a *app) recoverTerminalClaims(log *session.Log, owner string) error {
	if log == nil {
		return errors.New("terminal: recovery log is unavailable")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil
	}
	a.modelTermMu.Lock()
	for _, rec := range a.modelTerms {
		if rec != nil && rec.owner == owner && !rec.closing {
			a.modelTermMu.Unlock()
			return nil
		}
	}
	a.modelTermMu.Unlock()

	events := log.Events()
	open := make(map[string]bool)
	order := make([]string, 0, len(events))
	var firstErr error
	for _, event := range events {
		// Session logs mix audit-only facts such as schedule/change with
		// terminal lifecycle records. Only terminal records participate in
		// owner close; unfiltered parsing would make any unrelated id-less
		// event wedge Agent disposal.
		if event.Type != session.EventTerminalStart && event.Type != session.EventTerminalStop {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode terminal %s event %d: %w", event.Type, event.Seq, err)
			}
			continue
		}
		id := strings.TrimSpace(payload.ID)
		if id == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("terminal %s event %d has no id", event.Type, event.Seq)
			}
			continue
		}
		switch event.Type {
		case session.EventTerminalStart:
			if _, seen := open[id]; !seen {
				order = append(order, id)
			}
			open[id] = true
		case session.EventTerminalStop:
			open[id] = false
		}
	}
	if firstErr != nil {
		return firstErr
	}
	for _, id := range order {
		if !open[id] {
			continue
		}
		if _, err := log.Append(session.EventTerminalStop, session.NewTerminalStop(id, "process_restart")); err != nil {
			return fmt.Errorf("terminal: release stale claim %s: %w", id, err)
		}
	}
	return nil
}

func (a *app) modelTerminalOpenValue(rec *modelTerminalRecord) map[string]any {
	value := map[string]any{"sessionId": rec.sess.ID(), "type": rec.typ, "status": modelSessionStatus(rec.sess.Status()), "motd": ""}
	if rec.name != "" {
		value["name"] = rec.name
	}
	if pid := rec.sess.PID(); pid > 0 {
		value["pid"] = pid
	}
	return value
}

func (a *app) modelTerminalSnapshot(rec *modelTerminalRecord) map[string]any {
	value := map[string]any{"sessionId": rec.sess.ID(), "type": rec.typ, "status": modelSessionStatus(rec.sess.Status())}
	if rec.name != "" {
		value["name"] = rec.name
	}
	if pid := rec.sess.PID(); pid > 0 {
		value["pid"] = pid
	}
	return value
}

func modelSessionStatus(status terminal.SessionStatus) map[string]any {
	if status.Kind == "exited" {
		return map[string]any{"kind": "exited", "exitCode": status.ExitCode, "signal": nil}
	}
	return map[string]any{"kind": "running"}
}

func modelWaitReason(reason terminal.WaitReason) string {
	if reason == terminal.WaitStdinRead {
		return "inferred_idle"
	}
	return string(reason)
}

func (a *app) closeModelTerminalSessions() {
	a.modelTermMu.Lock()
	owners := make([]string, 0, len(a.modelTerms))
	seen := make(map[string]struct{})
	for _, rec := range a.modelTerms {
		if rec != nil && rec.owner != "" {
			if _, ok := seen[rec.owner]; !ok {
				seen[rec.owner] = struct{}{}
				owners = append(owners, rec.owner)
			}
		}
	}
	a.modelTermMu.Unlock()
	for _, owner := range owners {
		_ = a.closeModelTerminalOwner(owner)
	}
	// Unowned terminals are only possible through legacy embedders. Keep the
	// process shutdown path responsible for those records as well.
	a.modelTermMu.Lock()
	legacy := make([]*modelTerminalRecord, 0)
	for _, rec := range a.modelTerms {
		if rec != nil && rec.owner == "" && !rec.closing {
			rec.closing = true
			legacy = append(legacy, rec)
		}
	}
	a.modelTermMu.Unlock()
	for _, rec := range legacy {
		id, owner := rec.sess.ID(), rec.owner
		rec.mu.Lock()
		_ = rec.sess.Close()
		rec.mu.Unlock()
		// Shutdown is still part of the durable lifecycle. Resolve the owner's
		// log explicitly; using runtimeLog with a background context here could
		// accidentally append a child session's stop to the REPL current log.
		if log, err := a.sessionLogForAgent(context.Background(), owner); err == nil && log != nil {
			_, _ = log.Append(session.EventTerminalStop, session.NewTerminalStop(id, "process_shutdown"))
		}
	}
}

// closeModelTerminalOwner is the Agent-scope terminal disposer. Persistent
// model terminals are owned by the session that opened them; process-wide
// shutdown alone is too late because a Web/ACP session can be closed and
// recreated while the host remains alive.
func (a *app) closeModelTerminalOwner(owner string) error {
	if strings.TrimSpace(owner) == "" {
		return nil
	}
	a.modelTermStopMu.Lock()
	defer a.modelTermStopMu.Unlock()
	a.modelTermMu.Lock()
	records := make([]*modelTerminalRecord, 0)
	for _, rec := range a.modelTerms {
		if rec != nil && rec.owner == owner && !rec.stopRecorded {
			rec.closing = true
			records = append(records, rec)
		}
	}
	a.modelTermMu.Unlock()
	var first error
	for _, rec := range records {
		rec.mu.Lock()
		err := rec.sess.Close()
		rec.mu.Unlock()
		if err != nil && first == nil {
			first = err
		}
		var receiptErr error
		if log, logErr := a.sessionLogForAgent(context.Background(), owner); logErr == nil && log != nil {
			_, receiptErr = log.Append(session.EventTerminalStop, session.NewTerminalStop(rec.sess.ID(), "agent_scope_close"))
		} else if first == nil && logErr != nil {
			receiptErr = logErr
		}
		if receiptErr == nil {
			a.modelTermMu.Lock()
			rec.stopRecorded = true
			if current, ok := a.modelTerms[rec.sess.ID()]; ok && current == rec {
				delete(a.modelTerms, rec.sess.ID())
			}
			a.modelTermMu.Unlock()
		} else if first == nil {
			first = fmt.Errorf("terminal: persist stop %s: %w", rec.sess.ID(), receiptErr)
		}
	}
	return first
}
