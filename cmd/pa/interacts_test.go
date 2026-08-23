package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// fakeSensitiveTool is a stand-in for a whitelisted tool whose name a wiring
// test lists in interact.sensitive_tools. It records whether it executed, so
// the gate tests can prove approved → the tool runs and rejected → it never
// does.
type fakeSensitiveTool struct {
	name     string
	executed bool
}

func (f *fakeSensitiveTool) Name() string        { return f.name }
func (f *fakeSensitiveTool) Description() string { return "fake sensitive tool" }
func (f *fakeSensitiveTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f *fakeSensitiveTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	f.executed = true
	return "ran", nil
}

// makeInteractApp builds a minimal app for interact wiring tests: only the
// fields registerInteracts touches (cfg.Interact, reg, log, currentID,
// approveInput) are set.
func makeInteractApp(enabled bool, sensitive []string) *app {
	return &app{
		cfg: config.Config{
			Interact: config.InteractConfig{Enabled: config.Bool(enabled), SensitiveTools: sensitive},
		},
		reg:          tools.New(),
		log:          session.New(),
		currentID:    "s-interact",
		approveInput: strings.NewReader("y\n"),
	}
}

// interactPolicy whitelists the tools the test executes so the registry Execute
// gate can run them (in production config.applyDefaults + PolicyFromConfig do
// this).
func interactPolicy(extra ...string) tools.Policy {
	return tools.Policy{Enabled: append([]string{"interact_ask", "interact_status"}, extra...)}
}

// TestRegisterInteractsDisabledRegistersNothing verifies the D10 gate: with
// interact.enabled=false the composition root creates no Engine, registers no
// interact_* tool, and installs no sensitive-tool gate (dispatch-m6d-2 §5).
func TestRegisterInteractsDisabledRegistersNothing(t *testing.T) {
	a := makeInteractApp(false, nil)
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	if a.interacts != nil {
		t.Fatal("interact engine must be nil when interact.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "interact_") {
			t.Fatalf("interact tool %q registered while interact disabled", spec.Name)
		}
	}
	// No gate installed: a whitelisted tool runs without any approval read.
	// approveInput would block or feed junk if a gate existed; here the tool
	// executes untouched.
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	a.reg.SetPolicy(interactPolicy("bash"))
	if _, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("run_command with interact disabled: %v", err)
	}
	if !ft.executed {
		t.Fatal("run_command must run with no gate when interact is disabled")
	}
}

// TestRegisterInteractsEnabledRegistersToolsAndEvents verifies the enabled
// path: the Provider + Engine are created, both interact_* tools are
// registered, D7 rejects bad arguments at the Execute gate, valid calls flow
// through (ask → status), and the interact/* events land in the session log
// (D3).
func TestRegisterInteractsEnabledRegistersToolsAndEvents(t *testing.T) {
	a := makeInteractApp(true, nil)
	a.reg.SetPolicy(interactPolicy())
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()
	if a.interacts == nil {
		t.Fatal("interact engine must be created when interact.enabled=true")
	}
	names := make([]string, 0, len(a.reg.Specs()))
	for _, s := range a.reg.Specs() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"interact_ask", "interact_status"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"interact_ask", `{}`},                       // missing required prompt
		{"interact_ask", `{"prompt":123}`},           // prompt must be a string
		{"interact_ask", `{"prompt":"p","extra":1}`}, // additional properties rejected
		{"interact_status", `{}`},                    // missing required id
		{"interact_status", `{"id":123}`},            // id must be a string
		{"interact_status", `{"id":"x","extra":1}`},  // additional properties rejected
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid ask flows through and lands interact/request (D3).
	res, err := a.reg.Execute(context.Background(), "interact_ask", json.RawMessage(`{"prompt":"proceed?"}`))
	if err != nil {
		t.Fatalf("interact_ask via registry: %v", err)
	}
	if !strings.Contains(res.Output, "req-1") {
		t.Fatalf("interact_ask output = %q, want it to carry req-1", res.Output)
	}
	if !hasEvent(a.log, session.EventInteractRequest) {
		t.Fatal("interact/request event missing from the session log after interact_ask")
	}
	// interact_status reports the request's status and lands interact/status
	// (D3).
	if _, err := a.reg.Execute(context.Background(), "interact_status", json.RawMessage(`{"id":"req-1"}`)); err != nil {
		t.Fatalf("interact_status via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventInteractStatus) {
		t.Fatal("interact/status event missing from the session log after interact_status")
	}
	// An unknown id errors.
	if _, err := a.reg.Execute(context.Background(), "interact_status", json.RawMessage(`{"id":"req-99"}`)); err == nil {
		t.Fatal("interact_status of an unknown id must error")
	}
	// The interact/* rows never derive into model messages (log-only).
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("interact/* events must not derive into messages: %+v", msgs)
	}
}

// TestSensitiveGateApprovedRuns verifies the ADR 决策 M6d gate approved path
// (dispatch-m6d-2 §4): a tool listed in sensitive_tools first creates a pending
// approval request (interact/request), reads the user's y answer on the CLI
// serial path, records the decision (interact/resolve), and only then executes
// the underlying tool — whose output is returned.
func TestSensitiveGateApprovedRuns(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.approveInput = strings.NewReader("y\n")
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("run_command through the approved gate: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (approved → the tool runs)", res.Output, ft.executed)
	}
	// The gate recorded request + resolve, and no deny.
	if !hasEvent(a.log, session.EventInteractRequest) || !hasEvent(a.log, session.EventInteractResolve) {
		t.Fatalf("log = %+v, want interact/request + interact/resolve", a.log.Events())
	}
	if hasEvent(a.log, session.EventInteractDeny) {
		t.Fatal("interact/deny must not fire on an approved execution")
	}
}

// TestSensitiveGateRejectedReturnsDenial verifies the rejected path: the user's
// n answer records the decision, appends interact/deny, and the gate returns a
// denial to the model — the underlying tool never executes.
func TestSensitiveGateRejectedReturnsDenial(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.approveInput = strings.NewReader("n\n")
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	_, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`))
	if err == nil {
		t.Fatal("run_command through a rejected gate must return a denial")
	}
	if !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("denial error = %v, want it to mention the user rejection", err)
	}
	if ft.executed {
		t.Fatal("run_command must NOT execute after a rejection")
	}
	for _, typ := range []string{session.EventInteractRequest, session.EventInteractResolve, session.EventInteractDeny} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log = %+v, want %s recorded", a.log.Events(), typ)
		}
	}
}

// TestSensitiveGateMissedDoesNotIntercept verifies a whitelisted tool that is
// NOT listed in sensitive_tools passes through untouched: it executes with no
// approval request, no events and no terminal read.
func TestSensitiveGateMissedDoesNotIntercept(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.reg.SetPolicy(interactPolicy("read"))
	ft := &fakeSensitiveTool{name: "read"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.Execute(context.Background(), "read", json.RawMessage(`{"path":"x"}`))
	if err != nil {
		t.Fatalf("non-sensitive read: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (no interception)", res.Output, ft.executed)
	}
	if len(a.log.Events()) != 0 {
		t.Fatalf("non-sensitive execution must log no interact event: %+v", a.log.Events())
	}
}

// TestSensitiveGateEmptyListNoGate verifies that an enabled interact with an
// empty sensitive_tools registers the interact_* tools but installs no gate
// (dispatch-m6d-2 §2/§5): a whitelisted tool runs with no approval.
func TestSensitiveGateEmptyListNoGate(t *testing.T) {
	a := makeInteractApp(true, nil)
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("run_command with an empty sensitive list: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (no gate with empty sensitive_tools)", res.Output, ft.executed)
	}
	if len(a.log.Events()) != 0 {
		t.Fatalf("empty sensitive_tools must log no interact event: %+v", a.log.Events())
	}
}
