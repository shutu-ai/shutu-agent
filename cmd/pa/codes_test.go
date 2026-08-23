package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
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
		reg: tools.New(),
		log: session.New(),
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
// local Provider + Engine are created, run_code is registered, D7 rejects bad
// arguments at the Execute gate, a valid run flows through and lands code/run
// in the session log (D3) without deriving into history (log-only), and a
// non-zero exit is returned to the model.
func TestRegisterCodeEnabledRegistersAndValidates(t *testing.T) {
	a := makeCodeApp(true)
	a.reg.SetPolicy(codePolicy())
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

	cwd := t.TempDir()
	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct{ name, args string }{
		{"run_code", `{}`},                                     // missing required lang/code
		{"run_code", `{"lang":"python","code":"x"}`},           // lang outside the enum
		{"run_code", `{"lang":"sh","code":"x","extra":1}`},     // additional properties rejected
		{"run_code", `{"lang":"sh","code":123}`},               // code must be a string
		{"run_code", `{"lang":"sh","code":"x","timeout":"5"}`}, // timeout must be a number
		{"run_code", `{"lang":"sh","code":"x","timeout":-1}`},  // timeout must be >= 0
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid run works and lands the code/run event (D3).
	good := fmt.Sprintf(`{"lang":"sh","code":"echo hi","cwd":%s}`, jsonString(cwd))
	res, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(good))
	if err != nil {
		t.Fatalf("run_code via registry: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("run_code output = %q, want it to carry hi", res.Output)
	}
	if !hasEvent(a.log, session.EventCodeRun) {
		t.Fatal("code/run event missing from the session log after run_code")
	}
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("code/run events must not derive into messages: %+v", msgs)
	}

	// A non-zero exit is returned to the model as a normal result (tool/result,
	// not tool/error) and still lands code/run.
	fail := fmt.Sprintf(`{"lang":"sh","code":%s,"cwd":%s}`, jsonString(failCommandString()), jsonString(cwd))
	res2, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(fail))
	if err != nil {
		t.Fatalf("non-zero exit run_code via registry must be a normal result: %v", err)
	}
	if !strings.Contains(res2.Output, "[exit code: 3]") {
		t.Fatalf("run_code output = %q, want [exit code: 3]", res2.Output)
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
	cwd := t.TempDir()
	args := fmt.Sprintf(`{"lang":"sh","code":%s,"timeout":30,"cwd":%s}`,
		jsonString(longRunningCommand()), jsonString(cwd))
	res, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(args))
	if err != nil {
		t.Fatalf("run_code after the policy deadline must be a normal timeout result, not an error: %v", err)
	}
	if !strings.Contains(res.Output, "[timed out") {
		t.Fatalf("run_code output = %q, want a timeout marker (the code.timeout bound cut the run)", res.Output)
	}
}

// failCommandString returns a one-line command that exits 3 with "oops" on
// stderr (mirrors internal/code's failCommand helper).
func failCommandString() string {
	if runtime.GOOS == "windows" {
		return "echo oops 1>&2 & exit 3"
	}
	return "echo oops 1>&2; exit 3"
}

// longRunningCommand returns a command that blocks far longer than any test
// deadline (cmd-internal loop on Windows so killing the direct child is clean).
func longRunningCommand() string {
	if runtime.GOOS == "windows" {
		return "for /L %i in (1,1,1000000000) do @rem"
	}
	return "sleep 30"
}

// jsonString returns s as a JSON string literal (for embedding paths/code in
// tool argument JSON).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
