package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
)

// DSHTools is the model-facing goal/todo surface. It deliberately reuses the
// existing plan engine as a storage projection while exposing only DSH's
// canonical tool names.
type DSHTools struct {
	e                  Engine
	resolve            func(context.Context) (Engine, error)
	onEvent            func(string, any)
	owner              func() string
	ownerContext       func(context.Context) string
	activation         func() bool
	setActivation      func(bool)
	activationContext  func(context.Context) bool
	setActivationCtx   func(context.Context, bool)
	allowParallelTodo  bool
	blockedAfterRounds int
}

func NewDSHTools(e Engine, onEvent func(string, any)) *DSHTools {
	return NewDSHToolsWithOwnerAndActivation(e, onEvent, nil, nil, nil, true, 3)
}

// NewDSHToolsWithOwner binds the model-facing todo list to the addressed
// session. DSH rejects todo writes without an owning agent; the owner callback
// is intentionally supplied by the composition root rather than inferred
// from a process-global plan engine.
func NewDSHToolsWithOwner(e Engine, onEvent func(string, any), owner func() string, allowParallel bool) *DSHTools {
	return NewDSHToolsWithOwnerAndActivation(e, onEvent, owner, nil, nil, allowParallel, 3)
}

// NewDSHToolsWithOwnerAndActivation binds goal activation to the addressed
// session and applies DSH's configurable minimum blocked-round threshold.
func NewDSHToolsWithOwnerAndActivation(e Engine, onEvent func(string, any), owner func() string, activation func() bool, setActivation func(bool), allowParallel bool, blockedAfterRounds int) *DSHTools {
	if blockedAfterRounds < 1 {
		blockedAfterRounds = 3
	}
	return &DSHTools{e: e, onEvent: onEvent, owner: owner, activation: activation, setActivation: setActivation, allowParallelTodo: allowParallel, blockedAfterRounds: blockedAfterRounds}
}

// NewDSHToolsWithResolver binds the model-facing tools to a session-aware
// engine resolver. The resolver is evaluated for every Execute, which keeps
// concurrent Agent sessions from sharing the disposable plan projection.
func NewDSHToolsWithResolver(resolve func(context.Context) (Engine, error), onEvent func(string, any), owner func() string, activation func() bool, setActivation func(bool), allowParallel bool, blockedAfterRounds int) *DSHTools {
	if blockedAfterRounds < 1 {
		blockedAfterRounds = 3
	}
	return &DSHTools{resolve: resolve, onEvent: onEvent, owner: owner, activation: activation, setActivation: setActivation, allowParallelTodo: allowParallel, blockedAfterRounds: blockedAfterRounds}
}

// NewDSHToolsWithResolverContext is the Agent-owned constructor. The owner
// resolver is evaluated with the addressed runtime context for each execute;
// the legacy resolver remains available to direct embedders.
func NewDSHToolsWithResolverContext(resolve func(context.Context) (Engine, error), onEvent func(string, any), owner func(context.Context) string, activation func() bool, setActivation func(bool), allowParallel bool, blockedAfterRounds int) *DSHTools {
	if blockedAfterRounds < 1 {
		blockedAfterRounds = 3
	}
	return &DSHTools{resolve: resolve, onEvent: onEvent, ownerContext: owner, activation: activation, setActivation: setActivation, allowParallelTodo: allowParallel, blockedAfterRounds: blockedAfterRounds}
}

// SetContextActivation supplies the session-aware activation callbacks used
// by Agent runtimes. The legacy callbacks remain the fallback for direct CLI
// callers that have no runtime context.
func (t *DSHTools) SetContextActivation(activation func(context.Context) bool, setActivation func(context.Context, bool)) {
	t.activationContext = activation
	t.setActivationCtx = setActivation
}

func (t *DSHTools) engine(ctx context.Context) (Engine, error) {
	if t.resolve != nil {
		return t.resolve(ctx)
	}
	if t.e == nil {
		return nil, errors.New("plan engine is unavailable")
	}
	return t.e, nil
}

func (t *DSHTools) requireOwner(tool string, ctx ...context.Context) error {
	if len(ctx) > 0 {
		if sessionID := runtimectx.SessionID(ctx[0]); sessionID != "" {
			return nil
		}
		if t.ownerContext != nil {
			if strings.TrimSpace(t.ownerContext(ctx[0])) == "" {
				return fmt.Errorf("%s requires an owning agent session", tool)
			}
			return nil
		}
	}
	if t.owner != nil && strings.TrimSpace(t.owner()) == "" {
		return fmt.Errorf("%s requires an owning agent session", tool)
	}
	return nil
}

func (t *DSHTools) setArmed(armed bool) {
	if t.setActivation != nil {
		t.setActivation(armed)
	}
}

func (t *DSHTools) setArmedContext(ctx context.Context, armed bool) {
	if t.setActivationCtx != nil {
		t.setActivationCtx(ctx, armed)
		return
	}
	t.setArmed(armed)
}

func (t *DSHTools) GetGoal() GetGoalTool       { return GetGoalTool{t: t} }
func (t *DSHTools) CreateGoal() CreateGoalTool { return CreateGoalTool{t: t} }
func (t *DSHTools) UpdateGoal() UpdateGoalTool { return UpdateGoalTool{t: t} }
func (t *DSHTools) TodoWrite() TodoWriteTool   { return TodoWriteTool{t: t} }

func (t *DSHTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *DSHTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

type GetGoalTool struct{ t *DSHTools }

func (GetGoalTool) Name() string { return "get_goal" }
func (GetGoalTool) Description() string {
	return "read the current goal for this session"
}
func (GetGoalTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (GetGoalTool) OutputSchema() map[string]any { return goalOutputSchema() }
func (t GetGoalTool) Execute(ctx context.Context, args any) (string, error) {
	if _, err := decodeEmpty(args); err != nil {
		return "", fmt.Errorf("get_goal: %w", err)
	}
	if err := t.t.requireOwner("get_goal", ctx); err != nil {
		return "", err
	}
	g, ok, err := t.t.currentGoal(ctx)
	if err != nil {
		return "", fmt.Errorf("get_goal: %w", err)
	}
	if !ok {
		return marshalGoalValue(map[string]any{"goal": nil}), nil
	}
	return marshalGoalValue(t.t.goalValue(ctx, g)), nil
}

func (t GetGoalTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	value, err := t.value(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	return agenttools.ToolResult{Value: value, Output: marshalGoalValue(value)}, nil
}

func (t GetGoalTool) value(ctx context.Context, args any) (map[string]any, error) {
	if _, err := decodeEmpty(args); err != nil {
		return nil, fmt.Errorf("get_goal: %w", err)
	}
	if err := t.t.requireOwner("get_goal", ctx); err != nil {
		return nil, err
	}
	g, ok, err := t.t.currentGoal(ctx)
	if err != nil {
		return nil, fmt.Errorf("get_goal: %w", err)
	}
	if !ok {
		return map[string]any{"goal": nil}, nil
	}
	return t.t.goalValue(ctx, g), nil
}

type CreateGoalTool struct{ t *DSHTools }

func (CreateGoalTool) Name() string { return "create_goal" }
func (CreateGoalTool) Description() string {
	return "create the concrete completion goal inferred from the user's request"
}
func (CreateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective":       map[string]any{"type": "string", "minLength": 1},
			"max_goal_rounds": map[string]any{"type": "integer"},
		},
		"required": []string{"objective"}, "additionalProperties": false,
	}
}
func (CreateGoalTool) OutputSchema() map[string]any { return goalOutputSchema() }
func (t CreateGoalTool) Execute(ctx context.Context, args any) (string, error) {
	value, err := t.value(ctx, args)
	if err != nil {
		return "", err
	}
	return marshalGoalValue(value), nil
}

func (t CreateGoalTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	value, err := t.value(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	return agenttools.ToolResult{Value: value, Output: marshalGoalValue(value)}, nil
}

func (t CreateGoalTool) value(ctx context.Context, args any) (map[string]any, error) {
	if err := t.t.requireOwner("create_goal", ctx); err != nil {
		return nil, err
	}
	var in struct {
		Objective     string `json:"objective"`
		MaxGoalRounds int    `json:"max_goal_rounds"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("create_goal: %w", err)
	}
	in.Objective = strings.TrimSpace(in.Objective)
	if in.Objective == "" {
		return nil, errors.New("create_goal: objective is required")
	}
	if _, ok, err := t.t.currentGoal(ctx); err != nil {
		return nil, fmt.Errorf("create_goal: %w", err)
	} else if ok {
		return nil, errors.New("create_goal: an active goal already exists")
	}
	e, err := t.t.engine(ctx)
	if err != nil {
		return nil, fmt.Errorf("create_goal: %w", err)
	}
	var g Goal
	if creator, ok := e.(interface {
		CreateGoalWithMaxRounds(context.Context, string, string, int) (Goal, error)
	}); ok {
		g, err = creator.CreateGoalWithMaxRounds(ctx, firstGoalTitle(in.Objective), in.Objective, in.MaxGoalRounds)
	} else {
		g, err = e.CreateGoal(ctx, firstGoalTitle(in.Objective), in.Objective)
	}
	if err != nil {
		return nil, fmt.Errorf("create_goal: %w", err)
	}
	if err := t.t.emitContext(ctx, session.EventPlanCreate, session.NewPlanCreate(string(ScopeGoal), g.ID, g.Title, nil, map[string]any{
		"objective": g.Objective, "status": g.Status, "revision": g.Revision,
		"maxRounds": g.MaxRounds, "roundsStarted": g.RoundsStarted, "createdAt": g.CreatedAt,
	})); err != nil {
		return nil, fmt.Errorf("create_goal: persist event: %w", err)
	}
	return t.t.goalValue(ctx, g), nil
}

type UpdateGoalTool struct{ t *DSHTools }

func (UpdateGoalTool) Name() string { return "update_goal" }
func (UpdateGoalTool) Description() string {
	return "update the exact current goal revision: edit, pause, resume, complete, or blocked"
}
func (UpdateGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal_id":         map[string]any{"type": "string", "minLength": 1},
			"revision":        map[string]any{"type": "integer", "minimum": 1},
			"action":          map[string]any{"type": "string", "enum": []string{"edit", "pause", "resume", "complete", "blocked"}},
			"objective":       map[string]any{"type": "string"},
			"max_goal_rounds": map[string]any{"type": "integer", "minimum": 1},
			"blocked_reason":  map[string]any{"type": "string"},
		},
		"required": []string{"goal_id", "revision", "action"}, "additionalProperties": false,
	}
}
func (UpdateGoalTool) OutputSchema() map[string]any { return goalOutputSchema() }
func (t UpdateGoalTool) Execute(ctx context.Context, args any) (string, error) {
	value, err := t.value(ctx, args)
	if err != nil {
		return "", err
	}
	return marshalGoalValue(value), nil
}

func (t UpdateGoalTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	value, err := t.value(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	return agenttools.ToolResult{Value: value, Output: marshalGoalValue(value)}, nil
}

func (t UpdateGoalTool) value(ctx context.Context, args any) (map[string]any, error) {
	if err := t.t.requireOwner("update_goal", ctx); err != nil {
		return nil, err
	}
	var in struct {
		GoalID        string  `json:"goal_id"`
		Revision      int     `json:"revision"`
		Action        string  `json:"action"`
		Objective     *string `json:"objective"`
		MaxGoalRounds *int    `json:"max_goal_rounds"`
		BlockedReason *string `json:"blocked_reason"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("update_goal: %w", err)
	}
	if in.GoalID == "" || in.GoalID != strings.TrimSpace(in.GoalID) || in.Revision < 1 {
		return nil, errors.New("update_goal: goal_id must be non-empty and revision must be a positive integer")
	}
	e, err := t.t.engine(ctx)
	if err != nil {
		return nil, fmt.Errorf("update_goal: %w", err)
	}
	g, err := t.t.goalByIDWithEngine(ctx, e, in.GoalID, in.Revision)
	if err != nil {
		return nil, fmt.Errorf("update_goal: %w", err)
	}
	hasObjective := in.Objective != nil && *in.Objective != ""
	hasRounds := in.MaxGoalRounds != nil && *in.MaxGoalRounds != 0
	hasBlockedReason := in.BlockedReason != nil && *in.BlockedReason != ""
	switch in.Action {
	case "edit":
		if !hasObjective && !hasRounds {
			return nil, errors.New("update_goal: edit requires objective or max_goal_rounds")
		}
		if hasBlockedReason {
			return nil, errors.New("update_goal: blocked_reason is valid only with action blocked")
		}
		updater, ok := e.(interface {
			UpdateGoalIfRevision(context.Context, string, int, *string, *int) (Goal, error)
		})
		if !ok {
			return nil, errors.New("update_goal: revisioned edit is unavailable")
		}
		var objective *string
		if hasObjective {
			objective = in.Objective
		}
		var maxRounds *int
		if hasRounds {
			maxRounds = in.MaxGoalRounds
		}
		g, err = updater.UpdateGoalIfRevision(ctx, g.ID, in.Revision, objective, maxRounds)
		if err == nil {
			if emitErr := t.t.emitContext(ctx, session.EventPlanUpdate, session.NewPlanUpdate(string(ScopeGoal), g.ID, map[string]any{"title": g.Title, "objective": g.Objective, "maxRounds": g.MaxRounds})); emitErr != nil {
				return nil, fmt.Errorf("update_goal: persist event: %w", emitErr)
			}
		}
	case "pause", "resume", "complete", "blocked":
		if (in.Action == "pause" || in.Action == "resume") && (hasObjective || hasRounds || hasBlockedReason) {
			return nil, errors.New("update_goal: objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
		}
		if (in.Action == "complete" || in.Action == "blocked") && (hasObjective || hasRounds) {
			return nil, errors.New("update_goal: objective and max_goal_rounds are valid only with action edit")
		}
		if in.Action == "complete" && hasBlockedReason {
			return nil, errors.New("update_goal: blocked_reason is valid only with action blocked")
		}
		if in.Action == "blocked" && !hasBlockedReason {
			return nil, errors.New("update_goal: blocked_reason is required with action blocked")
		}
		if in.Action == "blocked" && IsGoalRound(ctx) && g.RoundsStarted < t.t.blockedAfterRounds {
			return nil, fmt.Errorf("blocked requires at least %d consecutive goal rounds; current round is %d", t.t.blockedAfterRounds, g.RoundsStarted)
		}
		status := map[string]Status{"pause": StatusPaused, "resume": StatusInProgress, "complete": StatusDone, "blocked": StatusBlocked}[in.Action]
		setter, ok := e.(interface {
			SetGoalStatusIfRevision(context.Context, string, int, Status) error
		})
		if !ok {
			return nil, errors.New("update_goal: revisioned status update is unavailable")
		}
		err = setter.SetGoalStatusIfRevision(ctx, g.ID, in.Revision, status)
		if err == nil && in.Action == "blocked" {
			if reasoner, ok := e.(interface {
				SetGoalBlockedReason(context.Context, string, string) error
			}); ok {
				err = reasoner.SetGoalBlockedReason(ctx, g.ID, strings.TrimSpace(*in.BlockedReason))
			}
		}
		if err == nil {
			reason := ""
			if in.Action == "blocked" {
				reason = strings.TrimSpace(*in.BlockedReason)
			}
			if emitErr := t.t.emitContext(ctx, session.EventPlanStatus, session.NewPlanStatus(string(ScopeGoal), g.ID, string(status), reason)); emitErr != nil {
				return nil, fmt.Errorf("update_goal: persist event: %w", emitErr)
			}
		}
	default:
		return nil, fmt.Errorf("update_goal: unknown action %q", in.Action)
	}
	if err != nil {
		return nil, fmt.Errorf("update_goal: %w", err)
	}
	g, err = t.t.goalByIDWithEngine(ctx, e, g.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("update_goal: %w", err)
	}
	if in.Action == "resume" {
		t.t.setArmedContext(ctx, true)
	}
	if in.Action == "complete" || in.Action == "blocked" {
		t.t.setArmedContext(ctx, false)
	}
	return t.t.goalValue(ctx, g), nil
}

type TodoWriteTool struct{ t *DSHTools }

func (TodoWriteTool) Name() string { return "todo_write" }
func (TodoWriteTool) Description() string {
	return "replace the complete task list; each task has content and status pending, in_progress, or completed"
}
func (TodoWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "properties": map[string]any{
			"todos": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{
					"content": map[string]any{"type": "string", "minLength": 1},
					"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
				}, "required": []string{"content", "status"}, "additionalProperties": false,
			}},
		}, "required": []string{"todos"}, "additionalProperties": false,
	}
}
func (TodoWriteTool) OutputSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"todos", "counts"},
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"content", "status"},
					"properties": map[string]any{
						"content": map[string]any{"type": "string"},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
					},
				},
			},
			"counts": map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"pending", "inProgress", "completed"},
				"properties": map[string]any{
					"pending":    map[string]any{"type": "integer"},
					"inProgress": map[string]any{"type": "integer"},
					"completed":  map[string]any{"type": "integer"},
				},
			},
		},
	}
}

func (t TodoWriteTool) Execute(ctx context.Context, args any) (string, error) {
	value, err := t.write(ctx, args)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(value)
	return string(b), err
}

// ExecuteResult exposes DSH's structured todo result to the registry. Output
// is the compact native render while Value remains the complete canonical
// snapshot for replay, validation and UI consumers.
func (t TodoWriteTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	value, err := t.write(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	counts := value["counts"].(map[string]any)
	return agenttools.ToolResult{
		Value:  value,
		Output: fmt.Sprintf("Updated todo list: %d pending, %d in progress, %d completed.", counts["pending"], counts["inProgress"], counts["completed"]),
	}, nil
}

func (t TodoWriteTool) write(ctx context.Context, args any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("todo_write: %w", err)
	}
	if err := t.t.requireOwner("todo_write", ctx); err != nil {
		return nil, err
	}
	items := make([]any, len(in.Todos))
	seen := make(map[string]struct{}, len(in.Todos))
	counts := map[string]any{"pending": 0, "inProgress": 0, "completed": 0}
	for i, todo := range in.Todos {
		content := strings.TrimSpace(todo.Content)
		if content == "" {
			return nil, fmt.Errorf("invalid todo: `content` must be a non-empty string")
		}
		if _, ok := seen[content]; ok {
			return nil, fmt.Errorf("invalid todos: duplicate content %q", content)
		}
		seen[content] = struct{}{}
		if todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed" {
			return nil, fmt.Errorf("todo_write: invalid status %q", todo.Status)
		}
		items[i] = map[string]any{"content": content, "status": todo.Status}
		switch todo.Status {
		case "pending":
			counts["pending"] = counts["pending"].(int) + 1
		case "in_progress":
			counts["inProgress"] = counts["inProgress"].(int) + 1
		case "completed":
			counts["completed"] = counts["completed"].(int) + 1
		}
	}
	if !t.t.allowParallelTodo && counts["inProgress"].(int) > 1 {
		return nil, fmt.Errorf("invalid todos: at most one task may be in_progress (got %d)", counts["inProgress"])
	}
	payload := map[string]any{"todos": items}
	if err := t.t.emitContext(ctx, session.EventTodoWrite, session.NewTodoWrite(payload["todos"])); err != nil {
		return nil, fmt.Errorf("todo_write: persist event: %w", err)
	}
	return map[string]any{"todos": items, "counts": counts}, nil
}

func (t *DSHTools) currentGoal(ctx context.Context) (Goal, bool, error) {
	e, err := t.engine(ctx)
	if err != nil {
		return Goal{}, false, err
	}
	goals, err := e.List(ctx)
	if err != nil {
		return Goal{}, false, err
	}
	for i := len(goals) - 1; i >= 0; i-- {
		if goals[i].Status != StatusDone && goals[i].Status != StatusCancelled {
			return goals[i], true, nil
		}
	}
	return Goal{}, false, nil
}

func (t *DSHTools) goalByID(ctx context.Context, id string, revision int) (Goal, error) {
	e, err := t.engine(ctx)
	if err != nil {
		return Goal{}, err
	}
	return t.goalByIDWithEngine(ctx, e, id, revision)
}

func (t *DSHTools) goalByIDWithEngine(ctx context.Context, e Engine, id string, revision int) (Goal, error) {
	if id == "" || id != strings.TrimSpace(id) {
		return Goal{}, errors.New("goal_id must be non-empty and trimmed")
	}
	goals, err := e.List(ctx)
	if err != nil {
		return Goal{}, err
	}
	for _, g := range goals {
		if g.ID == id {
			if revision > 0 && g.Revision != revision {
				return Goal{}, fmt.Errorf("stale goal revision %d (current %d)", revision, g.Revision)
			}
			return g, nil
		}
	}
	return Goal{}, fmt.Errorf("goal %q was not found", id)
}

func (t *DSHTools) goalValue(ctx context.Context, g Goal) map[string]any {
	phase := "active"
	switch g.Status {
	case StatusPaused:
		phase = "paused"
	case StatusBlocked:
		phase = "blocked"
	case StatusDone, StatusCancelled:
		phase = "complete"
	}
	goal := map[string]any{
		"id": g.ID, "revision": g.Revision, "objective": g.Objective,
		"phase": phase, "roundsStarted": g.RoundsStarted,
		"maxGoalRounds": g.MaxRounds,
	}
	if g.BlockedReason != "" {
		goal["blockedReason"] = map[string]any{"code": "model-reported", "message": g.BlockedReason}
	}
	activation := "armed"
	armed := t.activation != nil && t.activation()
	if t.activationContext != nil {
		armed = t.activationContext(ctx)
	}
	if (t.activation != nil || t.activationContext != nil) && !armed {
		activation = "disarmed"
	}
	return map[string]any{"goal": goal, "activation": activation}
}

func goalOutputSchema() map[string]any {
	return map[string]any{
		"oneOf": []any{
			map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"goal"}, "properties": map[string]any{"goal": map[string]any{"type": "null"}},
			},
			map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"goal", "activation"}, "properties": map[string]any{
					"goal": map[string]any{
						"type": "object", "additionalProperties": false,
						"required": []string{"id", "revision", "objective", "phase", "roundsStarted", "maxGoalRounds"},
						"properties": map[string]any{
							"id": map[string]any{"type": "string"}, "revision": map[string]any{"type": "integer"},
							"objective": map[string]any{"type": "string"}, "phase": map[string]any{"type": "string", "enum": []string{"active", "paused", "blocked", "complete"}},
							"roundsStarted": map[string]any{"type": "integer"}, "maxGoalRounds": map[string]any{"type": "integer"},
							"blockedReason": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}},
						},
					},
					"activation": map[string]any{"type": "string", "enum": []string{"armed", "disarmed"}},
				},
			},
		},
	}
}

func marshalGoalValue(value map[string]any) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func decodeEmpty(args any) (map[string]any, error) {
	var in map[string]any
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return nil, err
	}
	if len(in) != 0 {
		return nil, errors.New("arguments are not supported")
	}
	return in, nil
}
