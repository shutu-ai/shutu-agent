package main

import (
	"context"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

func TestRuntimeLogPrefersAgentOwnedLogWhenSessionMatchesCurrentID(t *testing.T) {
	legacy, target := session.New(), session.New()
	a := &app{currentID: "same", log: legacy, runtimeLogs: map[string]*session.Log{"same": target}}
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "same"})
	if got := a.runtimeLog(ctx); got != target {
		t.Fatalf("runtime log = %p, want Agent-owned log %p", got, target)
	}
}

func TestRuntimeLogFailsClosedForUnmaterializedAgentOverlap(t *testing.T) {
	legacy := session.New()
	a := &app{agentRegistry: agent.NewRegistry(), currentID: "same", log: legacy}
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "same"})
	if got := a.runtimeLog(ctx); got != nil {
		t.Fatalf("runtime log = %p, want nil while Agent-owned log is not materialized", got)
	}
}

func TestRuntimeOwnerDoesNotFallBackToLegacySelection(t *testing.T) {
	a := &app{agentRegistry: agent.NewRegistry(), currentID: "legacy"}
	if got := a.runtimeSessionID(context.Background()); got != "" {
		t.Fatalf("runtime session id = %q, want empty without Agent context", got)
	}
	if got := a.terminalOwner(context.Background()); got != "" {
		t.Fatalf("terminal owner = %q, want empty without Agent context", got)
	}
	if got := a.webSessionID(context.Background()); got != "" {
		t.Fatalf("web session id = %q, want empty without Agent context", got)
	}
	if got := a.webLog(context.Background()); got != nil {
		t.Fatalf("web log = %p, want nil without Agent context", got)
	}
}
