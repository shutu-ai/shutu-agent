// Package observability contains in-process, lossless execution telemetry.
// It is deliberately independent from the session store: metrics and spans
// are best-effort observations, while durable session events remain the source
// of truth for replay.
package observability

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

// Snapshot is a point-in-time aggregate suitable for a status endpoint or
// metrics exporter. Counters are process-local and never used for control flow.
type Snapshot struct {
	Turns             uint64 `json:"turns"`
	Steps             uint64 `json:"steps"`
	Requests          uint64 `json:"requests"`
	RequestFailures   uint64 `json:"requestFailures"`
	ToolCalls         uint64 `json:"toolCalls"`
	ToolFailures      uint64 `json:"toolFailures"`
	InputTokens       uint64 `json:"inputTokens"`
	OutputTokens      uint64 `json:"outputTokens"`
	CachedTokens      uint64 `json:"cachedTokens"`
	ExtensionCalls    uint64 `json:"extensionCalls"`
	ExtensionFailures uint64 `json:"extensionFailures"`
}

// Exporter receives point-in-time telemetry snapshots. Export is deliberately
// outside the execution critical path: callers may invoke Metrics.Export from
// a background worker and treat an exporter error as diagnostic only.
type Exporter interface {
	Export(context.Context, Snapshot) error
}

// ExportFunc adapts a function to Exporter.
type ExportFunc func(context.Context, Snapshot) error

func (f ExportFunc) Export(ctx context.Context, snapshot Snapshot) error {
	if f == nil {
		return nil
	}
	return f(ctx, snapshot)
}

// JSONLExporter is a small dependency-free external export adapter. Each
// snapshot is one JSON object followed by a newline; the writer is serialized
// so multiple Agent workers cannot interleave records.
type JSONLExporter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewJSONLExporter(w io.Writer) *JSONLExporter { return &JSONLExporter{w: w} }

func (e *JSONLExporter) Export(_ context.Context, snapshot Snapshot) error {
	if e == nil || e.w == nil {
		return nil
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(append(b, '\n'))
	return err
}

// Metrics is a concurrency-safe aggregate. All methods are nil-safe so an
// observability sink can be disabled without branching in the execution path.
type Metrics struct {
	exporterMu        sync.RWMutex
	exporter          Exporter
	turns             atomic.Uint64
	steps             atomic.Uint64
	requests          atomic.Uint64
	requestFailures   atomic.Uint64
	toolCalls         atomic.Uint64
	toolFailures      atomic.Uint64
	inputTokens       atomic.Uint64
	outputTokens      atomic.Uint64
	cachedTokens      atomic.Uint64
	extensionCalls    atomic.Uint64
	extensionFailures atomic.Uint64
}

func New() *Metrics { return &Metrics{} }
func (m *Metrics) Turn() {
	if m != nil {
		m.turns.Add(1)
	}
}
func (m *Metrics) Step() {
	if m != nil {
		m.steps.Add(1)
	}
}
func (m *Metrics) Request(err error) {
	if m != nil {
		m.requests.Add(1)
		if err != nil {
			m.requestFailures.Add(1)
		}
	}
}
func (m *Metrics) Tool(err error) {
	if m != nil {
		m.toolCalls.Add(1)
		if err != nil {
			m.toolFailures.Add(1)
		}
	}
}

func (m *Metrics) Extension(err error) {
	if m != nil {
		m.extensionCalls.Add(1)
		if err != nil {
			m.extensionFailures.Add(1)
		}
	}
}
func (m *Metrics) Usage(usage llm.TokenUsage) {
	if m == nil {
		return
	}
	if usage.InputTokens > 0 {
		m.inputTokens.Add(uint64(usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		m.outputTokens.Add(uint64(usage.OutputTokens))
	}
	cached := usage.CacheReadTokens + usage.CacheWriteTokens
	if cached == 0 {
		cached = usage.CachedInputTokens
	}
	if cached > 0 {
		m.cachedTokens.Add(uint64(cached))
	}
}

func (m *Metrics) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	return Snapshot{
		Turns: m.turns.Load(), Steps: m.steps.Load(), Requests: m.requests.Load(),
		RequestFailures: m.requestFailures.Load(), ToolCalls: m.toolCalls.Load(),
		ToolFailures: m.toolFailures.Load(), InputTokens: m.inputTokens.Load(),
		OutputTokens: m.outputTokens.Load(), CachedTokens: m.cachedTokens.Load(),
		ExtensionCalls: m.extensionCalls.Load(), ExtensionFailures: m.extensionFailures.Load(),
	}
}

// SetExporter installs or clears the optional external exporter. Replacing an
// exporter is atomic with respect to Export; metrics updates never acquire
// this lock and therefore cannot be delayed by a slow telemetry backend.
func (m *Metrics) SetExporter(exporter Exporter) {
	if m == nil {
		return
	}
	m.exporterMu.Lock()
	m.exporter = exporter
	m.exporterMu.Unlock()
}

// Export sends one snapshot to the configured exporter. The returned error is
// intentionally observable to the caller but never changes counters or model
// execution state; production callers should log it and continue.
func (m *Metrics) Export(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.exporterMu.RLock()
	exporter := m.exporter
	m.exporterMu.RUnlock()
	if exporter == nil {
		return nil
	}
	return exporter.Export(ctx, m.Snapshot())
}

// Span is a bounded in-process trace record. End is idempotent; callers may
// safely finish a span from a cancellation and a cleanup defer concurrently.
type Span struct {
	ID          string                 `json:"id"`
	ParentID    string                 `json:"parentId,omitempty"`
	Name        string                 `json:"name"`
	Correlation runtimectx.Correlation `json:"correlation"`
	StartedAt   time.Time              `json:"startedAt"`
	EndedAt     time.Time              `json:"endedAt,omitempty"`
	ErrorCode   string                 `json:"errorCode,omitempty"`
	ended       *atomic.Bool
}

type Tracer struct {
	mu       sync.Mutex
	next     uint64
	max      int
	finished []Span
}

func NewTracer(max int) *Tracer {
	if max <= 0 {
		max = 4096
	}
	return &Tracer{max: max}
}

func (t *Tracer) Start(correlation runtimectx.Correlation, name, parentID string) *Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.next++
	id := time.Now().UTC().Format("20060102T150405.000000000Z")
	span := &Span{ID: id + ":" + formatCounter(t.next), ParentID: parentID, Name: name, Correlation: correlation, StartedAt: time.Now().UTC(), ended: &atomic.Bool{}}
	t.mu.Unlock()
	return span
}

func (t *Tracer) End(span *Span, err error) {
	if t == nil || span == nil || span.ended == nil || !span.ended.CompareAndSwap(false, true) {
		return
	}
	span.EndedAt = time.Now().UTC()
	if facts, ok := llm.FailureFacts(err); ok {
		span.ErrorCode = facts.Code
	} else if err != nil {
		span.ErrorCode = "ERROR"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.finished) == t.max {
		copy(t.finished, t.finished[1:])
		t.finished[len(t.finished)-1] = *span
	} else {
		t.finished = append(t.finished, *span)
	}
}

func (t *Tracer) Spans() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Span(nil), t.finished...)
}

func formatCounter(value uint64) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = digits[value%10]
		value /= 10
	}
	return string(buf[i:])
}
