package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

func TestRecoverSubagentCompletionWakeAfterDurableEnd(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventSubagentEnd, session.NewSubagentEnd("child-1", "spawn", "completed", "answer")); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(agent.Options{
		ID:           "parent-1",
		InboxJournal: sessionInboxJournal{log: log},
		Runner:       func(context.Context, *agent.Agent, agent.TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := (&app{}).recoverSubagentCompletionWakes(log, handle); err != nil {
		t.Fatalf("recover completion wake: %v", err)
	}
	if err := (&app{}).recoverSubagentCompletionWakes(log, handle); err != nil {
		t.Fatalf("repeat completion recovery: %v", err)
	}
	events := log.Events()
	if len(events) != 2 || events[1].Type != session.EventAgentInboxSpliced {
		t.Fatalf("recovery events = %+v, want one durable inbox splice", events)
	}
	if got := len(handle.Agent().Inbox().PendingMessages()); got != 1 {
		t.Fatalf("pending completion wakes = %d, want 1", got)
	}
}

func TestRecoverSubagentCompletionWakeDoesNotDuplicateClaimedWake(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventSubagentEnd, session.NewSubagentEnd("child-1", "spawn", "completed", "answer")); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(agent.Options{
		ID:           "parent-1",
		InboxJournal: sessionInboxJournal{log: log},
		Runner:       func(context.Context, *agent.Agent, agent.TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&app{}).recoverSubagentCompletionWakes(log, handle); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := handle.Agent().Inbox().ClaimWithError(1); err != nil || !ok {
		t.Fatalf("claim recovered wake ok=%v err=%v", ok, err)
	}
	if err := (&app{}).recoverSubagentCompletionWakes(log, handle); err != nil {
		t.Fatalf("recovery after claim: %v", err)
	}
	events := log.Events()
	if len(events) != 3 || events[2].Type != session.EventAgentInboxSpliced {
		t.Fatalf("claimed recovery events = %+v, want only claim splice", events)
	}
}

func TestRecoverJobCompletionWakeAfterDurableDone(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventJobDone, session.NewJobDone("job-1", "completed", "exit 0", "output")); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(agent.Options{
		ID:           "owner-1",
		InboxJournal: sessionInboxJournal{log: log},
		Runner:       func(context.Context, *agent.Agent, agent.TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&app{}).recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("recover job wake: %v", err)
	}
	if err := (&app{}).recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("repeat job recovery: %v", err)
	}
	events := log.Events()
	if len(events) != 2 || events[1].Type != session.EventAgentInboxSpliced {
		t.Fatalf("job recovery events = %+v, want one durable inbox splice", events)
	}
	pending := handle.Agent().Inbox().PendingMessages()
	if len(pending) != 1 || pending[0].Metadata["dedupe_key"] != "job:job-1" {
		t.Fatalf("pending job wake = %+v", pending)
	}
}

type flakyInboxJournal struct {
	mu       sync.Mutex
	failures int
	events   []agent.InboxEvent
}

func (j *flakyInboxJournal) AppendInboxEvent(event agent.InboxEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(event.Inserted) > 0 && j.failures > 0 {
		j.failures--
		return errors.New("injected inbox journal failure")
	}
	j.events = append(j.events, event)
	return nil
}

func TestRecoverJobCompletionWakeRetriesFailedInboxReceipt(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventJobDone, session.NewJobDone("job-failure", "completed", "exit 0", "output")); err != nil {
		t.Fatal(err)
	}
	journal := &flakyInboxJournal{failures: 1}
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(agent.Options{
		ID:           "owner-failure",
		InboxJournal: journal,
		Runner:       func(context.Context, *agent.Agent, agent.TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := (&app{}).recoverJobCompletionWakes(log, handle); err == nil {
		t.Fatal("first recovery incorrectly hid the inbox journal failure")
	}
	if got := len(log.Events()); got != 1 {
		t.Fatalf("failed recovery events = %d, want only durable job/done", got)
	}
	if got := len(journal.events); got != 0 {
		t.Fatalf("failed recovery journal = %+v, want no committed inbox splice", journal.events)
	}

	journal.failures = 0
	if err := (&app{}).recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	if err := (&app{}).recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("repeat recovery after retry: %v", err)
	}
	if got := len(journal.events); got != 1 || len(journal.events[0].Inserted) != 1 {
		t.Fatalf("retry journal = %+v, want exactly one inserted receipt", journal.events)
	}
	if got := len(handle.Agent().Inbox().PendingMessages()); got != 1 {
		t.Fatalf("retry pending messages = %d, want 1", got)
	}
}

func TestJobCompletionRecoverySpendsAndRestoresWakeBudget(t *testing.T) {
	log := session.New()
	a := &app{cfg: config.Config{
		Jobs: config.JobsConfig{CompletionDelivery: "wakeup", MaxConsecutiveWakes: 1},
	}}
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(agent.Options{
		ID:           "budget-owner",
		InboxJournal: sessionInboxJournal{log: log},
		Runner:       func(context.Context, *agent.Agent, agent.TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	appendDone := func(id string) {
		t.Helper()
		if _, err := log.Append(session.EventJobDone, session.NewJobDone(id, "completed", "exit 0", "output")); err != nil {
			t.Fatal(err)
		}
	}

	appendDone("job-wake")
	if err := a.recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), time.Second)
	defer cancelIdle()
	if err := handle.WhenIdle(idleCtx); err != nil {
		t.Fatalf("first wake did not settle: %v", err)
	}

	appendDone("job-inject")
	if err := a.recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("budget-spent recovery: %v", err)
	}
	pending := handle.Agent().Inbox().PendingMessages()
	if len(pending) != 1 || pending[0].Kind != agent.MessageInjection || pending[0].Metadata["job_id"] != "job-inject" {
		t.Fatalf("budget-spent pending = %+v, want quiet job-inject injection", pending)
	}
	if got := a.jobWakeCounts["budget-owner"]; got != 1 {
		t.Fatalf("job wake count = %d, want 1", got)
	}

	// A user-authored turn claims input through runAgentTurn and calls this
	// reset; restore the budget explicitly here to pin that side of the
	// reference contract without invoking the full provider/tool stack.
	a.resetJobWakeBudget("budget-owner")
	appendDone("job-restored")
	if err := a.recoverJobCompletionWakes(log, handle); err != nil {
		t.Fatalf("restored recovery: %v", err)
	}
	pending = handle.Agent().Inbox().PendingMessages()
	found := false
	for _, message := range pending {
		if message.Metadata["job_id"] == "job-restored" {
			found = message.Kind == agent.MessageNextTurn
		}
	}
	if !found {
		t.Fatalf("restored pending = %+v, want waking job-restored message", pending)
	}
	if got := a.jobWakeCounts["budget-owner"]; got != 1 {
		t.Fatalf("restored job wake count = %d, want 1", got)
	}
}

type flakyAgentRuntimeStore struct {
	store.Store
	mu         sync.Mutex
	failLoad   bool
	failAppend bool
}

func (s *flakyAgentRuntimeStore) LoadSession(ctx context.Context, sessionID string) ([]session.Event, error) {
	s.mu.Lock()
	fail := s.failLoad
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected session log load failure")
	}
	return s.Store.LoadSession(ctx, sessionID)
}

func (s *flakyAgentRuntimeStore) AppendEvents(ctx context.Context, sessionID string, events []session.Event) error {
	s.mu.Lock()
	fail := s.failAppend
	s.mu.Unlock()
	if fail {
		return errors.New("injected session sink failure")
	}
	return s.Store.AppendEvents(ctx, sessionID, events)
}

func (s *flakyAgentRuntimeStore) setFailures(load, appendEvents bool) {
	s.mu.Lock()
	s.failLoad, s.failAppend = load, appendEvents
	s.mu.Unlock()
}

func dropRuntimeLog(a *app, sessionID string) {
	a.runtimeMu.Lock()
	delete(a.runtimeLogs, sessionID)
	a.runtimeMu.Unlock()
}

func appendExternalJobDone(t *testing.T, st store.Store, sessionID, jobID string, seq uint64) {
	t.Helper()
	data, err := json.Marshal(session.NewJobDone(jobID, "completed", "exit 0", "external output"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.AppendEvents(context.Background(), sessionID, []session.Event{{
		Seq: seq, Type: session.EventJobDone, At: time.Now().UTC(),
		Version: session.EventVersion, Data: data,
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func durableReceiptCounts(t *testing.T, st store.Store, sessionID string) (jobDone, inserted, inboxEvents int) {
	t.Helper()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replaySessionInbox(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventJobDone {
			jobDone++
		}
		if event.Type == session.EventAgentInboxSpliced {
			inboxEvents++
		}
	}
	for _, event := range inbox {
		inserted += len(event.Inserted)
	}
	return jobDone, inserted, inboxEvents
}

// TestSessionAgentMemoRetriesTransientLogFailures drives the production memo
// path, not just the recovery helper. A transient load failure must not strand
// an externally appended receipt; a stale cache must reload it; a transient
// sink failure must roll the inbox splice back; and the next memo lookup must
// retry exactly once under the stable dedupe key.
func TestSessionAgentMemoRetriesTransientLogFailures(t *testing.T) {
	ctx := context.Background()
	base, err := store.OpenSQLite(filepath.Join(t.TempDir(), "session-agent-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	if err := base.CreateSession(ctx, "retry-owner", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wrapped := &flakyAgentRuntimeStore{Store: base}
	a := &app{
		store:         wrapped,
		baseCtx:       ctx,
		agentRegistry: agent.NewRegistry(),
		cfg:           config.Config{Jobs: config.JobsConfig{CompletionDelivery: "quiet"}},
	}
	handle, err := a.sessionAgent("retry-owner")
	if err != nil {
		t.Fatal(err)
	}

	// A memo hit must not turn a transient authoritative-log read failure into
	// a permanent recovery failure. The next lookup reloads and can recover.
	dropRuntimeLog(a, "retry-owner")
	wrapped.setFailures(true, false)
	retry, err := a.sessionAgent("retry-owner")
	if err != nil || retry != handle {
		t.Fatalf("memo lookup during load failure = %v, %v; want same handle", retry, err)
	}
	appendExternalJobDone(t, base, "retry-owner", "external-job-1", 1)
	wrapped.setFailures(false, false)
	dropRuntimeLog(a, "retry-owner")
	retry, err = a.sessionAgent("retry-owner")
	if err != nil || retry != handle {
		t.Fatalf("memo retry after load failure = %v, %v; want same handle", retry, err)
	}
	jobDone, inserted, inboxEvents := durableReceiptCounts(t, base, "retry-owner")
	if jobDone != 1 || inserted != 1 || inboxEvents != 1 {
		t.Fatalf("after external receipt: job=%d inserted=%d inbox=%d; want 1/1/1", jobDone, inserted, inboxEvents)
	}

	// A sink failure at the inbox journal must roll back the splice. The next
	// production lookup retries and commits exactly one quiet owner receipt.
	appendExternalJobDone(t, base, "retry-owner", "external-job-2", 3)
	wrapped.setFailures(false, true)
	dropRuntimeLog(a, "retry-owner")
	retry, err = a.sessionAgent("retry-owner")
	if err != nil || retry != handle {
		t.Fatalf("memo lookup during append failure = %v, %v; want same handle", retry, err)
	}
	jobDone, inserted, inboxEvents = durableReceiptCounts(t, base, "retry-owner")
	if jobDone != 2 || inserted != 1 || inboxEvents != 1 {
		t.Fatalf("after failed append: job=%d inserted=%d inbox=%d; want 2/1/1", jobDone, inserted, inboxEvents)
	}
	wrapped.setFailures(false, false)
	dropRuntimeLog(a, "retry-owner")
	retry, err = a.sessionAgent("retry-owner")
	if err != nil || retry != handle {
		t.Fatalf("memo retry after append failure = %v, %v; want same handle", retry, err)
	}
	if err := retry.WhenIdle(ctx); err != nil {
		t.Fatal(err)
	}
	jobDone, inserted, inboxEvents = durableReceiptCounts(t, base, "retry-owner")
	if jobDone != 2 || inserted != 2 || inboxEvents != 2 {
		t.Fatalf("after append retry: job=%d inserted=%d inbox=%d; want 2/2/2", jobDone, inserted, inboxEvents)
	}
	if _, err = a.sessionAgent("retry-owner"); err != nil {
		t.Fatal(err)
	}
	_, inserted, inboxEvents = durableReceiptCounts(t, base, "retry-owner")
	if inserted != 2 || inboxEvents != 2 {
		t.Fatalf("repeat recovery inserted=%d inbox=%d; want 2/2", inserted, inboxEvents)
	}
}

type sessionOwnedLLM struct {
	sessionID string
	requests  []llm.ChatRequest
}

func (l *sessionOwnedLLM) ID() string      { return "session-owned" }
func (l *sessionOwnedLLM) Available() bool { return true }

func (l *sessionOwnedLLM) Stream(_ context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	l.requests = append(l.requests, req)
	text := "answer:" + l.sessionID
	if len(req.Messages) > 1 {
		text = "answer:" + req.Messages[1].Text()
	}
	return &scriptedReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: text},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

// TestConcurrentAddressedAgentsDoNotCrossGlobalState exercises the production
// Agent runtime with two SQLite-backed sessions at once. Each turn must bind its
// own durable log and cloned registry/provider snapshot; a currentID fallback
// would interleave or cross-write one of these projections.
func TestConcurrentAddressedAgentsDoNotCrossGlobalState(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"session-a", "session-b"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	model := &sessionOwnedLLM{sessionID: "session-a"}
	a := &app{
		cfg:           config.Config{Mode: config.ModeStandard, Model: "test-model"},
		store:         st,
		prompt:        prompt.New("test"),
		reg:           tools.New(),
		basePolicy:    tools.Policy{Profile: config.ModeStandard, Enabled: []string{"get_time"}},
		llm:           model,
		agentRegistry: agent.NewRegistry(),
		baseCtx:       ctx,
	}
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	a.reg.SetPolicy(a.basePolicy)

	handles := make(map[string]*agent.Handle)
	for _, id := range []string{"session-a", "session-b"} {
		handle, err := a.sessionAgent(id)
		if err != nil {
			t.Fatal(err)
		}
		handles[id] = handle
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index, id := range []string{"session-a", "session-b"} {
		wg.Add(1)
		go func(index int, id string) {
			defer wg.Done()
			errs[index] = handles[id].Run(ctx, "hello:"+id, nil)
			idleCtx, cancelIdle := context.WithTimeout(ctx, 5*time.Second)
			defer cancelIdle()
			if err := handles[id].WhenIdle(idleCtx); err != nil {
				errs[index] = errors.Join(errs[index], err)
			}
		}(index, id)
	}
	wg.Wait()
	for index, id := range []string{"session-a", "session-b"} {
		if errs[index] != nil {
			t.Fatalf("%s turn: %v", id, errs[index])
		}
	}

	for _, id := range []string{"session-a", "session-b"} {
		log, err := a.sessionLogForAgent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		history := log.DeriveHistory()
		if len(history) < 2 || history[0].Text() != "hello:"+id || history[len(history)-1].Text() != "answer:hello:"+id {
			t.Fatalf("%s history = %+v, want its user/assistant pair as boundaries", id, history)
		}
		other := "session-b"
		if id == "session-b" {
			other = "session-a"
		}
		for _, message := range history {
			if strings.Contains(message.Text(), "hello:"+other) || strings.Contains(message.Text(), "answer:hello:"+other) {
				t.Fatalf("%s history crossed %s state: %q", id, other, message.Text())
			}
		}
	}
}
