package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestApprovalAnswererScopesAndCommitsForAllSurfaces(t *testing.T) {
	ctx := context.Background()
	eng := interact.NewEngine(nil)
	defer eng.Close()
	log := session.New()
	a := &app{currentID: "web-session", log: log, interacts: eng}

	first, err := eng.RequestForSession(ctx, "web-session", "approve", "danger", "{}")
	if err != nil {
		t.Fatal(err)
	}
	second, err := eng.RequestForSession(ctx, "web-session", "approve", "danger", "{}")
	if err != nil {
		t.Fatal(err)
	}
	a.rememberInteraction(first.ID, first.SessionID, first.CallID)
	a.rememberInteraction(second.ID, second.SessionID, second.CallID)

	answerer := a.approvalAnswerer()
	items, err := answerer.List(ctx, "web-session")
	if err != nil || len(items) != 2 || items[0].ID != first.ID || items[1].ID != second.ID {
		t.Fatalf("web list = %+v, err=%v", items, err)
	}
	if _, err := answerer.List(ctx, "other-session"); err != nil {
		t.Fatalf("other-session list leaked an engine error: %v", err)
	}
	if err := answerer.Resolve(ctx, first.SessionID, first.ID, interact.StatusAllowedOnce, "", false); err != nil {
		t.Fatalf("Web/ACP-style resolve: %v", err)
	}
	if err := answerer.Resolve(ctx, "other-session", second.ID, interact.StatusApproved, "", false); !errors.Is(err, interact.ErrUnknownRequest) {
		t.Fatalf("cross-session resolve = %v, want unknown request", err)
	}

	// The CLI compatibility flag changes only the historical event spelling;
	// it still traverses this same session-scoped resolver and durable boundary.
	if err := answerer.Resolve(ctx, second.SessionID, second.ID, interact.StatusApproved, "", true); err != nil {
		t.Fatalf("CLI-style resolve: %v", err)
	}
	if got := log.Events(); len(got) != 2 || got[0].Type != session.EventApprovalDecided || got[1].Type != session.EventInteractResolve {
		t.Fatalf("CLI compatibility events = %+v", got)
	}
}

func TestACPSharedAnswererUsesApplicationContract(t *testing.T) {
	ctx := context.Background()
	eng := interact.NewEngine(nil)
	defer eng.Close()
	log := session.New()
	a := &app{currentID: "acp-session", log: log, interacts: eng}
	req, err := eng.RequestForSession(ctx, "acp-session", "approve", "danger", "{}")
	if err != nil {
		t.Fatal(err)
	}
	a.rememberInteraction(req.ID, req.SessionID, req.CallID)
	s := &acpSession{app: a, id: "acp-session", log: log, approval: eng, sharedApproval: true}
	if err := s.resolveACPApproval(ctx, req, interact.StatusAllowedOnce, map[string]any{"id": req.ID, "outcome": "allowed-once"}); err != nil {
		t.Fatalf("ACP shared resolve: %v", err)
	}
	items, err := eng.ListForSession(ctx, "acp-session")
	if err != nil || len(items) != 1 || items[0].Status != interact.StatusAllowedOnce {
		t.Fatalf("ACP resolved request = %+v, err=%v", items, err)
	}
	if events := log.Events(); len(events) != 1 || events[0].Type != session.EventApprovalDecided {
		t.Fatalf("ACP durable events = %+v", events)
	}
}

func TestAgentApprovalDisposalUsesRuntimeLogWhenIDsOverlap(t *testing.T) {
	ctx := context.Background()
	eng := interact.NewEngine(nil)
	defer eng.Close()
	legacy := session.New()
	target := session.New()
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	a := &app{
		currentID:     "same-session",
		log:           legacy,
		interacts:     eng,
		agentRegistry: registry,
		runtimeLogs:   map[string]*session.Log{"same-session": target},
	}
	if _, err := eng.RequestForSession(ctx, "same-session", "approve", "danger", "{}"); err != nil {
		t.Fatal(err)
	}
	a.clearSessionApprovalPolicy("same-session")
	if got := legacy.Events(); len(got) != 0 {
		t.Fatalf("legacy log received Agent disposal event: %+v", got)
	}
	if got := target.Events(); len(got) != 1 || got[0].Type != session.EventApprovalDecided {
		t.Fatalf("runtime log disposal events = %+v", got)
	}
}
