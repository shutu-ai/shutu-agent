package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/pathsecure"
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
func modelShellToolName() string {
	if runtime.GOOS == "windows" {
		return "pwsh"
	}
	return "bash"
}

func termPolicy() tools.Policy {
	return tools.Policy{
		Enabled: []string{modelShellToolName()},
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
	return app.reg.Execute(context.Background(), modelShellToolName(), b)
}

// TestRegisterTerminalDisabledRegistersNothing verifies the gate: with
// terminal.enabled=false registerTerminal registers no pwsh tool.
func TestRegisterTerminalDisabledRegistersNothing(t *testing.T) {
	app := makeTermApp(false)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if containsStr(specNames(app.reg), modelShellToolName()) {
		t.Fatal("model shell registered while terminal disabled")
	}
}

// TestModelTerminalCancellationClassification documents the audited boundary:
// foreground writes reset the terminal on cancellation, while lifecycle
// receipts remain non-cancellable.
func TestModelTerminalCancellationClassification(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerModelTerminalTools(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range app.reg.Catalog() {
		if entry.Name == modelShellToolName() && !entry.Cancellable {
			t.Fatal("fresh-process model shell must declare foreground cooperative cancellation")
		}
		if strings.HasPrefix(entry.Name, "terminal_") && entry.Cancellable != (entry.Name == "terminal_send") {
			t.Fatalf("%s catalog cancellable = %v, want write-only contract", entry.Name, entry.Cancellable)
		}
	}
}

// TestRegisterTerminalEnabledRegistersPwsh verifies the enabled path: the
// fresh-process pwsh tool lands in the registry, and run_in_background is
// advertised exactly when jobs is wired (app.jobs non-nil).
func TestRegisterTerminalEnabledRegistersModelShell(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	if !containsStr(specNames(app.reg), modelShellToolName()) {
		t.Fatalf("registered tools %v lack %q", specNames(app.reg), modelShellToolName())
	}
	schema, _ := json.Marshal(toolSchema(app.reg, modelShellToolName()))
	if strings.Contains(string(schema), "run_in_background") {
		t.Fatalf("schema without jobs must not advertise run_in_background: %s", schema)
	}

	app = makeTermAppWithJobs(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	schema, _ = json.Marshal(toolSchema(app.reg, modelShellToolName()))
	if !strings.Contains(string(schema), "run_in_background") {
		t.Fatalf("schema with jobs must advertise run_in_background: %s", schema)
	}
}

func TestMinimalRegistersOnlyPersistentPlatformShell(t *testing.T) {
	app := makeTermApp(true)
	app.cfg.Mode = config.ModeMinimal
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	want := "bash"
	if runtime.GOOS == "windows" {
		want = "pwsh"
	}
	if !containsStr(specNames(app.reg), want) {
		t.Fatalf("minimal registered tools = %v, want %q", specNames(app.reg), want)
	}
	for _, name := range []string{"terminal_open", "terminal_list", "terminal_read", "terminal_send", "terminal_signal", "terminal_close"} {
		if containsStr(specNames(app.reg), name) {
			t.Fatalf("minimal must not register generic %s; DSH exposes the persistent shell directly", name)
		}
	}
	defer app.closeModelTerminalSessions()
	app.reg.SetPolicy(tools.Policy{Enabled: []string{want}, Timeout: time.Minute})
	firstCommand := "export SHUTU_PERSISTENT_STATE=kept; echo persistent-shell-ready"
	secondCommand := "echo $SHUTU_PERSISTENT_STATE"
	if runtime.GOOS == "windows" {
		firstCommand = "$env:SHUTU_PERSISTENT_STATE = 'kept'; Write-Output persistent-shell-ready"
		secondCommand = "Write-Output $env:SHUTU_PERSISTENT_STATE"
	}
	first, err := app.reg.Execute(context.Background(), want, json.RawMessage(fmt.Sprintf(`{"command":%q}`, firstCommand)))
	if err != nil {
		t.Fatalf("first persistent shell call: %v", err)
	}
	if !strings.Contains(first.Output, "persistent-shell-ready") {
		t.Fatalf("first output = %q", first.Output)
	}
	second, err := app.reg.Execute(context.Background(), want, json.RawMessage(fmt.Sprintf(`{"command":%q}`, secondCommand)))
	if err != nil {
		t.Fatalf("second persistent shell call: %v", err)
	}
	if !strings.Contains(second.Output, "kept") {
		t.Fatalf("second output = %q", second.Output)
	}
}

func TestPersistentTerminalToolsMatchDshSurface(t *testing.T) {
	app := makeTermApp(true)
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	app.reg.SetPolicy(tools.Policy{Enabled: []string{modelShellToolName(), "terminal_open", "terminal_list", "terminal_read", "terminal_send", "terminal_signal", "terminal_close"}, Timeout: time.Minute})
	opened, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(`{"type":"shell","name":"dsh-test"}`))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	value, ok := opened.Value.(map[string]any)
	if !ok {
		if opened.Error != nil {
			t.Fatalf("terminal_open value = %T output=%q error=%s/%s", opened.Value, opened.Output, opened.Error.Name, opened.Error.Code)
		}
		t.Fatalf("terminal_open value = %T output=%q error=<nil>", opened.Value, opened.Output)
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

func TestModelTerminalWorkspaceBoundaryAndDurableLifecycle(t *testing.T) {
	root := t.TempDir()
	app := makeTermApp(true)
	app.cfg.Workspace.DefaultDir = root
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	app.reg.SetPolicy(tools.Policy{Enabled: []string{"terminal_open", "terminal_close"}, Timeout: time.Minute})
	defer app.closeModelTerminalSessions()

	opened, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(`{"type":"shell","name":"scoped"}`))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	value, ok := opened.Value.(map[string]any)
	if !ok {
		if opened.Error != nil {
			t.Fatalf("terminal_open value = %T output=%q error=%s/%s", opened.Value, opened.Output, opened.Error.Name, opened.Error.Code)
		}
		t.Fatalf("terminal_open value = %T output=%q error=<nil>", opened.Value, opened.Output)
	}
	id, _ := value["sessionId"].(string)
	if id == "" {
		t.Fatalf("terminal_open returned no id: %#v", value)
	}
	rec, err := app.currentModelTerminalFor(app.currentID, id)
	if err != nil {
		t.Fatalf("lookup terminal: %v", err)
	}
	wantRoot, err := pathsecure.ResolveExisting(root)
	if err != nil {
		t.Fatalf("resolve expected workspace: %v", err)
	}
	if got, want := filepath.Clean(rec.cwd), filepath.Clean(wantRoot); got != want {
		t.Fatalf("terminal cwd = %q, want workspace %q", got, want)
	}

	escaped, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(fmt.Sprintf(`{"type":"shell","cwd":%q}`, filepath.Dir(root))))
	if err != nil {
		t.Fatalf("terminal_open escape should be a classified tool result, got execution error: %v", err)
	}
	if !escaped.IsError || !strings.Contains(escaped.Output, "escapes session workspace") {
		t.Fatalf("terminal_open escape result = %#v, want a workspace-boundary error", escaped)
	}
	if _, err := app.reg.Execute(context.Background(), "terminal_close", json.RawMessage(fmt.Sprintf(`{"sessionId":%q}`, id))); err != nil {
		t.Fatalf("terminal_close: %v", err)
	}

	var got []string
	for _, event := range app.log.Events() {
		if event.Type == session.EventTerminalStart || event.Type == session.EventTerminalStop {
			got = append(got, event.Type)
		}
	}
	want := []string{session.EventTerminalStart, session.EventTerminalStop}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal lifecycle events = %v, want %v", got, want)
	}
}

func TestModelTerminalOwnerCloseStopsOnlyOwnedSessions(t *testing.T) {
	root := t.TempDir()
	app := makeTermApp(true)
	app.cfg.Workspace.DefaultDir = root
	app.runtimeLogs = map[string]*session.Log{app.currentID: app.log}
	if err := app.registerTerminal(); err != nil {
		t.Fatalf("registerTerminal: %v", err)
	}
	app.reg.SetPolicy(tools.Policy{Enabled: []string{"terminal_open", "terminal_list"}, Timeout: time.Minute})

	opened, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(`{"type":"shell","name":"owned"}`))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	id := opened.Value.(map[string]any)["sessionId"].(string)

	other := &modelTerminalRecord{owner: "other", typ: "shell", sess: nil}
	// The owner disposer must not touch records belonging to another Agent.
	// Use a real session for the foreign record so its lifecycle is observable.
	other.sess, err = terminal.NewSession(terminal.SessionOpts{Shell: app.cfg.Terminal.Shell, Workdir: root, IdleMS: 100, TimeoutMS: 2500})
	if err != nil {
		t.Fatalf("foreign terminal: %v", err)
	}
	app.modelTermMu.Lock()
	app.modelTerms[other.sess.ID()] = other
	app.modelTermMu.Unlock()
	defer func() { _ = other.sess.Close() }()

	if err := app.closeModelTerminalOwner(app.currentID); err != nil {
		t.Fatalf("closeModelTerminalOwner: %v", err)
	}
	if err := app.closeModelTerminalOwner(app.currentID); err != nil {
		t.Fatalf("repeat closeModelTerminalOwner: %v", err)
	}
	listed, err := app.reg.Execute(context.Background(), "terminal_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("terminal_list: %v", err)
	}
	if got := listed.Value.([]any); len(got) != 0 {
		t.Fatalf("owned terminal remains listed after owner close: %#v", got)
	}
	if rec, err := app.currentModelTerminalFor(app.currentID, id); err == nil || rec != nil {
		t.Fatalf("closed owned terminal remains addressable: rec=%v err=%v", rec, err)
	}
	app.modelTermMu.Lock()
	_, retained := app.modelTerms[id]
	app.modelTermMu.Unlock()
	if retained {
		t.Fatal("closed owned terminal remains in the process registry")
	}
	if status := other.sess.Status(); status.Kind != "running" {
		t.Fatalf("foreign terminal status = %#v, owner close must not stop it", status)
	}
	var stops int
	for _, event := range app.log.Events() {
		if event.Type == session.EventTerminalStop {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("terminal stop events = %d, want one owner-scoped stop", stops)
	}
}

func TestRecoverTerminalClaimsAppendsOneColdRestartStop(t *testing.T) {
	app := makeTermApp(true)
	log := session.New()
	if _, err := log.Append(session.EventTerminalStart, session.NewTerminalStart("terminal-old", app.currentID)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventTerminalStart, session.NewTerminalStart("terminal-closed", app.currentID)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventTerminalStop, session.NewTerminalStop("terminal-closed", "tool_close")); err != nil {
		t.Fatal(err)
	}

	if err := app.recoverTerminalClaims(log, app.currentID); err != nil {
		t.Fatalf("recover terminal claims: %v", err)
	}
	if err := app.recoverTerminalClaims(log, app.currentID); err != nil {
		t.Fatalf("repeat terminal claim recovery: %v", err)
	}
	var stops []string
	for _, event := range log.Events() {
		if event.Type != session.EventTerminalStop {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		stops = append(stops, payload.ID)
	}
	if !reflect.DeepEqual(stops, []string{"terminal-closed", "terminal-old"}) {
		t.Fatalf("terminal stop receipts = %v, want the closed edge then one cold-restart edge", stops)
	}
}

func TestRecoverTerminalClaimsSkipsLiveProcessOwner(t *testing.T) {
	root := t.TempDir()
	app := makeTermApp(true)
	app.cfg.Workspace.DefaultDir = root
	if err := app.registerTerminal(); err != nil {
		t.Fatal(err)
	}
	app.reg.SetPolicy(tools.Policy{Enabled: []string{"terminal_open"}, Timeout: time.Minute})
	defer app.closeModelTerminalSessions()
	opened, err := app.reg.Execute(context.Background(), "terminal_open", json.RawMessage(`{"type":"shell"}`))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	id := opened.Value.(map[string]any)["sessionId"].(string)
	if _, err := app.log.Append(session.EventTerminalStart, session.NewTerminalStart("terminal-stale", app.currentID)); err != nil {
		t.Fatal(err)
	}
	if err := app.recoverTerminalClaims(app.log, app.currentID); err != nil {
		t.Fatal(err)
	}
	for _, event := range app.log.Events() {
		if event.Type != session.EventTerminalStop {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ID == id {
			t.Fatalf("live terminal %s was marked stale", id)
		}
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
	setStateCommand := `export PA_PWSH_STATE=leak`
	readStateCommand := `if [ -n "$PA_PWSH_STATE" ]; then printf %s "$PA_PWSH_STATE"; else echo fresh; fi`
	if runtime.GOOS == "windows" {
		setStateCommand = `$env:PA_PWSH_STATE = "leak"`
		readStateCommand = `if ($env:PA_PWSH_STATE) { $env:PA_PWSH_STATE } else { "fresh" }`
	}
	if _, err := execTerm(t, app, map[string]any{
		"command":     setStateCommand,
		"description": "set a variable",
	}); err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	if app.termSess != nil {
		t.Fatal("the fresh-process pwsh tool must never start the M9 session")
	}
	res, err := execTerm(t, app, map[string]any{
		"command":     readStateCommand,
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
	exitCommand := "exit 3"
	if runtime.GOOS == "windows" {
		exitCommand = "exit 3"
	}
	res, err := execTerm(t, app, map[string]any{
		"command":     exitCommand,
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
	outputCommand := "echo bg-composed"
	if runtime.GOOS == "windows" {
		outputCommand = "Write-Output bg-composed"
	}
	res, err := execTerm(t, app, map[string]any{
		"command":           outputCommand,
		"description":       "background echo",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("pwsh: %v", err)
	}
	wantJobID := modelShellToolName() + "-1"
	if !strings.Contains(res.Output, "started background job "+wantJobID) {
		t.Fatalf("ack = %q, want the job id", res.Output)
	}
	snap, err := app.jobs.Wait(context.Background(), wantJobID, "s-term", 10*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if snap.Status != jobs.StatusCompleted {
		t.Fatalf("status = %s, want completed", snap.Status)
	}
	got, _, err := app.jobs.Read(context.Background(), wantJobID, "s-term")
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
