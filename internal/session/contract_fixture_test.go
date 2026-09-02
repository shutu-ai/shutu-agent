package session

import (
	"testing"

	"github.com/jabing/shutu-agent/internal/contractfixture"
	"github.com/jabing/shutu-agent/internal/llm"
)

func TestCoreTurnReplayFixtureValidatesAndDerives(t *testing.T) {
	rawRecords, err := contractfixture.CoreTurnReplay()
	if err != nil {
		t.Fatalf("load raw fixture: %v", err)
	}
	for _, raw := range rawRecords {
		if err := ValidateWireEvent(raw); err != nil {
			t.Fatalf("fixture event rejected: %v\n%s", err, raw)
		}
	}
	events, err := contractfixture.CoreTurnEvents()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	log := New()
	for _, event := range events {
		if _, err := log.Append(event.Type, event.Data); err != nil {
			t.Fatalf("append fixture event: %v", err)
		}
	}
	history := log.DeriveHistory()
	if len(history) != 3 {
		t.Fatalf("derived history length = %d, want user/assistant/tool", len(history))
	}
	if history[0].Role != llm.RoleUser || history[1].Role != llm.RoleAssistant || history[2].Role != llm.RoleTool {
		t.Fatalf("derived roles = %#v", []llm.Role{history[0].Role, history[1].Role, history[2].Role})
	}
	if history[2].ToolCallID != "call-fixture" || history[2].Text() != "fixture output" {
		t.Fatalf("derived tool result = %+v", history[2])
	}
}
