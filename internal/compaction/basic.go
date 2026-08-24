// BasicEngine is the default compaction provider (ADR 2026-08-18-m5-agent-core.md
// 决策 ③, dispatch-m5c-1b): token-pressure detection over the derived surface, a
// retained tail of the last RetainTurns user turns, LLM summarization of the
// shadowed prefix, and append-only surfaceOp.replace logging (D1). It is
// zero-dependency beyond the standard library and internal/llm + internal/session.
package compaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

// TokenEstimator estimates the token count of one string. Providers accept it
// so tests and callers can inject a real tokenizer; nil means the built-in
// zero-dependency estimate.
type TokenEstimator func(text string) int

// BasicOpts configures BasicEngine.
type BasicOpts struct {
	// Tokenizer estimates tokens; nil uses the built-in len(bytes)/4 proxy.
	Tokenizer TokenEstimator
	// LLM generates summaries. Required for any compaction that produces a
	// summary; a nil LLM makes every compaction attempt fail with an error
	// (fail-open is the wiring's concern).
	LLM llm.LLM
	// Model is the summary model name (advisory; the adapter owns the
	// effective model, matching the kb.ExtractOpts convention).
	Model string
	// TokenThreshold is the pressure trigger: CompactIfNeeded compacts when the
	// estimated surface size exceeds it. <= 0 disables pressure-triggered
	// compaction (context-overflow still forces one).
	TokenThreshold int
	// RetainTurns is the number of most-recent user/message turns kept
	// unshadowed. <= 0 keeps none.
	RetainTurns int
	// RetainTokens is the dsh-style token budget kept at the tail of the
	// surface. When positive it takes precedence over RetainTurns.
	RetainTokens int
	// FrameSummary stores the dsh checkpoint envelope around the raw summary.
	FrameSummary bool
	// RequireSmallerSummary rejects a checkpoint whose framed summary is not
	// strictly smaller than the surface it replaces.
	RequireSmallerSummary bool
}

// defaultTokenEstimate is the zero-dependency token proxy: ~4 bytes per token,
// a common tokenizer rule of thumb (CJK underestimates slightly at ≈3
// bytes/token; pressure detection only needs a cheap, monotone bound). Chosen
// over rune counting because tokenizers run over bytes, not runes.
func defaultTokenEstimate(text string) int {
	return len(text) / 4
}

// BasicEngine implements Engine (ADR 决策 ③).
type BasicEngine struct {
	opts    BasicOpts
	est     TokenEstimator
	counter atomic.Uint64
}

// NewBasic returns a BasicEngine with the given options (a nil Tokenizer uses
// the built-in estimate).
func NewBasic(opts BasicOpts) *BasicEngine {
	est := opts.Tokenizer
	if est == nil {
		est = defaultTokenEstimate
	}
	return &BasicEngine{opts: opts, est: est}
}

// CompactIfNeeded compacts only when needed (dispatch-m5c-1b): under
// TriggerPressure it is a no-op while the estimated surface size is within
// TokenThreshold; under TriggerContextOverflow it forces one effective balanced
// reduction even below the threshold. It returns nil, nil when nothing needs to
// (or can) be compacted.
func (e *BasicEngine) CompactIfNeeded(ctx context.Context, sess SessionLike, trigger Trigger) (*Result, error) {
	over := e.opts.TokenThreshold > 0 && e.estimateTokens(sess) > e.opts.TokenThreshold
	if !over && trigger == TriggerPressure {
		return nil, nil
	}
	return e.compactPrefix(ctx, sess, true)
}

// CompactNow performs one effective compaction regardless of pressure: it
// shadows the largest pairing-balanced prefix that leaves the last RetainTurns
// user turns unshadowed, summarizes it, and appends the summary marker.
func (e *BasicEngine) CompactNow(ctx context.Context, sess SessionLike) (*Result, error) {
	return e.compactPrefix(ctx, sess, false)
}

// CompactRegion compacts the given surface range [start, end] after correcting
// both boundaries to pairing boundaries (dispatch-m5c-1b): Start is pushed
// forward to the first balanced boundary, End backward to the last one. It
// returns nil, nil when no balanced, user-containing range survives the
// correction.
func (e *BasicEngine) CompactRegion(ctx context.Context, sess SessionLike, start, end int64) (*Result, error) {
	r, seqs, ok := e.correctRegionRange(sess, start, end)
	if !ok {
		return nil, nil
	}
	return e.doCompact(ctx, sess, r, seqs, false)
}

// compactPrefix shadows the maximal pairing-balanced prefix before the retained
// tail and appends the summary marker (the shared body of CompactIfNeeded and
// CompactNow).
func (e *BasicEngine) compactPrefix(ctx context.Context, sess SessionLike, wholeSurface bool) (*Result, error) {
	r, seqs, ok := e.choosePrefixRange(sess)
	if !ok {
		return nil, nil
	}
	return e.doCompact(ctx, sess, r, seqs, wholeSurface)
}

// doCompact summarizes the shadowed range, appends the surfaceOp.replace
// summary marker (D1, append-only) and returns the Result.
func (e *BasicEngine) doCompact(ctx context.Context, sess SessionLike, r [2]int64, seqs []int64, wholeSurface bool) (*Result, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	start, end := r[0], r[1]
	snapshot := sess.Events()
	msgs := shadowedHistory(snapshot, start, end)
	summary, err := e.summarize(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("compaction: summarize [%d,%d]: %w", start, end, err)
	}
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("compaction: summary is empty")
	}
	if !snapshotStable(snapshot, sess.Events(), start, end, wholeSurface) {
		return nil, fmt.Errorf("compaction: history changed while summarizing [%d,%d]", start, end)
	}
	storedSummary := summary
	if e.opts.FrameSummary {
		storedSummary = frameSummary(summary)
	}
	shadowedTokens := e.estimateMessages(msgs)
	if e.opts.RequireSmallerSummary && e.est(storedSummary) >= shadowedTokens {
		return nil, fmt.Errorf("compaction: summary is not smaller (%d >= %d tokens)", e.est(storedSummary), shadowedTokens)
	}
	if _, err := sess.Append(session.EventUserMessage, session.NewUserMessageReplace(storedSummary, start, end)); err != nil {
		return nil, fmt.Errorf("compaction: append summary marker: %w", err)
	}
	return &Result{
		CompactionID:   e.nextID(),
		Summary:        summary,
		ShadowedRange:  [2]int64{start, end},
		ShadowedSeqs:   seqs,
		ShadowedTokens: shadowedTokens,
	}, nil
}

// choosePrefixRange picks the shadowable prefix: from the log start up to the
// largest pairing-balanced End that still leaves the last RetainTurns user
// messages unshadowed. It requires the range to contain at least one user
// message (a summary must replace at least a real turn). Returns false when
// there is nothing effective to shadow.
func (e *BasicEngine) choosePrefixRange(sess SessionLike) ([2]int64, []int64, bool) {
	events := sess.Events()
	if len(events) == 0 {
		return [2]int64{}, nil, false
	}
	firstSeq := int64(events[0].Seq)
	lastSeq := int64(events[len(events)-1].Seq)
	userSeqs := userMessageSeqs(events)
	if len(userSeqs) == 0 {
		return [2]int64{}, nil, false
	}
	if e.opts.RetainTokens > 0 {
		return e.choosePrefixByTokens(sess, events)
	}
	keep := e.opts.RetainTurns
	if keep < 0 {
		keep = 0
	}
	if keep >= len(userSeqs) {
		// every user turn is retained; there is no prefix before the tail
		return [2]int64{}, nil, false
	}
	var maxEnd int64
	if keep == 0 {
		maxEnd = lastSeq
	} else {
		// shadow everything before the first retained user message
		maxEnd = userSeqs[len(userSeqs)-keep] - 1
	}

	bal := pairingBalancedAfter(events)
	end := int64(0)
	found := false
	for i := len(events) - 1; i >= 0; i-- {
		if int64(events[i].Seq) <= maxEnd && bal[i] {
			end = int64(events[i].Seq)
			found = true
			break
		}
	}
	if !found || end < firstSeq {
		return [2]int64{}, nil, false
	}
	if !rangeHasUser(events, firstSeq, end) {
		return [2]int64{}, nil, false
	}
	return [2]int64{firstSeq, end}, seqsInRange(events, firstSeq, end), true
}

// choosePrefixByTokens mirrors dsh's retainTokens selection: walk the priced
// surface backwards until the retained tail reaches its token budget, then
// expand the tail backwards until its start is outside a tool pair.
func (e *BasicEngine) choosePrefixByTokens(sess SessionLike, events []session.Event) ([2]int64, []int64, bool) {
	keepFrom := len(events) - 1
	retained := 0
	for keepFrom >= 0 && retained < e.opts.RetainTokens {
		seq := int64(events[keepFrom].Seq)
		retained += e.estimateMessages(shadowedHistory(events, seq, seq))
		keepFrom--
	}
	if keepFrom < 0 {
		return [2]int64{}, nil, false
	}
	for keepFrom >= 0 && !ToolPairingBalancedBefore(sess, int64(events[keepFrom].Seq)) {
		keepFrom--
	}
	if keepFrom < 0 {
		return [2]int64{}, nil, false
	}
	first := int64(events[0].Seq)
	endLimit := int64(events[keepFrom].Seq) - 1
	bal := pairingBalancedAfter(events)
	for i := keepFrom; i >= 0; i-- {
		if int64(events[i].Seq) <= endLimit && bal[i] && rangeHasUser(events, first, int64(events[i].Seq)) {
			end := int64(events[i].Seq)
			return [2]int64{first, end}, seqsInRange(events, first, end), true
		}
	}
	return [2]int64{}, nil, false
}

// correctRegionRange clamps [start, end] to the log, pushes Start forward to
// the first pairing-balanced boundary and End backward to the last one, then
// requires a non-empty, user-containing range. Returns false when the corrected
// range is empty or shadows no user message.
func (e *BasicEngine) correctRegionRange(sess SessionLike, start, end int64) ([2]int64, []int64, bool) {
	events := sess.Events()
	if len(events) == 0 {
		return [2]int64{}, nil, false
	}
	firstSeq := int64(events[0].Seq)
	lastSeq := int64(events[len(events)-1].Seq)
	if start > end || end < firstSeq || start > lastSeq {
		return [2]int64{}, nil, false
	}
	if start < firstSeq {
		start = firstSeq
	}
	if end > lastSeq {
		end = lastSeq
	}
	bal := pairingBalancedAfter(events)

	// Start boundary: the smallest event index whose Seq >= start with a clean
	// boundary before it (the retained prefix is balanced).
	sIdx := -1
	for j, ev := range events {
		if int64(ev.Seq) >= start {
			if j == 0 || bal[j-1] {
				sIdx = j
				break
			}
		}
	}
	if sIdx < 0 {
		return [2]int64{}, nil, false
	}
	// End boundary: the largest event index whose Seq <= end with a clean
	// boundary after it (the shadowed prefix up to it is balanced).
	eIdx := -1
	for i := len(events) - 1; i >= 0; i-- {
		if int64(events[i].Seq) <= end && bal[i] {
			eIdx = i
			break
		}
	}
	if eIdx < 0 || eIdx < sIdx {
		return [2]int64{}, nil, false
	}
	start2 := int64(events[sIdx].Seq)
	end2 := int64(events[eIdx].Seq)
	if start2 > end2 || !rangeHasUser(events, start2, end2) {
		return [2]int64{}, nil, false
	}
	return [2]int64{start2, end2}, seqsInRange(events, start2, end2), true
}

// estimateTokens estimates the current model-visible surface size (the whole
// derived history, tool call arguments included).
func (e *BasicEngine) estimateTokens(sess SessionLike) int {
	return e.estimateMessages(sess.DeriveHistory())
}

func (e *BasicEngine) estimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += e.est(m.Text())
		for _, tc := range m.ToolCalls {
			total += e.est(tc.Name) + e.est(tc.Arguments)
		}
	}
	return total
}

// summarySystemPrompt asks the model to fold an older conversation excerpt into
// a compact but faithful summary, preserving the facts needed to continue.
const summarySystemPrompt = `You are compacting an older portion of a conversation to free context space.
Condense the excerpt below into a concise but faithful summary of everything still
relevant to continuing the work: decisions, constraints, goals, discovered facts,
tool outcomes that matter, open questions, and the user's preferences.
Keep code, commands, paths, and technical identifiers exactly.
Do not invent facts not present in the excerpt. Output plain text only — no JSON,
no markdown, no commentary outside the summary.`

// summarize folds the shadowed messages into one summary via the configured
// LLM (the same internal/llm interface the loop uses; the whole answer is
// accumulated from stream deltas, mirroring kb.callExtractionModel). Any model
// failure is returned as an error — fail-open is the wiring's decision.
func (e *BasicEngine) summarize(ctx context.Context, msgs []llm.Message) (string, error) {
	if e.opts.LLM == nil {
		return "", errors.New("compaction: llm required for summary")
	}
	full := make([]llm.Message, 0, len(msgs)+1)
	full = append(full, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(summarySystemPrompt)}})
	full = append(full, msgs...)
	reader, err := e.opts.LLM.Stream(ctx, llm.ChatRequest{Model: e.opts.Model, Messages: full})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if ev.Kind == llm.StreamTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

const (
	checkpointPreamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation."
	checkpointOpen     = "<compacted-summary>"
	checkpointClose    = "</compacted-summary>"
)

func frameSummary(summary string) string {
	return checkpointPreamble + "\n\n" + checkpointOpen + "\n" + strings.TrimSpace(summary) + "\n" + checkpointClose
}

func snapshotStable(before, after []session.Event, start, end int64, wholeSurface bool) bool {
	if wholeSurface {
		if len(before) != len(after) {
			return false
		}
		for i := range before {
			if !sameEvent(before[i], after[i]) {
				return false
			}
		}
		return true
	}
	bySeq := make(map[uint64]session.Event, len(after))
	for _, ev := range after {
		bySeq[ev.Seq] = ev
	}
	for _, ev := range before {
		seq := int64(ev.Seq)
		if seq >= start && seq <= end {
			current, ok := bySeq[ev.Seq]
			if !ok || !sameEvent(ev, current) {
				return false
			}
		}
	}
	return true
}

func sameEvent(a, b session.Event) bool {
	return a.Seq == b.Seq && a.Type == b.Type && a.Version == b.Version && bytes.Equal(a.Data, b.Data)
}

// nextID returns a self-generated compaction id (timestamp + per-engine
// counter); uniqueness is not a correctness requirement — the marker event's
// Seq is the real fold key.
func (e *BasicEngine) nextID() string {
	return fmt.Sprintf("c-%d-%d", time.Now().UTC().UnixMilli(), e.counter.Add(1))
}
