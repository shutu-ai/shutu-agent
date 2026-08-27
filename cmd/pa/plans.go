// plans.go — the M6b-2 composition-root orchestration (dispatch-m6b-2 §4).
// This is where the task-planning capability seam is wired into the REPL:
// registerPlans creates the in-memory Provider + Engine and registers the six
// plan_* tools when plan.enabled (D10), and wires the D3 event sink so
// plan/create, plan/status, plan/delete and plan/list are appended to the
// active session log. There is deliberately no pre-step injector: M6b is a
// planning model only — the model drives it entirely through the plan_* tools
// on the serial tool path (D5), and execution delegation to subagents is
// deferred to M6c+. The loop's turn/step structure is untouched (D4).
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/tools"
)

// registerPlans creates the event-replayable Provider + Engine and registers
// the six plan_* tools when plan.enabled, and wires the D3 event sink. When
// plan is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerSchedules).
func (a *app) registerPlans() error {
	if !config.Enabled(a.cfg.Plan.Enabled) {
		return nil
	}
	prov := plan.NewMemProvider()
	eng := plan.NewEngine(prov)
	a.plans = eng
	// D3 event sink: plan/* events are appended to the active session log. The
	// callback only ever runs inside a plan_* tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the job/subagent/schedule
	// wiring.
	onEvent := func(typ string, data any) {
		_, appendErr := a.log.Append(typ, data)
		if appendErr != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", appendErr)
		}
		if appendErr == nil && typ == "plan/create" {
			var fact struct {
				Scope string `json:"scope"`
			}
			if raw, err := json.Marshal(data); err == nil && json.Unmarshal(raw, &fact) == nil && fact.Scope == "goal" {
				a.setGoalActivation(a.currentID, true)
			}
		}
	}
	allowParallel := true
	if a.cfg.Plan.AllowParallelInProgress != nil {
		allowParallel = *a.cfg.Plan.AllowParallelInProgress
	}
	pt := plan.NewDSHToolsWithOwner(eng, onEvent, func() string { return a.currentID }, allowParallel)
	for _, t := range []tools.Tool{
		pt.GetGoal(),
		pt.CreateGoal(),
		pt.UpdateGoal(),
		pt.TodoWrite(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	return nil
}

// restorePlans rebuilds the current session's plan projection from its event
// log. The session log is authoritative; the provider is only a disposable
// query projection and is therefore reset on every new/resumed session.
func (a *app) restorePlans() error {
	if a.plans == nil || a.log == nil {
		return nil
	}
	r, ok := a.plans.(plan.EventRestorer)
	if !ok {
		return fmt.Errorf("pa: plan engine cannot restore session events")
	}
	return r.Restore(a.log.Events())
}
