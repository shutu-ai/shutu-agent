package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/schedule"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// makeScheduleApp builds a minimal app for schedule wiring tests: only the
// fields registerSchedules / the schedule pre-step injector touch (cfg.Schedule,
// cfg.Jobs, reg, log, currentID) are set. jobsEnabled gates whether registerJobs
// wires a job registry for the fire path.
func makeScheduleApp(scheduleEnabled, jobsEnabled bool) *app {
	return &app{
		cfg: config.Config{
			Schedule: config.ScheduleConfig{
				Enabled:      config.Bool(scheduleEnabled),
				TickInterval: config.Duration{Duration: time.Minute},
			},
			Jobs: config.JobsConfig{Enabled: config.Bool(jobsEnabled), MaxConcurrentJobsPerOwner: 10},
		},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-sched",
	}
}

func TestRegisterSchedulesProductionUsesDurableDshSurface(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := makeScheduleApp(true, false)
	a.store = st
	a.reg.SetPolicy(schedulePolicy())
	if err := a.registerSchedules(); err != nil {
		t.Fatal(err)
	}
	defer a.goalScheduler.Close()
	if a.schedules != nil || a.goalScheduler == nil {
		t.Fatalf("legacy=%v durable=%v", a.schedules, a.goalScheduler)
	}
	if _, err := a.reg.Execute(context.Background(), "schedule_create", json.RawMessage(`{"prompt":"check","after_seconds":1}`)); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(a.log, session.EventScheduleChange) {
		t.Fatal("schedule/change missing")
	}
}

func TestDurableSchedulesAreSessionScoped(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := makeScheduleApp(true, false)
	a.store = st
	a.reg.SetPolicy(schedulePolicy())
	if err := st.CreateSession(context.Background(), "s-other", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	other := session.New()
	a.runtimeMu.Lock()
	a.runtimeLogs = map[string]*session.Log{"s-other": other}
	a.runtimeMu.Unlock()
	if err := a.registerSchedules(); err != nil {
		t.Fatal(err)
	}
	defer a.closeGoalSchedulers()

	ctxA := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "s-sched"})
	ctxB := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "s-other"})
	if _, err := a.reg.Execute(ctxA, "schedule_create", json.RawMessage(`{"prompt":"A","after_seconds":60}`)); err != nil {
		t.Fatalf("session A create: %v", err)
	}
	if _, err := a.reg.Execute(ctxB, "schedule_create", json.RawMessage(`{"prompt":"B","after_seconds":60}`)); err != nil {
		t.Fatalf("session B create: %v", err)
	}
	listA, err := a.reg.Execute(ctxA, "schedule_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("session A list: %v", err)
	}
	listB, err := a.reg.Execute(ctxB, "schedule_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("session B list: %v", err)
	}
	if !strings.Contains(listA.Output, `"prompt":"A"`) || strings.Contains(listA.Output, `"prompt":"B"`) {
		t.Fatalf("session A schedules leaked: %s", listA.Output)
	}
	if !strings.Contains(listB.Output, `"prompt":"B"`) || strings.Contains(listB.Output, `"prompt":"A"`) {
		t.Fatalf("session B schedules leaked: %s", listB.Output)
	}
}

func TestDurableSchedulerDoesNotReappearAfterShutdownBegins(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "scheduler-close.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := makeScheduleApp(true, false)
	a.store = st
	a.reg.SetPolicy(schedulePolicy())
	if err := a.registerSchedules(); err != nil {
		t.Fatal(err)
	}
	a.runtimeMu.Lock()
	a.runtimeLogs = map[string]*session.Log{"s-after-close": session.New()}
	a.runtimeMu.Unlock()
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "s-after-close"})
	if _, err := a.durableSchedulerFor(ctx); err != nil {
		t.Fatalf("materialize session scheduler: %v", err)
	}
	a.closeGoalSchedulers()
	if _, err := a.durableSchedulerFor(ctx); !errors.Is(err, schedule.ErrDurableClosed) {
		t.Fatalf("scheduler lookup after shutdown = %v, want ErrDurableClosed", err)
	}
	if got := len(a.goalSchedulersSnapshot()); got != 0 {
		t.Fatalf("scheduler snapshot after shutdown = %d, want empty", got)
	}
}

// schedulePolicy whitelists the three schedule tools so the registry Execute
// gate can run them (in production config.applyDefaults + PolicyFromConfig do
// this).
func schedulePolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"schedule_create", "schedule_list", "schedule_delete"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// fireCountFor returns how many schedule/fire events carry id in the log.
func fireCountFor(log *session.Log, id string) int {
	n := 0
	for _, ev := range log.Events() {
		if ev.Type != session.EventScheduleFire {
			continue
		}
		var d struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.ID == id {
			n++
		}
	}
	return n
}

// scheduleEngineWith returns an Engine backed by a fresh in-memory Provider
// preloaded with the given schedules. Preloading through the Provider (bypassing
// Add) lets the tests seed controlled timestamps — a due schedule's NextFire in
// the past, a not-yet-due one in the future — so the fire tests are
// deterministic and never rely on wall-clock granularity (mirrors the engine
// test's craftInterval).
func scheduleEngineWith(t *testing.T, seed ...schedule.Schedule) schedule.Engine {
	t.Helper()
	prov := schedule.NewMemProvider()
	for _, s := range seed {
		if _, err := prov.Create(context.Background(), s); err != nil {
			t.Fatalf("seed schedule: %v", err)
		}
	}
	return schedule.NewEngine(prov)
}

// TestRegisterSchedulesDisabledRegistersNothing verifies the D10 gate: with
// schedule.enabled=false the composition root creates no Engine, registers no
// schedule_* tool, and wires no schedule pre-step injector (dispatch-m6a-2 §5).
func TestRegisterSchedulesDisabledRegistersNothing(t *testing.T) {
	a := makeScheduleApp(false, false)
	if err := a.registerSchedules(); err != nil {
		t.Fatalf("registerSchedules: %v", err)
	}
	if a.schedules != nil {
		t.Fatal("schedule engine must be nil when schedule.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "schedule_") {
			t.Fatalf("schedule tool %q registered while schedule disabled", spec.Name)
		}
	}
	for _, inj := range a.preStepInjectors() {
		if inj.Name == "schedule" {
			t.Fatal("schedule pre-step injector wired while schedule disabled")
		}
	}
}

// TestRegisterSchedulesEnabledRegistersAndValidates verifies the enabled path:
// the Provider + Engine are created, all three schedule_* tools are registered,
// D7 rejects bad arguments at the Execute gate, valid calls flow through
// (create → list → delete), the schedule/* events land in the session log (D3),
// and deleting an unknown id errors.
func TestRegisterSchedulesEnabledRegistersAndValidates(t *testing.T) {
	a := makeScheduleApp(true, false)
	a.reg.SetPolicy(schedulePolicy())
	if err := a.registerSchedules(); err != nil {
		t.Fatalf("registerSchedules: %v", err)
	}
	defer a.schedules.Close()
	if a.schedules == nil {
		t.Fatal("schedule engine must be created when schedule.enabled=true")
	}
	names := make([]string, 0, len(a.reg.Specs()))
	for _, s := range a.reg.Specs() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"schedule_create", "schedule_list", "schedule_delete"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"schedule_create", `{}`},                                         // missing required kind/spec
		{"schedule_create", `{"kind":"interval"}`},                        // missing required spec
		{"schedule_create", `{"kind":"weekly","spec":"30m"}`},             // kind outside the enum
		{"schedule_create", `{"kind":"interval","spec":"30m","extra":1}`}, // additional properties rejected
		{"schedule_create", `{"kind":123,"spec":"30m"}`},                  // kind must be a string
		{"schedule_list", `{"extra":1}`},                                  // list takes no arguments
		{"schedule_delete", `{}`},                                         // missing required id
		{"schedule_delete", `{"id":123}`},                                 // id must be a string
		{"schedule_delete", `{"id":"x","extra":1}`},                       // additional properties rejected
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid create flows through and lands schedule/create (D3).
	res, err := a.reg.Execute(context.Background(), "schedule_create", json.RawMessage(`{"kind":"interval","spec":"30m","payload":"ping"}`))
	if err != nil {
		t.Fatalf("schedule_create via registry: %v", err)
	}
	if !strings.Contains(res.Output, "created schedule sched-1") {
		t.Fatalf("schedule_create output = %q, want created schedule sched-1", res.Output)
	}
	if !hasEvent(a.log, session.EventScheduleCreate) {
		t.Fatal("schedule/create event missing from the session log after schedule_create")
	}
	// schedule_list renders the table and lands schedule/list (D3).
	if _, err := a.reg.Execute(context.Background(), "schedule_list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("schedule_list via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventScheduleList) {
		t.Fatal("schedule/list event missing from the session log after schedule_list")
	}
	// schedule_delete removes it and lands schedule/delete (D3).
	if _, err := a.reg.Execute(context.Background(), "schedule_delete", json.RawMessage(`{"id":"sched-1"}`)); err != nil {
		t.Fatalf("schedule_delete via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventScheduleDelete) {
		t.Fatal("schedule/delete event missing from the session log after schedule_delete")
	}
	// Deleting an unknown id errors.
	if res, err := a.reg.Execute(context.Background(), "schedule_delete", json.RawMessage(`{"id":"sched-99"}`)); err != nil || !res.IsError {
		t.Fatalf("schedule_delete of an unknown id must return a structured error: result=%+v err=%v", res, err)
	}
}

// TestSchedulePreStepFiresEventAndEnqueuesJob verifies the D5 serial trigger
// path (dispatch-m6a-2 §4): the "schedule" pre-step injector advances the clock
// with Engine.Tick, turns every due schedule into a schedule/fire event, and
// enqueues a background job (owner = current session) executing the payload
// when a job registry is wired. A not-yet-due schedule never fires, and no
// background ticker exists — everything happens on the injector's serial call.
func TestSchedulePreStepFiresEventAndEnqueuesJob(t *testing.T) {
	a := makeScheduleApp(true, true)
	if err := a.registerJobs(); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	defer a.jobs.Close()
	now := time.Now()
	a.schedules = scheduleEngineWith(t,
		// Due at the next Tick: NextFire is in the past.
		schedule.Schedule{Kind: schedule.KindInterval, Spec: "30m", Payload: "do the thing", Enabled: true, CreatedAt: now, NextFire: now.Add(-time.Minute)},
		// Not yet due: NextFire is in the future.
		schedule.Schedule{Kind: schedule.KindInterval, Spec: "1h", Payload: "later", Enabled: true, CreatedAt: now, NextFire: now.Add(time.Hour)},
	)
	defer a.schedules.Close()
	due, _ := a.schedules.List(context.Background())
	if len(due) != 2 {
		t.Fatalf("seeded schedules = %d, want 2", len(due))
	}
	dueID, futureID := due[0].ID, due[1].ID

	msgs := a.scheduleInjector().Inject(context.Background(), "hello")
	if msgs != nil {
		t.Fatalf("schedule injector injected context %+v, want none (schedule/fire is log-only)", msgs)
	}
	if got := fireCountFor(a.log, dueID); got != 1 {
		t.Fatalf("schedule/fire count for %s = %d, want exactly 1", dueID, got)
	}
	if got := fireCountFor(a.log, futureID); got != 0 {
		t.Fatalf("schedule/fire count for %s = %d, want 0 (not due)", futureID, got)
	}
	// The fire event carries the payload (bounded) so the executor text is a
	// log fact (D3).
	foundPayload := false
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventScheduleFire {
			continue
		}
		var d struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.ID == dueID && d.Payload == "do the thing" {
			foundPayload = true
		}
	}
	if !foundPayload {
		t.Fatal("schedule/fire event missing the bounded payload")
	}
	// The due schedule's payload was enqueued as a background job owned by the
	// current session (D5: fired job goroutine; the fire event was appended on
	// the serial path before Start).
	snaps, err := a.jobs.List(context.Background(), a.currentID)
	if err != nil {
		t.Fatalf("jobs.List: %v", err)
	}
	var firedJob *jobs.JobSnapshot
	for i := range snaps {
		if snaps[i].Kind == "schedule" && snaps[i].OwnerSession == a.currentID {
			firedJob = &snaps[i]
		}
	}
	if firedJob == nil {
		t.Fatalf("no schedule fire job enqueued for owner %s: %+v", a.currentID, snaps)
	}
	// The fired job settles asynchronously; await it (bounded) so job_output
	// sees the terminal output (the payload) — the same way a model would.
	if _, err := a.jobs.Wait(context.Background(), firedJob.ID, a.currentID, 2*time.Second); err != nil {
		t.Fatalf("jobs.Wait: %v", err)
	}
	out, _, err := a.jobs.Read(context.Background(), firedJob.ID, a.currentID)
	if err != nil {
		t.Fatalf("jobs.Read: %v", err)
	}
	if out != "do the thing" {
		t.Fatalf("schedule fire job output = %q, want the payload", out)
	}
}

// TestSchedulePreStepFiresEventOnlyWithoutJobs verifies the "no job engine ⇒
// only the fire event" branch (dispatch-m6a-2 §4): with jobs disabled the fire
// is logged but no job is enqueued — the injector never panics and touches
// only the serial pre-step path (D5).
func TestSchedulePreStepFiresEventOnlyWithoutJobs(t *testing.T) {
	a := makeScheduleApp(true, false)
	now := time.Now()
	a.schedules = scheduleEngineWith(t,
		schedule.Schedule{Kind: schedule.KindInterval, Spec: "30m", Payload: "payload", Enabled: true, CreatedAt: now, NextFire: now.Add(-time.Minute)},
	)
	defer a.schedules.Close()

	if msgs := a.scheduleInjector().Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("injected %+v, want none", msgs)
	}
	if !hasEvent(a.log, session.EventScheduleFire) {
		t.Fatal("schedule/fire event missing when no job engine is wired")
	}
	// Every schedule/* row in the log is a fire (create/list/delete were not
	// performed here); DeriveHistory stays empty (log-only).
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventScheduleFire {
			t.Fatalf("unexpected event %q in log", ev.Type)
		}
	}
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("schedule/fire events must not derive into messages: %+v", msgs)
	}
}

// TestPreStepInjectorsIncludeScheduleLast verifies the ordering contract
// (dispatch-m6a-2 §4): the "schedule" injector is appended after the skill
// injector, so the turn's order is recall → compaction → skill → schedule.
func TestPreStepInjectorsIncludeScheduleLast(t *testing.T) {
	a := makeScheduleApp(true, false)
	a.reg.SetPolicy(schedulePolicy())
	if err := a.registerSchedules(); err != nil {
		t.Fatalf("registerSchedules: %v", err)
	}
	defer a.schedules.Close()
	inj := a.preStepInjectors()
	if len(inj) == 0 || inj[len(inj)-1].Name != "schedule" {
		t.Fatalf("pre-step injectors = %+v, want the schedule injector last", inj)
	}
}

type blockedScheduleLLM struct{ release chan struct{} }

func (l *blockedScheduleLLM) ID() string      { return "schedule-blocked" }
func (l *blockedScheduleLLM) Available() bool { return true }
func (l *blockedScheduleLLM) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	<-l.release
	return &scriptedReader{events: []llm.StreamEvent{{Kind: llm.StreamFinish, FinishReason: "stop"}}}, nil
}

func newScheduleRecoveryApp(t *testing.T, st *store.SQLiteStore, release chan struct{}) *app {
	t.Helper()
	a := makeScheduleApp(true, false)
	a.store = st
	a.baseCtx = context.Background()
	a.agentRegistry = agent.NewRegistry()
	t.Cleanup(func() { _ = a.agentRegistry.CloseAll() })
	a.prompt = prompt.New("schedule recovery")
	a.llm = &blockedScheduleLLM{release: release}
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	a.basePolicy = tools.Policy{Enabled: []string{"get_time"}}
	a.goalSchedulers = map[string]*schedule.DurableScheduler{}
	if err := st.CreateSession(context.Background(), "schedule-owner", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return a
}

func seedDurableSchedule(t *testing.T, a *app, record schedule.DurableRecord) {
	t.Helper()
	change := schedule.DurableChange{
		Version: schedule.DurableChangeVersion, Operation: "create", Schedule: &record,
	}
	data, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	log, err := a.sessionLogForAgent(context.Background(), "schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventScheduleChange, json.RawMessage(data)); err != nil {
		t.Fatal(err)
	}
	scheduler := schedule.NewDurableScheduler(func(change schedule.DurableChange) error {
		encoded, err := json.Marshal(change)
		if err != nil {
			return err
		}
		_, err = log.Append(session.EventScheduleChange, json.RawMessage(encoded))
		return err
	})
	if err := scheduler.Restore(log.Events()); err != nil {
		t.Fatal(err)
	}
	a.goalSchedulers["schedule-owner"] = scheduler
}

func durableScheduleInboxInsertions(t *testing.T, a *app, occurrence string) int {
	t.Helper()
	log, err := a.sessionLogForAgent(context.Background(), "schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := replaySessionInbox(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	inserted := 0
	for _, event := range events {
		for _, message := range event.Inserted {
			if message.Metadata["dedupe_key"] == occurrence {
				inserted++
			}
		}
	}
	return inserted
}

// TestDurableScheduleReminderReplaysCrashWindows exercises the two production
// crash boundaries in the reminder protocol. A crash after the owner inbox
// receipt must not duplicate it; a crash after the durable fire receipt must
// still reach scheduler dispatch on the next pass.
func TestDurableScheduleReminderReplaysCrashWindows(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "schedule-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	release := make(chan struct{})
	defer close(release)
	a := newScheduleRecoveryApp(t, st, release)
	past := time.Now().UTC().Add(-time.Minute)

	// Window 1: the owner receipt was committed, but process death prevented
	// schedule/fire and scheduler dispatch. Replay must reuse the same dedupe
	// key and deliver exactly one owner insertion.
	first := schedule.DurableRecord{
		ID: "schedule-recover-1", Kind: schedule.DurableAfter, Prompt: "first",
		ScheduledAt: past,
	}
	seedDurableSchedule(t, a, first)
	dueFirst := schedule.DurableDue{Kind: schedule.DurableAfter, Records: []schedule.DurableRecord{first}, AcceptedAt: time.Now().UTC()}
	handle, err := a.sessionAgent("schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Followup(scheduleReminderPrompt(dueFirst), map[string]string{
		"source":                 "schedule",
		"schedule_occurrence_id": scheduleOccurrenceID(dueFirst),
		"dedupe_key":             "schedule:" + scheduleOccurrenceID(dueFirst),
	}); err != nil {
		t.Fatal(err)
	}
	if got := durableScheduleInboxInsertions(t, a, "schedule:"+scheduleOccurrenceID(dueFirst)); got != 1 {
		t.Fatalf("pre-crash first insertions = %d, want 1", got)
	}
	a.runScheduledReminder(ctx)
	log, err := a.sessionLogForAgent(context.Background(), "schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := fireCountFor(log, "schedule-recover-1"); got != 1 {
		t.Fatalf("first schedule/fire count = %d, want 1", got)
	}
	if got := durableScheduleInboxInsertions(t, a, "schedule:"+scheduleOccurrenceID(dueFirst)); got != 1 {
		t.Fatalf("first replay insertions = %d, want still 1", got)
	}

	// Window 2: schedule/fire was durable, but process death prevented owner
	// delivery and scheduler dispatch. Replay must deliver the owner receipt,
	// skip duplicate fire append, and dispatch the one-shot occurrence.
	second := schedule.DurableRecord{
		ID: "schedule-recover-2", Kind: schedule.DurableAfter, Prompt: "second",
		ScheduledAt: past.Add(time.Second),
	}
	seedDurableSchedule(t, a, second)
	dueSecond := schedule.DurableDue{Kind: schedule.DurableAfter, Records: []schedule.DurableRecord{second}, AcceptedAt: time.Now().UTC()}
	log, err = a.sessionLogForAgent(context.Background(), "schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	if !a.appendDurableScheduleFire(log, dueSecond) {
		t.Fatal("failed to seed the post-fire crash window")
	}
	a.runScheduledReminder(ctx)
	log, err = a.sessionLogForAgent(context.Background(), "schedule-owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := fireCountFor(log, "schedule-recover-2"); got != 1 {
		t.Fatalf("second schedule/fire count = %d, want 1", got)
	}
	if got := durableScheduleInboxInsertions(t, a, "schedule:"+scheduleOccurrenceID(dueSecond)); got != 1 {
		t.Fatalf("second replay insertions = %d, want 1", got)
	}
}
