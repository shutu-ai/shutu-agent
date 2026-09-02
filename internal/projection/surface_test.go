package projection

import (
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestClassifyEventSurfacesMatchesCanonicalReplacementFold(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("old question")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage("old answer", nil, "stop")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventLLMRequestEnd, map[string]any{"status": "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageReplaceWithSources("summary", 1, 2, []uint64{1, 2})); err != nil {
		t.Fatal(err)
	}

	classified, err := ClassifyEventSurfaces(log.Events())
	if err != nil {
		t.Fatalf("ClassifyEventSurfaces: %v", err)
	}
	if classified[1] != SurfaceShadowed || classified[2] != SurfaceShadowed {
		t.Fatalf("shadowed surfaces = %#v", classified)
	}
	if classified[3] != SurfaceLogOnly || classified[4] != SurfaceCurrent {
		t.Fatalf("current/log-only surfaces = %#v", classified)
	}
	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(snapshot.History) != 1 || snapshot.History[0].Text() != "summary" {
		t.Fatalf("history = %#v, want replacement summary", snapshot.History)
	}
}

func TestClassifyEventSurfacesRejectsInvalidDurableStream(t *testing.T) {
	_, err := ClassifyEventSurfaces([]session.Event{{Seq: 1, Type: "unknown/required", Data: []byte(`{}`)}})
	if err == nil {
		t.Fatal("invalid durable stream was accepted")
	}
}

func TestEventRelationsUsesValidatedSharedStream(t *testing.T) {
	log := session.New()
	appends := []struct {
		typ  string
		data any
	}{
		{session.EventUserMessage, session.NewUserMessage("old question")},
		{session.EventAssistantMessage, session.NewAssistantMessage("old answer", nil, "stop")},
		{session.EventUserMessage, session.NewUserMessageReplaceWithSources("summary", 1, 2, []uint64{1, 2})},
	}
	for _, item := range appends {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatalf("append %s: %v", item.typ, err)
		}
	}
	relations, err := EventRelations(log.Events(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if relations.ReplacedBy != 3 || len(relations.ReplacementChain) != 1 || relations.ReplacementChain[0] != 3 {
		t.Fatalf("replacement relations = %#v", relations)
	}
	if len(relations.Replaces) != 0 {
		t.Fatalf("replaced events = %#v", relations.Replaces)
	}
	relations, err = EventRelations(log.Events(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations.Replaces) != 2 || relations.Replaces[0] != 1 || relations.Replaces[1] != 2 {
		t.Fatalf("replaced events = %#v", relations.Replaces)
	}
	if _, err := EventRelations(append([]session.Event(nil), log.Events()...), 99); err == nil {
		t.Fatal("missing target event was accepted")
	}
}

func TestEventRelationsDerivesToolCallEdges(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage("", []llm.ToolCall{{ID: "call-1", Name: "lookup"}}, "tool-calls")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventToolStart, session.NewToolStart("call-1", "lookup", "{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventToolResult, session.NewToolResult("call-1", "lookup", "done", nil)); err != nil {
		t.Fatal(err)
	}
	start, err := EventRelations(log.Events(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Sources) != 1 || start.Sources[0] != 1 || len(start.Derived) != 1 || start.Derived[0] != 3 {
		t.Fatalf("tool-start relations = %#v", start)
	}
	result, err := EventRelations(log.Events(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0] != 2 || len(result.Derived) != 0 {
		t.Fatalf("tool-result relations = %#v", result)
	}
}

func TestEventRelationsRejectsInvalidDurableStream(t *testing.T) {
	events := []session.Event{{Seq: 1, Type: "unknown/required", Data: []byte(`{}`)}}
	if _, err := EventRelations(events, 1); err == nil {
		t.Fatal("relations accepted an invalid durable stream")
	}
}
