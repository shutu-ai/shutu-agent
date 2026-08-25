// jobs.go — the M5a-2 composition-root orchestration (dispatch-m5a-2 §4). This
// is where the jobs capability seam is wired into the REPL: registerJobs
// creates the in-memory Local registry and registers the dsh job
// tools when
// jobs.enabled (D10), and wires the D3 event sink so job/start, job/status and
// job/done are appended to the active session log. The loop's turn/step
// structure is untouched (D4): background job goroutines run independently and
// never enter a turn/step (D5) — the tools observe them through the serial
// tool path, and the deferred Close cancels and awaits every live job at
// shutdown so no goroutine leaks (lifecycle reversible, ADR 决策 ①).
package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/tools"
)

// registerJobs creates the Local registry and registers the dsh job tools when
// jobs.enabled, and wires the D3 event sink. When jobs is disabled it
// creates nothing and registers nothing (D10, mirrors registerKB).
func (a *app) registerJobs() error {
	if !config.Enabled(a.cfg.Jobs.Enabled) {
		return nil
	}
	a.jobs = jobs.NewLocal(jobs.LocalOpts{MaxConcurrentJobsPerOwner: a.cfg.Jobs.MaxConcurrentJobsPerOwner})
	// D3 event sink: job/* events are appended to the active session log. The
	// callback only ever runs inside a job_* tool Execute — the serial
	// main-loop path — so the session log is never touched from a background
	// job goroutine (D5; the dispatch-m5a-2 §4 tool-layer decision). a.log is
	// read at call time, so a session switch (/new, /resume) is honored the
	// same way as the kb onAdded wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	jt := jobs.NewJobTools(a.jobs, func() string { return a.currentID }, onEvent)
	for _, t := range []tools.Tool{
		jt.Start(),
		jt.DshOutput(),
		jt.DshKill(),
		jt.DshList(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	return nil
}
