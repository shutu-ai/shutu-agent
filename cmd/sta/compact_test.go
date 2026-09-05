package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// byteTokens is a deterministic 1-token-per-byte surface estimator for tests
// (mirrors compaction's own test estimator, used to drive pressure exactly).
func byteTokens(log *session.Log) int {
	total := 0
	for _, m := range log.DeriveHistory() {
		total += len(m.Text())
		for _, tc := range m.ToolCalls {
			total += len(tc.Name) + len(tc.Arguments)
		}
	}
	return total
}

func TestCompactAndLogStopsBeforeCompactionWhenStartEventCannotPersist(t *testing.T) {
	wantErr := errors.New("durable sink unavailable")
	log := session.New()
	log.SetSink(func(session.Event) error { return wantErr })
	var runCalls int
	app := &app{}
	_, err := app.compactAndLogOn(context.Background(), log, "test", "pressure", func() (*compaction.Result, error) {
		runCalls++
		return &compaction.Result{CompactionID: "c1"}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if runCalls != 0 {
		t.Fatalf("compaction run calls = %d, want 0 after start persistence failure", runCalls)
	}
	if got := len(log.Events()); got != 0 {
		t.Fatalf("in-memory events = %d, want 0 after rolled-back start", got)
	}
}

// byteTokensStr is the matching 1-token-per-byte estimator for BasicEngine's
// Tokenizer (a per-string estimator), so engine pressure and wiring pressure
// agree in tests.
func byteTokensStr(s string) int { return len(s) }

// compactStubLLM answers every summary request with a fixed text (or an error).
type compactStubLLM struct {
	text string
	err  error
}

func (f *compactStubLLM) Stream(_ context.Context, _ llm.ChatRequest) (llm.StreamReader, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &compactStubReader{text: f.text}, nil
}

type compactStubReader struct {
	done bool
	text string
}

func (r *compactStubReader) Next() (llm.StreamEvent, error) {
	if !r.done {
		r.done = true
		return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: r.text}, nil
	}
	return llm.StreamEvent{}, io.EOF
}

// threeTurnLog builds u1 a1 u2 a2 u3 a3 (seqs 1..6).
func threeTurnLog(t *testing.T) *session.Log {
	t.Helper()
	l := session.New()
	pairs := []struct {
		typ  string
		data any
	}{
		{session.EventUserMessage, session.NewUserMessage("q1")},
		{session.EventAssistantMessage, session.NewAssistantMessage("a1", nil, "stop")},
		{session.EventUserMessage, session.NewUserMessage("q2")},
		{session.EventAssistantMessage, session.NewAssistantMessage("a2", nil, "stop")},
		{session.EventUserMessage, session.NewUserMessage("q3")},
		{session.EventAssistantMessage, session.NewAssistantMessage("a3", nil, "stop")},
	}
	for _, p := range pairs {
		if _, err := l.Append(p.typ, p.data); err != nil {
			t.Fatalf("append %s: %v", p.typ, err)
		}
	}
	return l
}

// eventTypes returns the event types of a log in order.
func eventTypes(events []session.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

// indexOf returns the first index of typ in types, or -1.
func indexOf(types []string, typ string) int {
	for i, t := range types {
		if t == typ {
			return i
		}
	}
	return -1
}

// captureStdout runs f while capturing everything printed to os.Stdout and
// returns it. os.Stdout is restored before returning.
func captureStdout(f func()) string {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// makeCompactApp builds a minimal app for compaction tests: a compaction
// config with a small pressure threshold (5) and retained tail (1), and a fresh
// log. The engine and estimator are wired by each test.
func makeCompactApp(enabled bool) *app {
	return &app{
		cfg: config.Config{
			Compaction: config.CompactionConfig{
				Enabled:        config.Bool(enabled),
				TokenThreshold: 5,
				RetainTurns:    1,
			},
		},
		log: session.New(),
	}
}

// basicEngine builds a BasicEngine matching the app's small threshold/tail with
// the given tokenizer and stub LLM (consistent with the byteTokens estimator).
func basicEngine(est compaction.TokenEstimator, llm llm.LLM) compaction.Engine {
	return compaction.NewBasic(compaction.BasicOpts{
		Tokenizer:      est,
		LLM:            llm,
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
}

// byteTokensEngine is basicEngine with the byte-counting tokenizer.
func byteTokensEngine(llm llm.LLM) compaction.Engine {
	return basicEngine(byteTokensStr, llm)
}

// markerRange decodes the shadowed [start, end] of the summary marker
// user/message in the log (surfaceOp.replace), or returns ok=false when none
// exists.
func markerRange(t *testing.T, log *session.Log) ([2]int64, bool) {
	t.Helper()
	for _, ev := range log.Events() {
		if ev.Type != session.EventUserMessage {
			continue
		}
		var d struct {
			Text      string                  `json:"text"`
			SurfaceOp *session.SurfaceReplace `json:"surfaceOp,omitempty"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		if d.SurfaceOp != nil && d.SurfaceOp.Op == "replace" {
			return [2]int64{d.SurfaceOp.Start, d.SurfaceOp.End}, true
		}
	}
	return [2]int64{}, false
}

// TestRegisterCompactionDisabledCreatesNothing verifies the D10 gate: with
// compaction.enabled=false the composition root creates no engine and registers
// no pre-step injector (dispatch-m5c-2b §2).
func TestRegisterCompactionDisabledCreatesNothing(t *testing.T) {
	app := makeCompactApp(false)
	app.cfg.AgentInstructions.Enabled = config.Bool(false)
	if err := app.registerCompaction(); err != nil {
		t.Fatalf("registerCompaction: %v", err)
	}
	if app.compaction != nil {
		t.Fatal("compaction engine must be nil when compaction.enabled=false")
	}
	if got := app.preStepInjectors(); len(got) != 0 {
		t.Fatalf("pre-step injectors = %+v, want none when compaction disabled", got)
	}
}

// TestRegisterCompactionEnabledCreatesEngine verifies the enabled path: the
// BasicEngine is created and exactly one "compaction" pre-step injector is
// registered.
func TestRegisterCompactionEnabledCreatesEngine(t *testing.T) {
	app := makeCompactApp(true)
	app.cfg.AgentInstructions.Enabled = config.Bool(false)
	app.llm = &compactStubLLM{text: "S"}
	if err := app.registerCompaction(); err != nil {
		t.Fatalf("registerCompaction: %v", err)
	}
	if app.compaction == nil {
		t.Fatal("compaction engine must be created when compaction.enabled=true")
	}
	inj := app.preStepInjectors()
	if len(inj) != 1 || inj[0].Name != "compaction" {
		t.Fatalf("pre-step injectors = %+v, want one named \"compaction\"", inj)
	}
}

func TestCompactionEngineIsSessionScoped(t *testing.T) {
	app := makeCompactApp(true)
	app.currentID = "root"
	app.llm = &compactStubLLM{text: "S"}
	if err := app.registerCompaction(); err != nil {
		t.Fatalf("registerCompaction: %v", err)
	}
	childCtx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "child"})
	child := app.compactionEngineFor(childCtx, "child")
	if child == nil || child == app.compaction {
		t.Fatalf("child compaction engine = %T/%p, want a distinct session projection", child, child)
	}
	if again := app.compactionEngineFor(childCtx, "child"); again != child {
		t.Fatal("child compaction projection was not reused within its session")
	}
}

// TestCompactCommandDisabledReportsUnavailable verifies /compact with
// compaction.enabled=false prints the unavailable message (D10) and never
// touches the log.
func TestCompactCommandDisabledReportsUnavailable(t *testing.T) {
	app := makeCompactApp(false)
	app.log = threeTurnLog(t)
	out := captureStdout(func() {
		if err := app.compactCommand(context.Background(), nil); err != nil {
			t.Errorf("compactCommand: %v", err)
		}
	})
	if !strings.Contains(out, "disabled") {
		t.Fatalf("output = %q, want a disabled message", out)
	}
	if got := len(app.log.Events()); got != 6 {
		t.Fatalf("disabled /compact must not touch the log, got %d events", got)
	}
}

// TestCompactCommandEnabledCompactsAndLogs verifies /compact with the engine
// wired: it performs one manual compaction (summary marker + fold), appends
// compaction/start → compaction/summary → compaction/end exactly once in order
// (D3, serial command path), and prints the summary, shadowed range and tokens
// saved.
func TestCompactCommandEnabledCompactsAndLogs(t *testing.T) {
	app := makeCompactApp(true)
	app.log = threeTurnLog(t)
	app.compaction = basicEngine(nil, &compactStubLLM{text: "S"})

	out := captureStdout(func() {
		if err := app.compactCommand(context.Background(), nil); err != nil {
			t.Errorf("compactCommand: %v", err)
		}
	})
	if !strings.Contains(out, "compacted") {
		t.Fatalf("output = %q, want a compacted report", out)
	}
	if !strings.Contains(out, "summary: S") {
		t.Fatalf("output = %q, want the summary printed", out)
	}
	// The summary marker landed and shadows the foldable prefix [1,4]
	// (RetainTurns=1 keeps q3/a3).
	r, ok := markerRange(t, app.log)
	if !ok {
		t.Fatal("summary marker user/message missing after /compact")
	}
	if r != [2]int64{1, 4} {
		t.Fatalf("shadowed range = %v, want [1 4]", r)
	}
	// compaction/start, compaction/summary, compaction/end each exactly once,
	// and start → summary → end in that order.
	types := eventTypes(app.log.Events())
	if n := countEvent(app.log, session.EventCompactionStart); n != 1 {
		t.Fatalf("compaction/start count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionSummary); n != 1 {
		t.Fatalf("compaction/summary count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionEnd); n != 1 {
		t.Fatalf("compaction/end count = %d, want exactly 1 (%v)", n, types)
	}
	if si, mi, ei := indexOf(types, session.EventCompactionStart), indexOf(types, session.EventCompactionSummary), indexOf(types, session.EventCompactionEnd); !(si < mi && mi < ei) {
		t.Fatalf("event order = start(%d) summary(%d) end(%d), want start<summary<end", si, mi, ei)
	}
	// The folded history substitutes the summary for the shadowed prefix.
	msgs := app.log.DeriveHistory()
	if len(msgs) != 3 || msgs[0].Text() != "S" || msgs[1].Text() != "q3" || msgs[2].Text() != "a3" {
		t.Fatalf("derived = %+v, want [S q3 a3]", msgs)
	}
}

// TestCompactCommandRegion verifies /compact region <start> <end> routes to
// CompactRegion and logs the event chain once; bad arguments are rejected.
func TestCompactCommandRegion(t *testing.T) {
	app := makeCompactApp(true)
	app.log = threeTurnLog(t)
	app.compaction = basicEngine(nil, &compactStubLLM{text: "S"})

	out := captureStdout(func() {
		if err := app.compactCommand(context.Background(), []string{"region", "1", "4"}); err != nil {
			t.Errorf("compactCommand region: %v", err)
		}
	})
	if !strings.Contains(out, "compacted") {
		t.Fatalf("output = %q, want a compacted report", out)
	}
	r, ok := markerRange(t, app.log)
	if !ok || r != [2]int64{1, 4} {
		t.Fatalf("region shadowed range = %v (ok=%v), want [1 4]", r, ok)
	}
	if n := countEvent(app.log, session.EventCompactionStart); n != 1 {
		t.Fatalf("compaction/start count = %d, want exactly 1", n)
	}

	// Bad region args are rejected with a usage error.
	if err := app.compactCommand(context.Background(), []string{"region", "abc", "4"}); err == nil {
		t.Fatal("region with non-integer seqs must be rejected")
	}
	if err := app.compactCommand(context.Background(), []string{"nonsense"}); err == nil {
		t.Fatal("unknown /compact args must be rejected")
	}
}

// compactScriptedLLM serves one fixed stream per Stream call (the compaction
// summary first, then the loop turn), recording every request.
type compactScriptedLLM struct {
	steps [][]llm.StreamEvent
	calls []llm.ChatRequest
}

func (s *compactScriptedLLM) Stream(_ context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	s.calls = append(s.calls, req)
	if len(s.steps) == 0 {
		return &compactScriptedReader{}, nil
	}
	events := s.steps[0]
	s.steps = s.steps[1:]
	return &compactScriptedReader{events: events}, nil
}

type compactScriptedReader struct {
	events []llm.StreamEvent
	i      int
}

func (r *compactScriptedReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

// longSession builds 3 turns whose content is long enough that the default
// len/4 estimator exceeds the test threshold of 5 (20 bytes/message → 5
// tokens/message, 30 tokens total).
func longSession(t *testing.T) *session.Log {
	t.Helper()
	l := session.New()
	for i := 0; i < 3; i++ {
		if _, err := l.Append(session.EventUserMessage, session.NewUserMessage(strings.Repeat("q", 20))); err != nil {
			t.Fatalf("append user: %v", err)
		}
		if _, err := l.Append(session.EventAssistantMessage, session.NewAssistantMessage(strings.Repeat("a", 20), nil, "stop")); err != nil {
			t.Fatalf("append assistant: %v", err)
		}
	}
	return l
}

func TestDefaultCompactionEstimatorSkipsReplacedSurface(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage(strings.Repeat("u", 20))); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage(strings.Repeat("a", 20), nil, "stop")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageReplace("summary", 1, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("tail")); err != nil {
		t.Fatal(err)
	}
	want := len("summary")/4 + len("tail")/4
	if got := defaultCompactionEstimator(log); got != want {
		t.Fatalf("surface estimate = %d, want %d after replacing seq 1..2", got, want)
	}
}

func TestCompactionInjectorStopsBeforeEstimateWhenCancelled(t *testing.T) {
	app := makeCompactApp(true)
	app.log = threeTurnLog(t)
	app.compaction = byteTokensEngine(&compactStubLLM{text: "S"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if msgs := app.compactionInjector(byteTokens).Inject(ctx, "hello"); msgs != nil {
		t.Fatalf("injected %+v after cancellation, want nil", msgs)
	}
	if got := countEvent(app.log, session.EventCompactionStart); got != 0 {
		t.Fatalf("compaction/start count = %d after cancellation, want 0", got)
	}
}

// TestCompactionInjectorTriggersAndLogsExactlyOnce verifies the pre-step
// injector's pressure path (dispatch-m5c-2b §2/§3): when the estimated surface
// exceeds the threshold it calls CompactIfNeeded (summary marker + fold), logs
// compaction/start → compaction/summary → compaction/end each exactly once in
// order (D3, serial path), and injects only the short notice — not the summary
// body.
func TestCompactionInjectorTriggersAndLogsExactlyOnce(t *testing.T) {
	app := makeCompactApp(true)
	app.log = threeTurnLog(t) // byteTokens surface = 12 > threshold 5
	app.compaction = byteTokensEngine(&compactStubLLM{text: "S"})

	msgs := app.compactionInjector(byteTokens).Inject(context.Background(), "hello")
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("injected = %+v, want exactly one user notice", msgs)
	}
	if !strings.Contains(msgs[0].Text(), "compacted") {
		t.Fatalf("notice content = %q, want a compacted hint", msgs[0].Text())
	}
	if strings.Contains(msgs[0].Text(), "summary:") || msgs[0].Text() == "S" {
		t.Fatalf("injected content must not be the summary body: %q", msgs[0].Text())
	}
	// The summary marker landed (D1: old events stay, the marker shadows them).
	if _, ok := markerRange(t, app.log); !ok {
		t.Fatal("summary marker missing after auto-compaction")
	}
	// start → summary → end, each exactly once, in that order.
	types := eventTypes(app.log.Events())
	if n := countEvent(app.log, session.EventCompactionStart); n != 1 {
		t.Fatalf("compaction/start count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionSummary); n != 1 {
		t.Fatalf("compaction/summary count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionEnd); n != 1 {
		t.Fatalf("compaction/end count = %d, want exactly 1 (%v)", n, types)
	}
	if si, mi, ei := indexOf(types, session.EventCompactionStart), indexOf(types, session.EventCompactionSummary), indexOf(types, session.EventCompactionEnd); !(si < mi && mi < ei) {
		t.Fatalf("event order = start(%d) summary(%d) end(%d), want start<summary<end", si, mi, ei)
	}
	// The model-visible history is folded: summary substitutes the prefix.
	msgsD := app.log.DeriveHistory()
	if len(msgsD) != 3 || msgsD[0].Text() != "S" || msgsD[1].Text() != "q3" || msgsD[2].Text() != "a3" {
		t.Fatalf("derived = %+v, want [S q3 a3]", msgsD)
	}
}

// TestCompactionInjectorUnderPressureNoEvents verifies the no-op path: under
// the threshold the injector compacts nothing, logs no compaction/* event and
// injects no context (a per-turn no-op, so over-limit turns never spam the log
// with orphan compaction/start rows).
func TestCompactionInjectorUnderPressureNoEvents(t *testing.T) {
	app := makeCompactApp(true)
	app.cfg.Compaction.TokenThreshold = 1 << 20
	app.log = threeTurnLog(t)
	app.compaction = compaction.NewBasic(compaction.BasicOpts{
		Tokenizer:      byteTokensStr,
		LLM:            &compactStubLLM{text: "S"},
		Model:          "m",
		TokenThreshold: 1 << 20,
		RetainTurns:    1,
	})
	msgs := app.compactionInjector(byteTokens).Inject(context.Background(), "hi")
	if msgs != nil {
		t.Fatalf("injected %+v under threshold, want nil", msgs)
	}
	if n := countEvent(app.log, session.EventCompactionStart); n != 0 {
		t.Fatalf("compaction/start logged %d times under threshold, want 0", n)
	}
	if got := len(app.log.Events()); got != 6 {
		t.Fatalf("log grew to %d events under threshold, want 6", got)
	}
}

// TestCompactionInjectorNilEngineNoOp verifies the injector is inert when the
// engine is absent (the disabled guard, D10).
func TestCompactionInjectorNilEngineNoOp(t *testing.T) {
	app := makeCompactApp(false)
	app.log = threeTurnLog(t)
	if msgs := app.compactionInjector(byteTokens).Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("injected %+v with a nil engine, want nil", msgs)
	}
	if got := len(app.log.Events()); got != 6 {
		t.Fatalf("log grew to %d events with a nil engine, want 6", got)
	}
}

// TestCompactionInjectorSummaryFailureIsFailOpen verifies a failing summary
// model does not abort the turn: the injector returns nil context (fail-open),
// closes the lifecycle with compaction/end, and never appends the notice.
func TestCompactionInjectorSummaryFailureIsFailOpen(t *testing.T) {
	app := makeCompactApp(true)
	app.log = threeTurnLog(t)
	app.compaction = byteTokensEngine(&compactStubLLM{err: errors.New("model down")})
	msgs := app.compactionInjector(byteTokens).Inject(context.Background(), "hi")
	if msgs != nil {
		t.Fatalf("injected %+v on summary failure, want nil (fail-open)", msgs)
	}
	if n := countEvent(app.log, session.EventCompactionSummary); n != 0 {
		t.Fatalf("compaction/summary logged %d times after a failed summary, want 0", n)
	}
	if n := countEvent(app.log, session.EventCompactionEnd); n != 1 {
		t.Fatalf("compaction/end logged %d times after a failed summary, want 1", n)
	}
	// The attempt marker remains — an orphan start reveals the interrupted
	// attempt (ADR 决策 ③).
	if n := countEvent(app.log, session.EventCompactionStart); n != 1 {
		t.Fatalf("compaction/start count = %d, want exactly 1 (the attempt marker)", n)
	}
}

// TestLoopPreStepAutoCompacts is the end-to-end wiring test: a turn driven
// through app.newLoop() runs the "compaction" pre-step injector before the
// first step's model request, folds the over-threshold prefix, logs the
// compaction/* chain exactly once and injects the notice into the first request
// only (turn/step structure unchanged, D4).
func TestLoopPreStepAutoCompacts(t *testing.T) {
	app := makeCompactApp(true)
	app.log = longSession(t) // default len/4 estimator: 30 tokens > threshold 5
	model := &compactScriptedLLM{steps: [][]llm.StreamEvent{
		{ // call 1: the compaction summary request
			{Kind: llm.StreamTextDelta, Text: "S"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
		{ // call 2: the turn's first request
			{Kind: llm.StreamTextDelta, Text: "ok"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	app.llm = model
	// The engine uses the default (len/4) tokenizer so its pressure gate agrees
	// with preStepInjectors' defaultCompactionEstimator.
	app.compaction = compaction.NewBasic(compaction.BasicOpts{
		LLM:            model,
		Model:          "m",
		TokenThreshold: 5,
		RetainTurns:    1,
	})
	app.reg = tools.New()
	app.prompt = prompt.New("You are helpful.")

	lp := app.newLoop()
	captureStdout(func() {
		if err := lp.Run(context.Background(), "hello"); err != nil {
			t.Errorf("loop run: %v", err)
		}
	})

	// The summary model was called before the turn's first request.
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2 (summary then turn)", len(model.calls))
	}
	// The compaction/* chain landed exactly once in order.
	types := eventTypes(app.log.Events())
	if n := countEvent(app.log, session.EventCompactionStart); n != 1 {
		t.Fatalf("compaction/start count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionSummary); n != 1 {
		t.Fatalf("compaction/summary count = %d, want exactly 1 (%v)", n, types)
	}
	if n := countEvent(app.log, session.EventCompactionEnd); n != 1 {
		t.Fatalf("compaction/end count = %d, want exactly 1 (%v)", n, types)
	}
	// The first request carries the notice, the folded summary and the user
	// message; the turn still completed normally.
	req := model.calls[1].Messages
	var noticeSeen, summarySeen, userSeen bool
	for _, m := range req {
		switch {
		case m.Text() == "hello":
			userSeen = true
		case m.Text() == "S":
			summarySeen = true
		case strings.Contains(m.Text(), "compacted"):
			noticeSeen = true
		}
	}
	if !noticeSeen || !summarySeen || !userSeen {
		t.Fatalf("first request = %+v, want notice + folded summary + user message", req)
	}
}
