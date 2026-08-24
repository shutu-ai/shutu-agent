// Package goal implements the same-session goal round driver seam. It owns
// only continuation policy; plan state remains in internal/plan and model
// execution is injected as a runner, so the core loop is not re-entered by
// this package.
package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
)

const DefaultMaxRounds = 8

var (
	ErrNoRunner     = errors.New("goal: runner is not configured")
	ErrUnknownGoal  = errors.New("goal: unknown goal")
	ErrInvalidRound = errors.New("goal: invalid round configuration")
)

// Runner executes one user-facing turn in the same session. The runner must
// be serialized by its owner; Driver never starts a second round concurrently.
type Runner func(ctx context.Context, prompt string) error

// Observer supplies current external progress (for example subagent/eval
// summaries) to the next round prompt. It is fail-open: an observer error is
// recorded in the prompt as unavailable progress rather than aborting work.
type Observer func(ctx context.Context, goal plan.Goal) (string, error)

type Result struct {
	GoalID     string
	Rounds     int
	Status     plan.Status
	StopReason string // completed | blocked | round-limit | error | cancelled
}

type Driver struct {
	Plans     plan.Engine
	Log       *session.Log
	Runner    Runner
	Observe   Observer
	MaxRounds int
}

func (d *Driver) Run(ctx context.Context, goalID string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d.Plans == nil {
		return Result{}, fmt.Errorf("%w: plan engine", ErrNoRunner)
	}
	if d.Runner == nil {
		return Result{}, ErrNoRunner
	}
	max := d.MaxRounds
	if max == 0 {
		max = DefaultMaxRounds
	}
	if max < 0 {
		return Result{}, ErrInvalidRound
	}
	goal, err := findGoal(ctx, d.Plans, goalID)
	if err != nil {
		return Result{}, err
	}
	if goal.Status == plan.StatusDone {
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: "completed"}, nil
	}
	if goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled {
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: string(goal.Status)}, nil
	}
	if err := d.Plans.SetStatus(ctx, string(plan.ScopeGoal), goal.ID, plan.StatusInProgress); err != nil {
		return Result{}, err
	}
	for round := 1; round <= max; round++ {
		if err := ctx.Err(); err != nil {
			_ = d.Plans.SetStatus(context.Background(), string(plan.ScopeGoal), goal.ID, plan.StatusBlocked)
			d.appendEnd(goal.ID, round, "cancelled", err.Error())
			return Result{GoalID: goal.ID, Rounds: round - 1, Status: plan.StatusBlocked, StopReason: "cancelled"}, err
		}
		goal, err = findGoal(ctx, d.Plans, goalID)
		if err != nil {
			return Result{}, err
		}
		if goal.Status == plan.StatusDone {
			return Result{GoalID: goal.ID, Rounds: round - 1, Status: goal.Status, StopReason: "completed"}, nil
		}
		if goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled {
			return Result{GoalID: goal.ID, Rounds: round - 1, Status: goal.Status, StopReason: string(goal.Status)}, nil
		}
		prompt := renderPrompt(goal, round, max, d.Observe, ctx)
		d.appendStart(goal.ID, round, prompt)
		runErr := d.Runner(ctx, prompt)
		if runErr != nil {
			_ = d.Plans.SetStatus(context.Background(), string(plan.ScopeGoal), goal.ID, plan.StatusBlocked)
			d.appendEnd(goal.ID, round, "error", runErr.Error())
			return Result{GoalID: goal.ID, Rounds: round, Status: plan.StatusBlocked, StopReason: "error"}, runErr
		}
		goal, err = findGoal(ctx, d.Plans, goalID)
		if err != nil {
			return Result{}, err
		}
		d.appendEnd(goal.ID, round, string(goal.Status), "")
		if goal.Status == plan.StatusDone {
			return Result{GoalID: goal.ID, Rounds: round, Status: goal.Status, StopReason: "completed"}, nil
		}
		if goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled {
			return Result{GoalID: goal.ID, Rounds: round, Status: goal.Status, StopReason: string(goal.Status)}, nil
		}
	}
	_ = d.Plans.SetStatus(context.Background(), string(plan.ScopeGoal), goal.ID, plan.StatusBlocked)
	d.appendEnd(goal.ID, max, "round-limit", "")
	return Result{GoalID: goal.ID, Rounds: max, Status: plan.StatusBlocked, StopReason: "round-limit"}, nil
}

func findGoal(ctx context.Context, e plan.Engine, id string) (plan.Goal, error) {
	goals, err := e.List(ctx)
	if err != nil {
		return plan.Goal{}, err
	}
	for _, g := range goals {
		if g.ID == id {
			return g, nil
		}
	}
	return plan.Goal{}, fmt.Errorf("%w: %s", ErrUnknownGoal, id)
}

func renderPrompt(goal plan.Goal, round, max int, observer Observer, ctx context.Context) string {
	data, _ := json.Marshal(goal.Objective)
	prompt := fmt.Sprintf("<goal_round>\nObjective: %s\nRound: %d/%d\n\nContinue working toward this objective in this same session. Treat the current workspace, tool results, and durable plan state as authoritative. Verify evidence before marking the goal done; leave it active if work remains.\n", data, round, max)
	if observer != nil {
		state, err := observer(ctx, goal)
		if err != nil {
			state = "progress observer unavailable: " + err.Error()
		}
		if state != "" {
			prompt += "Observed progress:\n" + state + "\n"
		}
	}
	return prompt + "</goal_round>"
}

func (d *Driver) appendStart(id string, round int, prompt string) {
	if d.Log != nil {
		_, _ = d.Log.Append(session.EventGoalRoundStart, session.NewGoalRoundStart(id, round, prompt))
	}
}
func (d *Driver) appendEnd(id string, round int, status, errText string) {
	if d.Log != nil {
		_, _ = d.Log.Append(session.EventGoalRoundEnd, session.NewGoalRoundEnd(id, round, status, errText))
	}
}
