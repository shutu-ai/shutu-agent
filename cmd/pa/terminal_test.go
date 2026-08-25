package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/session"
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

// makeTermAppWithJobs adds the background-job registry the composed pwsh tool
// uses for run_in_background (registerJobs wiring is not needed here — the
// tool reads a.jobs directly).
func makeTermAppWithJobs(enabled bool) *app {
	app := makeTermApp(enabled)
	app.jobs = jobs.NewLocal(jobs.LocalOpts{})
	return app
}

// termPolicy whitelists pwsh, mirroring what config.applyDefaults does when
// terminal.enabled is true.
func termPolicy() tools.Policy {
	return tools.Policy{
		Enabled: []string{"pwsh"},
		Timeout: time.Minute,
	}
}

// execTerm executes the composed pwsh tool through the registry's serial gate
// — the same path the model uses.
func execTerm(t *testing.T, app *app, args map[string]any) (tools.ToolResult, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return app.reg.Execute(context.Background(), "pwsh", b)
}

// TestRegisterTerminalDisabledRegistersNothing verifies the gate: with
// terminal.enabled=false registerTerminal registers no pwsh tool.
func TestRegisterTerminalDisabledRegistersNothing(t *testing.T) {
	app := makeTermApp(false)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if containsStr(specNames(app.reg), "pwsh") {
		t.Fatal("pwsh registered while terminal disabled")
	}
}

// TestRegisterTerminalEnabledRegistersPwsh verifies the enabled path: the
// fresh-process pwsh tool lands in the registry, and run_in_background is
// advertised exactly when jobs is wired (app.jobs non-nil).
func TestRegisterTerminalEnabledRegistersPwsh(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if !containsStr(specNames(app.reg), "pwsh") {
		t.Fatalf("registered tools %v lack %q", specNames(app.reg), "pwsh")
	}
	schema, _ := json.Marshal(toolSchema(app.reg, "pwsh"))
	if strings.Contains(string(schema), "run_in_background") {
		t.Fatalf("schema without jobs must not advertise run_in_background: %s", schema)
	}

	app = makeTermAppWithJobs(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	schema, _ = json.Marshal(toolSchema(app.reg, "pwsh"))
	if !strings.Contains(string(schema), "run_in_background") {
		t.Fatalf("schema with jobs must advertise run_in_background: %s", schema)
	}
}

func TestPersistentTerminalToolsMatchDshSurface(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	app.reg.SetPolicy(tools.Policy{Enabled: []string{"pwsh", "terminal_open", "terminal_list", "terminal_read", "terminal_send", "terminal_signal", "terminal_close"}, Timeout: time.Minute})
	opened, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(`{"type":"shell","name":"dsh-test"}`))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	value, ok := opened.Value.(map[string]any)
	if !ok {
		t.Fatalf("terminal_open value = %T", opened.Value)
	}
	id, _ := value["sessionId"].(string)
	if id == "" {
		t.Fatalf("terminal_open returned no sessionId: %v", value)
	}
	defer app.closeModelTerminalSessions()
	listed, err := app.reg.Execute(context.Background(), "terminal_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("terminal_list: %v", err)
	}
	if list, ok := listed.Value.([]any); !ok || len(list) != 1 {
		t.Fatalf("terminal_list value = %#v", listed.Value)
	}
	sent, err := app.reg.Execute(context.Background(), "terminal_send", json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"text":"echo dsh-terminal"}`, id)))
	if err != nil {
		t.Fatalf("terminal_send: %v", err)
	}
	if !strings.Contains(sent.Output, "dsh-terminal") {
		t.Fatalf("terminal_send output = %q", sent.Output)
	}
	if _, err := app.reg.Execute(context.Background(), "terminal_close", json.RawMessage(fmt.Sprintf(`{"sessionId":%q}`, id))); err != nil {
		t.Fatalf("terminal_close: %v", err)
	}
}

// toolSchema returns the registered pwsh schema (nil when not registered).
func toolSchema(reg *tools.Registry, name string) map[string]any {
	for _, spec := range reg.Specs() {
		if spec.Name == name {
			return spec.Parameters
		}
	}
	return nil
}

// TestPwshFreshProcessNoSession drives the composed pwsh tool and verifies
// the dsh contract end-to-end: each call is a fresh pwsh process, no state
// persists between calls, and the M9 persistent session is never touched.
func TestPwshFreshProcessNoSession(t *testing.T) {
	app := makeTermApp(true)
	app.reg.SetPolicy(termPolicy())
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if _, err := execTerm(t, app, map[string]any{
		"command":     `$env:PA_PWSH_STATE = "leak"`,
		"description": "set a variable",
	}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if app.termSess != nil {
		t.Fatal("the fresh-process pwsh tool must never start the M9 session")
	}
	res, err := execTerm(t, app, map[string]any{
		"command":     `if ($env:PA_PWSH_STATE) { $env:PA_PWSH_STATE } else { "fresh" }`,
		"description": "read the variable",
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(res.Output, "fresh") || strings.Contains(res.Output, "leak") {
		t.Fatalf("output = %q, want a fresh process with no carried state", res.Output)
	}
	if app.termSess != nil {
		t.Fatal("the fresh-process pwsh tool must never start the M9 session")
	}
}

// TestPwshRejectsInvalidArgs verifies invalid arguments error at the registry
// gate (D7) before any process is spawned.
func TestPwshRejectsInvalidArgs(t *testing.T) {
	app := makeTermApp(true)
	app.reg.SetPolicy(termPolicy())
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	for _, args := range []map[string]any{
		{"command": ""},
		{},
		{"command": "echo hi", "description": ""},
	} {
		if _, err := execTerm(t, app, args); err == nil {
			t.Fatalf("pwsh with args %v must error", args)
		}
	}
}

// TestPwshExitCodeMarker verifies the composed tool reports a non-zero exit
// as a normal result with the marker.
func TestPwshExitCodeMarker(t *testing.T) {
	app := makeTermApp(true)
	app.reg.SetPolicy(termPolicy())
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	res, err := execTerm(t, app, map[string]any{
		"command":     "exit 3",
		"description": "fail with code 3",
	})
	if err != nil {
		t.Fatalf("non-zero exit must not be a hard error: %v", err)
	}
	if !strings.Contains(res.Output, "[exit code: 3]") {
		t.Fatalf("output = %q, want the exit-code marker", res.Output)
	}
}

// TestPwshBackgroundE2E verifies the composed run_in_background path: the
// acknowledgement carries the registry-issued job id and the job settles with
// its output readable through the jobs registry.
func TestPwshBackgroundE2E(t *testing.T) {
	app := makeTermAppWithJobs(true)
	app.reg.SetPolicy(termPolicy())
	defer app.jobs.Close()
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	res, err := execTerm(t, app, map[string]any{
		"command":           "Write-Output bg-composed",
		"description":       "background echo",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if !strings.Contains(res.Output, "started background job pwsh-1") {
		t.Fatalf("ack = %q, want the job id", res.Output)
	}
	snap, err := app.jobs.Wait(context.Background(), "pwsh-1", "s-term", 10*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if snap.Status != jobs.StatusCompleted {
		t.Fatalf("status = %s, want completed", snap.Status)
	}
	got, _, err := app.jobs.Read(context.Background(), "pwsh-1", "s-term")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, "bg-composed") {
		t.Fatalf("job output = %q, want the echo output", got)
	}
}

// TestTermCommandDisabled verifies /term is unavailable when the terminal is
// disabled.
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

// TestTermCommandSmoke drives the M9 /term REPL seam over a real session:
// start creates the single active session, write submits a command, and stop
// closes it (the session stays independent of the fresh-process pwsh tool).
func TestTermCommandSmoke(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	defer func() {
		if app.termSess != nil {
			app.termSess.Close()
		}
	}()
	if err := app.termCommand(context.Background(), []string{"start"}); err != nil {
		t.Fatalf("/term start: %v", err)
	}
	if app.termSess == nil {
		t.Fatal("/term start must create the active session")
	}
	if err := app.termCommand(context.Background(), []string{"write", "echo", "hello-term"}); err != nil {
		t.Fatalf("/term write: %v", err)
	}
	if err := app.termCommand(context.Background(), []string{"stop"}); err != nil {
		t.Fatalf("/term stop: %v", err)
	}
	if app.termSess != nil {
		t.Fatal("/term stop must detach the active session")
	}
}
