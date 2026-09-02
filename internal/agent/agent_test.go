package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

func TestInboxRichContentPreservesBlockOrder(t *testing.T) {
	i := NewInbox()
	want := []llm.ContentBlock{llm.Text("text"), {Kind: llm.BlockReasoning, Text: "thought"}}
	got, err := i.FollowupContent(want, nil)
	if err != nil {
		t.Fatal(err)
	}
	want[0].Text = "mutated"
	input, ok := i.Claim()
	if !ok || len(input.Messages) != 1 || !reflect.DeepEqual(input.Messages[0].Content, got.Content) || input.Messages[0].Content[0].Text != "text" {
		t.Fatalf("rich inbox message = %+v, want detached ordered blocks", input)
	}
}

func TestHandleFollowupWithIDReturnsEnqueueReceipt(t *testing.T) {
	registry := NewRegistry()
	handle, err := registry.Create(Options{
		ID:     "receipt-agent",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.CloseAll()
	message, err := handle.FollowupWithID("hello", map[string]string{"source": "sdk"})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == "" || message.Kind != MessageNextTurn || message.Text != "hello" {
		t.Fatalf("receipt = %+v, want identified next-turn message", message)
	}
	claimed, ok := handle.Agent().Inbox().Claim()
	if !ok || len(claimed.Messages) != 1 || claimed.Messages[0].ID != message.ID {
		t.Fatalf("claimed = %+v, want receipt %q", claimed, message.ID)
	}
}

func TestWhenIdleWaitsThroughClaimToRunningBoundary(t *testing.T) {
	claimStarted := make(chan struct{})
	releaseClaim := make(chan struct{})
	runnerStarted := make(chan struct{})
	releaseRunner := make(chan struct{})
	journal := &claimGateJournal{claimStarted: claimStarted, release: releaseClaim}
	registry := NewRegistry()
	defer registry.CloseAll()
	handle, err := registry.Create(Options{
		ID:           "claim-race",
		InboxJournal: journal,
		Runner: func(context.Context, *Agent, TurnInput) error {
			runnerStarted <- struct{}{}
			<-releaseRunner
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Send("work", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-claimStarted:
	case <-time.After(time.Second):
		t.Fatal("agent did not reach inbox claim")
	}
	idleDone := make(chan error, 1)
	go func() { idleDone <- handle.WhenIdle(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	close(releaseClaim)
	select {
	case <-runnerStarted:
	case <-time.After(time.Second):
		t.Fatal("gated claim did not reach runner")
	}
	select {
	case err := <-idleDone:
		t.Fatalf("WhenIdle returned during claim-to-running boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRunner)
	select {
	case err := <-idleDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WhenIdle did not return after runner completion")
	}
}

type claimGateJournal struct {
	claimStarted chan struct{}
	release      chan struct{}
}

func (j *claimGateJournal) AppendInboxEvent(event InboxEvent) error {
	if len(event.Inserted) == 0 {
		j.claimStarted <- struct{}{}
		<-j.release
	}
	return nil
}

func TestAgentMaintenanceWaitsBeforeDeliveringWake(t *testing.T) {
	registry := NewRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	runnerStarted := make(chan struct{})
	handle, err := registry.Create(Options{
		ID: "maintenance-agent",
		Runner: func(context.Context, *Agent, TurnInput) error {
			close(runnerStarted)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.CloseAll()

	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- handle.RunMaintenance(func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}

	whenIdleCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := handle.WhenIdle(whenIdleCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WhenIdle during maintenance = %v, want deadline", err)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- handle.Run(context.Background(), "wake after maintenance", nil) }()
	select {
	case <-runnerStarted:
		t.Fatal("wake ran while maintenance was active")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-maintenanceDone; err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("queued Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued wake did not run after maintenance")
	}
}

func TestAgentCloseCancelsMaintenance(t *testing.T) {
	handle, err := NewRegistry().Create(Options{
		ID:     "maintenance-close",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- handle.RunMaintenance(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case err := <-maintenanceDone:
		t.Fatalf("maintenance finished before close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-maintenanceDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("maintenance after Close = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel maintenance")
	}
}

func TestAgentConcurrentCloseWaitsForOneMaintenanceDisposal(t *testing.T) {
	handle, err := NewRegistry().Create(Options{
		ID:     "maintenance-concurrent-close",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- handle.RunMaintenance(func(ctx context.Context) error {
			close(started)
			_ = ctx
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}

	closeDone := make(chan struct{}, 2)
	go func() { _ = handle.Close(); closeDone <- struct{}{} }()
	go func() { _ = handle.Close(); closeDone <- struct{}{} }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while maintenance was still active")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent Close did not settle")
		}
	}
	if err := <-maintenanceDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("maintenance: %v", err)
	}
}

func TestScopeResolvesParentAndDisposesInReverseOrder(t *testing.T) {
	root := NewScope(nil)
	if err := root.Provide("root", "value"); err != nil {
		t.Fatal(err)
	}
	child := NewScope(root)
	if err := child.Provide("child", 42); err != nil {
		t.Fatal(err)
	}
	if got, err := child.Resolve("root"); err != nil || got != "value" {
		t.Fatalf("Resolve(root) = %#v, %v", got, err)
	}
	if _, err := child.Resolve("missing"); !errors.Is(err, ErrValueAbsent) {
		t.Fatalf("Resolve(missing) error = %v", err)
	}
	var mu sync.Mutex
	var disposed []string
	for _, name := range []string{"first", "second"} {
		name := name
		if err := child.AddCleanup(func() error {
			mu.Lock()
			disposed = append(disposed, name)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := disposed, []string{"second", "first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("disposal order = %v, want %v", got, want)
	}
	if _, err := child.Resolve("child"); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("closed scope resolve error = %v", err)
	}
	if err := child.Provide("later", nil); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("closed scope provide error = %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScopeCloseCascadesToChildrenBeforeParentDisposers(t *testing.T) {
	root := NewScope(nil)
	child := NewScope(root)
	grandchild := NewScope(child)
	var order []string
	if err := root.AddCleanup(func() error { order = append(order, "root"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := child.AddCleanup(func() error { order = append(order, "child"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := grandchild.AddCleanup(func() error { order = append(order, "grandchild"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"grandchild", "child", "root"}) {
		t.Fatalf("cascade disposal order = %v", order)
	}
	if _, err := grandchild.Resolve("anything"); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("grandchild resolve after parent close = %v", err)
	}
}

func TestInboxQuietInjectionWaitsForWakeAndClaimsOneTurnMessage(t *testing.T) {
	i := NewInbox()
	if _, err := i.Inject("context", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := i.Claim(); ok {
		t.Fatal("quiet injection woke a turn")
	}
	if _, err := i.Send("step", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Followup("turn-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Followup("turn-2", nil); err != nil {
		t.Fatal(err)
	}
	input, ok := i.Claim()
	if !ok {
		t.Fatal("Claim returned no waking input")
	}
	var got []string
	for _, message := range input.Messages {
		got = append(got, message.Text)
	}
	if want := []string{"step", "turn-1", "context"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first claim = %v, want %v", got, want)
	}
	input, ok = i.Claim()
	if !ok || len(input.Messages) != 1 || input.Messages[0].Text != "turn-2" {
		t.Fatalf("second claim = %#v, %v", input, ok)
	}
	if _, ok := i.Claim(); ok {
		t.Fatal("empty inbox returned a claim")
	}
}

func TestInboxDeduplicatesTeamMessageAcrossRetry(t *testing.T) {
	i := NewInbox()
	metadata := map[string]string{"team_message_id": "msg-1"}
	first, err := i.Followup("first", metadata)
	if err != nil {
		t.Fatal(err)
	}
	second, err := i.Followup("duplicate", map[string]string{"team_message_id": "msg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Text != first.Text {
		t.Fatalf("deduplicated messages = %#v / %#v", first, second)
	}
	input, ok := i.Claim()
	if !ok || len(input.Messages) != 1 || input.Messages[0].ID != first.ID {
		t.Fatalf("claimed messages = %#v, ok=%v", input.Messages, ok)
	}
}

type recordingInboxJournal struct {
	mu     sync.Mutex
	events []InboxEvent
	err    error
}

func (j *recordingInboxJournal) AppendInboxEvent(event InboxEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	j.events = append(j.events, event)
	return nil
}

func TestDurableInboxReplaysPendingWorkAndCommitsBeforeMutation(t *testing.T) {
	journal := &recordingInboxJournal{}
	inbox, err := NewDurableInbox(journal, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := inbox.Followup("queued", map[string]string{"source": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 1 || journal.events[0].Target != "next-turn" || len(journal.events[0].Inserted) != 1 {
		t.Fatalf("insert journal = %#v", journal.events)
	}
	step, err := inbox.Inject("quiet", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := inbox.ClaimWithError(7); err != nil || !ok {
		t.Fatalf("claim = ok:%v err:%v", ok, err)
	}
	if len(journal.events) != 4 || journal.events[2].Target != "next-step" || journal.events[2].Turn != 7 || journal.events[3].Target != "next-turn" {
		t.Fatalf("claim journal = %#v", journal.events)
	}

	// Rebuild from the same durable splice stream: claimed messages are absent
	// and the next generated id must not collide with a replayed id.
	replayed, err := NewDurableInbox(journal, journal.events[:1])
	if err != nil {
		t.Fatal(err)
	}
	next, err := replayed.Followup("later", nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatalf("replayed inbox reused message id %q", next.ID)
	}
	if step.ID == "" {
		t.Fatal("quiet message id is empty")
	}

	journal.err = errors.New("disk full")
	if _, err := replayed.Followup("must fail", nil); err == nil {
		t.Fatal("journal failure was hidden")
	}
	journal.err = nil
	if input, ok := replayed.Claim(); !ok || len(input.Messages) != 1 || input.Messages[0].Text != "queued" {
		t.Fatalf("failed insert changed live inbox: input=%+v ok=%v", input, ok)
	}
	if input, ok := replayed.Claim(); !ok || len(input.Messages) != 1 || input.Messages[0].Text != "later" {
		t.Fatalf("failed insert removed existing queue state: input=%+v ok=%v", input, ok)
	}
}

func TestInboxDoesNotCrossDeduplicateLocalTeamMessageIDs(t *testing.T) {
	i := NewInbox()
	first, err := i.Followup("first", map[string]string{"team_id": "team-a", "team_message_id": "msg-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := i.Followup("second", map[string]string{"team_id": "team-b", "team_message_id": "msg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("messages from different teams were coalesced: first=%+v second=%+v", first, second)
	}
}

func TestAgentSteerCancelsCurrentTurnAndPreservesQueuedInput(t *testing.T) {
	rt := NewRegistry()
	defer func() { _ = rt.CloseAll() }()
	started := make(chan TurnInput, 2)
	finished := make(chan error, 1)
	handle, err := rt.Create(Options{
		ID: "root",
		Runner: func(ctx context.Context, _ *Agent, input TurnInput) error {
			started <- input
			<-ctx.Done()
			finished <- ctx.Err()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Send("first", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first turn did not start")
	}
	if err := handle.Steer("replacement", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-finished:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("first turn cancel error = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not cancel")
	}
	select {
	case input := <-started:
		if len(input.Messages) != 1 || input.Messages[0].Text != "replacement" {
			t.Fatalf("steered turn input = %#v", input)
		}
	case <-time.After(time.Second):
		t.Fatal("steered turn did not start")
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryChildScopeAndPublication(t *testing.T) {
	rt := NewRegistry()
	defer func() { _ = rt.CloseAll() }()
	if err := rt.RootScope().Provide("provider", "root-provider"); err != nil {
		t.Fatal(err)
	}
	runner := func(context.Context, *Agent, TurnInput) error { return nil }
	parent, err := rt.Create(Options{ID: "parent", Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	child, err := rt.Create(Options{ID: "child", ParentID: parent.ID(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := child.Scope().Resolve("provider"); err != nil || got != "root-provider" {
		t.Fatalf("child inherited provider = %#v, %v", got, err)
	}
	if len(rt.List()) != 2 {
		t.Fatalf("registry list length = %d, want 2", len(rt.List()))
	}
	if err := rt.Close(child.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Lookup(child.ID()); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("closed child lookup error = %v", err)
	}
}

func TestRegistryParentCloseCascadesToPublishedChildren(t *testing.T) {
	rt := NewRegistry()
	parent, err := rt.Create(Options{ID: "parent-cascade", Runner: func(context.Context, *Agent, TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	child, err := rt.Create(Options{ID: "child-cascade", ParentID: parent.ID(), Runner: func(ctx context.Context, _ *Agent, _ TurnInput) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if child.Status() != StatusClosed {
		t.Fatalf("child status after parent close = %s, want closed", child.Status())
	}
	if _, err := rt.Lookup(child.ID()); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("child remained published after parent close: %v", err)
	}
	if err := rt.CloseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCloseAllPreventsConcurrentRestartPublication(t *testing.T) {
	registry := NewRegistry()
	if err := registry.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := registry.CloseAll(); err != nil {
		t.Fatalf("second CloseAll = %v, want idempotent close", err)
	}
	if _, err := registry.Create(Options{
		ID:     "after-close",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Create after CloseAll = %v, want ErrRegistryClosed", err)
	}
}

func TestRegistryCreateRollsBackWhenParentScopeAlreadyClosed(t *testing.T) {
	registry := NewRegistry()
	parent, err := registry.Create(Options{
		ID:     "closed-parent",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = registry.Create(Options{
		ID:       "orphan-child",
		ParentID: parent.ID(),
		Runner:   func(context.Context, *Agent, TurnInput) error { return nil },
	})
	if !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("Create with closed parent scope = %v, want ErrScopeClosed", err)
	}
	if _, lookupErr := registry.Lookup("orphan-child"); !errors.Is(lookupErr, ErrAgentNotFound) {
		t.Fatalf("rolled-back child lookup = %v, want ErrAgentNotFound", lookupErr)
	}
	if err := registry.CloseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryConcurrentCreateAndCloseAllHasTerminalBarrier(t *testing.T) {
	registry := NewRegistry()
	const count = 64
	start := make(chan struct{})
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		id := ID(fmt.Sprintf("concurrent-%d", i))
		go func() {
			<-start
			_, err := registry.Create(Options{
				ID:     id,
				Runner: func(context.Context, *Agent, TurnInput) error { return nil },
			})
			if err != nil && !errors.Is(err, ErrRegistryClosed) {
				results <- err
				return
			}
			results <- nil
		}()
	}
	close(start)
	if err := registry.CloseAll(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Create = %v", err)
		}
	}
	if got := len(registry.List()); got != 0 {
		t.Fatalf("published agents after CloseAll barrier = %d, want 0", got)
	}
	if _, err := registry.Create(Options{
		ID:     "after-concurrent-close",
		Runner: func(context.Context, *Agent, TurnInput) error { return nil },
	}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Create after concurrent CloseAll = %v, want ErrRegistryClosed", err)
	}
}

func TestAgentRunSubmitsAndWaitsForTurnResult(t *testing.T) {
	rt := NewRegistry()
	defer func() { _ = rt.CloseAll() }()
	seen := make(chan string, 1)
	handle, err := rt.Create(Options{
		ID: "sync",
		Runner: func(_ context.Context, _ *Agent, input TurnInput) error {
			seen <- input.Messages[0].Text
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if got != "hello" {
			t.Fatalf("runner input = %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not receive synchronous input")
	}
	if err := handle.Start(context.Background()); !errors.Is(err, ErrAgentStarted) {
		t.Fatalf("second Start error = %v, want ErrAgentStarted", err)
	}
}

func TestAgentCancelClearsQueuedWorkUnlessKeepInbox(t *testing.T) {
	registry := NewRegistry()
	defer func() { _ = registry.CloseAll() }()
	runs := make(chan string, 2)
	handle, err := registry.Create(Options{
		ID: "cancel-queue",
		Runner: func(_ context.Context, _ *Agent, input TurnInput) error {
			runs <- input.Messages[0].Text
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Followup("discarded", nil); err != nil {
		t.Fatal(err)
	}
	if err := handle.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-runs:
		t.Fatalf("default cancel left queued work %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	if err := handle.Followup("preserved", nil); err != nil {
		t.Fatal(err)
	}
	if err := handle.CancelWithOptions(CancelOptions{KeepInbox: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-runs:
		if got != "preserved" {
			t.Fatalf("keep-inbox run = %q, want preserved", got)
		}
	case <-time.After(time.Second):
		t.Fatal("keep-inbox queued work did not run")
	}
}

func TestCanceledQueuedRunDoesNotCancelEarlierActiveTurn(t *testing.T) {
	registry := NewRegistry()
	defer func() { _ = registry.CloseAll() }()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	canceled := make(chan struct{}, 1)
	handle, err := registry.Create(Options{
		ID: "queued-run-cancel",
		Runner: func(ctx context.Context, _ *Agent, input TurnInput) error {
			if input.Messages[0].Text == "active" {
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					canceled <- struct{}{}
					return ctx.Err()
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Send("active", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active turn did not start")
	}

	queuedCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- handle.Run(queuedCtx, "queued", nil) }()
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("queued Run error = %v, want context.Canceled", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled queued Run remained blocked")
	}
	select {
	case <-canceled:
		t.Fatal("canceling queued Run canceled the earlier active turn")
	default:
	}
	close(release)
	if err := handle.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInboxSteeringRetainsDistinctKindAndClaimStepDoesNotConsumeFollowup(t *testing.T) {
	inbox := NewInbox()
	steer, err := inbox.Steer("redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	if steer.Kind != MessageSteering {
		t.Fatalf("steering kind = %q, want %q", steer.Kind, MessageSteering)
	}
	followup, err := inbox.Followup("later", nil)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := inbox.ClaimStep()
	if !ok || len(step.Messages) != 1 || step.Messages[0].ID != steer.ID {
		t.Fatalf("step claim = %+v, ok=%v", step, ok)
	}
	turn, ok := inbox.Claim()
	if !ok || len(turn.Messages) != 1 || turn.Messages[0].ID != followup.ID {
		t.Fatalf("turn claim = %+v, ok=%v", turn, ok)
	}
}

func TestAgentParentCancellationClosesRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	registry := NewRegistry()
	handle, err := registry.Create(Options{ID: "parent-cancel", Runner: func(context.Context, *Agent, TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	defer func() { _ = registry.CloseAll() }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for handle.Status() != StatusClosed {
		select {
		case <-deadline.C:
			t.Fatalf("agent status = %q after parent cancellation", handle.Status())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentParentCancellationDisposesScopeAndWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := NewRegistry()
	cleaned := make(chan struct{}, 1)
	active := make(chan struct{}, 1)
	handle, err := registry.Create(Options{ID: "parent-cancel-waiter", Runner: func(context.Context, *Agent, TurnInput) error {
		active <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Scope().AddCleanup(func() error { cleaned <- struct{}{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := handle.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := handle.Send("active", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-active:
	case <-time.After(time.Second):
		t.Fatal("active run did not start")
	}
	done := make(chan error, 1)
	go func() { done <- handle.Run(context.Background(), "queued", nil) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAgentClosed) {
			t.Fatalf("queued Run error = %v, want ErrAgentClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued Run remained blocked after parent cancellation")
	}
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("agent scope disposer did not run")
	}
	_ = registry.CloseAll()
}
