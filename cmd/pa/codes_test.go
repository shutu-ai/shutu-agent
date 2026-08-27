package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeCodeApp builds a minimal app for code wiring tests: only the fields
// registerCode touches (cfg.Code, reg, log) are set.
func makeCodeApp(codeEnabled bool) *app {
	return &app{
		cfg: config.Config{
			Code: config.CodeConfig{Enabled: config.Bool(codeEnabled)},
		},
		reg:        tools.New(),
		log:        session.New(),
		basePolicy: tools.Policy{Enabled: []string{"get_time", "run_code"}},
	}
}

// codePolicy whitelists run_code so the registry Execute gate can run it (in
// production config.applyDefaults + PolicyFromConfig do this).
func codePolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"run_code"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// TestRegisterCodeDisabledRegistersNothing verifies the D10 gate: with
// code.enabled=false the composition root creates no Engine and registers no
// run_code tool (dispatch-m6e-2 §4).
func TestRegisterCodeDisabledRegistersNothing(t *testing.T) {
	a := makeCodeApp(false)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	if a.code != nil {
		t.Fatal("code engine must be nil when code.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if spec.Name == "run_code" {
			t.Fatalf("run_code registered while code disabled")
		}
	}
}

// TestRegisterCodeEnabledRegistersAndValidates verifies the enabled path: the
// TypeScript runtime is created, run_code is registered, D7 rejects bad
// arguments at the Execute gate, a valid run flows through and lands code/run
// in the session log (D3) without deriving into history (log-only), and a
// non-zero exit is returned to the model.
func TestRegisterCodeEnabledRegistersAndValidates(t *testing.T) {
	a := makeCodeApp(true)
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatalf("register get_time: %v", err)
	}
	pol := codePolicy()
	pol.Enabled = []string{"get_time", "run_code"}
	a.reg.SetPolicy(pol)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	defer a.code.Close()
	if a.code == nil {
		t.Fatal("code engine must be created when code.enabled=true")
	}
	found := false
	for _, s := range a.reg.Specs() {
		if s.Name == "run_code" {
			found = true
		}
	}
	if !found {
		t.Fatal("run_code not registered when code.enabled=true")
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct{ name, args string }{
		{"run_code", `{}`},                                       // missing required code/description
		{"run_code", `{"code":"x"}`},                             // description required
		{"run_code", `{"description":"x"}`},                      // code required
		{"run_code", `{"code":"x","description":"x","extra":1}`}, // additional properties rejected
		{"run_code", `{"code":123,"description":"x"}`},           // code must be a string
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid run works and lands the code/run event (D3).
	good := fmt.Sprintf(`{"code":%s,"description":"call time and print a marker"}`, jsonString("const now = await tools.get_time({}); console.log('hi'); return now"))
	prepared, err := a.reg.Prepare(context.Background(), "outer-1", "run_code", json.RawMessage(good))
	if err != nil {
		t.Fatalf("prepare run_code via registry: %v", err)
	}
	res, err := a.reg.ExecutePrepared(context.Background(), prepared)
	if err != nil {
		t.Fatalf("run_code via registry: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("run_code output = %q, want it to carry hi", res.Output)
	}
	if !hasEvent(a.log, session.EventCodeRun) {
		t.Fatal("code/run event missing from the session log after run_code")
	}
	if !hasEvent(a.log, session.EventCodeDispatchStart) || !hasEvent(a.log, session.EventCodeDispatch) {
		t.Fatalf("nested code dispatch events missing: %+v", a.log.Events())
	}
	var dispatchSeen bool
	for _, event := range a.log.Events() {
		if event.Type == session.EventCodeDispatch && strings.Contains(string(event.Data), `"subCallId":"outer-1:code:1"`) {
			dispatchSeen = true
		}
	}
	if !dispatchSeen {
		t.Fatalf("nested dispatch did not preserve parent call id: %+v", a.log.Events())
	}
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("code/run events must not derive into messages: %+v", msgs)
	}

	// A nested tool failure is catchable inside the TypeScript program, matching
	// DSH's ToolCallError promise rejection contract.
	fail := `{"code":"try { await tools.missing({}); return 'unexpected' } catch (error) { return { name: error.name, message: error.message } }","description":"catch a nested tool failure"}`
	res2, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(fail))
	if err != nil {
		t.Fatalf("nested tool failure must be catchable: %v", err)
	}
	if !strings.Contains(res2.Output, "ToolCallError") || !strings.Contains(res2.Output, "unknown tool") {
		t.Fatalf("run_code output = %q, want caught ToolCallError", res2.Output)
	}
}

// TestRegisterCodePolicyDeadlineBoundsSandboxRun verifies code.timeout is the
// outer per-tool deadline for run_code (mirrors run_command): a sandbox run
// that would outlive the config bound is cut at the Execute gate even when the
// model requests a longer per-call timeout, and the cut surfaces as a normal
// sandbox timeout result (the model sees the "[timed out]" marker, not an
// error).
func TestRegisterCodePolicyDeadlineBoundsSandboxRun(t *testing.T) {
	a := makeCodeApp(true)
	pol := codePolicy()
	pol.CodeRun.Timeout = 200 * time.Millisecond
	a.reg.SetPolicy(pol)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	defer a.code.Close()
	args := `{"code":"await new Promise(() => {})","description":"wait forever"}`
	res, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(args))
	if err != nil {
		t.Fatalf("run_code after the policy deadline must be a normal timeout result, not an error: %v", err)
	}
	if !res.IsError || res.Error == nil || res.Error.Code != "TOOL_TIMEOUT" {
		t.Fatalf("run_code result = %+v, want structured timeout (the code.timeout bound cut the run)", res)
	}
}

// jsonString returns s as a JSON string literal (for embedding paths/code in
// tool argument JSON).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
