package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

func makePlanApp(planEnabled bool) *app {
	return &app{
		cfg: config.Config{Plan: config.PlanConfig{Enabled: config.Bool(planEnabled)}},
		reg: tools.New(),
		log: session.New(),
	}
}

func planPolicy() tools.Policy {
	return tools.Policy{Enabled: []string{"get_goal", "create_goal", "update_goal", "todo_write"}}
}

func TestRegisterPlansDisabledRegistersNothing(t *testing.T) {
	a := makePlanApp(false)
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	if a.plans != nil {
		t.Fatal("plan engine must be nil when plan.enabled=false")
	}
	if len(a.reg.Specs()) != 0 {
		t.Fatalf("disabled goal/todo tools registered: %+v", a.reg.Specs())
	}
}

func TestRegisterPlansEnabledRegistersDSHTools(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	want := map[string]bool{"get_goal": true, "create_goal": true, "update_goal": true, "todo_write": true}
	for _, spec := range a.reg.Specs() {
		delete(want, spec.Name)
		if strings.HasPrefix(spec.Name, "plan_") {
			t.Fatalf("legacy plan tool %q registered", spec.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("registered tools lack %v", want)
	}
	if _, err := a.reg.Execute(context.Background(), "create_goal", json.RawMessage(`{}`)); err == nil {
		t.Fatal("create_goal must reject missing objective")
	}
	created, err := a.reg.Execute(context.Background(), "create_goal", json.RawMessage(`{"objective":"Ship the agent","max_goal_rounds":3}`))
	if err != nil {
		t.Fatalf("create_goal: %v", err)
	}
	if !strings.Contains(created.Output, "goal-1") || !hasEvent(a.log, session.EventPlanCreate) {
		t.Fatalf("create_goal output/events = %q, %+v", created.Output, a.log.Events())
	}
	current, err := a.reg.Execute(context.Background(), "get_goal", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(current.Output, "Ship the agent") {
		t.Fatalf("get_goal = %q, err=%v", current.Output, err)
	}
	var goal struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal([]byte(current.Output), &goal); err != nil {
		t.Fatal(err)
	}
	updated, err := a.reg.Execute(context.Background(), "update_goal", json.RawMessage(`{"goal_id":"`+goal.ID+`","revision":`+jsonInt(goal.Revision)+`,"action":"complete"}`))
	if err != nil || !strings.Contains(updated.Output, `"status":"done"`) {
		t.Fatalf("update_goal complete = %q, err=%v", updated.Output, err)
	}
	todos, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[{"content":"Verify","status":"in_progress"}]}`))
	if err != nil || !strings.Contains(todos.Output, "Verify") {
		t.Fatalf("todo_write = %q, err=%v", todos.Output, err)
	}
}

func jsonInt(n int) string { return strings.TrimSpace(string(mustJSON(n))) }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestPlanGoalProjectionRebuildsAcrossAppRestart(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	first := makePlanApp(true)
	first.store, first.baseCtx = st, ctx
	first.reg.SetPolicy(planPolicy())
	if err := first.registerPlans(); err != nil {
		t.Fatal(err)
	}
	if err := first.newSession(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := first.reg.Execute(ctx, "create_goal", json.RawMessage(`{"objective":"durable"}`)); err != nil {
		t.Fatal(err)
	}
	sessionID := first.currentID
	first.plans.Close()
	second := makePlanApp(true)
	second.store, second.baseCtx = st, ctx
	second.reg.SetPolicy(planPolicy())
	if err := second.registerPlans(); err != nil {
		t.Fatal(err)
	}
	defer second.plans.Close()
	if err := second.resumeSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	goals, err := second.plans.List(ctx)
	if err != nil || len(goals) != 1 || goals[0].ID != "goal-1" {
		t.Fatalf("restored goals = %+v, err=%v", goals, err)
	}
}
