package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeSpillApp builds a minimal app for spill wiring tests: only the fields
// registerSpills / spillAutoSpill touch (cfg.Spill, reg, log, currentID) are
// set.
func makeSpillApp(spillEnabled bool, autoSpill *bool) *app {
	return &app{
		cfg: config.Config{
			Spill: config.SpillConfig{Enabled: config.Bool(spillEnabled), AutoSpill: autoSpill},
		},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-spill",
	}
}

// spillPolicy whitelists the four spill tools so the registry Execute gate can
// run them (in production config.applyDefaults + PolicyFromConfig do this).
func spillPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"spill_write", "spill_recall", "spill_list", "spill_delete"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// countEvents returns how many events of typ the log holds.
func countEvents(log *session.Log, typ string) int {
	n := 0
	for _, ev := range log.Events() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestRegisterSpillsDisabledRegistersNothing verifies the D10 gate: with
// spill.enabled=false the composition root creates no Engine, registers no
// spill_* tool, and the auto-sedimentation hook is a no-op (dispatch-m6c-2 §5).
func TestRegisterSpillsDisabledRegistersNothing(t *testing.T) {
	a := makeSpillApp(false, nil)
	if err := a.registerSpills(); err != nil {
		t.Fatalf("registerSpills: %v", err)
	}
	if a.spills != nil {
		t.Fatal("spill engine must be nil when spill.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "spill_") {
			t.Fatalf("spill tool %q registered while spill disabled", spec.Name)
		}
	}
	// The auto-sedimentation hook must be a no-op (a.spills == nil).
	a.log.Append(session.EventUserMessage, session.NewUserMessage("hi"))
	a.log.Append(session.EventAssistantMessage, session.NewAssistantMessage("Remember: the user prefers Go for new projects", nil, "stop"))
	a.spillAutoSpill(context.Background())
	if countEvents(a.log, session.EventSpillWrite) != 0 {
		t.Fatal("spill/write event appended while spill disabled")
	}
}

// TestRegisterSpillsEnabledRegistersAndValidates verifies the enabled path:
// the Provider + Engine are created, all four spill_* tools are registered,
// D7 rejects bad arguments at the Execute gate, valid calls flow through
// (write → recall → list → delete), the spill/* events land in the session
// log (D3), and deleting an unknown id errors.
func TestRegisterSpillsEnabledRegistersAndValidates(t *testing.T) {
	a := makeSpillApp(true, nil)
	a.reg.SetPolicy(spillPolicy())
	if err := a.registerSpills(); err != nil {
		t.Fatalf("registerSpills: %v", err)
	}
	defer a.spills.Close()
	if a.spills == nil {
		t.Fatal("spill engine must be created when spill.enabled=true")
	}
	names := make([]string, 0, len(a.reg.Specs()))
	for _, s := range a.reg.Specs() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"spill_write", "spill_recall", "spill_list", "spill_delete"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"spill_write", `{}`},                         // missing required content
		{"spill_write", `{"content":123}`},            // content must be a string
		{"spill_write", `{"content":"x","extra":1}`},  // additional properties rejected
		{"spill_recall", `{}`},                        // missing required query
		{"spill_recall", `{"query":"x","limit":"5"}`}, // limit must be an integer
		{"spill_recall", `{"query":"x","limit":0}`},   // limit below the minimum
		{"spill_recall", `{"query":"x","extra":1}`},   // additional properties rejected
		{"spill_list", `{"extra":1}`},                 // list takes no arguments
		{"spill_delete", `{}`},                        // missing required id
		{"spill_delete", `{"id":123}`},                // id must be a string
		{"spill_delete", `{"id":"x","extra":1}`},      // additional properties rejected
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid write flows through and lands spill/write (D3).
	res, err := a.reg.Execute(context.Background(), "spill_write", json.RawMessage(`{"content":"The user prefers Go for new projects","source":"session:1"}`))
	if err != nil {
		t.Fatalf("spill_write via registry: %v", err)
	}
	if !strings.HasPrefix(res.Output, "spilled memo memo-") {
		t.Fatalf("spill_write output = %q, want spilled memo memo-...", res.Output)
	}
	if !hasEvent(a.log, session.EventSpillWrite) {
		t.Fatal("spill/write event missing from the session log after spill_write")
	}
	// spill_recall returns the matching memo and lands spill/recall (D3).
	recall, err := a.reg.Execute(context.Background(), "spill_recall", json.RawMessage(`{"query":"Go","limit":5}`))
	if err != nil {
		t.Fatalf("spill_recall via registry: %v", err)
	}
	if !strings.Contains(recall.Output, "The user prefers Go for new projects") {
		t.Fatalf("spill_recall output = %q, want the matching memo", recall.Output)
	}
	if !hasEvent(a.log, session.EventSpillRecall) {
		t.Fatal("spill/recall event missing from the session log after spill_recall")
	}
	// spill_list returns the table and lands spill/list (D3).
	if _, err := a.reg.Execute(context.Background(), "spill_list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("spill_list via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventSpillList) {
		t.Fatal("spill/list event missing from the session log after spill_list")
	}
	// spill_delete removes it and lands spill/delete (D3).
	id := strings.TrimPrefix(res.Output, "spilled memo ")
	if _, err := a.reg.Execute(context.Background(), "spill_delete", json.RawMessage(`{"id":"`+id+`"}`)); err != nil {
		t.Fatalf("spill_delete via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventSpillDelete) {
		t.Fatal("spill/delete event missing from the session log after spill_delete")
	}
	// Deleting an unknown id errors.
	if res, err := a.reg.Execute(context.Background(), "spill_delete", json.RawMessage(`{"id":"memo-nope"}`)); err != nil || !res.IsError {
		t.Fatalf("spill_delete of an unknown id must return a structured error: result=%+v err=%v", res, err)
	}
	// The spill/* rows stay in the log and never derive into messages (log-only).
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("spill/* events must not derive into messages: %+v", msgs)
	}
}

// TestSpillAutoSpillFoldsTurnAndEmitsOnce verifies the D5 serial auto-sedimentation
// path (dispatch-m6c-2 §4): after a completed turn, spillAutoSpill folds the
// log into new memories through the pure AutoSpill policy and logs one
// spill/write fact per new memo. Re-running over the same log adds nothing —
// no new memo, no duplicate spill/write event (不重复沉淀) — and the events
// never derive into model messages.
func TestSpillAutoSpillFoldsTurnAndEmitsOnce(t *testing.T) {
	a := makeSpillApp(true, nil) // auto_spill absent ⇒ true
	a.reg.SetPolicy(spillPolicy())
	if err := a.registerSpills(); err != nil {
		t.Fatalf("registerSpills: %v", err)
	}
	defer a.spills.Close()

	// A completed turn whose assistant text is memory-worthy (≥24 runes and a
	// conclusive marker).
	a.log.Append(session.EventUserMessage, session.NewUserMessage("hi"))
	a.log.Append(session.EventAssistantMessage, session.NewAssistantMessage("Remember: the user prefers Go for new projects", nil, "stop"))

	a.spillAutoSpill(context.Background())

	all, err := a.spills.List(context.Background())
	if err != nil {
		t.Fatalf("spills.List: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("auto-spill stored no memories after a memory-worthy turn")
	}
	if got := countEvents(a.log, session.EventSpillWrite); got != 1 {
		t.Fatalf("spill/write events = %d, want exactly 1", got)
	}
	// The spill/write event carries the memo id + bounded content (D3).
	found := false
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventSpillWrite {
			continue
		}
		var d struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		if strings.HasPrefix(d.ID, "memo-") && d.Content == "Remember: the user prefers Go for new projects" {
			found = true
		}
	}
	if !found {
		t.Fatal("spill/write event missing the memo id and content head")
	}

	// Re-running the same log adds nothing: no new memo, no new event.
	beforeCount := len(all)
	beforeEvents := countEvents(a.log, session.EventSpillWrite)
	a.spillAutoSpill(context.Background())
	all2, err := a.spills.List(context.Background())
	if err != nil {
		t.Fatalf("spills.List: %v", err)
	}
	if len(all2) != beforeCount {
		t.Fatalf("memos grew from %d to %d on re-run, want unchanged (no duplicate sedimentation)", beforeCount, len(all2))
	}
	if got := countEvents(a.log, session.EventSpillWrite); got != beforeEvents {
		t.Fatalf("spill/write events grew from %d to %d on re-run, want unchanged", beforeEvents, got)
	}

	// Log-only: only the conversation derives into messages.
	if msgs := a.log.DeriveHistory(); len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
}

// TestSpillAutoSpillRespectsAutoSpillFalse verifies that auto_spill:false
// keeps the spill_* tools usable (registerSpills still wires the engine) while
// the auto-sedimentation hook is a no-op.
func TestSpillAutoSpillRespectsAutoSpillFalse(t *testing.T) {
	auto := false
	a := makeSpillApp(true, &auto)
	a.reg.SetPolicy(spillPolicy())
	if err := a.registerSpills(); err != nil {
		t.Fatalf("registerSpills: %v", err)
	}
	defer a.spills.Close()
	if a.spills == nil {
		t.Fatal("engine must be created even when auto_spill=false")
	}
	a.log.Append(session.EventUserMessage, session.NewUserMessage("hi"))
	a.log.Append(session.EventAssistantMessage, session.NewAssistantMessage("Remember: the user prefers Go for new projects", nil, "stop"))
	a.spillAutoSpill(context.Background())
	all, err := a.spills.List(context.Background())
	if err != nil {
		t.Fatalf("spills.List: %v", err)
	}
	if len(all) != 0 {
		t.Fatal("auto-spill must not run when auto_spill=false")
	}
	if countEvents(a.log, session.EventSpillWrite) != 0 {
		t.Fatal("spill/write event appended while auto_spill=false")
	}
}
