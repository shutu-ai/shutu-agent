package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

// TestCanonicalWorkerDeathRecoveryWaterfallReplay proves the durable crash
// contract for one fixture that crosses the three branches most likely to be
// patched independently: a durable provider retry, log-only compaction facts,
// and a worker death after tool dispatch. The recovery projection must repair
// only the open tail, preserve the committed prefix, remain a no-op when
// replayed twice, and survive the wire event round trip unchanged.
func TestCanonicalWorkerDeathRecoveryWaterfallReplay(t *testing.T) {
	open := New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := open.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}

	// Turn one reaches a terminal state only after a durable retry boundary.
	appendEvent(EventTurnStart, NewTurnStartAt(1))
	appendEvent(EventStepStart, NewStepStartAt(1, 1))
	appendEvent(EventUserMessage, NewUserMessage("inspect after retry"))
	appendEvent(EventRequestHeader, NewRequestHeader("request-1", llm.ChatRequest{
		Provider: "fixture", Model: "fixture-model",
	}, "initial"))
	failure := &llm.Failure{Code: "SERVER", Message: "temporary fixture failure", Status: 503}
	scheduled := llm.RetryEvent{
		RetryID: "retry-1", Attempt: 1, MaxRetries: 1, DelayMS: 1,
		Mode: "normal", PolicyKey: "fixture", Failure: failure,
	}
	appendEvent(EventLLMRetry, NewLLMRetryAt(1, 1, "fixture", "fixture-model", scheduled))
	appendEvent(EventLLMRetryStarted, NewLLMRetryStarted(scheduled, 1, 1))
	appendEvent(EventAssistantMessage, NewAssistantMessageAtWithUsage(
		1, 1, "retry recovered", nil, "stop", "", llm.TokenUsage{},
	))
	appendEvent(EventStepEnd, NewStepEndAt(1, 1, "completed", ""))
	appendEvent(EventTurnEnd, NewTurnEndAt(1, "completed", ""))

	// Compaction facts are log-only observations and must not erase or reorder
	// the committed turn boundary.
	appendEvent(EventCompactionStart, NewCompactionStart("pressure", "pre-step"))
	appendEvent(EventCompactionSummary, NewCompactionSummaryWithStats(
		"compaction-1", "compressed one turn", []int64{3}, 17, "pre-step",
	))
	appendEvent(EventCompactionEnd, NewCompactionEnd("compaction-1", [2]int64{3, 3}, 17))

	// Turn two models worker death after dispatch: both the model-visible call
	// and the execution-intent checkpoint are committed, but the result is not.
	appendEvent(EventTurnStart, NewTurnStartAt(2))
	appendEvent(EventStepStart, NewStepStartAt(2, 1))
	appendEvent(EventAssistantMessage, NewAssistantMessageAtWithUsage(
		2, 1, "", []llm.ToolCall{{
			ID: "worker-call", Name: "get_time", Arguments: "{}",
		}}, "tool_calls", "", llm.TokenUsage{},
	))
	appendEvent(EventToolCall, NewToolCall(2, 1, "worker-call", "get_time", "{}"))

	committed := append([]Event(nil), open.Events()...)
	closers, err := InterruptedTurnClosers(committed)
	if err != nil {
		t.Fatalf("interrupted turn closers: %v", err)
	}
	if len(closers) != 3 {
		t.Fatalf("closers = %d events, want tool result + step/end + turn/end", len(closers))
	}
	if closers[0].Type != EventToolResult || closers[1].Type != EventStepEnd || closers[2].Type != EventTurnEnd {
		t.Fatalf("closer types = %s/%s/%s, want tool/result/step/end/turn/end",
			closers[0].Type, closers[1].Type, closers[2].Type)
	}
	if !strings.Contains(string(closers[0].Data), `"code":"TOOL_OUTCOME_UNKNOWN"`) {
		t.Fatalf("dispatched tool closer = %s, want unknown outcome", closers[0].Data)
	}
	if !strings.Contains(string(closers[0].Data), `"sourceEventSeqs":[`) {
		t.Fatalf("dispatched tool closer lacks source checkpoint: %s", closers[0].Data)
	}
	if !strings.Contains(string(closers[1].Data), `"status":"interrupted"`) {
		t.Fatalf("step closer = %s, want interrupted", closers[1].Data)
	}
	if !strings.Contains(string(closers[2].Data), `"kind":"interrupted"`) {
		t.Fatalf("turn closer = %s, want interrupted", closers[2].Data)
	}

	// Exercise the persisted wire shape, not only the in-memory Event values.
	raw, err := json.Marshal(append(append([]Event(nil), committed...), closers...))
	if err != nil {
		t.Fatal(err)
	}
	var wire []Event
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	recovered := New()
	if err := recovered.Restore(wire); err != nil {
		t.Fatalf("canonical replay rejected recovered waterfall: %v", err)
	}

	replay, err := InterruptedTurnClosers(recovered.Events())
	if err != nil {
		t.Fatalf("second recovery pass: %v", err)
	}
	if len(replay) != 0 {
		t.Fatalf("second recovery emitted %+v, want idempotent no-op", replay)
	}

	history := recovered.DeriveHistory()
	var sawUnknownTool bool
	for _, message := range history {
		if message.Role != llm.RoleTool || message.ToolCallID != "worker-call" {
			continue
		}
		sawUnknownTool = true
		if !strings.Contains(message.Text(), "outcome is unknown") {
			t.Fatalf("unknown tool history = %q, want explicit outcome warning", message.Text())
		}
	}
	if !sawUnknownTool {
		t.Fatalf("recovered history lacks worker-call tool result: %+v", history)
	}
}
