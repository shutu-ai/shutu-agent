package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

// Compile-time assertions: the three eval_* tools implement the tool method set
// the composition root boxes into a tools.Registry.
var (
	_ = (*EvalRunTool)(nil)
	_ = (*EvalResultTool)(nil)
	_ = (*EvalListTool)(nil)
)

// eventLog is a minimal onEvent recorder for tool tests.
type eventLog struct {
	types []string
	data  []any
}

func (e *eventLog) record(typ string, data any) {
	e.types = append(e.types, typ)
	e.data = append(e.data, data)
}

// newTestTools wires a real eval Engine (with the mockEvaluator from
// engine_test.go) into a NewEvalTools bundle bound to an optional event log
// (nil log ⇒ no event sink).
func newTestTools(t *testing.T, mock *mockEvaluator, log *eventLog) *EvalTools {
	t.Helper()
	if mock == nil {
		mock = &mockEvaluator{}
	}
	eng, err := NewEngine(EngineOpts{Evaluator: mock})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	var onEvent func(typ string, data any)
	if log != nil {
		onEvent = log.record
	}
	return NewEvalTools(eng, onEvent)
}

// TestEvalToolsRun verifies eval_run judges through the Engine (the evaluator
// receives the full output and criteria), returns the formatted record, and
// emits exactly one eval/run event carrying the lean payload (D3 / D-EVAL-5).
func TestEvalToolsRun(t *testing.T) {
	ctx := context.Background()
	log := &eventLog{}
	mock := &mockEvaluator{verdict: VerdictPass, reason: "criteria met", kind: "rule"}
	et := newTestTools(t, mock, log)

	out, err := et.Run().Execute(ctx, json.RawMessage(`{"task_id":"todo-7","output":"deliverable text","criteria":["contains: done","manual: review"]}`))
	if err != nil {
		t.Fatalf("eval_run: %v", err)
	}
	if !strings.Contains(out, "eval eval-1: pass (kind=rule)") {
		t.Errorf("eval_run output = %q, want the record head", out)
	}
	if !strings.Contains(out, "task: todo-7") || !strings.Contains(out, "reason: criteria met") || !strings.Contains(out, "criteria: 2") {
		t.Errorf("eval_run output = %q, want task + reason + criteria lines", out)
	}
	// The evaluator received the full, unbounded deliverable and criteria.
	if mock.gotOut != "deliverable text" {
		t.Errorf("evaluator got output %q, want the full text", mock.gotOut)
	}
	if !equalStrings(mock.gotCrits, []string{"contains: done", "manual: review"}) {
		t.Errorf("evaluator got criteria %v", mock.gotCrits)
	}

	// Exactly one eval/run event with the lean payload (never the output).
	if len(log.types) != 1 || log.types[0] != session.EventEvalRun {
		t.Fatalf("emitted events = %v, want exactly [eval/run]", log.types)
	}
	var d struct {
		ID            string `json:"id"`
		TaskID        string `json:"taskId,omitempty"`
		Verdict       string `json:"verdict"`
		Reason        string `json:"reason,omitempty"`
		EvaluatorKind string `json:"evaluatorKind,omitempty"`
		CriteriaCount int    `json:"criteriaCount"`
	}
	raw, err := json.Marshal(log.data[0])
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if d.ID != "eval-1" || d.TaskID != "todo-7" || d.Verdict != "pass" ||
		d.Reason != "criteria met" || d.EvaluatorKind != "rule" || d.CriteriaCount != 2 {
		t.Errorf("eval/run payload = %+v, want the lean summary", d)
	}
}

func TestEvalRunDurableEventFailureIsReturned(t *testing.T) {
	et := newTestTools(t, &mockEvaluator{verdict: VerdictPass, reason: "ok", kind: "rule"}, nil)
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "session-1",
		Emit:      func(string, any) error { return errors.New("durable sink unavailable") },
	})
	_, err := et.Run().Execute(ctx, json.RawMessage(`{"task_id":"t1","output":"done","criteria":["contains:done"]}`))
	if err == nil || !strings.Contains(err.Error(), "persist event") {
		t.Fatalf("eval durable event error = %v, want persist event failure", err)
	}
}

// TestEvalToolsResult verifies eval_result returns a stored record by id in the
// formatted shape.
func TestEvalToolsResult(t *testing.T) {
	ctx := context.Background()
	mock := &mockEvaluator{verdict: VerdictManual, reason: "needs a human", kind: "manual"}
	et := newTestTools(t, mock, nil)

	if _, err := et.Run().Execute(ctx, json.RawMessage(`{"task_id":"t-1","output":"x","criteria":["manual: ok"]}`)); err != nil {
		t.Fatalf("eval_run: %v", err)
	}
	out, err := et.Result().Execute(ctx, json.RawMessage(`{"id":"eval-1"}`))
	if err != nil {
		t.Fatalf("eval_result: %v", err)
	}
	if !strings.Contains(out, "eval eval-1: manual (kind=manual)") || !strings.Contains(out, "reason: needs a human") {
		t.Errorf("eval_result output = %q", out)
	}

	// An unknown id surfaces as an error.
	if _, err := et.Result().Execute(ctx, json.RawMessage(`{"id":"nope-1"}`)); err == nil {
		t.Fatal("eval_result on an unknown id must fail")
	}
}

// TestEvalToolsList verifies eval_list renders the history most-recent-first,
// honors the limit, and reports an empty history explicitly.
func TestEvalToolsList(t *testing.T) {
	ctx := context.Background()
	et := newTestTools(t, &mockEvaluator{verdict: VerdictPass, kind: "rule"}, nil)

	// Empty history.
	out, err := et.List().Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("eval_list (empty): %v", err)
	}
	if out != "no evaluation records yet" {
		t.Errorf("eval_list (empty) = %q, want the empty notice", out)
	}

	// Three records, most recent first in the listing.
	for i := 1; i <= 3; i++ {
		if _, err := et.Run().Execute(ctx, json.RawMessage(`{"task_id":"t-`+string(rune('0'+i))+`","output":"o","criteria":["c"]}`)); err != nil {
			t.Fatalf("eval_run #%d: %v", i, err)
		}
	}
	out, err = et.List().Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("eval_list: %v", err)
	}
	if !strings.Contains(out, "eval eval-3: pass") || !strings.Contains(out, "eval eval-1: pass") {
		t.Errorf("eval_list = %q, want all three records", out)
	}
	// Most recent first: eval-3 precedes eval-1.
	if strings.Index(out, "eval-3") > strings.Index(out, "eval-1") {
		t.Errorf("eval_list = %q, want eval-3 before eval-1 (most recent first)", out)
	}

	// The limit pages the listing.
	out, err = et.List().Execute(ctx, json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatalf("eval_list (limit): %v", err)
	}
	if n := strings.Count(out, "eval "); n != 2 {
		t.Errorf("eval_list limit=2 returned %d records, want 2", n)
	}
	if !strings.Contains(out, "eval eval-3: pass") || strings.Contains(out, "eval eval-1: pass") {
		t.Errorf("eval_list limit=2 = %q, want the two most recent only", out)
	}
}

// TestEvalToolsEmptyInput verifies the tool's own defensive checks: empty
// output and empty criteria are rejected before the Engine runs (the registry
// schema enforces minLength, but whitespace-only and empty arrays would slip
// through a bare schema).
func TestEvalToolsEmptyInput(t *testing.T) {
	ctx := context.Background()
	et := newTestTools(t, &mockEvaluator{}, nil)

	if _, err := et.Run().Execute(ctx, json.RawMessage(`{"output":"   ","criteria":["c"]}`)); err == nil {
		t.Fatal("eval_run with a blank output must fail")
	}
	if _, err := et.Run().Execute(ctx, json.RawMessage(`{"output":"x","criteria":[]}`)); err == nil {
		t.Fatal("eval_run with empty criteria must fail")
	}
	// No evaluation was recorded by the rejected calls.
	recs, err := et.eng.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("List returned %d records, want 0 (rejected input must not be stored)", len(recs))
	}
}
