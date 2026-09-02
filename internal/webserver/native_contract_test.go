package webserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/contractfixture"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestNativeProjectionReplaysTheSharedCoreFixture(t *testing.T) {
	records, err := contractfixture.CoreTurnEvents()
	if err != nil {
		t.Fatalf("load shared fixture: %v", err)
	}
	events := make([]session.Event, 0, len(records))
	for _, record := range records {
		events = append(events, session.Event{Seq: record.Seq, Type: record.Type, At: time.UnixMilli(record.Time).UTC(), Version: session.EventVersion, Data: record.Data})
	}
	log := session.New()
	if err := log.Restore(events); err != nil {
		t.Fatalf("restore shared fixture: %v", err)
	}
	if err := session.ValidateLifecycle(log.Events()); err != nil {
		t.Fatalf("shared fixture lifecycle: %v", err)
	}
	if got := len(log.DeriveHistory()); got != 3 {
		t.Fatalf("shared fixture history length = %d, want 3", got)
	}
	cursor := newNativeProjectionCursor()
	for _, event := range events {
		projected := cursor.project("contract", event)
		raw, err := json.Marshal(projected)
		if err != nil {
			t.Fatalf("marshal projected %s: %v", event.Type, err)
		}
		if err := session.ValidateWireEvent(raw); err != nil {
			t.Fatalf("projected %s violates wire contract: %v (%s)", event.Type, err, raw)
		}
	}
}

func TestNativeCoreProjectionSatisfiesSessionWireContract(t *testing.T) {
	cursor := newNativeProjectionCursor()
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(2), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(3), Version: session.EventVersion, Data: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}],"source":{"kind":"user"}}`)},
		{Seq: 4, Type: session.EventAssistantChunk, At: time.UnixMilli(4), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"chunk":{"type":"text-delta","text":"ok"}}`)},
		{Seq: 5, Type: session.EventAssistantMessage, At: time.UnixMilli(5), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"source":{"kind":"model"}}}`)},
		{Seq: 6, Type: session.EventStepEnd, At: time.UnixMilli(6), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1}`)},
		{Seq: 7, Type: session.EventTurnEnd, At: time.UnixMilli(7), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	for _, event := range events {
		projected := cursor.project("contract", event)
		raw, err := json.Marshal(projected)
		if err != nil {
			t.Fatalf("marshal %s: %v", event.Type, err)
		}
		if err := session.ValidateWireEvent(raw); err != nil {
			t.Fatalf("projected %s violates wire contract: %v (%s)", event.Type, err, raw)
		}
	}
}
