package plan

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// Compile-time assertions: engine implements the Engine Service and memProvider
// implements the Provider interface.
var _ Engine = (*engine)(nil)
var _ Provider = (*memProvider)(nil)

// newTestEngine returns a fresh engine backed by the default in-memory provider.
func newTestEngine(t *testing.T) *engine {
	t.Helper()
	e := NewEngine(nil)
	t.Cleanup(func() { e.Close() })
	return e
}

// getGoal reads a goal through the engine's provider (the Engine interface has
// no single-record goal getter — reads go through List and the Provider).
func getGoal(t *testing.T, e *engine, id string) Goal {
	t.Helper()
	g, err := e.prov.GetGoal(context.Background(), id)
	if err != nil {
		t.Fatalf("provider GetGoal(%s): %v", id, err)
	}
	return g
}

// getPlan reads a plan through the engine's provider.
func getPlan(t *testing.T, e *engine, id string) Plan {
	t.Helper()
	p, err := e.prov.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("provider GetPlan(%s): %v", id, err)
	}
	return p
}

// --- CreateGoal / CreatePlan / AddTodo --------------------------------------

func TestEngineCreateGoalPlanTodo(t *testing.T) {
	e := newTestEngine(t)

	g, err := e.CreateGoal(context.Background(), "Ship the agent", "Make the Shutu Agent usable end to end")
	if err != nil {
		t.Fatalf("CreateGoal: %v", err)
	}
	if g.ID == "" {
		t.Error("CreateGoal must issue a non-empty id")
	}
	if g.Status != StatusPending {
		t.Errorf("new goal status = %q, want %q", g.Status, StatusPending)
	}
	if g.Objective != "Make the Shutu Agent usable end to end" {
		t.Errorf("goal objective = %q, want the given objective", g.Objective)
	}
	if len(g.Plans) != 0 {
		t.Errorf("new goal must have no plans, got %v", g.Plans)
	}
	if g.CompletedAt != nil {
		t.Error("new goal must have nil CompletedAt")
	}

	p, err := e.CreatePlan(context.Background(), g.ID, "Plan A", []string{"write code", "test", "ship"})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if p.ID == "" {
		t.Error("CreatePlan must issue a non-empty id")
	}
	if p.GoalID != g.ID {
		t.Errorf("plan GoalID = %q, want %q", p.GoalID, g.ID)
	}
	if p.Status != StatusPending {
		t.Errorf("new plan status = %q, want %q", p.Status, StatusPending)
	}
	if len(p.Steps) != 3 {
		t.Fatalf("plan has %d steps, want 3", len(p.Steps))
	}
	for i, want := range []string{"write code", "test", "ship"} {
		st := p.Steps[i]
		if st.Title != want || st.Status != StatusPending {
			t.Errorf("step %d = %+v, want title %q pending", i, st, want)
		}
	}

	todo, err := e.AddTodo(context.Background(), p.ID, "announce", nil)
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if todo.ID == "" {
		t.Error("AddTodo must issue a non-empty id")
	}
	if todo.Title != "announce" || todo.Status != StatusPending {
		t.Errorf("new todo = %+v, want title %q pending", todo, "announce")
	}

	// The goal aggregation must link the plan; the plan must carry the appended
	// todo.
	got := getGoal(t, e, g.ID)
	if !reflect.DeepEqual(got.Plans, []string{p.ID}) {
		t.Errorf("goal Plans = %v, want [%s]", got.Plans, p.ID)
	}
	plan2 := getPlan(t, e, p.ID)
	if len(plan2.Steps) != 4 || plan2.Steps[3].Title != "announce" {
		t.Errorf("plan Steps after AddTodo = %+v, want 4 steps ending with announce", plan2.Steps)
	}
}

func TestEngineNativeGoalEditUsesRevisionAndPreservesRoundBudget(t *testing.T) {
	e := newTestEngine(t)
	g, err := e.CreateGoal(context.Background(), "Ship", "ship the agent")
	if err != nil {
		t.Fatal(err)
	}
	maxRounds := 9
	updated, err := e.UpdateGoalIfRevision(context.Background(), g.ID, g.Revision, nil, &maxRounds)
	if err != nil {
		t.Fatalf("UpdateGoalIfRevision: %v", err)
	}
	if updated.Revision != 2 || updated.MaxRounds != maxRounds || updated.Objective != g.Objective {
		t.Fatalf("updated goal = %+v", updated)
	}
	if _, err := e.UpdateGoalIfRevision(context.Background(), g.ID, g.Revision, nil, &maxRounds); err == nil {
		t.Fatal("stale goal revision was accepted")
	}
}

func TestEngineCreatePlanStandalone(t *testing.T) {
	e := newTestEngine(t)
	p, err := e.CreatePlan(context.Background(), "", "Standalone", []string{"x"})
	if err != nil {
		t.Fatalf("CreatePlan(standalone): %v", err)
	}
	if p.GoalID != "" {
		t.Errorf("standalone plan GoalID = %q, want empty", p.GoalID)
	}
	// A standalone plan must not appear under any goal's tree.
	goals, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(goals) != 0 {
		t.Errorf("List returned %d goals, want 0 (standalone plan must not be in the tree)", len(goals))
	}
}

func TestEngineRestoreFromSessionEventsIsIdempotentAndReseedsIDs(t *testing.T) {
	e := newTestEngine(t)
	log := session.New()
	created := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	steps := []Todo{{ID: "todo-9", Title: "implement", Status: StatusPending, CreatedAt: created}}
	if _, err := log.Append(session.EventPlanCreate, session.NewPlanCreate("goal", "goal-7", "Ship", nil, map[string]any{
		"objective": "ship the durable tree", "status": StatusPending, "createdAt": created,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventPlanCreate, session.NewPlanCreate("plan", "plan-8", "Core", nil, map[string]any{
		"goalId": "goal-7", "status": StatusPending, "createdAt": created, "steps": steps,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventPlanCreate, session.NewPlanCreate("todo", "todo-9", "verify", []string{"contains:ok"}, map[string]any{
		"planId": "plan-8", "status": StatusPending, "createdAt": created,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventPlanStatus, session.NewPlanStatus("todo", "todo-9", string(StatusDone))); err != nil {
		t.Fatal(err)
	}
	if err := e.Restore(log.Events()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	firstGoals, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(firstGoals) != 1 || firstGoals[0].ID != "goal-7" || firstGoals[0].Plans[0] != "plan-8" {
		t.Fatalf("restored goals = %+v", firstGoals)
	}
	firstPlan := getPlan(t, e, "plan-8")
	if len(firstPlan.Steps) != 1 || firstPlan.Steps[0].ID != "todo-9" || firstPlan.Steps[0].Status != StatusDone {
		t.Fatalf("restored plan = %+v", firstPlan)
	}
	if firstPlan.Steps[0].Acceptance[0] != "contains:ok" || firstPlan.Steps[0].CompletedAt == nil {
		t.Fatalf("restored todo metadata = %+v", firstPlan.Steps[0])
	}
	if err := e.Restore(log.Events()); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	secondPlan := getPlan(t, e, "plan-8")
	if !reflect.DeepEqual(firstPlan, secondPlan) {
		t.Fatalf("Restore is not idempotent:\nfirst=%+v\nsecond=%+v", firstPlan, secondPlan)
	}
	newGoal, err := e.CreateGoal(context.Background(), "Next", "continue")
	if err != nil {
		t.Fatalf("CreateGoal after Restore: %v", err)
	}
	if newGoal.ID != "goal-8" {
		t.Fatalf("new goal id = %q, want goal-8", newGoal.ID)
	}
}

// TestAddTodoAcceptance verifies the eval seam (ADR D-EVAL-4): AddTodo stores
// the acceptance criteria list in order, hands back a fresh copy (the caller's
// slice is never aliased), and the list reads back through the aggregation
// tree (List → plan Steps).
func TestAddTodoAcceptance(t *testing.T) {
	e := newTestEngine(t)
	g, err := e.CreateGoal(context.Background(), "G", "o")
	if err != nil {
		t.Fatalf("CreateGoal: %v", err)
	}
	p, err := e.CreatePlan(context.Background(), g.ID, "P", nil)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	want := []string{"contains:输出包含报告", "llm:结论合理"}
	todo, err := e.AddTodo(context.Background(), p.ID, "step", want)
	if err != nil {
		t.Fatalf("AddTodo(acceptance): %v", err)
	}
	// Order-preserving copy on the returned todo.
	if !reflect.DeepEqual(todo.Acceptance, want) {
		t.Fatalf("todo.Acceptance = %v, want %v", todo.Acceptance, want)
	}
	// The stored list must not share its backing array with the caller's input:
	// mutating the input after the call must not leak into the engine's copy.
	input := []string{"contains:输出包含报告", "llm:结论合理"}
	if _, err := e.AddTodo(context.Background(), p.ID, "other", input); err != nil {
		t.Fatalf("AddTodo(acceptance 2): %v", err)
	}
	input[0] = "mutated"
	gotP := getPlan(t, e, p.ID)
	if got := gotP.Steps[1].Acceptance; !reflect.DeepEqual(got, []string{"contains:输出包含报告", "llm:结论合理"}) {
		t.Errorf("engine state leaked caller mutation: Acceptance = %v", got)
	}

	// Read back through the aggregation tree: List → goal Plans → plan Steps.
	goals, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(goals) != 1 || len(goals[0].Plans) != 1 {
		t.Fatalf("List tree = %+v, want goal-1 with one plan", goals)
	}
	plan := getPlan(t, e, goals[0].Plans[0])
	if got := plan.Steps[0].Acceptance; !reflect.DeepEqual(got, want) {
		t.Fatalf("plan.Steps[0].Acceptance read back = %v, want %v", got, want)
	}
}

// --- SetStatus --------------------------------------------------------------

func TestEngineSetStatus(t *testing.T) {
	e := newTestEngine(t)
	g, _ := e.CreateGoal(context.Background(), "G", "o")
	p, _ := e.CreatePlan(context.Background(), g.ID, "P", []string{"step"})
	todo := p.Steps[0]

	if err := e.SetStatus(context.Background(), "goal", g.ID, StatusDone); err != nil {
		t.Fatalf("SetStatus(goal, done): %v", err)
	}
	got := getGoal(t, e, g.ID)
	if got.Status != StatusDone || got.CompletedAt == nil {
		t.Errorf("goal after done = status %q CompletedAt %v, want done + stamped", got.Status, got.CompletedAt)
	}
	// Moving away from done clears CompletedAt.
	if err := e.SetStatus(context.Background(), "goal", g.ID, StatusInProgress); err != nil {
		t.Fatalf("SetStatus(goal, in-progress): %v", err)
	}
	got = getGoal(t, e, g.ID)
	if got.Status != StatusInProgress || got.CompletedAt != nil {
		t.Errorf("goal after in-progress = status %q CompletedAt %v, want in-progress + cleared", got.Status, got.CompletedAt)
	}

	if err := e.SetStatus(context.Background(), "plan", p.ID, StatusBlocked); err != nil {
		t.Fatalf("SetStatus(plan, blocked): %v", err)
	}
	gotP := getPlan(t, e, p.ID)
	if gotP.Status != StatusBlocked {
		t.Errorf("plan status = %q, want %q", gotP.Status, StatusBlocked)
	}

	if err := e.SetStatus(context.Background(), "todo", todo.ID, StatusDone); err != nil {
		t.Fatalf("SetStatus(todo, done): %v", err)
	}
	gotP = getPlan(t, e, p.ID)
	if gotP.Steps[0].Status != StatusDone || gotP.Steps[0].CompletedAt == nil {
		t.Errorf("todo after done = %+v, want done + stamped", gotP.Steps[0])
	}
	if err := e.SetStatus(context.Background(), "todo", todo.ID, StatusPending); err != nil {
		t.Fatalf("SetStatus(todo, pending): %v", err)
	}
	gotP = getPlan(t, e, p.ID)
	if gotP.Steps[0].Status != StatusPending || gotP.Steps[0].CompletedAt != nil {
		t.Errorf("todo after pending = %+v, want pending + cleared", gotP.Steps[0])
	}
}

func TestEngineSetStatusRejects(t *testing.T) {
	e := newTestEngine(t)
	g, _ := e.CreateGoal(context.Background(), "G", "o")
	p, _ := e.CreatePlan(context.Background(), g.ID, "P", []string{"step"})
	todo := p.Steps[0]

	// Invalid status is rejected regardless of scope.
	for _, bad := range []Status{"bogus", "Done", "", "done "} {
		if err := e.SetStatus(context.Background(), "goal", g.ID, bad); !errors.Is(err, ErrInvalidStatus) {
			t.Errorf("SetStatus(bad status %q): err = %v, want ErrInvalidStatus", bad, err)
		}
	}
	if err := e.SetStatus(context.Background(), "todo", todo.ID, "nope"); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("SetStatus(todo, bad): err = %v, want ErrInvalidStatus", err)
	}

	// Unknown ids are rejected per scope.
	if err := e.SetStatus(context.Background(), "goal", "goal-missing", StatusPending); !errors.Is(err, ErrUnknownGoal) {
		t.Errorf("SetStatus(unknown goal): err = %v, want ErrUnknownGoal", err)
	}
	if err := e.SetStatus(context.Background(), "plan", "plan-missing", StatusPending); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("SetStatus(unknown plan): err = %v, want ErrUnknownPlan", err)
	}
	if err := e.SetStatus(context.Background(), "todo", "todo-missing", StatusPending); !errors.Is(err, ErrUnknownTodo) {
		t.Errorf("SetStatus(unknown todo): err = %v, want ErrUnknownTodo", err)
	}

	// Unknown scope is rejected.
	if err := e.SetStatus(context.Background(), "bogus-scope", g.ID, StatusPending); !errors.Is(err, ErrUnknownScope) {
		t.Errorf("SetStatus(unknown scope): err = %v, want ErrUnknownScope", err)
	}
}

// --- List aggregation tree --------------------------------------------------

func TestEngineListTree(t *testing.T) {
	e := newTestEngine(t)

	// Goal ordering: creation order also equals id order (goal-1, goal-2).
	g1, _ := e.CreateGoal(context.Background(), "Ship", "ship the agent")
	g2, _ := e.CreateGoal(context.Background(), "Read", "read the spec")

	p1, _ := e.CreatePlan(context.Background(), g1.ID, "Code", []string{"write", "test"})
	p2, _ := e.CreatePlan(context.Background(), g1.ID, "Release", []string{"tag"})
	p3, _ := e.CreatePlan(context.Background(), g2.ID, "Notes", []string{"skim"})
	todo, _ := e.AddTodo(context.Background(), p1.ID, "self-review", nil)
	standalone, _ := e.CreatePlan(context.Background(), "", "Standalone", []string{"x"})

	goals, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(goals) != 2 {
		t.Fatalf("List returned %d goals, want 2", len(goals))
	}
	// Goals sorted by id: goal-1 (Ship) before goal-2 (Read).
	if goals[0].ID != g1.ID || goals[1].ID != g2.ID {
		t.Fatalf("List goal order = [%s %s], want [%s %s]", goals[0].ID, goals[1].ID, g1.ID, g2.ID)
	}

	// Plans within a goal are listed in creation order.
	if !reflect.DeepEqual(goals[0].Plans, []string{p1.ID, p2.ID}) {
		t.Errorf("g1 Plans = %v, want [%s %s] in creation order", goals[0].Plans, p1.ID, p2.ID)
	}
	if !reflect.DeepEqual(goals[1].Plans, []string{p3.ID}) {
		t.Errorf("g2 Plans = %v, want [%s]", goals[1].Plans, p3.ID)
	}

	// Todos within a plan are ordered: created steps first, appended todo last.
	gotP1 := getPlan(t, e, p1.ID)
	wantSteps := []string{"write", "test", "self-review"}
	if len(gotP1.Steps) != len(wantSteps) {
		t.Fatalf("p1 Steps length = %d, want %d", len(gotP1.Steps), len(wantSteps))
	}
	for i, want := range wantSteps {
		if gotP1.Steps[i].Title != want {
			t.Errorf("p1 step %d = %q, want %q", i, gotP1.Steps[i].Title, want)
		}
	}

	// The appended todo must have been issued a fresh distinct id.
	if todo.ID == p1.Steps[0].ID || todo.ID == p1.Steps[1].ID {
		t.Errorf("AddTodo re-used an existing todo id %q", todo.ID)
	}
	if standalone.ID == "" {
		t.Error("standalone plan must have a non-empty id")
	}
}

// --- Remove -----------------------------------------------------------------

func TestEngineRemoveCascade(t *testing.T) {
	e := newTestEngine(t)

	// Removing a goal cascades to its plans and todos.
	g1, _ := e.CreateGoal(context.Background(), "G1", "o")
	p1, _ := e.CreatePlan(context.Background(), g1.ID, "P1", []string{"a", "b"})
	p1b, _ := e.CreatePlan(context.Background(), g1.ID, "P1b", []string{"c"})
	if err := e.Remove(context.Background(), "goal", g1.ID); err != nil {
		t.Fatalf("Remove(goal): %v", err)
	}
	goals, _ := e.List(context.Background())
	if len(goals) != 0 {
		t.Errorf("List after goal removal = %d goals, want 0", len(goals))
	}
	if _, err := e.prov.GetPlan(context.Background(), p1.ID); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("cascaded plan %s still readable: err = %v, want ErrUnknownPlan", p1.ID, err)
	}
	if _, err := e.prov.GetPlan(context.Background(), p1b.ID); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("cascaded plan %s still readable: err = %v, want ErrUnknownPlan", p1b.ID, err)
	}

	// Removing a plan detaches it from its goal's Plans list.
	g2, _ := e.CreateGoal(context.Background(), "G2", "o")
	p2, _ := e.CreatePlan(context.Background(), g2.ID, "P2", []string{"x"})
	p2b, _ := e.CreatePlan(context.Background(), g2.ID, "P2b", []string{"y"})
	if err := e.Remove(context.Background(), "plan", p2.ID); err != nil {
		t.Fatalf("Remove(plan): %v", err)
	}
	got := getGoal(t, e, g2.ID)
	if !reflect.DeepEqual(got.Plans, []string{p2b.ID}) {
		t.Errorf("goal Plans after plan removal = %v, want [%s]", got.Plans, p2b.ID)
	}
	if _, err := e.prov.GetPlan(context.Background(), p2.ID); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("removed plan still readable: err = %v, want ErrUnknownPlan", err)
	}

	// Removing a todo deletes it from its owning plan's Steps.
	g3, _ := e.CreateGoal(context.Background(), "G3", "o")
	p3, _ := e.CreatePlan(context.Background(), g3.ID, "P3", []string{"keep"})
	todo, _ := e.AddTodo(context.Background(), p3.ID, "drop", nil)
	if err := e.Remove(context.Background(), "todo", todo.ID); err != nil {
		t.Fatalf("Remove(todo): %v", err)
	}
	gotP := getPlan(t, e, p3.ID)
	if len(gotP.Steps) != 1 || gotP.Steps[0].ID != p3.Steps[0].ID {
		t.Errorf("plan Steps after todo removal = %+v, want only the kept step", gotP.Steps)
	}

	// Unknown ids and scopes are rejected.
	if err := e.Remove(context.Background(), "goal", "goal-missing"); !errors.Is(err, ErrUnknownGoal) {
		t.Errorf("Remove(unknown goal): err = %v, want ErrUnknownGoal", err)
	}
	if err := e.Remove(context.Background(), "plan", "plan-missing"); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("Remove(unknown plan): err = %v, want ErrUnknownPlan", err)
	}
	if err := e.Remove(context.Background(), "todo", "todo-missing"); !errors.Is(err, ErrUnknownTodo) {
		t.Errorf("Remove(unknown todo): err = %v, want ErrUnknownTodo", err)
	}
	if err := e.Remove(context.Background(), "bogus-scope", g3.ID); !errors.Is(err, ErrUnknownScope) {
		t.Errorf("Remove(unknown scope): err = %v, want ErrUnknownScope", err)
	}
}

func TestEngineRemoveStandalonePlan(t *testing.T) {
	e := newTestEngine(t)
	p, _ := e.CreatePlan(context.Background(), "", "Standalone", []string{"x"})
	if err := e.Remove(context.Background(), "plan", p.ID); err != nil {
		t.Fatalf("Remove(standalone plan): %v", err)
	}
	if _, err := e.prov.GetPlan(context.Background(), p.ID); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("removed standalone plan still readable: err = %v, want ErrUnknownPlan", err)
	}
}

// --- Validation: unknown references on create -------------------------------

func TestEngineRejectsUnknownReferences(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.CreatePlan(context.Background(), "goal-missing", "P", nil); !errors.Is(err, ErrUnknownGoal) {
		t.Errorf("CreatePlan(unknown goal): err = %v, want ErrUnknownGoal", err)
	}
	if _, err := e.AddTodo(context.Background(), "plan-missing", "t", nil); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("AddTodo(unknown plan): err = %v, want ErrUnknownPlan", err)
	}
	if _, err := e.CreateGoal(context.Background(), "", "o"); !errors.Is(err, ErrInvalidTitle) {
		t.Errorf("CreateGoal(empty title): err = %v, want ErrInvalidTitle", err)
	}
	if _, err := e.CreatePlan(context.Background(), "", "", nil); !errors.Is(err, ErrInvalidTitle) {
		t.Errorf("CreatePlan(empty title): err = %v, want ErrInvalidTitle", err)
	}
	if _, err := e.AddTodo(context.Background(), "", "t", nil); !errors.Is(err, ErrUnknownPlan) {
		t.Errorf("AddTodo(empty plan id): err = %v, want ErrUnknownPlan", err)
	}
}

// --- Close ------------------------------------------------------------------

func TestEngineCloseIdempotent(t *testing.T) {
	e := NewEngine(nil)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close must be idempotent, got %v", err)
	}

	if _, err := e.CreateGoal(context.Background(), "G", "o"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("CreateGoal after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.CreatePlan(context.Background(), "g", "P", nil); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("CreatePlan after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.AddTodo(context.Background(), "p", "t", nil); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("AddTodo after Close: err = %v, want ErrEngineClosed", err)
	}
	if err := e.SetStatus(context.Background(), "goal", "g", StatusPending); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("SetStatus after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.List(context.Background()); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("List after Close: err = %v, want ErrEngineClosed", err)
	}
	if err := e.Remove(context.Background(), "goal", "g"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Remove after Close: err = %v, want ErrEngineClosed", err)
	}
}

func TestEngineUsesInjectedProvider(t *testing.T) {
	p := NewMemProvider()
	e := NewEngine(p)
	defer e.Close()

	g, err := e.CreateGoal(context.Background(), "G", "o")
	if err != nil {
		t.Fatalf("CreateGoal: %v", err)
	}
	all, err := p.ListGoals(context.Background())
	if err != nil {
		t.Fatalf("provider ListGoals: %v", err)
	}
	if len(all) != 1 || all[0].ID != g.ID {
		t.Errorf("injected provider holds %+v, want the created goal", all)
	}
}
