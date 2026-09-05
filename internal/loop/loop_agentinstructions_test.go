package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/agentinstructions"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestEffectiveInjectorsOrderInstructionsBeforeRuntime(t *testing.T) {
	l := New(Config{
		LLM:            &scriptedLLM{},
		Log:            session.New(),
		Tools:          newTestRegistry(t),
		Prompt:         prompt.New("test"),
		RuntimeContext: func(context.Context, string) []llm.Message { return nil },
		PreStep: []PreStepInjector{
			{Name: "agent-instructions"},
			{Name: "skill"},
		},
	})
	names := make([]string, 0, 3)
	for _, injector := range l.effectiveInjectors() {
		names = append(names, injector.Name)
	}
	want := []string{"agent-instructions", "runtime-context", "skill"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("injector order = %#v, want %#v", names, want)
	}
}

func TestAppendContextMessagePreservesProducerSource(t *testing.T) {
	log := session.New()
	l := &Loop{log: log}
	message := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("workspace rules")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceBaseline: true, SourceBaselineIdentity: "identity",
		SourceChanges: []map[string]string{{"action": "set", "scope": ".", "path": "AGENTS.md"}},
	}
	if err := l.appendContextMessage("agent-instructions", message); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Source *struct {
			Kind             string              `json:"kind"`
			Form             string              `json:"form"`
			Baseline         bool                `json:"baseline"`
			BaselineIdentity string              `json:"baselineIdentity"`
			Changes          []map[string]string `json:"changes"`
		} `json:"source"`
	}
	if err := json.Unmarshal(log.Events()[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source == nil || data.Source.Kind != "agent-instructions" ||
		data.Source.Form != "instructions" || !data.Source.Baseline ||
		data.Source.BaselineIdentity != "identity" || len(data.Source.Changes) != 1 {
		t.Fatalf("durable source = %+v", data.Source)
	}
}

func TestAgentInstructionsTurnLifecycle(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	instructionPath := filepath.Join(workspace, "AGENTS.md")
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionPath, []byte("first rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := agentinstructions.Config{Home: home}
	log := session.New()
	model := &scriptedLLM{}
	var state *agentinstructions.State
	runTurn := func(t *testing.T, text string) {
		t.Helper()
		model.steps = [][]llm.StreamEvent{{
			{Kind: llm.StreamTextDelta, Text: "ok"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}
		agentLoop := New(Config{
			LLM:    model,
			Log:    log,
			Tools:  newTestRegistry(t),
			Prompt: prompt.New("You are helpful."),
			Model:  "deepseek-chat",
			PreStep: []PreStepInjector{{
				Name: "agent-instructions",
				InjectWithError: func(context.Context, string) ([]llm.Message, error) {
					message, next, err := agentinstructions.Reconcile(workspace, config, state)
					if err != nil {
						return nil, err
					}
					if next != nil {
						state = next
					}
					if message == nil {
						return nil, nil
					}
					return []llm.Message{*message}, nil
				},
				OncePerTurn: false,
				Deduplicate: true,
				Unbounded:   true,
			}},
		})
		if err := agentLoop.Run(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	countBaselineEvents := func() int {
		count := 0
		for _, event := range log.Events() {
			if event.Type != session.EventUserMessage ||
				!strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
				continue
			}
			count++
		}
		return count
	}

	runTurn(t, "first")
	if countBaselineEvents() != 1 {
		t.Fatalf("first-turn baseline events = %d, want 1", countBaselineEvents())
	}
	first := model.calls[0].Messages
	if len(first) != 3 || first[1].Text() != "first" || !strings.Contains(first[2].Text(), "first rules") {
		t.Fatalf("first request = %+v, want user then instruction baseline", first)
	}

	runTurn(t, "second")
	if countBaselineEvents() != 1 {
		t.Fatalf("unchanged second-turn baseline events = %d, want 1", countBaselineEvents())
	}
	secondHasBaseline := false
	for _, message := range model.calls[1].Messages {
		if strings.Contains(message.Text(), "first rules") {
			secondHasBaseline = true
			break
		}
	}
	if !secondHasBaseline {
		t.Fatalf("second request lost durable baseline: %+v", model.calls[1].Messages)
	}

	if err := os.WriteFile(instructionPath, []byte("updated rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTurn(t, "third")
	if countBaselineEvents() != 2 {
		t.Fatalf("changed third-turn baseline events = %d, want 2", countBaselineEvents())
	}
	thirdHasUpdate := false
	for _, message := range model.calls[2].Messages {
		if strings.Contains(message.Text(), "updated rules") {
			thirdHasUpdate = true
			break
		}
	}
	if !thirdHasUpdate {
		t.Fatalf("third request = %+v, want changed instruction baseline", model.calls[2].Messages)
	}
	var lastInstruction struct {
		Source *struct {
			Baseline bool `json:"baseline"`
			Changes  []struct {
				Action string `json:"action"`
				Path   string `json:"path"`
			} `json:"changes"`
		} `json:"source"`
	}
	for _, event := range log.Events() {
		if event.Type == session.EventUserMessage &&
			strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
			lastInstruction = struct {
				Source *struct {
					Baseline bool `json:"baseline"`
					Changes  []struct {
						Action string `json:"action"`
						Path   string `json:"path"`
					} `json:"changes"`
				} `json:"source"`
			}{}
			if err := json.Unmarshal(event.Data, &lastInstruction); err != nil {
				t.Fatal(err)
			}
		}
	}
	if lastInstruction.Source == nil || lastInstruction.Source.Baseline ||
		len(lastInstruction.Source.Changes) != 1 ||
		lastInstruction.Source.Changes[0].Action != "replace" ||
		lastInstruction.Source.Changes[0].Path != "AGENTS.md" {
		t.Fatalf("changed instruction source = %+v, want non-baseline replace", lastInstruction.Source)
	}
}

func TestAgentInstructionsReconcilesAfterToolContinuation(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	instructionPath := filepath.Join(workspace, "AGENTS.md")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(instructionPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("before tool")
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{
			Kind: llm.StreamFinish, FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{ID: "touch-agents", Name: "touch_agents", Arguments: "{}"}},
		}},
		{{Kind: llm.StreamTextDelta, Text: "done"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	log := session.New()
	registry := newTestRegistry(t)
	if err := registry.Register(touchAgentsTool{path: instructionPath}); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(tools.Policy{Enabled: []string{"touch_agents"}})

	config := agentinstructions.Config{Home: home}
	var state *agentinstructions.State
	agentLoop := New(Config{
		LLM: model, Log: log, Tools: registry,
		Prompt: prompt.New("You are helpful."), Model: "deepseek-chat",
		PreStep: []PreStepInjector{{
			Name: "agent-instructions",
			InjectWithError: func(context.Context, string) ([]llm.Message, error) {
				message, next, err := agentinstructions.Reconcile(workspace, config, state)
				if err != nil {
					return nil, err
				}
				if next != nil {
					state = next
				}
				if message == nil {
					return nil, nil
				}
				return []llm.Message{*message}, nil
			},
			OncePerTurn: false,
			Deduplicate: true,
			Unbounded:   true,
		}},
	})
	if err := agentLoop.Run(context.Background(), "update instructions"); err != nil {
		t.Fatal(err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want tool continuation", len(model.calls))
	}
	var delta struct {
		Source *struct {
			Baseline bool `json:"baseline"`
			Changes  []struct {
				Action string `json:"action"`
				Path   string `json:"path"`
			} `json:"changes"`
		} `json:"source"`
	}
	for _, event := range log.Events() {
		if event.Type != session.EventUserMessage ||
			!strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
			continue
		}
		delta = struct {
			Source *struct {
				Baseline bool `json:"baseline"`
				Changes  []struct {
					Action string `json:"action"`
					Path   string `json:"path"`
				} `json:"changes"`
			} `json:"source"`
		}{}
		if err := json.Unmarshal(event.Data, &delta); err != nil {
			t.Fatal(err)
		}
	}
	if delta.Source == nil || delta.Source.Baseline ||
		len(delta.Source.Changes) != 1 ||
		delta.Source.Changes[0].Action != "replace" ||
		delta.Source.Changes[0].Path != "AGENTS.md" {
		t.Fatalf("tool continuation source = %+v, want same-turn replace", delta.Source)
	}
	second := model.calls[1].Messages
	if !strings.Contains(second[len(second)-1].Text(), "after tool") {
		t.Fatalf("continuation request = %+v, want reconciled instructions before next request", second)
	}
}

type touchAgentsTool struct {
	path string
}

func (t touchAgentsTool) Name() string        { return "touch_agents" }
func (t touchAgentsTool) Description() string { return "test-only instruction file touch" }
func (t touchAgentsTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (touchAgentsTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t touchAgentsTool) Execute(context.Context, any) (string, error) {
	if err := os.WriteFile(t.path, []byte("after tool"), 0o644); err != nil {
		return "", err
	}
	return "updated", nil
}
