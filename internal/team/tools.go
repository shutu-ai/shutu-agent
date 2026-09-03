package team

// This file contains the model-facing task/mailbox adapter. The Board remains
// a storage and concurrency primitive; this layer supplies the missing
// session/agent boundary so a caller cannot address another session's team by
// guessing a task or message id.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
)

const (
	ToolTaskCreate = "task_create"
	ToolTaskUpdate = "task_update"
	ToolTaskList   = "task_list"
	ToolTaskGet    = "task_get"
	ToolMessage    = "team_message"
)

var (
	ErrUnauthorized = errors.New("team: unauthorized member")
	ErrSelfMessage  = errors.New("team: a Team member cannot message itself")
)

// BoardResolver returns the board belonging to one session. Implementations
// should create it lazily and retain it for the life of that session.
type BoardResolver func(sessionID string) (*Board, error)
type BoardResolverContext func(context.Context, string) (*Board, error)

// ActorResolver returns the stable agent/member id for the current caller.
type ActorResolver func() (sessionID, memberID string)
type ActorResolverContext func(context.Context) (sessionID, memberID string)

type Tools struct {
	resolve        BoardResolver
	actor          ActorResolver
	resolveContext BoardResolverContext
	actorContext   ActorResolverContext
	snapshot       func(context.Context, string, Snapshot) error
	event          func(context.Context, string, string, any) error
	// mutationMu serializes the in-memory mutation and its durable commit. A
	// failed event/snapshot append rolls back the pre-mutation snapshot; without
	// this lock such a rollback could erase a concurrent successful mutation.
	mutationMu sync.Mutex
}

func NewTools(resolve BoardResolver, actor ActorResolver) *Tools {
	return &Tools{resolve: resolve, actor: actor}
}

func NewToolsWithContext(resolve BoardResolverContext, actor ActorResolverContext) *Tools {
	return &Tools{resolveContext: resolve, actorContext: actor}
}

// SetSnapshotSink binds durable board snapshots to the owning session. The
// sink is called after each mutation and receives a detached snapshot; a sink
// failure is returned to the model-facing tool so durable state never silently
// diverges from the live board.
func (t *Tools) SetSnapshotSink(sink func(context.Context, string, Snapshot) error) {
	if t != nil {
		t.snapshot = sink
	}
}

// SetEventSink binds the append-only Team journal. When configured, mutation
// tools publish a typed team event before returning; snapshots remain a legacy
// migration/recovery checkpoint but are not written for those mutations.
func (t *Tools) SetEventSink(sink func(context.Context, string, string, any) error) {
	if t != nil {
		t.event = sink
	}
}

func (t *Tools) Tools() []any {
	return []any{t.TaskCreate(), t.TaskUpdate(), t.TaskList(), t.TaskGet(), t.Message()}
}
func (t *Tools) TaskCreate() TaskCreateTool { return TaskCreateTool{t: t} }
func (t *Tools) TaskUpdate() TaskUpdateTool { return TaskUpdateTool{t: t} }
func (t *Tools) TaskList() TaskListTool     { return TaskListTool{t: t} }
func (t *Tools) TaskGet() TaskGetTool       { return TaskGetTool{t: t} }
func (t *Tools) Message() MessageTool       { return MessageTool{t: t} }

func (t *Tools) board(ctx context.Context) (*Board, string, string, error) {
	if t == nil || (t.resolve == nil && t.resolveContext == nil) || (t.actor == nil && t.actorContext == nil) {
		return nil, "", "", errors.New("team: tool runtime is not configured")
	}
	var sessionID, memberID string
	if t.actorContext != nil {
		sessionID, memberID = t.actorContext(ctx)
	} else {
		sessionID, memberID = t.actor()
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(memberID) == "" {
		return nil, "", "", fmt.Errorf("%w: session and member are required", ErrUnauthorized)
	}
	var board *Board
	var err error
	if t.resolveContext != nil {
		board, err = t.resolveContext(ctx, sessionID)
	} else {
		board, err = t.resolve(sessionID)
	}
	if err != nil {
		return nil, "", "", err
	}
	if board == nil {
		return nil, "", "", errors.New("team: board resolver returned nil")
	}
	return board, sessionID, memberID, nil
}

func (t *Tools) saveSnapshot(ctx context.Context, sessionID string, board *Board) error {
	if t == nil || t.snapshot == nil || t.event != nil || board == nil {
		return nil
	}
	return t.snapshot(ctx, sessionID, board.Snapshot())
}

func (t *Tools) saveEvent(ctx context.Context, sessionID, typ string, value any) error {
	if t == nil || t.event == nil {
		return nil
	}
	return t.event(ctx, sessionID, typ, value)
}

func authorize(board *Board, actor, target string) error {
	if roster := board.Roster(); roster != nil {
		return roster.Authorize(agent.ID(actor), agent.ID(target))
	}
	return nil
}

func textResult(value any, output string) agenttools.ToolResult {
	encoded, err := json.Marshal(value)
	if err == nil {
		var detached any
		if json.Unmarshal(encoded, &detached) == nil {
			value = detached
		}
	}
	return agenttools.ToolResult{Value: value, Output: output}
}

func decode(args any, dst any) error { return agenttools.DecodeArgs(args, dst) }

type TaskCreateTool struct{ t *Tools }

func (TaskCreateTool) Name() string                 { return ToolTaskCreate }
func (TaskCreateTool) Description() string          { return "create a session-scoped team task" }
func (TaskCreateTool) OutputSchema() map[string]any { return map[string]any{"type": "object"} }
func (TaskCreateTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"subject":      map[string]any{"type": "string", "minLength": 1},
		"description":  map[string]any{"type": "string"},
		"blocked_by":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"write_scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}, "required": []string{"subject"}, "additionalProperties": false}
}
func (t TaskCreateTool) Execute(ctx context.Context, args any) (string, error) {
	r, err := t.ExecuteResult(ctx, args)
	return r.Output, err
}
func (t TaskCreateTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	t.t.mutationMu.Lock()
	defer t.t.mutationMu.Unlock()
	var in struct {
		Subject     string   `json:"subject"`
		Description string   `json:"description"`
		BlockedBy   []string `json:"blocked_by"`
		WriteScopes []string `json:"write_scopes"`
	}
	if err := decode(args, &in); err != nil {
		return agenttools.ToolResult{}, err
	}
	b, sessionID, member, err := t.t.board(ctx)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := authorize(b, member, ""); err != nil {
		return agenttools.ToolResult{}, err
	}
	before := b.Snapshot()
	// Reference Team tasks start unowned; claiming is an explicit CAS action.
	task, err := b.CreateTask(in.Subject, in.Description, "", in.BlockedBy, in.WriteScopes)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveEvent(ctx, sessionID, "team/task", TaskEvent{Version: 1, TeamID: b.TeamID(), Task: task.Task}); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveSnapshot(ctx, sessionID, b); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	return textResult(task, fmt.Sprintf("created %s", task.ID)), nil
}

type TaskUpdateTool struct{ t *Tools }

func (TaskUpdateTool) Name() string { return ToolTaskUpdate }
func (TaskUpdateTool) Description() string {
	return "claim, release, edit, reconfigure, complete, reopen, reassign or delete a session-scoped team task using CAS"
}
func (TaskUpdateTool) OutputSchema() map[string]any { return map[string]any{"type": "object"} }
func (TaskUpdateTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"id":       map[string]any{"type": "string", "minLength": 1},
		"revision": map[string]any{"type": "integer", "minimum": 1},
		"action":   map[string]any{"type": "string", "enum": []string{"claim", "release", "edit", "set_dependencies", "complete", "reopen", "reassign", "delete"}},
		"subject":  map[string]any{"type": "string"}, "description": map[string]any{"type": "string"},
		"blocked_by":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"write_scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"owner":        map[string]any{"type": "string"},
	}, "required": []string{"id", "revision", "action"}, "additionalProperties": false}
}
func (t TaskUpdateTool) Execute(ctx context.Context, args any) (string, error) {
	r, err := t.ExecuteResult(ctx, args)
	return r.Output, err
}
func (t TaskUpdateTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	t.t.mutationMu.Lock()
	defer t.t.mutationMu.Unlock()
	var in struct {
		ID          string   `json:"id"`
		Revision    int      `json:"revision"`
		Action      string   `json:"action"`
		Subject     string   `json:"subject"`
		Description string   `json:"description"`
		BlockedBy   []string `json:"blocked_by"`
		WriteScopes []string `json:"write_scopes"`
		Owner       string   `json:"owner"`
	}
	if err := decode(args, &in); err != nil {
		return agenttools.ToolResult{}, err
	}
	b, sessionID, member, err := t.t.board(ctx)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := authorize(b, member, ""); err != nil {
		return agenttools.ToolResult{}, err
	}
	before := b.Snapshot()
	current, err := b.GetTask(in.ID)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	roster := b.Roster()
	isLead := roster != nil && roster.IsLead(agent.ID(member))
	if current.OwnerID != "" && current.OwnerID != member && !isLead {
		return agenttools.ToolResult{}, ErrUnauthorized
	}
	if (in.Action == "reassign") && !isLead {
		return agenttools.ToolResult{}, ErrUnauthorized
	}
	if in.Action == "reassign" && strings.TrimSpace(in.Owner) != "" {
		if roster == nil {
			return agenttools.ToolResult{}, ErrUnauthorized
		}
		if err := roster.Authorize(agent.ID(strings.TrimSpace(in.Owner)), ""); err != nil {
			return agenttools.ToolResult{}, err
		}
	}
	updated, err := b.UpdateTask(Update{ID: in.ID, Revision: in.Revision, Action: in.Action, OwnerID: func() string {
		if in.Action == "reassign" {
			return in.Owner
		}
		return member
	}(), ActorID: member, Subject: in.Subject, Description: in.Description, BlockedBy: in.BlockedBy, WriteScopes: in.WriteScopes})
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveEvent(ctx, sessionID, "team/task", TaskEvent{Version: 1, TeamID: b.TeamID(), Task: updated.Task}); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveSnapshot(ctx, sessionID, b); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	return textResult(updated, fmt.Sprintf("updated %s to %s", updated.ID, updated.Status)), nil
}

type TaskListTool struct{ t *Tools }

func (TaskListTool) Name() string                 { return ToolTaskList }
func (TaskListTool) Description() string          { return "list ready and active tasks for the current team" }
func (TaskListTool) OutputSchema() map[string]any { return map[string]any{"type": "array"} }
func (TaskListTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false}
}
func (t TaskListTool) Execute(ctx context.Context, args any) (string, error) {
	r, err := t.ExecuteResult(ctx, args)
	return r.Output, err
}
func (t TaskListTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	b, _, member, err := t.t.board(ctx)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := authorize(b, member, ""); err != nil {
		return agenttools.ToolResult{}, err
	}
	items := b.ListTasks()
	return textResult(items, fmt.Sprintf("%d tasks", len(items))), nil
}

type TaskGetTool struct{ t *Tools }

func (TaskGetTool) Name() string                 { return ToolTaskGet }
func (TaskGetTool) Description() string          { return "get one session-scoped team task" }
func (TaskGetTool) OutputSchema() map[string]any { return map[string]any{"type": "object"} }
func (TaskGetTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"id"}, "additionalProperties": false}
}
func (t TaskGetTool) Execute(ctx context.Context, args any) (string, error) {
	r, err := t.ExecuteResult(ctx, args)
	return r.Output, err
}
func (t TaskGetTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := decode(args, &in); err != nil {
		return agenttools.ToolResult{}, err
	}
	b, _, member, err := t.t.board(ctx)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := authorize(b, member, ""); err != nil {
		return agenttools.ToolResult{}, err
	}
	task, err := b.GetTask(in.ID)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	return textResult(task, task.ID), nil
}

type MessageTool struct{ t *Tools }

func (MessageTool) Name() string                 { return ToolMessage }
func (MessageTool) Description() string          { return "send a durable message to a team member" }
func (MessageTool) OutputSchema() map[string]any { return map[string]any{"type": "object"} }
func (MessageTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"target": map[string]any{"type": "string", "minLength": 1}, "content": map[string]any{"oneOf": []any{
			map[string]any{"type": "string", "minLength": 1},
			map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object"}},
		}},
		"delivery": map[string]any{"type": "string", "enum": []string{"quiet", "wakeup"}},
	}, "required": []string{"target", "content"}, "additionalProperties": false}
}
func (t MessageTool) Execute(ctx context.Context, args any) (string, error) {
	r, err := t.ExecuteResult(ctx, args)
	return r.Output, err
}
func (t MessageTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	t.t.mutationMu.Lock()
	defer t.t.mutationMu.Unlock()
	var in struct {
		Target   string          `json:"target"`
		Content  json.RawMessage `json:"content"`
		Delivery string          `json:"delivery"`
	}
	if err := decode(args, &in); err != nil {
		return agenttools.ToolResult{}, err
	}
	if in.Delivery == "" {
		in.Delivery = "wakeup"
	}
	_, contentBlocks, err := DecodeMessageContent(in.Content)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	b, sessionID, member, err := t.t.board(ctx)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	target := strings.TrimSpace(in.Target)
	if roster := b.Roster(); roster != nil {
		resolved, resolveErr := roster.ResolveTarget(target)
		if resolveErr != nil {
			return agenttools.ToolResult{}, resolveErr
		}
		target = resolved.ID
	}
	if target == member {
		return agenttools.ToolResult{}, ErrSelfMessage
	}
	if err := authorize(b, member, target); err != nil {
		return agenttools.ToolResult{}, err
	}
	before := b.Snapshot()
	senderName := member
	if roster := b.Roster(); roster != nil {
		if sender, senderErr := roster.Member(agent.ID(member)); senderErr == nil {
			senderName = sender.Name
		}
	}
	m, err := b.SendMessageWithContent(member, senderName, target, contentBlocks, in.Delivery)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveEvent(ctx, sessionID, "team/message/queued", MessageQueuedEvent{Version: 1, TeamID: b.TeamID(), Message: m}); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	if err := t.t.saveSnapshot(ctx, sessionID, b); err != nil {
		_ = b.Restore(before)
		return agenttools.ToolResult{}, err
	}
	if dispatch := b.messageDispatcher(); dispatch != nil {
		delivered, dispatchErr := dispatch(ctx, m)
		if dispatchErr != nil {
			// Queueing is already durable. A transient target/runtime failure
			// must not turn a recoverable queued message into data loss.
			return textResult(m, fmt.Sprintf("queued %s", m.ID)), nil
		}
		if delivered {
			// The queue event is already durable. Commit the delivery edge first;
			// only then mutate the live board. If this append fails, the message
			// must remain pending both in memory and after restart.
			if err := t.t.saveEvent(ctx, sessionID, "team/message/delivered", MessageDeliveredEvent{Version: 1, TeamID: b.TeamID(), MessageID: m.ID, TargetID: m.TargetID}); err != nil {
				return agenttools.ToolResult{}, err
			}
			if err := b.AckMessage(m.ID); err != nil {
				return agenttools.ToolResult{}, err
			}
			m.Delivered = true
			if err := t.t.saveSnapshot(ctx, sessionID, b); err != nil {
				return agenttools.ToolResult{}, err
			}
			return textResult(m, fmt.Sprintf("delivered %s", m.ID)), nil
		}
	}
	return textResult(m, fmt.Sprintf("queued %s", m.ID)), nil
}
