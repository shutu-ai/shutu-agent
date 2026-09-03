// Tool-result pruning (ADR 2026-08-18-m5-agent-core.md 决策 ③, dispatch-m5c-1b):
// pure deterministic (model-free) head/middle/tail truncation of oversized
// tool/result outputs, on Unicode code point boundaries (never splits a rune).
// PruneToolResults only computes the plan — it never mutates the log (D1); the
// compaction/prune event is logged by the wiring (M5c-2).
package compaction

import (
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// PruneResult reports which tool/result events are oversized and how many bytes
// their head/middle/tail truncation would save.
type PruneResult struct {
	Replaced   []int64 // Seq of each oversized tool/result event
	SavedBytes int     // sum of len(output) - len(truncated) over the replaced events
}

// PruneToolResults scans the session for tool/result events whose output
// exceeds maxBytes (the per-result budget) and computes their deterministic
// head/middle/tail replacement. It returns the affected Seq list and the total
// bytes saved. maxBytes must be > 0. The log is read-only: applying the plan
// (logging compaction/prune) is the wiring's job.
func PruneToolResults(sess SessionLike, maxBytes int) (PruneResult, error) {
	if maxBytes <= 0 {
		return PruneResult{}, fmt.Errorf("compaction: prune maxBytes must be positive, got %d", maxBytes)
	}
	var res PruneResult
	for _, ev := range sess.Events() {
		if ev.Type != session.EventToolResult {
			continue
		}
		var d toolResultPayload
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		// EventToolError is a source-compatibility alias for tool/result. Its
		// legacy compact payload has no dsh message envelope and must not be
		// mistaken for a model-visible tool result during pruning.
		if d.Code != "" && d.Message == nil {
			continue
		}
		// A literal legacy "tool/error" row may share the compatibility event
		// type alias but still carry the old string-valued error field.
		var legacy struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(ev.Data, &legacy) == nil && legacy.Error != "" {
			continue
		}
		if len(d.Output) <= maxBytes {
			continue
		}
		replaced := pruneResultText(d.Output, maxBytes)
		res.Replaced = append(res.Replaced, int64(ev.Seq))
		res.SavedBytes += len(d.Output) - len(replaced)
	}
	return res, nil
}

// pruneResultText truncates a tool/result output that exceeds maxBytes into a
// head + middle marker + tail form, never splitting a UTF-8 rune. The middle
// marker records the omitted byte count; it is budgeted using the original
// length (an upper bound on the digit count of the final omitted count), so the
// returned text always fits within maxBytes. Mirrors the truncation discipline
// of internal/jobs capOutput.
func pruneResultText(out string, maxBytes int) string {
	if maxBytes <= 0 || len(out) <= maxBytes {
		return out
	}
	// Reserve the marker with the original length: omitted < len(out), so the
	// final marker (with the real omitted count) is never longer than reserved.
	marker := "\n…[truncated: " + strconv.Itoa(len(out)) + " bytes omitted]\n"
	remaining := maxBytes - len(marker)
	if remaining < 0 {
		remaining = 0
	}
	headBytes := remaining / 2
	tailBytes := remaining - headBytes
	head := truncateUTF8(out, headBytes)
	tail := lastBytes(out, tailBytes)
	omitted := len(out) - len(head) - len(tail)
	return head + "\n…[truncated: " + strconv.Itoa(omitted) + " bytes omitted]\n" + tail
}

// truncateUTF8 shortens s to at most maxBytes bytes, backing off until the
// prefix is valid UTF-8 (never splits a rune). Local copy of the same pattern
// used by internal/jobs and internal/tools (kept here so the package stays
// dependency-free).
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

// lastBytes returns the UTF-8-safe tail of at most the last maxBytes bytes of
// s: if the cut would split a rune it advances past that rune, so the returned
// tail is never longer than maxBytes and never splits a rune.
func lastBytes(s string, maxBytes int) string {
	if maxBytes < 0 || len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	start := len(b) - maxBytes
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}
	return string(b[start:])
}
