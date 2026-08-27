package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
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
		session.EventUserMessage,
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
	var chunk struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(events[3].Data, &chunk); err != nil || chunk.Text != "Hi there" {
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
		LLM:             model,
		Log:             log,
		Tools:           reg,
		Prompt:          prompt.New("You are helpful."),
		Model:           "deepseek-v4-flash",
		ReasoningEffort: "max",
	})
	if err := loop.Run(context.Background(), "think"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 1 || model.calls[0].ReasoningEffort != "max" {
		t.Fatalf("calls = %+v, want one with ReasoningEffort=max", model.calls)
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
		session.EventUserMessage,
		session.EventStepStart,
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

func TestRunMaxSteps(t *testing.T) {
	// A model that never stops calling tools must hit the step cap.
	var steps [][]llm.StreamEvent
	for i := 0; i < maxSteps+1; i++ {
		steps = append(steps, []llm.StreamEvent{{
			Kind: llm.StreamFinish, FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "c", Name: "get_time", Arguments: "{}"}},
		}})
	}
	model := &scriptedLLM{steps: steps}
	loop, _, _ := newTestLoop(t, model)

	err := loop.Run(context.Background(), "loop")
	if err == nil {
		t.Fatal("expected max-steps error")
	}
	if !strings.Contains(err.Error(), "steps") {
		t.Fatalf("error = %v", err)
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
			Text        string `json:"text"`
			Interrupted bool   `json:"interrupted"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Text == "partial" && payload.Interrupted {
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
			return
		}
	}
	t.Fatal("timeout result was not durable")
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
