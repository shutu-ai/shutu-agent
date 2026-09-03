// ralph_test.go — the GAP-2 wiring tests (docs/dispatch-gap-2.md §6):
// registerRalph D10 gate + tool registration + whitelist, and the ralph E2E
// through the subagent spawn provider with a scripted LLM. The fakes mirror the
// subagent_test/eval_test pattern: makeRalphApp builds a minimal app, the
// whitelist policy lets the registry Execute the ralph tool, and ralphFakeLLM
// is a scripted llm.LLM (照 subagent spawn_test 的 scriptedLLM) that answers
// "DONE: 完成" or "BLOCKED: 缺凭证".
package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/ralph"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// ralphFakeLLM is a scripted llm.LLM: Stream returns a fixed single-delta text
// stream (the child's final answer, e.g. "DONE: 完成" or "BLOCKED: 缺凭证").
type ralphFakeLLM struct {
	text string
}

func (f ralphFakeLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return &ralphFakeReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: f.text},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

type ralphFakeReader struct {
	events []llm.StreamEvent
	i      int
}

func (r *ralphFakeReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

// makeRalphApp builds a minimal app for registerRalph tests: only the fields
// registerRalph and the registerSubagent it depends on touch (cfg.Ralph,
// cfg.Subagent, cfg.Model, reg, log, llm, prompt, currentID) are set. The
// subagent config is always enabled so registerSubagent can build the Runtime
// registerRalph spawns through.
func makeRalphApp(enabled bool, fakeLLM llm.LLM) *app {
	return &app{
		cfg: config.Config{
			Model:    "m",
			Subagent: config.SubagentConfig{Enabled: config.Bool(true), MaxDepth: 8, DefaultProvider: "spawn"},
			Ralph:    config.RalphConfig{Enabled: config.Bool(enabled)},
		},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-test",
		llm:       fakeLLM,
		prompt:    prompt.New("You are a subagent."),
	}
}

// ralphPolicy whitelists the ralph tool so registry Execute can run it (in
// production config.applyDefaults + PolicyFromConfig do this).
func ralphPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"ralph"},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

// TestRegisterRalphDisabledRegistersNothing verifies the D10 gate: with
// ralph.enabled=false the composition root registers no ralph tool
// (dispatch-gap-2 §6).
func TestRegisterRalphDisabledRegistersNothing(t *testing.T) {
	app := makeRalphApp(false, nil)
	if err := app.registerRalph(); err != nil {
		t.Fatalf("registerRalph: %v", err)
	}
	for _, spec := range app.reg.Specs() {
		if spec.Name == ralph.RalphToolName {
			t.Fatalf("ralph tool %q registered while ralph disabled", spec.Name)
		}
	}
}

// TestRegisterRalphEnabled verifies the enabled path: the ralph tool is
// registered and whitelisted (Execute succeeds through the registry).
func TestRegisterRalphEnabled(t *testing.T) {
	app := makeRalphApp(true, ralphFakeLLM{text: `{"status":"complete","summary":"完成","evidence":["verified"],"nextSteps":[],"blocker":""}`})
	app.reg.SetPolicy(ralphPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerRalph(); err != nil {
		t.Fatalf("registerRalph: %v", err)
	}
	names := make([]string, 0, len(app.reg.Specs()))
	for _, s := range app.reg.Specs() {
		names = append(names, s.Name)
	}
	if !containsStr(names, ralph.RalphToolName) {
		t.Fatalf("registered tools %v lack %q", names, ralph.RalphToolName)
	}
	// Whitelist: Execute succeeds only when ralph is both registered and
	// whitelisted (the D10-gated whitelist in production comes from
	// config.applyDefaults).
	if _, err := app.reg.Execute(context.Background(), ralph.RalphToolName, json.RawMessage(`{"objective":"x"}`)); err != nil {
		t.Fatalf("ralph must be registered and whitelisted when enabled: %v", err)
	}
}

// TestRalphRunE2E drives a full ralph run through the registry: the scripted
// child answers "DONE: 完成", the report renders the done outcome and the
// deliverable, and the ralph/run event lands in the session log (D3).
func TestRalphRunE2E(t *testing.T) {
	app := makeRalphApp(true, ralphFakeLLM{text: `{"status":"complete","summary":"完成","evidence":["verified"],"nextSteps":[],"blocker":""}`})
	app.reg.SetPolicy(ralphPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerRalph(); err != nil {
		t.Fatalf("registerRalph: %v", err)
	}
	res, err := app.reg.Execute(context.Background(), ralph.RalphToolName, json.RawMessage(`{"objective":"x"}`))
	if err != nil {
		t.Fatalf("ralph via registry: %v", err)
	}
	if !strings.Contains(res.Output, "complete") || !strings.Contains(res.Output, "完成") {
		t.Fatalf("ralph output = %q, want structured complete result", res.Output)
	}
	if !hasEvent(app.log, session.EventRalphRun) {
		t.Fatal("ralph/run event missing from the session log after ralph Execute")
	}
}

// TestRalphRunBlocked drives a ralph run whose child answers "BLOCKED: 缺凭证":
// the report renders the blocked outcome and the block reason.
func TestRalphRunBlocked(t *testing.T) {
	app := makeRalphApp(true, ralphFakeLLM{text: `{"status":"blocked","summary":"无法继续","evidence":[],"nextSteps":[],"blocker":"缺凭证"}`})
	app.reg.SetPolicy(ralphPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerRalph(); err != nil {
		t.Fatalf("registerRalph: %v", err)
	}
	res, err := app.reg.Execute(context.Background(), ralph.RalphToolName, json.RawMessage(`{"objective":"x"}`))
	if err != nil {
		t.Fatalf("ralph via registry: %v", err)
	}
	if !strings.Contains(res.Output, "blocked") || !strings.Contains(res.Output, "缺凭证") {
		t.Fatalf("ralph output = %q, want structured blocked result", res.Output)
	}
}
