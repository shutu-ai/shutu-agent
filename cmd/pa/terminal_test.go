package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/terminal"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeTermApp builds a minimal app for registerTerminal / termCommand tests:
// only the fields those touch (cfg.Terminal, reg, log, currentID) are set.
// ReadIdleMS / ReadTimeoutMS are kept short so the real-session smoke stays
// fast.
func makeTermApp(enabled bool) *app {
	return &app{
		cfg: config.Config{Terminal: config.TerminalConfig{
			Enabled: config.Bool(enabled), ReadIdleMS: 100, ReadTimeoutMS: 2500,
		}},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-term",
	}
}

// termPolicy whitelists pwsh, mirroring what config.applyDefaults does when
// terminal.enabled is true. The wiring tests drive the tool object directly
// (bypassing the registry), so this policy is only for cases that go through
// reg.Execute.
func termPolicy() tools.Policy {
	return tools.Policy{
		Enabled: []string{terminal.ToolPwshName},
	}
}

// execTerm executes the pwsh tool with JSON args — the same serial tool path
// the model and /term share. It bypasses the registry policy on purpose.
func execTerm(t *testing.T, tool tools.Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), b)
}

// TestRegisterTerminalDisabledRegistersNothing verifies the D10 gate: with
// terminal.enabled=false registerTerminal builds no tool bundle and registers
// no pwsh tool.
func TestRegisterTerminalDisabledRegistersNothing(t *testing.T) {
	app := makeTermApp(false)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if app.termTools != nil {
		t.Fatal("termTools must stay nil when terminal.enabled=false")
	}
	if containsStr(specNames(app.reg), terminal.ToolPwshName) {
		t.Fatal("pwsh registered while terminal disabled")
	}
}

// TestRegisterTerminalEnabledRegistersTools verifies the enabled path: the
// tool bundle is built and the pwsh tool lands in the registry.
func TestRegisterTerminalEnabledRegistersTools(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if app.termTools == nil {
		t.Fatal("termTools must be created when terminal.enabled=true")
	}
	if !containsStr(specNames(app.reg), terminal.ToolPwshName) {
		t.Fatalf("registered tools %v lack %q", specNames(app.reg), terminal.ToolPwshName)
	}
}

// TestPwshLifecycleE2E drives a real shell session through the composed
// pwsh tool: the first call starts the session implicitly (dsh) and runs the
// command, a second call reuses it, stop via the accessor ends the session,
// and a later call starts a fresh one. It also asserts the D3 event sink
// appended terminal/start to the session log.
func TestPwshLifecycleE2E(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	defer func() {
		if app.termSess != nil {
			app.termSess.Close()
		}
	}()

	// First call: the session comes up on first use and runs the command.
	out, err := execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "hi")
	}
	if app.termSess == nil {
		t.Fatal("first pwsh call must leave an active session")
	}
	if !hasEvent(app.log, session.EventTerminalStart) {
		t.Fatal("terminal/start event missing from the session log after first use")
	}

	// Second call reuses the session.
	out, err = execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo second"})
	if err != nil {
		t.Fatalf("pwsh second call: %v", err)
	}
	if !strings.Contains(out, "second") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "second")
	}

	// Stop ends the session; the next call starts a fresh one.
	if err := (&terminalAccess{a: app}).Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	out, err = execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo fresh"})
	if err != nil {
		t.Fatalf("pwsh after stop: %v", err)
	}
	if !strings.Contains(out, "fresh") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "fresh")
	}
}

// TestPwshRejectsEmptyCommand asserts an empty command is rejected before the
// session is touched.
func TestPwshRejectsEmptyCommand(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if _, err := execTerm(t, app.termTools.Pwsh(), map[string]any{"command": ""}); err == nil {
		t.Fatal("pwsh with an empty command must error")
	}
	if _, err := execTerm(t, app.termTools.Pwsh(), map[string]any{}); err == nil {
		t.Fatal("pwsh with no command must error")
	}
}

// TestTerminalOwnerFence verifies the D5 owner fence: switching the current
// session id makes the composed terminalAccess refuse access to a session the
// new owner does not hold, and switching back restores access.
func TestTerminalOwnerFence(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if _, err := execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo ok"}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	defer func() {
		if app.termSess != nil {
			app.termSess.Close()
		}
	}()

	app.currentID = "s-other"
	if _, err := execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo x"}); err == nil {
		t.Fatal("pwsh from a non-owner session must fail")
	} else if !strings.Contains(err.Error(), "another session") {
		t.Fatalf("owner-fence error = %v, want contains %q", err, "another session")
	}

	app.currentID = "s-term"
	out, err := execTerm(t, app.termTools.Pwsh(), map[string]any{"command": "echo ok"})
	if err != nil {
		t.Fatalf("pwsh after restoring owner: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("pwsh output = %q, want contains %q", out, "ok")
	}
}

// TestTermCommandDisabled verifies /term is unavailable when the terminal is
// disabled: no tools were registered, so termCommand reports disabled.
func TestTermCommandDisabled(t *testing.T) {
	app := makeTermApp(false)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	err := app.termCommand(context.Background(), []string{"start"})
	if err == nil {
		t.Fatal("termCommand must fail when terminal.enabled=false")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want contains %q", err, "disabled")
	}
}
