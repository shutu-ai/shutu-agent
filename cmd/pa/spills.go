// spills.go — the M6c-2 composition-root orchestration (dispatch-m6c-2
// §4/§5). This is where the long-term-memory capability seam is wired into the
// REPL: registerSpills creates the in-memory Provider + Engine and registers
// the four spill_* tools when spill.enabled (D10), wires the D3 event sink so
// spill/write, spill/recall, spill/list and spill/delete are appended to the
// active session log, and — when spill.auto_spill is on — the spillAutoSpill
// turn-completion hook folds the completed turn's event log into new memories
// on the serial path. The loop's turn/step structure is untouched (D4): the
// auto-sedimentation runs after a completed turn in the REPL's serial flow
// (next to the M4c extraction writeback), never inside a step and never from a
// background goroutine (D5). AutoSpill itself is the pure policy kernel
// (M6c-1); this wiring guarantees it is only ever invoked once per completed
// turn on the serial path, and its content-hash idempotence means re-running
// over the same log never duplicates.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/spill"
	"github.com/jabing/shutu-agent/internal/tools"
)

// registerSpills creates the configured durable Provider + Engine and registers
// the four spill_* tools when spill.enabled, and wires the D3 event sink. Bare
// test composition without DataDir intentionally falls back to memory. When
// spill is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerSchedules/registerPlans).
func (a *app) registerSpills() error {
	if !config.Enabled(a.cfg.Spill.Enabled) {
		return nil
	}
	var prov spill.Provider
	if a.cfg.DataDir == "" {
		// Bare composition-root tests intentionally omit runtime paths. Keep
		// those isolated in memory; loaded production configs always carry a
		// data directory and therefore use the durable backend below.
		prov = spill.NewMemProvider()
	} else {
		var err error
		prov, err = spill.NewFileProvider(filepath.Join(a.cfg.DataDir, "memory.json"))
		if err != nil {
			return fmt.Errorf("pa: open memory store: %w", err)
		}
	}
	eng := spill.NewEngine(prov)
	a.spills = eng
	// D3 event sink: spill/* events are appended to the active session log.
	// The callback only ever runs inside a spill_* tool Execute or the
	// spillAutoSpill turn-completion hook — the serial main-loop path (D5).
	// a.log is read at call time, so a session switch (/new, /resume) is
	// honored the same way as the job/subagent/schedule/plan wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	st := spill.NewSpillTools(eng, onEvent)
	for _, t := range []tools.Tool{
		st.Write(),
		st.Recall(),
		st.List(),
		st.Delete(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	return nil
}

// spillAutoSpill is the turn-completion auto-sedimentation hook
// (dispatch-m6c-2 §4): when spill.enabled && spill.auto_spill, it folds the
// current log into new memories through the engine's pure AutoSpill policy and
// logs one spill/write fact (D3) per newly stored memo. It runs once per
// completed turn, on the REPL's serial path (after a successful Loop.Run — the
// same turn-completion slot as the M4c extraction writeback, D4), never from a
// background goroutine (D5). The before/after memo diff keeps the spill/write
// events exact (only genuinely new memos are logged), and the engine's
// content-hash idempotence plus the once-per-turn call guarantee the path
// never duplicates — re-running over the same log adds nothing (不重复沉淀).
// Every failure is surfaced as a stderr warning and contributes nothing
// (fail-open, the same contract as the kb extraction / schedule tick hooks).
func (a *app) spillAutoSpill(ctx context.Context) {
	if a.spills == nil || !a.cfg.Spill.AutoSpillValue() {
		return
	}
	before, err := a.spills.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[spill auto-spill failed open]", err)
		return
	}
	beforeIDs := make(map[string]bool, len(before))
	for _, m := range before {
		beforeIDs[m.ID] = true
	}
	added, err := a.spills.AutoSpill(ctx, a.log.Events())
	if err != nil {
		fmt.Fprintln(os.Stderr, "[spill auto-spill failed open]", err)
		return
	}
	if added == 0 {
		return
	}
	after, err := a.spills.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[spill auto-spill failed open]", err)
		return
	}
	for _, m := range after {
		if beforeIDs[m.ID] {
			continue
		}
		if _, err := a.log.Append(session.EventSpillWrite, session.NewSpillWrite(m.ID, m.Content)); err != nil {
			fmt.Fprintln(os.Stderr, "pa: spill/write event:", err)
		}
	}
}
