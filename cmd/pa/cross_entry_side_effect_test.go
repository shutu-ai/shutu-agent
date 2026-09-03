package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/sdkclient"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

const sdkCrossEntryHelperEnv = "SHUTU_SDK_CROSS_ENTRY_SIDE_EFFECT_HELPER"

// TestCrossEntryWebSideEffectCleanupAndToolError drives the Web message
// handler through the shared fixture's tool error path. The tool commits an
// external effect before failing; the oracle proves the effect remains, the
// bounded lock is cleaned, the failure is a stable durable tool result, and a
// fresh SQLite handle rebuilds the same assistant projection.
func TestCrossEntryWebSideEffectCleanupAndToolError(t *testing.T) {
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
	root := t.TempDir()
	effectPath := filepath.Join(root, "web-effect.json")
	dbPath := filepath.Join(root, "web.db")
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessionID := "web-cross-entry"
	if err := st.CreateSession(ctx, sessionID, time.Now()); err != nil {
		t.Fatal(err)
	}

	registry := tools.New()
	effectTool := &crossEntryEffectTool{effectPath: effectPath, cleanupDir: root, output: fixture.Tool.Output}
	if err := registry.Register(effectTool); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(tools.Policy{Enabled: []string{fixture.Tool.Name}})
	registry.Allow(fixture.Tool.Name)
	provider := &crossEntryEffectProvider{toolName: fixture.Tool.Name}
	a := &app{
		cfg:           crossEntryConfig(),
		basePolicy:    tools.Policy{Enabled: []string{fixture.Tool.Name}},
		baseCtx:       ctx,
		store:         st,
		currentID:     sessionID,
		log:           session.New(),
		hub:           NewEventHub(),
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
		llm:           provider,
		reg:           registry,
		prompt:        prompt.New("cross-entry Web side-effect agent"),
	}
	a.attachSink(ctx)
	if err := a.webMessage(ctx, sessionID, promptBlock.Text, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	assertCrossEntryEffectSettlement(t, root, effectPath, dbPath, sessionID, fixture.Assistant, fixture.Tool.Output)
}

// TestCrossEntrySDKSideEffectCleanupAndToolError drives the same tool-error
// oracle through the public SDK client and the production SDK server in a real
// child process. The child owns SQLite; the parent may only read after the
// child transport has closed.
func TestCrossEntrySDKSideEffectCleanupAndToolError(t *testing.T) {
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
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	effectPath := filepath.Join(workspace, "sdk-effect.json")
	dbPath := filepath.Join(root, "sdk.db")
	client := sdkclient.NewClient(sdkclient.ClientOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestSDKCrossEntrySideEffectHelper$"},
		Dir:     workspace,
		Env: append(os.Environ(),
			sdkCrossEntryHelperEnv+"=1",
			"SHUTU_SDK_CROSS_ENTRY_DB="+dbPath,
			"SHUTU_SDK_CROSS_ENTRY_EFFECT="+effectPath,
			"SHUTU_SDK_CROSS_ENTRY_CLEANUP="+workspace,
		),
		RequestTimeout: 3 * time.Second,
	})
	subscription := client.Subscribe(nil)
	if _, err := client.Initialize(context.Background(), sdkclient.InitializeParams{CWD: workspace}); err != nil {
		t.Fatalf("SDK initialize: %v", err)
	}
	messageID, err := client.Prompt(context.Background(), "sdk-cross-entry", []sdkclient.ContentBlock{sdkclient.TextContent(promptBlock.Text)})
	if err != nil || messageID == "" {
		t.Fatalf("SDK prompt = %q, err=%v", messageID, err)
	}
	sawAssistant := false
	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !sawAssistant {
		notification, nextErr := subscription.Next(deadline)
		if nextErr != nil {
			t.Fatalf("SDK notifications: %v", nextErr)
		}
		if notification.Method != "session.event" {
			continue
		}
		var params struct {
			Event sdkclient.SessionEvent `json:"event"`
		}
		if json.Unmarshal(notification.Params, &params) != nil || params.Event.Type != session.EventAssistantMessage {
			continue
		}
		var data struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(params.Event.Data, &data) == nil && len(data.Message.Content) > 0 &&
			data.Message.Content[0].Text == fixture.Assistant {
			sawAssistant = true
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("SDK child close: %v", err)
	}
	assertCrossEntryEffectSettlement(t, workspace, effectPath, dbPath, "sdk-cross-entry", fixture.Assistant, fixture.Tool.Output)
}

// TestSDKCrossEntrySideEffectHelper is the real child SDK runtime. It owns the
// database and composition root; the parent only owns the public line protocol.
func TestSDKCrossEntrySideEffectHelper(t *testing.T) {
	if os.Getenv(sdkCrossEntryHelperEnv) != "1" {
		t.Skip("SDK cross-entry side-effect helper")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_SDK_CROSS_ENTRY_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	registry := tools.New()
	effectTool := &crossEntryEffectTool{
		effectPath: os.Getenv("SHUTU_SDK_CROSS_ENTRY_EFFECT"),
		cleanupDir: os.Getenv("SHUTU_SDK_CROSS_ENTRY_CLEANUP"),
		output:     fixture.Tool.Output,
	}
	if err := registry.Register(effectTool); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(tools.Policy{Enabled: []string{fixture.Tool.Name}})
	registry.Allow(fixture.Tool.Name)
	provider := &crossEntryEffectProvider{toolName: fixture.Tool.Name}
	llmRegistry := llm.NewRegistry()
	if err := llmRegistry.Register(provider); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{
			Model: "cross-entry-model",
			Mode:  config.ModeStandard,
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		basePolicy:    tools.Policy{Enabled: []string{fixture.Tool.Name}},
		baseCtx:       context.Background(),
		store:         st,
		hub:           NewEventHub(),
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
		llm:           provider,
		llmReg:        llmRegistry,
		reg:           registry,
		prompt:        prompt.New("cross-entry SDK side-effect agent"),
	}
	server := newSDKServer(a, os.Stdin, os.Stdout)
	if err := server.run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type crossEntryEffectProvider struct {
	toolName string
	calls    atomic.Int32
}

func (p *crossEntryEffectProvider) ID() string      { return "deepseek-official" }
func (p *crossEntryEffectProvider) Available() bool { return true }

func (p *crossEntryEffectProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	if p.calls.Add(1) == 1 {
		return &turnReader{events: []llm.StreamEvent{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: "cross-entry-call", Name: p.toolName,
				Arguments: `{"text":"trigger"}`,
			}},
		}}}, nil
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		return nil, err
	}
	return &turnReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: fixture.Assistant},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

type crossEntryEffectTool struct {
	effectPath string
	cleanupDir string
	output     string
}

func (t *crossEntryEffectTool) Name() string { return "fixture_echo" }
func (t *crossEntryEffectTool) Description() string {
	return "Commit the cross-entry fixture effect, then fail to exercise error settlement."
}
func (t *crossEntryEffectTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"text"},
	}
}
func (t *crossEntryEffectTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}

func (t *crossEntryEffectTool) Execute(context.Context, any) (string, error) {
	lock, err := os.CreateTemp(t.cleanupDir, "cross-entry-cleanup-*.lock")
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

	effect, err := json.Marshal(map[string]string{"output": t.output})
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(t.effectPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(effect); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return "", errors.New("fixture tool failed after external effect")
}

func assertCrossEntryEffectSettlement(t *testing.T, cleanupDir, effectPath, dbPath, sessionID, assistant, wantOutput string) {
	t.Helper()
	raw, err := os.ReadFile(effectPath)
	if err != nil {
		if debug, debugErr := store.OpenSQLite(dbPath); debugErr == nil {
			defer debug.Close()
			if debugEvents, eventErr := debug.LoadSession(context.Background(), sessionID); eventErr == nil {
				for _, event := range debugEvents {
					t.Logf("debug event %d %s %s", event.Seq, event.Type, event.Data)
				}
			}
		}
		t.Fatalf("read external effect: %v", err)
	}
	var effect struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &effect) != nil || effect.Output != wantOutput {
		t.Fatalf("external effect = %s", raw)
	}
	leftovers, err := filepath.Glob(filepath.Join(cleanupDir, "cross-entry-cleanup-*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("tool cleanup left %d locks: %v", len(leftovers), leftovers)
	}

	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var sawToolError bool
	for _, event := range events {
		if event.Type != session.EventToolError {
			continue
		}
		var payload struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.Error != nil &&
			payload.Error.Code == tools.CodeToolExecutionError {
			sawToolError = true
		}
	}
	if !sawToolError {
		t.Fatalf("durable events contain no stable tool execution error; count=%d", len(events))
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) == 0 || snapshot.History[len(snapshot.History)-1].Role != llm.RoleAssistant ||
		!strings.Contains(snapshot.History[len(snapshot.History)-1].Text(), assistant) {
		t.Fatalf("post-effect projection history = %#v, want final %q", snapshot.History, assistant)
	}
}

func crossEntryConfig() config.Config {
	return config.Config{Model: "cross-entry-model", Mode: config.ModeStandard}
}
