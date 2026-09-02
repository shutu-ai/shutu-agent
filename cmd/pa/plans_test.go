package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

func makePlanApp(planEnabled bool) *app {
	return &app{
		cfg:       config.Config{Plan: config.PlanConfig{Enabled: config.Bool(planEnabled)}},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-plan",
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
		Goal struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
		} `json:"goal"`
	}
	if err := json.Unmarshal([]byte(current.Output), &goal); err != nil {
		t.Fatal(err)
	}
	updated, err := a.reg.Execute(context.Background(), "update_goal", json.RawMessage(`{"goal_id":"`+goal.Goal.ID+`","revision":`+jsonInt(goal.Goal.Revision)+`,"action":"complete"}`))
	if err != nil || !strings.Contains(updated.Output, `"phase":"complete"`) {
		t.Fatalf("update_goal complete = %q, err=%v", updated.Output, err)
	}
	todos, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[{"content":"Verify","status":"in_progress"}]}`))
	if err != nil || todos.Output != "Updated todo list: 0 pending, 1 in progress, 0 completed." {
		t.Fatalf("todo_write = %q, err=%v", todos.Output, err)
	}
}

func TestTodoWriteMatchesDshValidationAndStructuredResult(t *testing.T) {
	a := makePlanApp(true)
	a.cfg.Plan.AllowParallelInProgress = config.Bool(false)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	duplicate, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[{"content":" inspect ","status":"pending"},{"content":"inspect","status":"completed"}]}`))
	if err != nil {
		t.Fatalf("duplicate call transport error: %v", err)
	}
	if !duplicate.IsError || !strings.Contains(duplicate.Output, `duplicate content "inspect"`) {
		t.Fatalf("duplicate result = %#v", duplicate)
	}
	multiple, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[{"content":"one","status":"in_progress"},{"content":"two","status":"in_progress"}]}`))
	if err != nil {
		t.Fatalf("multiple active transport error: %v", err)
	}
	if !multiple.IsError || !strings.Contains(multiple.Output, "at most one task may be in_progress") {
		t.Fatalf("multiple active result = %#v", multiple)
	}
	if _, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[{"content":"one","status":"pending"},{"content":"two","status":"completed"}]}`)); err != nil {
		t.Fatalf("valid todo_write: %v", err)
	}
	valid, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[]}`))
	if err != nil {
		t.Fatalf("empty todo_write: %v", err)
	}
	if valid.Output != "Updated todo list: 0 pending, 0 in progress, 0 completed." {
		t.Fatalf("empty result = %q", valid.Output)
	}
}

func TestTodoWriteRequiresOwningAgentSession(t *testing.T) {
	a := makePlanApp(true)
	a.currentID = ""
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	result, err := a.reg.Execute(context.Background(), "todo_write", json.RawMessage(`{"todos":[]}`))
	if err != nil {
		t.Fatalf("todo_write transport error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Output, "requires an owning agent session") {
		t.Fatalf("owner result = %#v", result)
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

func TestPlanProjectionIsSessionScopedForAgentContexts(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatal(err)
	}
	defer a.closePlanEngines()
	one := session.New()
	if _, err := one.Append(session.EventPlanCreate, session.NewPlanCreate("goal", "goal-1", "first", nil)); err != nil {
		t.Fatal(err)
	}
	two := session.New()
	a.currentID = "legacy"
	a.log = one
	a.runtimeMu.Lock()
	a.runtimeLogs = map[string]*session.Log{"s-one": one, "s-two": two}
	a.runtimeMu.Unlock()
	ctxOne := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "s-one"})
	ctxTwo := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "s-two"})
	first, err := a.reg.Execute(ctxOne, "get_goal", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(first.Output, "goal-1") {
		t.Fatalf("session one goal = %#v, err=%v", first, err)
	}
	second, err := a.reg.Execute(ctxTwo, "create_goal", json.RawMessage(`{"objective":"second"}`))
	if err != nil || !strings.Contains(second.Output, "goal-1") {
		t.Fatalf("session two goal = %#v, err=%v", second, err)
	}
	if strings.Contains(second.Output, "first") {
		t.Fatalf("session two reused session one's plan projection: %q", second.Output)
	}
	engineOne, err := a.planEngineFor(ctxOne)
	if err != nil {
		t.Fatal(err)
	}
	engineTwo, err := a.planEngineFor(ctxTwo)
	if err != nil {
		t.Fatal(err)
	}
	goalsOne, _ := engineOne.List(ctxOne)
	goalsTwo, _ := engineTwo.List(ctxTwo)
	if len(goalsOne) != 1 || len(goalsTwo) != 1 || goalsOne[0].Title == goalsTwo[0].Title {
		t.Fatalf("session plan projections leaked: one=%+v two=%+v", goalsOne, goalsTwo)
	}
}
