package interact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Compile-time assertions: engine implements the Engine Service and memProvider
// implements the Provider interface.
var _ Engine = (*engine)(nil)
var _ Provider = (*memProvider)(nil)

// newTestEngine returns a fresh engine backed by the default in-memory provider.
func newTestEngine(t *testing.T) *engine {
	t.Helper()
	e := NewEngine(nil)
	t.Cleanup(func() { e.Close() })
	return e
}

// --- Request / Resolve ------------------------------------------------------

func TestEngineRequestResolve(t *testing.T) {
	e := newTestEngine(t)

	r, err := e.Request(context.Background(), "Allow running: rm -rf /tmp/x", "bash", `{"command":"rm -rf /tmp/x"}`)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if r.ID == "" {
		t.Error("Request must issue a non-empty id")
	}
	if r.Status != StatusPending {
		t.Errorf("new request status = %q, want %q", r.Status, StatusPending)
	}
	if r.Prompt != "Allow running: rm -rf /tmp/x" || r.ToolName != "bash" {
		t.Errorf("request fields = prompt %q tool %q, want the given prompt/tool", r.Prompt, r.ToolName)
	}
	if r.Args != `{"command":"rm -rf /tmp/x"}` {
		t.Errorf("request args = %q, want the given args", r.Args)
	}
	if r.CreatedAt.IsZero() {
		t.Error("new request must have a CreatedAt")
	}
	if r.ResolvedAt != nil {
		t.Error("new request must have nil ResolvedAt")
	}

	// Approved path.
	got, err := e.Resolve(context.Background(), r.ID, StatusApproved)
	if err != nil {
		t.Fatalf("Resolve(approved): %v", err)
	}
	if got.ID != r.ID || got.Status != StatusApproved {
		t.Errorf("resolved request = %+v, want id %s approved", got, r.ID)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved request must have a ResolvedAt")
	}

	// Rejected path (fresh request).
	r2, _ := e.Request(context.Background(), "Allow delete", "delete_file", "{}")
	got2, err := e.Resolve(context.Background(), r2.ID, StatusRejected)
	if err != nil {
		t.Fatalf("Resolve(rejected): %v", err)
	}
	if got2.Status != StatusRejected || got2.ResolvedAt == nil {
		t.Errorf("rejected request = %+v, want rejected + stamped", got2)
	}
}

// --- Validation -------------------------------------------------------------

func TestEngineRequestValidation(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.Request(context.Background(), "", "tool", "{}"); !errors.Is(err, ErrInvalidPrompt) {
		t.Errorf("Request(empty prompt): err = %v, want ErrInvalidPrompt", err)
	}
	// The bound counts runes, not bytes — 201 multibyte runes must be rejected
	// while exactly the bound is accepted.
	longArgs := strings.Repeat("界", maxArgsLen+1)
	if _, err := e.Request(context.Background(), "p", "tool", longArgs); !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("Request(over-long args): err = %v, want ErrInvalidArgs", err)
	}
	if _, err := e.Request(context.Background(), "p", "tool", strings.Repeat("界", maxArgsLen)); err != nil {
		t.Errorf("Request(bound-length args): %v", err)
	}
}

func TestEngineResolveInvalidStatus(t *testing.T) {
	e := newTestEngine(t)
	r, _ := e.Request(context.Background(), "p", "tool", "{}")
	for _, bad := range []ApprovalStatus{StatusPending, StatusExpired, "bogus", ""} {
		if _, err := e.Resolve(context.Background(), r.ID, bad); !errors.Is(err, ErrInvalidStatus) {
			t.Errorf("Resolve(status %q): err = %v, want ErrInvalidStatus", bad, err)
		}
	}
	// The request must still be pending after all the rejected resolutions.
	all, _ := e.List(context.Background())
	if len(all) != 1 || all[0].Status != StatusPending {
		t.Errorf("request after rejected resolutions = %+v, want still pending", all)
	}
}

func TestAllowedOnceIsAValidApprovalOutcome(t *testing.T) {
	e := newTestEngine(t)
	request, err := e.Request(context.Background(), "allow", "bash", "{}")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := e.Resolve(context.Background(), request.ID, StatusAllowedOnce)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != StatusAllowedOnce {
		t.Fatalf("status = %q, want %q", resolved.Status, StatusAllowedOnce)
	}
}

func TestPendingApprovalExpiresAndCannotBeResolved(t *testing.T) {
	e := newTestEngine(t)
	expiry := interface{}(e).(ExpiryController)
	expiry.SetRequestTTL(time.Nanosecond)
	r, err := e.Request(context.Background(), "expire me", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	items, err := e.List(context.Background())
	if err != nil || len(items) != 1 || items[0].Status != StatusExpired {
		t.Fatalf("expired list = %+v, err=%v", items, err)
	}
	if _, err := e.Resolve(context.Background(), r.ID, StatusApproved); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("resolve after expiry = %v, want ErrAlreadyResolved", err)
	}
}

func TestPendingApprovalExpiryInvokesCanonicalAuditSeam(t *testing.T) {
	e := newTestEngine(t)
	controller := interface{}(e).(ExpiryController)
	auditor := interface{}(e).(ExpiryAuditor)
	controller.SetRequestTTL(time.Nanosecond)
	var audited Request
	var calls int
	auditor.SetExpiryAuditor(func(_ context.Context, request Request) error {
		audited = request
		calls++
		return nil
	})
	requester := interface{}(e).(SessionRequester)
	request, err := requester.RequestForSession(context.Background(), "session-expiry", "expire", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	items, err := interface{}(e).(SessionLister).ListForSession(context.Background(), "session-expiry")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != StatusExpired || audited.ID != request.ID || audited.SessionID != "session-expiry" || calls != 1 {
		t.Fatalf("expired approval = %+v, audited=%+v calls=%d", items, audited, calls)
	}
}

// --- Unknown id / duplicate resolution ---------------------------------------

func TestSessionApprovalPolicyNeverRejectsWithoutPendingRequest(t *testing.T) {
	engine := NewEngine(nil)
	defer engine.Close()
	controller, ok := interface{}(engine).(PolicyController)
	if !ok {
		t.Fatal("engine does not expose PolicyController")
	}
	if err := controller.SetSessionPolicy("session-never", PolicyNever); err != nil {
		t.Fatal(err)
	}
	requester := interface{}(engine).(SessionRequester)
	request, err := requester.RequestForSession(context.Background(), "session-never", "approve", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != StatusRejected || request.ID != "" {
		t.Fatalf("never policy request = %+v, want rejected without id", request)
	}
	if pending, err := engine.List(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending after never policy = %+v, err=%v", pending, err)
	}
}

func TestSessionApprovalPolicyNeverAppliesToCallAndQuestionRequests(t *testing.T) {
	engine := NewEngine(nil)
	defer engine.Close()
	controller := interface{}(engine).(PolicyController)
	if err := controller.SetSessionPolicy("session-never", PolicyNever); err != nil {
		t.Fatal(err)
	}
	requester := interface{}(engine).(SessionCallRequester)
	call, err := requester.RequestForSessionWithCallID(context.Background(), "session-never", "call-1", "approve", "write", "{}")
	if err != nil || call.Status != StatusRejected || call.ID != "" || call.CallID != "call-1" {
		t.Fatalf("never call request = %+v, err=%v", call, err)
	}
	structured := interface{}(engine).(StructuredSessionRequester)
	question, err := structured.RequestForSessionWithQuestions(context.Background(), "session-never", "choose", "ask_user_question", "{}", []Question{{ID: "q", Question: "Proceed?"}})
	if err != nil || question.Status != StatusRejected || question.ID != "" || len(question.Questions) != 1 {
		t.Fatalf("never question request = %+v, err=%v", question, err)
	}
}

func TestSessionApprovalOwnershipIsEnforcedByService(t *testing.T) {
	e := newTestEngine(t)
	requester := interface{}(e).(SessionRequester)
	resolver := interface{}(e).(SessionResolver)
	r, err := requester.RequestForSession(context.Background(), "owner", "approve", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "owner" {
		t.Fatalf("request session id = %q, want owner", r.SessionID)
	}
	if _, err := resolver.ResolveForSession(context.Background(), "other", r.ID, StatusApproved); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("cross-session resolve = %v, want ErrWrongSession", err)
	}
	resolved, err := resolver.ResolveForSession(context.Background(), "owner", r.ID, StatusAllowedOnce)
	if err != nil || resolved.Status != StatusAllowedOnce {
		t.Fatalf("owner resolve = %+v, err=%v", resolved, err)
	}
}

func TestSessionApprovalListIsOwnershipScoped(t *testing.T) {
	e := newTestEngine(t)
	requester := interface{}(e).(SessionRequester)
	first, err := requester.RequestForSession(context.Background(), "owner-a", "a", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requester.RequestForSession(context.Background(), "owner-b", "b", "write", "{}"); err != nil {
		t.Fatal(err)
	}
	lister := interface{}(e).(SessionLister)
	items, err := lister.ListForSession(context.Background(), "owner-a")
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("owner-a approvals = %+v, err=%v", items, err)
	}
	if _, err := lister.ListForSession(context.Background(), ""); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("empty session list = %v, want ErrWrongSession", err)
	}
}

func TestSessionApprovalCancellationOnlyClosesDisposedOwner(t *testing.T) {
	e := newTestEngine(t)
	requester := interface{}(e).(SessionRequester)
	first, err := requester.RequestForSession(context.Background(), "owner-a", "a", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	second, err := requester.RequestForSession(context.Background(), "owner-b", "b", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	canceler := interface{}(e).(SessionCanceler)
	cancelled, err := canceler.CancelForSession(context.Background(), "owner-a")
	if err != nil || len(cancelled) != 1 || cancelled[0].ID != first.ID || cancelled[0].Status != StatusCanceled {
		t.Fatalf("owner-a cancellation = %+v, err=%v", cancelled, err)
	}
	items, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]ApprovalStatus{}
	for _, item := range items {
		statuses[item.ID] = item.Status
	}
	if statuses[first.ID] != StatusCanceled || statuses[second.ID] != StatusPending {
		t.Fatalf("approval statuses = %#v, want owner-a cancelled and owner-b pending", statuses)
	}
}

func TestEngineResolveUnknownID(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Resolve(context.Background(), "req-missing", StatusApproved); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("Resolve(unknown id): err = %v, want ErrUnknownRequest", err)
	}
}

func TestEngineResolveDuplicate(t *testing.T) {
	e := newTestEngine(t)
	r, _ := e.Request(context.Background(), "p", "tool", "{}")
	if _, err := e.Resolve(context.Background(), r.ID, StatusApproved); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := e.Resolve(context.Background(), r.ID, StatusRejected); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("second Resolve: err = %v, want ErrAlreadyResolved", err)
	}
	// The stored record must keep the first decision.
	all, _ := e.List(context.Background())
	if len(all) != 1 || all[0].Status != StatusApproved {
		t.Errorf("request after duplicate resolve = %+v, want approved unchanged", all)
	}
}

// --- Pending cap ------------------------------------------------------------

func TestEnginePendingLimit(t *testing.T) {
	e := newTestEngine(t)
	for i := 0; i < defaultPendingLimit; i++ {
		if _, err := e.Request(context.Background(), fmt.Sprintf("prompt %d", i), "tool", "{}"); err != nil {
			t.Fatalf("Request %d: %v", i, err)
		}
	}
	if _, err := e.Request(context.Background(), "over the cap", "tool", "{}"); !errors.Is(err, ErrPendingLimit) {
		t.Errorf("Request over the cap: err = %v, want ErrPendingLimit", err)
	}
	// Resolving one request frees a slot.
	all, _ := e.List(context.Background())
	if _, err := e.Resolve(context.Background(), all[0].ID, StatusRejected); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := e.Request(context.Background(), "after a resolve", "tool", "{}"); err != nil {
		t.Errorf("Request after a resolve freed the cap: %v", err)
	}
}

func TestEnginePendingLimitIsAtomicAcrossConcurrentRequests(t *testing.T) {
	e := newTestEngine(t)
	e.pendingLimit = 1
	const callers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	limitErrors := 0
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.Request(context.Background(), fmt.Sprintf("concurrent %d", i), "write", "{}")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else if errors.Is(err, ErrPendingLimit) {
				limitErrors++
			} else {
				t.Errorf("concurrent Request: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 1 || limitErrors != callers-1 {
		t.Fatalf("concurrent admission succeeded=%d limitErrors=%d, want 1/%d", succeeded, limitErrors, callers-1)
	}
}

// --- List -------------------------------------------------------------------

func TestEngineList(t *testing.T) {
	e := newTestEngine(t)
	r1, _ := e.Request(context.Background(), "one", "tool", `{"a":1}`)
	r2, _ := e.Request(context.Background(), "two", "tool", `{"a":2}`)
	all, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d requests, want 2", len(all))
	}
	if all[0].ID != r1.ID || all[1].ID != r2.ID {
		t.Errorf("List order = [%s %s], want creation order [%s %s]", all[0].ID, all[1].ID, r1.ID, r2.ID)
	}
	// A resolution must be reflected in List.
	if _, err := e.Resolve(context.Background(), r1.ID, StatusApproved); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	all, _ = e.List(context.Background())
	if all[0].Status != StatusApproved || all[0].ResolvedAt == nil {
		t.Errorf("resolved request in List = %+v, want approved + stamped", all[0])
	}
}

func TestEngineRestorePreservesIDsAndLifecycle(t *testing.T) {
	eng := NewEngine(NewMemProvider())
	created := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	resolved := created.Add(time.Minute)
	if err := eng.Restore(context.Background(), []Request{
		{ID: "req-7", Prompt: "restart approval", ToolName: "bash", Status: StatusPending, CreatedAt: created},
		{ID: "req-8", Prompt: "done approval", ToolName: "bash", Status: StatusApproved, CreatedAt: created, ResolvedAt: &resolved},
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	items, err := eng.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("List after Restore = %+v, err=%v", items, err)
	}
	if _, err := eng.Resolve(context.Background(), "req-7", StatusRejected); err != nil {
		t.Fatalf("Resolve restored request: %v", err)
	}
	createdAfter, err := eng.Request(context.Background(), "new", "bash", "{}")
	if err != nil {
		t.Fatalf("Request after Restore: %v", err)
	}
	if createdAfter.ID != "req-9" {
		t.Fatalf("new id after Restore = %q, want req-9", createdAfter.ID)
	}
}

func TestSessionAwareRequestsHaveDistinctProviderIDs(t *testing.T) {
	requester := interface{}(NewEngine(NewMemProvider())).(SessionRequester)
	first, err := requester.RequestForSession(context.Background(), "session-a", "approve", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	second, err := requester.RequestForSession(context.Background(), "session-b", "approve", "write", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("session-aware provider IDs collided: %q", first.ID)
	}
	if first.SessionID != "session-a" || second.SessionID != "session-b" {
		t.Fatalf("session ownership = %q/%q", first.SessionID, second.SessionID)
	}
}

// --- Await ------------------------------------------------------------------

func TestEngineAwaitResolved(t *testing.T) {
	e := newTestEngine(t)
	e.poll = time.Millisecond // injected short poll interval

	r, err := e.Request(context.Background(), "allow write", "write_file", `{"path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	type awaitResult struct {
		req Request
		err error
	}
	done := make(chan awaitResult, 1)
	go func() {
		req, err := e.Await(context.Background(), r.ID)
		done <- awaitResult{req, err}
	}()

	// Give Await a chance to start polling, then resolve from the CLI side.
	time.Sleep(5 * time.Millisecond)
	if _, err := e.Resolve(context.Background(), r.ID, StatusApproved); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Await: %v", res.err)
		}
		if res.req.Status != StatusApproved {
			t.Errorf("Await returned status %q, want %q", res.req.Status, StatusApproved)
		}
		if res.req.ResolvedAt == nil {
			t.Error("Await returned a request without ResolvedAt")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after Resolve")
	}
}

func TestEngineAwaitRejected(t *testing.T) {
	e := newTestEngine(t)
	e.poll = time.Millisecond
	r, _ := e.Request(context.Background(), "p", "tool", "{}")

	done := make(chan Request, 1)
	go func() {
		req, err := e.Await(context.Background(), r.ID)
		if err != nil {
			t.Errorf("Await: %v", err)
		}
		done <- req
	}()
	time.Sleep(5 * time.Millisecond)
	if _, err := e.Resolve(context.Background(), r.ID, StatusRejected); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case req := <-done:
		if req.Status != StatusRejected {
			t.Errorf("Await returned status %q, want %q", req.Status, StatusRejected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after Reject")
	}
}

func TestEngineAwaitContextCancel(t *testing.T) {
	e := newTestEngine(t)
	e.poll = time.Millisecond
	r, _ := e.Request(context.Background(), "p", "tool", "{}")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Await(ctx, r.ID)
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Await after cancel: err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after context cancel")
	}
}

func TestEngineAwaitUnknownID(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Await(context.Background(), "req-missing"); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("Await(unknown id): err = %v, want ErrUnknownRequest", err)
	}
}

// --- Close ------------------------------------------------------------------

func TestEngineCloseIdempotent(t *testing.T) {
	e := NewEngine(nil)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close must be idempotent, got %v", err)
	}

	if _, err := e.Request(context.Background(), "p", "tool", "{}"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Request after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.Resolve(context.Background(), "req-1", StatusApproved); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Resolve after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.Await(context.Background(), "req-1"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Await after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.List(context.Background()); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("List after Close: err = %v, want ErrEngineClosed", err)
	}
}

func TestEngineUsesInjectedProvider(t *testing.T) {
	p := NewMemProvider()
	e := NewEngine(p)
	defer e.Close()

	r, err := e.Request(context.Background(), "p", "tool", "{}")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	all, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("provider List: %v", err)
	}
	if len(all) != 1 || all[0].ID != r.ID {
		t.Errorf("injected provider holds %+v, want the created request", all)
	}
}
