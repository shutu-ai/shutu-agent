// terminal.go — the M9-2b persistent-terminal seam (dispatch-m9-2 §4). This is
// where the terminal capability is wired into the REPL: registerTerminal
// builds the terminal.TerminalAccess over the app's single active session,
// registers the single pwsh tool (dsh: the persistent shell runs commands in
// the configured shell — Pwsh / Git Bash / WSL / Cmd), and wires the D3 event
// sink so terminal/start and terminal/stop are appended to the active session
// log. config.applyDefaults already whitelisted pwsh when terminal.enabled was
// true. The single active session (D5) is closed at shutdown by main's
// deferred cleanup. The loop's turn/step structure is untouched (D4): the
// shell runs as a child process and is observed only through the serial tool
// path.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/terminal"
)

// registerTerminal wires the M9 persistent-terminal seam when terminal.enabled
// (默认关 D10): it builds the terminal.TerminalAccess over the app's single
// active session, registers the pwsh tool into the registry, and wires the D3
// event sink so terminal/start and terminal/stop are appended to the active
// session log. config.applyDefaults already whitelisted pwsh when
// terminal.enabled was true. The single active session (D5) is closed at
// shutdown by main's deferred cleanup. The loop's turn/step structure is
// untouched (D4): the shell runs as a child process and is observed only
// through the serial tool path.
func (a *app) registerTerminal() error {
	if !config.Enabled(a.cfg.Terminal.Enabled) {
		return nil
	}
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	tt := terminal.NewTerminalTools(&terminalAccess{a: a}, onEvent)
	a.termTools = tt
	if err := a.reg.Register(tt.Pwsh()); err != nil {
		return fmt.Errorf("pa: register %s: %w", tt.Pwsh().Name(), err)
	}
	return nil
}

// terminalAccess adapts the app to the terminal seam's accessor: it owns the
// single active session (D5) and fences every access by owner session id.
type terminalAccess struct{ a *app }

func (ac *terminalAccess) Owner() string { return ac.a.currentID }

func (ac *terminalAccess) GetActive() (*terminal.Session, error) {
	if ac.a.termSess == nil {
		return nil, fmt.Errorf("%w (start one with a pwsh call)", terminal.ErrNoActive)
	}
	if ac.a.termOwner != ac.a.currentID {
		return nil, fmt.Errorf("terminal session belongs to another session (owner=%s)", ac.a.termOwner)
	}
	return ac.a.termSess, nil
}

// Start creates the single active session from config defaults.
func (ac *terminalAccess) Start(opts terminal.SessionOpts) (*terminal.Session, error) {
	if ac.a.termSess != nil {
		return nil, fmt.Errorf("already active terminal session")
	}
	opts = terminal.SessionOpts{
		Shell:              ac.a.cfg.Terminal.Shell,
		Args:               ac.a.cfg.Terminal.Args,
		Workdir:            ac.a.cfg.Terminal.Workdir,
		IdleMS:             ac.a.cfg.Terminal.ReadIdleMS,
		TimeoutMS:          ac.a.cfg.Terminal.ReadTimeoutMS,
		ScrollbackMaxBytes: ac.a.cfg.Terminal.ScrollbackMaxBytes,
		ScrollbackLines:    ac.a.cfg.Terminal.ScrollbackLines,
	}
	sess, err := terminal.NewSession(opts)
	if err != nil {
		return nil, err
	}
	ac.a.termSess = sess
	ac.a.termOwner = ac.a.currentID
	return sess, nil
}

// Stop closes and detaches the active session (idempotent).
func (ac *terminalAccess) Stop() error {
	if ac.a.termSess == nil {
		return fmt.Errorf("no active terminal session")
	}
	err := ac.a.termSess.Close()
	ac.a.termSess = nil
	ac.a.termOwner = ""
	return err
}

// termCommand implements the /term REPL command group over the same accessor
// the pwsh tool uses (single source of truth for the session semantics).
func (a *app) termCommand(ctx context.Context, args []string) error {
	if a.termTools == nil {
		return fmt.Errorf("terminal disabled (terminal.enabled=false)")
	}
	acc := &terminalAccess{a: a}
	if len(args) == 0 {
		return fmt.Errorf("usage: /term <start [command] | write <text> | read [offset count] | signal <stop|interrupt> | stop>")
	}
	switch args[0] {
	case "start":
		sess, err := acc.Start(terminal.SessionOpts{})
		if err != nil {
			return err
		}
		msg := "started terminal session " + sess.ID()
		if len(args) > 1 {
			res, err := sess.Write(strings.Join(args[1:], " "), true)
			if err != nil {
				return err
			}
			msg += "\nviewport:\n" + res.Viewport
		}
		fmt.Println("term:", msg)
		return nil
	case "write":
		if len(args) < 2 {
			return fmt.Errorf("usage: /term write <text>")
		}
		sess, err := acc.GetActive()
		if err != nil {
			return err
		}
		res, err := sess.Write(strings.Join(args[1:], " "), true)
		if err != nil {
			return err
		}
		fmt.Println("term:", res.Viewport)
		return nil
	case "read":
		offset, count := 0, 500
		if len(args) > 1 {
			if _, err := fmt.Sscanf(args[1], "%d", &offset); err != nil {
				return fmt.Errorf("invalid offset %q", args[1])
			}
		}
		if len(args) > 2 {
			if _, err := fmt.Sscanf(args[2], "%d", &count); err != nil {
				return fmt.Errorf("invalid count %q", args[2])
			}
		}
		sess, err := acc.GetActive()
		if err != nil {
			return err
		}
		text, _ := sess.Read(offset, count)
		fmt.Println("term:", text)
		return nil
	case "signal":
		if len(args) < 2 {
			return fmt.Errorf("usage: /term signal <stop|interrupt>")
		}
		sess, err := acc.GetActive()
		if err != nil {
			return err
		}
		return sess.Signal(args[1])
	case "stop":
		return acc.Stop()
	default:
		return fmt.Errorf("unknown /term subcommand %q (try: start|write|read|signal|stop)", args[0])
	}
}
