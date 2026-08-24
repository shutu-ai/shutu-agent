package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

// EventRestorer is implemented by the built-in plan engine. It rebuilds the
// provider projection from the current session event log; the session log
// remains the source of truth and the plan provider remains a replaceable
// in-memory projection.
type EventRestorer interface {
	Restore([]session.Event) error
}

type planCreateEvent struct {
	Scope      string         `json:"scope"`
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Acceptance []string       `json:"acceptance"`
	Detail     map[string]any `json:"detail"`
}

type planUpdateEvent struct {
	Scope     string `json:"scope"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Objective string `json:"objective"`
}

type planSnapshot struct {
	Objective string    `json:"objective"`
	GoalID    string    `json:"goalId"`
	PlanID    string    `json:"planId"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	Steps     []Todo    `json:"steps"`
}

type planStatusEvent struct {
	Scope  string `json:"scope"`
	ID     string `json:"id"`
	Status Status `json:"status"`
}

type planDeleteEvent struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
}

// restoreEvents folds plan facts into fresh maps. It is deliberately
// deterministic and idempotent: calling it repeatedly starts from empty
// state and produces the same projection for the same event sequence.
func restoreEvents(events []session.Event) (map[string]Goal, map[string]Plan, error) {
	goals := map[string]Goal{}
	plans := map[string]Plan{}
	var lastGoalID, lastPlanID string

	for _, ev := range events {
		switch ev.Type {
		case session.EventPlanCreate:
			var data planCreateEvent
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				return nil, nil, fmt.Errorf("plan: decode %s: %w", ev.Type, err)
			}
			if data.ID == "" || data.Scope == "" {
				return nil, nil, fmt.Errorf("plan: invalid %s payload", ev.Type)
			}
			snap, err := decodeSnapshot(data.Detail)
			if err != nil {
				return nil, nil, fmt.Errorf("plan: decode %s snapshot: %w", ev.Type, err)
			}
			switch data.Scope {
			case string(ScopeGoal):
				created := snap.CreatedAt
				if created.IsZero() {
					created = ev.At
				}
				status := snap.Status
				if !validStatus(status) {
					status = StatusPending
				}
				goals[data.ID] = Goal{
					ID: data.ID, Title: data.Title, Objective: snap.Objective,
					Status: status, Plans: []string{}, CreatedAt: created,
				}
				lastGoalID = data.ID
			case string(ScopePlan):
				created := snap.CreatedAt
				if created.IsZero() {
					created = ev.At
				}
				status := snap.Status
				if !validStatus(status) {
					status = StatusPending
				}
				goalID := snap.GoalID
				// Pre-M6b-2 events did not carry parent ids. Preserve those
				// sessions with the best deterministic event-order fallback.
				if goalID == "" {
					goalID = lastGoalID
				}
				p := Plan{ID: data.ID, Title: data.Title, GoalID: goalID, Status: status, Steps: append([]Todo(nil), snap.Steps...), CreatedAt: created}
				plans[data.ID] = p
				if g, ok := goals[goalID]; ok {
					if !containsID(g.Plans, data.ID) {
						g.Plans = append(g.Plans, data.ID)
						goals[goalID] = g
					}
				}
				lastPlanID = data.ID
			case string(ScopeTodo):
				planID := snap.PlanID
				if planID == "" {
					planID = lastPlanID
				}
				p, ok := plans[planID]
				if !ok {
					// A legacy todo event without a parent cannot be placed
					// safely; keep replay fail-closed for malformed new events,
					// but tolerate old opaque facts.
					if len(data.Detail) == 0 {
						continue
					}
					return nil, nil, fmt.Errorf("plan: todo %s references unknown plan %s", data.ID, planID)
				}
				todo := Todo{ID: data.ID, Title: data.Title, Acceptance: append([]string(nil), data.Acceptance...), Status: snap.Status, CreatedAt: snap.CreatedAt}
				if !validStatus(todo.Status) {
					todo.Status = StatusPending
				}
				if todo.CreatedAt.IsZero() {
					todo.CreatedAt = ev.At
				}
				p.Steps = upsertTodo(p.Steps, todo)
				plans[planID] = p
			default:
				return nil, nil, fmt.Errorf("plan: unknown create scope %q", data.Scope)
			}

		case session.EventPlanStatus:
			var data planStatusEvent
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				return nil, nil, fmt.Errorf("plan: decode %s: %w", ev.Type, err)
			}
			if !validStatus(data.Status) {
				continue
			}
			switch data.Scope {
			case string(ScopeGoal):
				if g, ok := goals[data.ID]; ok {
					g.Status = data.Status
					applyRestoreCompletedAt(&g.CompletedAt, data.Status, ev.At)
					goals[data.ID] = g
				}
			case string(ScopePlan):
				if p, ok := plans[data.ID]; ok {
					p.Status = data.Status
					plans[data.ID] = p
				}
			case string(ScopeTodo):
				for pid, p := range plans {
					for i := range p.Steps {
						if p.Steps[i].ID == data.ID {
							p.Steps[i].Status = data.Status
							applyRestoreCompletedAt(&p.Steps[i].CompletedAt, data.Status, ev.At)
							plans[pid] = p
						}
					}
				}
			}

		case session.EventPlanUpdate:
			var data planUpdateEvent
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				return nil, nil, fmt.Errorf("plan: decode %s: %w", ev.Type, err)
			}
			if data.Scope == string(ScopeGoal) {
				if g, ok := goals[data.ID]; ok {
					if data.Title != "" {
						g.Title = data.Title
					}
					g.Objective = data.Objective
					goals[data.ID] = g
				}
			}

		case session.EventPlanDelete:
			var data planDeleteEvent
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				return nil, nil, fmt.Errorf("plan: decode %s: %w", ev.Type, err)
			}
			switch data.Scope {
			case string(ScopeGoal):
				delete(goals, data.ID)
				for pid, p := range plans {
					if p.GoalID == data.ID {
						delete(plans, pid)
					}
				}
			case string(ScopePlan):
				if p, ok := plans[data.ID]; ok {
					delete(plans, data.ID)
					if g, ok := goals[p.GoalID]; ok {
						g.Plans = removeID(g.Plans, data.ID)
						goals[p.GoalID] = g
					}
				}
			case string(ScopeTodo):
				for pid, p := range plans {
					p.Steps = removeTodo(p.Steps, data.ID)
					plans[pid] = p
				}
			}
		}
	}
	return goals, plans, nil
}

func decodeSnapshot(detail map[string]any) (planSnapshot, error) {
	if len(detail) == 0 {
		return planSnapshot{}, nil
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return planSnapshot{}, err
	}
	var out planSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		return planSnapshot{}, err
	}
	return out, nil
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, candidate := range ids {
		if candidate != id {
			out = append(out, candidate)
		}
	}
	return out
}

func upsertTodo(steps []Todo, todo Todo) []Todo {
	for i := range steps {
		if steps[i].ID == todo.ID {
			steps[i] = todo
			return steps
		}
	}
	return append(steps, todo)
}

func removeTodo(steps []Todo, id string) []Todo {
	out := steps[:0]
	for _, todo := range steps {
		if todo.ID != id {
			out = append(out, todo)
		}
	}
	return out
}

func applyRestoreCompletedAt(dst **time.Time, status Status, at time.Time) {
	if status == StatusDone {
		stamp := at
		if stamp.IsZero() {
			stamp = time.Unix(0, 0).UTC()
		}
		*dst = &stamp
		return
	}
	*dst = nil
}

func maxID(ids ...string) int {
	max := 0
	for _, id := range ids {
		parts := strings.Split(id, "-")
		if len(parts) < 2 {
			continue
		}
		n, err := strconv.Atoi(parts[len(parts)-1])
		if err == nil && n > max {
			max = n
		}
	}
	return max
}

// Restore rebuilds the built-in engine from session plan facts and reseeds id
// issuance so a post-restart create cannot collide with an existing record.
func (e *engine) Restore(events []session.Event) error {
	if err := e.checkOpen(); err != nil {
		return err
	}
	r, ok := e.prov.(interface {
		Restore([]session.Event) error
	})
	if !ok {
		return fmt.Errorf("plan: provider %q cannot restore events", e.prov.Name())
	}
	if err := r.Restore(events); err != nil {
		return err
	}
	ctx := context.Background()
	goals, err := e.prov.ListGoals(ctx)
	if err != nil {
		return err
	}
	plans, err := e.prov.ListPlans(ctx)
	if err != nil {
		return err
	}
	goalIDs := make([]string, 0, len(goals))
	planIDs := make([]string, 0, len(plans))
	todoIDs := make([]string, 0)
	for _, g := range goals {
		goalIDs = append(goalIDs, g.ID)
	}
	for _, p := range plans {
		planIDs = append(planIDs, p.ID)
		for _, todo := range p.Steps {
			todoIDs = append(todoIDs, todo.ID)
		}
	}
	e.mu.Lock()
	e.goalSeq = maxID(goalIDs...)
	e.planSeq = maxID(planIDs...)
	e.todoSeq = maxID(todoIDs...)
	e.mu.Unlock()
	return nil
}
