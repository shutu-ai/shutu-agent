package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

// scriptedLLM returns a fixed per-step sequence of stream events (one Stream
// call per step), then EOF — the subagent-test counterpart of the loop's
// scriptedLLM.
type scriptedLLM struct {
	mu    sync.Mutex
	steps [][]llm.StreamEvent
	calls []llm.ChatRequest
}

func (s *scriptedLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedLLM) call(index int) llm.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[index]
}

type captureMaxTokensLLM struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (m *captureMaxTokensLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return &scriptedReader{events: []llm.StreamEvent{{Kind: llm.StreamFinish, FinishReason: "stop"}}}, nil
}

func (s *scriptedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if len(s.steps) == 0 {
		return &scriptedReader{}, nil
	}
	events := s.steps[0]
	s.steps = s.steps[1:]
	s.mu.Unlock()
	defer s.mu.Lock()
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

// blockingLLM returns a reader that blocks on Next until ctx is done, standing
// in for a live child that honors cancellation.
type blockingLLM struct {
	started chan struct{}
	once    sync.Once
}

func (m *blockingLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	m.once.Do(func() { close(m.started) })
	return &blockingReader{ctx: ctx}, nil
}

type blockingReader struct{ ctx context.Context }

func (r *blockingReader) Next() (llm.StreamEvent, error) {
	<-r.ctx.Done()
	return llm.StreamEvent{}, r.ctx.Err()
}

// mixedLLM answers the first Stream immediately and blocks on the second
// (honoring ctx), letting one provider own one completed + one live child.
type mixedLLM struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	first   []llm.StreamEvent
}

func (m *mixedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		return &scriptedReader{events: m.first}, nil
	}
	close(m.started)
	return &blockingReader{ctx: ctx}, nil
}

// errorLLM fails its Stream immediately, standing in for a model/transport
// failure during the child run.
type errorLLM struct{ err error }

func (m *errorLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return nil, m.err
}

type failingSessionCreateStore struct {
	store.Store
	err error
}

func (s failingSessionCreateStore) CreateSessionWithEvents(context.Context, string, time.Time, store.SessionHeader, []session.Event) error {
	return s.err
}

type failingSessionCreateOptionsStore struct {
	store.SessionCreateStore
	store.Store
	err error
}

func (s failingSessionCreateOptionsStore) CreateSessionWithOptions(context.Context, string, time.Time, store.SessionCreateOptions, []session.Event) error {
	return s.err
}

type blockingSessionCreateStore struct {
	store.Store
	started chan struct{}
}

func (s blockingSessionCreateStore) CreateSessionWithEvents(ctx context.Context, _ string, _ time.Time, _ store.SessionHeader, _ []session.Event) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

type filteredTool struct{ name string }

func (f filteredTool) Name() string                                 { return f.name }
func (f filteredTool) Description() string                          { return f.name }
func (f filteredTool) Schema() map[string]any                       { return map[string]any{"type": "object"} }
func (f filteredTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (f filteredTool) Execute(context.Context, any) (string, error) { return f.name, nil }

// TestSpawnFullRound runs one complete child-agent round with a fake LLM and
// verifies the child owns an independent, replayable session (the parent log is
// never polluted), the terminal result is returned, and ListChildren reflects
// the settled child under its parent.
func TestSpawnFullRound(t *testing.T) {
	parentLog := session.New()
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child "},
		{Kind: llm.StreamTextDelta, Text: "answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewSpawnProvider(Deps{
		Log:    parentLog,
		LLM:    model,
		Tools:  tools.New(),
		Prompt: prompt.New("You are a subagent."),
		Model:  "deepseek-chat",
	})
	ctx := context.Background()

	run, err := prov.Start(ctx, StartRequest{
		Label: "researcher", Prompt: "summarize the docs", ParentSessionID: "parent-1",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if run.ID == "" {
		t.Fatal("run id must be non-empty")
	}
	res, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Output != "child answer" {
		t.Fatalf("output = %q, want the child's last non-empty assistant text", res.Output)
	}
	if res.StopReason != StopCompleted {
		t.Fatalf("stopReason = %q, want %q", res.StopReason, StopCompleted)
	}

	// The parent session log is never polluted: the child owns an independent
	// session (ADR 残余风险: 子代理不串扰父会话日志).
	if n := len(parentLog.Events()); n != 0 {
		t.Fatalf("parent log has %d events, want 0 (child must be independent)", n)
	}

	// The child session is complete and replayable: user/message first, the
	// assistant/message answer last, and DeriveHistory reproduces the turn.
	childLog, ok := prov.ChildLog(run.ID)
	if !ok {
		t.Fatalf("child log for %s not found", run.ID)
	}
	events := childLog.Events()
	if len(events) != 7 {
		t.Fatalf("child events = %d, want 7 (turn/step lifecycle + user, aggregated chunk, assistant)", len(events))
	}
	if events[2].Type != session.EventUserMessage {
		t.Fatalf("child user event = %q, want user/message", events[2].Type)
	}
	if events[4].Type != session.EventAssistantMessage {
		t.Fatalf("assistant child event = %q, want assistant/message", events[4].Type)
	}
	hist := childLog.DeriveHistory()
	if len(hist) != 2 || hist[0].Role != llm.RoleUser || hist[0].Text() != "summarize the docs" ||
		hist[1].Role != llm.RoleAssistant || hist[1].Text() != "child answer" {
		t.Fatalf("derived child history = %+v, want user prompt + assistant answer", hist)
	}
	if len(model.calls) != 1 {
		t.Fatalf("child llm calls = %d, want 1", len(model.calls))
	}

	// ListChildren: the settled child is listed under its parent, not under a
	// different parent, and no longer running.
	children, err := prov.ListChildren(ctx, "parent-1")
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 1 || children[0].ID != run.ID || children[0].Label != "researcher" || children[0].Running {
		t.Fatalf("children = %+v, want one settled child %s", children, run.ID)
	}
	if other, _ := prov.ListChildren(ctx, "other-parent"); len(other) != 0 {
		t.Fatalf("children under a different parent = %+v, want none", other)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSpawnMaxTokensInheritsAndAllowsPerChildOverride(t *testing.T) {
	model := &captureMaxTokensLLM{}
	prov := NewSpawnProvider(Deps{
		LLM: model, Tools: tools.New(), Prompt: prompt.New("child"), Model: "m", MaxTokens: 111,
	})

	parent, err := prov.Start(context.Background(), StartRequest{Prompt: "parent", MaxTokens: 111})
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	if _, err := parent.Result(context.Background()); err != nil {
		t.Fatalf("parent result: %v", err)
	}
	inherited, err := prov.Start(context.Background(), StartRequest{Prompt: "inherited", ParentSessionID: parent.ID})
	if err != nil {
		t.Fatalf("start inherited child: %v", err)
	}
	overridden, err := prov.Start(context.Background(), StartRequest{Prompt: "overridden", ParentSessionID: parent.ID, MaxTokens: 222})
	if err != nil {
		t.Fatalf("start overridden child: %v", err)
	}
	for _, run := range []*Run{inherited, overridden} {
		if _, err := run.Result(context.Background()); err != nil {
			t.Fatalf("child result: %v", err)
		}
	}

	model.mu.Lock()
	defer model.mu.Unlock()
	seen := map[int]int{}
	for _, request := range model.requests {
		seen[request.MaxTokens]++
	}
	if seen[111] != 2 || seen[222] != 1 {
		t.Fatalf("max token requests = %#v, want inherited 111 twice and override 222 once", seen)
	}
}

func TestSpawnMaxTokensInheritsFromHostOwnedParent(t *testing.T) {
	model := &captureMaxTokensLLM{}
	prov := NewSpawnProvider(Deps{
		LLM: model, Tools: tools.New(), Prompt: prompt.New("child"), Model: "m", MaxTokens: 111,
		MaxTokensFor: func(_ context.Context, parent string) int {
			if parent == "sdk-parent" {
				return 333
			}
			return 0
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "host child", ParentSessionID: "sdk-parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) != 1 || model.requests[0].MaxTokens != 333 {
		t.Fatalf("host-parent max tokens = %+v, want 333", model.requests)
	}
}

func TestSpawnUsesParentSessionProviderAndModelResolvers(t *testing.T) {
	global := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	selected := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamTextDelta, Text: "session"}, {Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	var boundID string
	var boundLog *session.Log
	prov := NewSpawnProvider(Deps{
		LLM: global, Model: "global-model", Tools: tools.New(), Prompt: prompt.New("child"),
		BindSessionLog: func(id string, log *session.Log) { boundID, boundLog = id, log },
		LLMFor: func(_ context.Context, parent string) llm.LLM {
			if parent == "parent-session" {
				return selected
			}
			return nil
		},
		ModelFor: func(_ context.Context, parent string) string {
			if parent == "parent-session" {
				return "session-model"
			}
			return ""
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "use selected runtime", ParentSessionID: "parent-session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Result(context.Background())
	if err != nil || result.Output != "session" {
		t.Fatalf("result = %+v, err=%v", result, err)
	}
	if boundID != run.ID || boundLog == nil {
		t.Fatalf("child log binding = (%q, %p), want (%q, non-nil)", boundID, boundLog, run.ID)
	}
	if len(global.calls) != 0 || len(selected.calls) != 1 {
		t.Fatalf("provider resolver calls: global=%d selected=%d", len(global.calls), len(selected.calls))
	}
	if selected.calls[0].Model != "session-model" {
		t.Fatalf("child model = %q, want session-model", selected.calls[0].Model)
	}
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestForkSeedsResolvedLiveParentCompletedPrefix(t *testing.T) {
	parentLog := session.New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := parentLog.Append(typ, data); err != nil {
			t.Fatalf("append parent %s: %v", typ, err)
		}
	}
	appendEvent(session.EventTurnStart, session.NewTurnStartAt(1))
	appendEvent(session.EventStepStart, session.NewStepStartAt(1, 1))
	appendEvent(session.EventUserMessage, session.NewUserMessageAt(1, 1, 0, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("parent context")},
	}))
	appendEvent(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "parent answer", nil, "stop", "", llm.TokenUsage{}))
	appendEvent(session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", ""))
	appendEvent(session.EventTurnEnd, session.NewTurnEndAt(1, "completed", ""))

	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewForkProvider(Deps{
		LLM: model, Tools: tools.New(), Prompt: prompt.New("child"), Model: "m",
		ParentLogFor: func(_ context.Context, id string) *session.Log {
			if id == "live-parent" {
				return parentLog
			}
			return nil
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{
		Prompt: "continue from parent", ParentSessionID: "live-parent", InheritParentContext: true,
	})
	if err != nil {
		t.Fatalf("start fork: %v", err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatalf("fork result: %v", err)
	}
	child, ok := prov.ChildLog(run.ID)
	if !ok {
		t.Fatalf("missing child log %q", run.ID)
	}
	events := child.Events()
	turnEnds := 0
	for _, event := range events {
		if event.Type == session.EventTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 2 {
		t.Fatalf("turn ends = %d, want seeded parent turn plus child turn", turnEnds)
	}
	history := child.DeriveHistory()
	if len(history) != 4 || history[0].Text() != "parent context" || history[1].Text() != "parent answer" || history[2].Text() != "continue from parent" || history[3].Text() != "child answer" {
		t.Fatalf("fork history = %+v, want completed parent prefix followed by child turn", history)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSpawnUsesParentSessionToolAndPromptResolvers(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	global := tools.New()
	if err := global.Register(filteredTool{name: "global-only"}); err != nil {
		t.Fatal(err)
	}
	if err := global.Register(filteredTool{name: "session-tool"}); err != nil {
		t.Fatal(err)
	}
	scoped := global.Clone()
	scoped.SetPolicy(tools.Policy{Enabled: []string{"session-tool"}})
	prov := NewSpawnProvider(Deps{
		LLM: model, Tools: global, Prompt: prompt.New("global prompt"), Model: "m",
		ToolsFor: func(_ context.Context, parent string) *tools.Registry {
			if parent == "parent-session" {
				return scoped
			}
			return nil
		},
		PromptFor: func(_ context.Context, parent string) *prompt.Builder {
			if parent == "parent-session" {
				return prompt.New("session prompt")
			}
			return nil
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "scoped child", ParentSessionID: "parent-session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(model.calls) != 1 || len(model.calls[0].Tools) != 1 || model.calls[0].Tools[0].Name != "session-tool" {
		t.Fatalf("child tool surface = %+v", model.calls)
	}
	var system string
	for _, message := range model.calls[0].Messages {
		if message.Role == llm.RoleSystem {
			system = message.Text()
		}
	}
	if !strings.Contains(system, "session prompt") || strings.Contains(system, "global prompt") {
		t.Fatalf("child prompt = %q", system)
	}
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnAppliesPersonaAndToolFilterToChildScope(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	registry := tools.New()
	if err := registry.Register(filteredTool{name: "visible"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(filteredTool{name: "hidden"}); err != nil {
		t.Fatal(err)
	}
	prov := NewSpawnProvider(Deps{LLM: model, Tools: registry, Prompt: prompt.New("base persona"), Model: "m"})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "do", Persona: "child persona", ToolFilter: []string{"visible"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d", len(model.calls))
	}
	if len(model.calls[0].Tools) != 1 || model.calls[0].Tools[0].Name != "visible" {
		t.Fatalf("child tools = %+v", model.calls[0].Tools)
	}
	var system string
	for _, message := range model.calls[0].Messages {
		if message.Role == llm.RoleSystem {
			system = message.Text()
		}
	}
	if !strings.Contains(system, "child persona") || strings.Contains(system, "base persona") {
		t.Fatalf("child persona = %q", system)
	}
}

func TestSpawnStructuredOutputUsesScopedToolAndReturnsValue(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: structuredOutputToolName, Arguments: `{"answer":"ok","score":3}`}}}},
		{{Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	parentTools := tools.New()
	prov := NewSpawnProvider(Deps{LLM: model, Tools: parentTools, Prompt: prompt.New("child"), Model: "m"})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
			"score":  map[string]any{"type": "integer"},
		},
		"required":             []string{"answer", "score"},
		"additionalProperties": false,
	}
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "return a report", OutputSchema: schema})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	result, err := run.Result(context.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	value, ok := result.Structured.(map[string]any)
	if !ok || value["answer"] != "ok" || value["score"] != float64(3) {
		t.Fatalf("structured result = %#v, want validated object", result.Structured)
	}
	if result.StopReason != StopCompleted {
		t.Fatalf("stop reason = %q, want completed", result.StopReason)
	}
	if len(model.calls) < 1 {
		t.Fatal("model was not called")
	}
	found := false
	for _, spec := range model.calls[0].Tools {
		if spec.Name == structuredOutputToolName {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("first child request did not expose scoped structured_output tool")
	}
	if len(parentTools.Specs()) != 0 {
		t.Fatalf("parent registry specs = %+v, structured tool leaked out of child scope", parentTools.Specs())
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnContinuableSend verifies the process-local dsh continuation shape:
// a live continuable child accepts a follow-up message and appends another
// complete turn to the same independent session.
func TestSpawnContinuableSend(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamTextDelta, Text: "first"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
		{{Kind: llm.StreamTextDelta, Text: "second"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	prov := NewSpawnProvider(Deps{LLM: model, Tools: tools.New(), Prompt: prompt.New("x"), Model: "m"})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "first task", ParentSessionID: "p", Continuable: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for model.callCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("initial turn calls = %d, want 1", got)
	}
	if err := run.SendQuiet(context.Background(), "background context"); err != nil {
		t.Fatalf("quiet send: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := model.callCount(); got != 1 {
		t.Fatalf("quiet send woke child: calls = %d, want 1", got)
	}
	if err := run.Send(context.Background(), "follow up"); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for model.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := run.Cancel("done"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := model.callCount(); got < 2 {
		t.Fatalf("follow-up calls = %d, want 2", got)
	}
	call := model.call(1)
	last := call.Messages[len(call.Messages)-1].Text()
	if !strings.Contains(last, "background context") || !strings.Contains(last, "follow up") {
		t.Fatalf("waking turn did not receive quiet context and follow-up: %q", last)
	}
	res, err := run.Result(context.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.StopReason != StopAborted {
		t.Fatalf("stop reason = %q, want %q", res.StopReason, StopAborted)
	}
	log, ok := prov.ChildLog(run.ID)
	if !ok {
		t.Fatal("child log missing")
	}
	if got := len(log.DeriveHistory()); got != 4 {
		t.Fatalf("derived messages = %d, want 4", got)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestContinuableChildGetsOnlyScopedReportTool(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "report-1", Name: "report", Arguments: `{"output":"finding"}`}}}},
		{{Kind: llm.StreamTextDelta, Text: "finished"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	parentTools := tools.New()
	reports := make(chan string, 1)
	prov := NewSpawnProvider(Deps{
		LLM: model, Tools: parentTools, Prompt: prompt.New("x"), Model: "m",
		Report: func(childID, parentID, output string) (string, error) {
			reports <- childID + ":" + parentID + ":" + output
			return "report-message-1", nil
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "investigate", ParentSessionID: "parent", Continuable: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case got := <-reports:
		if got != run.ID+":parent:finding" {
			t.Fatalf("report delivery = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("continuable child did not execute its scoped report tool")
	}
	if _, err := parentTools.Execute(context.Background(), "report", json.RawMessage(`{"output":"leak"}`)); err == nil {
		t.Fatal("report leaked into the parent registry")
	}
	if err := run.Cancel("test complete"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnPersistsChildLog verifies that child events use the existing Store
// seam and can be replayed independently of the in-memory provider registry.
func TestSpawnPersistsChildLog(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "persisted"}, {Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewSpawnProvider(Deps{LLM: model, Tools: tools.New(), Prompt: prompt.New("x"), Model: "m", Store: st})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "persist me", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatalf("result: %v", err)
	}
	events, err := st.LoadSession(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("load child session: %v", err)
	}
	if len(events) == 0 || events[len(events)-1].Type != session.EventTurnEnd {
		t.Fatalf("persisted child events = %d, last=%q; want replayable turn end", len(events), events[len(events)-1].Type)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh provider instance can recover the child lineage and append a new
	// turn to the same durable session.
	resumedModel := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "resumed"}, {Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	resumed := NewSpawnProvider(Deps{LLM: resumedModel, Tools: tools.New(), Prompt: prompt.New("x"), Model: "m", Store: st})
	resumedRun, err := resumed.Resume(context.Background(), run.ID, "continue after restart", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	result, err := resumedRun.Result(context.Background())
	if err != nil || result.Output != "resumed" {
		t.Fatalf("resumed result = %+v, err=%v", result, err)
	}
	replayed, err := st.LoadSession(context.Background(), run.ID)
	if err != nil || len(replayed) <= len(events) {
		t.Fatalf("replayed events after resume = %d, err=%v; want appended turn", len(replayed), err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatalf("close resumed provider: %v", err)
	}

	// A new provider instance must continue the durable id sequence instead of
	// attempting to recreate spawn-1 after a process restart.
	coldStartModel := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "cold-start child"}, {Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	coldStart := NewSpawnProvider(Deps{LLM: coldStartModel, Tools: tools.New(), Prompt: prompt.New("x"), Model: "m", Store: st})
	newRun, err := coldStart.Start(context.Background(), StartRequest{Prompt: "new after restart", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("cold-start child: %v", err)
	}
	if newRun.ID == run.ID {
		t.Fatalf("cold-start child reused durable id %q", newRun.ID)
	}
	if _, err := newRun.Result(context.Background()); err != nil {
		t.Fatalf("cold-start child result: %v", err)
	}
	if err := coldStart.Close(); err != nil {
		t.Fatalf("close cold-start provider: %v", err)
	}
}

func TestForkPersistsSeedAndLineageAtomically(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	parent := session.New()
	appendParent := func(typ string, data any) {
		t.Helper()
		if _, err := parent.Append(typ, data); err != nil {
			t.Fatalf("append parent %s: %v", typ, err)
		}
	}
	appendParent(session.EventTurnStart, session.NewTurnStartAt(1))
	appendParent(session.EventStepStart, session.NewStepStartAt(1, 1))
	appendParent(session.EventUserMessage, session.NewUserMessageAt(1, 1, 0, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("seed me")},
	}))
	appendParent(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "seed answer", nil, "stop", "", llm.TokenUsage{}))
	appendParent(session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", ""))
	appendParent(session.EventTurnEnd, session.NewTurnEndAt(1, "completed", ""))

	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "fork answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewForkProvider(Deps{
		LLM: model, Tools: tools.New(), Prompt: prompt.New("child"), Model: "m", Store: st,
		ParentLogFor: func(_ context.Context, id string) *session.Log {
			if id == "parent" {
				return parent
			}
			return nil
		},
	})
	run, err := prov.Start(context.Background(), StartRequest{
		Prompt: "fork from seed", ParentSessionID: "parent", InheritParentContext: true,
	})
	if err != nil {
		t.Fatalf("start fork: %v", err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatalf("fork result: %v", err)
	}
	header, err := st.GetSessionHeader(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get child header: %v", err)
	}
	if header.Parent != "parent" || header.Origin != "subagent" || header.SeedLength != len(parent.Events()) || header.DelegationDepth != 1 {
		t.Fatalf("child header = %+v, want parent/seed lineage", header)
	}
	persisted, err := st.LoadSessionRaw(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("load child raw: %v", err)
	}
	if len(persisted) <= len(parent.Events()) || persisted[len(parent.Events())-1].Type != session.EventTurnEnd {
		t.Fatalf("persisted fork prefix = %d events, boundary=%q; want durable completed seed", len(persisted), persisted[len(parent.Events())-1].Type)
	}
	for i, event := range parent.Events() {
		if persisted[i].Seq != event.Seq || persisted[i].Type != event.Type || string(persisted[i].Data) != string(event.Data) {
			t.Fatalf("seed event %d = %+v, want parent event %+v", i, persisted[i], event)
		}
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestForkSeedsColdDurableParentWhenRuntimeIsNotLive(t *testing.T) {
	st, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	parent := session.New()
	for _, event := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStartAt(1)},
		{session.EventStepStart, session.NewStepStartAt(1, 1)},
		{session.EventUserMessage, session.NewUserMessageAt(1, 1, 0, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("cold context")}})},
		{session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "cold answer", nil, "stop", "", llm.TokenUsage{})},
		{session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", "")},
		{session.EventTurnEnd, session.NewTurnEndAt(1, "completed", "")},
	} {
		if _, err := parent.Append(event.typ, event.data); err != nil {
			t.Fatalf("append parent %s: %v", event.typ, err)
		}
	}
	parentEvents := parent.Events()
	if err := st.CreateSession(context.Background(), "cold-parent", time.Now().UTC()); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := st.AppendEvents(context.Background(), "cold-parent", parentEvents); err != nil {
		t.Fatalf("persist parent: %v", err)
	}

	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "cold child"}, {Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewForkProvider(Deps{LLM: model, Tools: tools.New(), Prompt: prompt.New("child"), Model: "m", Store: st})
	run, err := prov.Start(context.Background(), StartRequest{ParentSessionID: "cold-parent", Prompt: "use cold seed", InheritParentContext: true})
	if err != nil {
		t.Fatalf("start cold fork: %v", err)
	}
	if _, err := run.Result(context.Background()); err != nil {
		t.Fatalf("fork result: %v", err)
	}
	child, ok := prov.ChildLog(run.ID)
	if !ok {
		t.Fatalf("missing child log %q", run.ID)
	}
	history := child.DeriveHistory()
	if len(history) != 4 || history[0].Text() != "cold context" || history[1].Text() != "cold answer" || history[2].Text() != "use cold seed" || history[3].Text() != "cold child" {
		t.Fatalf("cold fork history = %+v, want durable parent prefix followed by child turn", history)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSpawnDoesNotPublishChildBeforeInitializationCommits(t *testing.T) {
	base, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer base.Close()

	var bound string
	prov := NewSpawnProvider(Deps{
		LLM:   &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}},
		Tools: tools.New(), Prompt: prompt.New("child"), Model: "m",
		Store:          failingSessionCreateStore{Store: base, err: errors.New("injected create failure")},
		BindSessionLog: func(id string, _ *session.Log) { bound = id },
	})
	_, err = prov.Start(context.Background(), StartRequest{Prompt: "must not publish", ParentSessionID: "parent"})
	if err == nil || !strings.Contains(err.Error(), "injected create failure") {
		t.Fatalf("start error = %v, want injected persistence failure", err)
	}
	if bound != "" {
		t.Fatalf("child log published as %q before initialization committed", bound)
	}
	if children, listErr := prov.ListChildren(context.Background(), "parent"); listErr != nil || len(children) != 0 {
		t.Fatalf("children after failed initialization = %+v, err=%v; want none", children, listErr)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnAtomicPublicationFailureLeavesNoDurableChild exercises the
// production SQLite path: header, fork seed, and the subagent/start receipt are
// one transaction. An injected transaction failure must therefore leave neither
// a live provider child nor a cold durable child that looks publishable.
func TestSpawnAtomicPublicationFailureLeavesNoDurableChild(t *testing.T) {
	base, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()

	var bound string
	prov := NewSpawnProvider(Deps{
		LLM:   &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}},
		Tools: tools.New(), Prompt: prompt.New("child"), Model: "m",
		Store:          failingSessionCreateOptionsStore{Store: base, SessionCreateStore: base, err: errors.New("injected atomic publication failure")},
		BindSessionLog: func(id string, _ *session.Log) { bound = id },
	})
	_, err = prov.Start(context.Background(), StartRequest{Prompt: "must not publish", ParentSessionID: "parent"})
	if err == nil || !strings.Contains(err.Error(), "injected atomic publication failure") {
		t.Fatalf("start error = %v, want injected atomic publication failure", err)
	}
	if bound != "" {
		t.Fatalf("child log published as %q before atomic publication committed", bound)
	}
	if children, listErr := prov.ListChildren(context.Background(), "parent"); listErr != nil || len(children) != 0 {
		t.Fatalf("children after failed atomic publication = %+v, err=%v; want none", children, listErr)
	}
	if _, loadErr := base.LoadSession(context.Background(), "spawn-1"); !errors.Is(loadErr, store.ErrNotFound) {
		t.Fatalf("durable child after failed atomic publication = %v, want ErrNotFound", loadErr)
	}
	if metas, listErr := base.ListSessions(context.Background()); listErr != nil {
		t.Fatalf("list sessions after failed atomic publication: %v", listErr)
	} else {
		for _, meta := range metas {
			if meta.ID == "spawn-1" {
				t.Fatalf("durable child metadata survived failed atomic publication: %+v", meta)
			}
		}
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSpawnStartCancellationStopsDurableInitialization(t *testing.T) {
	base, err := store.OpenSQLite(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer base.Close()
	creating := &blockingSessionCreateStore{Store: base, started: make(chan struct{})}
	var bound string
	prov := NewSpawnProvider(Deps{
		LLM:   &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}},
		Tools: tools.New(), Prompt: prompt.New("child"), Model: "m", Store: creating,
		BindSessionLog: func(id string, _ *session.Log) { bound = id },
	})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() {
		_, startErr := prov.Start(ctx, StartRequest{Prompt: "cancel during create", ParentSessionID: "parent"})
		started <- startErr
	}()
	select {
	case <-creating.started:
	case <-time.After(time.Second):
		t.Fatal("durable child creation did not start")
	}
	cancel()
	select {
	case startErr := <-started:
		if !errors.Is(startErr, context.Canceled) {
			t.Fatalf("start error = %v, want context.Canceled", startErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not honor cancellation during durable creation")
	}
	if bound != "" {
		t.Fatalf("child log published as %q after cancelled initialization", bound)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnDepthExceeded verifies the delegation-depth enforcement: a child of
// a depth-1 child is depth 2 and is rejected when MaxDepth=1; MaxDepth=0 means
// no limit.
func TestSpawnDepthExceeded(t *testing.T) {
	prov := NewSpawnProvider(Deps{
		LLM:    &scriptedLLM{},
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	ctx := context.Background()

	run, err := prov.Start(ctx, StartRequest{Label: "r", Prompt: "go", ParentSessionID: "root", MaxDepth: 1})
	if err != nil {
		t.Fatalf("start depth-1 child: %v", err)
	}
	if _, err := prov.Start(ctx, StartRequest{Label: "g", Prompt: "go", ParentSessionID: run.ID, MaxDepth: 1}); !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("grandchild err = %v, want ErrDepthExceeded", err)
	}
	// MaxDepth 0 removes the limit.
	if _, err := prov.Start(ctx, StartRequest{Label: "g2", Prompt: "go", ParentSessionID: run.ID, MaxDepth: 0}); err != nil {
		t.Fatalf("no-limit grandchild: %v", err)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnCancelAborts verifies Cancel cancels a live child's current turn and
// the terminal result maps to aborted; cancelling an already-finished child
// fails.
func TestSpawnCancelAborts(t *testing.T) {
	started := make(chan struct{})
	prov := NewSpawnProvider(Deps{
		LLM:    &blockingLLM{started: started},
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	ctx := context.Background()

	run, err := prov.Start(ctx, StartRequest{Label: "slow", Prompt: "work", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-started // the child is inside its first model request
	if err := run.Cancel("user interrupt"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	res, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.StopReason != StopAborted {
		t.Fatalf("stopReason = %q, want %q", res.StopReason, StopAborted)
	}
	if err := run.Cancel("again"); err == nil {
		t.Fatal("cancelling a finished child must fail")
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnStopReasonMapping covers the finish-reason → StopReason mapping for
// clean completions (max-tokens and refusal included).
func TestSpawnStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", StopCompleted},
		{"", StopCompleted},
		{"length", StopMaxTokens},
		{"max_tokens", StopMaxTokens},
		{"content_filter", StopRefusal},
	}
	for _, tc := range cases {
		model := &scriptedLLM{steps: [][]llm.StreamEvent{{
			{Kind: llm.StreamFinish, FinishReason: tc.finish},
		}}}
		prov := NewSpawnProvider(Deps{
			LLM:    model,
			Tools:  tools.New(),
			Prompt: prompt.New("x"),
			Model:  "m",
		})
		run, err := prov.Start(context.Background(), StartRequest{Prompt: "go", ParentSessionID: "p"})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		res, err := run.Result(context.Background())
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		if res.StopReason != tc.want {
			t.Fatalf("finish %q: stopReason = %q, want %q", tc.finish, res.StopReason, tc.want)
		}
		if err := prov.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// TestSpawnStopError verifies a failed child run maps to StopReason error.
func TestSpawnStopError(t *testing.T) {
	prov := NewSpawnProvider(Deps{
		LLM:    &errorLLM{err: errors.New("model boom")},
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	run, err := prov.Start(context.Background(), StartRequest{Prompt: "go", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := run.Result(context.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.StopReason != StopError {
		t.Fatalf("stopReason = %q, want %q", res.StopReason, StopError)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnCloseNoLeak verifies Close cancels a live child and awaits every
// child (one completed + one blocking), returns promptly, and rejects Start
// afterwards — so no background goroutine leaks.
func TestSpawnCloseNoLeak(t *testing.T) {
	started := make(chan struct{})
	mixed := &mixedLLM{
		started: started,
		first:   []llm.StreamEvent{{Kind: llm.StreamFinish, FinishReason: "stop"}},
	}
	prov := NewSpawnProvider(Deps{
		LLM:    mixed,
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	ctx := context.Background()

	run1, err := prov.Start(ctx, StartRequest{Label: "fast", Prompt: "a", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("start fast: %v", err)
	}
	if _, err := run1.Result(ctx); err != nil {
		t.Fatalf("fast result: %v", err)
	}
	if _, err := prov.Start(ctx, StartRequest{Label: "slow", Prompt: "b", ParentSessionID: "p"}); err != nil {
		t.Fatalf("start slow: %v", err)
	}
	<-started // the slow child is live and blocking

	closed := make(chan error, 1)
	go func() { closed <- prov.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a live child")
	}
	if _, err := prov.Start(ctx, StartRequest{Prompt: "c", ParentSessionID: "p"}); !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("start after close err = %v, want ErrProviderClosed", err)
	}
}

// userMessageText returns the text of the first RoleUser message of a model
// request — the child's prompt as the model actually saw it (the loop builds
// the request as [system, ...injected context, ...history], so the user prompt
// is not at index 0).
func userMessageText(req llm.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			return m.Text()
		}
	}
	return ""
}

// TestWithAcceptance verifies the pure withAcceptance helper: empty criteria
// leave the prompt untouched; non-empty criteria append the
// "验收标准（交付自检）" section with one "- <criterion>" line per criterion;
// blank criteria are skipped; and the original prompt is preserved.
func TestWithAcceptance(t *testing.T) {
	if got := withAcceptance("do X", nil); got != "do X" {
		t.Fatalf("withAcceptance(nil) = %q, want prompt unchanged", got)
	}
	if got := withAcceptance("do X", []string{}); got != "do X" {
		t.Fatalf("withAcceptance(empty) = %q, want prompt unchanged", got)
	}

	prompt := "do X"
	out := withAcceptance(prompt, []string{"contains:输出含报告", "llm:结论合理", "  "})
	if !strings.HasPrefix(out, prompt) {
		t.Fatalf("withAcceptance output = %q, want the original prompt preserved", out)
	}
	if !strings.Contains(out, "验收标准（交付自检）") {
		t.Fatalf("withAcceptance output = %q, want the acceptance section header", out)
	}
	for _, c := range []string{"contains:输出含报告", "llm:结论合理"} {
		if !strings.Contains(out, "- "+c) {
			t.Fatalf("withAcceptance output = %q, want criterion line %q", out, "- "+c)
		}
	}
	if n := strings.Count(out, "\n- "); n != 2 {
		t.Fatalf("withAcceptance output = %q, want 2 criterion bullets, got %d", out, n)
	}
}

// TestSpawnInjectsAcceptance verifies SpawnProvider injects the acceptance
// criteria into the child's prompt (the model's first request carries the
// "验收标准" section and every criterion) and the run completes normally.
func TestSpawnInjectsAcceptance(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewSpawnProvider(Deps{
		Log:    session.New(),
		LLM:    model,
		Tools:  tools.New(),
		Prompt: prompt.New("You are a subagent."),
		Model:  "deepseek-chat",
	})
	ctx := context.Background()

	run, err := prov.Start(ctx, StartRequest{
		Prompt:             "do X",
		AcceptanceCriteria: []string{"contains:输出含报告", "llm:结论合理"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.StopReason != StopCompleted {
		t.Fatalf("stopReason = %q, want %q", res.StopReason, StopCompleted)
	}
	if len(model.calls) != 1 {
		t.Fatalf("child llm calls = %d, want 1", len(model.calls))
	}
	user := userMessageText(model.calls[0])
	if !strings.Contains(user, "验收标准") || !strings.Contains(user, "contains:输出含报告") || !strings.Contains(user, "llm:结论合理") {
		t.Fatalf("child user message = %q, want the acceptance section with both criteria", user)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSpawnNoCriteriaNoInjection verifies that without AcceptanceCriteria the
// child's user message is exactly the original prompt — no acceptance section
// is injected.
func TestSpawnNoCriteriaNoInjection(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewSpawnProvider(Deps{
		LLM:    model,
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	ctx := context.Background()

	run, err := prov.Start(ctx, StartRequest{Prompt: "do X", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := run.Result(ctx); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("child llm calls = %d, want 1", len(model.calls))
	}
	user := userMessageText(model.calls[0])
	if user != "do X" {
		t.Fatalf("child user message = %q, want the original prompt unchanged", user)
	}
	if strings.Contains(user, "验收标准") {
		t.Fatalf("child user message = %q, want no acceptance section", user)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestRuntimeWithSpawnProvider integrates the spawn provider into a Runtime:
// register, Start through the runtime (with capability validation passing for
// MaxDepth against the depth-limit provider), ListChildren aggregation, and
// Close releasing the provider.
func TestRuntimeWithSpawnProvider(t *testing.T) {
	rt := NewRuntime()
	parentLog := session.New()
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "done"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	prov := NewSpawnProvider(Deps{
		Log:    parentLog,
		LLM:    model,
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	if err := rt.RegisterProvider(prov); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	run, err := rt.Start(ctx, "spawn", StartRequest{Label: "r", Prompt: "go", ParentSessionID: "p"})
	if err != nil {
		t.Fatalf("runtime start: %v", err)
	}
	if run.ID == "" {
		t.Fatal("run id must be non-empty")
	}
	res, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.StopReason != StopCompleted {
		t.Fatalf("stopReason = %q, want %q", res.StopReason, StopCompleted)
	}
	// MaxDepth passes the capability gate because spawn declares DepthLimit.
	if _, err := rt.Start(ctx, "spawn", StartRequest{Prompt: "x", ParentSessionID: "p", MaxDepth: 5}); err != nil {
		t.Fatalf("runtime start with max_depth: %v", err)
	}
	children, err := rt.ListChildren(ctx, "p")
	if err != nil {
		t.Fatalf("runtime list children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %+v, want 2 spawned under p", children)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("runtime close: %v", err)
	}
}
