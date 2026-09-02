package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	llmretry "github.com/jabing/shutu-agent/internal/llm/retry"
	"github.com/jabing/shutu-agent/internal/observability"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// scriptedLLM returns a fixed per-step sequence of stream events, one Stream
// call per step, then EOF.
type scriptedLLM struct {
	steps [][]llm.StreamEvent
	calls []llm.ChatRequest
}

func (s *scriptedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	s.calls = append(s.calls, req)
	if len(s.steps) == 0 {
		return &scriptedReader{}, nil
	}
	events := s.steps[0]
	s.steps = s.steps[1:]
	return &scriptedReader{events: events}, nil
}

type scriptedReader struct {
	events []llm.StreamEvent
	i      int
}

type structuredFailureTool struct{}

type additionalContextTool struct{}
type concludingTool struct{}

func (additionalContextTool) Name() string        { return "additional_context" }
func (additionalContextTool) Description() string { return "returns deferred context" }
func (additionalContextTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (additionalContextTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (additionalContextTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (additionalContextTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{
		Value:  "ok",
		Output: "ok",
		AdditionalContextMessages: []llm.Message{{
			Role: llm.RoleUser, SourceKind: "plugin", SourcePlugin: "deferred-test",
			Content: []llm.ContentBlock{llm.Text("deferred rich context")},
		}},
	}, nil
}

func (concludingTool) Name() string        { return "conclude_turn" }
func (concludingTool) Description() string { return "ends the current turn" }
func (concludingTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (concludingTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (concludingTool) Execute(context.Context, any) (string, error) { return "done", nil }
func (concludingTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{Value: "done", Output: "done", ConcludesTurn: true}, nil
}

func (structuredFailureTool) Name() string        { return "structured_failure" }
func (structuredFailureTool) Description() string { return "returns a structured failure" }
func (structuredFailureTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (structuredFailureTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (structuredFailureTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (structuredFailureTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{Output: "structured failure", IsError: true, Error: &tools.ErrorInfo{
		Name: "ToolError", Code: tools.CodeToolExecutionError,
	}}, nil
}
func (structuredFailureTool) ConcurrencySafe(any) bool { return true }

func TestToolMetricsCountStructuredFailuresInSerialAndParallelCalls(t *testing.T) {
	cases := []struct {
		name      string
		calls     []llm.ToolCall
		parallel  int
		wantCalls uint64
	}{
		{name: "serial", calls: []llm.ToolCall{{ID: "serial-1", Name: "structured_failure", Arguments: "{}"}}, parallel: 1, wantCalls: 1},
		{name: "parallel", calls: []llm.ToolCall{{ID: "parallel-1", Name: "structured_failure", Arguments: "{}"}, {ID: "parallel-2", Name: "structured_failure", Arguments: "{}"}}, parallel: 2, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedLLM{steps: [][]llm.StreamEvent{
				{{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: tc.calls}},
				{{Kind: llm.StreamTextDelta, Text: "done"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
			}}
			loop, _, reg := newTestLoop(t, model)
			if err := reg.Register(structuredFailureTool{}); err != nil {
				t.Fatal(err)
			}
			reg.SetPolicy(tools.Policy{Enabled: []string{"structured_failure"}})
			loop.maxParallelToolCalls = tc.parallel
			loop.metrics = observability.New()
			if err := loop.Run(context.Background(), tc.name); err != nil {
				t.Fatalf("run: %v", err)
			}
			got := loop.metrics.Snapshot()
			if got.ToolCalls != tc.wantCalls || got.ToolFailures != tc.wantCalls {
				t.Fatalf("metrics = %+v, want %d failed calls", got, tc.wantCalls)
			}
		})
	}
}

func TestTracerCorrelatesProviderAndToolSpans(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "trace-call", Name: "conclude_turn", Arguments: "{}"}}}},
	}}
	loop, _, reg := newTestLoop(t, model)
	if err := reg.Register(concludingTool{}); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{"conclude_turn"}})
	loop.runtimeAgentID = "trace-agent"
	loop.runtimeSessionID = "trace-session"
	loop.tracer = observability.NewTracer(16)
	if err := loop.Run(context.Background(), "trace this"); err != nil {
		t.Fatalf("run: %v", err)
	}
	spans := loop.tracer.Spans()
	byName := make(map[string]observability.Span)
	for _, span := range spans {
		byName[span.Name] = span
	}
	for _, name := range []string{"agent.step", "llm.request", "tool.conclude_turn"} {
		if byName[name].ID == "" {
			t.Fatalf("missing %s span: %+v", name, spans)
		}
	}
	if byName["llm.request"].Correlation.RequestID != "turn:1:step:1" || byName["llm.request"].Correlation.SessionID == "" {
		t.Fatalf("request correlation = %+v", byName["llm.request"].Correlation)
	}
	if byName["llm.request"].ParentID != byName["agent.step"].ID || byName["tool.conclude_turn"].ParentID != byName["agent.step"].ID {
		t.Fatalf("span parents = request:%q tool:%q step:%q", byName["llm.request"].ParentID, byName["tool.conclude_turn"].ParentID, byName["agent.step"].ID)
	}
}

func TestToolAdditionalContextsArePersistedAfterResultAndReachNextStep(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "ctx-1", Name: "additional_context", Arguments: "{}"}}}},
		{{Kind: llm.StreamTextDelta, Text: "done"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	loop, log, reg := newTestLoop(t, model)
	if err := reg.Register(additionalContextTool{}); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{"additional_context"}})
	if err := loop.Run(context.Background(), "use context"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(model.calls))
	}
	found := false
	for _, message := range model.calls[1].Messages {
		if message.Text() == "deferred rich context" {
			found = true
			if message.SourceKind != "" {
				t.Fatalf("runtime-only source leaked into provider request: %+v", message)
			}
		}
	}
	if !found {
		t.Fatalf("second request lost additional context: %+v", model.calls[1].Messages)
	}
	events := log.Events()
	resultIndex, contextIndex := -1, -1
	for i, event := range events {
		if event.Type == session.EventToolResult && resultIndex < 0 {
			resultIndex = i
		}
		if event.Type == session.EventUserMessage && strings.Contains(string(event.Data), "deferred rich context") {
			contextIndex = i
		}
	}
	if resultIndex < 0 || contextIndex <= resultIndex {
		t.Fatalf("additional context ordering result=%d context=%d events=%v", resultIndex, contextIndex, events)
	}
}

func TestConcludesTurnStopsAfterSubmittedToolBatch(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "end-1", Name: "conclude_turn", Arguments: "{}"}}},
	}}}
	loop, log, reg := newTestLoop(t, model)
	if err := reg.Register(concludingTool{}); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{"conclude_turn"}})
	if err := loop.Run(context.Background(), "finish now"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("llm calls = %d, want one terminal step", len(model.calls))
	}
	if !strings.Contains(string(log.Events()[len(log.Events())-1].Data), `"status":"completed"`) {
		t.Fatalf("turn did not close completed: %+v", log.Events())
	}
}

func TestDurableRequestHeaderFailureStopsProviderExecution(t *testing.T) {
	wantErr := errors.New("durable request header failed")
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "must not run"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	log := session.New()
	log.SetSink(func(ev session.Event) error {
		if ev.Type == session.EventRequestHeader {
			return wantErr
		}
		return nil
	})
	loop := New(Config{
		LLM: model, Log: log, Tools: newTestRegistry(t),
		Prompt: prompt.New("You are helpful."), Provider: "deepseek", Model: "deepseek-chat",
	})
	if err := loop.Run(context.Background(), "hello"); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want durable error %v", err, wantErr)
	}
	if len(model.calls) != 0 {
		t.Fatalf("provider calls = %d after durable header failure, want 0", len(model.calls))
	}
	for _, ev := range log.Events() {
		if ev.Type == session.EventRequestHeader {
			t.Fatal("failed request/header was left in the live log")
		}
	}
}

type failingThenScriptedLLM struct {
	failures int
	next     *scriptedLLM
	calls    int
}

type alwaysFailureThenScriptedLLM struct {
	failures int
	next     *scriptedLLM
	calls    int
}

func (s *alwaysFailureThenScriptedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return nil, llm.NewFailureError("authorization temporarily unavailable", "AUTH", errors.New("upstream"))
	}
	return s.next.Stream(ctx, req)
}

func (s *alwaysFailureThenScriptedLLM) ID() string      { return "always-fake" }
func (s *alwaysFailureThenScriptedLLM) Available() bool { return true }
func (s *alwaysFailureThenScriptedLLM) RetryPolicy() llm.RetryPolicy {
	return llm.RetryPolicy{Mode: "always", InitialDelayMS: 1, MaxDelayMS: 1, JitterRatio: 0}
}

func (s *failingThenScriptedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return nil, llm.NewFailureError("temporary provider failure", "SERVER", errors.New("upstream"))
	}
	return s.next.Stream(ctx, req)
}

func (s *failingThenScriptedLLM) ID() string      { return "fake" }
func (s *failingThenScriptedLLM) Available() bool { return true }
func (s *failingThenScriptedLLM) RetryPolicy() llm.RetryPolicy {
	return llm.RetryPolicy{
		Mode: "normal", MaxRetries: 1,
		InitialDelayMS: 1, MaxDelayMS: 1,
		RetryableCodes: []string{"SERVER"},
	}
}

func (r *scriptedReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

func newTestLoop(t *testing.T, llm llm.LLM) (*Loop, *session.Log, *tools.Registry) {
	t.Helper()
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatalf("register get_time: %v", err)
	}
	log := session.New()
	loop := New(Config{
		LLM:    llm,
		Log:    log,
		Tools:  reg,
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
	})
	return loop, log, reg
}

func TestRunSimpleAnswerNoTools(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "Hi "},
		{Kind: llm.StreamTextDelta, Text: "there"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, log, _ := newTestLoop(t, model)

	var streamed strings.Builder
	loop.onText = func(delta string) { streamed.WriteString(delta) }

	if err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if streamed.String() != "Hi there" {
		t.Fatalf("streamed = %q", streamed.String())
	}

	events := log.Events()
	if len(events) != 7 {
		t.Fatalf("events = %d, want 7 (turn, user, step, aggregated chunk, assistant, step, turn)", len(events))
	}
	types := []string{}
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := []string{
		session.EventTurnStart,
		session.EventStepStart,
		session.EventUserMessage,
		session.EventAssistantChunk,
		session.EventAssistantMessage,
		session.EventStepEnd,
		session.EventTurnEnd,
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (all: %v)", i, types[i], want[i], types)
		}
	}
	var chunk struct {
		Chunk struct {
			Text string `json:"text"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(events[3].Data, &chunk); err != nil || chunk.Chunk.Text != "Hi there" {
		t.Fatalf("aggregated chunk = %s, want Hi there", events[3].Data)
	}
	// The model must receive the user message inside a system-prefixed request.
	if len(model.calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(model.calls))
	}
	msgs := model.calls[0].Messages
	if msgs[0].Role != llm.RoleSystem {
		t.Fatalf("first message role = %v, want system", msgs[0].Role)
	}
	if len(msgs) < 2 || msgs[1].Role != llm.RoleUser || msgs[1].Text() != "hello" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestRequestContextIsDeduplicatedAndChangesWithRouteCapacity(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamTextDelta, Text: "one"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
		{{Kind: llm.StreamTextDelta, Text: "two"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
		{{Kind: llm.StreamTextDelta, Text: "three"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	log := session.New()
	for _, text := range []string{"one", "two"} {
		loop := New(Config{
			LLM: model, Log: log, Tools: newTestRegistry(t),
			Prompt: prompt.New("You are helpful."), Provider: "mock", Model: "m", ContextWindow: 128000,
		})
		if err := loop.Run(context.Background(), text); err != nil {
			t.Fatalf("run %q: %v", text, err)
		}
	}
	loop := New(Config{
		LLM: model, Log: log, Tools: newTestRegistry(t),
		Prompt: prompt.New("You are helpful."), Provider: "mock", Model: "m", ContextWindow: 256000,
	})
	if err := loop.Run(context.Background(), "three"); err != nil {
		t.Fatalf("run capacity change: %v", err)
	}
	var got []string
	for _, event := range log.Events() {
		if event.Type != session.EventRequestContext {
			continue
		}
		var data struct {
			Provider      string `json:"provider"`
			Model         string `json:"model"`
			ContextWindow int    `json:"contextWindow,omitempty"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s/%s/%d", data.Provider, data.Model, data.ContextWindow))
	}
	want := []string{"mock/m/128000", "mock/m/256000"}
	if len(got) != len(want) || len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("request/context records = %v, want %v", got, want)
	}
}

func TestLoopPropagatesStructuredCorrelationToRequestHook(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, _, _ := newTestLoop(t, model)
	loop.runtimeAgentID = "agent-1"
	loop.runtimeSessionID = "session-1"
	var got runtimectx.Correlation
	loop.requestHooks = []RequestHook{func(ctx context.Context, payload RequestPayload, next RequestNext) (llm.ChatRequest, error) {
		var ok bool
		got, ok = runtimectx.CorrelationOf(ctx)
		if !ok {
			t.Fatal("request hook lost correlation context")
		}
		return next(ctx, payload)
	}}
	if err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.AgentID != "agent-1" || got.SessionID != "session-1" || got.TurnID != "turn:1" || got.StepID != "step:1" || got.RequestID != "turn:1:step:1" {
		t.Fatalf("request correlation = %+v", got)
	}
}

func TestRunMessagesPreservesClaimedInputsAsSeparateEvents(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, log, _ := newTestLoop(t, model)
	inputs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("first")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("second")}},
	}
	if err := loop.RunMessages(context.Background(), inputs); err != nil {
		t.Fatalf("RunMessages: %v", err)
	}
	history := log.DeriveHistory()
	if len(history) < 3 || history[0].Text() != "first" || history[1].Text() != "second" {
		t.Fatalf("derived history = %+v, want two separate user messages", history)
	}
	if len(model.calls) != 1 || len(model.calls[0].Messages) < 3 || model.calls[0].Messages[1].Text() != "first" || model.calls[0].Messages[2].Text() != "second" {
		t.Fatalf("model messages = %+v, want separate inputs", model.calls)
	}
}

// TestModelRequestUsesDurableHistoryNotProcessMemory proves the loop's request
// assembly is a pure projection of committed events. A caller-declared
// Persisted input that has no durable row is intentionally omitted.
func TestModelRequestUsesDurableHistoryNotProcessMemory(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, log, _ := newTestLoop(t, model)
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("durable")); err != nil {
		t.Fatal(err)
	}
	input := llm.Message{
		Role:      llm.RoleUser,
		Content:   []llm.ContentBlock{llm.Text("memory-only")},
		Persisted: true,
	}
	if err := loop.RunMessages(context.Background(), []llm.Message{input}); err != nil {
		t.Fatalf("RunMessages: %v", err)
	}
	if len(model.calls) != 1 || len(model.calls[0].Messages) != 2 {
		t.Fatalf("model request = %+v, want system plus one durable message", model.calls)
	}
	if got := model.calls[0].Messages[1].Text(); got != "durable" {
		t.Fatalf("model history text = %q, want durable row", got)
	}
	if strings.Contains(model.calls[0].Messages[1].Text(), "memory-only") {
		t.Fatalf("process-memory-only input reached provider: %+v", model.calls[0].Messages)
	}
}

func TestPreStepWaterfallCanRewriteAndReject(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	var order []string
	loop, _, _ := newTestLoop(t, model)
	loop.preStepHooks = []PreStepHook{
		func(ctx context.Context, payload PreStepPayload, next PreStepNext) (PreStepDecision, error) {
			order = append(order, "outer-before")
			decision, err := next(ctx, payload)
			order = append(order, "outer-after")
			if err == nil && decision.Kind == "enter" {
				decision.Messages[0].SetText("rewritten")
			}
			return decision, err
		},
		func(ctx context.Context, payload PreStepPayload, next PreStepNext) (PreStepDecision, error) {
			order = append(order, "inner-before")
			return next(ctx, payload)
		},
	}
	if err := loop.Run(context.Background(), "original"); err != nil {
		t.Fatalf("rewritten run: %v", err)
	}
	if strings.Join(order, ",") != "outer-before,inner-before,outer-after" {
		t.Fatalf("waterfall order = %v", order)
	}
	if len(model.calls) != 1 || model.calls[0].Messages[1].Text() != "rewritten" {
		t.Fatalf("request messages = %+v", model.calls[0].Messages)
	}

	rejectedModel := &scriptedLLM{}
	rejected, rejectedLog, _ := newTestLoop(t, rejectedModel)
	rejected.preStepHooks = []PreStepHook{func(context.Context, PreStepPayload, PreStepNext) (PreStepDecision, error) {
		return PreStepDecision{Kind: "reject"}, nil
	}}
	if err := rejected.Run(context.Background(), "blocked"); err != nil {
		t.Fatalf("rejected run: %v", err)
	}
	if len(rejectedModel.calls) != 0 {
		t.Fatalf("rejected request count = %d, want 0", len(rejectedModel.calls))
	}
	if got := rejectedLog.Events(); len(got) != 2 || got[0].Type != session.EventTurnStart || got[1].Type != session.EventTurnEnd {
		t.Fatalf("rejected durable events = %+v, want turn start/end only", got)
	}
	if !strings.Contains(string(rejectedLog.Events()[1].Data), `"kind":"blocked"`) {
		t.Fatalf("rejected turn end = %s, want blocked reason", rejectedLog.Events()[1].Data)
	}
}

func TestRequestWaterfallCanRetry(t *testing.T) {
	model := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "recovered"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, model)
	loop.requestErrorHooks = []RequestErrorHook{func(ctx context.Context, payload RequestErrorPayload, next func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		if payload.Error == nil {
			t.Fatal("missing request error")
		}
		return true, nil
	}}
	if err := loop.Run(context.Background(), "retry"); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if model.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", model.calls)
	}
	if got := log.DeriveHistory(); len(got) < 2 || got[len(got)-1].Text() != "recovered" {
		t.Fatalf("history = %+v", got)
	}
}

func TestProviderRetryPublishesScheduledAndStartedDurably(t *testing.T) {
	inner := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "recovered"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	model := llmretry.WrapProvider(inner, llmretry.Config{
		MaxRetries: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	})
	loop, log, _ := newTestLoop(t, model)
	loop.provider = "fake"
	if err := loop.Run(context.Background(), "retry lifecycle"); err != nil {
		t.Fatalf("retry lifecycle run: %v", err)
	}
	events := log.Events()
	find := func(kind string) int {
		for i, event := range events {
			if event.Type == kind {
				return i
			}
		}
		return -1
	}
	scheduled, started, assistant := find(session.EventLLMRetry), find(session.EventLLMRetryStarted), find(session.EventAssistantMessage)
	if scheduled < 0 || started < 0 || assistant < 0 || !(scheduled < started && started < assistant) {
		t.Fatalf("retry lifecycle indexes = scheduled %d, started %d, assistant %d; events=%v", scheduled, started, assistant, events)
	}
	var scheduledData struct {
		RetryID string      `json:"retryId"`
		Failure llm.Failure `json:"failure"`
	}
	var startedData struct {
		RetryID string `json:"retryId"`
	}
	if err := json.Unmarshal(events[scheduled].Data, &scheduledData); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[started].Data, &startedData); err != nil {
		t.Fatal(err)
	}
	if scheduledData.RetryID == "" || scheduledData.RetryID != startedData.RetryID || scheduledData.Failure.Code != "SERVER" {
		t.Fatalf("retry payloads = scheduled=%+v started=%+v", scheduledData, startedData)
	}
}

func TestProviderRetryUsesEffectiveHookRoutedProvider(t *testing.T) {
	model := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "routed"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, model)
	loop.requestHooks = []RequestHook{func(ctx context.Context, payload RequestPayload, next RequestNext) (llm.ChatRequest, error) {
		request, err := next(ctx, payload)
		if err != nil {
			return llm.ChatRequest{}, err
		}
		request.Provider = "hook-routed"
		request.Model = "hook-model"
		return request, nil
	}}
	if err := loop.Run(context.Background(), "route retry"); err != nil {
		t.Fatalf("route retry run: %v", err)
	}
	for _, event := range log.Events() {
		if event.Type != session.EventLLMRetry {
			continue
		}
		var payload struct {
			Provider string `json:"provider"`
			Turn     int    `json:"turn"`
			Step     int    `json:"step"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Provider != "hook-routed" || payload.Turn != 1 || payload.Step != 1 {
			t.Fatalf("retry route payload = %+v, want hook-routed at 1/1", payload)
		}
		return
	}
	t.Fatal("missing provider retry event")
}

func TestRequestResolverUsesRoutedTransportAndRetryPolicy(t *testing.T) {
	primary := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	secondary := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "secondary"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, primary)
	loop.provider = "primary"
	loop.requestHooks = []RequestHook{func(ctx context.Context, payload RequestPayload, next RequestNext) (llm.ChatRequest, error) {
		request, err := next(ctx, payload)
		if err != nil {
			return llm.ChatRequest{}, err
		}
		request.Provider = "secondary"
		request.Model = "secondary-model"
		return request, nil
	}}
	loop.resolveRequestLLM = func(_ context.Context, request llm.ChatRequest) (llm.LLM, error) {
		if request.Provider != "secondary" || request.Model != "secondary-model" {
			t.Fatalf("resolver saw pre-hook request: %+v", request)
		}
		return secondary, nil
	}
	if err := loop.Run(context.Background(), "route transport"); err != nil {
		t.Fatalf("routed run: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("primary transport calls = %d, want 0", primary.calls)
	}
	if secondary.calls != 2 {
		t.Fatalf("secondary transport calls = %d, want initial failure plus policy retry", secondary.calls)
	}
	if got := log.DeriveHistory(); len(got) < 2 || got[len(got)-1].Text() != "secondary" {
		t.Fatalf("routed history = %+v", got)
	}
}

func TestLoopExecutesDeferredProviderRetryAtDurableBoundary(t *testing.T) {
	inner := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "recovered at boundary"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	model := llmretry.WrapProviderForLoop(inner, llmretry.Config{
		MaxRetries: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		JitterRatio: 0,
	})
	loop, log, _ := newTestLoop(t, model)
	loop.provider = "fake"
	if err := loop.Run(context.Background(), "deferred retry"); err != nil {
		t.Fatalf("deferred retry run: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", inner.calls)
	}
	var scheduled, started, updates int
	for _, event := range log.Events() {
		switch event.Type {
		case session.EventLLMRetry:
			scheduled++
		case session.EventLLMRetryStarted:
			started++
		case session.EventRequestHeader:
			var payload struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Reason == "update" {
				updates++
			}
		}
	}
	if scheduled != 1 || started != 1 || updates != 1 {
		t.Fatalf("retry boundary events = scheduled %d started %d header updates %d", scheduled, started, updates)
	}
}

func TestAlwaysProviderRetryDelegatesToRequestErrorHooksAndRetriesAnyFailure(t *testing.T) {
	model := &alwaysFailureThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "recovered"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, model)
	loop.provider = "always-fake"
	seen := 0
	loop.requestErrorHooks = []RequestErrorHook{func(_ context.Context, payload RequestErrorPayload, _ func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		seen++
		failure, ok := llm.FailureFacts(payload.Error)
		if !ok || failure.Code != "AUTH" {
			t.Fatalf("request failure = %+v, ok=%v", failure, ok)
		}
		return false, nil // allow always-mode provider fallback to own the retry
	}}
	if err := loop.Run(context.Background(), "always fallback"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if model.calls != 2 || seen != 1 {
		t.Fatalf("calls/hooks = %d/%d, want 2/1", model.calls, seen)
	}
	var scheduled int
	for _, event := range log.Events() {
		if event.Type == session.EventLLMRetry {
			scheduled++
		}
	}
	if scheduled != 1 {
		t.Fatalf("scheduled retries = %d, want 1", scheduled)
	}
}

func TestRequestErrorWaterfallPreservesProviderFailureMetadata(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "content_filter", Failure: &llm.Failure{
			Message: "blocked", Code: "CONTENT_FILTER", Status: 429,
			ProviderRetryAfterMS: 1500, RequestID: "req-42",
		}},
	}}}
	loop, _, _ := newTestLoop(t, model)
	var observed llm.Failure
	loop.requestErrorHooks = []RequestErrorHook{func(_ context.Context, payload RequestErrorPayload, _ func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		facts, ok := llm.FailureFacts(payload.Error)
		if !ok {
			t.Fatal("request error lost failure facts")
		}
		observed = facts
		return false, nil
	}}
	if err := loop.Run(context.Background(), "metadata"); err == nil {
		t.Fatal("failed request unexpectedly succeeded")
	}
	if observed.Status != 429 || observed.ProviderRetryAfterMS != 1500 || observed.RequestID != "req-42" {
		t.Fatalf("failure metadata = %+v", observed)
	}
}

func TestNormalizeRequestErrorUsesRetryVocabularyAndRedacts(t *testing.T) {
	err := normalizeRequestError(errors.New(`upstream 503: authorization: Bearer super-secret`))
	failure, ok := llm.FailureFacts(err)
	if !ok || failure.Code != "SERVER" {
		t.Fatalf("failure = %+v, ok=%v, want retryable SERVER", failure, ok)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(failure.Message, "super-secret") {
		t.Fatalf("normalized diagnostic leaked credential: %q", err)
	}
}

func TestRequestErrorWaterfallCanRetryEachFailedAttempt(t *testing.T) {
	model := &failingThenScriptedLLM{failures: 2, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "recovered after two failures"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, model)
	recoveries := 0
	loop.requestErrorHooks = []RequestErrorHook{func(_ context.Context, payload RequestErrorPayload, _ func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		recoveries++
		if payload.Error == nil {
			t.Fatal("missing request error")
		}
		return true, nil
	}}
	if err := loop.Run(context.Background(), "retry twice"); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if model.calls != 3 || recoveries != 2 {
		t.Fatalf("provider calls/recoveries = %d/%d, want 3/2", model.calls, recoveries)
	}
	if got := log.DeriveHistory(); len(got) < 2 || got[len(got)-1].Text() != "recovered after two failures" {
		t.Fatalf("history = %+v", got)
	}
}

func TestRequestErrorRetryLosesToCancellation(t *testing.T) {
	model := &failingThenScriptedLLM{failures: 1, next: &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "must not run"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}}
	loop, log, _ := newTestLoop(t, model)
	ctx, cancel := context.WithCancel(context.Background())
	loop.requestErrorHooks = []RequestErrorHook{func(context.Context, RequestErrorPayload, func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		cancel()
		return true, nil
	}}
	if err := loop.Run(ctx, "cancel retry"); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context.Canceled", err)
	}
	if model.calls != 1 {
		t.Fatalf("provider calls = %d, want one failed call", model.calls)
	}
	if got := log.Events(); got[len(got)-1].Type != session.EventTurnEnd {
		t.Fatalf("last event = %q, want turn/end", got[len(got)-1].Type)
	}
}

func TestUnsupportedFinishReasonIsStructuredFailure(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "blocked"},
		{Kind: llm.StreamFinish, FinishReason: "content_filter"},
	}}}
	loop, log, _ := newTestLoop(t, model)
	if err := loop.Run(context.Background(), "blocked"); err == nil {
		t.Fatal("run error = nil, want content-filter failure")
	} else {
		failure, ok := llm.FailureFacts(err)
		if !ok || failure.Code != "CONTENT_FILTER" {
			t.Fatalf("failure = %+v, ok=%v", failure, ok)
		}
	}
	events := log.Events()
	end := events[len(events)-1]
	if end.Type != session.EventTurnEnd || !strings.Contains(string(end.Data), `"CONTENT_FILTER"`) {
		t.Fatalf("turn end = %+v, want CONTENT_FILTER", end)
	}
}

func TestUnsupportedFinishReasonCanBeRetried(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "refusal"}},
		{{Kind: llm.StreamTextDelta, Text: "recovered"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	loop, log, _ := newTestLoop(t, model)
	loop.requestErrorHooks = []RequestErrorHook{func(_ context.Context, payload RequestErrorPayload, _ func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		failure, ok := llm.FailureFacts(payload.Error)
		if !ok || failure.Code != "REFUSAL" {
			return false, fmt.Errorf("unexpected failure: %v", payload.Error)
		}
		return true, nil
	}}
	if err := loop.Run(context.Background(), "retry refusal"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(model.calls))
	}
	if got := log.DeriveHistory(); len(got) < 2 || got[len(got)-1].Text() != "recovered" {
		t.Fatalf("history = %+v", got)
	}
}

func TestRequestErrorWaterfallCanRetryRepeatedFailedFinishes(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "refusal"}},
		{{Kind: llm.StreamFinish, FinishReason: "content_filter"}},
		{{Kind: llm.StreamTextDelta, Text: "finally recovered"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	loop, log, _ := newTestLoop(t, model)
	seen := make([]string, 0, 2)
	loop.requestErrorHooks = []RequestErrorHook{func(_ context.Context, payload RequestErrorPayload, _ func(context.Context, RequestErrorPayload) (bool, error)) (bool, error) {
		failure, ok := llm.FailureFacts(payload.Error)
		if !ok {
			t.Fatalf("request error lost failure facts: %v", payload.Error)
		}
		seen = append(seen, failure.Code)
		return len(seen) < 3, nil
	}}
	if err := loop.Run(context.Background(), "retry failed finishes"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 3 || len(seen) != 2 || seen[0] != "REFUSAL" || seen[1] != "CONTENT_FILTER" {
		t.Fatalf("calls/errors = %d/%v, want 3/[REFUSAL CONTENT_FILTER]", len(model.calls), seen)
	}
	if got := log.DeriveHistory(); len(got) < 2 || got[len(got)-1].Text() != "finally recovered" {
		t.Fatalf("history = %+v", got)
	}
}

func TestStreamFailureIsStructuredAndPersisted(t *testing.T) {
	failure := llm.Failure{Message: "completed stream contained no content", Code: "EMPTY_RESPONSE"}
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop", Failure: &failure},
	}}}
	loop, log, _ := newTestLoop(t, model)
	if err := loop.Run(context.Background(), "empty"); err == nil {
		t.Fatal("run error = nil, want empty-response failure")
	} else if got, ok := llm.FailureFacts(err); !ok || got.Code != "EMPTY_RESPONSE" {
		t.Fatalf("failure = %+v, ok=%v", got, ok)
	}
	events := log.Events()
	if len(events) == 0 {
		t.Fatal("no durable events after stream failure")
	}
	if !strings.Contains(string(events[len(events)-1].Data), `"EMPTY_RESPONSE"`) {
		t.Fatalf("last event = %+v, want persisted EMPTY_RESPONSE", events[len(events)-1])
	}
}

// TestRunCarriesReasoningEffort verifies the loop forwards the configured
// thinking effort to every model request (dsh 思考强度 对齐).
func TestRunCarriesReasoningEffort(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	loop := New(Config{
		LLM:                   model,
		Log:                   log,
		Tools:                 reg,
		Prompt:                prompt.New("You are helpful."),
		Model:                 "deepseek-v4-flash",
		ReasoningEffort:       "max",
		ReasoningBudgetTokens: 2048,
	})
	if err := loop.Run(context.Background(), "think"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 1 || model.calls[0].ReasoningEffort != "max" || model.calls[0].ReasoningBudgetTokens != 2048 {
		t.Fatalf("calls = %+v, want max effort with 2048-token budget", model.calls)
	}
}

func TestRunRecordsLLMRequestMetadata(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	loop := New(Config{
		LLM:             model,
		Log:             log,
		Tools:           reg,
		Prompt:          prompt.New("You are helpful."),
		Provider:        "deepseek-official",
		Model:           "deepseek-v4-flash",
		ReasoningEffort: "high",
	})
	if err := loop.Run(context.Background(), "metadata"); err != nil {
		t.Fatalf("run: %v", err)
	}
	var starts, ends int
	for _, ev := range log.Events() {
		if ev.Type == session.EventLLMRequestStart {
			starts++
			if !strings.Contains(string(ev.Data), "deepseek-official") || !strings.Contains(string(ev.Data), "deepseek-v4-flash") {
				t.Fatalf("request_start data = %s", ev.Data)
			}
		}
		if ev.Type == session.EventLLMRequestEnd {
			ends++
			if !strings.Contains(string(ev.Data), "completed") {
				t.Fatalf("request_end data = %s", ev.Data)
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("llm request events = start:%d end:%d, want 1/1", starts, ends)
	}
	if len(model.calls) != 1 || model.calls[0].Provider != "deepseek-official" {
		t.Fatalf("request provider = %+v", model.calls)
	}
}

func TestRunToolCallExecutesAndLogs(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model asks for get_time
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "get_time", Arguments: "{}"},
			}},
		},
		{ // step 2: model answers
			{Kind: llm.StreamTextDelta, Text: "It is now."},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	loop, log, _ := newTestLoop(t, model)

	if err := loop.Run(context.Background(), "what time is it"); err != nil {
		t.Fatalf("run: %v", err)
	}

	events := log.Events()
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := []string{
		session.EventTurnStart,
		session.EventStepStart,
		session.EventUserMessage,
		session.EventAssistantMessage,
		session.EventToolStart,
		session.EventToolResult,
		session.EventStepEnd,
		session.EventStepStart,
		session.EventAssistantChunk,
		session.EventAssistantMessage,
		session.EventStepEnd,
		session.EventTurnEnd,
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (all: %v)", i, types[i], want[i], types)
		}
	}

	// Tool result must reference the call id.
	var toolResult session.Event
	for _, candidate := range events {
		if candidate.Type == session.EventToolResult {
			toolResult = candidate
			break
		}
	}
	if toolResult.Type == "" || !strings.Contains(string(toolResult.Data), "call_1") {
		t.Fatalf("tool result data = %s", toolResult.Data)
	}

	// Two model requests: step 2 must include the tool result as a tool-role
	// message so the model sees it (D3).
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(model.calls))
	}
	last := model.calls[1].Messages
	foundTool := false
	for _, m := range last {
		if m.Role == llm.RoleTool && m.ToolCallID == "call_1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("step 2 messages lack tool result: %+v", last)
	}
}

type parallelState struct {
	current int32
	max     int32
}

type parallelTool struct {
	name  string
	state *parallelState
}

func (p parallelTool) Name() string        { return p.name }
func (p parallelTool) Description() string { return "test-only parallel read" }
func (p parallelTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (p parallelTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (p parallelTool) ConcurrencySafe(any) bool     { return true }
func (p parallelTool) Execute(ctx context.Context, args any) (string, error) {
	current := atomic.AddInt32(&p.state.current, 1)
	for {
		max := atomic.LoadInt32(&p.state.max)
		if current <= max || atomic.CompareAndSwapInt32(&p.state.max, max, current) {
			break
		}
	}
	defer atomic.AddInt32(&p.state.current, -1)
	select {
	case <-time.After(75 * time.Millisecond):
		return p.name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestRunParallelToolCalls verifies the dsh rolling pool: safe sibling calls
// overlap, while durable call/result rows remain in model order.
func TestRunParallelToolCalls(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			{ID: "parallel_a", Name: "parallel_a", Arguments: "{}"},
			{ID: "parallel_b", Name: "parallel_b", Arguments: "{}"},
		}}},
		{{Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	reg := tools.New()
	state := &parallelState{}
	for _, name := range []string{"parallel_a", "parallel_b"} {
		if err := reg.Register(parallelTool{name: name, state: state}); err != nil {
			t.Fatal(err)
		}
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{"parallel_a", "parallel_b"}, Timeout: time.Second, OutputLimit: tools.DefaultOutputLimit})
	log := session.New()
	loop := New(Config{
		LLM: model, Log: log, Tools: reg, Prompt: prompt.New("You are helpful."),
		Model: "deepseek-chat", MaxParallelToolCalls: 2,
	})
	if err := loop.Run(context.Background(), "run both reads"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&state.max); got < 2 {
		t.Fatalf("maximum concurrent calls = %d, want 2", got)
	}
	var ordered []string
	for _, ev := range log.Events() {
		if ev.Type == session.EventToolCall || ev.Type == session.EventToolResult {
			ordered = append(ordered, string(ev.Data))
		}
	}
	if len(ordered) != 4 || !strings.Contains(ordered[0], "parallel_a") || !strings.Contains(ordered[1], "parallel_b") || !strings.Contains(ordered[2], "parallel_a") || !strings.Contains(ordered[3], "parallel_b") {
		t.Fatalf("tool event order = %v, want call a, call b, result a, result b", ordered)
	}
}

func TestRunUnknownToolLogsErrorAndContinues(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model calls a tool that does not exist
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_x", Name: "nonexistent", Arguments: "{}"},
			}},
		},
		{ // step 2: model gives up
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	loop, log, _ := newTestLoop(t, model)

	if err := loop.Run(context.Background(), "call something"); err != nil {
		t.Fatalf("run: %v", err)
	}
	events := log.Events()
	types := []string{}
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	foundError := false
	for _, typ := range types {
		if typ == session.EventToolError {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Fatalf("expected tool/error, got %v", types)
	}
	for _, ev := range events {
		if ev.Type == session.EventToolResult && strings.Contains(string(ev.Data), "nonexistent") {
			if !strings.Contains(string(ev.Data), `"code":"UNKNOWN_TOOL"`) {
				t.Fatalf("unknown tool result lost dsh code: %s", ev.Data)
			}
			if !strings.Contains(string(ev.Data), `"sourceEventSeqs":[`) {
				t.Fatalf("unknown tool result lost source event linkage: %s", ev.Data)
			}
			return
		}
	}
	t.Fatal("unknown tool result was not durable")
}

// TestRunPreStepContextIsDurable verifies that pre-step context is carried
// into the tool-result follow-up request.
func TestRunPreStepContextIsDurable(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model asks for get_time
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "get_time", Arguments: "{}"},
			}},
		},
		{ // step 2: model answers
			{Kind: llm.StreamTextDelta, Text: "It is now."},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	log := session.New()
	var injectCalls int
	loop := New(Config{
		LLM:    model,
		Log:    log,
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{{Name: "context", Inject: func(ctx context.Context, userText string) []llm.Message {
			injectCalls++
			if userText != "what time is it" {
				t.Errorf("injector received userText %q, want %q", userText, "what time is it")
			}
			return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("durable context")}}}
		}}},
	})

	if err := loop.Run(context.Background(), "what time is it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if injectCalls != 2 {
		t.Fatalf("pre-step injector called %d times, want 2 (once per step)", injectCalls)
	}
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(model.calls))
	}
	first := model.calls[0].Messages
	if len(first) < 3 || first[0].Role != llm.RoleSystem || first[1].Role != llm.RoleUser || first[1].Text() != "what time is it" || first[2].Text() != "durable context" {
		t.Fatalf("first request messages = %+v, want system + user + durable context", first)
	}
	found := false
	for _, m := range model.calls[1].Messages {
		if strings.Contains(m.Text(), "durable context") {
			found = true
		}
	}
	if !found {
		t.Fatalf("second request must carry the durable context: %+v", model.calls[1].Messages)
	}
}

// newTestRegistry is a helper returning a registry with get_time registered
// (used by pre-step tests).
func newTestRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatalf("register get_time: %v", err)
	}
	return reg
}

// TestRunPreStepNilKeepsTurnFlow verifies a nil injector result changes
// nothing: the turn runs with a plain request.
func TestRunPreStepNilKeepsTurnFlow(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, _, _ := newTestLoop(t, model)
	loop.preStep = []PreStepInjector{{Name: "empty", Inject: func(ctx context.Context, userText string) []llm.Message { return nil }}}

	if err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 2 || msgs[0].Role != llm.RoleSystem || msgs[1].Role != llm.RoleUser {
		t.Fatalf("request messages = %+v, want system + user only", msgs)
	}
}

// TestRunCancelContext verifies a cancelled context aborts before any step.
func TestRunCancelContext(t *testing.T) {
	model := &scriptedLLM{}
	loop, _, _ := newTestLoop(t, model)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before any step
	if err := loop.Run(ctx, "nope"); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestRunAllowsMoreThanTenSteps(t *testing.T) {
	// DSH does not impose a fixed ten-step turn cap. A model may continue
	// requesting tools until it returns a normal completion or is cancelled.
	var steps [][]llm.StreamEvent
	for i := 0; i < 11; i++ {
		steps = append(steps, []llm.StreamEvent{{
			Kind: llm.StreamFinish, FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "c", Name: "get_time", Arguments: "{}"}},
		}})
	}
	steps = append(steps, []llm.StreamEvent{{Kind: llm.StreamTextDelta, Text: "done"}, {Kind: llm.StreamFinish, FinishReason: "stop"}})
	model := &scriptedLLM{steps: steps}
	loop, _, _ := newTestLoop(t, model)

	err := loop.Run(context.Background(), "loop")
	if err != nil {
		t.Fatalf("run should continue past ten steps: %v", err)
	}
	if len(model.calls) != 12 {
		t.Fatalf("model calls = %d, want 12 (11 tool steps plus completion)", len(model.calls))
	}
}

// cancelReader blocks on Next until the context is done, standing in for a
// streaming HTTP body that honors ctx (the DeepSeek SSE reader aborts its read
// when the request context is cancelled).
type cancelReader struct{ ctx context.Context }

func (r *cancelReader) Next() (llm.StreamEvent, error) {
	<-r.ctx.Done()
	return llm.StreamEvent{}, r.ctx.Err()
}

type partialCancelReader struct {
	ctx  context.Context
	done bool
}

func (r *partialCancelReader) Next() (llm.StreamEvent, error) {
	if !r.done {
		r.done = true
		return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: "partial"}, nil
	}
	<-r.ctx.Done()
	return llm.StreamEvent{}, r.ctx.Err()
}

type cancelLLM struct{ ctx context.Context }

func (m *cancelLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return &cancelReader{ctx: m.ctx}, nil
}

type partialCancelLLM struct{ ctx context.Context }

func (m *partialCancelLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return &partialCancelReader{ctx: m.ctx}, nil
}

type steerContinuationLLM struct {
	calls atomic.Int32
}

func (m *steerContinuationLLM) Stream(ctx context.Context, _ llm.ChatRequest) (llm.StreamReader, error) {
	if m.calls.Add(1) == 1 {
		return &cancelReader{ctx: ctx}, nil
	}
	return &scriptedReader{events: []llm.StreamEvent{{Kind: llm.StreamTextDelta, Text: "continued"}, {Kind: llm.StreamFinish, FinishReason: "stop"}}}, nil
}

func TestRunSteeringCancellationContinuesSameDurableTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &steerContinuationLLM{}
	loop, log, _ := newTestLoop(t, model)
	continued := atomic.Bool{}
	loop.SetContinueOnCancel(func(context.Context) ([]llm.Message, bool, error) {
		if continued.Swap(true) {
			return nil, false, nil
		}
		return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("change direction")}}}, true, nil
	})
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := loop.Run(ctx, "initial"); err != nil {
		t.Fatalf("steering continuation: %v", err)
	}
	starts, ends, steps := 0, 0, 0
	var sawSteering bool
	for _, event := range log.Events() {
		switch event.Type {
		case session.EventTurnStart:
			starts++
		case session.EventTurnEnd:
			ends++
		case session.EventStepStart:
			steps++
		case session.EventUserMessage:
			if strings.Contains(string(event.Data), "change direction") {
				sawSteering = true
			}
		}
	}
	if starts != 1 || ends != 1 || steps != 2 || !sawSteering {
		t.Fatalf("steering lifecycle = turns %d/%d steps %d steering %v events=%+v", starts, ends, steps, sawSteering, log.Events())
	}
}

// TestRunCancelDuringStream covers Ctrl+C during streaming (dispatch-m3: 流式
// 中断): the loop returns promptly with a cancellation error.
func TestRunCancelDuringStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	loop, _, _ := newTestLoop(t, &cancelLLM{ctx: ctx})
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := loop.Run(ctx, "go")
	if err == nil {
		t.Fatal("expected cancellation error during stream")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunCancelDuringStreamPersistsInterruptedAssistant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	loop, log, _ := newTestLoop(t, &partialCancelLLM{ctx: ctx})
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := loop.Run(ctx, "go")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	var found bool
	for _, ev := range log.Events() {
		if ev.Type != session.EventAssistantMessage {
			continue
		}
		var payload struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Interrupted bool `json:"interrupted"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		var text string
		if len(payload.Message.Content) > 0 {
			text = payload.Message.Content[0].Text
		}
		if text == "partial" && payload.Interrupted {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing interrupted assistant anchor: %+v", log.Events())
	}
	if got := log.DeriveHistory(); len(got) != 2 || got[1].Text() != "partial" {
		t.Fatalf("history = %+v, want user + partial assistant", got)
	}
}

type cancellingTool struct {
	name   string
	cancel context.CancelFunc
}

func (t cancellingTool) Name() string        { return t.name }
func (t cancellingTool) Description() string { return "cancels the turn when dispatched" }
func (t cancellingTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t cancellingTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t cancellingTool) Execute(ctx context.Context, args any) (string, error) {
	t.cancel()
	return "done", nil
}

func TestRunCancelLogsUndispatchedToolCalls(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "cancel_tool", Arguments: "{}"},
			{ID: "call_2", Name: "never_dispatched", Arguments: "{}"},
		}},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	loop, log, reg := newTestLoop(t, model)
	if err := reg.Register(cancellingTool{name: "cancel_tool", cancel: cancel}); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{
		Enabled:     []string{"get_time", "cancel_tool"},
		Timeout:     time.Second,
		OutputLimit: tools.DefaultOutputLimit,
	})
	err := loop.Run(ctx, "cancel after first tool")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	var found bool
	for _, ev := range log.Events() {
		if ev.Type != session.EventToolResult || !strings.Contains(string(ev.Data), "ABORTED_BEFORE_DISPATCH") {
			continue
		}
		if strings.Contains(string(ev.Data), "call_2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing aborted result for undispatched call: %+v", log.Events())
	}
}

// blockingTool is a slow tool killed by the Execute pipeline's deadline.
type blockingTool struct{ name string }

func (b blockingTool) Name() string        { return b.name }
func (b blockingTool) Description() string { return "blocks until the context is done" }
func (b blockingTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (b blockingTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (b blockingTool) Execute(ctx context.Context, args any) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestRunToolTimeoutLogsToolError verifies a tool that blows its deadline is
// interrupted by the Execute pipeline and the timeout lands as a tool/error
// event (dispatch-m3: sleep 工具被掐断并落 tool/error, D3).
func TestRunToolTimeoutLogsToolError(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model calls the slow tool
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_t", Name: "sleep_tool", Arguments: "{}"},
			}},
		},
		{ // step 2: model answers
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	loop, log, reg := newTestLoop(t, model)
	reg.Register(blockingTool{name: "sleep_tool"})
	reg.SetPolicy(tools.Policy{
		Enabled:     []string{"get_time", "sleep_tool"},
		Timeout:     100 * time.Millisecond,
		OutputLimit: tools.DefaultOutputLimit,
	})

	if err := loop.Run(context.Background(), "run it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantTypes := []string{
		session.EventTurnStart,
		session.EventStepStart,
		session.EventUserMessage,
		session.EventAssistantMessage,
		session.EventToolCall,
		session.EventToolResult,
		session.EventStepEnd,
		session.EventStepStart,
		session.EventAssistantMessage,
		session.EventStepEnd,
		session.EventTurnEnd,
	}
	events := log.Events()
	gotTypes := make([]string, 0, len(events))
	for _, event := range events {
		if event.Type == session.EventAssistantChunk {
			continue
		}
		gotTypes = append(gotTypes, event.Type)
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("timeout waterfall event types = %v, want %v", gotTypes, wantTypes)
	}
	var found bool
	for _, ev := range log.Events() {
		if ev.Type == session.EventToolError && strings.Contains(string(ev.Data), "timed out") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a tool/error with a timeout message; events: %+v", log.Events())
	}
	for _, ev := range log.Events() {
		if ev.Type == session.EventToolResult && strings.Contains(string(ev.Data), "sleep_tool") && strings.Contains(string(ev.Data), "timed out") {
			if !strings.Contains(string(ev.Data), `"code":"TOOL_TIMEOUT"`) {
				t.Fatalf("timeout result lost dsh code: %s", ev.Data)
			}
			if !strings.Contains(string(ev.Data), `"sourceEventSeqs":[`) {
				t.Fatalf("timeout result lost source event linkage: %s", ev.Data)
			}
		}
	}

	// A timeout is durable state, not just a live diagnostic: cold replay must
	// preserve the same model-visible call/result pairing and assistant tail.
	restored := session.New()
	if err := restored.Restore(log.Events()); err != nil {
		t.Fatalf("restore timeout log: %v", err)
	}
	if !reflect.DeepEqual(log.DeriveHistory(), restored.DeriveHistory()) {
		t.Fatal("replayed timeout history diverged")
	}
}

// TestApprovalDenialPreservesCanonicalWaterfallAndReplay matches the
// reference pre-execute permission pattern end to end. The denied call still
// owns a durable tool/call and model-visible tool/result before the next step;
// restoring the committed events must produce the same model history.
func TestApprovalDenialPreservesCanonicalWaterfallAndReplay(t *testing.T) {
	denied := &deniedTool{}
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{
			Kind: llm.StreamFinish, FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "call-denied", Name: "danger", Arguments: `{}`}},
		}},
		{{Kind: llm.StreamTextDelta, Text: "recover after denial"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	loop, log, reg := newTestLoop(t, model)
	if err := reg.Register(denied); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{
		Profile: "standard", Enabled: []string{"get_time", "danger"},
		Timeout: tools.DefaultTimeout, OutputLimit: tools.DefaultOutputLimit,
	})
	reg.AddPreExecuteHook(func(_ context.Context, execution tools.Execution) (tools.PreToolDecision, error) {
		if execution.Name == "danger" {
			return tools.PreToolDecision{Kind: "deny", Reason: "blocked dangerous tool"}, nil
		}
		return tools.PreToolDecision{Kind: "allow"}, nil
	})

	if err := loop.Run(context.Background(), "use danger"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if denied.runs.Load() != 0 {
		t.Fatal("denied tool executed")
	}

	events := log.Events()
	wantTypes := []string{
		session.EventTurnStart,
		session.EventStepStart,
		session.EventUserMessage,
		session.EventAssistantMessage,
		session.EventToolCall,
		session.EventToolResult,
		session.EventStepEnd,
		session.EventStepStart,
		session.EventAssistantMessage,
		session.EventStepEnd,
		session.EventTurnEnd,
	}
	gotTypes := make([]string, 0, len(events))
	for _, event := range events {
		// Streaming adapters may retain chunk fidelity rows. They are ignored by
		// DeriveHistory and are not part of the branch's lifecycle contract.
		if event.Type == session.EventAssistantChunk {
			continue
		}
		gotTypes = append(gotTypes, event.Type)
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("waterfall event types = %v, want %v", gotTypes, wantTypes)
	}

	var call struct {
		CallID string `json:"callId"`
	}
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		SourceEventSeqs []uint64 `json:"sourceEventSeqs"`
	}
	for _, event := range events {
		switch event.Type {
		case session.EventToolCall:
			if err := json.Unmarshal(event.Data, &call); err != nil {
				t.Fatalf("decode tool/call: %v", err)
			}
		case session.EventToolResult:
			if err := json.Unmarshal(event.Data, &result); err != nil {
				t.Fatalf("decode tool/result: %v", err)
			}
		}
	}
	if call.CallID != "call-denied" {
		t.Fatalf("denied call = %q", call.CallID)
	}
	if result.Error.Code != tools.CodeToolDenied {
		t.Fatalf("denial result code = %q, want %q", result.Error.Code, tools.CodeToolDenied)
	}
	if len(result.SourceEventSeqs) != 1 || result.SourceEventSeqs[0] == 0 {
		t.Fatalf("denial result source linkage = %v, want one call seq", result.SourceEventSeqs)
	}

	restored := session.New()
	if err := restored.Restore(events); err != nil {
		t.Fatalf("restore canonical denial log: %v", err)
	}
	originalHistory := log.DeriveHistory()
	replayedHistory := restored.DeriveHistory()
	if !reflect.DeepEqual(originalHistory, replayedHistory) {
		t.Fatalf("replayed denial history diverged:\noriginal=%+v\nreplay=%+v", originalHistory, replayedHistory)
	}
}

type deniedTool struct{ runs atomic.Int32 }

func (d *deniedTool) Name() string        { return "danger" }
func (d *deniedTool) Description() string { return "sensitive tool" }
func (d *deniedTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (d *deniedTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (d *deniedTool) Execute(context.Context, any) (string, error) {
	d.runs.Add(1)
	return "must not run", nil
}

// bigResultTool returns a payload larger than any test output cap.
type bigResultTool struct{ text string }

func (b bigResultTool) Name() string        { return "big_tool" }
func (b bigResultTool) Description() string { return "returns a lot of text" }
func (b bigResultTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (b bigResultTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (b bigResultTool) Execute(ctx context.Context, args any) (string, error) {
	return b.text, nil
}

// TestRunSpilledResultLogsLocator verifies an oversized tool result is
// truncated and spilled, and the tool/result event records the locator (which
// the model reads the full output through, D3).
func TestRunSpilledResultLogsLocator(t *testing.T) {
	spillDir := t.TempDir()
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model calls the big-output tool
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_s", Name: "big_tool", Arguments: "{}"},
			}},
		},
		{ // step 2: model answers
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	loop, log, reg := newTestLoop(t, model)
	reg.Register(bigResultTool{text: strings.Repeat("x", 4096)})
	reg.SetPolicy(tools.Policy{
		Enabled:     []string{"get_time", "big_tool"},
		Timeout:     time.Hour,
		OutputLimit: 64,
		SpillDir:    spillDir,
	})
	reg.SetOwner(tools.Owner{SessionID: "s-loop", NextSeq: func() uint64 { return log.NextSeq() }})

	if err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	var found bool
	for _, ev := range log.Events() {
		if ev.Type != session.EventToolResult {
			continue
		}
		var d struct {
			Output string            `json:"output"`
			Spill  *session.SpillRef `json:"spill"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if d.Spill != nil && d.Spill.Locator != "" && strings.Contains(d.Output, d.Spill.Locator) {
			if _, err := os.Stat(d.Spill.Locator); err != nil {
				t.Fatalf("spill file %s: %v", d.Spill.Locator, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a spilled tool/result with a locator; events: %+v", log.Events())
	}
}

func TestContextOverflowRecognizesStructuredFailureCode(t *testing.T) {
	if !isContextOverflowError(llm.NewFailureError("request rejected", "CONTEXT_WINDOW_EXCEEDED", nil)) {
		t.Fatal("structured context-window failure was not recognized")
	}
}
