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
	toolCalls    []extension.ToolCallRequest
	result       func(index int, ctx context.Context) (extension.ContextResult, error)
	toolCall     func(index int, ctx context.Context) (extension.ToolCallResult, error)
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
		c.mu.Lock()
		index := len(c.toolCalls)
		c.toolCalls = append(c.toolCalls, params.(extension.ToolCallRequest))
		toolFn := c.toolCall
		c.mu.Unlock()
		value := extension.ToolCallResult{Value: "tool-result"}
		if toolFn != nil {
			var err error
			value, err = toolFn(index, ctx)
			if err != nil {
				return err
			}
		}
		if target, ok := result.(*extension.ToolCallResult); ok {
			*target = value
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

func (c *strategyConnection) tools() []extension.ToolCallRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]extension.ToolCallRequest(nil), c.toolCalls...)
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

func TestContextOncePerTurnRunsAgainOnNextTurn(t *testing.T) {
	registry := tools.New()
	registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
	conn := &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
		return extension.ContextResult{Contributions: []extension.ContextContribution{{
			Source: "strategy", Content: "turn evidence", Truncatable: true,
		}}}, nil
	}}
	host := strategyHost(t, extension.ContextOncePerTurn, false, conn, registry)
	defer host.Close()

	log := session.New()
	model := &strategyModel{toolCalls: 1}
	for _, input := range []string{"first turn", "second turn"} {
		agent := loop.New(loop.Config{
			LLM: model, Log: log, Tools: registry, Prompt: promptForTest("system"),
			RuntimeSessionID: "turn-session", RuntimeAgentID: "turn-session",
			PreStep: []loop.PreStepInjector{host.ContextInjector()},
		})
		if err := agent.Run(context.Background(), input); err != nil {
			t.Fatalf("agent turn %q: %v", input, err)
		}
	}
	if got := len(model.requests); got != 3 {
		t.Fatalf("model calls across turns = %d, want 3", got)
	}
	if got := len(conn.calls()); got != 2 {
		t.Fatalf("context calls across turns = %d, want 2", got)
	}
}

func TestContextBudgetsAreDeterministic(t *testing.T) {
	host := New(Config{
		GlobalContextChars: 220, MaxContributionChars: 240,
		GlobalContextTokens: 100, MaxContributionTokens: 100,
	})
	defer host.Close()
	newProvider := func(id string, priority, providerChars int, content string) *managedExtension {
		return &managedExtension{
			manifest: extension.Manifest{
				ID: id, Capabilities: extension.Capabilities{ContextProvider: true},
				ContextProvider: extension.ContextProviderConfig{
					Enabled: true, Strategy: extension.ContextBeforeEveryModelCall,
					Priority: priority, MaxChars: providerChars,
				},
			},
			connection: &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: id, Content: content, Priority: priority, Truncatable: true,
				}}}, nil
			}},
			grants:      map[string]struct{}{},
			initialized: extension.InitializeResult{Capabilities: extension.Capabilities{ContextProvider: true}},
		}
	}
	host.mu.Lock()
	host.items = []*managedExtension{
		newProvider("low", 10, 160, strings.Repeat("B", 200)),
		newProvider("high", 20, 120, strings.Repeat("A", 200)),
	}
	host.mu.Unlock()

	contributions, err := host.ProvideContext(context.Background(), "budget request", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 2 ||
		contributions[0].Source != "high" ||
		contributions[0].Content != truncateUTF8(strings.Repeat("A", 200), 120, true) ||
		contributions[1].Source != "low" ||
		contributions[1].Content != strings.Repeat("B", 80) {
		t.Fatalf("budgeted contributions = %#v", contributions)
	}
}

func TestContextContributionDeduplication(t *testing.T) {
	host := New(Config{})
	defer host.Close()
	newProvider := func(id string) *managedExtension {
		return &managedExtension{
			manifest: extension.Manifest{
				ID: id, Capabilities: extension.Capabilities{ContextProvider: true},
				ContextProvider: extension.ContextProviderConfig{
					Enabled: true, Strategy: extension.ContextBeforeEveryModelCall,
				},
			},
			connection: &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: "shared", Content: "same evidence", Truncatable: true,
				}}}, nil
			}},
			grants:      map[string]struct{}{},
			initialized: extension.InitializeResult{Capabilities: extension.Capabilities{ContextProvider: true}},
		}
	}
	host.mu.Lock()
	host.items = []*managedExtension{newProvider("first"), newProvider("second")}
	host.mu.Unlock()

	contributions, err := host.ProvideContext(context.Background(), "duplicate request", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 1 || contributions[0].Source != "shared" || contributions[0].Content != "same evidence" {
		t.Fatalf("deduplicated contributions = %#v", contributions)
	}
}

func TestContextGlobalTokenBudgetIsDeterministic(t *testing.T) {
	host := New(Config{
		GlobalContextChars: 1000, MaxContributionChars: 1000,
		GlobalContextTokens: 10, MaxContributionTokens: 100,
	})
	defer host.Close()
	newProvider := func(id, content string) *managedExtension {
		return &managedExtension{
			manifest: extension.Manifest{
				ID: id, Capabilities: extension.Capabilities{ContextProvider: true},
				ContextProvider: extension.ContextProviderConfig{
					Enabled: true, Strategy: extension.ContextBeforeEveryModelCall,
				},
			},
			connection: &strategyConnection{result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: id, Content: content, EstimatedTokens: 10, Truncatable: true,
				}}}, nil
			}},
			grants:      map[string]struct{}{},
			initialized: extension.InitializeResult{Capabilities: extension.Capabilities{ContextProvider: true}},
		}
	}
	host.mu.Lock()
	host.items = []*managedExtension{
		newProvider("high", strings.Repeat("A", 40)),
		newProvider("low", strings.Repeat("B", 40)),
	}
	host.mu.Unlock()

	contributions, err := host.ProvideContext(context.Background(), "token budget request", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 1 || contributions[0].Source != "high" || contributions[0].Content != strings.Repeat("A", 20) {
		t.Fatalf("token-budgeted contributions = %#v", contributions)
	}
}

func TestContextAfterToolResultToolOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		toolCall    func(index int, ctx context.Context) (extension.ToolCallResult, error)
		wantContext int
	}{
		{
			name: "successful tool result", wantContext: 1,
		},
		{
			name: "failed tool result", wantContext: 1,
			toolCall: func(int, context.Context) (extension.ToolCallResult, error) {
				return extension.ToolCallResult{Error: "simulated tool failure"}, nil
			},
		},
		{
			name: "cancelled tool result", wantContext: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registry := tools.New()
			registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
			conn := &strategyConnection{
				result: func(int, context.Context) (extension.ContextResult, error) {
					return extension.ContextResult{Contributions: []extension.ContextContribution{{
						Source: "strategy", Content: "after tool", Truncatable: true,
					}}}, nil
				},
				toolCall: test.toolCall,
			}
			host := strategyHost(t, extension.ContextAfterToolResult, false, conn, registry)
			defer host.Close()
			if test.wantContext == 0 {
				conn.toolCall = func(_ int, callCtx context.Context) (extension.ToolCallResult, error) {
					cancel()
					return extension.ToolCallResult{}, context.Canceled
				}
			}

			err := func() error {
				log := session.New()
				model := &strategyModel{toolCalls: 1}
				agent := loop.New(loop.Config{
					LLM: model, Log: log, Tools: registry, Prompt: promptForTest("system"),
					RuntimeSessionID: "tool-outcome-session", RuntimeAgentID: "tool-outcome-session",
					PreStep: []loop.PreStepInjector{host.ContextInjector()},
				})
				return agent.Run(ctx, "tool outcome")
			}()
			if test.wantContext == 0 {
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled loop error = %v, want context.Canceled", err)
				}
			} else if err != nil {
				t.Fatalf("agent loop: %v", err)
			}
			if len(conn.tools()) != 1 {
				t.Fatalf("tool calls = %d, want 1", len(conn.tools()))
			}
			if got := len(conn.calls()); got != test.wantContext {
				t.Fatalf("post-tool context calls = %d, want %d", got, test.wantContext)
			}
		})
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

func TestContextFailureModelAcrossStrategies(t *testing.T) {
	strategies := []extension.ContextStrategy{
		extension.ContextOncePerTurn,
		extension.ContextBeforeEveryModelCall,
		extension.ContextOnUserInputChange,
		extension.ContextAfterToolResult,
		extension.ContextManual,
	}
	failureModes := []struct {
		name       string
		result     func(int, context.Context) (extension.ContextResult, error)
		wantCount  int
		wantPrefix string
	}{
		{
			name: "timeout",
			result: func(_ int, ctx context.Context) (extension.ContextResult, error) {
				select {
				case <-ctx.Done():
					return extension.ContextResult{}, ctx.Err()
				case <-time.After(100 * time.Millisecond):
					return extension.ContextResult{}, errors.New("late")
				}
			},
		},
		{
			name: "crash",
			result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{}, errors.New("simulated extension crash")
			},
		},
		{
			name:      "empty contribution",
			result:    func(int, context.Context) (extension.ContextResult, error) { return extension.ContextResult{}, nil },
			wantCount: 0,
		},
		{
			name: "oversized contribution",
			result: func(int, context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{Contributions: []extension.ContextContribution{{
					Source: "strategy", Content: strings.Repeat("x", 200), Truncatable: true,
				}}}, nil
			},
			wantCount: 1, wantPrefix: "xxxx",
		},
		{
			name: "cancellation",
			result: func(_ int, ctx context.Context) (extension.ContextResult, error) {
				return extension.ContextResult{}, context.Canceled
			},
		},
	}

	for _, strategy := range strategies {
		for _, failure := range failureModes {
			t.Run(string(strategy)+"/"+failure.name, func(t *testing.T) {
				registry := tools.New()
				registry.SetPolicy(tools.Policy{Timeout: time.Second, Enabled: []string{PublicToolName("strategy", "probe")}})
				conn := &strategyConnection{result: failure.result}
				host := strategyHost(t, strategy, false, conn, registry)
				defer host.Close()
				ctx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{
					SessionID: "failure-matrix-session", TurnID: "turn:1", StepID: "step:2",
				})
				state := &contextCadenceState{}
				if strategy == extension.ContextAfterToolResult {
					state.lastTurn, state.lastStepID = "turn:1", "step:1"
				}

				var contributions []extension.ContextContribution
				var err error
				if strategy == extension.ContextManual {
					contributions, err = host.RefreshContext(ctx, "failure request")
				} else {
					contributions, err = host.ProvideContext(ctx, "failure request", state)
				}
				if err != nil {
					t.Fatalf("optional provider changed loop outcome: %v", err)
				}
				if len(contributions) != failure.wantCount {
					t.Fatalf("contributions = %#v, want %d", contributions, failure.wantCount)
				}
				if failure.wantPrefix != "" && (!strings.HasPrefix(contributions[0].Content, failure.wantPrefix) || len(contributions[0].Content) > 80) {
					t.Fatalf("truncated contribution = %q", contributions[0].Content)
				}
			})
		}
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
