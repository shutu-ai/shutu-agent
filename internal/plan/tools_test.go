package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// eventRec is one event emitted through the PlanTools onEvent sink.
type eventRec struct {
	typ  string
	data any
}

// newPlanToolsWithEvents returns an engine and a PlanTools bundle wired to a
// slice that records every emitted plan/* event (the composition root wires the
// same sink to the session log in cmd/pa, D3).
func newPlanToolsWithEvents(t *testing.T) (*engine, *PlanTools, *[]eventRec) {
	t.Helper()
	e := NewEngine(nil)
	t.Cleanup(func() { e.Close() })
	recs := &[]eventRec{}
	pt := NewPlanTools(e, func(typ string, data any) {
		*recs = append(*recs, eventRec{typ: typ, data: data})
	})
	return e, pt, recs
}

// execTool is the subset of tools.Tool the plan tools implement structurally.
type execTool interface {
	Execute(ctx context.Context, args any) (string, error)
}

// mustExec runs one tool Execute and fails the test on error.
func mustExec(t *testing.T, tool execTool, args string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return out
}

// mustFail runs one tool Execute and asserts it fails; the error message (not a
// panic) is the contract for unknown ids / invalid arguments (dispatch-m6b-2
// §3).
func mustFail(t *testing.T, tool execTool, args, wantSub string) {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("Execute(%s) = %q, want an error containing %q", args, out, wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("Execute(%s) error = %q, want it to contain %q", args, err, wantSub)
	}
}

// decodeEvent unmarshals a captured event payload into T (the session payloads
// are plain JSON-serializable data).
func decodeEvent[T any](t *testing.T, ev eventRec) T {
	t.Helper()
	raw, err := json.Marshal(ev.data)
	if err != nil {
		t.Fatalf("marshal %s event data: %v", ev.typ, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s event data %s: %v", ev.typ, raw, err)
	}
	return out
}

// eventTypes returns the emitted event types in order.
func eventTypes(recs []eventRec) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.typ)
	}
	return out
}

func TestPlanGoalToolCreatesAndEmits(t *testing.T) {
	_, pt, recs := newPlanToolsWithEvents(t)
	out := mustExec(t, pt.Goal(), `{"title":"Ship the agent","objective":"Make the personal agent usable end to end"}`)
	if !strings.Contains(out, "created goal goal-1") {
		t.Fatalf("plan_goal output = %q, want created goal goal-1", out)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "plan/create" {
		t.Fatalf("emitted types = %v, want [plan/create]", got)
	}
	d := decodeEvent[struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
		Title string `json:"title"`
	}](t, (*recs)[0])
	if d.Scope != "goal" || d.ID != "goal-1" || d.Title != "Ship the agent" {
		t.Fatalf("plan/create payload = %+v, want scope goal/id goal-1/title", d)
	}
}

func TestPlanGoalToolRejectsEmptyTitle(t *testing.T) {
	_, pt, recs := newPlanToolsWithEvents(t)
	mustFail(t, pt.Goal(), `{"title":"   "}`, "empty title")
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed create, got %v", eventTypes(*recs))
	}
}

// TestPlanToolsBuildTreeListAndStatus drives the whole tree bottom-up through
// the tools and asserts the events and the plan_list aggregation tree.
func TestPlanToolsBuildTreeListAndStatus(t *testing.T) {
	_, pt, recs := newPlanToolsWithEvents(t)

	mustExec(t, pt.Goal(), `{"title":"Ship","objective":"ship it"}`) // goal-1, plan/create(goal)
	mustExec(t, pt.Goal(), `{"title":"Read"}`)                       // goal-2, plan/create(goal)
	out := mustExec(t, pt.Plan(), `{"goal_id":"goal-1","title":"Code","steps":["write","test"]}`)
	if !strings.Contains(out, "created plan plan-1 under goal goal-1: Code (2 steps)") {
		t.Fatalf("plan_plan output = %q", out)
	}
	out = mustExec(t, pt.Plan(), `{"goal_id":"goal-2","title":"Notes","steps":["skim"]}`)
	if !strings.Contains(out, "plan-2") {
		t.Fatalf("plan_plan output = %q, want plan-2", out)
	}
	out = mustExec(t, pt.Todo(), `{"plan_id":"plan-1","title":"self-review"}`)
	if !strings.Contains(out, "added todo todo-4 to plan plan-1: self-review") {
		t.Fatalf("plan_todo output = %q", out)
	}
	out = mustExec(t, pt.Status(), `{"scope":"goal","id":"goal-1","status":"in-progress"}`)
	if !strings.Contains(out, "set goal goal-1 status to in-progress") {
		t.Fatalf("plan_status output = %q", out)
	}

	// plan_list returns the aggregation tree: goals → plans → todos.
	out = mustExec(t, pt.List(), `{}`)
	for _, want := range []string{"goal-1: Ship (in-progress)", "objective: ship it", "plan-1: Code (pending)", "todo-1: write (pending)", "todo-2: test (pending)", "todo-4: self-review (pending)", "goal-2: Read (pending)", "plan-2: Notes (pending)", "todo-3: skim (pending)"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan_list output lacks %q:\n%s", want, out)
		}
	}

	// Event order and payloads.
	wantTypes := []string{
		"plan/create", "plan/create", "plan/create", "plan/create", "plan/create",
		"plan/status", "plan/list",
	}
	if got := eventTypes(*recs); !equalStrings(got, wantTypes) {
		t.Fatalf("emitted types = %v, want %v", got, wantTypes)
	}
	// plan/create for the plan/todo levels carries the right scope.
	planCreate := decodeEvent[struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
	}](t, (*recs)[2]) // plan-1
	if planCreate.Scope != "plan" || planCreate.ID != "plan-1" {
		t.Fatalf("plan create(plan) payload = %+v", planCreate)
	}
	todoCreate := decodeEvent[struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
	}](t, (*recs)[4]) // todo-4
	if todoCreate.Scope != "todo" || todoCreate.ID != "todo-4" {
		t.Fatalf("plan create(todo) payload = %+v", todoCreate)
	}
	// plan/status carries scope/id/status.
	status := decodeEvent[struct {
		Scope  string `json:"scope"`
		ID     string `json:"id"`
		Status string `json:"status"`
	}](t, (*recs)[5])
	if status.Scope != "goal" || status.ID != "goal-1" || status.Status != "in-progress" {
		t.Fatalf("plan/status payload = %+v", status)
	}
	// plan/list carries the goal count.
	list := decodeEvent[struct {
		Count int `json:"count"`
	}](t, (*recs)[6])
	if list.Count != 2 {
		t.Fatalf("plan/list count = %d, want 2", list.Count)
	}
}

// TestPlanTodoToolAcceptance verifies the eval seam (ADR D-EVAL-4) on plan_todo:
// the schema declares the optional acceptance list and Execute with acceptance
// stores it on the todo, returns it in the plan/create payload, and it reads
// back through the engine.
func TestPlanTodoToolAcceptance(t *testing.T) {
	e, pt, recs := newPlanToolsWithEvents(t)
	mustExec(t, pt.Goal(), `{"title":"G","objective":"o"}`)               // goal-1
	mustExec(t, pt.Plan(), `{"goal_id":"goal-1","title":"P","steps":[]}`) // plan-1

	// Schema declares the acceptance property (optional array of strings).
	schema := toolSchema(pt.Todo())
	props, _ := schema["properties"].(map[string]any)
	acc, ok := props["acceptance"].(map[string]any)
	if !ok {
		t.Fatal("plan_todo schema lacks the acceptance property")
	}
	if acc["type"] != "array" {
		t.Errorf("acceptance schema type = %v, want array", acc["type"])
	}

	want := []string{"contains:输出包含报告", "llm:结论合理"}
	out := mustExec(t, pt.Todo(), `{"plan_id":"plan-1","title":"step","acceptance":["contains:输出包含报告","llm:结论合理"]}`)
	if !strings.Contains(out, "added todo todo-1 to plan plan-1: step") {
		t.Fatalf("plan_todo output = %q", out)
	}
	// The plan/create payload carries the todo's acceptance list.
	create := decodeEvent[struct {
		Scope      string   `json:"scope"`
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Acceptance []string `json:"acceptance"`
	}](t, (*recs)[len(*recs)-1])
	if create.Scope != "todo" || create.ID != "todo-1" || !equalStrings(create.Acceptance, want) {
		t.Fatalf("plan/create(todo) payload = %+v, want scope todo / todo-1 / acceptance %v", create, want)
	}
	// The stored todo's Acceptance reads back through the engine.
	got := getPlan(t, e, "plan-1")
	if len(got.Steps) != 1 || !equalStrings(got.Steps[0].Acceptance, want) {
		t.Fatalf("plan.Steps[0].Acceptance read back = %v, want %v", got.Steps[0].Acceptance, want)
	}
}

func TestPlanRemoveToolEmitsAndCascades(t *testing.T) {
	e, pt, recs := newPlanToolsWithEvents(t)
	mustExec(t, pt.Goal(), `{"title":"G1","objective":"o"}`)                      // goal-1
	mustExec(t, pt.Plan(), `{"goal_id":"goal-1","title":"P1","steps":["a","b"]}`) // plan-1
	out := mustExec(t, pt.Remove(), `{"scope":"plan","id":"plan-1"}`)
	if !strings.Contains(out, "removed plan plan-1") {
		t.Fatalf("plan_remove output = %q", out)
	}
	del := decodeEvent[struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
	}](t, (*recs)[len(*recs)-1])
	if del.Scope != "plan" || del.ID != "plan-1" {
		t.Fatalf("plan/delete payload = %+v", del)
	}
	// plan-1 is gone; goal-1 still lists no plans (detached).
	if _, err := e.prov.GetPlan(context.Background(), "plan-1"); err == nil {
		t.Fatal("removed plan-1 still readable")
	}
	goals, _ := e.List(context.Background())
	if len(goals) != 1 || len(goals[0].Plans) != 0 {
		t.Fatalf("goal tree after plan removal = %+v, want goal-1 with no plans", goals)
	}
	// Removing a goal cascades to its plans.
	mustExec(t, pt.Remove(), `{"scope":"goal","id":"goal-1"}`)
	if _, err := e.prov.GetGoal(context.Background(), "goal-1"); err == nil {
		t.Fatal("removed goal-1 still readable")
	}
}

// TestPlanToolsRejectUnknownAndInvalid verifies the error-message (never panic)
// contract for unknown ids, unknown scopes and invalid statuses, and that a
// failed call emits no event.
func TestPlanToolsRejectUnknownAndInvalid(t *testing.T) {
	_, pt, recs := newPlanToolsWithEvents(t)
	mustExec(t, pt.Goal(), `{"title":"G","objective":"o"}`) // goal-1
	mustExec(t, pt.Plan(), `{"goal_id":"goal-1","title":"P","steps":["s"]}`)

	// Unknown goal on plan_plan.
	mustFail(t, pt.Plan(), `{"goal_id":"goal-missing","title":"P","steps":[]}`, "unknown goal")
	// Unknown plan on plan_todo.
	mustFail(t, pt.Todo(), `{"plan_id":"plan-missing","title":"t"}`, "unknown plan")
	// Unknown scope / invalid status on plan_status.
	mustFail(t, pt.Status(), `{"scope":"bogus","id":"goal-1","status":"pending"}`, "unknown scope")
	mustFail(t, pt.Status(), `{"scope":"goal","id":"goal-1","status":"bogus"}`, "invalid status")
	// Unknown id on plan_status.
	mustFail(t, pt.Status(), `{"scope":"goal","id":"goal-missing","status":"done"}`, "unknown goal")
	// Unknown scope / id on plan_remove.
	mustFail(t, pt.Remove(), `{"scope":"bogus","id":"goal-1"}`, "unknown scope")
	mustFail(t, pt.Remove(), `{"scope":"goal","id":"goal-missing"}`, "unknown goal")
	// Unknown todo on plan_remove.
	mustFail(t, pt.Remove(), `{"scope":"todo","id":"todo-missing"}`, "unknown todo")

	// None of the failures may have emitted an event.
	if got := eventTypes(*recs); !equalStrings(got, []string{"plan/create", "plan/create"}) {
		t.Fatalf("failed calls emitted events: %v, want only the two creates", got)
	}
}

// TestPlanToolsSchemasEnforceD7 asserts each tool's schema declares
// additionalProperties: false and the required argument names (D7 is enforced
// at the registry Execute gate; these checks pin the schema shapes the tools
// register).
func TestPlanToolsSchemasEnforceD7(t *testing.T) {
	_, pt, _ := newPlanToolsWithEvents(t)
	for _, tc := range []struct {
		tool    execTool
		require []string
	}{
		{pt.Goal(), []string{"title"}},
		{pt.Plan(), []string{"goal_id", "title"}},
		{pt.Todo(), []string{"plan_id", "title"}},
		{pt.Status(), []string{"scope", "id", "status"}},
		{pt.List(), nil},
		{pt.Remove(), []string{"scope", "id"}},
	} {
		schema := toolSchema(tc.tool)
		if schema["type"] != "object" {
			t.Errorf("%T schema type = %v, want object", tc.tool, schema["type"])
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%T schema must set additionalProperties:false (D7)", tc.tool)
		}
		req, _ := schema["required"].([]string)
		if len(req) != len(tc.require) {
			t.Errorf("%T required = %v, want %v", tc.tool, req, tc.require)
			continue
		}
		for _, want := range tc.require {
			found := false
			for _, r := range req {
				if r == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%T required %v lacks %q", tc.tool, req, want)
			}
		}
	}
}

// toolSchema returns the JSON Schema of an execTool (mirrors the tools package
// loading a Tool.Schema(); here we only need the raw map).
func toolSchema(tool execTool) map[string]any {
	if s, ok := tool.(interface{ Schema() map[string]any }); ok {
		return s.Schema()
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
