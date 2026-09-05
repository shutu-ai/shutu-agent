// tools.go — the M6b-2 Consumer half of the plan seam (design.md §8 Consumer /
// D2, dispatch-m6b-2 §3): plan_goal, plan_plan, plan_todo, plan_status,
// plan_list and plan_remove are registered into the tools.Registry by the
// composition root (cmd/sta) when plan.enabled, and auto-whitelisted by
// config.applyDefaults the same way the job_*/subagent_*/skill_*/schedule_*
// tools are. They implement the tools.Tool method set structurally (Go
// structural typing), so this package never imports the tools package — the
// seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; plan_status/plan_remove restrict scope to the goal|plan|todo enum and
// plan_status restricts status to the five-status enum) before this code runs;
// the scope/status checks are repeated here so a direct call can never bypass
// them.
//
// D3 event logging follows the tool-layer decision (ADR 决策 M6b /
// dispatch-m6b-2 §3): plan_goal/plan_plan/plan_todo emit plan/create (scope
// tells which tree level), plan_status emits plan/status, plan_list emits
// plan/list, plan_remove emits plan/delete — all through the injected onEvent
// sink (the composition root wires it to the session log), and each append
// happens inside a tool Execute — the serial main-loop path (D5).
package plan

import (
	"context"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// Tool names (whitelisted when plan.enabled; see config.planToolNames).
const (
	ToolGoalName   = "plan_goal"
	ToolPlanName   = "plan_plan"
	ToolTodoName   = "plan_todo"
	ToolStatusName = "plan_status"
	ToolListName   = "plan_list"
	ToolRemoveName = "plan_remove"
)

// PlanTools bundles the shared state of the six plan_* tools: the Engine
// service and the event sink.
type PlanTools struct {
	e       Engine
	onEvent func(typ string, data any)
}

// NewPlanTools returns the shared plan-tool bundle bound to an Engine. onEvent,
// when non-nil, receives the plan/* event payloads; the composition root wires
// it to the session log (D3).
func NewPlanTools(e Engine, onEvent func(typ string, data any)) *PlanTools {
	return &PlanTools{e: e, onEvent: onEvent}
}

// Goal returns the plan_goal tool.
func (t *PlanTools) Goal() PlanGoalTool { return PlanGoalTool{t: t} }

// Plan returns the plan_plan tool.
func (t *PlanTools) Plan() PlanPlanTool { return PlanPlanTool{t: t} }

// Todo returns the plan_todo tool.
func (t *PlanTools) Todo() PlanTodoTool { return PlanTodoTool{t: t} }

// Status returns the plan_status tool.
func (t *PlanTools) Status() PlanStatusTool { return PlanStatusTool{t: t} }

// List returns the plan_list tool.
func (t *PlanTools) List() PlanListTool { return PlanListTool{t: t} }

// Remove returns the plan_remove tool.
func (t *PlanTools) Remove() PlanRemoveTool { return PlanRemoveTool{t: t} }

// emit forwards one plan/* event payload to the injected sink (D3).
func (t *PlanTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *PlanTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// readPlan fetches one plan for the plan_list tree renderer. The shipped
// *engine reaches its Provider package-internally (GetPlan), so the default
// wiring renders the full goal → plans → todos tree; a custom Engine without
// provider access degrades to the goal → plan-ids level. The ok-guard means a
// foreign Engine never panics (dispatch-m6b-2 §3: 未知 id/非法状态返回错误消息，非
// panic).
func (t *PlanTools) readPlan(ctx context.Context, id string) (Plan, bool) {
	if e, ok := t.e.(*engine); ok {
		p, err := e.prov.GetPlan(ctx, id)
		if err == nil {
			return p, true
		}
	}
	return Plan{}, false
}

// renderTree formats the goal → plans → todos aggregation tree as model-facing
// text: one "- <id>: <title> (<status>)" line per goal (with its objective),
// then per plan (with its ordered steps), exactly in the tree's creation order.
func (t *PlanTools) renderTree(ctx context.Context, goals []Goal) string {
	if len(goals) == 0 {
		return "no goals"
	}
	var sb strings.Builder
	for _, g := range goals {
		fmt.Fprintf(&sb, "- %s: %s (%s)\n", g.ID, g.Title, g.Status)
		if g.Objective != "" {
			fmt.Fprintf(&sb, "    objective: %s\n", g.Objective)
		}
		if len(g.Plans) == 0 {
			continue
		}
		for _, pid := range g.Plans {
			p, ok := t.readPlan(ctx, pid)
			if !ok {
				// Plan record unreachable (foreign engine): degrade to the id.
				fmt.Fprintf(&sb, "    - %s\n", pid)
				continue
			}
			fmt.Fprintf(&sb, "    - %s: %s (%s)\n", p.ID, p.Title, p.Status)
			for _, todo := range p.Steps {
				fmt.Fprintf(&sb, "        - %s: %s (%s)\n", todo.ID, todo.Title, todo.Status)
			}
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// validScope reports whether s is one of the three plan-tree levels.
func validScope(s string) bool {
	switch s {
	case string(ScopeGoal), string(ScopePlan), string(ScopeTodo):
		return true
	default:
		return false
	}
}

// PlanGoalTool creates a goal — the root of the goal → plan → todo tree — and
// returns its id.
type PlanGoalTool struct {
	t *PlanTools
}

func (PlanGoalTool) Name() string { return ToolGoalName }

func (PlanGoalTool) Description() string {
	return "create a goal — the root of a goal → plan → todo planning tree — and return its id; build a plan under it with plan_plan"
}

func (PlanGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "one-line goal title",
			},
			"objective": map[string]any{
				"type":        "string",
				"description": "one-paragraph goal description (optional)",
			},
			"max_rounds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "maximum same-session continuation rounds (default 256)",
			},
		},
		"required":             []string{"title"},
		"additionalProperties": false,
	}
}

func (t PlanGoalTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Title     string `json:"title"`
		Objective string `json:"objective"`
		MaxRounds int    `json:"max_rounds"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("plan_goal: %w", err)
	}
	if strings.TrimSpace(a.Title) == "" {
		return "", fmt.Errorf("plan_goal: empty title")
	}
	var g Goal
	var err error
	if creator, ok := t.t.e.(interface {
		CreateGoalWithMaxRounds(context.Context, string, string, int) (Goal, error)
	}); ok {
		g, err = creator.CreateGoalWithMaxRounds(ctx, a.Title, a.Objective, a.MaxRounds)
	} else {
		g, err = t.t.e.CreateGoal(ctx, a.Title, a.Objective)
	}
	if err != nil {
		return "", fmt.Errorf("plan_goal: %w", err)
	}
	// plan/create is a log-only fact (D3); the created goal id/title are logged
	// with the goal scope, and the returned text is what the loop logs as
	// tool/result.
	if err := t.t.emitContext(ctx, session.EventPlanCreate, session.NewPlanCreate(string(ScopeGoal), g.ID, g.Title, nil, map[string]any{
		"objective":     g.Objective,
		"status":        g.Status,
		"revision":      g.Revision,
		"maxRounds":     g.MaxRounds,
		"roundsStarted": g.RoundsStarted,
		"createdAt":     g.CreatedAt,
	})); err != nil {
		return "", fmt.Errorf("plan_goal: persist event: %w", err)
	}
	return fmt.Sprintf("created goal %s: %s", g.ID, g.Title), nil
}

// PlanPlanTool creates a plan under an existing goal, with one pending todo
// per step, and returns its id.
type PlanPlanTool struct {
	t *PlanTools
}

func (PlanPlanTool) Name() string { return ToolPlanName }

func (PlanPlanTool) Description() string {
	return "create a plan under an existing goal, one pending todo per step, and return its id; append more steps with plan_todo"
}

func (PlanPlanTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal_id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the goal id (returned by plan_goal or plan_list) this plan belongs to",
			},
			"title": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "one-line plan title",
			},
			"steps": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "minLength": 1},
				"description": "ordered step titles; each becomes a pending todo (optional)",
			},
		},
		"required":             []string{"goal_id", "title"},
		"additionalProperties": false,
	}
}

func (t PlanPlanTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		GoalID string   `json:"goal_id"`
		Title  string   `json:"title"`
		Steps  []string `json:"steps"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("plan_plan: %w", err)
	}
	if strings.TrimSpace(a.Title) == "" {
		return "", fmt.Errorf("plan_plan: empty title")
	}
	if strings.TrimSpace(a.GoalID) == "" {
		return "", fmt.Errorf("plan_plan: goal_id is required (a plan belongs under a goal)")
	}
	p, err := t.t.e.CreatePlan(ctx, a.GoalID, a.Title, a.Steps)
	if err != nil {
		return "", fmt.Errorf("plan_plan: %w", err)
	}
	if err := t.t.emitContext(ctx, session.EventPlanCreate, session.NewPlanCreate(string(ScopePlan), p.ID, p.Title, nil, map[string]any{
		"goalId":    p.GoalID,
		"status":    p.Status,
		"createdAt": p.CreatedAt,
		"steps":     p.Steps,
	})); err != nil {
		return "", fmt.Errorf("plan_plan: persist event: %w", err)
	}
	return fmt.Sprintf("created plan %s under goal %s: %s (%d steps)", p.ID, p.GoalID, p.Title, len(p.Steps)), nil
}

// PlanTodoTool appends one todo (step) to an existing plan and returns its id.
type PlanTodoTool struct {
	t *PlanTools
}

func (PlanTodoTool) Name() string { return ToolTodoName }

func (PlanTodoTool) Description() string {
	return "append one todo (step) to an existing plan and return its id"
}

func (PlanTodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the plan id (returned by plan_plan or plan_list) this todo belongs to",
			},
			"title": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "one-line step title",
			},
			"acceptance": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "minLength": 1},
				"description": "optional acceptance criteria this todo must satisfy (eval); entries may carry a mode prefix (contains:/not:/llm:/manual:)",
			},
		},
		"required":             []string{"plan_id", "title"},
		"additionalProperties": false,
	}
}

func (t PlanTodoTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		PlanID     string   `json:"plan_id"`
		Title      string   `json:"title"`
		Acceptance []string `json:"acceptance"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("plan_todo: %w", err)
	}
	if strings.TrimSpace(a.Title) == "" {
		return "", fmt.Errorf("plan_todo: empty title")
	}
	todo, err := t.t.e.AddTodo(ctx, a.PlanID, a.Title, a.Acceptance)
	if err != nil {
		return "", fmt.Errorf("plan_todo: %w", err)
	}
	if err := t.t.emitContext(ctx, session.EventPlanCreate, session.NewPlanCreate(string(ScopeTodo), todo.ID, todo.Title, todo.Acceptance, map[string]any{
		"planId":    a.PlanID,
		"status":    todo.Status,
		"createdAt": todo.CreatedAt,
	})); err != nil {
		return "", fmt.Errorf("plan_todo: persist event: %w", err)
	}
	return fmt.Sprintf("added todo %s to plan %s: %s", todo.ID, a.PlanID, todo.Title), nil
}

// PlanStatusTool sets the status of a goal, plan or todo.
type PlanStatusTool struct {
	t *PlanTools
}

func (PlanStatusTool) Name() string { return ToolStatusName }

func (PlanStatusTool) Description() string {
	return "set the status of a goal, plan or todo (pending, in-progress, paused, blocked, done, cancelled); done stamps its completion time"
}

func (PlanStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{
				"type":        "string",
				"enum":        []string{string(ScopeGoal), string(ScopePlan), string(ScopeTodo)},
				"description": "tree level to update: goal, plan or todo",
			},
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the id of the goal/plan/todo to update",
			},
			"revision": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "expected current goal revision (optional CAS fence)",
			},
			"status": map[string]any{
				"type": "string",
				"enum": []string{
					string(StatusPending), string(StatusInProgress), string(StatusPaused), string(StatusBlocked),
					string(StatusDone), string(StatusCancelled),
				},
				"description": "new status: pending, in-progress, paused, blocked, done or cancelled",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "optional human-readable blocker reason when status is blocked",
			},
		},
		"required":             []string{"scope", "id", "status"},
		"additionalProperties": false,
	}
}

func (t PlanStatusTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Scope    string `json:"scope"`
		ID       string `json:"id"`
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Revision int    `json:"revision"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("plan_status: %w", err)
	}
	if !validScope(a.Scope) {
		return "", fmt.Errorf("plan_status: unknown scope %q (expected goal, plan or todo)", a.Scope)
	}
	st := Status(a.Status)
	if !validStatus(st) {
		return "", fmt.Errorf("plan_status: invalid status %q (expected pending, in-progress, paused, blocked, done or cancelled)", a.Status)
	}
	var statusErr error
	if a.Scope == string(ScopeGoal) && a.Revision > 0 {
		if setter, ok := t.t.e.(interface {
			SetGoalStatusIfRevision(context.Context, string, int, Status) error
		}); ok {
			statusErr = setter.SetGoalStatusIfRevision(ctx, a.ID, a.Revision, st)
		} else {
			statusErr = t.t.e.SetStatus(ctx, a.Scope, a.ID, st)
		}
	} else {
		statusErr = t.t.e.SetStatus(ctx, a.Scope, a.ID, st)
	}
	if statusErr != nil {
		return "", fmt.Errorf("plan_status: %w", statusErr)
	}
	if a.Scope == string(ScopeGoal) && st == StatusBlocked && a.Reason != "" {
		if setter, ok := t.t.e.(interface {
			SetGoalBlockedReason(context.Context, string, string) error
		}); ok {
			if err := setter.SetGoalBlockedReason(ctx, a.ID, strings.TrimSpace(a.Reason)); err != nil {
				return "", fmt.Errorf("plan_status: %w", err)
			}
		}
	}
	if err := t.t.emitContext(ctx, session.EventPlanStatus, session.NewPlanStatus(a.Scope, a.ID, string(st), strings.TrimSpace(a.Reason))); err != nil {
		return "", fmt.Errorf("plan_status: persist event: %w", err)
	}
	return fmt.Sprintf("set %s %s status to %s", a.Scope, a.ID, st), nil
}

// PlanListTool returns the full goal → plans → todos aggregation tree.
type PlanListTool struct {
	t *PlanTools
}

func (PlanListTool) Name() string { return ToolListName }

func (PlanListTool) Description() string {
	return "list the full goal → plans → todos planning tree (id, title, status)"
}

func (PlanListTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

func (t PlanListTool) Execute(ctx context.Context, args any) (string, error) {
	goals, err := t.t.e.List(ctx)
	if err != nil {
		return "", fmt.Errorf("plan_list: %w", err)
	}
	// plan/list is a log-only fact (D3) carrying the number of goals in the
	// returned tree.
	if err := t.t.emitContext(ctx, session.EventPlanList, session.NewPlanList(len(goals))); err != nil {
		return "", fmt.Errorf("plan_list: persist event: %w", err)
	}
	return t.t.renderTree(ctx, goals), nil
}

// PlanRemoveTool removes a goal (cascading to its plans), a plan (detaching it
// from its goal) or a todo, by id.
type PlanRemoveTool struct {
	t *PlanTools
}

func (PlanRemoveTool) Name() string { return ToolRemoveName }

func (PlanRemoveTool) Description() string {
	return "remove a goal (cascading to its plans and todos), a plan or a todo by id"
}

func (PlanRemoveTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{
				"type":        "string",
				"enum":        []string{string(ScopeGoal), string(ScopePlan), string(ScopeTodo)},
				"description": "tree level to remove: goal, plan or todo",
			},
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the id of the goal/plan/todo to remove",
			},
		},
		"required":             []string{"scope", "id"},
		"additionalProperties": false,
	}
}

func (t PlanRemoveTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("plan_remove: %w", err)
	}
	if !validScope(a.Scope) {
		return "", fmt.Errorf("plan_remove: unknown scope %q (expected goal, plan or todo)", a.Scope)
	}
	if err := t.t.e.Remove(ctx, a.Scope, a.ID); err != nil {
		return "", fmt.Errorf("plan_remove: %w", err)
	}
	if err := t.t.emitContext(ctx, session.EventPlanDelete, session.NewPlanDelete(a.Scope, a.ID)); err != nil {
		return "", fmt.Errorf("plan_remove: persist event: %w", err)
	}
	return fmt.Sprintf("removed %s %s", a.Scope, a.ID), nil
}
