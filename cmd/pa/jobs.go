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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/observability"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// registerJobs creates the Local registry and registers the dsh job tools when
// jobs.enabled, and wires the D3 event sink. When jobs is disabled it
// creates nothing and registers nothing.
func (a *app) registerJobs() error {
	if !config.Enabled(a.cfg.Jobs.Enabled) {
		return nil
	}
	jobOptions := jobs.LocalOpts{
		MaxConcurrentJobsPerOwner: a.cfg.Jobs.MaxConcurrentJobsPerOwner,
		OnStarted:                 a.onJobStarted,
		OnSettled:                 a.onJobSettled,
	}
	if reservations, ok := a.store.(store.IDReservationStore); ok {
		jobOptions.ReserveID = func(ctx context.Context, namespace, id string) (bool, error) {
			return reservations.ReserveID(ctx, namespace, id)
		}
	}
	a.jobs = jobs.NewLocal(jobOptions)
	// D3 event sink: job/* events are appended to the active session log. The
	// callback only ever runs inside a job_* tool Execute — the serial
	// main-loop path — so the session log is never touched from a background
	// job goroutine (D5; the dispatch-m5a-2 §4 tool-layer decision). a.log is
	// read at call time, so a session switch (/new, /resume) is honored the
	// same way as the other session-bound event wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.appendRuntimeEvent(a.log, typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", err)
		}
	}
	jt := jobs.NewJobToolsWithContext(a.jobs, func(ctx context.Context) string {
		return a.runtimeSessionID(ctx)
	}, func(ctx context.Context) string {
		return a.sessionCWDFor(a.runtimeSessionID(ctx))
	}, onEvent)
	for _, t := range []tools.Tool{
		jt.DshOutput(),
		jt.DshKill(),
		jt.DshList(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("sta: register %s: %w", t.Name(), err)
		}
	}
	return nil
}

// onJobStarted opens the process-local span for an asynchronous job before
// its producer goroutine is launched. The runtime correlation was captured at
// jobs.Registry.Start because the initiating tool context may be gone by the
// time the job settles.
func (a *app) onJobStarted(snap jobs.JobSnapshot) {
	if a == nil || a.tracer == nil || strings.TrimSpace(snap.ID) == "" {
		return
	}
	correlation := snap.Correlation
	if correlation.SessionID == "" {
		correlation.SessionID = snap.OwnerSession
	}
	if correlation.AgentID == "" {
		correlation.AgentID = correlation.SessionID
	}
	span := a.tracer.Start(correlation, "job."+string(snap.Kind), "")
	if span == nil {
		return
	}
	a.jobTraceMu.Lock()
	if a.jobTraceSpans == nil {
		a.jobTraceSpans = make(map[string]*observability.Span)
	}
	a.jobTraceSpans[snap.ID] = span
	a.jobTraceMu.Unlock()
}

func (a *app) finishJobSpan(snap jobs.JobSnapshot) {
	if a == nil || a.tracer == nil {
		return
	}
	a.jobTraceMu.Lock()
	span := a.jobTraceSpans[snap.ID]
	delete(a.jobTraceSpans, snap.ID)
	a.jobTraceMu.Unlock()
	if span == nil {
		return
	}
	var err error
	switch snap.Status {
	case jobs.StatusFailed:
		err = errors.New(snap.Detail)
	case jobs.StatusKilled:
		err = context.Canceled
	}
	a.tracer.End(span, err)
}

// onJobSettled delivers a producer-independent completion notice to the
// owning Agent. This is the durable wake-up path for jobs started by
// schedules, subagents and terminal tools: no model-facing job_output call is
// required before completion is visible to the owner session.
func (a *app) onJobSettled(snap jobs.JobSnapshot, output string) {
	a.finishJobSpan(snap)
	owner := strings.TrimSpace(snap.OwnerSession)
	if owner == "" || a == nil {
		return
	}
	if strings.TrimSpace(output) == "" {
		output = "(empty)"
	}
	log, err := a.sessionLogForAgent(context.Background(), owner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta: job completion log:", err)
		return
	}
	// A completion observer runs even when no model-facing job_wait/status call
	// ever observes the terminal transition. Persist the terminal fact here so
	// cold replay and the wake-up message have the same source of truth. The
	// scan makes this compatible with the synchronous job-tool path, which may
	// already have emitted the same job/done row.
	if err := a.ensureJobDone(log, snap, output); err != nil {
		fmt.Fprintln(os.Stderr, "sta: job completion event:", err)
		return
	}
	// The durable terminal fact is committed even when shutdown admission has
	// already closed the Agent registry. A completion observer must not lose
	// the replay source merely because its best-effort live wake can no longer
	// create or access an Agent.
	if a.agentRegistry == nil || a.shutdownStarted() {
		return
	}
	handle, err := a.sessionAgent(owner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta: job completion agent:", err)
		return
	}
	prompt := fmt.Sprintf("[JOB COMPLETION]\njob_id: %s\nkind: %s\nstatus: %s\ndetail: %s\noutput:\n%s",
		snap.ID, snap.Kind, snap.Status, snap.Detail, output)
	metadata := map[string]string{
		"source":     "job",
		"job_id":     snap.ID,
		"dedupe_key": "job:" + snap.ID,
	}
	// Match the harness delivery policy: a busy owner receives a next-step
	// injection; an idle owner is woken only within a bounded budget, after
	// which completions remain quiet until another user-authored turn arrives.
	deliverErr := a.deliverJobCompletionWake(handle, owner, prompt, metadata)
	if deliverErr != nil && !errors.Is(deliverErr, agent.ErrAgentClosed) {
		fmt.Fprintln(os.Stderr, "sta: job completion delivery:", deliverErr)
		// job/done is already durable. Retry the missing inbox receipt from
		// that event so a transient journal/agent failure does not lose the
		// completion notification until the next process restart.
		if recoveryErr := a.recoverJobCompletionWakes(log, handle); recoveryErr != nil {
			fmt.Fprintln(os.Stderr, "sta: job completion recovery:", recoveryErr)
		}
	}
}

// ensureJobDone commits one terminal job projection idempotently. The lock is
// intentionally held across the scan and append: the synchronous tool path
// and the provider completion observer can reach this boundary concurrently.
func (a *app) ensureJobDone(log *session.Log, snap jobs.JobSnapshot, output string) error {
	if a == nil || log == nil {
		return errors.New("job completion log is unavailable")
	}
	a.jobEventMu.Lock()
	defer a.jobEventMu.Unlock()
	if jobDoneRecorded(log, snap.ID) {
		return nil
	}
	_, err := log.Append(session.EventJobDone, session.NewJobDone(snap.ID, string(snap.Status), snap.Detail, output))
	return err
}

// appendRuntimeEvent is the single application sink for job/* events. Other
// runtime events retain the session.Log's own ordering, while job events also
// need a process-wide check-and-append boundary shared with onJobSettled.
func (a *app) appendRuntimeEvent(log *session.Log, typ string, data any) (session.Event, error) {
	if log == nil {
		return session.Event{}, errors.New("runtime event sink is unavailable")
	}
	if !strings.HasPrefix(typ, "job/") {
		return log.Append(typ, data)
	}
	a.jobEventMu.Lock()
	defer a.jobEventMu.Unlock()
	return log.Append(typ, data)
}

func jobDoneRecorded(log *session.Log, jobID string) bool {
	if log == nil || strings.TrimSpace(jobID) == "" {
		return false
	}
	for _, event := range log.Events() {
		if event.Type != session.EventJobDone {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.ID == jobID {
			return true
		}
	}
	return false
}

func (a *app) resetJobWakeBudget(sessionID string) {
	if a == nil {
		return
	}
	a.jobWakeMu.Lock()
	if a.jobWakeCounts != nil {
		delete(a.jobWakeCounts, sessionID)
	}
	a.jobWakeMu.Unlock()
}

// deliverJobCompletionWake owns the reference delivery policy. Both the live
// settlement observer and cold receipt recovery must use this one boundary:
// otherwise a memoized Agent's recovery path can reopen a turn even after the
// configured consecutive-wake budget has been spent.
func (a *app) deliverJobCompletionWake(handle *agent.Handle, owner, prompt string, metadata map[string]string) error {
	cfg := a.providerConfigSnapshot()
	delivery := cfg.Jobs.CompletionDelivery
	if delivery == "" {
		delivery = config.DefaultJobCompletionDelivery
	}
	if delivery == "wakeup" && handle.Status() == agent.StatusIdle {
		a.jobWakeMu.Lock()
		if a.jobWakeCounts == nil {
			a.jobWakeCounts = make(map[string]int)
		}
		limit := cfg.Jobs.MaxConsecutiveWakes
		if limit <= 0 {
			limit = config.DefaultJobMaxConsecutiveWakes
		}
		if a.jobWakeCounts[owner] < limit {
			a.jobWakeCounts[owner]++
			a.jobWakeMu.Unlock()
			return handle.Followup(prompt, metadata)
		}
		a.jobWakeMu.Unlock()
	}
	return handle.Inject(prompt, metadata)
}
