package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

func TestPlanModeRuntimeCommitsIdleAndAppliesQueuedSelection(t *testing.T) {
	a := &app{cfg: config.Config{}, reg: tools.New(), log: session.New(), currentID: "plan-runtime"}
	a.agentRegistry = agent.NewRegistry()

	action, err := a.setPlanModeFor(context.Background(), a.currentID, a.log, true)
	if err != nil || action != planModeCommitted {
		t.Fatalf("idle set = %q, err=%v; want committed", action, err)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("idle plan mode was not committed")
	}

	if _, err := a.log.Append(session.EventTurnStart, map[string]any{"turn": 1}); err != nil {
		t.Fatal(err)
	}
	action, err = a.setPlanModeFor(context.Background(), a.currentID, a.log, false)
	if err != nil || action != planModeQueued {
		t.Fatalf("in-turn set = %q, err=%v; want queued", action, err)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("queued selection changed durable mode before boundary")
	}
	if err := a.applyPendingPlanMode(a.currentID, a.log); err != nil {
		t.Fatalf("apply pending: %v", err)
	}
	if session.FoldPlanMode(a.log.Events()) {
		t.Fatal("queued plan mode was not applied at boundary")
	}
}

func TestCurrentPlanModeActiveUsesStrictCanonicalProjection(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatal(err)
	}
	active, err := currentPlanModeActive(log)
	if err != nil || !active {
		t.Fatalf("projected plan mode = %v, err=%v; want active", active, err)
	}

	malformed := []session.Event{{
		Seq: 0, Type: session.EventPlanMode, Version: session.EventVersion,
		Data: json.RawMessage(`{"active":"yes"}`),
	}}
	if _, err := currentPlanModeActiveFromEvents(malformed); err == nil {
		t.Fatal("malformed plan-mode event was accepted by the canonical projection")
	}
}

func TestFoldPendingPlanModeReplaysSuccessfulCommandLifecycle(t *testing.T) {
	log := session.New()
	commandID := "plan-command-1"
	if _, err := log.Append(session.EventCommandRun, session.NewCommandRun(commandID, "plan", "enter release planning")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventCommandDone, session.NewCommandDone(commandID, "success", "")); err != nil {
		t.Fatal(err)
	}
	if target, ok := foldPendingPlanMode(log.Events()); !ok || !target {
		t.Fatalf("pending replay = %v, %v; want true, true", target, ok)
	}
	if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatal(err)
	}
	if target, ok := foldPendingPlanMode(log.Events()); ok {
		t.Fatalf("pending replay after mode commit = %v, %v; want no pending", target, ok)
	}
}

func TestFoldPendingPlanModeIgnoresFailedOrUnfinishedCommands(t *testing.T) {
	failed := session.New()
	if _, err := failed.Append(session.EventCommandRun, session.NewCommandRun("failed", "plan", "on")); err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Append(session.EventCommandDone, session.NewCommandDone("failed", "error", "rejected")); err != nil {
		t.Fatal(err)
	}
	if _, ok := foldPendingPlanMode(failed.Events()); ok {
		t.Fatal("failed plan command became pending")
	}

	unfinished := session.New()
	if _, err := unfinished.Append(session.EventCommandRun, session.NewCommandRun("unfinished", "plan", "on")); err != nil {
		t.Fatal(err)
	}
	if _, ok := foldPendingPlanMode(unfinished.Events()); ok {
		t.Fatal("unfinished plan command became pending")
	}
}
