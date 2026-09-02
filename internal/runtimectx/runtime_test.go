package runtimectx

import (
	"context"
	"sync"
	"testing"
)

func TestRuntimeContextRoutesConcurrentEventsBySession(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	emit := func(id string) func(string, any) error {
		return func(typ string, _ any) error {
			mu.Lock()
			seen[id] = append(seen[id], typ)
			mu.Unlock()
			return nil
		}
	}
	base := context.Background()
	a := With(base, Runtime{SessionID: "a", Emit: emit("a")})
	b := With(base, Runtime{SessionID: "b", Emit: emit("b")})
	var wg sync.WaitGroup
	for _, current := range []context.Context{a, b} {
		current := current
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := SessionID(current)
			if err := Emit(current, "event/"+id, nil); err != nil {
				t.Errorf("emit %s: %v", id, err)
			}
		}()
	}
	wg.Wait()
	if len(seen["a"]) != 1 || seen["a"][0] != "event/a" || len(seen["b"]) != 1 || seen["b"][0] != "event/b" {
		t.Fatalf("routed events = %#v", seen)
	}
}

func TestCorrelationContextPreservesRuntimeAndSupportsNarrowing(t *testing.T) {
	base := With(context.Background(), Runtime{
		SessionID: "session-1",
		Emit:      func(string, any) error { return nil },
		Trace:     Correlation{AgentID: "agent-1", SessionID: "session-1", TurnID: "turn-1"},
	})
	narrowed := WithCorrelation(base, Correlation{AgentID: "agent-1", TurnID: "turn-1", StepID: "step-2", RequestID: "request-2", CallID: "call-2"})
	got, ok := CorrelationOf(narrowed)
	if !ok {
		t.Fatal("correlation should be present")
	}
	if got.AgentID != "agent-1" || got.SessionID != "session-1" || got.TurnID != "turn-1" || got.StepID != "step-2" || got.RequestID != "request-2" || got.CallID != "call-2" {
		t.Fatalf("correlation = %+v", got)
	}
	if SessionID(narrowed) != "session-1" {
		t.Fatalf("session id = %q", SessionID(narrowed))
	}
}
