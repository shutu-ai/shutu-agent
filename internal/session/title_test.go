package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeTitleStripsControlsAndCollapsesWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "写一首诗", "写一首诗"},
		{"collapse whitespace", "   hello \n\t   world  ", "hello world"},
		{"csi color", "\x1b[31mred\x1b[0m", "red"},
		{"osc hyperlink", "\x1b]8;;http://x\x07link\x1b]8;;\x07", "link"},
		{"bell within osc", "\x1b]0;title\x07rest", "rest"},
		{"directional bidi", "abc\u202eevil\u202c", "abcevil"},
		{"control char", "a\x00b\x1fc", "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeTitle(c.in, TitleMaxBytes); got != c.want {
				t.Fatalf("NormalizeTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeTitleByteBoundNoCodePointSplit(t *testing.T) {
	// A 4-byte rune at the boundary must be either kept whole or dropped.
	input := strings.Repeat("a", TitleMaxBytes-1) + "汉" // 汉 is 3 UTF-8 bytes
	got := NormalizeTitle(input, TitleMaxBytes)
	if got != strings.Repeat("a", TitleMaxBytes-1) {
		t.Fatalf("NormalizeTitle byte-bound = %q, want %d 'a's", got, TitleMaxBytes-1)
	}
	// Emoji (4 bytes) at the boundary is dropped whole.
	emojiInput := strings.Repeat("a", TitleMaxBytes-3) + "🚀"
	if got := NormalizeTitle(emojiInput, TitleMaxBytes); got != strings.Repeat("a", TitleMaxBytes-3) {
		t.Fatalf("NormalizeTitle emoji-bound = %q, want %d 'a's", got, TitleMaxBytes-3)
	}
}

func TestNormalizeTitleEmptyAfterSanitization(t *testing.T) {
	if got := NormalizeTitle("\x1b[0m   ", TitleMaxBytes); got != "" {
		t.Fatalf("NormalizeTitle(only controls) = %q, want empty", got)
	}
}

func TestFallbackTitleFirstWordsAndBytes(t *testing.T) {
	if got := FallbackTitle("帮 我 写 一 首 关 于 春 天 的 诗", TitleFallbackMaxWords, TitleFallbackMaxBytes); got != "帮 我 写 一 首" {
		t.Fatalf("FallbackTitle words = %q", got)
	}
	// Byte bound applies after the word cap.
	long := strings.Repeat("word ", 100)
	if got := FallbackTitle(long, TitleFallbackMaxWords, TitleFallbackMaxBytes); len(got) > TitleFallbackMaxBytes {
		t.Fatalf("FallbackTitle exceeds byte budget: %d", len(got))
	}
	// Empty input yields empty title.
	if got := FallbackTitle("   ", TitleFallbackMaxWords, TitleFallbackMaxBytes); got != "" {
		t.Fatalf("FallbackTitle(empty) = %q, want empty", got)
	}
}

func TestFirstEligibleUserTextUsesRichContentProjection(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "image"}, {"type": "text", "text": "rich"}, {"type": "text", "text": "prompt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Seq: 1, Type: EventUserMessage, Version: EventVersion, Data: data},
	}
	if got := FirstEligibleUserText(events); got != "rich\nprompt" {
		t.Fatalf("FirstEligibleUserText = %q, want rich\\nprompt", got)
	}
}

func TestFirstEligibleUserTextSkipsNonUserProvenance(t *testing.T) {
	events := []Event{
		{Seq: 1, Type: EventUserMessage, Version: EventVersion, Data: json.RawMessage(`{"text":"team input","source":{"kind":"team-message"}}`)},
		{Seq: 2, Type: EventUserMessage, Version: EventVersion, Data: json.RawMessage(`{"text":"human input","source":{"kind":"user"}}`)},
	}
	if got := FirstEligibleUserText(events); got != "human input" {
		t.Fatalf("FirstEligibleUserText provenance = %q, want human input", got)
	}
}

func TestTruncateUTF8DoesNotSplitCodePoints(t *testing.T) {
	if got := TruncateUTF8("你好世界", 5); got != "你" {
		t.Fatalf("TruncateUTF8 = %q, want 你", got)
	}
	if got := TruncateUTF8("hello", 3); got != "hel" {
		t.Fatalf("TruncateUTF8 = %q, want hel", got)
	}
	if got := TruncateUTF8("x", 0); got != "" {
		t.Fatalf("TruncateUTF8(max 0) = %q, want empty", got)
	}
}
