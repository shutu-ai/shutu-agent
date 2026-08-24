// Package compaction defines the context-compaction capability seam
// (design.md §10 D2, ADR 2026-08-18-m5-agent-core.md 决策 ③): a Service
// definition (Engine) plus pairing helpers that let a provider fold an old
// surface range of a session into a summary user/message. The default provider
// is BasicEngine (basic.go, token pressure + LLM summary); optional tool-result
// pruning lives in pruner.go. This package never imports config, jobs,
// subagent or the loop: consumers depend only on the seam (D2).
//
// Compaction is pure-event (D1): it never physically deletes or rewrites old
// events — it appends one new user/message carrying surfaceOp.replace
// (M5c-1a) that shadows [Start, End] during history derivation, so the log
// stays append-only and session.derive does the folding.
package compaction

import (
	"context"
	"encoding/json"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

// Trigger says why a compaction was requested (ADR 决策 ③).
type Trigger string

const (
	// TriggerPressure is a normal token-pressure trigger: compact only when
	// the estimated surface size exceeds the configured threshold.
	TriggerPressure Trigger = "pressure"
	// TriggerContextOverflow is an overflow trigger: force one effective
	// balanced compaction even when the estimate is still under threshold.
	TriggerContextOverflow Trigger = "context-overflow"
)

// Result reports one completed compaction (ADR 决策 ③). ShadowedRange is the
// first/last event Seq of the shadowed span; ShadowedSeqs is the authoritative
// set of every event Seq in that span — exactly the events session.derive folds
// out when it meets the appended surfaceOp.replace marker.
type Result struct {
	CompactionID   string
	Summary        string
	ShadowedRange  [2]int64 // first/last shadowed surface seq (event seq span)
	ShadowedSeqs   []int64  // every event seq in [Start, End] — the authoritative fold set
	ShadowedTokens int      // estimated tokens of the shadowed surface
}

// SessionLike is the minimal session surface a compaction consumer needs (D2).
// *session.Log satisfies it: Events is read-only (the provider never mutates
// old events, D1), Append is how the summary marker lands, DeriveHistory is
// the current model-visible surface.
type SessionLike interface {
	Events() []session.Event
	Append(typ string, data any) (session.Event, error)
	DeriveHistory() []llm.Message
}

// Engine is the compaction Service (ADR 决策 ③). Providers implement it.
type Engine interface {
	// CompactIfNeeded compacts only when needed: under TriggerPressure it is a
	// no-op when the estimated surface size is within the threshold;
	// under TriggerContextOverflow it forces one effective balanced reduction
	// even below the threshold. Returns nil, nil when nothing needs to (or can)
	// be compacted.
	CompactIfNeeded(ctx context.Context, sess SessionLike, trigger Trigger) (*Result, error)
	// CompactNow performs one effective compaction regardless of pressure.
	CompactNow(ctx context.Context, sess SessionLike) (*Result, error)
	// CompactRegion compacts the given surface range after correcting both
	// boundaries to pairing boundaries.
	CompactRegion(ctx context.Context, sess SessionLike, start, end int64) (*Result, error)
}

// ---- Read-only payload mirrors ----
//
// The session package owns the on-disk shapes; compaction only unmarshals the
// JSON to see call ids and text. llm.ToolCall carries no JSON tags, so its
// marshaled keys are the Go field names ("ID", "Name", "Arguments") and
// unmarshaling back into llm.ToolCall matches them case-insensitively.

type assistantPayload struct {
	Text      string         `json:"text"`
	Reasoning string         `json:"reasoning,omitempty"` // M8: folded into a reasoning block like session.derive
	ToolCalls []llm.ToolCall `json:"toolCalls,omitempty"`
}

type toolResultPayload struct {
	CallID string `json:"callId"`
	Name   string `json:"name,omitempty"`
	Output string `json:"output,omitempty"`
}

type toolErrorPayload struct {
	CallID string `json:"callId"`
	Name   string `json:"name,omitempty"`
	Error  string `json:"error,omitempty"`
}

// eventCallID returns the tool call id a tool/result or tool/error event
// answers, or "" when it cannot be parsed.
func eventCallID(ev session.Event) string {
	var r toolResultPayload
	if json.Unmarshal(ev.Data, &r) == nil && r.CallID != "" {
		return r.CallID
	}
	var e toolErrorPayload
	if json.Unmarshal(ev.Data, &e) == nil {
		return e.CallID
	}
	return ""
}

// pairingBalancedAfter returns, for each event index i, whether the surface
// prefix Events()[0..i] is pairing-balanced: every assistant tool_calls issued
// in the prefix has its tool result(s) inside the prefix, and no tool result
// appears without a matching call (an orphan poisons every later boundary).
// A balanced boundary never cuts a tool call from its result (ADR 决策 ③:
// 不切断配对中间). Only assistant/tool events participate; every other type is
// opaque to pairing.
func pairingBalancedAfter(events []session.Event) []bool {
	open := map[string]bool{}
	clean := true
	res := make([]bool, len(events))
	for i, ev := range events {
		switch ev.Type {
		case session.EventAssistantMessage:
			var d assistantPayload
			if json.Unmarshal(ev.Data, &d) == nil {
				for _, tc := range d.ToolCalls {
					if tc.ID != "" {
						open[tc.ID] = true
					}
				}
			}
		case session.EventToolResult, session.EventToolError:
			if id := eventCallID(ev); id != "" {
				if open[id] {
					delete(open, id)
				} else {
					clean = false
				}
			}
		}
		res[i] = clean && len(open) == 0
	}
	return res
}

// ToolPairingBalancedBefore reports whether the surface prefix strictly before
// seq is pairing-balanced, i.e. the boundary at seq never cuts an assistant
// tool_calls from its tool result (ADR 决策 ③). An empty prefix is balanced.
// It is the boundary a compaction range must satisfy at its Start.
func ToolPairingBalancedBefore(sess SessionLike, seq int64) bool {
	events := sess.Events()
	j := firstIndexSeqGE(events, seq)
	if j <= 0 {
		return true
	}
	return pairingBalancedAfter(events)[j-1]
}

// ToolPairingBalancedAfter reports whether the surface prefix up to and
// including seq is pairing-balanced, i.e. the boundary after seq never cuts an
// assistant tool_calls from its tool result. An empty prefix is balanced. It is
// the boundary a compaction range must satisfy at its End.
func ToolPairingBalancedAfter(sess SessionLike, seq int64) bool {
	events := sess.Events()
	i := lastIndexSeqLE(events, seq)
	if i < 0 {
		return true
	}
	return pairingBalancedAfter(events)[i]
}

// ---- event helpers ----

func firstIndexSeqGE(events []session.Event, seq int64) int {
	for i, ev := range events {
		if int64(ev.Seq) >= seq {
			return i
		}
	}
	return len(events)
}

func lastIndexSeqLE(events []session.Event, seq int64) int {
	for i := len(events) - 1; i >= 0; i-- {
		if int64(events[i].Seq) <= seq {
			return i
		}
	}
	return -1
}

// userMessageSeqs returns the Seq of every EventUserMessage, in log order.
func userMessageSeqs(events []session.Event) []int64 {
	var out []int64
	for _, ev := range events {
		if ev.Type == session.EventUserMessage && !isSurfaceReplacement(ev) {
			out = append(out, int64(ev.Seq))
		}
	}
	return out
}

// seqsInRange returns every event Seq in [start, end] (log order) — the
// authoritative set session.derive folds out for the range.
func seqsInRange(events []session.Event, start, end int64) []int64 {
	var out []int64
	for _, ev := range events {
		seq := int64(ev.Seq)
		if seq >= start && seq <= end {
			out = append(out, seq)
		}
	}
	return out
}

// rangeHasUser reports whether any EventUserMessage has a Seq in [start, end].
func rangeHasUser(events []session.Event, start, end int64) bool {
	for _, ev := range events {
		seq := int64(ev.Seq)
		if seq >= start && seq <= end && ev.Type == session.EventUserMessage && !isSurfaceReplacement(ev) {
			return true
		}
	}
	return false
}

// isSurfaceReplacement identifies a compaction checkpoint user/message. A
// checkpoint is a replacement marker, not a new conversational turn, so it
// must not displace the current user turn from the retained tail when pressure
// compaction performs its follow-up attempt.
func isSurfaceReplacement(ev session.Event) bool {
	if ev.Type != session.EventUserMessage {
		return false
	}
	var data struct {
		SurfaceOp *struct {
			Op string `json:"op"`
		} `json:"surfaceOp,omitempty"`
	}
	return json.Unmarshal(ev.Data, &data) == nil && data.SurfaceOp != nil && data.SurfaceOp.Op == "replace"
}

// shadowedHistory folds the events in [start, end] into model-visible messages,
// mirroring session.derive's rules for the range (user → user, assistant/message
// → assistant, tool/result & tool/error → tool, chunks and opaque types
// folded). For a range that contains no prior compaction marker this is exactly
// the shadowed part of DeriveHistory — the summary context (dispatch-m5c-1b).
func shadowedHistory(events []session.Event, start, end int64) []llm.Message {
	var out []llm.Message
	for _, ev := range events {
		seq := int64(ev.Seq)
		if seq < start || seq > end {
			continue
		}
		switch ev.Type {
		case session.EventUserMessage:
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			out = append(out, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(d.Text)}})
		case session.EventAssistantMessage:
			var d assistantPayload
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			// Mirror session.derive: reasoning block before the text block.
			content := make([]llm.ContentBlock, 0, 2)
			if d.Reasoning != "" {
				content = append(content, llm.ContentBlock{Kind: llm.BlockReasoning, Text: d.Reasoning})
			}
			content = append(content, llm.Text(d.Text))
			out = append(out, llm.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: d.ToolCalls})
		case session.EventToolResult:
			var d toolResultPayload
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: d.CallID, Content: []llm.ContentBlock{llm.Text(d.Output)}})
		case session.EventToolError:
			var d toolErrorPayload
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: d.CallID, Content: []llm.ContentBlock{llm.Text("Error: " + d.Error)}})
		}
	}
	return out
}
