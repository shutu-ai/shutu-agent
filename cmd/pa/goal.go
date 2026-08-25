package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	goalservice "github.com/jabing/shutu-agent/internal/goal"
	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
)

// runIdleGoal advances the newest unfinished goal belonging to the current
// session. It is called only after the outer turn has settled, so the Runner
// may safely acquire turnMu for each continuation round without recursively
// entering the loop from a plan tool Execute.
func (a *app) runIdleGoal(ctx context.Context, interactive bool) error {
	if a.plans == nil || a.log == nil || a.currentID == "" {
		return nil
	}
	goalID, err := a.currentActiveGoal(ctx)
	if err != nil || goalID == "" {
		return err
	}
	driver := &goalservice.Driver{
		Plans:  a.plans,
		Log:    a.log,
		Armed:  func(plan.Goal) bool { return a.goalIsArmed(a.currentID) },
		Disarm: func(g plan.Goal) { a.setGoalActivation(a.currentID, false) },
		Runner: func(roundCtx context.Context, prompt string) error {
			return a.runTurn(roundCtx, prompt, interactive)
		},
		Observe: a.observeGoal,
	}
	result, err := driver.Run(ctx, goalID)
	if interactive && result.Rounds > 0 {
		fmt.Printf("\ngoal %s: %s after %d round(s)\n", result.GoalID, result.StopReason, result.Rounds)
	}
	return err
}

func (a *app) setGoalActivation(sessionID string, armed bool) {
	if sessionID == "" {
		return
	}
	a.goalActivationMu.Lock()
	defer a.goalActivationMu.Unlock()
	if a.goalActivation == nil {
		a.goalActivation = make(map[string]bool)
	}
	a.goalActivation[sessionID] = armed
}

func (a *app) goalIsArmed(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	a.goalActivationMu.Lock()
	defer a.goalActivationMu.Unlock()
	if a.goalActivation == nil {
		// Bare app instances used by the CLI/tests predate the activation map;
		// their explicit goal driver remains backwards compatible.
		return true
	}
	armed, ok := a.goalActivation[sessionID]
	return !ok || armed
}

// currentActiveGoal resolves ownership from the append-only session log and
// status from the plan engine. Plan goals are currently in-memory, while the
// session log survives restart; this makes the lookup safe when switching
// sessions and avoids accidentally driving another session's goal.
func (a *app) currentActiveGoal(ctx context.Context) (string, error) {
	goals, err := a.plans.List(ctx)
	if err != nil {
		return "", err
	}
	status := make(map[string]plan.Status, len(goals))
	for _, g := range goals {
		status[g.ID] = g.Status
	}
	for i := len(a.log.Events()) - 1; i >= 0; i-- {
		ev := a.log.Events()[i]
		if ev.Type != session.EventPlanCreate {
			continue
		}
		var data struct {
			Scope string `json:"scope"`
			ID    string `json:"id"`
		}
		if json.Unmarshal(ev.Data, &data) != nil || data.Scope != string(plan.ScopeGoal) {
			continue
		}
		switch status[data.ID] {
		case plan.StatusPending, plan.StatusInProgress:
			if !a.goalIsArmed(a.currentID) {
				return "", nil
			}
			return data.ID, nil
		}
	}
	return "", nil
}

func (a *app) observeGoal(ctx context.Context, goal plan.Goal) (string, error) {
	goals, err := a.plans.List(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "goal %s: %s (%s); plans=%d", goal.ID, goal.Title, goal.Status, len(goal.Plans))
	for _, candidate := range goals {
		if candidate.ID == goal.ID {
			continue
		}
		if candidate.Status == plan.StatusInProgress || candidate.Status == plan.StatusPending {
			fmt.Fprintf(&b, "; other active goal=%s (%s)", candidate.ID, candidate.Status)
			break
		}
	}
	if a.subagents != nil {
		children, childErr := a.subagents.ListChildren(ctx, a.currentID)
		if childErr != nil {
			fmt.Fprintf(&b, "; subagents unavailable: %v", childErr)
		} else {
			running := 0
			for _, child := range children {
				if child.Running {
					running++
				}
			}
			fmt.Fprintf(&b, "; subagents=%d running=%d", len(children), running)
		}
	}
	if a.evalEng != nil {
		records, evalErr := a.evalEng.List(ctx)
		if evalErr != nil {
			fmt.Fprintf(&b, "; eval unavailable: %v", evalErr)
		} else {
			fmt.Fprintf(&b, "; eval_records=%d", len(records))
		}
	}
	return b.String(), nil
}
