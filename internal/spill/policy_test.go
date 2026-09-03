package spill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// testEvent builds one session event carrying data through the session package's
// authoritative payload builders.
func testEvent(t *testing.T, seq uint64, typ string, data any) session.Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal event %d: %v", seq, err)
	}
	return session.Event{Seq: seq, Type: typ, At: time.Now().UTC(), Version: session.EventVersion, Data: raw}
}

func TestWorthRemembering(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"blank", "   ", false},
		{"chit-chat greeting cn", "你好", false},
		{"chit-chat ack cn", "好的", false},
		{"chit-chat ack en", "ok", false},
		{"chit-chat thanks", "thanks", false},
		{"short no marker", "今天天气不错", false},
		{"short no marker en", "Let me check", false},
		{"short conclusive cn", "好的，我会记住的。", true},
		{"short conclusive cn key", "重要：明天开会。", true},
		{"short conclusive en", "Remember this fact.", true},
		{"long informational no marker", "这是一条比较长的信息性文本，虽然没有任何结论性标记，但因为它足够长，所以也应当被自动沉淀到长期记忆里。", true},
		{"long english no marker", "The deployment pipeline builds, tests and publishes the release artifact to the internal registry.", true},
	}
	for _, c := range cases {
		if got := worthRemembering(c.text); got != c.want {
			t.Errorf("%s: worthRemembering(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
}

func TestAutoSpillCandidatesExtraction(t *testing.T) {
	events := []session.Event{
		testEvent(t, 1, session.EventUserMessage, session.NewUserMessage("what is the release date?")),
		testEvent(t, 2, session.EventToolResult, session.NewToolResult("call_1", "search_tool", "The release is scheduled for 2026-09-01.", nil)),
		testEvent(t, 3, session.EventAssistantMessage, session.NewAssistantMessage("durable note", nil, "stop")),
	}
	got := autoSpillCandidates(events)
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
	}
	if got[0].content != "The release is scheduled for 2026-09-01." {
		t.Fatalf("candidate[0].content = %q", got[0].content)
	}
	if got[1].content != "durable note" {
		t.Fatalf("candidate[1].content = %q", got[1].content)
	}
	if got[1].source != "session:3" {
		t.Fatalf("candidate[1].source = %q, want session:3", got[1].source)
	}
}

func TestAutoSpillSkipsToolCallFrames(t *testing.T) {
	events := []session.Event{
		testEvent(t, 1, session.EventUserMessage, session.NewUserMessage("read the file")),
		testEvent(t, 2, session.EventAssistantMessage, session.NewAssistantMessage("", []llm.ToolCall{{ID: "call_1", Name: "read", Arguments: `{"path":"x"}`}}, "")),
		testEvent(t, 3, session.EventToolResult, session.NewToolResult("call_1", "read", "file contents are short", nil)),
	}
	// The tool-call frame is excluded and the short tool output is under the
	// length threshold with no marker — nothing worth remembering.
	if got := autoSpillCandidates(events); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

func TestAutoSpillMidTurnTextAndFinal(t *testing.T) {
	events := []session.Event{
		testEvent(t, 1, session.EventUserMessage, session.NewUserMessage("query the db")),
		// Non-empty assistant text alongside a tool call is not a frame: it is
		// extracted as a candidate too.
		testEvent(t, 2, session.EventAssistantMessage, session.NewAssistantMessage("Let me query the database for the total revenue.", []llm.ToolCall{{ID: "c", Name: "db_query", Arguments: `{}`}}, "")),
		testEvent(t, 3, session.EventToolResult, session.NewToolResult("c", "db_query", "查询结果：总营收为 1,234,567 元。", nil)),
		testEvent(t, 4, session.EventAssistantMessage, session.NewAssistantMessage("结论：总营收 123 万。", nil, "stop")),
	}
	got := autoSpillCandidates(events)
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3: %+v", len(got), got)
	}
	if got[0].content != "Let me query the database for the total revenue." {
		t.Fatalf("candidate[0].content = %q", got[0].content)
	}
	if got[1].source != "session:3:tool:db_query" {
		t.Fatalf("candidate[1].source = %q, want session:3:tool:db_query", got[1].source)
	}
	if got[2].content != "结论：总营收 123 万。" {
		t.Fatalf("candidate[2].content = %q", got[2].content)
	}
}

func TestAutoSpillCountAndDedup(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	events := []session.Event{
		testEvent(t, 1, session.EventUserMessage, session.NewUserMessage("hello")),
		testEvent(t, 2, session.EventAssistantMessage, session.NewAssistantMessage("好的", nil, "stop")),                           // chit-chat → filtered
		testEvent(t, 3, session.EventAssistantMessage, session.NewAssistantMessage("记住：项目用 Go 编写。", nil, "stop")),                // conclusive → spilled
		testEvent(t, 4, session.EventToolResult, session.NewToolResult("call_1", "bash", "The build passed all 42 tests.", nil)), // long → spilled
	}

	n, err := eng.AutoSpill(context.Background(), events)
	if err != nil {
		t.Fatalf("AutoSpill: %v", err)
	}
	if n != 2 {
		t.Fatalf("AutoSpill added %d, want 2", n)
	}

	// Idempotent: re-running over the same events adds nothing new.
	n, err = eng.AutoSpill(context.Background(), events)
	if err != nil {
		t.Fatalf("re-AutoSpill: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-AutoSpill added %d, want 0", n)
	}

	all, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("stored %d memos, want 2: %+v", len(all), all)
	}
}

func TestAutoSpillDedupAcrossEvents(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	// The same conclusive statement repeated twice collapses to one memo.
	events := []session.Event{
		testEvent(t, 1, session.EventAssistantMessage, session.NewAssistantMessage("记住：服务器重启于凌晨三点。", nil, "stop")),
		testEvent(t, 2, session.EventAssistantMessage, session.NewAssistantMessage("记住：服务器重启于凌晨三点。", nil, "stop")),
	}
	n, err := eng.AutoSpill(context.Background(), events)
	if err != nil {
		t.Fatalf("AutoSpill: %v", err)
	}
	if n != 1 {
		t.Fatalf("AutoSpill added %d, want 1 (deduped)", n)
	}
}

func TestAutoSpillEmptyEvents(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()
	n, err := eng.AutoSpill(context.Background(), nil)
	if err != nil {
		t.Fatalf("AutoSpill(nil): %v", err)
	}
	if n != 0 {
		t.Fatalf("AutoSpill(nil) added %d, want 0", n)
	}
}

func TestSummarizeToolResult(t *testing.T) {
	long := strings.Repeat("x", maxToolResultRunes+50)
	s := summarizeToolResult(long)
	if utf8.RuneCountInString(s) != maxToolResultRunes+1 {
		t.Fatalf("summary length = %d, want %d (truncated + ellipsis)", utf8.RuneCountInString(s), maxToolResultRunes+1)
	}
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("summary %q missing ellipsis suffix", s)
	}

	// Short output is trimmed but not truncated.
	if got := summarizeToolResult("  hello  "); got != "hello" {
		t.Fatalf("short summary = %q, want hello", got)
	}
	if got := summarizeToolResult("   "); got != "" {
		t.Fatalf("blank summary = %q, want empty", got)
	}
}

func TestAutoSpillCandidatesPureDeterministic(t *testing.T) {
	events := []session.Event{
		testEvent(t, 1, session.EventUserMessage, session.NewUserMessage("ping")),
		testEvent(t, 2, session.EventAssistantMessage, session.NewAssistantMessage("记住：纯函数无副作用。", nil, "stop")),
	}
	first := autoSpillCandidates(events)
	second := autoSpillCandidates(events)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("candidate counts differ: %d vs %d", len(first), len(second))
	}
	if first[0].content != second[0].content || first[0].source != second[0].source {
		t.Fatalf("policy is not deterministic: %+v vs %+v", first, second)
	}
}
