package compaction

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

func TestPruneResultTextHeadMiddleTail(t *testing.T) {
	out := strings.Repeat("a", 1000)
	got := pruneResultText(out, 200)
	if len(got) > 200 {
		t.Fatalf("replaced text = %d bytes, want <= 200", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("replaced text is not valid UTF-8")
	}
	// head keeps the leading bytes, tail the trailing bytes, marker in the middle.
	if !strings.HasPrefix(got, strings.Repeat("a", 80)) {
		t.Fatalf("head not preserved: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("a", 80)) {
		t.Fatalf("tail not preserved: %q", got)
	}
	if !strings.Contains(got, "[truncated") {
		t.Fatalf("missing truncation marker: %q", got)
	}
	if !strings.Contains(got, "bytes omitted") {
		t.Fatalf("missing omitted count: %q", got)
	}
}

func TestPruneResultTextUnderBudgetReturnsAsIs(t *testing.T) {
	out := strings.Repeat("b", 100)
	if got := pruneResultText(out, 200); got != out {
		t.Fatalf("under-budget output must be unchanged")
	}
}

func TestPruneResultTextUnicodeBoundary(t *testing.T) {
	// 1000 three-byte runes; a 100-byte budget must never split a rune.
	out := strings.Repeat("你", 1000)
	got := pruneResultText(out, 100)
	if len(got) > 100 {
		t.Fatalf("replaced text = %d bytes, want <= 100", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("replaced text is not valid UTF-8 (a rune was split)")
	}
	// Extract the head and tail segments around the marker and verify each is a
	// clean prefix/suffix of the original.
	idx := strings.Index(got, "[truncated")
	if idx < 0 {
		t.Fatalf("no marker in %q", got)
	}
	head := strings.TrimSuffix(got[:idx], "\n…")
	tail := strings.TrimPrefix(got[idx+strings.Index(got[idx:], "]\n"):], "]\n")
	if !strings.HasPrefix(out, head) {
		t.Fatalf("head %q is not a UTF-8 prefix of the original", head)
	}
	if !strings.HasSuffix(out, tail) {
		t.Fatalf("tail %q is not a UTF-8 suffix of the original", tail)
	}
}

func TestTruncateHelpersNeverSplitRune(t *testing.T) {
	out := strings.Repeat("好", 100) // 300 bytes, 3 per rune
	for _, n := range []int{0, 1, 2, 3, 4, 5, 50, 151, 299, 300} {
		head := truncateUTF8(out, n)
		if !utf8.ValidString(head) || !strings.HasPrefix(out, head) {
			t.Fatalf("truncateUTF8(%d) = %q splits or corrupts a rune", n, head)
		}
		tail := lastBytes(out, n)
		if !utf8.ValidString(tail) || !strings.HasSuffix(out, tail) {
			t.Fatalf("lastBytes(%d) = %q splits or corrupts a rune", n, tail)
		}
		// When the budget is at most half the original, head and tail cannot
		// overlap; beyond that they legitimately cover the whole string.
		if n <= len(out)/2 && len(head)+len(tail) > len(out) {
			t.Fatalf("head+tail (%d+%d) exceed the original %d", len(head), len(tail), len(out))
		}
	}
}

func TestPruneToolResultsReplacesOversizedOnly(t *testing.T) {
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", strings.Repeat("a", 1000)), // seq 3: oversized
		session.EventUserMessage, session.NewUserMessage("u2"),
		session.EventAssistantMessage, toolCallMsg("callY", "y"),
		session.EventToolResult, toolResultMsg("callY", "y", strings.Repeat("b", 50)), // seq 6: under budget
	)
	res, err := PruneToolResults(sess, 200)
	if err != nil {
		t.Fatalf("PruneToolResults: %v", err)
	}
	if !reflectDeepEqualInt64(res.Replaced, []int64{3}) {
		t.Fatalf("replaced = %v, want [3]", res.Replaced)
	}
	repl := pruneResultText(strings.Repeat("a", 1000), 200)
	if want := 1000 - len(repl); res.SavedBytes != want {
		t.Fatalf("saved = %d, want %d", res.SavedBytes, want)
	}
	if res.SavedBytes <= 0 {
		t.Fatalf("saved bytes = %d, want > 0", res.SavedBytes)
	}
}

func TestPruneToolResultsMultiple(t *testing.T) {
	sess := buildSession(t,
		session.EventUserMessage, session.NewUserMessage("u1"),
		session.EventAssistantMessage, toolCallMsg("callX", "x"),
		session.EventToolResult, toolResultMsg("callX", "x", strings.Repeat("a", 1000)), // seq 3
		session.EventAssistantMessage, toolCallMsg("callY", "y"),
		session.EventToolResult, toolResultMsg("callY", "y", strings.Repeat("c", 500)), // seq 5
	)
	res, err := PruneToolResults(sess, 100)
	if err != nil {
		t.Fatalf("PruneToolResults: %v", err)
	}
	if !reflectDeepEqualInt64(res.Replaced, []int64{3, 5}) {
		t.Fatalf("replaced = %v, want [3 5]", res.Replaced)
	}
	want := 1000 - len(pruneResultText(strings.Repeat("a", 1000), 100)) +
		500 - len(pruneResultText(strings.Repeat("c", 500), 100))
	if res.SavedBytes != want {
		t.Fatalf("saved = %d, want %d", res.SavedBytes, want)
	}
}

func TestPruneIgnoresToolErrorAndNonResults(t *testing.T) {
	// A huge tool/error and huge assistant text must not be pruned — only
	// tool/result events participate.
	l := session.New()
	l.Append(session.EventUserMessage, session.NewUserMessage("u1"))
	l.Append(session.EventAssistantMessage, session.NewAssistantMessage(strings.Repeat("z", 5000), nil, "stop"))
	l.Append(session.EventToolError, session.NewToolError("callX", "x", strings.Repeat("e", 5000)))
	res, err := PruneToolResults(l, 100)
	if err != nil {
		t.Fatalf("PruneToolResults: %v", err)
	}
	if len(res.Replaced) != 0 || res.SavedBytes != 0 {
		t.Fatalf("pruned non-tool-result events: %+v", res)
	}
}

func TestPruneToolResultsRejectsNonPositiveBudget(t *testing.T) {
	sess := session.New()
	for _, n := range []int{0, -1} {
		if _, err := PruneToolResults(sess, n); err == nil {
			t.Fatalf("PruneToolResults(%d) must error", n)
		}
	}
}

func reflectDeepEqualInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
