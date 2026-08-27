// Package goal implements the same-session goal round driver seam. It owns
// continuation policy; durable goal state remains in internal/plan and model
// execution is injected as a runner.
package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
)

// dsh's default deployment cap. A Goal may carry a smaller durable cap.
const DefaultMaxRounds = 256

var (
	ErrNoRunner     = errors.New("goal: runner is not configured")
	ErrUnknownGoal  = errors.New("goal: unknown goal")
	ErrInvalidRound = errors.New("goal: invalid round configuration")
)

// Runner executes one user-facing turn in the same session. The runner must
// be serialized by its owner; Driver never starts a second round concurrently.
type Runner func(ctx context.Context, prompt string) error

// Observer supplies current external progress to the next round prompt. It is
// fail-open: observer errors are rendered as unavailable progress.
type Observer func(ctx context.Context, goal plan.Goal) (string, error)

type Result struct {
	GoalID     string
	Rounds     int
	Status     plan.Status
	StopReason string // completed | paused | disarmed | round-limit | error | cancelled
}

type Driver struct {
	Plans     plan.Engine
	Log       *session.Log
	Runner    Runner
	Observe   Observer
	MaxRounds int
	// Armed is process-local continuation authority. It is never persisted;
	// nil keeps standalone callers backwards compatible and means armed.
	Armed func(plan.Goal) bool
	// Disarm removes automatic continuation authority without changing the
	// durable objective. dsh uses this after resume/session-start and failures.
	Disarm func(plan.Goal)
}

func (d *Driver) Run(ctx context.Context, goalID string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d.Plans == nil || d.Runner == nil {
		return Result{}, ErrNoRunner
	}
	goal, err := findGoal(ctx, d.Plans, goalID)
	if err != nil {
		return Result{}, err
	}
	if goal.Status == plan.StatusDone {
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: "completed"}, nil
	}
	if goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled || goal.Status == plan.StatusPaused {
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: string(goal.Status)}, nil
	}
	if d.Armed != nil && !d.Armed(goal) {
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: "disarmed"}, nil
	}

	max := d.MaxRounds
	if max <= 0 {
		max = goal.MaxRounds
	}
	if max <= 0 {
		max = DefaultMaxRounds
	}
	if max < 1 {
		return Result{}, ErrInvalidRound
	}
	if goal.RoundsStarted >= max {
		d.disarm(goal)
		return Result{GoalID: goal.ID, Status: goal.Status, StopReason: "round-limit"}, nil
	}
	if goal.Status == plan.StatusPending {
		if err := d.setStatus(ctx, goal.ID, plan.StatusInProgress); err != nil {
			return Result{}, err
		}
	}

	started := 0
	remaining := max - goal.RoundsStarted
	for started < remaining {
		if err := ctx.Err(); err != nil {
			d.disarm(goal)
			_ = d.setStatus(context.Background(), goal.ID, plan.StatusPaused)
			d.appendEnd(goal.ID, goal.RoundsStarted, "cancelled", err.Error())
			return Result{GoalID: goal.ID, Rounds: started, Status: plan.StatusPaused, StopReason: "cancelled"}, err
		}
		goal, err = findGoal(ctx, d.Plans, goalID)
		if err != nil {
			return Result{}, err
		}
		if goal.Status == plan.StatusDone || goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled || goal.Status == plan.StatusPaused {
			reason := string(goal.Status)
			if goal.Status == plan.StatusDone {
				reason = "completed"
			}
			return Result{GoalID: goal.ID, Rounds: started, Status: goal.Status, StopReason: reason}, nil
		}
		if d.Armed != nil && !d.Armed(goal) {
			return Result{GoalID: goal.ID, Rounds: started, Status: goal.Status, StopReason: "disarmed"}, nil
		}

		admitted := goal.RoundsStarted + 1
		if counter, ok := d.Plans.(interface {
			StartGoalRound(context.Context, string) (plan.Goal, error)
		}); ok {
			goal, err = counter.StartGoalRound(ctx, goal.ID)
			if err != nil {
				return Result{}, err
			}
			admitted = goal.RoundsStarted
		}
		prompt := renderPrompt(goal, admitted, max, d.Observe, ctx)
		d.appendStart(goal.ID, admitted, prompt)
		started++
		runErr := d.Runner(plan.WithGoalRound(ctx), prompt)
		if runErr != nil {
			d.disarm(goal)
			if ctx.Err() != nil {
				_ = d.setStatus(context.Background(), goal.ID, plan.StatusPaused)
				d.appendEnd(goal.ID, admitted, "cancelled", ctx.Err().Error())
				return Result{GoalID: goal.ID, Rounds: started, Status: plan.StatusPaused, StopReason: "cancelled"}, ctx.Err()
			}
			// Provider and persistence failures are not prompt-level blockers. The
			// objective stays active and requires an explicit resume.
			d.appendEnd(goal.ID, admitted, "error", runErr.Error())
			return Result{GoalID: goal.ID, Rounds: started, Status: plan.StatusInProgress, StopReason: "error"}, runErr
		}

		goal, err = findGoal(ctx, d.Plans, goalID)
		if err != nil {
			return Result{}, err
		}
		d.appendEnd(goal.ID, admitted, string(goal.Status), "")
		if goal.Status == plan.StatusDone {
			return Result{GoalID: goal.ID, Rounds: started, Status: goal.Status, StopReason: "completed"}, nil
		}
		if goal.Status == plan.StatusBlocked || goal.Status == plan.StatusCancelled || goal.Status == plan.StatusPaused {
			return Result{GoalID: goal.ID, Rounds: started, Status: goal.Status, StopReason: string(goal.Status)}, nil
		}
	}
	d.disarm(goal)
	d.appendEnd(goal.ID, goal.RoundsStarted, "round-limit", "")
	return Result{GoalID: goal.ID, Rounds: started, Status: goal.Status, StopReason: "round-limit"}, nil
}

func (d *Driver) disarm(goal plan.Goal) {
	if d.Disarm != nil {
		d.Disarm(goal)
	}
}

func (d *Driver) setStatus(ctx context.Context, id string, status plan.Status) error {
	if err := d.Plans.SetStatus(ctx, string(plan.ScopeGoal), id, status); err != nil {
		return err
	}
	if d.Log != nil {
		_, _ = d.Log.Append(session.EventPlanStatus, session.NewPlanStatus(string(plan.ScopeGoal), id, string(status)))
	}
	return nil
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
	prompt := fmt.Sprintf("<goal_round>\nObjective: %s\nRound: %d/%d\n\nContinue working toward the objective in this same session. Treat the current workspace, tool results, and durable session state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current goal, and mark it complete. If work remains, leave the goal active for the next round. Follow the configured goal-tool policy before reporting a blocker.\n", data, round, max)
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
