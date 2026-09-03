package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

// Compile-time assertion: Local implements the Registry Service.
var _ Registry = (*Local)(nil)

func TestLocalCapturesCorrelationBeforeBackgroundRun(t *testing.T) {
	started := make(chan JobSnapshot, 1)
	runSawCorrelation := make(chan runtimectx.Correlation, 1)
	l := NewLocal(LocalOpts{OnStarted: func(snap JobSnapshot) { started <- snap }})
	defer l.Close()
	want := runtimectx.Correlation{AgentID: "agent-1", SessionID: "session-1", TurnID: "turn:2", StepID: "step:3", RequestID: "req-4", CallID: "call-5", GenerationID: "gen-6"}
	id, err := l.Start(context.Background(), JobStart{
		Kind: "test", Label: "correlated", OwnerSession: want.SessionID, Correlation: want,
		Run: func(ctx context.Context) (JobOutcome, error) {
			got, ok := runtimectx.CorrelationOf(ctx)
			if !ok {
				t.Errorf("job context has no correlation")
			} else {
				runSawCorrelation <- got
			}
			return JobOutcome{Status: StatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	startedSnap := <-started
	if startedSnap.ID != id || startedSnap.Correlation != want {
		t.Fatalf("started snapshot = %+v, want id=%s correlation=%+v", startedSnap, id, want)
	}
	if got := (<-runSawCorrelation); got != want {
		t.Fatalf("run correlation = %+v, want %+v", got, want)
	}
	snap := waitForTerminal(t, l, id, want.SessionID, time.Second)
	if snap.Correlation != want {
		t.Fatalf("terminal correlation = %+v, want %+v", snap.Correlation, want)
	}
}

func TestLocalUsesDurableReservationForGeneratedIDs(t *testing.T) {
	calls := 0
	l := NewLocal(LocalOpts{ReserveID: func(context.Context, string, string) (bool, error) {
		calls++
		return calls > 1, nil
	}})
	defer l.Close()
	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "reserved", Run: func(context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "bash-2" || calls != 2 {
		t.Fatalf("id=%q reservation calls=%d, want bash-2 and 2 calls", id, calls)
	}
}

// gateRun is a Run body that settles as completed when its gate closes or
// killed when its context is cancelled. The ctx branch guarantees every
// started job settles under Close, so no test ever leaks a goroutine.
func gateRun(gate <-chan struct{}) func(ctx context.Context) (JobOutcome, error) {
	return func(ctx context.Context) (JobOutcome, error) {
		select {
		case <-gate:
			return JobOutcome{Status: StatusCompleted, Detail: "done"}, nil
		case <-ctx.Done():
			return JobOutcome{Status: StatusKilled, Detail: "cancelled"}, nil
		}
	}
}

// mustStart starts a gated job and returns its id, failing the test on error.
func mustStart(t *testing.T, l *Local, kind Kind, owner string, gate <-chan struct{}) string {
	t.Helper()
	id, err := l.Start(context.Background(), JobStart{
		Kind:         kind,
		Label:        "job",
		OwnerSession: owner,
		Run:          gateRun(gate),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}

// waitForTerminal polls Get until the job settles or the deadline elapses.
func waitForTerminal(t *testing.T, l *Local, id, caller string, timeout time.Duration) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snap, err := l.Get(context.Background(), id, caller)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if isTerminal(snap.Status) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s not terminal within %s; last status = %q", id, timeout, snap.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- start -> list/get -------------------------------------------------------

func TestLocalStartListGet(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	started := make(chan struct{})
	id, err := l.Start(context.Background(), JobStart{
		Kind:         "bash",
		Label:        "sleep 1",
		OwnerSession: "sess-1",
		Run: func(ctx context.Context) (JobOutcome, error) {
			close(started)
			return gateRun(make(chan struct{}))(ctx)
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	if id != "bash-1" {
		t.Fatalf("id = %q, want bash-1", id)
	}

	snap, err := l.Get(context.Background(), id, "sess-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.Status != StatusRunning {
		t.Fatalf("status = %q, want running", snap.Status)
	}
	if snap.OwnerSession != "sess-1" || snap.Kind != "bash" || snap.Label != "sleep 1" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.FinishedAt != nil {
		t.Fatalf("FinishedAt = %v, want nil while running", snap.FinishedAt)
	}

	list, err := l.List(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("List = %+v, want exactly [%s]", list, id)
	}
}

func TestLocalCompletionObserverReceivesFirstSettledSnapshot(t *testing.T) {
	notices := make(chan struct {
		snapshot JobSnapshot
		output   string
	}, 2)
	l := NewLocal(LocalOpts{OnSettled: func(snapshot JobSnapshot, output string) {
		notices <- struct {
			snapshot JobSnapshot
			output   string
		}{snapshot: snapshot, output: output}
	}})
	defer l.Close()

	id, err := l.Start(context.Background(), JobStart{
		Kind: "schedule", Label: "reminder", OwnerSession: "sess-owner",
		OutputLimitBytes: 5,
		Run: func(context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusCompleted, Detail: "fired", Output: "abcdef"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case notice := <-notices:
		if notice.snapshot.ID != id || notice.snapshot.OwnerSession != "sess-owner" {
			t.Fatalf("notice snapshot = %+v, want id=%s owner=sess-owner", notice.snapshot, id)
		}
		if notice.snapshot.Status != StatusCompleted || notice.output != capOutput("abcdef", 5) {
			t.Fatalf("notice = %+v output=%q, want completed and bounded output", notice.snapshot, notice.output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion observer was not called")
	}
	select {
	case duplicate := <-notices:
		t.Fatalf("completion observer called more than once: %+v", duplicate)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestLocalCloseWaitsForCompletionObserver(t *testing.T) {
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	closeReturned := make(chan struct{})
	l := NewLocal(LocalOpts{OnSettled: func(JobSnapshot, string) {
		close(observerEntered)
		<-releaseObserver
	}})

	_, err := l.Start(context.Background(), JobStart{
		Kind: "schedule", Label: "quiesce", OwnerSession: "sess-owner",
		Run: func(context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-observerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("completion observer was not entered")
	}
	go func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		close(closeReturned)
	}()

	select {
	case <-closeReturned:
		t.Fatal("Close returned before completion observer finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseObserver)
	select {
	case <-closeReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after completion observer finished")
	}
}

func TestLocalIDSequence(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	if id := mustStart(t, l, "bash", "s", make(chan struct{})); id != "bash-1" {
		t.Fatalf("first bash id = %q, want bash-1", id)
	}
	if id := mustStart(t, l, "bash", "s", make(chan struct{})); id != "bash-2" {
		t.Fatalf("second bash id = %q, want bash-2", id)
	}
	if id := mustStart(t, l, "subagent", "s", make(chan struct{})); id != "subagent-1" {
		t.Fatalf("first subagent id = %q, want subagent-1", id)
	}
}

// --- status transitions ------------------------------------------------------

func TestLocalStatusCompleted(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "ok", OwnerSession: "s",
		Run: func(ctx context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusCompleted, Detail: "exit code: 0", Output: "hello"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	snap := waitForTerminal(t, l, id, "s", 2*time.Second)
	if snap.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", snap.Status)
	}
	if snap.Detail != "exit code: 0" {
		t.Fatalf("detail = %q, want exit code: 0", snap.Detail)
	}
	if snap.FinishedAt == nil {
		t.Fatalf("FinishedAt is nil for a terminal job")
	}
	// Windows clock granularity can make StartedAt and FinishedAt share one
	// tick for an instant job, so only require FinishedAt to be no earlier.
	if snap.FinishedAt.Before(snap.StartedAt) {
		t.Fatalf("FinishedAt %v before StartedAt %v", snap.FinishedAt, snap.StartedAt)
	}

	text, _, err := l.Read(context.Background(), id, "s")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if text != "hello" {
		t.Fatalf("Read output = %q, want hello", text)
	}
}

func TestLocalFailedOnRunError(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "boom", OwnerSession: "s",
		Run: func(ctx context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusCompleted}, errors.New("exploded")
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	snap := waitForTerminal(t, l, id, "s", 2*time.Second)
	if snap.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", snap.Status)
	}
	if snap.Detail != "exploded" {
		t.Fatalf("detail = %q, want exploded", snap.Detail)
	}
}

func TestLocalFailedOnPanic(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "panic", OwnerSession: "s",
		Run: func(ctx context.Context) (JobOutcome, error) {
			panic("kaboom")
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	snap := waitForTerminal(t, l, id, "s", 2*time.Second)
	if snap.Status != StatusFailed {
		t.Fatalf("status = %q, want failed (panic contained)", snap.Status)
	}
	if !strings.Contains(snap.Detail, "panic: kaboom") {
		t.Fatalf("detail = %q, want to mention the panic", snap.Detail)
	}
}

func TestLocalFailedOnInvalidOutcomeStatus(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "bad-outcome", OwnerSession: "s",
		Run: func(ctx context.Context) (JobOutcome, error) {
			return JobOutcome{Status: StatusRunning}, nil // not a terminal status
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	snap := waitForTerminal(t, l, id, "s", 2*time.Second)
	if snap.Status != StatusFailed {
		t.Fatalf("status = %q, want failed for a non-terminal outcome", snap.Status)
	}
}

// --- kill --------------------------------------------------------------------

func TestLocalKillTransitionsToKilled(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id := mustStart(t, l, "bash", "s", make(chan struct{}))
	if snap, _ := l.Get(context.Background(), id, "s"); snap.Status != StatusRunning {
		t.Fatalf("pre-kill status = %q, want running", snap.Status)
	}

	res, err := l.Kill(context.Background(), id, "s", "user asked")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if res != "requested" {
		t.Fatalf("Kill returned %q, want requested", res)
	}

	snap := waitForTerminal(t, l, id, "s", 2*time.Second)
	if snap.Status != StatusKilled {
		t.Fatalf("status after kill = %q, want killed", snap.Status)
	}

	// Kill on a terminal job is idempotent.
	res, err = l.Kill(context.Background(), id, "s", "again")
	if err != nil {
		t.Fatalf("second Kill: %v", err)
	}
	if res != "already-finished" {
		t.Fatalf("second Kill returned %q, want already-finished", res)
	}
}

func TestLocalKillAlreadyFinished(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "s", gate)
	close(gate)
	waitForTerminal(t, l, id, "s", 2*time.Second)

	res, err := l.Kill(context.Background(), id, "s", "too late")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if res != "already-finished" {
		t.Fatalf("Kill returned %q, want already-finished", res)
	}
}

func TestLocalKillInvokesCancelHook(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	gotReason := make(chan string, 1)
	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "x", OwnerSession: "s",
		Run: gateRun(make(chan struct{})),
		Cancel: func(reason string) error {
			gotReason <- reason
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := l.Kill(context.Background(), id, "s", "user requested")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if res != "requested" {
		t.Fatalf("Kill returned %q, want requested", res)
	}
	select {
	case reason := <-gotReason:
		if reason != "user requested" {
			t.Fatalf("Cancel hook reason = %q, want user requested", reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("Cancel hook was not invoked")
	}
	waitForTerminal(t, l, id, "s", 2*time.Second)
}

// --- wait --------------------------------------------------------------------

func TestLocalWaitTimeoutReturnsSnapshot(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id := mustStart(t, l, "bash", "s", make(chan struct{}))
	timeout := 40 * time.Millisecond

	start := time.Now()
	snap, err := l.Wait(context.Background(), id, "s", timeout)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if snap.Status != StatusRunning {
		t.Fatalf("snapshot status = %q, want running (timeout path)", snap.Status)
	}
	if elapsed < timeout {
		t.Fatalf("Wait returned early: %s < %s", elapsed, timeout)
	}
}

func TestLocalWaitSettled(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "s", gate)

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(gate)
	}()
	snap, err := l.Wait(context.Background(), id, "s", 2*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if snap.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", snap.Status)
	}
}

func TestLocalWaitImmediateTerminal(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "s", gate)
	close(gate)
	waitForTerminal(t, l, id, "s", 2*time.Second)

	snap, err := l.Wait(context.Background(), id, "s", time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if snap.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", snap.Status)
	}
}

func TestLocalWaitCtxCancel(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id := mustStart(t, l, "bash", "s", make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := l.Wait(ctx, id, "s", 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
}

// --- owner isolation ---------------------------------------------------------

func TestLocalOwnerIsolation(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id := mustStart(t, l, "bash", "alice", make(chan struct{}))

	ctx := context.Background()
	if _, err := l.Get(ctx, id, "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner Get error = %v, want ErrForbidden", err)
	}
	if _, _, err := l.Read(ctx, id, "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner Read error = %v, want ErrForbidden", err)
	}
	if _, err := l.Kill(ctx, id, "bob", "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner Kill error = %v, want ErrForbidden", err)
	}
	if _, err := l.Wait(ctx, id, "bob", 10*time.Millisecond); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner Wait error = %v, want ErrForbidden", err)
	}
	// An owned job is not visible to a session-less caller either.
	if _, err := l.Get(ctx, id, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("session-less Get error = %v, want ErrForbidden", err)
	}

	list, err := l.List(ctx, "bob")
	if err != nil {
		t.Fatalf("List(bob): %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List(bob) = %+v, want empty (owner fenced)", list)
	}

	// The owner can still observe and kill her own job.
	if snap, err := l.Get(ctx, id, "alice"); err != nil || snap.Status != StatusRunning {
		t.Fatalf("owner Get: snap=%+v err=%v, want running", snap, err)
	}
	if res, err := l.Kill(ctx, id, "alice", "cleanup"); err != nil || res != "requested" {
		t.Fatalf("owner Kill: res=%q err=%v, want requested", res, err)
	}
	waitForTerminal(t, l, id, "alice", 2*time.Second)
}

func TestLocalUnownedOpenToAnyone(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	id := mustStart(t, l, "daemon", "", make(chan struct{})) // no owner

	ctx := context.Background()
	if snap, err := l.Get(ctx, id, "anyone"); err != nil || snap.Status != StatusRunning {
		t.Fatalf("Get(anyone): snap=%+v err=%v, want running", snap, err)
	}
	if _, err := l.Get(ctx, id, ""); err != nil {
		t.Fatalf("Get(\"\"): %v, want open to a session-less caller", err)
	}
	list, err := l.List(ctx, "anyone")
	if err != nil {
		t.Fatalf("List(anyone): %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("List(anyone) = %+v, want the unowned job", list)
	}
}

// --- concurrency limit -------------------------------------------------------

func TestLocalConcurrencyLimitPerOwner(t *testing.T) {
	l := NewLocal(LocalOpts{MaxConcurrentJobsPerOwner: 2})
	defer l.Close()

	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	ids := make([]string, 2)
	for i := 0; i < 2; i++ {
		ids[i] = mustStart(t, l, "bash", "a", gates[i])
	}

	// A third concurrent job for the same owner is rejected.
	_, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "j", OwnerSession: "a", Run: gateRun(make(chan struct{})),
	})
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("over-limit Start error = %v, want ErrLimitReached", err)
	}

	// A different owner has its own bucket and is unaffected.
	mustStart(t, l, "bash", "b", make(chan struct{}))

	// Terminal settlement releases the owner's slot.
	close(gates[0])
	waitForTerminal(t, l, ids[0], "a", 2*time.Second)
	if _, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "j", OwnerSession: "a", Run: gateRun(make(chan struct{})),
	}); err != nil {
		t.Fatalf("Start after release: %v, want success", err)
	}
}

func TestLocalConcurrencyLimitSharedUnownedBucket(t *testing.T) {
	l := NewLocal(LocalOpts{MaxConcurrentJobsPerOwner: 2})
	defer l.Close()

	mustStart(t, l, "daemon", "", make(chan struct{}))
	mustStart(t, l, "daemon", "", make(chan struct{}))

	// The shared unowned bucket is full: a third unowned job is rejected…
	_, err := l.Start(context.Background(), JobStart{
		Kind: "daemon", Label: "j", OwnerSession: "", Run: gateRun(make(chan struct{})),
	})
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("over-limit unowned Start error = %v, want ErrLimitReached", err)
	}
	// …while an owned job of any owner still has room.
	mustStart(t, l, "bash", "x", make(chan struct{}))
}

// --- output bound ------------------------------------------------------------

func TestLocalOutputTruncation(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()

	startWithOutput := func(limit int, output string) string {
		t.Helper()
		id, err := l.Start(context.Background(), JobStart{
			Kind: "bash", Label: "out", OwnerSession: "s", OutputLimitBytes: limit,
			Run: func(ctx context.Context) (JobOutcome, error) {
				return JobOutcome{Status: StatusCompleted, Detail: "ok", Output: output}, nil
			},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return id
	}

	// Oversized output is truncated to the cap with a notice, and the result
	// never exceeds the cap.
	big := strings.Repeat("x", 1000)
	id := startWithOutput(100, big)
	waitForTerminal(t, l, id, "s", 2*time.Second)
	text, snap, err := l.Read(context.Background(), id, "s")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(text) > 100 {
		t.Fatalf("truncated output length = %d, want <= 100", len(text))
	}
	if !strings.Contains(text, "[output truncated:") || !strings.Contains(text, " bytes omitted]") {
		t.Fatalf("truncated output missing notice: %q", text)
	}
	if snap.OutputLimitBytes != 100 {
		t.Fatalf("snapshot OutputLimitBytes = %d, want 100", snap.OutputLimitBytes)
	}
	// Read is idempotent: repeated reads return the same bounded text.
	again, _, err := l.Read(context.Background(), id, "s")
	if err != nil || again != text {
		t.Fatalf("second Read = %q err=%v, want identical to first", again, err)
	}

	// No cap: output is kept in full.
	id2 := startWithOutput(0, big)
	waitForTerminal(t, l, id2, "s", 2*time.Second)
	text2, _, err := l.Read(context.Background(), id2, "s")
	if err != nil {
		t.Fatalf("Read(unbounded): %v", err)
	}
	if text2 != big {
		t.Fatalf("unbounded output length = %d, want 1000", len(text2))
	}

	// Output under the cap is unchanged.
	id3 := startWithOutput(200, "short")
	waitForTerminal(t, l, id3, "s", 2*time.Second)
	text3, _, err := l.Read(context.Background(), id3, "s")
	if err != nil {
		t.Fatalf("Read(short): %v", err)
	}
	if text3 != "short" {
		t.Fatalf("short output = %q, want unchanged", text3)
	}
}

// --- close -------------------------------------------------------------------

func TestLocalCloseCancelsLiveJobs(t *testing.T) {
	l := NewLocal(LocalOpts{})

	ids := make([]string, 5)
	for i := range ids {
		ids[i] = mustStart(t, l, "bash", "s", make(chan struct{})) // gates never close
	}

	done := make(chan struct{})
	go func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Close did not return: live jobs were not cancelled (goroutine leak)")
	}

	// Every live job settled as killed — Close cancelled the work.
	for _, id := range ids {
		snap, err := l.Get(context.Background(), id, "s")
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if snap.Status != StatusKilled {
			t.Fatalf("job %s status after Close = %q, want killed", id, snap.Status)
		}
	}
}

func TestLocalCloseOwnerCancelsAwaitsAndRemovesOnlyOwnerJobs(t *testing.T) {
	l := NewLocal(LocalOpts{})
	ownerID := mustStart(t, l, "bash", "owner", make(chan struct{}))
	otherID := mustStart(t, l, "bash", "other", make(chan struct{}))
	unownedID := mustStart(t, l, "bash", "", make(chan struct{}))

	if err := l.CloseOwner("owner"); err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if _, err := l.Get(context.Background(), ownerID, "owner"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("owner job after CloseOwner = %v, want ErrUnknownJob", err)
	}
	for _, tc := range []struct {
		id    string
		owner string
	}{
		{otherID, "other"},
		{unownedID, "observer"},
	} {
		snap, err := l.Get(context.Background(), tc.id, tc.owner)
		if err != nil {
			t.Fatalf("Get(%s) after CloseOwner: %v", tc.id, err)
		}
		if snap.Status != StatusStopping && snap.Status != StatusRunning {
			t.Fatalf("job %s status after unrelated CloseOwner = %q", tc.id, snap.Status)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLocalCloseOwnerClosesAdmissionAndWaitsForObserver(t *testing.T) {
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	l := NewLocal(LocalOpts{})
	id, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "owner", OwnerSession: "owner",
		Run: func(ctx context.Context) (JobOutcome, error) {
			<-ctx.Done()
			return JobOutcome{Status: StatusKilled}, nil
		},
		OnSettled: func(JobSnapshot) {
			close(observerEntered)
			<-releaseObserver
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- l.CloseOwner("owner") }()
	<-observerEntered
	if _, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "late", OwnerSession: "owner",
		Run: func(context.Context) (JobOutcome, error) { return JobOutcome{Status: StatusCompleted}, nil },
	}); !errors.Is(err, ErrOwnerClosed) {
		t.Fatalf("late owner Start error = %v, want ErrOwnerClosed", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("CloseOwner returned before observer release: %v", err)
	default:
	}
	close(releaseObserver)
	if err := <-closeDone; err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if _, err := l.Get(context.Background(), id, "owner"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("owner job after CloseOwner = %v, want ErrUnknownJob", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLocalStartAfterCloseRejected(t *testing.T) {
	l := NewLocal(LocalOpts{})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := l.Start(context.Background(), JobStart{
		Kind: "bash", Label: "x", OwnerSession: "s", Run: gateRun(make(chan struct{})),
	})
	if !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Start after Close error = %v, want ErrRegistryClosed", err)
	}
}

func TestLocalCloseIdempotent(t *testing.T) {
	l := NewLocal(LocalOpts{})
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
}
