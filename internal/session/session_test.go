package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

func TestAppendAssignsSeqAndType(t *testing.T) {
	l := New()
	ev, err := l.Append(EventUserMessage, NewUserMessage("hello"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if ev.Seq != 1 || ev.Type != EventUserMessage {
		t.Fatalf("seq/type = %d/%q, want 1/%q", ev.Seq, ev.Type, EventUserMessage)
	}
	var d userMessageData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Text != "hello" {
		t.Fatalf("text = %q, want hello", d.Text)
	}
	ev2, _ := l.Append(EventAssistantChunk, NewAssistantChunk("hi"))
	if ev2.Seq != 2 {
		t.Fatalf("seq = %d, want 2", ev2.Seq)
	}
}

func TestLLMRequestStartDetailPreservesInspectorProjection(t *testing.T) {
	payload := NewLLMRequestStartDetail("turn:1:step:1", llm.ChatRequest{
		Provider: "deepseek", Model: "reasoner", ReasoningEffort: "high",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hello")}}},
		Tools:    []llm.ToolSchema{{Name: "read", Description: "read a file", Parameters: map[string]any{"type": "object"}}},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request detail: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode request detail: %v", err)
	}
	if got["requestId"] != "turn:1:step:1" || got["provider"] != "deepseek" || got["model"] != "reasoner" {
		t.Fatalf("request metadata = %+v", got)
	}
	if len(got["messages"].([]any)) != 1 || len(got["tools"].([]any)) != 1 {
		t.Fatalf("request context = %+v, want one message and one tool", got)
	}
}

func TestEventsSnapshotIsolation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("a"))
	snap := l.Events()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	l.Append(EventUserMessage, NewUserMessage("b"))
	if len(snap) != 1 {
		t.Fatalf("snapshot mutated: len = %d, want 1", len(snap))
	}
	if len(l.Events()) != 2 {
		t.Fatalf("log len = %d, want 2", len(l.Events()))
	}
}

func TestDeriveHistoryBasicConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("what time is it"))
	l.Append(EventAssistantChunk, NewAssistantChunk("Let "))
	l.Append(EventAssistantChunk, NewAssistantChunk("me check"))
	l.Append(EventAssistantMessage, NewAssistantMessage("Let me check", nil, "stop"))

	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "what time is it" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	// chunks fold away; the authoritative assistant/message wins
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Let me check" {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
}

func TestDeriveHistoryToolRoundTrip(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("read the file"))
	l.Append(EventAssistantMessage, NewAssistantMessage("", []llm.ToolCall{
		{ID: "call_1", Name: "read", Arguments: `{"path":"/tmp/x"}`},
	}, "tool_calls"))
	l.Append(EventToolResult, NewToolResult("call_1", "read", "file contents", nil))

	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3", len(msgs))
	}
	asst := msgs[1]
	if asst.Role != llm.RoleAssistant || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant msg = %+v", asst)
	}
	if asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Name != "read" {
		t.Fatalf("tool call = %+v", asst.ToolCalls[0])
	}
	tool := msgs[2]
	if tool.Role != llm.RoleTool || tool.ToolCallID != "call_1" || tool.Text() != "file contents" {
		t.Fatalf("tool msg = %+v", tool)
	}
}

func TestDeriveHistoryToolErrorBecomesToolMessage(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("do it"))
	l.Append(EventAssistantMessage, NewAssistantMessage("", []llm.ToolCall{
		{ID: "call_2", Name: "read", Arguments: `{"path":"/nope"}`},
	}, "tool_calls"))
	l.Append(EventToolError, NewToolError("call_2", "read", "no such file"))

	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3", len(msgs))
	}
	tool := msgs[2]
	if tool.Role != llm.RoleTool || tool.ToolCallID != "call_2" {
		t.Fatalf("tool msg = %+v", tool)
	}
	if tool.Text() != "Error: no such file" {
		t.Fatalf("tool error content = %q", tool.Text())
	}
}

func TestAppendAssignsEventVersion(t *testing.T) {
	l := New()
	ev, err := l.Append(EventUserMessage, NewUserMessage("hi"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if ev.Version != EventVersion {
		t.Fatalf("version = %d, want %d", ev.Version, EventVersion)
	}
}

// TestRestoreRebuildsLogAndContinuesSeq verifies startup replay rebuilds the
// log from persisted events and the next Append continues the sequence.
func TestRestoreRebuildsLogAndContinuesSeq(t *testing.T) {
	stored := []Event{
		{Seq: 1, Type: EventUserMessage, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"hello"}`)},
		{Seq: 2, Type: EventAssistantMessage, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"hi"}`)},
	}
	l := New()
	if err := l.Restore(stored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(l.Events()) != 2 {
		t.Fatalf("events = %d, want 2", len(l.Events()))
	}
	ev, err := l.Append(EventUserMessage, NewUserMessage("next"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if ev.Seq != 3 {
		t.Fatalf("seq = %d, want 3 (continues after restored seq 2)", ev.Seq)
	}
	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3", len(msgs))
	}
}

func TestRestoreRejectsNonMonotonicSeq(t *testing.T) {
	l := New()
	if err := l.Restore([]Event{
		{Seq: 2, Type: EventUserMessage},
		{Seq: 1, Type: EventUserMessage},
	}); err == nil {
		t.Fatal("expected non-monotonic seq error")
	}
}

// TestAppendSinkPersistsEvent verifies the durable sink receives every
// committed event (dispatch-m2: 事件追加写入).
func TestAppendSinkPersistsEvent(t *testing.T) {
	var got []Event
	l := New()
	l.SetSink(func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if _, err := l.Append(EventUserMessage, NewUserMessage("hi")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventUserMessage {
		t.Fatalf("sink got %+v", got)
	}
}

// TestAppendSinkErrorRollsBack verifies a failing sink rolls the event back
// out of the log and fails the Append, so memory never drifts from disk.
func TestAppendSinkErrorRollsBack(t *testing.T) {
	l := New()
	l.SetSink(func(Event) error { return errors.New("disk full") })
	if _, err := l.Append(EventUserMessage, NewUserMessage("hi")); err == nil {
		t.Fatal("expected sink error")
	}
	if len(l.Events()) != 0 {
		t.Fatalf("log has %d events after failed persist, want 0", len(l.Events()))
	}
}

func TestConcurrentAppendKeepsUniqueContiguousSequences(t *testing.T) {
	const writers = 32
	const eventsPerWriter = 16
	l := New()
	var mu sync.Mutex
	seen := make(map[uint64]bool, writers*eventsPerWriter)
	l.SetSink(func(ev Event) error {
		mu.Lock()
		defer mu.Unlock()
		if seen[ev.Seq] {
			return fmt.Errorf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
		return nil
	})

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerWriter; i++ {
				if _, err := l.Append(EventUserMessage, NewUserMessage("concurrent")); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	events := l.Events()
	if len(events) != writers*eventsPerWriter {
		t.Fatalf("event count = %d, want %d", len(events), writers*eventsPerWriter)
	}
	for i, ev := range events {
		want := uint64(i + 1)
		if ev.Seq != want {
			t.Fatalf("events[%d].Seq = %d, want %d", i, ev.Seq, want)
		}
	}
}

// TestRestoreDoesNotInvokeSink verifies replay never writes back through the
// sink (loading is not appending).
func TestRestoreDoesNotInvokeSink(t *testing.T) {
	var calls int
	l := New()
	l.SetSink(func(Event) error { calls++; return nil })
	if err := l.Restore([]Event{{Seq: 1, Type: EventUserMessage, Version: 1}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if calls != 0 {
		t.Fatalf("sink invoked %d times during restore, want 0", calls)
	}
}

// TestNextSeq verifies NextSeq reports the seq the next Append will assign
// (M3 spill naming depends on it).
func TestNextSeq(t *testing.T) {
	l := New()
	if got := l.NextSeq(); got != 1 {
		t.Fatalf("NextSeq = %d, want 1", got)
	}
	l.Append(EventUserMessage, NewUserMessage("a"))
	l.Append(EventUserMessage, NewUserMessage("b"))
	if got := l.NextSeq(); got != 3 {
		t.Fatalf("NextSeq = %d, want 3", got)
	}
}

// TestToolResultSpillRecordsLocator verifies a spilled tool/result event keeps
// the structured spill record (locator + byte count) alongside the truncated
// output, and that deriving history still yields the model-visible text (which
// embeds the locator notice, D3).
func TestToolResultSpillRecordsLocator(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("read it"))
	l.Append(EventAssistantMessage, NewAssistantMessage("", []llm.ToolCall{
		{ID: "call_9", Name: "read", Arguments: `{"path":"/big"}`},
	}, "tool_calls"))
	l.Append(EventToolResult, NewToolResult("call_9", "read", "head...[truncated; see spill]", &SpillRef{
		Locator: `D:\data\spill\s-x-7.txt`,
		Bytes:   100000,
	}))

	var d toolResultData
	if err := json.Unmarshal(l.Events()[2].Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Spill == nil || d.Spill.Locator != `D:\data\spill\s-x-7.txt` || d.Spill.Bytes != 100000 {
		t.Fatalf("spill record = %+v", d.Spill)
	}
	msgs := l.DeriveHistory()
	if msgs[2].Text() != "head...[truncated; see spill]" {
		t.Fatalf("derived tool content = %q", msgs[2].Text())
	}
}

// TestJobEventsAppendAndReplay verifies the M5a job/* event types
// (job/start, job/status, job/done — dispatch-m5a-2 §1 / D3): each appends
// with the right vocabulary, survives the JSON round-trip and restart replay,
// and stays opaque to history derivation (job state is surfaced to the model
// through the job_* tools' tool/result, not through these log-only events).
func TestJobEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventJobStart, NewJobStart("bash-1", "bash", "echo hello", "s-abc")); err != nil {
		t.Fatalf("append job/start: %v", err)
	}
	if _, err := l.Append(EventJobStatus, NewJobStatus("bash-1", "stopping", "cancelled")); err != nil {
		t.Fatalf("append job/status: %v", err)
	}
	if _, err := l.Append(EventJobDone, NewJobDone("bash-1", "killed", "cancelled", strings.Repeat("very long output ", 50))); err != nil {
		t.Fatalf("append job/done: %v", err)
	}
	events := l.Events()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	if events[0].Type != EventJobStart || events[1].Type != EventJobStatus || events[2].Type != EventJobDone {
		t.Fatalf("types = %q/%q/%q", events[0].Type, events[1].Type, events[2].Type)
	}
	for i, ev := range events {
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var st jobStartData
	if err := json.Unmarshal(events[0].Data, &st); err != nil {
		t.Fatalf("unmarshal job/start: %v", err)
	}
	if st.ID != "bash-1" || st.Kind != "bash" || st.Label != "echo hello" || st.OwnerSession != "s-abc" {
		t.Fatalf("job/start payload = %+v", st)
	}
	var ss jobStatusData
	if err := json.Unmarshal(events[1].Data, &ss); err != nil {
		t.Fatalf("unmarshal job/status: %v", err)
	}
	if ss.ID != "bash-1" || ss.Status != "stopping" || ss.Detail != "cancelled" {
		t.Fatalf("job/status payload = %+v", ss)
	}
	var sd jobDoneData
	if err := json.Unmarshal(events[2].Data, &sd); err != nil {
		t.Fatalf("unmarshal job/done: %v", err)
	}
	if sd.ID != "bash-1" || sd.Status != "killed" || sd.Detail != "cancelled" {
		t.Fatalf("job/done payload = %+v", sd)
	}
	// The output summary must be bounded (dispatch-m5a-2: 输出只记摘要，有界).
	if got := len([]rune(sd.OutputSummary)); got > jobOutputSummaryMax+1 {
		t.Fatalf("job/done summary = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(sd.OutputSummary, "very long output") {
		t.Fatalf("job/done summary = %q, want it to carry the output head", sd.OutputSummary)
	}
	if len(persisted) != 3 || persisted[0].Type != EventJobStart {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every job event, and deriving history treats them all as opaque data.
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range []string{EventJobStart, EventJobStatus, EventJobDone} {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("job/* events must not derive into messages: %+v", msgs)
	}
}

// TestSubagentEventsAppendAndReplay verifies the M5b subagent/* event types
// (subagent/start, subagent/end, subagent/report — dispatch-m5b-2 §1 / D3):
// each appends with the right vocabulary, survives the JSON round-trip and
// restart replay, and stays opaque to history derivation (subagent state is
// surfaced to the model through the subagent_* tools' tool/result, not through
// these log-only events).
func TestSubagentEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventSubagentStart, NewSubagentStart("spawn-1", "spawn", "s-abc", "researcher")); err != nil {
		t.Fatalf("append subagent/start: %v", err)
	}
	if _, err := l.Append(EventSubagentEnd, NewSubagentEnd("spawn-1", "spawn", "completed", strings.Repeat("very long output ", 50))); err != nil {
		t.Fatalf("append subagent/end: %v", err)
	}
	if _, err := l.Append(EventSubagentReport, NewSubagentReport("spawn-1", "s-abc", "done researching")); err != nil {
		t.Fatalf("append subagent/report: %v", err)
	}
	events := l.Events()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	if events[0].Type != EventSubagentStart || events[1].Type != EventSubagentEnd || events[2].Type != EventSubagentReport {
		t.Fatalf("types = %q/%q/%q", events[0].Type, events[1].Type, events[2].Type)
	}
	for i, ev := range events {
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var st subagentStartData
	if err := json.Unmarshal(events[0].Data, &st); err != nil {
		t.Fatalf("unmarshal subagent/start: %v", err)
	}
	if st.ID != "spawn-1" || st.Provider != "spawn" || st.ParentSession != "s-abc" || st.Label != "researcher" {
		t.Fatalf("subagent/start payload = %+v", st)
	}
	var se subagentEndData
	if err := json.Unmarshal(events[1].Data, &se); err != nil {
		t.Fatalf("unmarshal subagent/end: %v", err)
	}
	if se.ID != "spawn-1" || se.Provider != "spawn" || se.StopReason != "completed" {
		t.Fatalf("subagent/end payload = %+v", se)
	}
	// The output summary must be bounded (dispatch-m5b-2 §1: 输出只记摘要，有界
	// 200 rune).
	if got := len([]rune(se.OutputSummary)); got > jobOutputSummaryMax+1 {
		t.Fatalf("subagent/end summary = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(se.OutputSummary, "very long output") {
		t.Fatalf("subagent/end summary = %q, want it to carry the output head", se.OutputSummary)
	}
	var sr subagentReportData
	if err := json.Unmarshal(events[2].Data, &sr); err != nil {
		t.Fatalf("unmarshal subagent/report: %v", err)
	}
	if sr.ID != "spawn-1" || sr.ParentSession != "s-abc" || sr.Content != "done researching" {
		t.Fatalf("subagent/report payload = %+v", sr)
	}
	if len(persisted) != 3 || persisted[0].Type != EventSubagentStart {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every subagent event, and deriving history treats them all as opaque
	// data.
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range []string{EventSubagentStart, EventSubagentEnd, EventSubagentReport} {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("subagent/* events must not derive into messages: %+v", msgs)
	}
}

// TestCompactionEventsAppendAndReplay verifies the M5c compaction/* event
// types (compaction/start, compaction/summary, compaction/end,
// compaction/prune — dispatch-m5c-2 §1 / D3): each appends with the right
// vocabulary, survives the JSON round-trip and restart replay, and stays
// opaque to history derivation (the summary body itself is a user/message
// carrying surfaceOp.replace, M5c-1a; these events are its log-only
// observation records). The compaction/summary projection is bounded to 200
// runes like job/done and subagent/end.
func TestCompactionEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventCompactionStart, NewCompactionStart("surface exceeded threshold", "pressure")); err != nil {
		t.Fatalf("append compaction/start: %v", err)
	}
	if _, err := l.Append(EventCompactionSummary, NewCompactionSummary("cmp-1", strings.Repeat("very long summary ", 50))); err != nil {
		t.Fatalf("append compaction/summary: %v", err)
	}
	if _, err := l.Append(EventCompactionEnd, NewCompactionEnd("cmp-1", [2]int64{5, 42}, 12000)); err != nil {
		t.Fatalf("append compaction/end: %v", err)
	}
	if _, err := l.Append(EventCompactionPrune, NewCompactionPrune("cmp-1", 3, 4096)); err != nil {
		t.Fatalf("append compaction/prune: %v", err)
	}
	events := l.Events()
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	if events[0].Type != EventCompactionStart || events[1].Type != EventCompactionSummary ||
		events[2].Type != EventCompactionEnd || events[3].Type != EventCompactionPrune {
		t.Fatalf("types = %q/%q/%q/%q", events[0].Type, events[1].Type, events[2].Type, events[3].Type)
	}
	for i, ev := range events {
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var cs compactionStartData
	if err := json.Unmarshal(events[0].Data, &cs); err != nil {
		t.Fatalf("unmarshal compaction/start: %v", err)
	}
	if cs.Reason != "surface exceeded threshold" || cs.Trigger != "pressure" {
		t.Fatalf("compaction/start payload = %+v", cs)
	}
	var cm compactionSummaryData
	if err := json.Unmarshal(events[1].Data, &cm); err != nil {
		t.Fatalf("unmarshal compaction/summary: %v", err)
	}
	if cm.CompactionID != "cmp-1" {
		t.Fatalf("compaction/summary compactionId = %q, want cmp-1", cm.CompactionID)
	}
	// The summary projection must be bounded (dispatch-m5c-2 §1: 输出只记摘要，
	// 有界 200 rune).
	if got := len([]rune(cm.Summary)); got > jobOutputSummaryMax+1 {
		t.Fatalf("compaction/summary summary = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(cm.Summary, "very long summary") {
		t.Fatalf("compaction/summary summary = %q, want it to carry the summary head", cm.Summary)
	}
	var ce compactionEndData
	if err := json.Unmarshal(events[2].Data, &ce); err != nil {
		t.Fatalf("unmarshal compaction/end: %v", err)
	}
	if ce.CompactionID != "cmp-1" || ce.ShadowedRange != [2]int64{5, 42} || ce.ShadowedTokens != 12000 {
		t.Fatalf("compaction/end payload = %+v", ce)
	}
	var cp compactionPruneData
	if err := json.Unmarshal(events[3].Data, &cp); err != nil {
		t.Fatalf("unmarshal compaction/prune: %v", err)
	}
	if cp.CompactionID != "cmp-1" || cp.Replaced != 3 || cp.SavedBytes != 4096 {
		t.Fatalf("compaction/prune payload = %+v", cp)
	}
	if len(persisted) != 4 || persisted[0].Type != EventCompactionStart {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every compaction event, and deriving history treats them all as opaque
	// data.
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range []string{EventCompactionStart, EventCompactionSummary, EventCompactionEnd, EventCompactionPrune} {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("compaction/* events must not derive into messages: %+v", msgs)
	}
	// Even interleaved with a real conversation, compaction/* events never
	// contribute a derived message: only the user/message does.
	if _, err := fresh.Append(EventUserMessage, NewUserMessage("hello")); err != nil {
		t.Fatalf("append user/message: %v", err)
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 1 || msgs[0].Text() != "hello" {
		t.Fatalf("DeriveHistory with interleaved compaction/* events = %+v, want just the user message", msgs)
	}
}

// TestSkillEventsAppendAndReplay verifies the M5d-2 skill/* event types
// (skill/catalog, skill/load — dispatch-m5d-2 §1 / D3): each appends with the
// right vocabulary, survives the JSON round-trip and restart replay, and stays
// opaque to history derivation (the model sees the catalog through the pre-step
// injected message and the body through skill_load's tool/result, not through
// these log-only events). The skill/load summary is bounded to 200 runes like
// job/done and subagent/end.
func TestSkillEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventSkillCatalog, NewSkillCatalog(3, "abc123")); err != nil {
		t.Fatalf("append skill/catalog: %v", err)
	}
	if _, err := l.Append(EventSkillLoad, NewSkillLoad("review-bash", "project-dsh", strings.Repeat("very long body ", 50))); err != nil {
		t.Fatalf("append skill/load: %v", err)
	}
	events := l.Events()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != EventSkillCatalog || events[1].Type != EventSkillLoad {
		t.Fatalf("types = %q/%q", events[0].Type, events[1].Type)
	}
	for i, ev := range events {
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var sc skillCatalogData
	if err := json.Unmarshal(events[0].Data, &sc); err != nil {
		t.Fatalf("unmarshal skill/catalog: %v", err)
	}
	if sc.EntryCount != 3 || sc.Version != "abc123" {
		t.Fatalf("skill/catalog payload = %+v", sc)
	}
	var sl skillLoadData
	if err := json.Unmarshal(events[1].Data, &sl); err != nil {
		t.Fatalf("unmarshal skill/load: %v", err)
	}
	if sl.Name != "review-bash" || sl.Source != "project-dsh" {
		t.Fatalf("skill/load payload = %+v", sl)
	}
	// The body summary must be bounded (dispatch-m5d-2 §1: 正文摘要 200-rune 有界).
	if got := len([]rune(sl.Summary)); got > jobOutputSummaryMax+1 {
		t.Fatalf("skill/load summary = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(sl.Summary, "very long body") {
		t.Fatalf("skill/load summary = %q, want it to carry the body head", sl.Summary)
	}
	if len(persisted) != 2 || persisted[0].Type != EventSkillCatalog {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every skill event, and deriving history treats them all as opaque data.
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range []string{EventSkillCatalog, EventSkillLoad} {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("skill/* events must not derive into messages: %+v", msgs)
	}
}

// M6a-2: schedule/* events follow the same append/replay/no-derive contract as
// job/subagent/compaction/skill (dispatch-m6a-2 §1 / D3), with schedule/fire's
// payload bounded to a 200-rune summary head.
func TestScheduleEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventScheduleCreate, NewScheduleCreate("sched-1", "interval", "30m")); err != nil {
		t.Fatalf("append schedule/create: %v", err)
	}
	if _, err := l.Append(EventScheduleList, NewScheduleList(2)); err != nil {
		t.Fatalf("append schedule/list: %v", err)
	}
	if _, err := l.Append(EventScheduleDelete, NewScheduleDelete("sched-1")); err != nil {
		t.Fatalf("append schedule/delete: %v", err)
	}
	if _, err := l.Append(EventScheduleFire, NewScheduleFire("sched-2", strings.Repeat("action ", 100))); err != nil {
		t.Fatalf("append schedule/fire: %v", err)
	}
	events := l.Events()
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	wantTypes := []string{EventScheduleCreate, EventScheduleList, EventScheduleDelete, EventScheduleFire}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var sc scheduleCreateData
	if err := json.Unmarshal(events[0].Data, &sc); err != nil {
		t.Fatalf("unmarshal schedule/create: %v", err)
	}
	if sc.ID != "sched-1" || sc.Kind != "interval" || sc.Spec != "30m" {
		t.Fatalf("schedule/create payload = %+v", sc)
	}
	var sl scheduleListData
	if err := json.Unmarshal(events[1].Data, &sl); err != nil {
		t.Fatalf("unmarshal schedule/list: %v", err)
	}
	if sl.Count != 2 {
		t.Fatalf("schedule/list payload = %+v", sl)
	}
	var sd scheduleDeleteData
	if err := json.Unmarshal(events[2].Data, &sd); err != nil {
		t.Fatalf("unmarshal schedule/delete: %v", err)
	}
	if sd.ID != "sched-1" {
		t.Fatalf("schedule/delete payload = %+v", sd)
	}
	var sf scheduleFireData
	if err := json.Unmarshal(events[3].Data, &sf); err != nil {
		t.Fatalf("unmarshal schedule/fire: %v", err)
	}
	if sf.ID != "sched-2" {
		t.Fatalf("schedule/fire id = %q, want sched-2", sf.ID)
	}
	// The fire payload must be bounded (dispatch-m6a-2 §1: payload 摘要
	// 200-rune 有界) yet still carry the payload head.
	if got := len([]rune(sf.Payload)); got > jobOutputSummaryMax+1 {
		t.Fatalf("schedule/fire payload = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(sf.Payload, "action") {
		t.Fatalf("schedule/fire payload = %q, want it to carry the payload head", sf.Payload)
	}
	if len(persisted) != 4 || persisted[0].Type != EventScheduleCreate {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every schedule event, and deriving history treats them all as opaque
	// data (log-only, like the M5 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("schedule/* events must not derive into messages: %+v", msgs)
	}
}

// TestScheduleEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: schedule/* rows interleaved with a real conversation do
// not appear in the derived history, and the conversation round-trips
// unchanged (D4 — adding the events never changes the turn/step structure).
func TestScheduleEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("set up a reminder"))
	l.Append(EventScheduleCreate, NewScheduleCreate("sched-1", "interval", "30m"))
	l.Append(EventAssistantMessage, NewAssistantMessage("Done.", nil, "stop"))
	l.Append(EventScheduleFire, NewScheduleFire("sched-1", "do the thing"))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "set up a reminder" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Done." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the schedule rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// M5c-1a compaction fold rule: a user/message carrying surfaceOp.replace
// substitutes a summary for the shadowed surface range [Start, End] in the
// derived history, while the shadowed events stay in the append-only log (D1).

func TestDeriveHistoryReplaceFoldsSummaryPlusTail(t *testing.T) {
	l := New()
	// Shadowed surface: seqs 1-4 (an old exchange).
	l.Append(EventUserMessage, NewUserMessage("old question"))
	l.Append(EventAssistantMessage, NewAssistantMessage("old answer", nil, "stop"))
	l.Append(EventUserMessage, NewUserMessage("old question 2"))
	l.Append(EventAssistantMessage, NewAssistantMessage("old answer 2", nil, "stop"))
	// Compaction summary marker appended after the shadowed range (D1).
	l.Append(EventUserMessage, NewUserMessageReplace("summarized", 1, 4))
	// Unshadowed tail continues after the compaction.
	l.Append(EventUserMessage, NewUserMessage("new question"))
	l.Append(EventAssistantMessage, NewAssistantMessage("new answer", nil, "stop"))

	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "summarized" {
		t.Fatalf("msg0 = %+v, want user summary", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Text() != "new question" {
		t.Fatalf("msg1 = %+v, want unshadowed tail user", msgs[1])
	}
	if msgs[2].Role != llm.RoleAssistant || msgs[2].Text() != "new answer" {
		t.Fatalf("msg2 = %+v, want unshadowed tail assistant", msgs[2])
	}
	// D1: shadowed events are still physically in the log.
	if got := len(l.Events()); got != 7 {
		t.Fatalf("log events = %d, want 7 (append-only, shadowed events retained)", got)
	}
}

func TestDeriveHistoryWithoutReplaceUnchanged(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("a"))
	l.Append(EventAssistantChunk, NewAssistantChunk("A"))
	l.Append(EventAssistantMessage, NewAssistantMessage("A", nil, "stop"))
	l.Append(EventUserMessage, NewUserMessage("b"))
	l.Append(EventAssistantMessage, NewAssistantMessage("B", []llm.ToolCall{
		{ID: "call_x", Name: "get_time", Arguments: `{}`},
	}, "tool_calls"))
	l.Append(EventToolResult, NewToolResult("call_x", "get_time", "12:00", nil))

	want := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("a")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.Text("A")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("b")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.Text("B")}, ToolCalls: []llm.ToolCall{{ID: "call_x", Name: "get_time", Arguments: `{}`}}},
		{Role: llm.RoleTool, ToolCallID: "call_x", Content: []llm.ContentBlock{llm.Text("12:00")}},
	}
	if msgs := l.DeriveHistory(); !reflect.DeepEqual(msgs, want) {
		t.Fatalf("derived = %+v, want %+v (no replace marker must not change folding)", msgs, want)
	}
}

func TestDeriveHistoryReplaceShadowingMixedEvents(t *testing.T) {
	l := New()
	// Shadowed range spans user, assistant (with a tool call) and tool/result.
	l.Append(EventUserMessage, NewUserMessage("read the file")) // 1
	l.Append(EventAssistantMessage, NewAssistantMessage("", []llm.ToolCall{
		{ID: "call_1", Name: "read", Arguments: `{"path":"/tmp/x"}`},
	}, "tool_calls")) // 2
	l.Append(EventToolResult, NewToolResult("call_1", "read", "file contents", nil)) // 3
	l.Append(EventAssistantMessage, NewAssistantMessage("Here it is", nil, "stop"))  // 4
	l.Append(EventUserMessage, NewUserMessageReplace("compacted 1-4", 1, 4))         // 5
	l.Append(EventUserMessage, NewUserMessage("continue"))                           // 6
	l.Append(EventAssistantMessage, NewAssistantMessage("continuing", nil, "stop"))  // 7

	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "compacted 1-4" {
		t.Fatalf("msg0 = %+v, want summary over mixed shadowed events", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Text() != "continue" {
		t.Fatalf("msg1 = %+v, want tail user", msgs[1])
	}
	if msgs[2].Role != llm.RoleAssistant || msgs[2].Text() != "continuing" {
		t.Fatalf("msg2 = %+v, want tail assistant", msgs[2])
	}
}

func TestDeriveHistoryReplaceEmptySummaryPreserved(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("old"))                              // 1
	l.Append(EventAssistantMessage, NewAssistantMessage("old reply", nil, "stop")) // 2
	l.Append(EventUserMessage, NewUserMessageReplace("", 1, 2))                    // 3: empty summary text
	l.Append(EventUserMessage, NewUserMessage("new"))                              // 4

	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "" {
		t.Fatalf("msg0 = %+v, want preserved empty summary user message", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Text() != "new" {
		t.Fatalf("msg1 = %+v, want tail user", msgs[1])
	}
}

func TestNewUserMessageReplaceJSONRoundTrip(t *testing.T) {
	// surfaceOp serializes with the replace marker on the replace payload.
	raw, err := json.Marshal(NewUserMessageReplace("summary", 2, 7))
	if err != nil {
		t.Fatalf("marshal replace payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal replace payload: %v", err)
	}
	if m["text"] != "summary" {
		t.Fatalf("text = %v, want summary", m["text"])
	}
	so, ok := m["surfaceOp"].(map[string]any)
	if !ok {
		t.Fatalf("surfaceOp missing in %s", raw)
	}
	if so["op"] != "replace" || so["start"] != float64(2) || so["end"] != float64(7) {
		t.Fatalf("surfaceOp = %+v, want {op:replace start:2 end:7}", so)
	}

	// NewUserMessage stays surfaceOp-free (omitempty, backward compatible).
	plain, err := json.Marshal(NewUserMessage("hi"))
	if err != nil {
		t.Fatalf("marshal plain payload: %v", err)
	}
	var pm map[string]any
	if err := json.Unmarshal(plain, &pm); err != nil {
		t.Fatalf("unmarshal plain payload: %v", err)
	}
	if _, ok := pm["surfaceOp"]; ok {
		t.Fatalf("plain user/message payload must not carry surfaceOp: %s", plain)
	}

	// surfaceOp deserializes back into the typed payload.
	var d userMessageData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal into userMessageData: %v", err)
	}
	if d.Text != "summary" || d.SurfaceOp == nil || d.SurfaceOp.Op != "replace" ||
		d.SurfaceOp.Start != 2 || d.SurfaceOp.End != 7 {
		t.Fatalf("userMessageData = %+v", d)
	}

	// Full round trip through Append + Restore: the persisted surfaceOp payload
	// folds the shadowed range out after a restart replay.
	l := New()
	l.Append(EventUserMessage, NewUserMessage("x"))                        // 1
	l.Append(EventAssistantMessage, NewAssistantMessage("y", nil, "stop")) // 2
	l.Append(EventUserMessage, NewUserMessageReplace("s", 1, 2))           // 3
	fresh := New()
	if err := fresh.Restore(l.Events()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	msgs := fresh.DeriveHistory()
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser || msgs[0].Text() != "s" {
		t.Fatalf("round-trip derived = %+v, want [user s]", msgs)
	}
}

// M6b-2: the four plan event types append with a monotonic Seq, serialize
// their payloads, persist through the sink and survive a restart replay, and
// never derive into model-visible messages (log-only, D3 — dispatch-m6b-2 §1).
func TestPlanEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventPlanCreate, NewPlanCreate("goal", "goal-1", "Ship the agent", nil)); err != nil {
		t.Fatalf("append plan/create: %v", err)
	}
	if _, err := l.Append(EventPlanUpdate, NewPlanUpdate("plan", "plan-1")); err != nil {
		t.Fatalf("append plan/update: %v", err)
	}
	if _, err := l.Append(EventPlanStatus, NewPlanStatus("todo", "todo-1", "done")); err != nil {
		t.Fatalf("append plan/status: %v", err)
	}
	if _, err := l.Append(EventPlanDelete, NewPlanDelete("goal", "goal-1")); err != nil {
		t.Fatalf("append plan/delete: %v", err)
	}
	if _, err := l.Append(EventPlanList, NewPlanList(2)); err != nil {
		t.Fatalf("append plan/list: %v", err)
	}
	events := l.Events()
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
	wantTypes := []string{EventPlanCreate, EventPlanUpdate, EventPlanStatus, EventPlanDelete, EventPlanList}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var pc planCreateData
	if err := json.Unmarshal(events[0].Data, &pc); err != nil {
		t.Fatalf("unmarshal plan/create: %v", err)
	}
	if pc.Scope != "goal" || pc.ID != "goal-1" || pc.Title != "Ship the agent" {
		t.Fatalf("plan/create payload = %+v", pc)
	}
	var pu planUpdateData
	if err := json.Unmarshal(events[1].Data, &pu); err != nil {
		t.Fatalf("unmarshal plan/update: %v", err)
	}
	if pu.Scope != "plan" || pu.ID != "plan-1" {
		t.Fatalf("plan/update payload = %+v", pu)
	}
	var ps planStatusData
	if err := json.Unmarshal(events[2].Data, &ps); err != nil {
		t.Fatalf("unmarshal plan/status: %v", err)
	}
	if ps.Scope != "todo" || ps.ID != "todo-1" || ps.Status != "done" {
		t.Fatalf("plan/status payload = %+v", ps)
	}
	var pd planDeleteData
	if err := json.Unmarshal(events[3].Data, &pd); err != nil {
		t.Fatalf("unmarshal plan/delete: %v", err)
	}
	if pd.Scope != "goal" || pd.ID != "goal-1" {
		t.Fatalf("plan/delete payload = %+v", pd)
	}
	var pl planListData
	if err := json.Unmarshal(events[4].Data, &pl); err != nil {
		t.Fatalf("unmarshal plan/list: %v", err)
	}
	if pl.Count != 2 {
		t.Fatalf("plan/list payload = %+v", pl)
	}
	if len(persisted) != 5 || persisted[0].Type != EventPlanCreate {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every plan event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6a events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("plan/* events must not derive into messages: %+v", msgs)
	}
}

// TestPlanEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: plan/* rows interleaved with a real conversation do not
// appear in the derived history, and the conversation round-trips unchanged
// (D4 — adding the events never changes the turn/step structure).
func TestPlanEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("plan the release"))
	l.Append(EventPlanCreate, NewPlanCreate("goal", "goal-1", "Ship", nil))
	l.Append(EventAssistantMessage, NewAssistantMessage("Created goal-1.", nil, "stop"))
	l.Append(EventPlanStatus, NewPlanStatus("goal", "goal-1", "in-progress"))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "plan the release" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Created goal-1." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the plan rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestSpillEventsAppendAndReplay verifies the spill/* vocabulary
// (dispatch-m6c-2 §1 / D3): each of the four event types appends with the next
// Seq/version, round-trips its payload through the durable sink, survives a
// restart replay, and never derives into model messages (log-only).
func TestSpillEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	longContent := strings.Repeat("fact ", 100)
	if _, err := l.Append(EventSpillWrite, NewSpillWrite("memo-1", longContent)); err != nil {
		t.Fatalf("append spill/write: %v", err)
	}
	if _, err := l.Append(EventSpillRecall, NewSpillRecall("go", 2)); err != nil {
		t.Fatalf("append spill/recall: %v", err)
	}
	if _, err := l.Append(EventSpillList, NewSpillList(3)); err != nil {
		t.Fatalf("append spill/list: %v", err)
	}
	if _, err := l.Append(EventSpillDelete, NewSpillDelete("memo-1")); err != nil {
		t.Fatalf("append spill/delete: %v", err)
	}
	events := l.Events()
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	wantTypes := []string{EventSpillWrite, EventSpillRecall, EventSpillList, EventSpillDelete}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var sw spillWriteData
	if err := json.Unmarshal(events[0].Data, &sw); err != nil {
		t.Fatalf("unmarshal spill/write: %v", err)
	}
	if sw.ID != "memo-1" {
		t.Fatalf("spill/write id = %q, want memo-1", sw.ID)
	}
	// The content must be a bounded summary (dispatch-m6c-2 §1: content 摘要
	// 200-rune 有界) yet still carry the head.
	if got := len([]rune(sw.Content)); got > jobOutputSummaryMax+1 {
		t.Fatalf("spill/write content = %d runes, want <= %d+ellipsis", got, jobOutputSummaryMax)
	}
	if !strings.Contains(sw.Content, "fact") {
		t.Fatalf("spill/write content = %q, want it to carry the content head", sw.Content)
	}
	var sr spillRecallData
	if err := json.Unmarshal(events[1].Data, &sr); err != nil {
		t.Fatalf("unmarshal spill/recall: %v", err)
	}
	if sr.Query != "go" || sr.Count != 2 {
		t.Fatalf("spill/recall payload = %+v", sr)
	}
	var sl spillListData
	if err := json.Unmarshal(events[2].Data, &sl); err != nil {
		t.Fatalf("unmarshal spill/list: %v", err)
	}
	if sl.Count != 3 {
		t.Fatalf("spill/list payload = %+v", sl)
	}
	var sd spillDeleteData
	if err := json.Unmarshal(events[3].Data, &sd); err != nil {
		t.Fatalf("unmarshal spill/delete: %v", err)
	}
	if sd.ID != "memo-1" {
		t.Fatalf("spill/delete payload = %+v", sd)
	}
	if len(persisted) != 4 || persisted[0].Type != EventSpillWrite {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every spill event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("spill/* events must not derive into messages: %+v", msgs)
	}
}

// TestSpillEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: spill/* rows interleaved with a real conversation do not
// appear in the derived history, and the conversation round-trips unchanged
// (D4 — adding the events never changes the turn/step structure).
func TestSpillEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("remember this for me"))
	l.Append(EventSpillWrite, NewSpillWrite("memo-1", "the user prefers Go"))
	l.Append(EventAssistantMessage, NewAssistantMessage("Remembered.", nil, "stop"))
	l.Append(EventSpillRecall, NewSpillRecall("go", 1))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "remember this for me" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Remembered." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the spill rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestInteractEventsAppendAndReplay verifies the interact/* vocabulary
// (dispatch-m6d-2 §1 / D3): each of the four event types appends with the next
// Seq/version, round-trips its payload through the durable sink, survives a
// restart replay, and never derives into model messages (log-only).
func TestInteractEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventInteractRequest, NewInteractRequest("req-1", "bash")); err != nil {
		t.Fatalf("append interact/request: %v", err)
	}
	if _, err := l.Append(EventInteractResolve, NewInteractResolve("req-1", true)); err != nil {
		t.Fatalf("append interact/resolve: %v", err)
	}
	if _, err := l.Append(EventInteractDeny, NewInteractDeny("req-2")); err != nil {
		t.Fatalf("append interact/deny: %v", err)
	}
	if _, err := l.Append(EventInteractStatus, NewInteractStatus("req-1", "approved")); err != nil {
		t.Fatalf("append interact/status: %v", err)
	}
	events := l.Events()
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	wantTypes := []string{EventInteractRequest, EventInteractResolve, EventInteractDeny, EventInteractStatus}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var ir interactRequestData
	if err := json.Unmarshal(events[0].Data, &ir); err != nil {
		t.Fatalf("unmarshal interact/request: %v", err)
	}
	if ir.ID != "req-1" || ir.ToolName != "bash" {
		t.Fatalf("interact/request payload = %+v", ir)
	}
	var iv interactResolveData
	if err := json.Unmarshal(events[1].Data, &iv); err != nil {
		t.Fatalf("unmarshal interact/resolve: %v", err)
	}
	if iv.ID != "req-1" || !iv.Approved {
		t.Fatalf("interact/resolve payload = %+v", iv)
	}
	var id interactDenyData
	if err := json.Unmarshal(events[2].Data, &id); err != nil {
		t.Fatalf("unmarshal interact/deny: %v", err)
	}
	if id.ID != "req-2" {
		t.Fatalf("interact/deny payload = %+v", id)
	}
	var ist interactStatusData
	if err := json.Unmarshal(events[3].Data, &ist); err != nil {
		t.Fatalf("unmarshal interact/status: %v", err)
	}
	if ist.ID != "req-1" || ist.Status != "approved" {
		t.Fatalf("interact/status payload = %+v", ist)
	}
	if len(persisted) != 4 || persisted[0].Type != EventInteractRequest {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every interact event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("interact/* events must not derive into messages: %+v", msgs)
	}
}

// TestInteractEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: interact/* rows interleaved with a real conversation do
// not appear in the derived history, and the conversation round-trips
// unchanged (D4 — adding the events never changes the turn/step structure).
func TestInteractEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("run the report"))
	l.Append(EventInteractRequest, NewInteractRequest("req-1", "bash"))
	l.Append(EventAssistantMessage, NewAssistantMessage("Done.", nil, "stop"))
	l.Append(EventInteractDeny, NewInteractDeny("req-1"))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "run the report" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Done." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the interact rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestCodeRunEventAppendsAndReplays verifies the M6e-2 code/run vocabulary
// (dispatch-m6e-2 §1 / D3): it appends with the next Seq/version, round-trips
// its payload through the durable sink, survives a restart replay, and never
// derives into model messages (log-only — the model sees the run outcome
// through run_code's tool/result).
func TestCodeRunEventAppendsAndReplays(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventCodeRun, NewCodeRun("sh", 0, false, false)); err != nil {
		t.Fatalf("append code/run: %v", err)
	}
	if _, err := l.Append(EventCodeRun, NewCodeRun("sh", 3, true, true)); err != nil {
		t.Fatalf("append code/run (timed out): %v", err)
	}
	events := l.Events()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	for i, ev := range events {
		if ev.Type != EventCodeRun {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, EventCodeRun)
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var ok codeRunData
	if err := json.Unmarshal(events[0].Data, &ok); err != nil {
		t.Fatalf("unmarshal code/run: %v", err)
	}
	if ok.Lang != "sh" || ok.ExitCode != 0 || ok.TimedOut || ok.Truncated {
		t.Fatalf("code/run payload = %+v, want lang sh / exit 0 / no markers", ok)
	}
	var ko codeRunData
	if err := json.Unmarshal(events[1].Data, &ko); err != nil {
		t.Fatalf("unmarshal code/run (timed out): %v", err)
	}
	if ko.Lang != "sh" || ko.ExitCode != 3 || !ko.TimedOut || !ko.Truncated {
		t.Fatalf("code/run timed-out payload = %+v, want lang sh / exit 3 / timedOut+truncated", ko)
	}
	if len(persisted) != 2 || persisted[0].Type != EventCodeRun {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every code/run event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, ev := range fresh.Events() {
		if ev.Type != EventCodeRun {
			t.Fatalf("replayed type %d = %q, want %q", i, ev.Type, EventCodeRun)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("code/run events must not derive into messages: %+v", msgs)
	}
}

// TestCodeRunEventMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: code/run rows interleaved with a real conversation do not
// appear in the derived history, and the conversation round-trips unchanged
// (D4 — adding the event never changes the turn/step structure).
func TestCodeRunEventMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("run a quick script"))
	l.Append(EventCodeRun, NewCodeRun("sh", 0, false, false))
	l.Append(EventAssistantMessage, NewAssistantMessage("Ran it.", nil, "stop"))
	l.Append(EventCodeRun, NewCodeRun("sh", 7, true, true))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "run a quick script" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Ran it." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the code/run rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestMcpEventsAppendAndReplay verifies the M6f-2 mcp/* vocabulary
// (dispatch-m6f-2 §1 / D3): each of the two event types appends with the next
// Seq/version, round-trips its payload through the durable sink, survives a
// restart replay, and never derives into model messages (log-only — the model
// sees the tool table through mcp_list's tool/result and the call outcome
// through mcp_call's tool/result).
func TestMcpEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventMcpList, NewMcpList(3)); err != nil {
		t.Fatalf("append mcp/list: %v", err)
	}
	if _, err := l.Append(EventMcpCall, NewMcpCall("echo", false)); err != nil {
		t.Fatalf("append mcp/call: %v", err)
	}
	if _, err := l.Append(EventMcpCall, NewMcpCall("delete_file", true)); err != nil {
		t.Fatalf("append mcp/call (isError): %v", err)
	}
	events := l.Events()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	wantTypes := []string{EventMcpList, EventMcpCall, EventMcpCall}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var ml mcpListData
	if err := json.Unmarshal(events[0].Data, &ml); err != nil {
		t.Fatalf("unmarshal mcp/list: %v", err)
	}
	if ml.Count != 3 {
		t.Fatalf("mcp/list payload = %+v", ml)
	}
	var mc mcpCallData
	if err := json.Unmarshal(events[1].Data, &mc); err != nil {
		t.Fatalf("unmarshal mcp/call: %v", err)
	}
	if mc.Name != "echo" || mc.IsError {
		t.Fatalf("mcp/call payload = %+v", mc)
	}
	var me mcpCallData
	if err := json.Unmarshal(events[2].Data, &me); err != nil {
		t.Fatalf("unmarshal mcp/call (isError): %v", err)
	}
	if me.Name != "delete_file" || !me.IsError {
		t.Fatalf("mcp/call isError payload = %+v", me)
	}
	if len(persisted) != 3 || persisted[0].Type != EventMcpList {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every mcp event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("mcp/* events must not derive into messages: %+v", msgs)
	}
}

// TestMcpEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: mcp/* rows interleaved with a real conversation do not
// appear in the derived history, and the conversation round-trips unchanged
// (D4 — adding the events never changes the turn/step structure).
func TestMcpEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("list the tools"))
	l.Append(EventMcpList, NewMcpList(2))
	l.Append(EventAssistantMessage, NewAssistantMessage("Listed 2 tools.", nil, "stop"))
	l.Append(EventMcpCall, NewMcpCall("echo", false))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "list the tools" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Listed 2 tools." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the mcp rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestFsEventsAppendAndReplay verifies the M6f-3 fs/* vocabulary
// (dispatch-m6f-3 §2 / D3): each of the three event types appends with the
// next Seq/version, round-trips its payload through the durable sink, survives
// a restart replay, and never derives into model messages (log-only — the
// model sees the file content / write outcome / listing through the fs_* tools'
// tool/result events).
func TestFsEventsAppendAndReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	if _, err := l.Append(EventFsRead, NewFsRead("notes.txt", 12)); err != nil {
		t.Fatalf("append fs/read: %v", err)
	}
	if _, err := l.Append(EventFsWrite, NewFsWrite("notes.txt")); err != nil {
		t.Fatalf("append fs/write: %v", err)
	}
	if _, err := l.Append(EventFsList, NewFsList(".", 3)); err != nil {
		t.Fatalf("append fs/list: %v", err)
	}
	events := l.Events()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	wantTypes := []string{EventFsRead, EventFsWrite, EventFsList}
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("type %d = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.Version != EventVersion {
			t.Fatalf("event %d version = %d, want %d", i, ev.Version, EventVersion)
		}
	}
	// JSON round-trip of each payload.
	var fr fsReadData
	if err := json.Unmarshal(events[0].Data, &fr); err != nil {
		t.Fatalf("unmarshal fs/read: %v", err)
	}
	if fr.Path != "notes.txt" || fr.Size != 12 {
		t.Fatalf("fs/read payload = %+v, want path notes.txt / size 12", fr)
	}
	var fw fsWriteData
	if err := json.Unmarshal(events[1].Data, &fw); err != nil {
		t.Fatalf("unmarshal fs/write: %v", err)
	}
	if fw.Path != "notes.txt" {
		t.Fatalf("fs/write payload = %+v", fw)
	}
	var fl fsListData
	if err := json.Unmarshal(events[2].Data, &fl); err != nil {
		t.Fatalf("unmarshal fs/list: %v", err)
	}
	if fl.Dir != "." || fl.Count != 3 {
		t.Fatalf("fs/list payload = %+v, want dir . / count 3", fl)
	}
	if len(persisted) != 3 || persisted[0].Type != EventFsRead {
		t.Fatalf("sink (append path) = %+v", persisted)
	}
	// Restart replay: a fresh log rebuilt from what was persisted still sees
	// every fs event, and deriving history treats them all as opaque data
	// (log-only, like the M5/M6 events).
	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for i, want := range wantTypes {
		if got := fresh.Events()[i].Type; got != want {
			t.Fatalf("replayed type %d = %q, want %q", i, got, want)
		}
	}
	if msgs := fresh.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("fs/* events must not derive into messages: %+v", msgs)
	}
}

// TestFsEventsMixedWithConversationDeriveOnlyConversation verifies the
// log-only contract: fs/* rows interleaved with a real conversation do not
// appear in the derived history, and the conversation round-trips unchanged
// (D4 — adding the events never changes the turn/step structure).
func TestFsEventsMixedWithConversationDeriveOnlyConversation(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("read notes.txt"))
	l.Append(EventFsRead, NewFsRead("notes.txt", 5))
	l.Append(EventAssistantMessage, NewAssistantMessage("Read it.", nil, "stop"))
	l.Append(EventFsList, NewFsList(".", 1))
	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2 (conversation only): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "read notes.txt" {
		t.Fatalf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Read it." {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	// D1: the fs rows stay physically in the log.
	if got := len(l.Events()); got != 4 {
		t.Fatalf("log events = %d, want 4 (append-only)", got)
	}
}

// TestDeriveHistoryAssistantReasoningFoldsBeforeText verifies the M8 fold
// contract for a new-format assistant/message: the logged reasoning folds into
// a reasoning block placed before the text block, Message.Text() excludes it,
// and Message.Reasoning() recovers it (dispatch-m8 §5).
func TestDeriveHistoryAssistantReasoningFoldsBeforeText(t *testing.T) {
	l := New()
	l.Append(EventUserMessage, NewUserMessage("what is 2+2"))
	l.Append(EventAssistantMessage, NewAssistantMessage("It is 4.", nil, "stop", "Carry the two. Add the units."))

	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2: %+v", len(msgs), msgs)
	}
	asst := msgs[1]
	if asst.Role != llm.RoleAssistant {
		t.Fatalf("asst role = %q", asst.Role)
	}
	if len(asst.Content) != 2 {
		t.Fatalf("asst content = %+v, want [reasoning text]", asst.Content)
	}
	if asst.Content[0].Kind != llm.BlockReasoning || asst.Content[0].Text != "Carry the two. Add the units." {
		t.Fatalf("block 0 = %+v, want the reasoning block first", asst.Content[0])
	}
	if asst.Content[1].Kind != llm.BlockText || asst.Content[1].Text != "It is 4." {
		t.Fatalf("block 1 = %+v, want the text block after", asst.Content[1])
	}
	if asst.Text() != "It is 4." {
		t.Fatalf("Text() = %q, want the text only (reasoning excluded)", asst.Text())
	}
	if asst.Reasoning() != "Carry the two. Add the units." {
		t.Fatalf("Reasoning() = %q, want the reasoning text", asst.Reasoning())
	}
}

// TestDeriveHistoryAssistantReasoningSurvivesReplay verifies the M8 reasoning
// round-trip through the durable sink + restart replay (D1/D8): the logged
// reasoning stays in the assistant/message row and folds back identically after
// a fresh log is rebuilt from the persisted events.
func TestDeriveHistoryAssistantReasoningSurvivesReplay(t *testing.T) {
	var persisted []Event
	l := New()
	l.SetSink(func(ev Event) error {
		persisted = append(persisted, ev)
		return nil
	})
	l.Append(EventUserMessage, NewUserMessage("plan it"))
	l.Append(EventAssistantMessage, NewAssistantMessage("Done.", nil, "stop", "First consider the constraints."))

	fresh := New()
	if err := fresh.Restore(persisted); err != nil {
		t.Fatalf("restore: %v", err)
	}
	msgs := fresh.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2: %+v", len(msgs), msgs)
	}
	asst := msgs[1]
	if asst.Text() != "Done." || asst.Reasoning() != "First consider the constraints." {
		t.Fatalf("replayed assistant = %+v, want text+reasoning preserved", asst)
	}
	if asst.Content[0].Kind != llm.BlockReasoning {
		t.Fatalf("replayed assistant block 0 = %+v, want reasoning first", asst.Content[0])
	}
}

// TestDeriveHistoryUserContentBlocksReserved verifies the M8-3 reservation: a
// user/message carrying content blocks (images, M8-3) folds those blocks as-is,
// while a plain text-only user/message still folds into a single text block
// (dispatch-m8 §4/§5).
func TestDeriveHistoryUserContentBlocksReserved(t *testing.T) {
	l := New()
	// Plain text-only user message → single text block.
	l.Append(EventUserMessage, NewUserMessage("hello"))
	// A user message with explicit content blocks (M8-3 reservation; not
	// written by this milestone's helpers, but folded correctly if present).
	raw := `{"text":"","content":[{"Kind":"image","Image":{"ID":"att-1","MediaType":"image/png","Path":"/tmp/a.png"}}]}`
	if _, err := l.Append(EventUserMessage, json.RawMessage(raw)); err != nil {
		t.Fatalf("append blocks user/message: %v", err)
	}

	msgs := l.DeriveHistory()
	if len(msgs) != 2 {
		t.Fatalf("derived %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Content[0].Kind != llm.BlockText || msgs[0].Text() != "hello" {
		t.Fatalf("msg0 = %+v, want a single text block", msgs[0])
	}
	if len(msgs[1].Content) != 1 || msgs[1].Content[0].Kind != llm.BlockImage {
		t.Fatalf("msg1 content = %+v, want the reserved image block folded as-is", msgs[1].Content)
	}
	if !msgs[1].HasImage() {
		t.Fatal("user message with an image block must report HasImage")
	}
}

// TestDeriveHistoryOldFormatReplayNoRegression verifies the D8 old-format
// replay contract: legacy logs whose user/message and assistant/message rows
// carry only plain text (no content/reasoning fields) fold into single text
// blocks and reproduce the old string behavior exactly (dispatch-m8 §5/§6).
func TestDeriveHistoryOldFormatReplayNoRegression(t *testing.T) {
	// Old-format rows: pure strings, no content/reasoning keys at all.
	old := []Event{
		{Seq: 1, Type: EventUserMessage, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"what time is it"}`)},
		{Seq: 2, Type: EventAssistantChunk, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"Let "}`)},
		{Seq: 3, Type: EventAssistantChunk, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"me check"}`)},
		{Seq: 4, Type: EventAssistantMessage, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"text":"Let me check","toolCalls":null,"finishReason":"stop"}`)},
		{Seq: 5, Type: EventToolResult, At: time.Now().UTC(), Version: 1, Data: json.RawMessage(`{"callId":"c1","name":"get_time","output":"12:00"}`)},
	}
	l := New()
	if err := l.Restore(old); err != nil {
		t.Fatalf("restore old-format events: %v", err)
	}
	msgs := l.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "what time is it" {
		t.Fatalf("old user = %+v, want the plain string preserved", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Text() != "Let me check" || msgs[1].Reasoning() != "" {
		t.Fatalf("old assistant = %+v, want text preserved with no reasoning", msgs[1])
	}
	if len(msgs[1].Content) != 1 || msgs[1].Content[0].Kind != llm.BlockText {
		t.Fatalf("old assistant content = %+v, want a single text block (D8)", msgs[1].Content)
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].Text() != "12:00" {
		t.Fatalf("old tool = %+v, want the plain output preserved", msgs[2])
	}
}

// M8-3: NewUserMessageWithBlocks logs a user/message whose content carries an
// image block holding only the ImageRef — never the image bytes — and
// DeriveHistory folds that back into a model-visible user message with the
// image block intact (dispatch-m8-3 §4/§5: 事件 data 含 image block 且只有 ImageRef
// 无字节).
func TestNewUserMessageWithBlocksImageRefOnly(t *testing.T) {
	ref := llm.ImageRef{
		ID:        "a1b2c3",
		MediaType: "image/png",
		Bytes:     16,
		Width:     0,
		Height:    0,
		Path:      "attachments/a1b2c3.png",
	}
	payload := NewUserMessageWithBlocks("", []llm.ContentBlock{{Kind: llm.BlockImage, Image: ref}})

	// The payload marshals the image block as a ref-only shape (no bytes, no
	// base64, no data field — ContentBlock carries no byte payload).
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, forbidden := range []string{"base64", "dataURL"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("user/message payload must not carry image bytes (found %q): %s", forbidden, s)
		}
	}
	var d userMessageData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(d.Content) != 1 || d.Content[0].Kind != llm.BlockImage {
		t.Fatalf("content = %+v, want one image block", d.Content)
	}
	if d.Content[0].Image != ref {
		t.Fatalf("image = %+v, want ref %+v", d.Content[0].Image, ref)
	}

	// Append + derive: the block folds back into a user message with the image
	// block intact, and HasImage() reports it.
	l := New()
	if _, err := l.Append(EventUserMessage, payload); err != nil {
		t.Fatalf("append: %v", err)
	}
	msgs := l.DeriveHistory()
	if len(msgs) != 1 {
		t.Fatalf("derived %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Role != llm.RoleUser {
		t.Fatalf("role = %v, want user", m.Role)
	}
	if !m.HasImage() {
		t.Fatal("derived message must have an image block")
	}
	if len(m.Content) != 1 || m.Content[0].Kind != llm.BlockImage || m.Content[0].Image != ref {
		t.Fatalf("derived content = %+v, want the image ref block", m.Content)
	}
}

// Eval-3a: eval/run is an opaque log fact (ADR D-EVAL-5 / D3) — it appends
// with the lean payload and DeriveHistory ignores it, so the turn/step
// structure is unchanged (D4). The payload never carries the deliverable
// output, only a lean summary.
func TestEvalRunEventAppendsAndStaysOpaque(t *testing.T) {
	l := New()
	if _, err := l.Append(EventUserMessage, NewUserMessage("evaluate the deliverable")); err != nil {
		t.Fatalf("append user: %v", err)
	}
	ev, err := l.Append(EventEvalRun, NewEvalRun("eval-1", "todo-7", "pass", "criteria met", "rule", 2))
	if err != nil {
		t.Fatalf("append eval/run: %v", err)
	}
	if ev.Type != EventEvalRun {
		t.Fatalf("type = %q, want %q", ev.Type, EventEvalRun)
	}
	var d evalRunData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.ID != "eval-1" || d.TaskID != "todo-7" || d.Verdict != "pass" ||
		d.Reason != "criteria met" || d.EvaluatorKind != "rule" || d.CriteriaCount != 2 {
		t.Errorf("payload = %+v, want the lean eval/run summary", d)
	}
	if len(ev.Data) != len(mustMarshal(t, d)) {
		t.Errorf("payload length %d, want the exact marshaled summary (no deliverable output)", len(ev.Data))
	}

	// DeriveHistory treats eval/run as opaque: only the user message derives.
	msgs := l.DeriveHistory()
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("derived %d messages, want 1 user message (eval/run opaque)", len(msgs))
	}
}

// GAP-2: ralph/run is an opaque log fact (D-GAP-3 / D3) — it appends with the
// lean payload (objective + outcome markers, never the worker outputs) and
// DeriveHistory ignores it, so the turn/step structure is unchanged (D4).
func TestRalphRunEventAppendsAndStaysOpaque(t *testing.T) {
	l := New()
	if _, err := l.Append(EventUserMessage, NewUserMessage("run ralph over the objective")); err != nil {
		t.Fatalf("append user: %v", err)
	}
	ev, err := l.Append(EventRalphRun, NewRalphRun("deliver the report", 3, true, false))
	if err != nil {
		t.Fatalf("append ralph/run: %v", err)
	}
	if ev.Type != EventRalphRun {
		t.Fatalf("type = %q, want %q", ev.Type, EventRalphRun)
	}
	var d ralphRunData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Objective != "deliver the report" || d.Rounds != 3 || !d.Done || d.Blocked {
		t.Errorf("payload = %+v, want the lean ralph/run summary", d)
	}
	if len(ev.Data) != len(mustMarshal(t, d)) {
		t.Errorf("payload length %d, want the exact marshaled summary (no worker outputs)", len(ev.Data))
	}

	// DeriveHistory treats ralph/run as opaque: only the user message derives.
	msgs := l.DeriveHistory()
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("derived %d messages, want 1 user message (ralph/run opaque)", len(msgs))
	}
}

// GAP-3: workflow/run is an opaque log fact (D-GAP-2 / D3) — it appends with
// the lean counts payload (total/completed/failed, never the task outputs) and
// DeriveHistory ignores it, so the turn/step structure is unchanged (D4).
func TestWorkflowRunEventAppendsAndStaysOpaque(t *testing.T) {
	l := New()
	if _, err := l.Append(EventUserMessage, NewUserMessage("orchestrate the tasks")); err != nil {
		t.Fatalf("append user: %v", err)
	}
	ev, err := l.Append(EventWorkflowRun, NewWorkflowRun(5, 4, 1))
	if err != nil {
		t.Fatalf("append workflow/run: %v", err)
	}
	if ev.Type != EventWorkflowRun {
		t.Fatalf("type = %q, want %q", ev.Type, EventWorkflowRun)
	}
	var d workflowRunData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Total != 5 || d.Completed != 4 || d.Failed != 1 {
		t.Errorf("payload = %+v, want the lean workflow/run counts", d)
	}
	if len(ev.Data) != len(mustMarshal(t, d)) {
		t.Errorf("payload length %d, want the exact marshaled counts (no task outputs)", len(ev.Data))
	}

	// DeriveHistory treats workflow/run as opaque: only the user message
	// derives.
	msgs := l.DeriveHistory()
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("derived %d messages, want 1 user message (workflow/run opaque)", len(msgs))
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
