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
	eng := mustEngine(t, &fakeSpawn{outputs: []string{`{"status":"complete","summary":"完成报告","evidence":["verified"],"nextSteps":[],"blocker":""}`}})
	var events []eventCapture
	tool := NewRalphTool(eng, func(typ string, data any) {
		events = append(events, eventCapture{typ: typ, data: data})
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"交付目标","maxRounds":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"complete", "agentsStarted", "完成报告"} {
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
	eng := mustEngine(t, &fakeSpawn{outputs: []string{`{"status":"blocked","summary":"无法继续","evidence":[],"nextSteps":[],"blocker":"缺凭证"}`}})
	tool := NewRalphTool(eng, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"x","maxRounds":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "blocked") || !strings.Contains(out, "缺凭证") {
		t.Errorf("report %q lacks the blocked result", out)
	}
}

// TestRalphToolExecuteRejectsEmptyObjective: an empty (or whitespace-only /
// absent) objective is rejected before any loop runs.
func TestRalphToolExecuteRejectsEmptyObjective(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{`{"status":"complete","summary":"x","evidence":["x"],"nextSteps":[],"blocker":""}`}})
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
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"进展一","evidence":["one"],"nextSteps":["two"],"blocker":""}`,
		`{"status":"continue","summary":"进展二","evidence":["two"],"nextSteps":["three"],"blocker":""}`,
		`{"status":"continue","summary":"进展三","evidence":["three"],"nextSteps":["more"],"blocker":""}`,
	}})
	tool := NewRalphTool(eng, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"objective":"x","maxRounds":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "budget-limited") || !strings.Contains(out, "agentsStarted") {
		t.Errorf("report %q lacks the budget-limited structured result", out)
	}
}
