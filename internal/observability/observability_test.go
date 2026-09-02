package observability

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/runtimectx"
)

func TestMetricsAreConcurrencySafeAndSnapshotCounters(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Turn()
			m.Step()
			m.Request(errors.New("request"))
			m.Tool(nil)
			m.Usage(llm.TokenUsage{InputTokens: 3, OutputTokens: 2, CachedInputTokens: 1})
		}()
	}
	wg.Wait()
	got := m.Snapshot()
	if got.Turns != 32 || got.Steps != 32 || got.Requests != 32 || got.RequestFailures != 32 || got.ToolCalls != 32 || got.ToolFailures != 0 || got.InputTokens != 96 || got.OutputTokens != 64 || got.CachedTokens != 32 {
		t.Fatalf("metrics snapshot = %+v", got)
	}
}

func TestTracerIsIdempotentBoundedAndClassifiesFailures(t *testing.T) {
	tracer := NewTracer(1)
	correlation := runtimectx.Correlation{AgentID: "a", SessionID: "s", RequestID: "r"}
	span := tracer.Start(correlation, "request", "")
	tracer.End(span, llm.NewFailureError("bad", "RATE_LIMIT", nil))
	tracer.End(span, errors.New("second end must be ignored"))
	spans := tracer.Spans()
	if len(spans) != 1 || spans[0].ErrorCode != "RATE_LIMIT" || spans[0].Correlation.SessionID != "s" || spans[0].EndedAt.IsZero() {
		t.Fatalf("spans = %+v", spans)
	}
	second := tracer.Start(correlation, "tool", spans[0].ID)
	tracer.End(second, nil)
	spans = tracer.Spans()
	if len(spans) != 1 || spans[0].Name != "tool" || spans[0].ParentID == "" {
		t.Fatalf("bounded spans = %+v", spans)
	}
}

func TestMetricsExportIsOptionalAndFailureIsNonFatal(t *testing.T) {
	m := New()
	m.Turn()
	var out bytes.Buffer
	m.SetExporter(NewJSONLExporter(&out))
	if err := m.Export(context.Background()); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"turns":1`)) {
		t.Fatalf("export = %q", out.String())
	}
	before := m.Snapshot()
	m.SetExporter(ExportFunc(func(context.Context, Snapshot) error { return errors.New("telemetry offline") }))
	if err := m.Export(context.Background()); err == nil {
		t.Fatal("failing exporter should be observable")
	}
	if after := m.Snapshot(); after != before {
		t.Fatalf("export failure changed execution counters: before=%+v after=%+v", before, after)
	}
}
