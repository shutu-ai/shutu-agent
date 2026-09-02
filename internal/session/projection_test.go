package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestProjectSessionListMetadataMatchesEngagementAndPromptRules(t *testing.T) {
	metadata := ProjectSessionListMetadata([]Event{
		{Seq: 1, Type: EventUserMessage, At: time.UnixMilli(1234), Data: json.RawMessage(`{"text":"before turn"}`)},
		{Seq: 2, Type: EventUserMessage, At: time.UnixMilli(2000), Data: json.RawMessage(`{"text":"team","source":{"kind":"team-message"}}`)},
	})
	if !metadata.Blank {
		t.Fatal("user/message without turn/start made session non-blank")
	}
	if metadata.LastPromptAt == nil || *metadata.LastPromptAt != 1234 {
		t.Fatalf("last prompt = %v, want 1234", metadata.LastPromptAt)
	}
	metadata.Apply(Event{Seq: 3, Type: EventTurnStart, At: time.UnixMilli(3000), Data: json.RawMessage(`{"turn":1}`)})
	if metadata.Blank {
		t.Fatal("turn/start left session blank")
	}
}
