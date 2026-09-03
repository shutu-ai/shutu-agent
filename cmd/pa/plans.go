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
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/plan"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/tools"
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
	a.planMu.Lock()
	if a.planEngines == nil {
		a.planEngines = make(map[string]plan.Engine)
	}
	a.planMu.Unlock()
	// D3 event sink: plan/* events are appended to the active session log. The
	// callback only ever runs inside a plan_* tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the job/subagent/schedule
	// wiring.
	onEvent := func(typ string, data any) {
		_, appendErr := a.log.Append(typ, data)
		if appendErr != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", appendErr)
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
	blockedAfterRounds := 3
	if a.cfg.Plan.BlockedAfterConsecutiveRounds != nil {
		blockedAfterRounds = *a.cfg.Plan.BlockedAfterConsecutiveRounds
	}
	pt := plan.NewDSHToolsWithResolverContext(a.planEngineFor, onEvent, func(ctx context.Context) string {
		return a.runtimeSessionID(ctx)
	}, func() bool {
		return a.goalIsArmed(a.currentID)
	}, func(armed bool) { a.setGoalActivation(a.currentID, armed) }, allowParallel, blockedAfterRounds)
	pt.SetContextActivation(func(ctx context.Context) bool {
		return a.goalIsArmed(a.runtimeSessionID(ctx))
	}, func(ctx context.Context, armed bool) {
		a.setGoalActivation(a.runtimeSessionID(ctx), armed)
	})
	for _, t := range []tools.Tool{
		pt.GetGoal(),
		pt.CreateGoal(),
		pt.UpdateGoal(),
		pt.TodoWrite(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("sta: register %s: %w", t.Name(), err)
		}
	}
	return nil
}

// planEngineFor returns the disposable plan projection owned by the addressed
// runtime session. The legacy engine remains the fallback for direct CLI and
// unit-test calls without a runtime context.
func (a *app) planEngineFor(ctx context.Context) (plan.Engine, error) {
	if a.plans == nil {
		return nil, fmt.Errorf("planning is disabled")
	}
	sessionID := runtimectx.SessionID(ctx)
	if sessionID == "" || (a.agentRegistry == nil && sessionID == a.currentID) {
		return a.plans, nil
	}
	a.planMu.Lock()
	if existing := a.planEngines[sessionID]; existing != nil {
		a.planMu.Unlock()
		return existing, nil
	}
	a.planMu.Unlock()
	log := a.runtimeLog(ctx)
	if log == nil {
		return nil, fmt.Errorf("plan session %q runtime is unavailable", sessionID)
	}
	engine := plan.NewEngine(plan.NewMemProvider())
	if restorer, ok := any(engine).(plan.EventRestorer); ok {
		if err := restorer.Restore(log.Events()); err != nil {
			_ = engine.Close()
			return nil, fmt.Errorf("restore plan session %q: %w", sessionID, err)
		}
	}
	a.planMu.Lock()
	if existing := a.planEngines[sessionID]; existing != nil {
		a.planMu.Unlock()
		_ = engine.Close()
		return existing, nil
	}
	if a.planEngines == nil {
		a.planEngines = make(map[string]plan.Engine)
	}
	a.planEngines[sessionID] = engine
	a.planMu.Unlock()
	return engine, nil
}

func (a *app) closePlanEngines() {
	a.planMu.Lock()
	engines := make([]plan.Engine, 0, len(a.planEngines)+1)
	seen := make(map[plan.Engine]struct{}, len(a.planEngines)+1)
	for _, engine := range a.planEngines {
		if engine != nil {
			engines = append(engines, engine)
			seen[engine] = struct{}{}
		}
	}
	if a.plans != nil {
		if _, ok := seen[a.plans]; !ok {
			engines = append(engines, a.plans)
		}
	}
	a.planEngines = nil
	a.planMu.Unlock()
	for _, engine := range engines {
		_ = engine.Close()
	}
}

// restorePlans rebuilds the current session's plan projection from its event
// log. The session log is authoritative; the provider is only a disposable
// query projection and is therefore reset on every new/resumed session.
func (a *app) restorePlans() error {
	if a.plans == nil || a.log == nil {
		return nil
	}
	var stale plan.Engine
	a.planMu.Lock()
	if a.planEngines != nil {
		stale = a.planEngines[a.currentID]
		delete(a.planEngines, a.currentID)
	}
	a.planMu.Unlock()
	if stale != nil {
		_ = stale.Close()
	}
	r, ok := a.plans.(plan.EventRestorer)
	if !ok {
		return fmt.Errorf("sta: plan engine cannot restore session events")
	}
	return r.Restore(a.log.Events())
}
