package interact

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

func TestSQLiteProviderAtomicRequestAndAudit(t *testing.T) {
	backend, err := store.OpenSQLite(filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	engine := NewEngine(mustSQLiteProvider(t, backend))
	creator, ok := interface{}(engine).(AtomicSessionCallRequester)
	if !ok {
		t.Fatal("engine must expose atomic request creation")
	}
	var audit session.Event
	request, atomic, err := creator.RequestForSessionWithCallIDAndEvent(context.Background(), "session-a", "call-1", "Approve?", "bash", "{}", func(created Request) session.Event {
		audit = session.Event{Seq: 1, Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: []byte(`{"id":"` + created.ID + `","toolName":"bash"}`)}
		return audit
	})
	if err != nil || !atomic || request.ID == "" {
		t.Fatalf("atomic request = %+v, atomic=%v, err=%v", request, atomic, err)
	}
	items, err := mustSQLiteProvider(t, backend).List(context.Background())
	if err != nil || len(items) != 1 || items[0].ID != request.ID {
		t.Fatalf("approval projection = %+v, err=%v", items, err)
	}
	events, err := backend.InspectSession(context.Background(), "session-a")
	if err != nil || len(events) != 1 || events[0].Type != session.EventApprovalAsked || string(events[0].Data) != string(audit.Data) {
		t.Fatalf("audit events = %+v, err=%v", events, err)
	}
}

func TestSQLiteProviderAtomicStructuredRequestAndAudit(t *testing.T) {
	backend, err := store.OpenSQLite(filepath.Join(t.TempDir(), "structured-approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	engine := NewEngine(mustSQLiteProvider(t, backend))
	creator, ok := interface{}(engine).(AtomicStructuredSessionRequester)
	if !ok {
		t.Fatal("engine must expose atomic structured request creation")
	}
	var audit session.Event
	questions := []Question{{ID: "mode", Question: "Mode?", Options: []QuestionOption{{Label: "safe"}}}}
	request, atomic, err := creator.RequestForSessionWithQuestionsAndEvent(context.Background(), "session-a", "Choose", ToolAskUserQuestionName, "{}", questions, func(created Request) session.Event {
		audit = session.Event{Seq: 1, Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: []byte(`{"id":"` + created.ID + `","questions":[{"id":"mode"}]}`)}
		return audit
	})
	if err != nil || !atomic || request.ID == "" || len(request.Questions) != 1 {
		t.Fatalf("atomic structured request = %+v, atomic=%v, err=%v", request, atomic, err)
	}
	events, err := backend.InspectSession(context.Background(), "session-a")
	if err != nil || len(events) != 1 || string(events[0].Data) != string(audit.Data) {
		t.Fatalf("structured audit events = %+v, err=%v", events, err)
	}
}

func mustSQLiteProvider(t *testing.T, backend *store.SQLiteStore) Provider {
	t.Helper()
	provider, err := NewSQLiteProvider(backend)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestSQLiteProviderSurvivesRestartAndRejectsDuplicateResolution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.db")
	backend, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewSQLiteProvider(backend)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(provider)
	request, err := engine.RequestForSessionWithCallID(context.Background(), "session-a", "call-1", "Approve?", "bash", "{}")
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := interface{}(engine).(AnswerResolver)
	if !ok {
		t.Fatal("engine must expose answer resolver")
	}
	if _, err := resolver.ResolveWithAnswer(context.Background(), request.ID, StatusAllowedOnce, `{"answers":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Resolve(context.Background(), request.ID, StatusRejected); err == nil {
		t.Fatal("duplicate resolution must be rejected")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedProvider, err := NewSQLiteProvider(reopened)
	if err != nil {
		t.Fatal(err)
	}
	items, err := reopenedProvider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != request.ID || items[0].Status != StatusAllowedOnce || items[0].SessionID != "session-a" || items[0].CallID != "call-1" || items[0].Answer != `{"answers":[]}` {
		t.Fatalf("reopened approvals = %+v, want the resolved request with durable identity", items)
	}
}

func TestSQLiteApprovalResolutionIsCrossProcessCompareAndSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross-process-approvals.db")
	firstStore, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()

	first := NewEngine(mustSQLiteProvider(t, firstStore))
	second := NewEngine(mustSQLiteProvider(t, secondStore))
	defer first.Close()
	defer second.Close()
	creator, ok := interface{}(first).(AtomicSessionCallRequester)
	if !ok {
		t.Fatal("engine must expose atomic request creation")
	}
	request, atomic, err := creator.RequestForSessionWithCallIDAndEvent(context.Background(), "cross-process", "call-1", "Approve?", "bash", "{}", func(created Request) session.Event {
		return session.Event{Seq: 1, Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: []byte(`{"id":"` + created.ID + `"}`)}
	})
	if err != nil || !atomic {
		t.Fatalf("create approval = %+v atomic=%v err=%v", request, atomic, err)
	}

	resolve := func(engine Engine, outcome ApprovalStatus) error {
		resolver, ok := interface{}(engine).(AtomicEventResolver)
		if !ok {
			return errors.New("engine must expose atomic decision resolution")
		}
		_, _, err := resolver.ResolveForSessionWithAnswerAndEvent(context.Background(), "cross-process", request.ID, outcome, "", session.Event{
			Seq: 2, Type: session.EventApprovalDecided, At: time.Now().UTC(), Version: session.EventVersion,
			Data: []byte(`{"id":"` + request.ID + `","outcome":"` + string(outcome) + `"}`),
		})
		return err
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index, outcome := range []ApprovalStatus{StatusAllowedOnce, StatusRejected} {
		outcome := outcome
		resolverEngine := Engine(first)
		if index == 1 {
			resolverEngine = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- resolve(resolverEngine, outcome)
		}()
	}
	wg.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyResolved):
			conflicts++
		default:
			t.Fatalf("cross-process resolution error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("cross-process outcomes successes=%d conflicts=%d", successes, conflicts)
	}
	rows, err := secondStore.ListApprovalRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status == string(StatusPending) {
		t.Fatalf("durable approval after concurrent resolution = %+v", rows)
	}
	events, err := secondStore.InspectSession(context.Background(), "cross-process")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != session.EventApprovalDecided {
		t.Fatalf("durable approval events = %+v", events)
	}
}

func TestSQLiteProviderScopedListReadsOnlyOwnedRows(t *testing.T) {
	backend, err := store.OpenSQLite(filepath.Join(t.TempDir(), "scoped-approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	provider, err := NewSQLiteProvider(backend)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(provider)
	defer engine.Close()
	requester := interface{}(engine).(SessionRequester)
	owned, err := requester.RequestForSession(context.Background(), "owner-a", "owned", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requester.RequestForSession(context.Background(), "owner-b", strings.Repeat("secret", 100), "write", "{}"); err != nil {
		t.Fatal(err)
	}
	lister := interface{}(engine).(SessionLister)
	items, err := lister.ListForSession(context.Background(), "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != owned.ID || items[0].SessionID != "owner-a" {
		t.Fatalf("scoped approvals = %+v, want only %s", items, owned.ID)
	}
}

func TestSQLiteProviderRestoresPendingRequestWithoutReissuingID(t *testing.T) {
	backend, err := store.OpenSQLite(filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	provider, err := NewSQLiteProvider(backend)
	if err != nil {
		t.Fatal(err)
	}
	original := Request{ID: "req-restored", SessionID: "s", Prompt: "p", ToolName: "t", Args: "{}", Status: StatusPending}
	restorer, ok := provider.(RequestRestorer)
	if !ok {
		t.Fatal("SQLite provider must support request restoration")
	}
	if err := restorer.Restore(context.Background(), []Request{original}); err != nil {
		t.Fatal(err)
	}
	items, err := provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != original.ID || items[0].Status != StatusPending {
		t.Fatalf("restored approvals = %+v", items)
	}
}

func TestSQLiteProviderReplacesOrphanedProjectionFromEventSnapshot(t *testing.T) {
	backend, err := store.OpenSQLite(filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	provider, err := NewSQLiteProvider(backend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Create(context.Background(), Request{
		SessionID: "crashed", Prompt: "orphan", ToolName: "bash", Args: "{}", Status: StatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	replacer, ok := provider.(RequestReplacer)
	if !ok {
		t.Fatal("SQLite provider must support projection replacement")
	}
	canonical := Request{
		ID: "req-from-log", SessionID: "session-a", CallID: "call-a", Prompt: "Approve?",
		ToolName: "bash", Args: "{}", Status: StatusRejected,
	}
	if err := replacer.Replace(context.Background(), []Request{canonical}); err != nil {
		t.Fatal(err)
	}
	items, err := provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != canonical.ID || items[0].SessionID != canonical.SessionID || items[0].Status != StatusRejected {
		t.Fatalf("reconciled approvals = %+v, want only the event-log snapshot", items)
	}
}
