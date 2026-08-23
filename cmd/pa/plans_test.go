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

// makePlanApp builds a minimal app for plan wiring tests: only the fields
// registerPlans touches (cfg.Plan, reg, log) are set.
func makePlanApp(planEnabled bool) *app {
	return &app{
		cfg: config.Config{
			Plan: config.PlanConfig{Enabled: config.Bool(planEnabled)},
		},
		reg: tools.New(),
		log: session.New(),
	}
}

// planPolicy whitelists the six plan tools so the registry Execute gate can run
// them (in production config.applyDefaults + PolicyFromConfig do this).
func planPolicy() tools.Policy {
	return tools.Policy{
		Enabled: []string{
			"plan_goal", "plan_plan", "plan_todo", "plan_status", "plan_list", "plan_remove",
		},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// TestRegisterPlansDisabledRegistersNothing verifies the D10 gate: with
// plan.enabled=false the composition root creates no Engine and registers no
// plan_* tool (dispatch-m6b-2 §4).
func TestRegisterPlansDisabledRegistersNothing(t *testing.T) {
	a := makePlanApp(false)
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	if a.plans != nil {
		t.Fatal("plan engine must be nil when plan.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "plan_") {
			t.Fatalf("plan tool %q registered while plan disabled", spec.Name)
		}
	}
}

// TestRegisterPlansEnabledRegistersAndValidates verifies the enabled path: the
// Provider + Engine are created, all six plan_* tools are registered, D7
// rejects bad arguments at the Execute gate, valid calls flow through
// (goal → plan → todo → status → list → remove), the plan/* events land in the
// session log (D3) without deriving into history (log-only), and unknown ids
// error.
func TestRegisterPlansEnabledRegistersAndValidates(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	if a.plans == nil {
		t.Fatal("plan engine must be created when plan.enabled=true")
	}
	names := make([]string, 0, len(a.reg.Specs()))
	for _, s := range a.reg.Specs() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"plan_goal", "plan_plan", "plan_todo", "plan_status", "plan_list", "plan_remove"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"plan_goal", `{}`},                                               // missing required title
		{"plan_goal", `{"title":"x","extra":1}`},                          // additional properties rejected
		{"plan_plan", `{"title":"P"}`},                                    // missing required goal_id
		{"plan_plan", `{"goal_id":"g","title":123}`},                      // title must be a string
		{"plan_todo", `{}`},                                               // missing required plan_id/title
		{"plan_todo", `{"plan_id":"p","title":"t","x":1}`},                // additional properties rejected
		{"plan_status", `{}`},                                             // missing required scope/id/status
		{"plan_status", `{"scope":"goal","id":"g","status":"bogus"}`},     // status outside the enum
		{"plan_status", `{"scope":"widget","id":"g","status":"pending"}`}, // scope outside the enum
		{"plan_list", `{"extra":1}`},                                      // list takes no arguments
		{"plan_remove", `{"id":"g"}`},                                     // missing required scope
		{"plan_remove", `{"scope":"goal","id":123}`},                      // id must be a string
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid goal → plan → todo flow works and lands the plan/* events (D3).
	if _, err := a.reg.Execute(context.Background(), "plan_goal", json.RawMessage(`{"title":"Ship","objective":"ship the agent"}`)); err != nil {
		t.Fatalf("plan_goal via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventPlanCreate) {
		t.Fatal("plan/create event missing from the session log after plan_goal")
	}
	if _, err := a.reg.Execute(context.Background(), "plan_plan", json.RawMessage(`{"goal_id":"goal-1","title":"Code","steps":["write","test"]}`)); err != nil {
		t.Fatalf("plan_plan via registry: %v", err)
	}
	if _, err := a.reg.Execute(context.Background(), "plan_todo", json.RawMessage(`{"plan_id":"plan-1","title":"self-review"}`)); err != nil {
		t.Fatalf("plan_todo via registry: %v", err)
	}
	if _, err := a.reg.Execute(context.Background(), "plan_status", json.RawMessage(`{"scope":"goal","id":"goal-1","status":"in-progress"}`)); err != nil {
		t.Fatalf("plan_status via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventPlanStatus) {
		t.Fatal("plan/status event missing from the session log after plan_status")
	}
	res, err := a.reg.Execute(context.Background(), "plan_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("plan_list via registry: %v", err)
	}
	if !strings.Contains(res.Output, "plan-1: Code (pending)") {
		t.Fatalf("plan_list output lacks the plan tree:\n%s", res.Output)
	}
	if !hasEvent(a.log, session.EventPlanList) {
		t.Fatal("plan/list event missing from the session log after plan_list")
	}
	// The plan/* rows are log-only: no derived messages.
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("plan/* events must not derive into messages: %+v", msgs)
	}
	// plan_remove removes and lands plan/delete (D3); unknown ids error.
	if _, err := a.reg.Execute(context.Background(), "plan_remove", json.RawMessage(`{"scope":"todo","id":"todo-3"}`)); err != nil {
		t.Fatalf("plan_remove via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventPlanDelete) {
		t.Fatal("plan/delete event missing from the session log after plan_remove")
	}
	if _, err := a.reg.Execute(context.Background(), "plan_remove", json.RawMessage(`{"scope":"goal","id":"goal-99"}`)); err == nil {
		t.Fatal("plan_remove of an unknown id must error")
	}
	if _, err := a.reg.Execute(context.Background(), "plan_status", json.RawMessage(`{"scope":"plan","id":"plan-99","status":"done"}`)); err == nil {
		t.Fatal("plan_status of an unknown id must error")
	}
}
