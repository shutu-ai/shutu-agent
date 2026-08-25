package spill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// eventRec is one event emitted through the SpillTools onEvent sink.
type eventRec struct {
	typ  string
	data any
}

// newSpillToolsWithEvents returns an engine and a SpillTools bundle wired to a
// slice that records every emitted spill/* event (the composition root wires
// the same sink to the session log in cmd/pa, D3).
func newSpillToolsWithEvents(t *testing.T) (*engine, *SpillTools, *[]eventRec) {
	t.Helper()
	e := NewEngine(nil)
	t.Cleanup(func() { e.Close() })
	recs := &[]eventRec{}
	st := NewSpillTools(e, func(typ string, data any) {
		*recs = append(*recs, eventRec{typ: typ, data: data})
	})
	return e, st, recs
}

// execTool is the subset of tools.Tool the spill tools implement structurally.
type execTool interface {
	Execute(ctx context.Context, args any) (string, error)
}

// mustExec runs one tool Execute and fails the test on error.
func mustExec(t *testing.T, tool execTool, args string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return out
}

// mustFail runs one tool Execute and asserts it fails with an error containing
// wantSub (the error message — not a panic — is the contract for unknown ids /
// invalid arguments, dispatch-m6c-2 §3).
func mustFail(t *testing.T, tool execTool, args, wantSub string) {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("Execute(%s) = %q, want an error containing %q", args, out, wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("Execute(%s) error = %q, want it to contain %q", args, err, wantSub)
	}
}

// eventTypes returns the emitted event types in order.
func eventTypes(recs []eventRec) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.typ)
	}
	return out
}

// spillWritePayload is the shape of the spill/write event payload.
type spillWritePayload struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// spillRecallPayload is the shape of the spill/recall event payload.
type spillRecallPayload struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

// spillListPayload is the shape of the spill/list event payload.
type spillListPayload struct {
	Count int `json:"count"`
}

// spillDeletePayload is the shape of the spill/delete event payload.
type spillDeletePayload struct {
	ID string `json:"id"`
}

// decodeEvent unmarshals a captured event payload into T (the session payloads
// are plain JSON-serializable data).
func decodeEvent[T any](t *testing.T, ev eventRec) T {
	t.Helper()
	raw, err := json.Marshal(ev.data)
	if err != nil {
		t.Fatalf("marshal %s event data: %v", ev.typ, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s event data %s: %v", ev.typ, raw, err)
	}
	return out
}

func TestSpillWriteToolWritesAndEmits(t *testing.T) {
	_, st, recs := newSpillToolsWithEvents(t)
	out := mustExec(t, st.Write(), `{"content":"The user prefers Go for new projects","source":"session:1"}`)
	if !strings.Contains(out, "spilled memo memo-") {
		t.Fatalf("spill_write output = %q, want spilled memo memo-...", out)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "spill/write" {
		t.Fatalf("emitted types = %v, want [spill/write]", got)
	}
	d := decodeEvent[spillWritePayload](t, (*recs)[0])
	if !strings.HasPrefix(d.ID, "memo-") {
		t.Fatalf("spill/write id = %q, want a memo- prefix", d.ID)
	}
	if d.Content != "The user prefers Go for new projects" {
		t.Fatalf("spill/write content = %q, want the full content", d.Content)
	}
}

func TestSpillWriteToolDedupsAndDefaultsSource(t *testing.T) {
	e, st, recs := newSpillToolsWithEvents(t)
	// Same content written twice → same memo id, one memo, one memo stored
	// (idempotent, dispatch-m6c-1: 同内容不重复写). The source defaults to the
	// tool name when omitted.
	first := mustExec(t, st.Write(), `{"content":"Dedup me"}`)
	second := mustExec(t, st.Write(), `{"content":"Dedup me"}`)
	if first != second {
		t.Fatalf("re-write output = %q, want %q (same id)", second, first)
	}
	all, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("stored %d memos, want 1 (dedup)", len(all))
	}
	if all[0].Source != ToolWriteName {
		t.Fatalf("memo source = %q, want the default tool name", all[0].Source)
	}
	if got := len(*recs); got != 2 {
		t.Fatalf("spill/write events = %d, want 2 (one per explicit write)", got)
	}
}

func TestSpillWriteToolRejectsBlankContent(t *testing.T) {
	_, st, recs := newSpillToolsWithEvents(t)
	mustFail(t, st.Write(), `{"content":"   "}`, "empty content")
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed write, got %v", eventTypes(*recs))
	}
}

func TestSpillRecallToolRecallsAndEmits(t *testing.T) {
	_, st, recs := newSpillToolsWithEvents(t)
	mustExec(t, st.Write(), `{"content":"alpha fact about cats","source":"s1"}`)
	mustExec(t, st.Write(), `{"content":"beta fact about dogs","source":"s2"}`)
	out := mustExec(t, st.Recall(), `{"query":"CATS","limit":5}`)
	if !strings.Contains(out, "alpha fact about cats") {
		t.Fatalf("spill_recall output = %q, want the matching memo", out)
	}
	if strings.Contains(out, "beta fact about dogs") {
		t.Fatalf("spill_recall output = %q, must not contain the non-matching memo", out)
	}
	if got := eventTypes(*recs); len(got) != 3 || got[2] != "spill/recall" {
		t.Fatalf("emitted types = %v, want the third event spill/recall", got)
	}
	d := decodeEvent[spillRecallPayload](t, (*recs)[2])
	if d.Query != "CATS" || d.Count != 1 {
		t.Fatalf("spill/recall payload = %+v, want query CATS / count 1", d)
	}
}

func TestSpillRecallToolNoMatch(t *testing.T) {
	_, st, _ := newSpillToolsWithEvents(t)
	out := mustExec(t, st.Recall(), `{"query":"zebra"}`)
	if !strings.Contains(out, "no memories") {
		t.Fatalf("spill_recall output = %q, want no memories", out)
	}
}

func TestSpillListToolListsAndEmits(t *testing.T) {
	_, st, recs := newSpillToolsWithEvents(t)
	mustExec(t, st.Write(), `{"content":"one memory","source":"s1"}`)
	out := mustExec(t, st.List(), `{}`)
	if !strings.Contains(out, "one memory") {
		t.Fatalf("spill_list output = %q, want the stored memo", out)
	}
	if got := eventTypes(*recs); len(got) != 2 || got[1] != "spill/list" {
		t.Fatalf("emitted types = %v, want the second event spill/list", got)
	}
	d := decodeEvent[spillListPayload](t, (*recs)[1])
	if d.Count != 1 {
		t.Fatalf("spill/list payload = %+v, want count 1", d)
	}
}

func TestSpillDeleteToolDeletesAndEmits(t *testing.T) {
	e, st, recs := newSpillToolsWithEvents(t)
	writeOut := mustExec(t, st.Write(), `{"content":"remove me","source":"s1"}`)
	id := strings.TrimPrefix(writeOut, "spilled memo ")
	// Empty table before delete is not it — delete the stored memo.
	mustExec(t, st.Delete(), `{"id":"`+id+`"}`)
	all, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("after delete = %d memos, want 0", len(all))
	}
	if got := eventTypes(*recs); len(got) != 2 || got[1] != "spill/delete" {
		t.Fatalf("emitted types = %v, want the second event spill/delete", got)
	}
	d := decodeEvent[spillDeletePayload](t, (*recs)[1])
	if d.ID != id {
		t.Fatalf("spill/delete payload = %+v, want id %s", d, id)
	}
}

func TestSpillDeleteToolUnknownIDErrorsWithoutEvent(t *testing.T) {
	_, st, recs := newSpillToolsWithEvents(t)
	mustFail(t, st.Delete(), `{"id":"memo-nope"}`, "unknown memo")
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed delete, got %v", eventTypes(*recs))
	}
}
