// sessionruntime_test.go — Phase 2 (按会话切换): applySessionRuntime resolves a
// session's per-turn provider/model/effort, per-mode prompt builder and
// permission-tier policy (session override ?? global), and restores the base
// policy afterwards. llmFor covers the dsh ModelSelection routing (a session
// pinned to a provider talks to that provider's adapter).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

// stubProvider is a minimal llm.Provider for routing tests; Stream always
// errors so a turn can never accidentally use it.
type stubProvider struct{ id string }

func (s stubProvider) ID() string      { return s.id }
func (s stubProvider) Available() bool { return true }
func (s stubProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return nil, errors.New("stub provider")
}

// TestLLMForRouting verifies llmFor (dsh ModelSelection 对齐): a session's
// provider override resolves to that provider's adapter; an empty id and an
// unknown id fall back to the global LLM (fail-open).
func TestLLMForRouting(t *testing.T) {
	reg := llm.NewRegistry()
	global := stubProvider{id: "global"}
	openai := stubProvider{id: "openai"}
	if err := reg.Register(stubProvider{id: "deepseek-official"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(openai); err != nil {
		t.Fatal(err)
	}
	a := &app{llm: global, llmReg: reg}

	if got := a.llmFor(""); got != global {
		t.Fatalf("llmFor(\"\") = %v, want the global LLM", got)
	}
	if got := a.llmFor("openai"); got != openai {
		t.Fatalf("llmFor(openai) = %v, want the openai adapter", got)
	}
	if got := a.llmFor("nope"); got != global {
		t.Fatalf("llmFor(unknown) = %v, want the global LLM (fail-open)", got)
	}
	// No registry at all → global.
	a2 := &app{llm: global}
	if got := a2.llmFor("openai"); got != global {
		t.Fatalf("llmFor without registry = %v, want the global LLM", got)
	}
}

func TestModeToolWhitelist(t *testing.T) {
	standard := modeToolWhitelist(config.ModeStandard, []string{"read", "run_code", "read"})
	if len(standard) != 2 || standard[0] != "read" || standard[1] != "read" {
		t.Fatalf("standard tools = %#v, want run_code excluded", standard)
	}
	ptc := modeToolWhitelist(config.ModeCode, []string{"read", "run_code"})
	if len(ptc) != 1 || ptc[0] != "run_code" {
		t.Fatalf("PTC tools = %#v, want only run_code", ptc)
	}
	minimal := modeToolWhitelist(config.ModeMinimal, []string{"run_code"})
	if len(minimal) == 0 || minimal[0] == "run_code" {
		t.Fatalf("minimal tools = %#v, want fixed terminal/file tools", minimal)
	}
}

func TestApplySessionRuntime(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A registry holding exactly get_time; base policy rejects everything so a
	// "full" tier demonstrably opens the whitelist and a restore re-arms it.
	a := &app{
		cfg:    config.Config{Model: "global-model", Mode: "standard", ReasoningEffort: "high"},
		reg:    tools.New(),
		prompt: prompt.New("You are a standard agent."),
		store:  st,
	}
	a.basePolicy = tools.Policy{Enabled: []string{}} // empty → reject-all
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	a.reg.SetPolicy(a.basePolicy)

	mustSession := func(id, provider, model, mode, effort, perm string) string {
		t.Helper()
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		cfg := store.SessionConfig{Provider: provider, Model: model, AgentPreset: mode, ReasoningEffort: effort, Permission: perm}
		if err := st.SetSessionConfig(ctx, id, cfg); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// 1. A bare session (no override) falls back to the globals and receives a
	// per-turn prompt clone so DSH model/cwd variables cannot leak across sessions.
	mustSession("s-global", "", "", "", "", "")
	rt, restore := a.applySessionRuntime("s-global")
	if rt.model != "global-model" || rt.provider != "" || rt.effort != "high" || rt.prompt == a.prompt {
		t.Fatalf("global session runtime = (%q, %q, %q, %p), want (global-model, '', high, %p)", rt.model, rt.provider, rt.effort, rt.prompt, a.prompt)
	}
	restore()

	// 2. A model override is honoured.
	mustSession("s-model", "", "per-session-model", "", "", "")
	rt, restore = a.applySessionRuntime("s-model")
	if rt.model != "per-session-model" {
		t.Fatalf("model override runtime = %q, want per-session-model", rt.model)
	}
	restore()

	// 2b. A provider + effort override (dsh ModelSelection) routes the session.
	mustSession("s-prov", "openai", "gpt-4o", "", "max", "")
	rt, restore = a.applySessionRuntime("s-prov")
	if rt.provider != "openai" || rt.model != "gpt-4o" || rt.effort != "max" {
		t.Fatalf("provider session runtime = (%q, %q, %q), want (openai, gpt-4o, max)", rt.provider, rt.model, rt.effort)
	}
	restore()

	// 3. A minimal-mode session picks a different prompt builder (the mode's).
	mustSession("s-min", "", "", "minimal", "", "")
	rt, restore = a.applySessionRuntime("s-min")
	if rt.prompt == a.prompt {
		t.Fatalf("minimal session prompt must differ from the global builder")
	}
	restore()

	// 4. A full-permission session opens the whitelist, then restores it.
	mustSession("s-full", "", "", "", "", "full")
	rt, restore = a.applySessionRuntime("s-full")
	if _, err := a.reg.Execute(ctx, "get_time", json.RawMessage("{}")); err != nil {
		t.Fatalf("full tier should allow get_time: %v", err)
	}
	restore()
	if _, err := a.reg.Execute(ctx, "get_time", json.RawMessage("{}")); err == nil {
		t.Fatalf("get_time must be rejected again after restore (base whitelist empty)")
	}
	_ = rt

	// 5. readonly permission narrows the whitelist to the DSH read-only tools.
	mustSession("s-ro", "", "", "", "", "readonly")
	_, restore = a.applySessionRuntime("s-ro")
	if _, err := a.reg.Execute(ctx, "get_time", json.RawMessage("{}")); err != nil {
		t.Fatalf("readonly tier must expose safe get_time: %v", err)
	}
	restore()
}

// stubCodeRun is a fake run_code tool so a test registry can exercise the
// mode projection without the code seam.
type stubCodeRun struct{}

func (stubCodeRun) Name() string        { return "run_code" }
func (stubCodeRun) Description() string { return "stub code sandbox" }
func (stubCodeRun) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (stubCodeRun) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (stubCodeRun) Execute(context.Context, any) (string, error) { return "ok", nil }

type stubStrReplaceEditor struct{}

func (stubStrReplaceEditor) Name() string        { return "str_replace_editor" }
func (stubStrReplaceEditor) Description() string { return "stub editor" }
func (stubStrReplaceEditor) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (stubStrReplaceEditor) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (stubStrReplaceEditor) Execute(context.Context, any) (string, error) { return "ok", nil }

// recordLLM captures the model-facing tool schemas of every request, so a test
// can assert the session's mode projection on the wire.
type recordLLM struct {
	mu    sync.Mutex
	calls [][]string // tool names per Stream call
}

func (l *recordLLM) Stream(_ context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	l.mu.Lock()
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	l.calls = append(l.calls, names)
	l.mu.Unlock()
	return &turnReader{events: []llm.StreamEvent{{Kind: llm.StreamFinish, FinishReason: "stop"}}}, nil
}

func (l *recordLLM) lastTools() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return nil
	}
	return l.calls[len(l.calls)-1]
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestSessionModeWireSurface verifies the dsh mode contract end to end: a
// session's agent preset (locked at creation) owns BOTH the model-facing tool
// array sent on the wire AND the execution whitelist. Standard never sees or
// executes run_code, PTC sees and executes only run_code, minimal only its
// fixed seam.
func TestSessionModeWireSurface(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := &recordLLM{}
	a := &app{
		cfg:    config.Config{Model: "m", Mode: config.ModeStandard, ReasoningEffort: "high"},
		store:  st,
		reg:    tools.New(),
		llm:    rec,
		prompt: prompt.New("You are a test agent."),
		log:    session.New(),
	}
	for _, tt := range []tools.Tool{tools.GetTime{}, tools.ReadFile{}, stubCodeRun{}, stubStrReplaceEditor{}} {
		if err := a.reg.Register(tt); err != nil {
			t.Fatal(err)
		}
	}
	a.basePolicy = tools.Policy{Enabled: []string{"get_time", "read", "run_code", "str_replace_editor"}}
	a.reg.SetPolicy(a.basePolicy)

	mustSession := func(id, preset string) {
		t.Helper()
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSessionConfig(ctx, id, store.SessionConfig{AgentPreset: preset}); err != nil {
			t.Fatal(err)
		}
	}

	// standard: native tools, run_code neither advertised nor executable.
	mustSession("s-std", config.ModeStandard)
	rt, restore := a.applySessionRuntime("s-std")
	if err := a.newLoopFor(rt, false).Run(ctx, "hi"); err != nil {
		t.Fatalf("standard turn: %v", err)
	}
	restore()
	wire := rec.lastTools()
	if hasName(wire, "run_code") {
		t.Fatalf("standard wire tools = %v, must not contain run_code", wire)
	}
	if !hasName(wire, "get_time") || !hasName(wire, "read") {
		t.Fatalf("standard wire tools = %v, want native read-only tools", wire)
	}
	_, restore = a.applySessionRuntime("s-std")
	if _, err := a.reg.Execute(ctx, "run_code", json.RawMessage(`{}`)); err == nil {
		t.Fatal("standard session must not execute run_code")
	}
	restore()

	// PTC: only run_code is advertised and executable.
	mustSession("s-ptc", config.ModeCode)
	rt, restore = a.applySessionRuntime("s-ptc")
	if err := a.newLoopFor(rt, false).Run(ctx, "hi"); err != nil {
		t.Fatalf("PTC turn: %v", err)
	}
	restore()
	wire = rec.lastTools()
	if len(wire) != 1 || wire[0] != "run_code" {
		t.Fatalf("PTC wire tools = %v, want exactly [run_code]", wire)
	}
	_, restore = a.applySessionRuntime("s-ptc")
	if _, err := a.reg.Execute(ctx, "run_code", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("PTC session must execute run_code: %v", err)
	}
	if _, err := a.reg.Execute(ctx, "read", json.RawMessage(`{"file_path":"x"}`)); err == nil {
		t.Fatal("PTC session must not execute native tools directly")
	}
	restore()

	// minimal: only the fixed DSH shell/editor seam.
	mustSession("s-min", config.ModeMinimal)
	rt, restore = a.applySessionRuntime("s-min")
	if err := a.newLoopFor(rt, false).Run(ctx, "hi"); err != nil {
		t.Fatalf("minimal turn: %v", err)
	}
	restore()
	wire = rec.lastTools()
	if hasName(wire, "run_code") {
		t.Fatalf("minimal wire tools = %v, must not contain run_code", wire)
	}
	if !hasName(wire, "str_replace_editor") {
		t.Fatalf("minimal wire tools = %v, want str_replace_editor", wire)
	}
}
