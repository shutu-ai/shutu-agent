package spill

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// Constants that tune the v1 auto-sedimentation policy.
const (
	// minSpillRunes is the length threshold: a text of at least this many runes
	// is worth remembering even without a conclusive marker. It is the primary
	// filter against pure chit-chat, which is short.
	minSpillRunes = 24
	// maxToolResultRunes bounds a tool/result output stored as a memo summary.
	maxToolResultRunes = 240
)

// conclusiveMarkers are the "new information" signals: a text containing one
// is treated as a definite, memory-worthy statement even when short. Chinese
// markers are matched verbatim; ASCII markers are matched case-insensitively
// (the text is normalized before matching).
var conclusiveMarkers = []string{
	// 中文：明确的结论性 / 新信息 / 事实标记
	"记住", "记得", "结论", "决定", "建议", "发现", "结果",
	"重要", "关键", "注意", "总结", "计划", "安排", "确认",
	// 英文：结论 / 事实 / 提醒
	"remember", "conclusion", "decision", "suggestion", "important",
	"key", "summary", "plan", "remind", "learned", "found", "result", "note", "fact", "todo",
}

// chitchatPhrases are pure greetings/acknowledgments that are never worth
// remembering. They are short, so the length threshold would usually reject
// them anyway; the list makes the "non-chit-chat" rule explicit and testable.
var chitchatPhrases = []string{
	"hi", "hello", "hey", "hello there",
	"你好", "您好", "嗨",
	"ok", "okay", "好的", "好", "嗯", "恩", "是的", "对", "收到", "了解", "明白", "明白了",
	"thanks", "thank you", "谢谢", "感谢",
	"bye", "再见", "see you",
	"sure", "yep", "yes", "no problem", "不客气", "没关系",
}

// candidate is one auto-spill candidate: a text the policy judged worth
// remembering, with its provenance source.
type candidate struct {
	content string
	source  string
}

// autoSpillCandidates is the v1 auto-sedimentation policy kernel (pure: it has
// no side effects and never touches the Engine or Provider). It folds an event
// log into the texts worth remembering:
//
//   - from each assistant/message row it takes the final text, excluding
//     tool-call frames (rows whose text is empty — those only announce tool
//     calls and carry nothing to remember);
//   - from each tool/result row it takes a bounded summary of the output;
//   - both are filtered through worthRemembering (length threshold + conclusive
//     marker + non-chit-chat).
//
// The result preserves event order. Deduplication is deliberately left to the
// caller: Spill is idempotent by content hash, so AutoSpill's count of new
// memos stays correct even when two events yield the same text.
func autoSpillCandidates(events []session.Event) []candidate {
	var out []candidate
	var pending string
	var pendingSeq uint64
	flush := func() {
		if pending != "" && worthRemembering(pending) {
			out = append(out, candidate{content: pending, source: fmt.Sprintf("session:%d", pendingSeq)})
		}
		pending = ""
	}
	for _, ev := range events {
		switch ev.Type {
		case session.EventAssistantMessage:
			var d assistantEventData
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			// A tool-call frame has no final text — nothing to spill.
			if strings.TrimSpace(d.Text) == "" {
				continue
			}
			// Within a run of assistant rows the last non-empty text wins (the
			// final message); flush only when a different event type arrives.
			pending = d.Text
			pendingSeq = ev.Seq
		case session.EventToolResult:
			flush()
			var d toolResultEventData
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			summary := summarizeToolResult(d.Output)
			if summary != "" && worthRemembering(summary) {
				out = append(out, candidate{content: summary, source: fmt.Sprintf("session:%d:tool:%s", ev.Seq, d.Name)})
			}
		default:
			flush()
		}
	}
	flush()
	return out
}

// assistantEventData mirrors the session assistant/message payload. The session
// package keeps its payload structs private, so the policy parses the stable
// JSON contract itself (only the text field is needed here).
type assistantEventData struct {
	Text string `json:"text"`
}

// toolResultEventData mirrors the session tool/result payload.
type toolResultEventData struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

// summarizeToolResult bounds a tool output to maxToolResultRunes runes (the
// stored memo is a summary, not the full output).
func summarizeToolResult(output string) string {
	t := strings.TrimSpace(output)
	if t == "" {
		return ""
	}
	runes := []rune(t)
	if len(runes) > maxToolResultRunes {
		return string(runes[:maxToolResultRunes]) + "…"
	}
	return t
}

// worthRemembering decides whether one candidate text is a durable fact worth
// spilling. It is the v1 heuristic (pure, deterministic, testable):
//
//  1. empty or pure chit-chat → no;
//  2. at least minSpillRunes runes → yes (substantial statements);
//  3. otherwise → yes only if it contains a conclusive marker.
//
// A short text with neither length nor a marker is treated as chit-chat and
// dropped.
func worthRemembering(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	norm := normalizeText(t)
	if isChitchat(norm) {
		return false
	}
	if utf8.RuneCountInString(t) >= minSpillRunes {
		return true
	}
	return containsConclusive(norm)
}

// normalizeText lowercases ASCII and collapses whitespace runs to single
// spaces, so English markers and phrases match case-insensitively.
func normalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case unicode.IsSpace(r):
			if !space {
				b.WriteByte(' ')
			}
			space = true
		default:
			b.WriteRune(r)
			space = false
		}
	}
	return strings.TrimSpace(b.String())
}

func containsConclusive(norm string) bool {
	for _, m := range conclusiveMarkers {
		if strings.Contains(norm, m) {
			return true
		}
	}
	return false
}

func isChitchat(norm string) bool {
	for _, p := range chitchatPhrases {
		if norm == p {
			return true
		}
	}
	return false
}
