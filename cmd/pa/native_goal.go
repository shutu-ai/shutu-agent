package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/webserver"
)

// nativeGoalMutation is the composition-root side of DSH's mutation-only
// goal.* API. The active session log remains the durable source of truth; the
// plan engine is updated first and its corresponding fact is then appended.
func (a *app) nativeGoalMutation(ctx context.Context, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	if a.plans == nil || a.log == nil || a.currentID == "" {
		return webserver.NativeGoalMutationResult{}, errors.New("planning is not available for the active session")
	}
	if strings.TrimSpace(mutation.SessionID) != a.currentID {
		return webserver.NativeGoalMutationResult{}, fmt.Errorf("session %q is not the active session", mutation.SessionID)
	}

	switch mutation.Action {
	case "goal.create":
		return a.nativeCreateGoal(ctx, mutation)
	case "goal.edit":
		return a.nativeEditGoal(ctx, mutation)
	case "goal.pause":
		return a.nativeSetGoalStatus(ctx, mutation, plan.StatusPaused)
	case "goal.resume":
		return a.nativeSetGoalStatus(ctx, mutation, plan.StatusInProgress)
	case "goal.complete":
		return a.nativeSetGoalStatus(ctx, mutation, plan.StatusDone)
	case "goal.clear":
		return a.nativeClearGoal(ctx, mutation)
	default:
		return webserver.NativeGoalMutationResult{}, fmt.Errorf("unknown native goal action %q", mutation.Action)
	}
}

func (a *app) nativeCreateGoal(ctx context.Context, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	if _, ok, err := a.currentGoal(ctx); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	} else if ok {
		return webserver.NativeGoalMutationResult{}, errors.New("a goal is already active")
	}
	objective := strings.TrimSpace(valueOrEmpty(mutation.Objective))
	if objective == "" {
		return webserver.NativeGoalMutationResult{}, errors.New("objective is required")
	}
	title := nativeGoalTitle(objective)
	var created plan.Goal
	var err error
	if creator, ok := a.plans.(interface {
		CreateGoalWithMaxRounds(context.Context, string, string, int) (plan.Goal, error)
	}); ok {
		maxRounds := plan.DefaultMaxGoalRounds
		if mutation.MaxGoalRounds != nil {
			maxRounds = *mutation.MaxGoalRounds
		}
		created, err = creator.CreateGoalWithMaxRounds(ctx, title, objective, maxRounds)
	} else {
		created, err = a.plans.CreateGoal(ctx, title, objective)
	}
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := a.log.Append(session.EventPlanCreate, session.NewPlanCreate(string(plan.ScopeGoal), created.ID, created.Title, nil, map[string]any{
		"objective":     created.Objective,
		"status":        created.Status,
		"revision":      created.Revision,
		"maxRounds":     created.MaxRounds,
		"roundsStarted": created.RoundsStarted,
		"createdAt":     created.CreatedAt,
	})); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(a.currentID, true)
	return webserver.NativeGoalMutationResult{GoalID: created.ID, Revision: created.Revision}, nil
}

func (a *app) nativeEditGoal(ctx context.Context, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, mutation.GoalID, mutation.Revision)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	updater, ok := a.plans.(interface {
		UpdateGoalIfRevision(context.Context, string, int, *string, *int) (plan.Goal, error)
	})
	if !ok {
		return webserver.NativeGoalMutationResult{}, errors.New("goal edit is not supported by the plan engine")
	}
	updated, err := updater.UpdateGoalIfRevision(ctx, current.ID, mutation.Revision, mutation.Objective, mutation.MaxGoalRounds)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := a.log.Append(session.EventPlanUpdate, session.NewPlanUpdate(string(plan.ScopeGoal), updated.ID, map[string]any{
		"title":     updated.Title,
		"objective": updated.Objective,
		"maxRounds": updated.MaxRounds,
	})); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	return webserver.NativeGoalMutationResult{GoalID: updated.ID, Revision: updated.Revision}, nil
}

func (a *app) nativeSetGoalStatus(ctx context.Context, mutation webserver.NativeGoalMutation, status plan.Status) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, mutation.GoalID, mutation.Revision)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	switch mutation.Action {
	case "goal.pause":
		if current.Status != plan.StatusPending && current.Status != plan.StatusInProgress {
			return webserver.NativeGoalMutationResult{}, fmt.Errorf("goal is not active (%s)", current.Status)
		}
	case "goal.resume":
		if current.Status != plan.StatusPaused {
			return webserver.NativeGoalMutationResult{}, fmt.Errorf("goal is not paused (%s)", current.Status)
		}
	case "goal.complete":
		if current.Status == plan.StatusDone || current.Status == plan.StatusCancelled {
			return webserver.NativeGoalMutationResult{}, fmt.Errorf("goal is already terminal (%s)", current.Status)
		}
	}
	if setter, ok := a.plans.(interface {
		SetGoalStatusIfRevision(context.Context, string, int, plan.Status) error
	}); ok {
		err = setter.SetGoalStatusIfRevision(ctx, current.ID, mutation.Revision, status)
	} else {
		err = a.plans.SetStatus(ctx, string(plan.ScopeGoal), current.ID, status)
	}
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := a.log.Append(session.EventPlanStatus, session.NewPlanStatus(string(plan.ScopeGoal), current.ID, string(status))); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(a.currentID, status == plan.StatusInProgress || status == plan.StatusPending)
	updated, err := a.nativeGoalByID(ctx, current.ID, 0)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	return webserver.NativeGoalMutationResult{GoalID: updated.ID, Revision: updated.Revision}, nil
}

func (a *app) nativeClearGoal(ctx context.Context, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, mutation.GoalID, mutation.Revision)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if err := a.plans.Remove(ctx, string(plan.ScopeGoal), current.ID); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := a.log.Append(session.EventPlanDelete, session.NewPlanDelete(string(plan.ScopeGoal), current.ID)); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(a.currentID, false)
	return webserver.NativeGoalMutationResult{Cleared: true}, nil
}

func (a *app) nativeGoalByID(ctx context.Context, id string, revision int) (plan.Goal, error) {
	goals, err := a.plans.List(ctx)
	if err != nil {
		return plan.Goal{}, err
	}
	for _, candidate := range goals {
		if candidate.ID != id {
			continue
		}
		if revision > 0 && candidate.Revision != revision {
			return plan.Goal{}, fmt.Errorf("stale goal revision %d (current %d)", revision, candidate.Revision)
		}
		return candidate, nil
	}
	return plan.Goal{}, fmt.Errorf("goal %q was not found", id)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nativeGoalTitle(objective string) string {
	if fields := strings.Fields(objective); len(fields) > 0 {
		return fields[0]
	}
	return "Goal"
}
