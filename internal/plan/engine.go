package plan

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// engine is the default plan Service implementation (ADR 决策 M6b): it owns id
// issuance and validation — status legality, unknown-goal/plan/todo rejection,
// cascade removal — delegating storage to a Provider. It is safe for concurrent
// use; Close is idempotent and releases the Provider. The unexported concrete
// type (mirroring the schedule seam's engine) keeps the Engine interface the
// only public shape; NewEngine returns it as a concrete *engine that satisfies
// Engine.
type engine struct {
	prov Provider

	mu      sync.Mutex
	goalSeq int
	planSeq int
	todoSeq int
	closed  bool
}

// NewEngine returns an engine backed by prov; a nil prov selects the default
// in-memory Provider (newMemProvider). Each engine should own its provider:
// Close releases it.
func NewEngine(prov Provider) *engine {
	if prov == nil {
		prov = newMemProvider()
	}
	return &engine{prov: prov}
}

// CreateGoal validates the title and creates a pending goal with a fresh
// engine-issued id, returning it.
func (e *engine) CreateGoal(ctx context.Context, title, objective string) (Goal, error) {
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Goal{}, err
	}
	if title == "" {
		return Goal{}, fmt.Errorf("%w: title is empty", ErrInvalidTitle)
	}

	e.mu.Lock()
	e.goalSeq++
	g := Goal{
		ID:        fmt.Sprintf("goal-%d", e.goalSeq),
		Title:     title,
		Objective: objective,
		Status:    StatusPending,
		Plans:     []string{},
		CreatedAt: time.Now(),
	}
	e.mu.Unlock()

	if err := e.prov.PutGoal(ctx, g); err != nil {
		return Goal{}, err
	}
	return g, nil
}

// UpdateGoal edits an existing goal while preserving its id, status and plan
// links. The caller owns the durable plan/update event.
func (e *engine) UpdateGoal(ctx context.Context, id, title, objective string) (Goal, error) {
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Goal{}, err
	}
	if title == "" {
		return Goal{}, fmt.Errorf("%w: title is empty", ErrInvalidTitle)
	}
	g, err := e.prov.GetGoal(ctx, id)
	if err != nil {
		return Goal{}, err
	}
	g.Title = title
	g.Objective = objective
	if err := e.prov.PutGoal(ctx, g); err != nil {
		return Goal{}, err
	}
	return g, nil
}

// CreatePlan creates a pending plan under goalID — one pending todo per step —
// and links it into the goal's Plans list. An unknown goalID is rejected; an
// empty goalID creates a standalone plan.
func (e *engine) CreatePlan(ctx context.Context, goalID, title string, steps []string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Plan{}, err
	}
	if title == "" {
		return Plan{}, fmt.Errorf("%w: title is empty", ErrInvalidTitle)
	}
	// A non-empty goalID must reference an existing goal before anything is
	// written.
	if goalID != "" {
		if _, err := e.prov.GetGoal(ctx, goalID); err != nil {
			return Plan{}, err
		}
	}

	e.mu.Lock()
	e.planSeq++
	now := time.Now()
	plan := Plan{
		ID:        fmt.Sprintf("plan-%d", e.planSeq),
		Title:     title,
		GoalID:    goalID,
		Status:    StatusPending,
		Steps:     make([]Todo, 0, len(steps)),
		CreatedAt: now,
	}
	for _, step := range steps {
		e.todoSeq++
		plan.Steps = append(plan.Steps, Todo{
			ID:        fmt.Sprintf("todo-%d", e.todoSeq),
			Title:     step,
			Status:    StatusPending,
			CreatedAt: now,
		})
	}
	e.mu.Unlock()

	if err := e.prov.PutPlan(ctx, plan); err != nil {
		return Plan{}, err
	}
	if goalID != "" {
		if err := e.linkPlan(ctx, goalID, plan.ID); err != nil {
			return Plan{}, err
		}
	}
	return plan, nil
}

// AddTodo appends a pending todo to the plan with planID; an unknown planID is
// rejected. acceptance is the optional eval criteria list (ADR D-EVAL-4); the
// returned todo carries a copy so callers can never alias the engine's state.
func (e *engine) AddTodo(ctx context.Context, planID, title string, acceptance []string) (Todo, error) {
	if err := ctx.Err(); err != nil {
		return Todo{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Todo{}, err
	}
	if title == "" {
		return Todo{}, fmt.Errorf("%w: title is empty", ErrInvalidTitle)
	}
	p, err := e.prov.GetPlan(ctx, planID)
	if err != nil {
		return Todo{}, err
	}

	e.mu.Lock()
	e.todoSeq++
	todo := Todo{
		ID:         fmt.Sprintf("todo-%d", e.todoSeq),
		Title:      title,
		Status:     StatusPending,
		Acceptance: append([]string(nil), acceptance...),
		CreatedAt:  time.Now(),
	}
	e.mu.Unlock()

	p.Steps = append(p.Steps, todo)
	if err := e.prov.PutPlan(ctx, p); err != nil {
		return Todo{}, err
	}
	return todo, nil
}

// SetStatus sets the Status of the goal/plan/todo with id. Invalid statuses,
// unknown ids and unknown scopes are rejected; a transition to done stamps
// CompletedAt (goals and todos), a transition away clears it.
func (e *engine) SetStatus(ctx context.Context, scope string, id string, st Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkOpen(); err != nil {
		return err
	}
	if !validStatus(st) {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, st)
	}
	switch scope {
	case string(ScopeGoal):
		g, err := e.prov.GetGoal(ctx, id)
		if err != nil {
			return err
		}
		g.Status = st
		applyCompletedAt(&g.CompletedAt, st)
		return e.prov.PutGoal(ctx, g)
	case string(ScopePlan):
		p, err := e.prov.GetPlan(ctx, id)
		if err != nil {
			return err
		}
		p.Status = st
		return e.prov.PutPlan(ctx, p)
	case string(ScopeTodo):
		return e.setTodoStatus(ctx, id, st)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// setTodoStatus locates the todo with id across every plan, updates its status
// and stamps/clears CompletedAt, then stores the owning plan back. An id that
// matches no todo is rejected.
func (e *engine) setTodoStatus(ctx context.Context, id string, st Status) error {
	all, err := e.prov.ListPlans(ctx)
	if err != nil {
		return err
	}
	for i := range all {
		p := &all[i]
		for j := range p.Steps {
			if p.Steps[j].ID == id {
				p.Steps[j].Status = st
				applyCompletedAt(&p.Steps[j].CompletedAt, st)
				return e.prov.PutPlan(ctx, *p)
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrUnknownTodo, id)
}

// List returns every goal — the goal aggregation tree, sorted by id. Each goal
// carries its ordered Plans ids; the plans' ordered Steps are reached via
// GetPlan. Standalone plans are not in the tree.
func (e *engine) List(ctx context.Context) ([]Goal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}
	return e.prov.ListGoals(ctx)
}

// Remove deletes the goal/plan/todo with id. Removing a goal cascades to its
// plans (Provider contract); removing a plan detaches it from its goal's Plans
// list; removing a todo deletes it from its owning plan's Steps.
func (e *engine) Remove(ctx context.Context, scope string, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkOpen(); err != nil {
		return err
	}
	switch scope {
	case string(ScopeGoal):
		return e.prov.DeleteGoal(ctx, id)
	case string(ScopePlan):
		return e.removePlan(ctx, id)
	case string(ScopeTodo):
		return e.removeTodo(ctx, id)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownScope, scope)
	}
}

// removePlan deletes the plan and detaches its id from the owning goal's Plans
// list so the aggregation tree stays consistent.
func (e *engine) removePlan(ctx context.Context, id string) error {
	p, err := e.prov.GetPlan(ctx, id)
	if err != nil {
		return err
	}
	if err := e.prov.DeletePlan(ctx, id); err != nil {
		return err
	}
	if p.GoalID == "" {
		return nil
	}
	g, err := e.prov.GetGoal(ctx, p.GoalID)
	if err != nil {
		return err
	}
	plans := g.Plans[:0]
	for _, pid := range g.Plans {
		if pid != id {
			plans = append(plans, pid)
		}
	}
	g.Plans = plans
	return e.prov.PutGoal(ctx, g)
}

// removeTodo locates the todo with id across every plan and deletes it from the
// owning plan's Steps, storing the plan back.
func (e *engine) removeTodo(ctx context.Context, id string) error {
	all, err := e.prov.ListPlans(ctx)
	if err != nil {
		return err
	}
	for i := range all {
		p := &all[i]
		for j := range p.Steps {
			if p.Steps[j].ID == id {
				p.Steps = append(p.Steps[:j], p.Steps[j+1:]...)
				return e.prov.PutPlan(ctx, *p)
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrUnknownTodo, id)
}

// linkPlan appends planID to the owning goal's Plans list (creation order).
func (e *engine) linkPlan(ctx context.Context, goalID, planID string) error {
	g, err := e.prov.GetGoal(ctx, goalID)
	if err != nil {
		return err
	}
	g.Plans = append(g.Plans, planID)
	return e.prov.PutGoal(ctx, g)
}

// applyCompletedAt stamps dst when st is done and clears it when the record
// returns to a live status.
func applyCompletedAt(dst **time.Time, st Status) {
	if st == StatusDone {
		now := time.Now()
		*dst = &now
		return
	}
	*dst = nil
}

// validStatus reports whether s is one of the five supported statuses.
func validStatus(s Status) bool {
	switch s {
	case StatusPending, StatusInProgress, StatusPaused, StatusBlocked, StatusDone, StatusCancelled:
		return true
	default:
		return false
	}
}

// checkOpen rejects operations on a closed engine.
func (e *engine) checkOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEngineClosed
	}
	return nil
}

// Close releases the backend (if it implements closer) and marks the engine
// closed so every other operation is rejected. It is idempotent.
func (e *engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	prov := e.prov
	e.mu.Unlock()
	if c, ok := prov.(closer); ok {
		return c.Close()
	}
	return nil
}
