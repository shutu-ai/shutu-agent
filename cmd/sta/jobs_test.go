package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/observability"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// makeJobsApp builds a minimal app for registerJobs tests: only the fields
// registerJobs touches (cfg.Jobs, reg, log, currentID) are set.
func makeJobsApp(enabled bool) *app {
	return &app{
		cfg:       config.Config{Jobs: config.JobsConfig{Enabled: config.Bool(enabled), MaxConcurrentJobsPerOwner: 10}},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-test",
	}
}

func TestAppBackgroundJobSpanRetainsInitiatingCorrelation(t *testing.T) {
	a := &app{tracer: observability.NewTracer(8), jobTraceSpans: make(map[string]*observability.Span)}
	settled := make(chan struct{})
	l := jobs.NewLocal(jobs.LocalOpts{
		OnStarted: a.onJobStarted,
		OnSettled: func(snap jobs.JobSnapshot, _ string) {
			a.finishJobSpan(snap)
			close(settled)
		},
	})
	defer l.Close()
	want := runtimectx.Correlation{AgentID: "agent-job", SessionID: "session-job", TurnID: "turn:1", StepID: "step:1", RequestID: "req-job", CallID: "call-job", GenerationID: "generation-job"}
	ctx := runtimectx.WithCorrelation(context.Background(), want)
	id, err := l.Start(ctx, jobs.JobStart{Kind: "bash", Label: "background", OwnerSession: want.SessionID,
		Run: func(context.Context) (jobs.JobOutcome, error) {
			return jobs.JobOutcome{Status: jobs.StatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("job settlement telemetry timed out")
	}
	spans := a.tracer.Spans()
	if len(spans) != 1 || spans[0].Name != "job.bash" || spans[0].Correlation != want || spans[0].EndedAt.IsZero() {
		t.Fatalf("job spans = %+v, want one completed correlated span for %s", spans, id)
	}
}

// jobsPolicy whitelists the canonical dsh job tools so registry Execute can run them
// (in production config.applyDefaults + PolicyFromConfig do this).
func jobsPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"job_output", "job_kill", "job_list"},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestRegisterJobsDisabledRegistersNothing verifies the D10 gate: with
// jobs.enabled=false the composition root creates no registry and registers no
// job_* tool (dispatch-m5a-2 §4).
func TestRegisterJobsDisabledRegistersNothing(t *testing.T) {
	app := makeJobsApp(false)
	if err := app.registerJobs(); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	if app.jobs != nil {
		t.Fatal("jobs registry must be nil when jobs.enabled=false")
	}
	for _, spec := range app.reg.Specs() {
		if strings.HasPrefix(spec.Name, "job_") {
			t.Fatalf("job tool %q registered while jobs disabled", spec.Name)
		}
	}
}

// TestRegisterJobsEnabledRegistersAndValidates verifies the enabled path: the
// registry is created, all canonical job_* tools are registered, D7 schema
// validation rejects bad arguments at the Execute gate, valid calls flow
// through, and the job/start event lands in the session log (D3 wiring).
func TestRegisterJobsEnabledRegistersAndValidates(t *testing.T) {
	app := makeJobsApp(true)
	app.reg.SetPolicy(jobsPolicy())
	if err := app.registerJobs(); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	defer app.jobs.Close()
	if app.jobs == nil {
		t.Fatal("jobs registry must be created when jobs.enabled=true")
	}
	specs := app.reg.Specs()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	for _, want := range []string{"job_output", "job_kill", "job_list"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}
	for _, removed := range []string{"job_start", "job_status", "job_cancel", "job_wait", "job_read"} {
		if containsStr(names, removed) {
			t.Fatalf("removed legacy tool %q is still advertised", removed)
		}
		if _, err := app.reg.Execute(context.Background(), removed, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("removed legacy tool %q is still executable", removed)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"job_output", `{}`},                            // missing required job_id
		{"job_output", `{"job_id":123}`},                // job_id must be a string
		{"job_kill", `{}`},                              // missing required job_id
		{"job_output", `{"job_id":"x","timeout_ms":0}`}, // timeout must be >= 1
		{"job_list", `{"extra":1}`},                     // additional properties rejected
	} {
		if _, err := app.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// Producers start jobs through the internal registry; the model observes
	// them only through DSH's job_output/job_list/job_kill surface.
	jobID, err := app.jobs.Start(context.Background(), jobs.JobStart{
		Kind: jobs.Kind("bash"), Label: "d7-ok", OwnerSession: "s-test",
		Run: func(context.Context) (jobs.JobOutcome, error) {
			return jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "d7-ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("internal job start: %v", err)
	}
	if _, err := app.reg.Execute(context.Background(), "job_output", json.RawMessage(`{"job_id":"`+jobID+`","wait":true}`)); err != nil {
		t.Fatalf("job_output via registry: %v", err)
	}
}

func TestEnsureJobDoneIsIdempotentAcrossConcurrentObservers(t *testing.T) {
	app := makeJobsApp(true)
	snap := jobs.JobSnapshot{ID: "bash-1", Kind: jobs.Kind("bash"), Status: jobs.StatusCompleted, Detail: "exit 0"}
	const observers = 32
	var wg sync.WaitGroup
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.ensureJobDone(app.log, snap, "output"); err != nil {
				t.Errorf("ensureJobDone: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := countEvent(app.log, session.EventJobDone); got != 1 {
		t.Fatalf("job/done events = %d, want exactly one", got)
	}
}

func TestJobSettlementPersistsBeforeShutdownSkipsLiveWake(t *testing.T) {
	app := makeJobsApp(true)
	app.agentRegistry = agent.NewRegistry()
	app.runtimeLogs = map[string]*session.Log{"owner": app.log}
	app.currentID = "other"
	app.beginShutdown()
	snap := jobs.JobSnapshot{ID: "bash-shutdown-1", Kind: jobs.Kind("bash"), OwnerSession: "owner", Status: jobs.StatusCompleted}
	app.onJobSettled(snap, "done")
	if got := countEvent(app.log, session.EventJobDone); got != 1 {
		t.Fatalf("shutdown settlement job/done count = %d, want one", got)
	}
	if err := app.agentRegistry.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
}

// TestJobCompletionMaterializesAcrossSQLiteApps closes the persisted-owner
// boundary left by the in-memory receipt tests. The first app commits only the
// durable job/done fact. A second app reconstructs the owner from SQLite and
// materializes its quiet inbox receipt through the live durable sink. A third
// store reader then proves exactly one job receipt and exactly one owner wake
// were persisted.
func TestJobCompletionMaterializesAcrossSQLiteApps(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "job-owner-cold.db")

	firstStore, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.CreateSession(ctx, "cold-owner", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first := &app{store: firstStore, baseCtx: ctx}
	firstLog, err := first.sessionLogForAgent(ctx, "cold-owner")
	if err != nil {
		t.Fatal(err)
	}
	snap := jobs.JobSnapshot{
		ID: "bash-cold", Kind: jobs.Kind("bash"), OwnerSession: "cold-owner",
		Status: jobs.StatusCompleted, Detail: "exit 0",
	}
	if err := first.ensureJobDone(firstLog, snap, "cold output"); err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	secondStore, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	second := &app{
		store:   secondStore,
		baseCtx: ctx,
		cfg: config.Config{Jobs: config.JobsConfig{
			CompletionDelivery:        "quiet",
			MaxConcurrentJobsPerOwner: 2,
		}},
		agentRegistry: agent.NewRegistry(),
	}
	handle, err := second.sessionAgent("cold-owner")
	if err != nil {
		t.Fatal(err)
	}
	pending := handle.Agent().Inbox().PendingMessages()
	if len(pending) != 1 || pending[0].Kind != agent.MessageInjection ||
		pending[0].Metadata["dedupe_key"] != "job:bash-cold" {
		t.Fatalf("cold owner inbox = %+v, want one quiet job:bash-cold injection", pending)
	}
	if err := handle.WhenIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.agentRegistry.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := secondStore.Close(); err != nil {
		t.Fatal(err)
	}

	thirdStore, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = thirdStore.Close() }()
	events, err := thirdStore.LoadSession(ctx, "cold-owner")
	if err != nil {
		t.Fatal(err)
	}
	jobDone, inboxSpliced := 0, 0
	for _, event := range events {
		switch event.Type {
		case session.EventJobDone:
			jobDone++
		case session.EventAgentInboxSpliced:
			inboxSpliced++
		}
	}
	if jobDone != 1 || inboxSpliced != 1 {
		t.Fatalf("durable counts job/done=%d inbox/spliced=%d, want 1/1", jobDone, inboxSpliced)
	}
	inboxEvents, err := replaySessionInbox(events)
	if err != nil {
		t.Fatal(err)
	}
	inserted := 0
	for _, event := range inboxEvents {
		inserted += len(event.Inserted)
	}
	if inserted != 1 {
		t.Fatalf("durable inserted owner receipts = %d, want 1", inserted)
	}
}
