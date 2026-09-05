package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

func TestNativeFeedbackCommandPersistsWithoutModelHistory(t *testing.T) {
	a := makePlanApp(true)
	a.currentID = "native-feedback"

	if err := a.command(context.Background(), "/feedback  keep this local "); err != nil {
		t.Fatalf("command: %v", err)
	}
	events := a.log.Events()
	if len(events) != 3 || events[0].Type != session.EventCommandRun || events[1].Type != session.EventFeedbackRecord || events[2].Type != session.EventCommandDone {
		t.Fatalf("events = %+v, want command/run + feedback/record + command/done", events)
	}
	if got := a.log.DeriveHistory(); len(got) != 0 {
		t.Fatalf("feedback entered model history: %+v", got)
	}
	var commandRun struct {
		Args string `json:"args"`
	}
	if err := json.Unmarshal(events[0].Data, &commandRun); err != nil {
		t.Fatal(err)
	}
	if commandRun.Args != "" {
		t.Fatalf("feedback command/run leaked input args = %q", commandRun.Args)
	}
}

func TestNativePlanCommandUsesSuffixAsTheTurn(t *testing.T) {
	a := makeTurnApp()
	a.currentID = "native-plan"

	if err := a.command(context.Background(), "/plan design the change"); err != nil {
		t.Fatalf("command: %v", err)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("/plan did not persist plan mode")
	}
	history := a.log.DeriveHistory()
	if len(history) == 0 || history[0].Text() != "design the change" {
		t.Fatalf("history = %+v, want suffix as first user message", history)
	}
	for _, event := range a.log.Events() {
		if event.Type == session.EventUserMessage && string(event.Data) == "{\"text\":\"/plan design the change\"}" {
			t.Fatal("the /plan command text was sent to the model")
		}
	}

	if err := a.command(context.Background(), "/plan off"); err != nil {
		t.Fatalf("/plan off: %v", err)
	}
	if session.FoldPlanMode(a.log.Events()) {
		t.Fatal("/plan off did not deactivate plan mode")
	}
}

func TestNativeCommandRejectsBlankInput(t *testing.T) {
	a := makePlanApp(true)
	if err := a.command(context.Background(), "   "); err == nil {
		t.Fatal("blank command was accepted")
	}
}
