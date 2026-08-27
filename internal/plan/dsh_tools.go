package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

// DSHTools is the model-facing goal/todo surface. It deliberately reuses the
// existing plan engine as a storage projection while exposing only DSH's
// canonical tool names.
type DSHTools struct {
	e                 Engine
	onEvent           func(string, any)
	owner             func() string
	allowParallelTodo bool
}

func NewDSHTools(e Engine, onEvent func(string, any)) *DSHTools {
	return NewDSHToolsWithOwner(e, onEvent, nil, true)
}

// NewDSHToolsWithOwner binds the model-facing todo list to the addressed
// session. DSH rejects todo writes without an owning agent; the owner callback
// is intentionally supplied by the composition root rather than inferred
// from a process-global plan engine.
func NewDSHToolsWithOwner(e Engine, onEvent func(string, any), owner func() string, allowParallel bool) *DSHTools {
	return &DSHTools{e: e, onEvent: onEvent, owner: owner, allowParallelTodo: allowParallel}
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

type GetGoalTool struct{ t *DSHTools }

func (GetGoalTool) Name() string { return "get_goal" }
func (GetGoalTool) Description() string {
	return "read the current goal for this session"
}
func (GetGoalTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (GetGoalTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t GetGoalTool) Execute(ctx context.Context, args any) (string, error) {
	if _, err := decodeEmpty(args); err != nil {
		return "", fmt.Errorf("get_goal: %w", err)
	}
	g, ok, err := t.t.currentGoal(ctx)
	if err != nil {
		return "", fmt.Errorf("get_goal: %w", err)
	}
	if !ok {
		return "no active goal", nil
	}
	return encodeGoal(g), nil
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
			"max_goal_rounds": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"objective"}, "additionalProperties": false,
	}
}
func (CreateGoalTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t CreateGoalTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		Objective     string `json:"objective"`
		MaxGoalRounds int    `json:"max_goal_rounds"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("create_goal: %w", err)
	}
	in.Objective = strings.TrimSpace(in.Objective)
	if in.Objective == "" {
		return "", errors.New("create_goal: objective is required")
	}
	if _, ok, err := t.t.currentGoal(ctx); err != nil {
		return "", fmt.Errorf("create_goal: %w", err)
	} else if ok {
		return "", errors.New("create_goal: an active goal already exists")
	}
	var g Goal
	var err error
	if creator, ok := t.t.e.(interface {
		CreateGoalWithMaxRounds(context.Context, string, string, int) (Goal, error)
	}); ok {
		g, err = creator.CreateGoalWithMaxRounds(ctx, firstGoalTitle(in.Objective), in.Objective, in.MaxGoalRounds)
	} else {
		g, err = t.t.e.CreateGoal(ctx, firstGoalTitle(in.Objective), in.Objective)
	}
	if err != nil {
		return "", fmt.Errorf("create_goal: %w", err)
	}
	t.t.emit(session.EventPlanCreate, session.NewPlanCreate(string(ScopeGoal), g.ID, g.Title, nil, map[string]any{
		"objective": g.Objective, "status": g.Status, "revision": g.Revision,
		"maxRounds": g.MaxRounds, "roundsStarted": g.RoundsStarted, "createdAt": g.CreatedAt,
	}))
	return encodeGoal(g), nil
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
func (UpdateGoalTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t UpdateGoalTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		GoalID        string  `json:"goal_id"`
		Revision      int     `json:"revision"`
		Action        string  `json:"action"`
		Objective     *string `json:"objective"`
		MaxGoalRounds *int    `json:"max_goal_rounds"`
		BlockedReason string  `json:"blocked_reason"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("update_goal: %w", err)
	}
	g, err := t.t.goalByID(ctx, in.GoalID, in.Revision)
	if err != nil {
		return "", fmt.Errorf("update_goal: %w", err)
	}
	switch in.Action {
	case "edit":
		if in.Objective == nil && in.MaxGoalRounds == nil {
			return "", errors.New("update_goal: edit requires objective or max_goal_rounds")
		}
		updater, ok := t.t.e.(interface {
			UpdateGoalIfRevision(context.Context, string, int, *string, *int) (Goal, error)
		})
		if !ok {
			return "", errors.New("update_goal: revisioned edit is unavailable")
		}
		g, err = updater.UpdateGoalIfRevision(ctx, g.ID, in.Revision, in.Objective, in.MaxGoalRounds)
		if err == nil {
			t.t.emit(session.EventPlanUpdate, session.NewPlanUpdate(string(ScopeGoal), g.ID, map[string]any{"title": g.Title, "objective": g.Objective, "maxRounds": g.MaxRounds}))
		}
	case "pause", "resume", "complete", "blocked":
		if in.Action == "blocked" && strings.TrimSpace(in.BlockedReason) == "" {
			return "", errors.New("update_goal: blocked_reason is required for blocked")
		}
		status := map[string]Status{"pause": StatusPaused, "resume": StatusInProgress, "complete": StatusDone, "blocked": StatusBlocked}[in.Action]
		setter, ok := t.t.e.(interface {
			SetGoalStatusIfRevision(context.Context, string, int, Status) error
		})
		if !ok {
			return "", errors.New("update_goal: revisioned status update is unavailable")
		}
		err = setter.SetGoalStatusIfRevision(ctx, g.ID, in.Revision, status)
		if err == nil && in.Action == "blocked" {
			if reasoner, ok := t.t.e.(interface {
				SetGoalBlockedReason(context.Context, string, string) error
			}); ok {
				err = reasoner.SetGoalBlockedReason(ctx, g.ID, strings.TrimSpace(in.BlockedReason))
			}
		}
		if err == nil {
			reason := ""
			if in.Action == "blocked" {
				reason = strings.TrimSpace(in.BlockedReason)
			}
			t.t.emit(session.EventPlanStatus, session.NewPlanStatus(string(ScopeGoal), g.ID, string(status), reason))
		}
	default:
		return "", fmt.Errorf("update_goal: unknown action %q", in.Action)
	}
	if err != nil {
		return "", fmt.Errorf("update_goal: %w", err)
	}
	g, err = t.t.goalByID(ctx, g.ID, 0)
	if err != nil {
		return "", fmt.Errorf("update_goal: %w", err)
	}
	return encodeGoal(g), nil
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
	if t.t.owner != nil && strings.TrimSpace(t.t.owner()) == "" {
		return nil, errors.New("todo_write requires an owning agent session")
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
	t.t.emit("todo/write", payload)
	return map[string]any{"todos": items, "counts": counts}, nil
}

func (t *DSHTools) currentGoal(ctx context.Context) (Goal, bool, error) {
	goals, err := t.e.List(ctx)
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
	goals, err := t.e.List(ctx)
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

func encodeGoal(g Goal) string {
	b, _ := json.Marshal(map[string]any{"id": g.ID, "objective": g.Objective, "status": string(g.Status), "revision": g.Revision, "max_goal_rounds": g.MaxRounds, "rounds_started": g.RoundsStarted, "blocked_reason": g.BlockedReason})
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
