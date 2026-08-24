package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/terminal"
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
	_, _ = t.log.Append(session.EventTerminalStart, session.NewTerminalStart(sess.ID(), t.owner))
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
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return "", fmt.Errorf("%w (call terminal_start first)", terminal.ErrNoActive)
	}
	res, err := t.active.Write(text, submit)
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
	_, _ = t.log.Append(session.EventTerminalStop, session.NewTerminalStop(id, reason))
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

func (t acpTerminalTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	switch t.name {
	case acpTerminalStart:
		var p struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", err
		}
		return t.service.Start(p.Command)
	case acpTerminalWrite:
		var p struct {
			Text   string `json:"text"`
			Submit *bool  `json:"submit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", err
		}
		submit := true
		if p.Submit != nil {
			submit = *p.Submit
		}
		return t.service.Write(p.Text, submit)
	case acpTerminalRead:
		var p struct {
			Offset int `json:"offset"`
			Count  int `json:"count"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
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
		if err := json.Unmarshal(args, &p); err != nil {
			return "", err
		}
		return "terminal signal sent", t.service.Signal(p.Kind)
	case acpTerminalStop:
		return "terminal stopped", t.service.Stop("tool_stop")
	default:
		return "", fmt.Errorf("unknown ACP terminal tool %q", t.name)
	}
}
