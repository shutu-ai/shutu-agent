package contractfixture_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// TestCrossEntryChildAgentFixture drives the shared protocol facts through the
// production child-agent runner. The LLM is scripted because this leg owns the
// delegation/runtime boundary, not a provider wire boundary. The child runs its
// real independent loop and SQLite log; after provider shutdown a separate
// store handle must rebuild the same canonical projection.
func TestCrossEntryChildAgentFixture(t *testing.T) {
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	var promptBlock struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(fixture.Prompt[0], &promptBlock); err != nil {
		t.Fatal(err)
	}

	model := &childAgentLLM{steps: [][]llm.StreamEvent{
		{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: "fixture-child-call", Name: fixture.Tool.Name,
				Arguments: string(fixture.Tool.Arguments),
			}},
		}},
		{
			{Kind: llm.StreamTextDelta, Text: fixture.Assistant},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}

	root := t.TempDir()
	storage, err := store.OpenSQLite(filepath.Join(root, "child-agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parentID := "child-agent-parent"
	if err := storage.CreateSession(ctx, parentID, time.UnixMilli(1).UTC()); err != nil {
		t.Fatal(err)
	}

	childTools := tools.New()
	echo := fixtureEchoTool{
		output:     fixture.Tool.Output,
		effectPath: filepath.Join(root, "fixture-effect.json"),
		cleanupDir: root,
	}
	if err := childTools.Register(echo); err != nil {
		t.Fatal(err)
	}
	childTools.Allow(echo.Name())

	provider := subagent.NewSpawnProvider(subagent.Deps{
		LLM:    model,
		Tools:  childTools,
		Prompt: prompt.New("child agent under test"),
		Model:  "cross-entry-child-model",
		Store:  storage,
	})
	run, err := provider.Start(ctx, subagent.StartRequest{
		Label:           fixture.Scenario,
		Prompt:          promptBlock.Text,
		ParentSessionID: parentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != fixture.Assistant || result.StopReason != subagent.StopCompleted {
		t.Fatalf("child result = %#v, want assistant %q and %q", result, fixture.Assistant, subagent.StopCompleted)
	}
	if len(model.calls) != 2 {
		t.Fatalf("child model calls = %d, want tool turn plus assistant turn", len(model.calls))
	}
	if len(model.calls[0].Tools) != 1 || model.calls[0].Tools[0].Name != fixture.Tool.Name {
		t.Fatalf("child tool schema = %#v, want %q", model.calls[0].Tools, fixture.Tool.Name)
	}
	var sawPrompt bool
	for _, message := range model.calls[0].Messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), promptBlock.Text) {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("first child request = %#v, want fixture prompt %q", model.calls[0].Messages, promptBlock.Text)
	}
	effect, err := os.ReadFile(echo.effectPath)
	if err != nil {
		t.Fatalf("read committed external effect: %v", err)
	}
	wantEffect, err := json.Marshal(map[string]string{
		"callId": "fixture-child-call",
		"output": fixture.Tool.Output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(effect) != string(wantEffect) {
		t.Fatalf("external effect = %s, want %s", effect, wantEffect)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, "fixture-effect-*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("tool cleanup left %d lock files: %v", len(leftovers), leftovers)
	}

	header, err := storage.GetSessionHeader(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Parent != parentID || header.Origin != "subagent" || header.DelegationDepth != 1 {
		t.Fatalf("child header = %#v, want one-step subagent lineage under %q", header, parentID)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen after runtime disposal so the assertion reads only committed
	// durable state, not the provider's in-memory child log.
	independent, err := store.OpenSQLite(filepath.Join(root, "child-agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	events, err := independent.LoadSession(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawToolResult bool
	for _, event := range events {
		if event.Type == session.EventToolResult && strings.Contains(string(event.Data), fixture.Tool.Output) {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("child events contain no fixture tool result; last events: %d total", len(events))
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AsOfSeq == 0 || len(snapshot.Surface) == 0 {
		t.Fatalf("child projection = %#v, want non-empty durable cursor and surface", snapshot)
	}
	if len(snapshot.History) < 2 ||
		snapshot.History[0].Role != llm.RoleUser ||
		snapshot.History[0].Text() != promptBlock.Text ||
		snapshot.History[len(snapshot.History)-1].Role != llm.RoleAssistant ||
		snapshot.History[len(snapshot.History)-1].Text() != fixture.Assistant {
		t.Fatalf("child projection history = %#v, want fixture prompt and final assistant", snapshot.History)
	}
}

type childAgentLLM struct {
	steps [][]llm.StreamEvent
	calls []llm.ChatRequest
}

func (m *childAgentLLM) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	m.calls = append(m.calls, request)
	if len(m.steps) == 0 {
		return &childAgentReader{}, nil
	}
	events := m.steps[0]
	m.steps = m.steps[1:]
	return &childAgentReader{events: events}, nil
}

type childAgentReader struct {
	events []llm.StreamEvent
	next   int
}

func (r *childAgentReader) Next() (llm.StreamEvent, error) {
	if r.next >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := r.events[r.next]
	r.next++
	return event, nil
}

type fixtureEchoTool struct {
	output     string
	effectPath string
	cleanupDir string
}

func (t fixtureEchoTool) Name() string { return "fixture_echo" }
func (t fixtureEchoTool) Description() string {
	return "Echo the shared cross-entry fixture output."
}
func (t fixtureEchoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"text"},
	}
}
func (t fixtureEchoTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}
func (t fixtureEchoTool) Execute(context.Context, any) (string, error) {
	lock, err := os.CreateTemp(t.cleanupDir, "fixture-effect-*.lock")
	if err != nil {
		return "", err
	}
	lockName := lock.Name()
	if _, err := lock.WriteString("external operation in flight"); err != nil {
		_ = lock.Close()
		return "", err
	}
	if err := lock.Sync(); err != nil {
		_ = lock.Close()
		return "", err
	}
	if err := lock.Close(); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(lockName) }()

	external, err := os.Create(t.effectPath)
	if err != nil {
		return "", err
	}
	if _, err := external.WriteString(`{"callId":"fixture-child-call","output":`); err != nil {
		_ = external.Close()
		return "", err
	}
	encoded, err := json.Marshal(t.output)
	if err != nil {
		_ = external.Close()
		return "", err
	}
	if _, err := external.Write(encoded); err != nil {
		_ = external.Close()
		return "", err
	}
	if _, err := external.WriteString("}"); err != nil {
		_ = external.Close()
		return "", err
	}
	if err := external.Sync(); err != nil {
		_ = external.Close()
		return "", err
	}
	if err := external.Close(); err != nil {
		return "", err
	}
	return t.output, nil
}
