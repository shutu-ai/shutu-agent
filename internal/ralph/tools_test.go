package ralph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/session"
)

// eventCapture records one emitted event's type and payload.
type eventCapture struct {
	typ  string
	data any
}

// TestRalphToolExecuteFormatsReport verifies Execute drives the engine, renders
// the bounded report text (objective head / rounds / outcome / final), and
// emits the ralph/run event with the lean payload (D3).
func TestRalphToolExecuteFormatsReport(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"DONE: 完成报告"}})
	var events []eventCapture
	tool := NewRalphTool(eng, func(typ string, data any) {
		events = append(events, eventCapture{typ: typ, data: data})
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"交付目标","max_rounds":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"ralph: 交付目标", "rounds: 1/1", "outcome: done", "final: 完成报告", "round 1: 完成报告"} {
		if !strings.Contains(out, want) {
			t.Errorf("report %q lacks %q", out, want)
		}
	}
	if len(events) != 1 || events[0].typ != session.EventRalphRun {
		t.Fatalf("events = %+v, want exactly one %s event", events, session.EventRalphRun)
	}
	raw, err := json.Marshal(events[0].data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p struct {
		Objective string `json:"objective"`
		Rounds    int    `json:"rounds"`
		Done      bool   `json:"done"`
		Blocked   bool   `json:"blocked"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Objective != "交付目标" || p.Rounds != 1 || !p.Done || p.Blocked {
		t.Errorf("ralph/run payload = %+v, want objective/rounds/done", p)
	}
}

// TestRalphToolExecuteBlocked verifies a BLOCKED outcome renders the blocked
// outcome and the block reason.
func TestRalphToolExecuteBlocked(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"BLOCKED: 缺凭证"}})
	tool := NewRalphTool(eng, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"x","max_rounds":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "outcome: blocked") {
		t.Errorf("report %q lacks the blocked outcome", out)
	}
	if !strings.Contains(out, "final: 缺凭证") {
		t.Errorf("report %q lacks the block reason", out)
	}
}

// TestRalphToolExecuteRejectsEmptyObjective: an empty (or whitespace-only /
// absent) objective is rejected before any loop runs.
func TestRalphToolExecuteRejectsEmptyObjective(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"DONE: x"}})
	tool := NewRalphTool(eng, nil)
	for _, args := range []string{`{}`, `{"objective":""}`, `{"objective":"   "}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Execute(%s) must be rejected (empty objective)", args)
		}
	}
}

// TestRalphToolExecuteRoundLimit: an all-progress run settles at the round cap
// and renders the round-limit outcome.
func TestRalphToolExecuteRoundLimit(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"进展一", "进展二", "进展三"}})
	tool := NewRalphTool(eng, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"x","max_rounds":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "outcome: round-limit") {
		t.Errorf("report %q lacks the round-limit outcome", out)
	}
	if !strings.Contains(out, "rounds: 3/3") {
		t.Errorf("report %q lacks rounds: 3/3", out)
	}
}
