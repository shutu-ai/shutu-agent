package main

import (
	"context"
	"errors"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/terminal"
)

const (
	acpTerminalStart  = "terminal_start"
	acpTerminalWrite  = "terminal_write"
	acpTerminalRead   = "terminal_read"
	acpTerminalSignal = "terminal_signal"
	acpTerminalStop   = "terminal_stop"
)

type acpTerminal struct {
	mu     sync.Mutex
	owner  string
	log    *session.Log
	opts   terminal.SessionOpts
	active *terminal.Session
}

func newACPCTerminal(cfg config.TerminalConfig, owner, cwd string, log *session.Log) *acpTerminal {
	return &acpTerminal{
		owner: owner,
		log:   log,
		opts: terminal.SessionOpts{
			Shell:              cfg.Shell,
			Args:               append([]string(nil), cfg.Args...),
			Workdir:            cwd,
			IdleMS:             cfg.ReadIdleMS,
			TimeoutMS:          cfg.ReadTimeoutMS,
			ScrollbackMaxBytes: cfg.ScrollbackMaxBytes,
			ScrollbackLines:    cfg.ScrollbackLines,
		},
	}
}

func (t *acpTerminal) Start(command string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active != nil {
		return "", fmt.Errorf("terminal session already active")
	}
	sess, err := terminal.NewSession(t.opts)
	if err != nil {
		return "", err
	}
	t.active = sess
	if t.log == nil {
		_ = sess.Close()
		t.active = nil
		return "", errors.New("terminal session log is unavailable")
	}
	if _, err := t.log.Append(session.EventTerminalStart, session.NewTerminalStart(sess.ID(), t.owner)); err != nil {
		_ = sess.Close()
		t.active = nil
		return "", fmt.Errorf("persist terminal start: %w", err)
	}
	if strings.TrimSpace(command) == "" {
		return fmt.Sprintf("started terminal session %s", sess.ID()), nil
	}
	res, err := sess.Write(command, true)
	if err != nil {
		_ = sess.Close()
		t.active = nil
		return "", err
	}
	return formatTerminalWrite(sess.ID(), res), nil
}

func (t *acpTerminal) Write(text string, submit bool) (string, error) {
	return t.WriteContext(context.Background(), text, submit)
}

// WriteContext propagates registry cancellation to the persistent terminal.
func (t *acpTerminal) WriteContext(ctx context.Context, text string, submit bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return "", fmt.Errorf("%w (call terminal_start first)", terminal.ErrNoActive)
	}
	res, err := t.active.WriteContext(ctx, text, submit)
	if err != nil {
		return "", err
	}
	return formatTerminalWrite(t.active.ID(), res), nil
}

func (t *acpTerminal) Read(offset, count int) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return "", fmt.Errorf("%w (call terminal_start first)", terminal.ErrNoActive)
	}
	text, truncated := t.active.Read(offset, count)
	if truncated {
		text += "\n[terminal output truncated or older lines omitted]"
	}
	return text, nil
}

func (t *acpTerminal) Signal(kind string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return fmt.Errorf("%w (call terminal_start first)", terminal.ErrNoActive)
	}
	return t.active.Signal(kind)
}

func (t *acpTerminal) Stop(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return nil
	}
	id := t.active.ID()
	err := t.active.Close()
	t.active = nil
	if t.log == nil {
		if err != nil {
			return err
		}
		return errors.New("terminal session log is unavailable")
	}
	if _, appendErr := t.log.Append(session.EventTerminalStop, session.NewTerminalStop(id, reason)); appendErr != nil {
		return errors.Join(err, fmt.Errorf("persist terminal stop: %w", appendErr))
	}
	return err
}

func (t *acpTerminal) Close() error { return t.Stop("session_close") }

func formatTerminalWrite(id string, res terminal.WriteResult) string {
	return fmt.Sprintf("terminal %s (%s):\n%s", id, res.Wait, res.Viewport)
}

type acpTerminalTool struct {
	service *acpTerminal
	name    string
}

func (t acpTerminalTool) Name() string { return t.name }

// CancellationAware only applies to terminal_write. Start/read/signal/stop
// either do not block on foreground command progress or own terminal receipts.
func (t acpTerminalTool) CancellationAware() bool { return t.name == acpTerminalWrite }

func (t acpTerminalTool) Description() string {
	switch t.name {
	case acpTerminalStart:
		return "start the session-owned persistent terminal; optionally run one command"
	case acpTerminalWrite:
		return "write text to the session-owned persistent terminal and return new output"
	case acpTerminalRead:
		return "read bounded scrollback from the session-owned persistent terminal"
	case acpTerminalSignal:
		return "send stop or interrupt to the session-owned persistent terminal"
	case acpTerminalStop:
		return "stop and release the session-owned persistent terminal"
	default:
		return "session-owned terminal operation"
	}
}

func (t acpTerminalTool) Schema() map[string]any {
	switch t.name {
	case acpTerminalStart:
		return map[string]any{"type": "object", "properties": map[string]any{
			"command": map[string]any{"type": "string"},
		}, "additionalProperties": false}
	case acpTerminalWrite:
		return map[string]any{"type": "object", "properties": map[string]any{
			"text":   map[string]any{"type": "string"},
			"submit": map[string]any{"type": "boolean"},
		}, "required": []string{"text"}, "additionalProperties": false}
	case acpTerminalRead:
		return map[string]any{"type": "object", "properties": map[string]any{
			"offset": map[string]any{"type": "integer", "minimum": 0},
			"count":  map[string]any{"type": "integer", "minimum": 1},
		}, "additionalProperties": false}
	case acpTerminalSignal:
		return map[string]any{"type": "object", "properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"stop", "interrupt"}},
		}, "required": []string{"kind"}, "additionalProperties": false}
	default:
		return map[string]any{"type": "object", "additionalProperties": false}
	}
}

func (t acpTerminalTool) Execute(ctx context.Context, args any) (string, error) {
	switch t.name {
	case acpTerminalStart:
		var p struct {
			Command string `json:"command"`
		}
		if err := agenttools.DecodeArgs(args, &p); err != nil {
			return "", err
		}
		return t.service.Start(p.Command)
	case acpTerminalWrite:
		var p struct {
			Text   string `json:"text"`
			Submit *bool  `json:"submit"`
		}
		if err := agenttools.DecodeArgs(args, &p); err != nil {
			return "", err
		}
		submit := true
		if p.Submit != nil {
			submit = *p.Submit
		}
		return t.service.WriteContext(ctx, p.Text, submit)
	case acpTerminalRead:
		var p struct {
			Offset int `json:"offset"`
			Count  int `json:"count"`
		}
		if err := agenttools.DecodeArgs(args, &p); err != nil {
			return "", err
		}
		if p.Count == 0 {
			p.Count = 500
		}
		return t.service.Read(p.Offset, p.Count)
	case acpTerminalSignal:
		var p struct {
			Kind string `json:"kind"`
		}
		if err := agenttools.DecodeArgs(args, &p); err != nil {
			return "", err
		}
		return "terminal signal sent", t.service.Signal(p.Kind)
	case acpTerminalStop:
		return "terminal stopped", t.service.Stop("tool_stop")
	default:
		return "", fmt.Errorf("unknown ACP terminal tool %q", t.name)
	}
}
