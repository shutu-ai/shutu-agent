// Package plan defines the task-planning capability seam (design.md §10 D2,
// ADR 2026-08-19-m6-agent-full.md 决策 M6b): a goal → plan → todo three-layer
// domain model with a Provider + Engine seam. An Engine (the seam's Service)
// owns id issuance and validation — status legality, unknown-goal/plan/todo
// rejection, cascade removal — and a Provider is the dumb backend that stores
// goals and plans. Consumers (M6b-2's plan_* tools and the plan/* event
// wiring) depend only on the seam's interfaces (D2), so swapping or persisting
// the backend never touches consumer code.
//
// The default Provider is the in-memory memProvider (mem.go), used as a
// disposable projection. The composition root rebuilds it from the current
// session event log on startup and session switch; the store remains the
// durable source of truth and no second plan table is introduced.
//
// Status values follow the dsh goal/plan-mode/tool-todo vocabulary (pending →
// in-progress → blocked / done / cancelled). A record that reaches done gets a
// CompletedAt timestamp (goals and todos); moving back to a live status clears
// it. The tree is built bottom-up (CreateGoal → CreatePlan → AddTodo) and
// observed through the goal-rooted List aggregation tree.
package plan

import (
	"context"
	"errors"
	"time"
)

// Status is one of the five task lifecycle states shared across goals, plans
// and todos (dsh goal GoalStatus / plan-mode Status / tool-todo TodoStatus).
type Status string

// DefaultMaxGoalRounds follows dsh's deployment default for same-session goal
// continuation. A goal may override it at creation time in future API layers;
// legacy callers receive this value automatically.
const DefaultMaxGoalRounds = 256

const (
	StatusPending    Status = "pending"     // created, not started
	StatusInProgress Status = "in-progress" // actively being worked on
	StatusPaused     Status = "paused"      // intentionally paused by the user
	StatusBlocked    Status = "blocked"     // waiting on something outside the task
	StatusDone       Status = "done"        // finished; stamps CompletedAt
	StatusCancelled  Status = "cancelled"   // abandoned before completion
)

// Scope identifies one of the three plan-tree levels for SetStatus and Remove.
type Scope string

const (
	ScopeGoal Scope = "goal"
	ScopePlan Scope = "plan"
	ScopeTodo Scope = "todo"
)

// Todo is one actionable step of a plan (dsh tool-todo Todo).
type Todo struct {
	ID      string // engine-issued id ("todo-N")
	Title   string // one-line step label
	Status  Status // pending by default
	Details string // free-form step notes (optional)
	// Acceptance lists the acceptance criteria this todo must satisfy when
	// evaluated (eval seam, ADR D-EVAL-4); entries may carry a mode prefix
	// (contains:/not:/llm:/manual:). Optional.
	Acceptance  []string
	CreatedAt   time.Time
	CompletedAt *time.Time // set when Status turns done; cleared on a live status
}

// Plan is an ordered list of todos that together advance a goal (dsh plan-mode
// Plan). A plan with an empty GoalID is standalone — it belongs to no goal and
// is reachable via GetPlan, not through the goal aggregation tree.
type Plan struct {
	ID        string // engine-issued id ("plan-N")
	Title     string
	GoalID    string // owning goal id; "" = independent plan
	Status    Status
	Steps     []Todo // ordered todos (creation order)
	CreatedAt time.Time
}

// Goal is the root of the aggregation tree (dsh goal Goal): a one-paragraph
// objective that owns an ordered list of plan ids.
type Goal struct {
	ID        string // engine-issued id ("goal-N")
	Title     string
	Objective string // one-paragraph goal description
	Status    Status
	Plans     []string // plan id list in creation order (DAG under the goal)
	Owner     string   // owning session id (optional)
	// Revision changes on every durable goal mutation. It gives callers a
	// lightweight compare-and-set fence, matching dsh's GoalRef semantics.
	Revision int `json:"revision"`
	// MaxRounds is the durable per-goal continuation budget. Zero is treated as
	// the deployment default when reading legacy records.
	MaxRounds int `json:"maxRounds"`
	// RoundsStarted counts admitted same-session continuation rounds. It is
	// separate from Revision and is restored from goal/round_start facts.
	RoundsStarted int `json:"roundsStarted"`
	// BlockedReason is policy text retained for the Web/model projection.
	BlockedReason string `json:"blockedReason,omitempty"`
	CreatedAt     time.Time
	CompletedAt   *time.Time // set when Status turns done; cleared on a live status
}

// Provider is one plan backend (design.md §10 D2: Service / Provider / Consumer
// three-piece seam). It is a dumb store: the Engine performs all validation and
// id issuance and calls through Put/Get/Delete. Callers receive fresh value
// copies, never live registry state.
type Provider interface {
	Name() string
	// ListGoals returns every goal, sorted by id.
	ListGoals(ctx context.Context) ([]Goal, error)
	// GetGoal returns the goal with id; an unknown id is rejected.
	GetGoal(ctx context.Context, id string) (Goal, error)
	// PutGoal is an idempotent upsert by id (an empty id is rejected).
	PutGoal(ctx context.Context, g Goal) error
	// DeleteGoal removes the goal with id and cascades to every plan (and its
	// todos) whose GoalID matches; an unknown id is rejected.
	DeleteGoal(ctx context.Context, id string) error
	// ListPlans returns every plan, sorted by id.
	ListPlans(ctx context.Context) ([]Plan, error)
	// GetPlan returns the plan with id; an unknown id is rejected.
	GetPlan(ctx context.Context, id string) (Plan, error)
	// PutPlan is an idempotent upsert by id (an empty id is rejected).
	PutPlan(ctx context.Context, p Plan) error
	// DeletePlan removes the plan with id; an unknown id is rejected. It does
	// not touch the owning goal's Plans list — the Engine keeps that list
	// consistent.
	DeletePlan(ctx context.Context, id string) error
}

// Engine is the plan Service (design.md §10 D2, ADR 决策 M6b). Consumers depend
// only on this interface, never on a concrete backend.
//
// Lifecycle: CreateGoal/CreatePlan/AddTodo build the goal → plan → todo tree
// bottom-up; SetStatus advances or blocks one record; List observes the goal
// aggregation tree; Remove deletes a goal (cascading to its plans), a plan (and
// detaches it from its goal's Plans list) or a todo; Close releases the backend
// and rejects further operations. Close is idempotent.
type Engine interface {
	// CreateGoal validates the title and creates a pending goal with a fresh
	// engine-issued id, returning it.
	CreateGoal(ctx context.Context, title, objective string) (Goal, error)
	// UpdateGoal edits the title and objective of an existing goal.
	UpdateGoal(ctx context.Context, id, title, objective string) (Goal, error)
	// CreatePlan creates a pending plan under goalID — one pending todo per
	// step — and links it into the goal's Plans list. An unknown goalID is
	// rejected; an empty goalID creates a standalone plan.
	CreatePlan(ctx context.Context, goalID, title string, steps []string) (Plan, error)
	// AddTodo appends a pending todo to the plan with planID; an unknown
	// planID is rejected. acceptance is the optional eval criteria list
	// (ADR D-EVAL-4).
	AddTodo(ctx context.Context, planID, title string, acceptance []string) (Todo, error)
	// SetStatus sets the Status of the goal/plan/todo with id. scope is one of
	// ScopeGoal, ScopePlan or ScopeTodo. Invalid statuses and unknown ids are
	// rejected; an unknown scope is rejected. A transition to done stamps
	// CompletedAt (goals and todos); a transition away clears it.
	SetStatus(ctx context.Context, scope string, id string, st Status) error
	// StartGoalRound admits exactly the next continuation round and persists its
	// counter in the projection. The driver records the corresponding durable
	// goal/round_start fact separately in the session log.
	StartGoalRound(ctx context.Context, id string) (Goal, error)
	// List returns every goal — the goal → plans → todos aggregation tree: each
	// goal carries its ordered Plans ids, each plan its ordered Steps (via
	// GetPlan). Goals are sorted by id. Standalone plans (GoalID "") are not in
	// the tree.
	List(ctx context.Context) ([]Goal, error)
	// Remove deletes the goal/plan/todo with id. scope is one of ScopeGoal,
	// ScopePlan or ScopeTodo. Removing a goal cascades to its plans; removing a
	// plan detaches it from its goal's Plans list. Unknown scopes and ids are
	// rejected.
	Remove(ctx context.Context, scope string, id string) error
	// Close releases the backend and marks the engine closed. It is idempotent;
	// every other operation after Close is rejected with ErrEngineClosed.
	Close() error
}

// closer is the optional extension a Provider implements to release its
// resources when the Engine is closed (mirrors the schedule seam's closer).
type closer interface {
	Close() error
}

// Sentinel errors returned by Engine and Provider implementations so callers
// can distinguish failures without parsing message text.
var (
	ErrInvalidStatus  = errors.New("plan: invalid status")
	ErrInvalidTitle   = errors.New("plan: invalid title")
	ErrUnknownGoal    = errors.New("plan: unknown goal")
	ErrUnknownPlan    = errors.New("plan: unknown plan")
	ErrUnknownTodo    = errors.New("plan: unknown todo")
	ErrUnknownScope   = errors.New("plan: unknown scope")
	ErrEngineClosed   = errors.New("plan: engine closed")
	ErrProviderClosed = errors.New("plan: provider closed")
)
