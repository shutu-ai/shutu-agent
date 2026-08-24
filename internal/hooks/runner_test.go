package hooks

import (
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func TestPayloadIsMetadataOnly(t *testing.T) {
	ev := session.Event{Seq: 7, Type: session.EventToolResult, At: time.Unix(10, 0), Version: 1, Data: []byte(`{"output":"private secret","callId":"c1"}`)}
	payload := payload("s1", ev)
	if !strings.Contains(payload, `"session_id":"s1"`) || !strings.Contains(payload, `"type":"tool/result"`) {
		t.Fatalf("payload missing metadata: %s", payload)
	}
	if strings.Contains(payload, "private secret") || strings.Contains(payload, "callId") {
		t.Fatalf("payload leaked event data: %s", payload)
	}
}

func TestRunnerMatchesConfiguredEventsAndExcludesHookEvents(t *testing.T) {
	runner, err := New(Config{Command: "unused", Events: []string{"turn/end"}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if !runner.matches(session.EventTurnEnd) {
		t.Fatal("turn/end should match")
	}
	if runner.matches(session.EventToolResult) || runner.matches("hook/result") {
		t.Fatal("unexpected event matched")
	}
}

func TestNewRejectsEmptyCommand(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty command accepted")
	}
}
