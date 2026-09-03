package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/plan"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

type nativeGoalRuntime struct {
	engine    plan.Engine
	log       *session.Log
	sessionID string
}

// nativeGoalMutation is the composition-root side of DSH's mutation-only
// goal.* API. The addressed session, rather than the process-global REPL
// selection, owns both the plan projection and durable event log.
func (a *app) nativeGoalMutation(ctx context.Context, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	if a.plans == nil {
		return webserver.NativeGoalMutationResult{}, errors.New("planning is not available for the active session")
	}
	sessionID := strings.TrimSpace(mutation.SessionID)
	if sessionID == "" {
		return webserver.NativeGoalMutationResult{}, errors.New("session id is required")
	}
	ctx = runtimectx.With(ctx, runtimectx.Runtime{SessionID: sessionID})
	log, err := a.sessionLogForAgent(ctx, sessionID)
	if err != nil {
		// Bare app instances used by direct CLI/unit tests may not have a
		// store yet. This fallback is safe only for the same explicitly
		// addressed current session; non-current sessions must have their own
		// durable runtime materialized.
		if a.agentRegistry != nil || sessionID != a.currentID || a.log == nil {
			return webserver.NativeGoalMutationResult{}, err
		}
		log = a.log
	}
	engine, err := a.planEngineFor(ctx)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	runtime := nativeGoalRuntime{engine: engine, log: log, sessionID: sessionID}
	a.nativeGoalMu.Lock()
	defer a.nativeGoalMu.Unlock()

	switch mutation.Action {
	case "goal.create":
		return a.nativeCreateGoal(ctx, runtime, mutation)
	case "goal.edit":
		return a.nativeEditGoal(ctx, runtime, mutation)
	case "goal.pause":
		return a.nativeSetGoalStatus(ctx, runtime, mutation, plan.StatusPaused)
	case "goal.resume":
		return a.nativeSetGoalStatus(ctx, runtime, mutation, plan.StatusInProgress)
	case "goal.complete":
		return a.nativeSetGoalStatus(ctx, runtime, mutation, plan.StatusDone)
	case "goal.clear":
		return a.nativeClearGoal(ctx, runtime, mutation)
	default:
		return webserver.NativeGoalMutationResult{}, fmt.Errorf("unknown native goal action %q", mutation.Action)
	}
}

func (a *app) nativeCreateGoal(ctx context.Context, runtime nativeGoalRuntime, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	activeGoalID, err := a.currentActiveGoalFor(ctx, runtime.sessionID, runtime.log)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	} else if activeGoalID != "" {
		return webserver.NativeGoalMutationResult{}, errors.New("a goal is already active")
	}
	objective := strings.TrimSpace(valueOrEmpty(mutation.Objective))
	if objective == "" {
		return webserver.NativeGoalMutationResult{}, errors.New("objective is required")
	}
	title := nativeGoalTitle(objective)
	var created plan.Goal
	if creator, ok := runtime.engine.(interface {
		CreateGoalWithMaxRounds(context.Context, string, string, int) (plan.Goal, error)
	}); ok {
		maxRounds := plan.DefaultMaxGoalRounds
		if mutation.MaxGoalRounds != nil {
			maxRounds = *mutation.MaxGoalRounds
		}
		created, err = creator.CreateGoalWithMaxRounds(ctx, title, objective, maxRounds)
	} else {
		created, err = runtime.engine.CreateGoal(ctx, title, objective)
	}
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := runtime.log.Append(session.EventPlanCreate, session.NewPlanCreate(string(plan.ScopeGoal), created.ID, created.Title, nil, map[string]any{
		"objective":     created.Objective,
		"status":        created.Status,
		"revision":      created.Revision,
		"maxRounds":     created.MaxRounds,
		"roundsStarted": created.RoundsStarted,
		"createdAt":     created.CreatedAt,
	})); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(runtime.sessionID, true)
	return webserver.NativeGoalMutationResult{GoalID: created.ID, Revision: created.Revision}, nil
}

func (a *app) nativeEditGoal(ctx context.Context, runtime nativeGoalRuntime, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, runtime.engine, mutation.GoalID, mutation.Revision)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	updater, ok := runtime.engine.(interface {
		UpdateGoalIfRevision(context.Context, string, int, *string, *int) (plan.Goal, error)
	})
	if !ok {
		return webserver.NativeGoalMutationResult{}, errors.New("goal edit is not supported by the plan engine")
	}
	updated, err := updater.UpdateGoalIfRevision(ctx, current.ID, mutation.Revision, mutation.Objective, mutation.MaxGoalRounds)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := runtime.log.Append(session.EventPlanUpdate, session.NewPlanUpdate(string(plan.ScopeGoal), updated.ID, map[string]any{
		"title":     updated.Title,
		"objective": updated.Objective,
		"maxRounds": updated.MaxRounds,
	})); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	return webserver.NativeGoalMutationResult{GoalID: updated.ID, Revision: updated.Revision}, nil
}

func (a *app) nativeSetGoalStatus(ctx context.Context, runtime nativeGoalRuntime, mutation webserver.NativeGoalMutation, status plan.Status) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, runtime.engine, mutation.GoalID, mutation.Revision)
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
	if setter, ok := runtime.engine.(interface {
		SetGoalStatusIfRevision(context.Context, string, int, plan.Status) error
	}); ok {
		err = setter.SetGoalStatusIfRevision(ctx, current.ID, mutation.Revision, status)
	} else {
		err = runtime.engine.SetStatus(ctx, string(plan.ScopeGoal), current.ID, status)
	}
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := runtime.log.Append(session.EventPlanStatus, session.NewPlanStatus(string(plan.ScopeGoal), current.ID, string(status))); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(runtime.sessionID, status == plan.StatusInProgress || status == plan.StatusPending)
	updated, err := a.nativeGoalByID(ctx, runtime.engine, current.ID, 0)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	return webserver.NativeGoalMutationResult{GoalID: updated.ID, Revision: updated.Revision}, nil
}

func (a *app) nativeClearGoal(ctx context.Context, runtime nativeGoalRuntime, mutation webserver.NativeGoalMutation) (webserver.NativeGoalMutationResult, error) {
	current, err := a.nativeGoalByID(ctx, runtime.engine, mutation.GoalID, mutation.Revision)
	if err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if err := runtime.engine.Remove(ctx, string(plan.ScopeGoal), current.ID); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	if _, err := runtime.log.Append(session.EventPlanDelete, session.NewPlanDelete(string(plan.ScopeGoal), current.ID)); err != nil {
		return webserver.NativeGoalMutationResult{}, err
	}
	a.setGoalActivation(runtime.sessionID, false)
	return webserver.NativeGoalMutationResult{GoalID: current.ID, Revision: current.Revision + 1, Cleared: true}, nil
}

func (a *app) nativeGoalByID(ctx context.Context, engine plan.Engine, id string, revision int) (plan.Goal, error) {
	goals, err := engine.List(ctx)
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
