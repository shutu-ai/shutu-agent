package terminal

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestNewBoundedTextBufferDefaults(t *testing.T) {
	b := NewBoundedTextBuffer(0, 0)
	if !b.Empty() {
		t.Fatal("new buffer should be empty")
	}
	if text, truncated := b.Snapshot(); text != "" || truncated {
		t.Fatalf("empty snapshot: got %q truncated=%v", text, truncated)
	}
	if text, truncated := b.Consume(); text != "" || truncated {
		t.Fatalf("empty consume: got %q truncated=%v", text, truncated)
	}
}

func TestByteTrimKeepTail(t *testing.T) {
	b := NewBoundedTextBuffer(8, 0)
	b.Append("abcdefghij") // 10 bytes
	text, truncated := b.Snapshot()
	if text != "cdefghij" {
		t.Fatalf("tail trim: got %q want %q", text, "cdefghij")
	}
	if !truncated {
		t.Fatal("expected truncated=true after byte trim")
	}
	if b.Empty() {
		t.Fatal("buffer should not be empty")
	}
}

func TestLineTrim(t *testing.T) {
	b := NewBoundedTextBuffer(0, 2)
	b.Append("1\n2\n3\n4\n5")
	text, truncated := b.Snapshot()
	if text != "4\n5" {
		t.Fatalf("line trim: got %q want %q", text, "4\n5")
	}
	if !truncated {
		t.Fatal("expected truncated=true after line trim")
	}
}

func TestLineTrimAcrossAppends(t *testing.T) {
	b := NewBoundedTextBuffer(0, 2)
	b.Append("1\n2\n3\n")
	b.Append("4\n5\n")
	text, _ := b.Snapshot()
	if text != "4\n5\n" {
		t.Fatalf("line trim across appends: got %q want %q", text, "4\n5\n")
	}
}

func TestLineTrimKeepsTrailingPartialLine(t *testing.T) {
	b := NewBoundedTextBuffer(0, 2)
	b.Append("alpha\nbeta\ngamma")
	text, _ := b.Snapshot()
	if text != "beta\ngamma" {
		t.Fatalf("line trim partial: got %q want %q", text, "beta\ngamma")
	}
}

func TestUTF8SafeByteTrim(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		maxBytes int
	}{
		{"cut through CJK boundary", "a你b", 4},
		{"cut inside CJK rune", "A你好B", 5},
		{"emoji 4-byte", "A😀B", 5},
		{"emoji plus CJK", "你😀好", 7},
		{"tiny limit multibyte only", "你好世界", 1},
		{"exact boundary", "你好世界", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBoundedTextBuffer(tc.maxBytes, 0)
			b.Append(tc.input)
			text, truncated := b.Snapshot()
			if !utf8.ValidString(text) {
				t.Fatalf("buffer produced invalid UTF-8: %q", text)
			}
			if len(text) > tc.maxBytes {
				t.Fatalf("buffer length %d exceeds maxBytes %d: %q", len(text), tc.maxBytes, text)
			}
			if !strings.HasSuffix(tc.input, text) {
				t.Fatalf("buffer %q is not a suffix of input %q", text, tc.input)
			}
			wantTruncated := len(tc.input) > tc.maxBytes
			if truncated != wantTruncated {
				t.Fatalf("truncated=%v want %v", truncated, wantTruncated)
			}
		})
	}
}

func TestConsumeDelta(t *testing.T) {
	b := NewBoundedTextBuffer(0, 0)
	b.Append("line1\n")
	b.Append("line2\n")
	text, _ := b.Consume()
	if text != "line1\nline2\n" {
		t.Fatalf("first consume: got %q want %q", text, "line1\nline2\n")
	}
	if !b.Empty() {
		t.Fatal("buffer should be empty after consume")
	}
	b.Append("line3")
	text2, _ := b.Consume()
	if text2 != "line3" {
		t.Fatalf("second consume: got %q want %q", text2, "line3")
	}
	if !b.Empty() {
		t.Fatal("buffer should be empty after second consume")
	}
}

func TestTruncatedResetsOnConsume(t *testing.T) {
	b := NewBoundedTextBuffer(4, 0)
	b.Append("abcdef") // 6 bytes -> trim to "cdef", truncated
	if _, truncated := b.Snapshot(); !truncated {
		t.Fatal("expected truncated before consume")
	}
	text, truncated := b.Consume()
	if text != "cdef" || !truncated {
		t.Fatalf("consume: got %q truncated=%v", text, truncated)
	}
	if _, truncated := b.Snapshot(); truncated {
		t.Fatal("truncated should reset after consume")
	}
	b.Append("ab") // under limit, no new truncation
	if _, truncated := b.Snapshot(); truncated {
		t.Fatal("truncated should stay false after non-trimming append")
	}
	b.Append("cdefgh") // exceeds again
	if _, truncated := b.Snapshot(); !truncated {
		t.Fatal("expected truncated after exceeding again")
	}
	if _, truncated := b.Consume(); !truncated {
		t.Fatal("consume should report the truncated delta")
	}
	if _, truncated := b.Snapshot(); truncated {
		t.Fatal("truncated should reset after final consume")
	}
}

func TestSnapshotDoesNotConsume(t *testing.T) {
	b := NewBoundedTextBuffer(0, 0)
	b.Append("hello")
	if text, _ := b.Snapshot(); text != "hello" {
		t.Fatalf("snapshot: got %q", text)
	}
	if text, _ := b.Snapshot(); text != "hello" {
		t.Fatalf("snapshot should not consume: got %q", text)
	}
}

func TestLineThenByteOrder(t *testing.T) {
	b := NewBoundedTextBuffer(6, 2)
	b.Append("alpha\nbeta\ngamma\n") // line trim -> "beta\ngamma\n", then byte trim -> "gamma\n"
	text, truncated := b.Snapshot()
	if text != "gamma\n" {
		t.Fatalf("line-then-byte: got %q want %q", text, "gamma\n")
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
}

func TestBoundaryExactLimit(t *testing.T) {
	b := NewBoundedTextBuffer(10, 0)
	b.Append("1234567890")
	if text, truncated := b.Snapshot(); text != "1234567890" || truncated {
		t.Fatalf("exact limit: got %q truncated=%v", text, truncated)
	}
}

func TestBoundaryMaxBytesOne(t *testing.T) {
	b := NewBoundedTextBuffer(1, 0)
	b.Append("AB")
	if text, _ := b.Snapshot(); text != "B" {
		t.Fatalf("maxBytes=1: got %q want %q", text, "B")
	}
	b.Append("C")
	if text, _ := b.Snapshot(); text != "C" {
		t.Fatalf("maxBytes=1 after append: got %q want %q", text, "C")
	}
}

func TestBoundaryUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	b := NewBoundedTextBuffer(0, 0)
	b.Append(long)
	if text, truncated := b.Snapshot(); text != long || truncated {
		t.Fatalf("unlimited bytes: got len %d truncated=%v", len(text), truncated)
	}
	b2 := NewBoundedTextBuffer(-1, -1)
	b2.Append(long + "y")
	if text, truncated := b2.Snapshot(); text != long+"y" || truncated {
		t.Fatalf("negative limits: got len %d truncated=%v", len(text), truncated)
	}
}

func TestEmpty(t *testing.T) {
	b := NewBoundedTextBuffer(100, 10)
	if !b.Empty() {
		t.Fatal("new buffer should be empty")
	}
	b.Append("x")
	if b.Empty() {
		t.Fatal("buffer should not be empty after append")
	}
	b.Consume()
	if !b.Empty() {
		t.Fatal("buffer should be empty after consume")
	}
	b.Append("")
	if !b.Empty() {
		t.Fatal("empty append should not change emptiness")
	}
}

func TestBoundedTextBufferIsSafeForConcurrentAppendAndConsume(t *testing.T) {
	b := NewBoundedTextBuffer(4096, 64)
	const workers = 8
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wg.Done()
			for index := 0; index < iterations; index++ {
				b.Append("terminal-output\n")
			}
		}()
		go func() {
			defer wg.Done()
			for index := 0; index < iterations; index++ {
				text, _ := b.Consume()
				if !utf8.ValidString(text) {
					t.Errorf("invalid UTF-8 consumed: %q", text)
					return
				}
			}
		}()
	}
	wg.Wait()
	if text, truncated := b.Snapshot(); utf8.ValidString(text) == false || truncated && len(text) > 4096 {
		t.Fatalf("buffer after concurrent access: len=%d truncated=%v", len(text), truncated)
	}
}
