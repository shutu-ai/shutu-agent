package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/webserver"
)

func TestNativeGoalMutationUsesAddressedSessionRuntime(t *testing.T) {
	legacy := session.New()
	target := session.New()
	a := &app{
		currentID: "repl-session",
		log:       legacy,
		runtimeLogs: map[string]*session.Log{
			"web-session": target,
		},
		plans: plan.NewEngine(plan.NewMemProvider()),
	}
	defer a.plans.Close()

	objective := "ship the addressed goal"
	result, err := a.nativeGoalMutation(context.Background(), webserver.NativeGoalMutation{
		Action: "goal.create", SessionID: "web-session", Objective: &objective,
	})
	if err != nil {
		t.Fatalf("addressed goal.create: %v", err)
	}
	if result.GoalID == "" || result.Revision == 0 {
		t.Fatalf("goal result = %+v, want durable goal identity and revision", result)
	}
	if len(legacy.Events()) != 0 {
		t.Fatalf("legacy current session was mutated: %+v", legacy.Events())
	}
	if len(target.Events()) != 1 || target.Events()[0].Type != session.EventPlanCreate {
		t.Fatalf("addressed session events = %+v, want one plan/create", target.Events())
	}
	if strings.Contains(string(target.Events()[0].Data), "repl-session") {
		t.Fatalf("addressed event leaked legacy session identity: %s", target.Events()[0].Data)
	}
}
