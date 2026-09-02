package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SpillStore writes full tool outputs that exceed the output limit to
// data/spill/<session>-<seq>.txt and returns the absolute file path as the
// model-facing locator (design.md §5 / dispatch-m3). The locator is an
// absolute path so read can consume it directly.
type SpillStore struct {
	dir string
}

// Save persists content as <dir>/<session>-<seq>.txt and returns the absolute
// path. Session ids are treated as a single safe path segment (the REPL's own
// ids are already safe; separators are neutralized defensively).
func (s *SpillStore) Save(sessionID string, seq uint64, content string) (string, error) {
	dir := s.dir
	if dir == "" {
		dir = DefaultSpillDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("spill: resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("spill: create dir %s: %w", abs, err)
	}
	name := fmt.Sprintf("%s-%d.txt", safeSegment(sessionID), seq)
	path := filepath.Join(abs, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("spill: write %s: %w", path, err)
	}
	return path, nil
}

// safeSegment neutralizes path-hostile characters in a session id so it can
// never escape the spill directory.
func safeSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	if s == "" {
		s = "unknown"
	}
	return s
}

// truncateResult builds the model-facing result for an oversized output: the
// full text is spilled to disk, the model sees a bounded head plus a notice
// carrying the omitted byte count and the locator. The head is truncated on a
// UTF-8 rune boundary and the replacement never exceeds the limit.
func truncateResult(out, locator string, limit int) ToolResult {
	const prefix = "\n\n[output truncated: "
	const suffix = " bytes omitted; full output at "
	// Reserve a worst-case notice (omitted digits bounded by len(out)) so the
	// final head+notice stays within the cap.
	placeholder := prefix + strconv.Itoa(len(out)) + suffix + locator + "]"
	budget := limit - len(placeholder)
	if budget < 0 {
		budget = 0
	}
	// Keep a deterministic head/tail preview. The reference spill policy uses
	// the same split so that a large command result retains both its beginning
	// (usually the command/error header) and its final lines (usually the useful
	// summary), rather than silently losing the tail.
	head, tail := truncateHeadTailUTF8(out, budget)
	omitted := len(out) - len(head) - len(tail)
	notice := prefix + strconv.Itoa(omitted) + suffix + locator + "]"
	return ToolResult{
		Output:     head + tail + notice,
		SpillPath:  locator,
		SpillBytes: len(out),
	}
}

// truncateHeadTailUTF8 retains approximately half of maxBytes from each end,
// backing off at UTF-8 cut boundaries. It returns the retained slices and does
// not insert a synthetic separator; the spill notice is the explicit boundary.
func truncateHeadTailUTF8(s string, maxBytes int) (string, string) {
	if maxBytes < 0 || len(s) <= maxBytes {
		return s, ""
	}
	headBudget := (maxBytes + 1) / 2
	tailBudget := maxBytes / 2
	head := truncateUTF8(s, headBudget)
	if tailBudget == 0 {
		return head, ""
	}
	tailBytes := []byte(s[len(s)-tailBudget:])
	for len(tailBytes) > 0 && (tailBytes[0]&0xc0) == 0x80 {
		tailBytes = tailBytes[1:]
	}
	return head, string(tailBytes)
}

// truncateUTF8 shortens s to at most maxBytes bytes, backing off until the
// prefix is valid UTF-8 (never splitting a rune).
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes < 0 || len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	b = b[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
