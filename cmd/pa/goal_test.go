package main

import (
	"context"
	"io"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
)

type goalFinishLLM struct {
	goalID string
	calls  int
}

func (l *goalFinishLLM) Stream(_ context.Context, _ llm.ChatRequest) (llm.StreamReader, error) {
	l.calls++
	if l.calls == 1 {
		return &goalReader{events: []llm.StreamEvent{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID:        "set-done",
				Name:      "update_goal",
				Arguments: `{"goal_id":"` + l.goalID + `","revision":2,"action":"complete"}`,
			}},
		}}}, nil
	}
	return &goalReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "goal complete"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

type goalReader struct {
	events []llm.StreamEvent
	index  int
}

func (r *goalReader) Next() (llm.StreamEvent, error) {
	if r.index >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.index]
	r.index++
	return ev, nil
}

func TestRunIdleGoalContinuesAfterOuterTurnWithoutRecursion(t *testing.T) {
	a := makePlanApp(true)
	a.cfg = config.Config{Model: "m", Plan: config.PlanConfig{Enabled: config.Bool(true)}}
	a.reg.SetPolicy(planPolicy())
	a.basePolicy = planPolicy()
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	a.currentID = "session-goal"
	a.prompt = makeTurnApp().prompt
	goal, err := a.plans.CreateGoal(context.Background(), "Ship", "ship the agent")
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := a.log.Append(session.EventPlanCreate, session.NewPlanCreate("goal", goal.ID, goal.Title, nil)); err != nil {
		t.Fatalf("append goal event: %v", err)
	}
	a.llm = &goalFinishLLM{goalID: goal.ID}

	if err := a.runIdleGoal(context.Background(), false); err != nil {
		t.Fatalf("runIdleGoal: %v", err)
	}
	goals, err := a.plans.List(context.Background())
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	if len(goals) != 1 || goals[0].Status != plan.StatusDone {
		t.Fatalf("goals = %+v, want one done goal", goals)
	}
	if a.llm.(*goalFinishLLM).calls != 2 {
		t.Fatalf("LLM calls = %d, want 2 (tool step + final step)", a.llm.(*goalFinishLLM).calls)
	}
	var starts, ends int
	for _, ev := range a.log.Events() {
		if ev.Type == session.EventGoalRoundStart {
			starts++
		}
		if ev.Type == session.EventGoalRoundEnd {
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("goal round events = start:%d end:%d, want 1/1", starts, ends)
	}
}
