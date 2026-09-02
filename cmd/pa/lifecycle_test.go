package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/store"
)

func TestAppShutdownClosesAdmissionBeforeOwnedServices(t *testing.T) {
	a := &app{}
	if err := a.requireRunning(); err != nil {
		t.Fatalf("fresh app admission = %v", err)
	}
	a.beginShutdown()
	a.beginShutdown()
	if !errors.Is(a.requireRunning(), errAppShuttingDown) {
		t.Fatal("shutdown gate did not reject new work")
	}
	if _, err := a.sessionAgent("late-session"); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("late Agent creation = %v, want shutdown error", err)
	}
	if err := a.runTurnFor(context.Background(), "late-session", "hello", false); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("late turn = %v, want shutdown error", err)
	}
}

func TestRunTurnRequiresAgentOwnedRuntime(t *testing.T) {
	a := &app{}
	if err := a.runTurnFor(context.Background(), "session", "hello", false); err == nil || !strings.Contains(err.Error(), "agent runtime is unavailable") {
		t.Fatalf("turn without Agent registry = %v, want fail-closed runtime error", err)
	}
}

func TestSessionAgentMemoIsRemovedWhenRegistryClosesAgent(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.CreateSession(context.Background(), "memo-session", time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	a := &app{
		store:         st,
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
	}
	handle, err := a.sessionAgent("memo-session")
	if err != nil {
		t.Fatalf("materialize Agent: %v", err)
	}
	if a.sessionAgents["memo-session"] != handle {
		t.Fatal("session Agent was not memoized")
	}
	if err := a.agentRegistry.Close(handle.ID()); err != nil {
		t.Fatalf("close Agent: %v", err)
	}
	if _, ok := a.sessionAgents["memo-session"]; ok {
		t.Fatal("closed Agent remained in sessionAgents memo")
	}
	if err := a.agentRegistry.CloseAll(); err != nil {
		t.Fatalf("close registry: %v", err)
	}
}

func TestSessionAgentReplacesStaleClosedMemo(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.CreateSession(context.Background(), "stale-memo-session", time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	a := &app{
		store:         st,
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
	}
	closed, err := a.sessionAgent("stale-memo-session")
	if err != nil {
		t.Fatalf("materialize initial Agent: %v", err)
	}
	if err := a.agentRegistry.Close(closed.ID()); err != nil {
		t.Fatalf("close initial Agent: %v", err)
	}
	// Simulate a legacy or racing close path that left the app memo behind.
	a.agentMu.Lock()
	a.sessionAgents["stale-memo-session"] = closed
	a.agentMu.Unlock()

	refreshed, err := a.sessionAgent("stale-memo-session")
	if err != nil {
		t.Fatalf("rematerialize stale Agent: %v", err)
	}
	if refreshed == closed {
		t.Fatal("stale closed Agent was returned")
	}
	if refreshed.Status() == agent.StatusClosed {
		t.Fatal("rematerialized Agent is already closed")
	}
	if err := a.agentRegistry.CloseAll(); err != nil {
		t.Fatalf("close registry: %v", err)
	}
}

// TestSessionAgentStartFailureRollsBackOwnedPublication injects the one
// externally observable publication failure boundary. Agent registry insertion
// and owner cleanups precede Start; a failed Start must dispose the unpublished
// handle, run its job-owner cleanup, and leave no app-side ghost memo.
func TestSessionAgentStartFailureRollsBackOwnedPublication(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	const sessionID = "failed-start"
	if err := st.CreateSession(context.Background(), sessionID, time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	jobRegistry := jobs.NewLocal(jobs.LocalOpts{})
	release := make(chan struct{})
	jobID, err := jobRegistry.Start(context.Background(), jobs.JobStart{
		Kind: jobs.Kind("bash"), Label: "owned by failed agent", OwnerSession: sessionID,
		Run: func(ctx context.Context) (jobs.JobOutcome, error) {
			<-ctx.Done()
			return jobs.JobOutcome{Status: jobs.StatusKilled, Detail: ctx.Err().Error()}, ctx.Err()
		},
		Cancel: func(string) error { close(release); return nil },
	})
	if err != nil {
		t.Fatalf("start owned job: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	a := &app{
		store:         st,
		jobs:          jobRegistry,
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
		baseCtx:       canceled,
	}
	if _, err := a.sessionAgent(sessionID); err == nil {
		t.Fatal("Agent Start with canceled context unexpectedly succeeded")
	}
	if _, ok := a.sessionAgents[sessionID]; ok {
		t.Fatal("failed Agent remained in the session memo")
	}
	snaps, err := jobRegistry.List(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("list jobs after failed publication: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("job owner survived failed Agent publication: %+v", snaps)
	}
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Fatal("owned job was not cancelled during rollback")
	}
	_ = jobID

	// The failed publication must not poison the durable session; a later
	// healthy request can materialize a fresh, live Agent.
	a.baseCtx = context.Background()
	refreshed, err := a.sessionAgent(sessionID)
	if err != nil {
		t.Fatalf("materialize Agent after failed start: %v", err)
	}
	if refreshed.Status() == agent.StatusClosed {
		t.Fatal("rematerialized Agent is already closed")
	}
	if err := a.agentRegistry.CloseAll(); err != nil {
		t.Fatalf("close registry: %v", err)
	}
}
