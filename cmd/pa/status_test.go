package main

// status_test — dsh ui-workspace session-status alignment tests: the status
// provider computes the dot state (idle / done / ongoing / warning) from the
// runtime signals (running turn, running subagents, pending interaction,
// finished-but-unviewed). The app is exercised directly (no store), since
// sessionStatus reads only the passed metadata plus runtime state.

import (
	"context"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/store"
)

func TestSessionStatusIdle(t *testing.T) {
	a := &app{}
	st := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s1", EventCount: 0})
	if st.State != "idle" || len(st.Statuses) == 0 || st.Statuses[0].Label != "空闲" {
		t.Fatalf("idle status = %+v", st)
	}
}

func TestSessionStatusRunning(t *testing.T) {
	a := &app{}
	a.runningSession.Store("s1")
	st := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s1", EventCount: 2})
	if st.State != "ongoing" || st.Statuses[0].Label != "运行中" {
		t.Fatalf("running status = %+v", st)
	}
	// A different, non-running session must not inherit the running state.
	other := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s2", EventCount: 2})
	if other.State == "ongoing" {
		t.Fatalf("s2 must not be running (running session = s1)")
	}
}

func TestSessionStatusTracksConcurrentAgentRuns(t *testing.T) {
	a := &app{}
	a.beginSessionRun("s1")
	a.beginSessionRun("s2")
	if !a.isSessionRunning("s1") || !a.isSessionRunning("s2") {
		t.Fatalf("concurrent running sessions not retained: %#v", a.runningSessions)
	}
	a.endSessionRun("s1")
	if a.isSessionRunning("s1") || !a.isSessionRunning("s2") {
		t.Fatalf("ending s1 changed s2 state: %#v", a.runningSessions)
	}
	a.endSessionRun("s2")
	if a.isSessionRunning("s2") {
		t.Fatal("ended s2 remains running")
	}
}

func TestSessionStatusCompletedUnviewed(t *testing.T) {
	a := &app{}
	updated := time.Now().UTC().Add(-time.Minute)
	// Never viewed → finished-but-unviewed reminder.
	st := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s1", EventCount: 5, UpdatedAt: updated})
	if st.State != "done" || st.Statuses[0].Label != "已完成" {
		t.Fatalf("unviewed status = %+v", st)
	}
	// Viewed after the last activity → idle.
	st2 := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s2", EventCount: 5, UpdatedAt: updated, LastViewedAt: time.Now().UTC()})
	if st2.State != "idle" {
		t.Fatalf("viewed status = %+v, want idle", st2)
	}
}

func TestSessionStatusPendingInteraction(t *testing.T) {
	prov := interact.NewMemProvider()
	eng := interact.NewEngine(prov)
	if _, err := eng.Request(context.Background(), "allow run_command?", "bash", "{}"); err != nil {
		t.Fatal(err)
	}
	a := &app{interacts: eng}
	a.runningSession.Store("s1")
	st := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s1", EventCount: 2})
	if st.State != "warning" || len(st.Statuses) == 0 || st.Statuses[0].Label != "等待审批" {
		t.Fatalf("pending status = %+v", st)
	}
}

func TestSessionStatusIgnoresResolvedInteraction(t *testing.T) {
	prov := interact.NewMemProvider()
	eng := interact.NewEngine(prov)
	req, err := eng.Request(context.Background(), "allow run_command?", "bash", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Resolve(context.Background(), req.ID, interact.StatusApproved); err != nil {
		t.Fatal(err)
	}
	a := &app{interacts: eng}
	st := a.sessionStatus(context.Background(), store.SessionMeta{ID: "s1", EventCount: 2})
	if st.State == "warning" {
		t.Fatalf("resolved interaction still warns: %+v", st)
	}
}
