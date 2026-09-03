package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// byteTokens is a deterministic 1 token-per-byte estimator for tests.
func byteTokens(s string) int { return len(s) }

// fakeReader yields one text delta then EOF.
type fakeReader struct {
	done bool
	text string
}

func (r *fakeReader) Next() (llm.StreamEvent, error) {
	if !r.done {
		r.done = true
		return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: r.text}, nil
	}
	return llm.StreamEvent{}, io.EOF
}

// fakeLLM records every ChatRequest and answers with a fixed summary (or an
// error).
type fakeLLM struct {
	reqs []llm.ChatRequest
	text string
	err  error
}

type invalidSession struct {
	events []session.Event
}

func (s *invalidSession) Events() []session.Event { return append([]session.Event(nil), s.events...) }

func (s *invalidSession) Append(string, any) (session.Event, error) {
	return session.Event{}, errors.New("invalid test session is immutable")
}

func (f *fakeLLM) Stream(_ context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.reqs = append(f.reqs, req)
	return &fakeReader{text: f.text}, nil
}

// buildSession appends (type, payload) pairs to a fresh log.
func buildSession(t *testing.T, pairs ...any) *session.Log {
	t.Helper()
	l := session.New()
	for i := 0; i < len(pairs); i += 2 {
		typ := pairs[i].(string)
		data := pairs[i+1]
		if _, err := l.Append(typ, data); err != nil {
			t.Fatalf("append %d (%s): %v", i/2, typ, err)
		}
	}
	return l
}

func toolCallMsg(id, name string) any {
	return session.NewAssistantMessage("", []llm.ToolCall{{ID: id, Name: name, Arguments: `{}`}}, "tool_calls")
}

func toolResultMsg(id, name, output string) any {
	return session.NewToolResult(id, name, output, nil)
}

// threeTurnSession builds: u1 a1 u2 a2 u3 a3 (seqs 1..6).
func threeTurnSession(t *testing.T) *session.Log {
	return buildSession(t,
		session.EventUserMessage, session.NewUserMessage("q1"),
		session.EventAssistantMessage, session.NewAssistantMessage("a1", nil, "stop"),
		session.EventUserMessage, session.NewUserMessage("q2"),
		session.EventAssistantMessage, session.NewAssistantMessage("a2", nil, "stop"),
		session.EventUserMessage, session.NewUserMessage("q3"),
		session.EventAssistantMessage, session.NewAssistantMessage("a3", nil, "stop"),
	)
}

func TestDefaultTokenEstimate(t *testing.T) {
	if got := defaultTokenEstimate(""); got != 0 {
		t.Fatalf("empty estimate = %d, want 0", got)
	}
	if got := defaultTokenEstimate("abcdefgh"); got != 2 {
		t.Fatalf("estimate = %d, want 2 (8 bytes / 4)", got)
	}
}

func TestCompactIfNeededFailsClosedOnInvalidProjection(t *testing.T) {
	sess := &invalidSession{events: []session.Event{{Seq: 1, Type: "unknown/event", Data: json.RawMessage(`{}`)}}}
	eng := NewBasic(BasicOpts{TokenThreshold: 1, LLM: &fakeLLM{text: "S"}})
	if _, err := eng.CompactIfNeeded(context.Background(), sess, TriggerPressure); err == nil || !strings.Contains(err.Error(), "compaction history projection") {
		t.Fatalf("CompactIfNeeded error = %v, want a projection failure", err)
	}
}

func TestCompactIfNeededNoPressure(t *testing.T) {
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 1 << 20,
		RetainTurns:    1,
	})
	res, err := eng.CompactIfNeeded(context.Background(), sess, TriggerPressure)
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if res != nil {
		t.Fatalf("pressure under threshold must not compact, got %+v", res)
	}
	if got := len(sess.Events()); got != 6 {
		t.Fatalf("log grew to %d events, want 6", got)
	}
}

func TestCompactIfNeededTriggersOnPressure(t *testing.T) {
	sess := threeTurnSession(t) // derived surface = 12 bytes > threshold 5
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
	res, err := eng.CompactIfNeeded(context.Background(), sess, TriggerPressure)
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if res == nil {
		t.Fatal("expected a compaction result")
	}
	if res.CompactionID == "" {
		t.Fatal("expected a self-generated CompactionID")
	}
	if res.Summary != "S" {
		t.Fatalf("summary = %q, want S", res.Summary)
	}
	if res.ShadowedRange != [2]int64{1, 4} {
		t.Fatalf("shadowed range = %v, want [1 4]", res.ShadowedRange)
	}
	if !reflect.DeepEqual(res.ShadowedSeqs, []int64{1, 2, 3, 4}) {
		t.Fatalf("shadowed seqs = %v, want [1 2 3 4]", res.ShadowedSeqs)
	}
	if res.ShadowedTokens != 8 { // q1+a1+q2+a2 = 8 bytes
		t.Fatalf("shadowed tokens = %d, want 8", res.ShadowedTokens)
	}
	// The marker was appended (D1, append-only) and folding substitutes it for
	// the shadowed prefix.
	if got := len(sess.Events()); got != 7 {
		t.Fatalf("log events = %d, want 7 (old events physically retained)", got)
	}
	var replacement struct {
		SourceEventSeqs []uint64 `json:"sourceEventSeqs"`
	}
	if err := json.Unmarshal(sess.Events()[6].Data, &replacement); err != nil {
		t.Fatalf("decode replacement provenance: %v", err)
	}
	if !reflect.DeepEqual(replacement.SourceEventSeqs, []uint64{1, 2, 3, 4}) {
		t.Fatalf("replacement sourceEventSeqs = %v, want [1 2 3 4]", replacement.SourceEventSeqs)
	}
	msgs := sess.DeriveHistory()
	if len(msgs) != 3 {
		t.Fatalf("derived %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Text() != "S" {
		t.Fatalf("msg0 = %+v, want summary user", msgs[0])
	}
	if msgs[1].Text() != "q3" || msgs[2].Text() != "a3" {
		t.Fatalf("retained tail = %+v, want q3/a3", msgs[1:])
	}
}

func TestCompactionCarriesExactSurfaceMeasurementBreakdown(t *testing.T) {
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{
		LLM:         &fakeLLM{text: "checkpoint"},
		RetainTurns: 1,
		Meter: func(SessionLike) SurfaceMeasurement {
			return SurfaceMeasurement{
				LogRevision:             6,
				BaselineEstimatedTokens: 11,
				BaselineUsageTokens:     17,
				SurfaceDeltaTokens:      3,
				TotalTokens:             20,
				SurfaceTokens:           9,
				Nodes: []SurfaceNode{
					{Seq: 1, Tokens: 2}, {Seq: 2, Tokens: 4},
					{Seq: 3, Tokens: 1}, {Seq: 4, Tokens: 5},
					{Seq: 5, Tokens: 2}, {Seq: 6, Tokens: 2},
				},
			}
		},
	})
	res, err := eng.CompactNow(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if res == nil {
		t.Fatal("expected compaction result")
	}
	if res.ShadowedTokens != 12 {
		t.Fatalf("shadowed tokens = %d, want 12", res.ShadowedTokens)
	}
	if got := res.Measurement; got.LogRevision != 6 || got.BaselineEstimatedTokens != 11 || got.BaselineUsageTokens != 17 || got.SurfaceDeltaTokens != 3 || got.TotalTokens != 20 || got.SurfaceTokens != 9 {
		t.Fatalf("measurement breakdown = %+v", got)
	}
}

func TestCompactNowBoundsLargeSummaryRequests(t *testing.T) {
	var pairs []any
	for i := 0; i < 6; i++ {
		pairs = append(pairs,
			session.EventUserMessage, session.NewUserMessage(strings.Repeat("u", 16)),
			session.EventAssistantMessage, session.NewAssistantMessage(strings.Repeat("a", 16), nil, "stop"),
		)
	}
	sess := buildSession(t, pairs...)
	model := &fakeLLM{text: "checkpoint"}
	eng := NewBasic(BasicOpts{
		Tokenizer:          byteTokens,
		LLM:                model,
		Model:              "m",
		RetainTurns:        1,
		SummaryInputTokens: 20,
	})
	if _, err := eng.CompactNow(context.Background(), sess); err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if len(model.reqs) < 2 {
		t.Fatalf("requests = %d, want staged summary requests", len(model.reqs))
	}
	for i, req := range model.reqs {
		if len(req.Messages) == 0 {
			t.Fatalf("request %d has no messages", i)
		}
		conversation := req.Messages[:len(req.Messages)-1] // final item is the DSH directive
		tokens := 0
		for _, msg := range conversation {
			tokens += len(msg.ToolCallID)
			for _, block := range msg.Content {
				tokens += len(block.Text)
			}
			for _, call := range msg.ToolCalls {
				tokens += len(call.Name) + len(call.Arguments)
			}
		}
		if tokens > 20 {
			t.Fatalf("request %d conversation tokens = %d, want <= 20: %+v", i, tokens, conversation)
		}
	}
}

func TestCompactNowBoundsSingleOversizedMessage(t *testing.T) {
	sess := threeTurnSession(t)
	model := &fakeLLM{text: "checkpoint"}
	eng := NewBasic(BasicOpts{
		Tokenizer:          byteTokens,
		LLM:                model,
		Model:              "m",
		RetainTurns:        1,
		SummaryInputTokens: 1,
	})
	if _, err := eng.CompactNow(context.Background(), sess); err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if len(model.reqs) == 0 {
		t.Fatal("expected a bounded summary request")
	}
	for _, msg := range model.reqs[0].Messages[:len(model.reqs[0].Messages)-1] {
		if got := len(msg.Text()); got > 1 {
			t.Fatalf("oversized message text = %d bytes, want <= 1", got)
		}
	}
}

func TestCompactIfNeededContextOverflowForces(t *testing.T) {
	sess := threeTurnSession(t)
	llm := &fakeLLM{text: "S"}
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            llm,
		Model:          "m",
		TokenThreshold: 1 << 20, // far above the 12-byte surface
		RetainTurns:    1,
	})
	// Under pressure the pressure trigger is a no-op...
	res, err := eng.CompactIfNeeded(context.Background(), sess, TriggerPressure)
	if err != nil || res != nil {
		t.Fatalf("pressure trigger = %+v, %v; want nil, nil", res, err)
	}
	// ...but context-overflow forces one effective reduction.
	res, err = eng.CompactIfNeeded(context.Background(), sess, TriggerContextOverflow)
	if err != nil {
		t.Fatalf("overflow CompactIfNeeded: %v", err)
	}
	if res == nil {
		t.Fatal("context-overflow must force a compaction")
	}
	if got := len(sess.Events()); got != 7 {
		t.Fatalf("log events = %d, want 7 after forced compaction", got)
	}
}

func TestRetainTurnsKeepsTail(t *testing.T) {
	// 4 turns: u1 a1 u2 a2 u3 a3 u4 a4 (seqs 1..8).
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("one"),
		session.EventAssistantMessage, session.NewAssistantMessage("A1", nil, "stop"),
		session.EventUserMessage, session.NewUserMessage("two"),
		session.EventAssistantMessage, session.NewAssistantMessage("A2", nil, "stop"),
		session.EventUserMessage, session.NewUserMessage("three"),
		session.EventAssistantMessage, session.NewAssistantMessage("A3", nil, "stop"),
		session.EventUserMessage, session.NewUserMessage("four"),
		session.EventAssistantMessage, session.NewAssistantMessage("A4", nil, "stop"),
	)
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "folded"},
		Model:          "m",
		TokenThreshold: 1 << 20,
		RetainTurns:    2, // keep the last 2 user turns (three/four)
	})
	res, err := eng.CompactNow(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if res == nil {
		t.Fatal("expected compaction")
	}
	// Shadowed prefix = everything before the first retained user ("three" at
	// seq 5): seqs 1..4.
	if res.ShadowedRange != [2]int64{1, 4} {
		t.Fatalf("shadowed range = %v, want [1 4]", res.ShadowedRange)
	}
	for _, seq := range res.ShadowedSeqs {
		if seq >= 5 {
			t.Fatalf("shadowed seq %d intrudes into the retained tail", seq)
		}
	}
	msgs := sess.DeriveHistory()
	want := []string{"folded", "three", "A3", "four", "A4"}
	if len(msgs) != len(want) {
		t.Fatalf("derived %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Text() != w {
			t.Fatalf("msg %d = %q, want %q", i, msgs[i].Text(), w)
		}
	}
}

// TestPairingCorrectionUnbalancedEnd verifies the shadowed prefix is corrected
// so it never cuts an assistant tool_calls from its tool result: the last
// shadowed turn's tool call has its result in the retained tail, so the End
// boundary walks back to before the call.
func TestPairingCorrectionUnbalancedEnd(t *testing.T) {
	// u1 a1(callX) r1 a2(callY) u3 a3 r2 — the callY/result pair straddles the
	// natural RetainTurns cut (its result arrives after the next user turn).
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", "resX"),
		session.EventUserMessage, session.NewUserMessage("u2"),
		session.EventAssistantMessage, toolCallMsg("callY", "y"),
		session.EventUserMessage, session.NewUserMessage("u3"),
		session.EventAssistantMessage, session.NewAssistantMessage("a3", nil, "stop"),
		session.EventToolResult, toolResultMsg("callY", "y", "resY"),
	)
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 1 << 20,
		RetainTurns:    1, // keep u3
	})
	res, err := eng.CompactNow(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if res == nil {
		t.Fatal("expected compaction")
	}
	// maxEnd = seq of u3 - 1 = 5, but prefix up to 5 has callY unclosed; the
	// largest balanced End <= 5 is 4 (before a2/callY).
	if res.ShadowedRange != [2]int64{1, 4} {
		t.Fatalf("shadowed range = %v, want [1 4] (pairing-corrected end)", res.ShadowedRange)
	}
	for _, seq := range res.ShadowedSeqs {
		if seq == 5 {
			t.Fatalf("seq 5 (callY) must stay outside the shadowed range: %v", res.ShadowedSeqs)
		}
	}
	// The callY tool pair (seqs 5, 8) survives intact in the derived tail.
	msgs := sess.DeriveHistory()
	foundCall, foundRes := false, false
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == "callY" {
				foundCall = true
			}
		}
		if m.Role == llm.RoleTool && m.ToolCallID == "callY" {
			foundRes = true
		}
	}
	if !foundCall || !foundRes {
		t.Fatalf("callY pair broken in derived tail: call=%v res=%v msgs=%+v", foundCall, foundRes, msgs)
	}
}

func TestCompactNowAlwaysCompacts(t *testing.T) {
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 1 << 20, // under pressure
		RetainTurns:    1,
	})
	res, err := eng.CompactNow(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if res == nil {
		t.Fatal("CompactNow must compact even under pressure")
	}
}

func TestCompactNowNothingToCompact(t *testing.T) {
	empty := session.New()
	eng := NewBasic(BasicOpts{LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	if res, err := eng.CompactNow(context.Background(), empty); err != nil || res != nil {
		t.Fatalf("empty session = %+v, %v; want nil, nil", res, err)
	}
	// One turn with RetainTurns=1: nothing before the retained tail.
	one := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("only"),
		session.EventAssistantMessage, session.NewAssistantMessage("answer", nil, "stop"),
	)
	if res, err := eng.CompactNow(context.Background(), one); err != nil || res != nil {
		t.Fatalf("single-turn session = %+v, %v; want nil, nil", res, err)
	}
	if got := len(one.Events()); got != 2 {
		t.Fatalf("single-turn log grew to %d events, want 2", got)
	}
}

// TestSummaryContextIsShadowedPart verifies the LLM is called with exactly the
// shadowed portion of the derived history (the summary context).
func TestSummaryContextIsShadowedPart(t *testing.T) {
	sess := threeTurnSession(t)
	fake := &fakeLLM{text: "S"}
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            fake,
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
	if _, err := eng.CompactIfNeeded(context.Background(), sess, TriggerPressure); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if len(fake.reqs) != 1 {
		t.Fatalf("model called %d times, want 1", len(fake.reqs))
	}
	msgs := fake.reqs[0].Messages
	// shadowed [q1 a1 q2 a2] + final dsh compaction instruction
	if len(msgs) != 5 {
		t.Fatalf("summary request has %d messages, want 5: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[len(msgs)-1].Role != llm.RoleUser {
		t.Fatalf("summary request boundary roles = %q..%q, want user conversation + final user instruction", msgs[0].Role, msgs[len(msgs)-1].Role)
	}
	want := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("q1")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.Text("a1")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("q2")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.Text("a2")}},
	}
	for i := range want {
		if msgs[i].Role != want[i].Role || msgs[i].Text() != want[i].Text() {
			t.Fatalf("shadowed msg %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
	if !strings.Contains(msgs[len(msgs)-1].Text(), "compaction engine") {
		t.Fatalf("final summary instruction = %q, want dsh compaction instruction", msgs[len(msgs)-1].Text())
	}
	// The retained tail must not leak into the summary context.
	var sb strings.Builder
	for _, m := range msgs[1:] {
		sb.WriteString(m.Text())
	}
	if strings.Contains(sb.String(), "q3") {
		t.Fatalf("retained tail leaked into summary context: %+v", msgs[1:])
	}
}

func TestCompactSummaryFailureReturnsError(t *testing.T) {
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{err: errors.New("model down")},
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
	if _, err := eng.CompactNow(context.Background(), sess); err == nil {
		t.Fatal("expected an error from a failing summary model")
	}
	if got := len(sess.Events()); got != 6 {
		t.Fatalf("log changed to %d events after failed summary, want 6", got)
	}
}

func TestCompactAppendFailureReturnsError(t *testing.T) {
	sess := threeTurnSession(t)
	sess.SetSink(func(session.Event) error { return errors.New("disk full") })
	eng := NewBasic(BasicOpts{
		Tokenizer:      byteTokens,
		LLM:            &fakeLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
	if _, err := eng.CompactNow(context.Background(), sess); err == nil {
		t.Fatal("expected an error when the summary marker cannot be appended")
	}
	if got := len(sess.Events()); got != 6 {
		t.Fatalf("log drifted to %d events after failed append, want 6", got)
	}
}

func TestCompactRegionCorrectsEndBoundary(t *testing.T) {
	// u1 a1(callX) r1 u2 a2(callY) r2 — requesting [1,5] would cut callY from
	// its result r2; the End boundary corrects to 4.
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", "resX"),
		session.EventUserMessage, session.NewUserMessage("u2"),
		session.EventAssistantMessage, toolCallMsg("callY", "y"),
		session.EventToolResult, toolResultMsg("callY", "y", "resY"),
	)
	eng := NewBasic(BasicOpts{Tokenizer: byteTokens, LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	res, err := eng.CompactRegion(context.Background(), sess, 1, 5)
	if err != nil {
		t.Fatalf("CompactRegion: %v", err)
	}
	if res == nil {
		t.Fatal("expected a compaction result")
	}
	if res.ShadowedRange != [2]int64{1, 4} {
		t.Fatalf("shadowed range = %v, want [1 4]", res.ShadowedRange)
	}
	if !reflect.DeepEqual(res.ShadowedSeqs, []int64{1, 2, 3, 4}) {
		t.Fatalf("shadowed seqs = %v, want [1 2 3 4]", res.ShadowedSeqs)
	}
	// callY (seq 5) and its result (seq 6) are both retained.
	msgs := sess.DeriveHistory()
	foundCall, foundRes := false, false
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == "callY" {
				foundCall = true
			}
		}
		if m.Role == llm.RoleTool && m.ToolCallID == "callY" {
			foundRes = true
		}
	}
	if !foundCall || !foundRes {
		t.Fatalf("callY pair broken: call=%v res=%v msgs=%+v", foundCall, foundRes, msgs)
	}
}

func TestCompactRegionCorrectsStartBoundary(t *testing.T) {
	// u1 a1(callX) r1 u2 a2 — requesting [3,5] starts at the tool result whose
	// call is retained (seq 2), so Start is pushed forward past the orphan to 4.
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", "resX"),
		session.EventUserMessage, session.NewUserMessage("u2"),
		session.EventAssistantMessage, session.NewAssistantMessage("a2", nil, "stop"),
	)
	eng := NewBasic(BasicOpts{Tokenizer: byteTokens, LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	res, err := eng.CompactRegion(context.Background(), sess, 3, 5)
	if err != nil {
		t.Fatalf("CompactRegion: %v", err)
	}
	if res == nil {
		t.Fatal("expected a compaction result")
	}
	if res.ShadowedRange != [2]int64{4, 5} {
		t.Fatalf("shadowed range = %v, want [4 5]", res.ShadowedRange)
	}
	// The tool result (seq 3) stays visible next to its call (seq 2).
	msgs := sess.DeriveHistory()
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID == "callX" {
			return // callX result survived with its call
		}
	}
	t.Fatalf("callX tool result was shadowed away from its call: %+v", msgs)
}

func TestCompactRegionNoValidRange(t *testing.T) {
	// Requesting only the mid-pair assistant message leaves no balanced,
	// user-containing range.
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", "resX"),
	)
	eng := NewBasic(BasicOpts{Tokenizer: byteTokens, LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	res, err := eng.CompactRegion(context.Background(), sess, 2, 2)
	if err != nil {
		t.Fatalf("CompactRegion: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil for a range with no balanced, user-containing span, got %+v", res)
	}
	if got := len(sess.Events()); got != 3 {
		t.Fatalf("log grew to %d events, want 3", got)
	}
}

func TestCompactRegionOutOfBounds(t *testing.T) {
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{Tokenizer: byteTokens, LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	// Inverted, entirely-after, and entirely-before ranges compact nothing.
	for _, r := range [][2]int64{{5, 2}, {10, 20}, {-3, -1}} {
		if res, err := eng.CompactRegion(context.Background(), sess, r[0], r[1]); err != nil || res != nil {
			t.Fatalf("CompactRegion(%d,%d) = %+v, %v; want nil, nil", r[0], r[1], res, err)
		}
	}
	if got := len(sess.Events()); got != 6 {
		t.Fatalf("log grew to %d events after out-of-bounds regions, want 6", got)
	}
}

func TestCompactRegionClampsToLogBounds(t *testing.T) {
	// A range wider than the log is clamped to [first, last] and compacted.
	sess := threeTurnSession(t)
	eng := NewBasic(BasicOpts{Tokenizer: byteTokens, LLM: &fakeLLM{text: "S"}, Model: "m", RetainTurns: 1})
	res, err := eng.CompactRegion(context.Background(), sess, 0, 100)
	if err != nil {
		t.Fatalf("CompactRegion: %v", err)
	}
	if res == nil {
		t.Fatal("expected a compaction result")
	}
	if res.ShadowedRange != [2]int64{1, 6} {
		t.Fatalf("shadowed range = %v, want [1 6]", res.ShadowedRange)
	}
	msgs := sess.DeriveHistory()
	if len(msgs) != 1 || msgs[0].Text() != "S" {
		t.Fatalf("derived = %+v, want a single summary", msgs)
	}
}

func TestToolPairingBalancedBeforeAfter(t *testing.T) {
	// A single complete pair: u1 a1(callX) r1.
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", "resX"),
	)
	if !ToolPairingBalancedBefore(sess, 1) {
		t.Fatal("Before(1): empty prefix must be balanced")
	}
	if !ToolPairingBalancedBefore(sess, 2) {
		t.Fatal("Before(2): prefix {1} has no calls, must be balanced")
	}
	if ToolPairingBalancedBefore(sess, 3) {
		t.Fatal("Before(3): prefix {1,2} has an unclosed callX")
	}
	if !ToolPairingBalancedAfter(sess, 1) {
		t.Fatal("After(1): prefix {1} is balanced")
	}
	if ToolPairingBalancedAfter(sess, 2) {
		t.Fatal("After(2): prefix {1,2} has an unclosed callX")
	}
	if !ToolPairingBalancedAfter(sess, 3) {
		t.Fatal("After(3): callX answered inside the prefix")
	}
	if !ToolPairingBalancedAfter(sess, 4) {
		t.Fatal("After(4): whole log is balanced")
	}
	if !ToolPairingBalancedAfter(sess, 0) {
		t.Fatal("After(0): empty prefix must be balanced")
	}
}

func TestToolPairingBalancedUnansweredCall(t *testing.T) {
	// u1 a1(callX) u2 — callX never answered.
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventUserMessage, session.NewUserMessage("u2"),
	)
	if ToolPairingBalancedAfter(sess, 3) {
		t.Fatal("After(3): callX was never answered, must be unbalanced")
	}
	if ToolPairingBalancedBefore(sess, 3) {
		t.Fatal("Before(3): prefix {1,2} has an unclosed callX")
	}
}

func TestToolPairingIgnoresOpaqueEvents(t *testing.T) {
	// job/subagent events between messages must not disturb pairing.
	l := session.New()
	l.Append(session.EventUserMessage, session.NewUserMessage("u1"))
	l.Append(session.EventJobStart, session.NewJobStart("j1", "bash", "echo", "s1"))
	l.Append(session.EventAssistantMessage, toolCallMsg("callX", "x"))
	l.Append(session.EventSubagentStart, session.NewSubagentStart("c1", "spawn", "s1", "worker"))
	l.Append(session.EventToolResult, toolResultMsg("callX", "x", "resX"))
	if !ToolPairingBalancedAfter(l, 5) {
		t.Fatal("After(5): opaque events must not affect pairing balance")
	}
}
