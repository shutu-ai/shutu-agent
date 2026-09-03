package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// subagentStubLLM returns a fixed single-step stream (assistant answer, no
// tool calls) so a spawned child completes immediately.
type subagentStubLLM struct{}

func (subagentStubLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return &subagentStubReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "child answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

type subagentStubReader struct {
	events []llm.StreamEvent
	i      int
}

func (r *subagentStubReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

// makeSubagentApp builds a minimal app for registerSubagent tests: only the
// fields registerSubagent touches (cfg.Subagent/cfg.Model, reg, llm, prompt,
// log, currentID) are set.
func makeSubagentApp(enabled bool) *app {
	return &app{
		cfg: config.Config{
			Model:    "m",
			Subagent: config.SubagentConfig{Enabled: config.Bool(enabled), MaxDepth: 8, DefaultProvider: "spawn"},
		},
		reg:       tools.New(),
		log:       session.New(),
		llm:       subagentStubLLM{},
		prompt:    prompt.New("You are a subagent."),
		currentID: "s-test",
	}
}

// subagentPolicy whitelists the DSH Agent control tools so registry Execute can
// run them (in production config.applyDefaults + PolicyFromConfig do this).
func subagentPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"subagent", "subagent_fork", "spawn_teammate", "send_message", "followup_task", "wait_agent", "interrupt_agent", "list_agents"},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

func hasEvent(log *session.Log, typ string) bool {
	for _, ev := range log.Events() {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func countEvent(log *session.Log, typ string) int {
	n := 0
	for _, ev := range log.Events() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestRegisterSubagentDisabledRegistersNothing verifies the D10 gate: with
// subagent.enabled=false the composition root creates no runtime and registers
// no subagent_* tool (dispatch-m5b-2 §4).
func TestRegisterSubagentDisabledRegistersNothing(t *testing.T) {
	app := makeSubagentApp(false)
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	if app.subagents != nil {
		t.Fatal("subagent runtime must be nil when subagent.enabled=false")
	}
	for _, spec := range app.reg.Specs() {
		if strings.HasPrefix(spec.Name, "subagent_") {
			t.Fatalf("subagent tool %q registered while subagent disabled", spec.Name)
		}
	}
}

// TestRegisterSubagentEnabledRegistersAndLogsEvents verifies the DSH surface.
func TestRegisterSubagentEnabledRegistersAndLogsEvents(t *testing.T) {
	app := makeSubagentApp(true)
	app.reg.SetPolicy(subagentPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if app.subagents == nil {
		t.Fatal("subagent runtime must be created when subagent.enabled=true")
	}
	specs := app.reg.Specs()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	for _, want := range []string{"subagent", "subagent_fork", "spawn_teammate", "send_message", "followup_task", "wait_agent", "interrupt_agent", "list_agents"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}
	visible := make(map[string]bool)
	for _, spec := range app.reg.VisibleSpecs() {
		visible[spec.Name] = true
	}
	if !visible["subagent_fork"] {
		t.Fatalf("subagent_fork is registered but not model-visible: %v", names)
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"subagent", `{}`},                               // missing required prompt
		{"subagent", `{"prompt":"x","extra":1}`},         // additional properties rejected
		{"send_message", `{}`},                           // missing required target/message
		{"send_message", `{"target":123,"message":"x"}`}, // target must be a string
		{"interrupt_agent", `{"agent_id":false}`},        // wrong id type
	} {
		if _, err := app.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid spawn flows through the registry and returns the child id.
	res, err := app.reg.Execute(context.Background(), "subagent", json.RawMessage(`{"description":"researcher","prompt":"do research"}`))
	if err != nil {
		t.Fatalf("subagent via registry: %v", err)
	}
	if !strings.Contains(res.Output, "started subagent spawn-1") {
		t.Fatalf("subagent output = %q, want started subagent spawn-1", res.Output)
	}
	if !hasEvent(app.log, session.EventSubagentStart) {
		t.Fatal("subagent/start event missing from the session log after subagent_spawn")
	}

	// The DSH teammate contract creates a continuable named child.
	if _, err := app.reg.Execute(context.Background(), "spawn_teammate", json.RawMessage(`{"name":"forked","description":"forked","prompt":"forked","context":"fork"}`)); err != nil {
		t.Fatalf("spawn_teammate via registry: %v", err)
	}
}

// TestRegisterSubagentExternalProviders verifies the D-GAP-4 wiring: an
// enabled external provider config is registered into the subagent Runtime
// under its tool-facing provider name (config key claude_code →
// "claude-code"), while a disabled one is never registered (D10). The command
// points at the current test binary (a stand-in CLI); registration never
// starts it.
func TestRegisterSubagentExternalProviders(t *testing.T) {
	app := makeSubagentApp(true)
	app.cfg.Subagent.ExternalProviders = map[string]config.ExternalProviderConfig{
		"codex":       {Enabled: true, Command: os.Args[0]},
		"claude_code": {Enabled: true, Command: os.Args[0]},
		"disabled":    {Enabled: false, Command: "nope"},
	}
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	got := app.subagents.ListProviders()
	for _, want := range []string{"spawn", "codex", "claude-code"} {
		if !containsStr(got, want) {
			t.Fatalf("registered providers %v lack %q", got, want)
		}
	}
	if containsStr(got, "disabled") {
		t.Fatalf("disabled external provider must not be registered (D10), got %v", got)
	}
}

// TestRegisterSubagentExternalDisabled verifies the D10 gate for external
// providers: with every external provider disabled, only the local spawn
// provider is registered.
func TestRegisterSubagentExternalDisabled(t *testing.T) {
	app := makeSubagentApp(true)
	app.cfg.Subagent.ExternalProviders = map[string]config.ExternalProviderConfig{
		"codex": {Enabled: false, Command: "codex"},
	}
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	got := app.subagents.ListProviders()
	if len(got) != 2 || !containsStr(got, "spawn") || !containsStr(got, "fork") {
		t.Fatalf("registered providers = %v, want [fork spawn] when no external provider is enabled", got)
	}
}
