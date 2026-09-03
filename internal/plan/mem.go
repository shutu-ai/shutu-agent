package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// memProvider is the default in-memory Provider (ADR 决策 M6b). It is a
// disposable query projection: Restore folds the durable session event log
// back into it after startup or session switching. It is safe for concurrent
// use and performs no reference validation: the Engine is the validation
// boundary (Put/Get/Delete are called through by the Engine).
type memProvider struct {
	mu     sync.Mutex
	goals  map[string]Goal
	plans  map[string]Plan
	closed bool
}

// NewMemProvider returns a fresh in-memory Provider — the default backend for
// NewEngine. It is exported so wiring and tests can inject it explicitly.
func NewMemProvider() Provider {
	return newMemProvider()
}

func newMemProvider() *memProvider {
	return &memProvider{
		goals: map[string]Goal{},
		plans: map[string]Plan{},
	}
}

// Restore replaces the in-memory projection with the state folded from the
// session event log. The replacement is atomic from readers' perspective.
func (m *memProvider) Restore(events []session.Event) error {
	goals, plans, err := restoreEvents(events)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	m.goals = goals
	m.plans = plans
	return nil
}

// Name identifies the provider in the registry ("memory").
func (m *memProvider) Name() string { return "memory" }

// ListGoals returns every goal, sorted by id.
func (m *memProvider) ListGoals(ctx context.Context) ([]Goal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrProviderClosed
	}
	out := make([]Goal, 0, len(m.goals))
	for _, g := range m.goals {
		out = append(out, cloneGoal(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetGoal returns the goal with id; an unknown id is rejected.
func (m *memProvider) GetGoal(ctx context.Context, id string) (Goal, error) {
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Goal{}, ErrProviderClosed
	}
	g, ok := m.goals[id]
	if !ok {
		return Goal{}, fmt.Errorf("%w: %s", ErrUnknownGoal, id)
	}
	return cloneGoal(g), nil
}

// PutGoal is an idempotent upsert by id (an empty id is rejected).
func (m *memProvider) PutGoal(ctx context.Context, g Goal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.ID == "" {
		return errors.New("plan: invalid goal id: expected a non-empty value")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	m.goals[g.ID] = cloneGoal(g)
	return nil
}

// DeleteGoal removes the goal with id and cascades to every plan (and its
// todos) whose GoalID matches; an unknown id is rejected.
func (m *memProvider) DeleteGoal(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	if _, ok := m.goals[id]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownGoal, id)
	}
	delete(m.goals, id)
	for pid, p := range m.plans {
		if p.GoalID == id {
			delete(m.plans, pid)
		}
	}
	return nil
}

// ListPlans returns every plan, sorted by id.
func (m *memProvider) ListPlans(ctx context.Context) ([]Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrProviderClosed
	}
	out := make([]Plan, 0, len(m.plans))
	for _, p := range m.plans {
		out = append(out, clonePlan(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetPlan returns the plan with id; an unknown id is rejected.
func (m *memProvider) GetPlan(ctx context.Context, id string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Plan{}, ErrProviderClosed
	}
	p, ok := m.plans[id]
	if !ok {
		return Plan{}, fmt.Errorf("%w: %s", ErrUnknownPlan, id)
	}
	return clonePlan(p), nil
}

// PutPlan is an idempotent upsert by id (an empty id is rejected).
func (m *memProvider) PutPlan(ctx context.Context, p Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.ID == "" {
		return errors.New("plan: invalid plan id: expected a non-empty value")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	m.plans[p.ID] = clonePlan(p)
	return nil
}

// DeletePlan removes the plan with id; an unknown id is rejected. It does not
// touch the owning goal's Plans list — the Engine keeps that list consistent.
func (m *memProvider) DeletePlan(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	if _, ok := m.plans[id]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPlan, id)
	}
	delete(m.plans, id)
	return nil
}

// Close marks the provider closed so no further operations are accepted. It is
// idempotent and releases nothing else (no goroutines live here).
func (m *memProvider) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// cloneGoal copies a Goal so the returned value never aliases the record's
// Plans slice or CompletedAt pointer.
func cloneGoal(g Goal) Goal {
	out := g
	out.Plans = append([]string(nil), g.Plans...)
	if g.CompletedAt != nil {
		t := *g.CompletedAt
		out.CompletedAt = &t
	}
	return out
}

// clonePlan copies a Plan so the returned value never aliases the record's
// Steps slice or any todo's CompletedAt pointer.
func clonePlan(p Plan) Plan {
	out := p
	out.Steps = make([]Todo, len(p.Steps))
	for i, t := range p.Steps {
		out.Steps[i] = t
		if t.CompletedAt != nil {
			tt := *t.CompletedAt
			out.Steps[i].CompletedAt = &tt
		}
	}
	return out
}
