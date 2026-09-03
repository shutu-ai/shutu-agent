// terminal.go — the dsh-aligned pwsh tool + the M9 /term REPL seam
// (dispatch-m9-2 §4). registerTerminal registers the FRESH-PROCESS pwsh tool
// (dsh tool-pwsh: one `pwsh -NoLogo -NoProfile -NonInteractive -Command`
// call per tool call — no state persists between calls; workdir/timeoutMs/
// run_in_background follow dsh) and keeps the terminal.TerminalAccess over
// the app's single active session for the /term REPL command (the M9
// persistent session stays a user-driven interactive seam — the model tool
// never touches it). config.applyDefaults already whitelisted pwsh when
// terminal.enabled was true. The single active session (D5) is closed at
// shutdown by main's deferred cleanup. The loop's turn/step structure is
// untouched (D4): the pwsh subprocess is a child process observed only
// through the serial tool path.
package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/terminal"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// registerTerminal wires the pwsh tool and the /term REPL seam when
// terminal.enabled (默认开, dsh 对齐 opt-out): it registers the fresh-process
// pwsh tool into the registry — background execution is available exactly
// when jobs.enabled (the registry is passed through; run_in_background is
// otherwise not advertised). The M9 session seam needs no wiring here: /term
// builds the accessor inline over the app's single active session.
func (a *app) registerTerminal() error {
	if !config.Enabled(a.cfg.Terminal.Enabled) {
		return nil
	}
	if a.cfg.Mode == config.ModeMinimal {
		return a.registerMinimalPersistentShell()
	}
	if runtime.GOOS != "windows" {
		bash := tools.NewRunCommandForWorkdirAndJobs(a.sessionCWD, func() jobs.Registry {
			return a.jobs
		}, func() string { return a.currentID }, a.jobs != nil)
		bash.WorkdirContextFunc = func(ctx context.Context) string { return a.sessionCWDFor(runtimectx.SessionID(ctx)) }
		bash.WorkdirRootContextFunc = func(ctx context.Context) string { return a.sessionCWDFor(runtimectx.SessionID(ctx)) }
		bash.JobsContextFunc = func(context.Context) jobs.Registry { return a.jobs }
		bash.OwnerContextFunc = func(ctx context.Context) string {
			return a.runtimeSessionID(ctx)
		}
		bash.DshEnvContextFunc = func(ctx context.Context) map[string]string {
			return tools.NewManagedDshEnv(a.cfg.DataDir, func() string {
				return runtimectx.SessionID(ctx)
			})()
		}
		if err := a.reg.Register(bash); err != nil {
			return fmt.Errorf("sta: register %s: %w", bash.Name(), err)
		}
		return a.registerModelTerminalTools()
	}
	pwsh := tools.NewPwsh(tools.PwshOpts{
		Workdir: a.cfg.Terminal.Workdir, // default working dir (session workspace)
		WorkdirFunc: func() string {
			if a.cfg.Terminal.Workdir == "" {
				return a.sessionCWD()
			}
			return a.cfg.Terminal.Workdir
		},
		WorkdirContextFunc: func(ctx context.Context) string {
			if a.cfg.Terminal.Workdir != "" {
				return a.cfg.Terminal.Workdir
			}
			return a.sessionCWDFor(runtimectx.SessionID(ctx))
		},
		WorkdirRootContextFunc: func(ctx context.Context) string {
			return a.sessionCWDFor(runtimectx.SessionID(ctx))
		},
		Jobs:  a.jobs, // nil when jobs disabled → no run_in_background
		Owner: func() string { return a.currentID },
		OwnerContextFunc: func(ctx context.Context) string {
			return a.runtimeSessionID(ctx)
		},
		DshEnvFunc: tools.NewManagedDshEnv(a.cfg.DataDir, func() string {
			return a.currentID
		}),
		DshEnvContextFunc: func(ctx context.Context) map[string]string {
			return tools.NewManagedDshEnv(a.cfg.DataDir, func() string {
				return runtimectx.SessionID(ctx)
			})()
		},
	})
	if err := a.reg.Register(pwsh); err != nil {
		return fmt.Errorf("sta: register %s: %w", pwsh.Name(), err)
	}
	return a.registerModelTerminalTools()
}

// terminalAccess adapts the app to the M9 session accessor used by /term: it
// owns the single active session (D5) and fences every access by owner
// session id.
type terminalAccess struct{ a *app }

func (ac *terminalAccess) Owner() string { return ac.a.currentID }

func (ac *terminalAccess) GetActive() (*terminal.Session, error) {
	if ac.a.termSess == nil {
		return nil, fmt.Errorf("%w (start one with /term start)", terminal.ErrNoActive)
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
		Workdir:            ac.a.sessionCWD(),
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

// termCommand implements the /term REPL command group over the M9 session
// accessor — the user-driven persistent shell (the model's pwsh tool runs a
// fresh process per call and shares no state with it).
func (a *app) termCommand(ctx context.Context, args []string) error {
	if !config.Enabled(a.cfg.Terminal.Enabled) {
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
