package extensionhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

type strategyConnection struct {
	mu           sync.Mutex
	contextCalls []extension.ContextRequest
	result       func(index int, ctx context.Context) (extension.ContextResult, error)
}

func (c *strategyConnection) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case extension.MethodProvideContext:
		request, ok := params.(extension.ContextRequest)
		if !ok {
			return errors.New("invalid context request")
		}
		c.mu.Lock()
		index := len(c.contextCalls)
		c.contextCalls = append(c.contextCalls, request)
		resultFn := c.result
		c.mu.Unlock()
		value, err := resultFn(index, ctx)
		if err != nil {
			return err
		}
		if target, ok := result.(*extension.ContextResult); ok {
			*target = value
		}
		return nil
	case extension.MethodCallTool:
		if target, ok := result.(*extension.ToolCallResult); ok {
			*target = extension.ToolCallResult{Value: "tool-result"}
		}
		return nil
	default:
		return nil
	}
}

func (c *strategyConnection) Close() error { return nil }

func (c *strategyConnection) calls() []extension.ContextRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]extension.ContextRequest(nil), c.contextCalls...)
}

type strategyModel struct {
	mu        sync.Mutex
	toolCalls int
	requests  []llm.ChatRequest
}

func (m *strategyModel) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	m.mu.Lock()
	index := len(m.requests)
	m.requests = append(m.requests, request)
	toolCalls := m.toolCalls
	m.mu.Unlock()
	if index < toolCalls {
		return newStrategyReader([]llm.StreamEvent{{
			Kind: llm.StreamFinish, FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: fmt.Sprintf("call-%d", index+1), Name: PublicToolName("strategy", "probe"), Arguments: "{}",
			}},
		}}), nil
	}
	return newStrategyReader([]llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "done"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}), nil
}

type strategyReader struct {
	events []llm.StreamEvent
	index  int
}

func newStrategyReader(events []llm.StreamEvent) *strategyReader {
	return &strategyReader{events: events}
}

func (r *strategyReader) Next() (llm.StreamEvent, error) {
	if r.index >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := r.events[r.index]
	r.index++
	return event, nil
}

func strategyHost(t *testing.T, strategy extension.ContextStrategy, required bool, conn *strategyConnection, registry *tools.Registry) *Host {
	t.Helper()
	host := New(Config{
		Registry: registry, MaxContributionChars: 80, MaxContributionTokens: 20,
		GlobalContextChars: 200, GlobalContextTokens: 50,
	})
	definition := extension.ToolDefinition{
		Name: "probe", Description: "probe", Risk: extension.ToolRiskRead,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "string"},
	}
	item := &managedExtension{
		manifest: extension.Manifest{
			ID:           "strategy",
			Capabilities: extension.Capabilities{Tools: true, ContextProvider: true},
			Tools:        extension.ToolsContribution{Definitions: []extension.ToolDefinition{definition}},
			ContextProvider: extension.ContextProviderConfig{
				Enabled: true, Strategy: strategy, Required: required, MaxChars: 4000,
			},
		},
		connection: conn, grants: map[string]struct{}{"session.id": {}, "session.turn": {}, "session.step": {}, "user.input": {}},
		initialized: extension.InitializeResult{
			Capabilities: extension.Capabilities{Tools: true, ContextProvider: true},
			Tools:        []extension.ToolDefinition{definition},
		},
	}
	if err := host.publishTools(item); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.items = append(host.items, item)
	host.mu.Unlock()
	return host
}

func runStrategyTurn(t *testing.T, host *Host, registry *tools.Registry, sessionID, input string, toolCalls int) *session.Log {
	t.Helper()
	log := session.New()
	model := &strategyModel{toolCalls: toolCalls}
	agent := loop.New(loop.Config{
		LLM: model, Log: log, Tools: registry, Prompt: promptForTest("system"),
		RuntimeSessionID: sessionID, RuntimeAgentID: sessionID,
		PreStep: []loop.PreStepInjector{host.ContextInjector()},
	})
	if err := agent.Run(context.Background(), input); err != nil {
		t.Fatalf("agent loop: %v", err)
	}
	if len(model.requests) != toolCalls+1 {
		t.Fatalf("model calls = %d, want %d", len(model.requests), toolCalls+1)
	}
	return log
}

func promptForTest(text string) *prompt.Builder { return prompt.New(text) }

func TestContextProviderStrategyIntegrationInAgentLoop(t *testing.T) {
	tests := []struct {
		name     string
		strategy extension.ContextStrategy
		want     int
	}{
		{name: "once per turn", strategy: extension.ContextOncePerTurn, want: 1},
		{name: "before every model call", strategy: extension.ContextBeforeEveryModelCall, want: 3},
		{name: "on user input change", strategy: extension.ContextOnUserInputChange, want: 1},
		{name: "after tool result", strategy: extension.ContextAfterToolResult, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := tools.New()
			registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
			conn := &strategyConnection{result: func(index int, _ context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: "strategy", Content: fmt.Sprintf("context %d", index), Truncatable: true,
				}}}, nil
			}}
			host := strategyHost(t, test.strategy, false, conn, registry)
			defer host.Close()
			runStrategyTurn(t, host, registry, "strategy-session", "user request", 2)
			if got := len(conn.calls()); got != test.want {
				t.Fatalf("context calls = %d, want %d", got, test.want)
			}
		})
	}
}

func TestContextProviderManualStrategyRequiresExplicitRefresh(t *testing.T) {
	registry := tools.New()
	conn := &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
		return extension.ContextResult{Contributions: []extension.ContextContribution{{
			Source: "strategy", Content: "manual evidence", Truncatable: true,
		}}}, nil
	}}
	host := strategyHost(t, extension.ContextManual, false, conn, registry)
	defer host.Close()
	runStrategyTurn(t, host, registry, "manual-session", "user request", 0)
	if got := len(conn.calls()); got != 0 {
		t.Fatalf("automatic calls = %d, want 0", got)
	}
	contributions, err := host.RefreshContext(context.Background(), "user request")
	if err != nil || len(contributions) != 1 {
		t.Fatalf("manual refresh = %#v, %v", contributions, err)
	}
	if got := len(conn.calls()); got != 1 {
		t.Fatalf("manual calls = %d, want 1", got)
	}
}

func TestContextOnUserInputChangeNormalizationAndSessionIsolation(t *testing.T) {
	registry := tools.New()
	registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
	conn := &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
		return extension.ContextResult{Contributions: []extension.ContextContribution{{
			Source: "strategy", Content: "evidence", Truncatable: true,
		}}}, nil
	}}
	host := strategyHost(t, extension.ContextOnUserInputChange, false, conn, registry)
	defer host.Close()
	runStrategyTurn(t, host, registry, "input-session", "  same   input  ", 2)
	runStrategyTurn(t, host, registry, "input-session", "same input", 0)
	runStrategyTurn(t, host, registry, "input-session", "different input", 0)
	if got := len(conn.calls()); got != 2 {
		t.Fatalf("calls across normalized input changes = %d, want 2", got)
	}

	registry = tools.New()
	registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
	conn = &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
		return extension.ContextResult{Contributions: []extension.ContextContribution{{
			Source: "strategy", Content: "second session", Truncatable: true,
		}}}, nil
	}}
	host = strategyHost(t, extension.ContextOncePerTurn, false, conn, registry)
	defer host.Close()
	runStrategyTurn(t, host, registry, "session-a", "same", 2)
	runStrategyTurn(t, host, registry, "session-b", "same", 2)
	if got := len(conn.calls()); got != 2 {
		t.Fatalf("calls across sessions = %d, want 2", got)
	}
}

func TestContextProviderFailureModelMatrix(t *testing.T) {
	tests := []struct {
		name        string
		result      func(int, context.Context) (extension.ContextResult, error)
		required    bool
		wantCount   int
		wantErr     bool
		wantContent string
	}{
		{
			name: "optional timeout fail soft", required: false,
			result: func(_ int, ctx context.Context) (extension.ContextResult, error) {
				select {
				case <-ctx.Done():
					return extension.ContextResult{}, ctx.Err()
				case <-time.After(50 * time.Millisecond):
					return extension.ContextResult{}, errors.New("late")
				}
			},
		},
		{
			name: "optional crash fail soft", required: false,
			result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{}, errors.New("simulated extension crash")
			},
		},
		{
			name: "required crash fails step", required: true,
			result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{}, errors.New("simulated extension crash")
			},
			wantErr: true,
		},
		{
			name: "empty contribution", result: func(int, context.Context) (extension.ContextResult, error) { return extension.ContextResult{}, nil },
		},
		{
			name: "oversized truncatable contribution", result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: "strategy", Content: strings.Repeat("x", 100), Truncatable: true,
				}}}, nil
			}, wantCount: 1, wantContent: "xxxx",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := tools.New()
			registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
			conn := &strategyConnection{result: test.result}
			host := strategyHost(t, extension.ContextBeforeEveryModelCall, test.required, conn, registry)
			defer host.Close()
			ctx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
				SessionID: "failure-session", TurnID: "turn:1", StepID: "step:1",
			})
			state := host.contextStateFor(ctx)
			contributions, err := host.ProvideContext(ctx, "request", state)
			if test.wantErr && err == nil {
				t.Fatal("required provider failure was swallowed")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("optional provider changed loop outcome: %v", err)
			}
			if len(contributions) != test.wantCount {
				t.Fatalf("contributions = %#v, want %d", contributions, test.wantCount)
			}
			if test.wantContent != "" && !strings.Contains(contributions[0].Content, test.wantContent) {
				t.Fatalf("truncated content = %q", contributions[0].Content)
			}
			if test.wantContent != "" && len(contributions[0].Content) > 80 {
				t.Fatalf("content exceeded contribution budget: %d", len(contributions[0].Content))
			}
		})
	}
}

func TestContextContributionIsPersistedOncePerInjection(t *testing.T) {
	registry := tools.New()
	registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
	conn := &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
		return extension.ContextResult{Contributions: []extension.ContextContribution{{
			Source: "strategy", Content: "stable evidence", Truncatable: true,
		}}}, nil
	}}
	host := strategyHost(t, extension.ContextOncePerTurn, false, conn, registry)
	defer host.Close()
	log := runStrategyTurn(t, host, registry, "persist-session", "user request", 2)
	count := 0
	for _, event := range log.Events() {
		if event.Type == session.EventUserMessage && strings.Contains(string(event.Data), `"plugin":"strategy"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("durable extension context rows = %d, want 1", count)
	}
}
