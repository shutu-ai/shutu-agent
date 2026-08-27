package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jabing/shutu-agent/internal/compaction"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
)

type acpSummaryLLM struct{}

func (acpSummaryLLM) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return &acpSummaryReader{}, nil
}

type acpSummaryReader struct{ done bool }

func (r *acpSummaryReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	r.done = true
	return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: "summary"}, nil
}

type acpDynamicExtensionTool struct{}

func (acpDynamicExtensionTool) Name() string        { return "plugin_load" }
func (acpDynamicExtensionTool) Description() string { return "test-only dynamic extension" }
func (acpDynamicExtensionTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (acpDynamicExtensionTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (acpDynamicExtensionTool) Execute(context.Context, any) (string, error) {
	return "unexpected", nil
}

func TestACPFactoryCreatesIndependentCWDAndLogs(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "marker.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "marker.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "pa.db")
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{
		cfg:        config.Config{Mode: config.ModeMinimal},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	factory := &acpFactory{app: a}
	firstSession, err := factory.NewSession(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := factory.NewSession(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	one := firstSession.(*acpSession)
	two := secondSession.(*acpSession)
	if one.terminal != nil || two.terminal != nil {
		t.Fatal("ACP terminal must remain disabled without explicit terminal.acp_enabled")
	}
	if one.id == two.id || one.log == two.log || one.registry == two.registry {
		t.Fatal("ACP sessions must not share identity, log, or registry")
	}
	got, err := one.registry.Execute(context.Background(), "str_replace_editor", []byte(`{"command":"view","path":"marker.txt"}`))
	if err != nil || got.Output != "1\tone" {
		t.Fatalf("first cwd read = %q, err=%v", got.Output, err)
	}
	got, err = two.registry.Execute(context.Background(), "str_replace_editor", []byte(`{"command":"view","path":"marker.txt"}`))
	if err != nil || got.Output != "1\ttwo" {
		t.Fatalf("second cwd read = %q, err=%v", got.Output, err)
	}
	metas, err := st.ListSessions(context.Background())
	if err != nil || len(metas) != 2 {
		t.Fatalf("session metadata count = %d, err=%v", len(metas), err)
	}
	seen := map[string]bool{}
	for _, meta := range metas {
		seen[meta.CWD] = true
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("session cwds = %#v", seen)
	}
}

func TestACPRegistryDoesNotInheritRuntimeExtensionsOrProfiles(t *testing.T) {
	a := &app{
		cfg:        config.Config{Mode: config.ModeStandard},
		basePolicy: tools.DefaultPolicy(),
		reg:        tools.New(),
		customProviders: []customProviderProfile{{
			ID: "runtime-profile", Name: "runtime profile",
		}},
		builtinProfiles: map[string]builtinProviderProfile{
			"deepseek-official": {BaseURL: "https://profile.example.invalid", Model: "profile-model"},
		},
	}
	if err := a.reg.Register(acpDynamicExtensionTool{}); err != nil {
		t.Fatal(err)
	}
	registry, err := acpRegistry(a, "acp-test", t.TempDir(), session.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range registry.Specs() {
		if spec.Name == "plugin_load" || spec.Name == "runtime-profile" || spec.Name == "deepseek-official" {
			t.Fatalf("ACP registry inherited global runtime extension/profile %q", spec.Name)
		}
	}
}

func TestACPExplicitTerminalOwnsLifecycleAndTools(t *testing.T) {
	root := t.TempDir()
	log := session.New()
	a := &app{
		cfg: config.Config{
			Terminal: config.TerminalConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
			},
		},
		basePolicy: tools.DefaultPolicy(),
	}
	service := newACPCTerminal(a.cfg.Terminal, "acp-test", root, log)
	registry, err := acpRegistry(a, "acp-test", root, log, service, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), acpTerminalStart, []byte(`{}`)); err != nil {
		t.Fatalf("terminal_start: %v", err)
	}
	if _, err := registry.Execute(context.Background(), acpTerminalStop, []byte(`{}`)); err != nil {
		t.Fatalf("terminal_stop: %v", err)
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventTerminalStart] != 1 || types[session.EventTerminalStop] != 1 {
		t.Fatalf("terminal events = %#v", types)
	}
}

type acpFakeMCPFactory struct{ client *acpFakeMCPClient }

func (f acpFakeMCPFactory) New(context.Context, string, []string) (mcp.Client, error) {
	return f.client, nil
}

type acpFakeMCPClient struct {
	mu     sync.Mutex
	closed bool
}

func (c *acpFakeMCPClient) Start(context.Context) error { return nil }
func (c *acpFakeMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "lookup", Description: "lookup data", InputSchema: map[string]any{"type": "object"}}}, nil
}
func (c *acpFakeMCPClient) Call(context.Context, string, map[string]any) (mcp.CallResult, error) {
	return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "ok"}}}, nil
}
func (c *acpFakeMCPClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func TestACPExplicitMCPOwnsClientAndRegistry(t *testing.T) {
	root := t.TempDir()
	log := session.New()
	fake := &acpFakeMCPClient{}
	a := &app{
		cfg: config.Config{
			Mcp: config.McpConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
				Servers:    []config.McpServer{{Name: "demo", Cmd: "fake"}},
			},
		},
		basePolicy: tools.DefaultPolicy(),
		mcpFactory: acpFakeMCPFactory{client: fake},
	}
	service, err := newACPMCP(context.Background(), a, "acp-test", log)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := acpRegistry(a, "acp-test", root, log, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	got, err := registry.Execute(context.Background(), "mcp__demo__lookup", []byte(`{}`))
	if err != nil || got.Output != "ok" {
		t.Fatalf("MCP advertised tool = %q, err=%v", got.Output, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	closed := fake.closed
	fake.mu.Unlock()
	if !closed {
		t.Fatal("ACP MCP client was not closed with its session")
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventMcpCall] != 1 || types[session.EventMcpList] != 0 {
		t.Fatalf("MCP events = %#v", types)
	}
}

func TestACPExplicitSubagentOwnsRuntimeAndTools(t *testing.T) {
	log := session.New()
	a := &app{
		cfg: config.Config{
			Model: "test-model",
			Subagent: config.SubagentConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
				MaxDepth:   8,
			},
		},
		basePolicy: tools.DefaultPolicy(),
		llm:        acpSummaryLLM{},
	}
	registry, err := acpRegistry(a, "acp-test", t.TempDir(), log, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pb := prompt.New("You are an ACP subagent.")
	pb.SetTools(func() []llm.ToolSchema { return registry.Specs() })
	rt, bundle, err := newACPSubagent(a, "acp-test", log, registry, pb)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerACPSubagentTools(registry, bundle); err != nil {
		_ = rt.Close()
		t.Fatal(err)
	}
	if providers := rt.ListProviders(); len(providers) != 2 || !containsStr(providers, "spawn") || !containsStr(providers, "fork") {
		t.Fatalf("ACP providers = %v, want isolated spawn and fork providers", providers)
	}
	if _, err := registry.Execute(context.Background(), subagent.ToolSpawnName, []byte(`{"description":"summary","prompt":"summarize","run_in_background":false}`)); err != nil {
		_ = rt.Close()
		t.Fatalf("ACP subagent_spawn: %v", err)
	}
	children, err := rt.ListChildren(context.Background(), "acp-test")
	if err != nil || len(children) != 1 {
		_ = rt.Close()
		t.Fatalf("ACP children = %v, err=%v", children, err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Start(context.Background(), "spawn", subagent.StartRequest{Prompt: "after close"}); err == nil {
		t.Fatal("ACP subagent runtime accepted Start after Close")
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventSubagentStart] != 1 {
		t.Fatalf("ACP subagent events = %#v", types)
	}
}

func TestACPCompactionUsesSessionLog(t *testing.T) {
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventUserMessage, session.NewUserMessage("old question")},
		{session.EventAssistantMessage, session.NewAssistantMessage("old answer", nil, "stop")},
		{session.EventUserMessage, session.NewUserMessage("recent question")},
		{session.EventAssistantMessage, session.NewAssistantMessage("recent answer", nil, "stop")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	s := &acpSession{
		app: &app{cfg: config.Config{Compaction: config.CompactionConfig{TokenThreshold: 1}}},
		log: log,
		compaction: compaction.NewBasic(compaction.BasicOpts{
			LLM:            acpSummaryLLM{},
			TokenThreshold: 1,
			RetainTurns:    1,
		}),
	}
	if got := s.compactionPreStep()(context.Background(), ""); len(got) != 1 {
		t.Fatalf("compaction injections = %d, want 1", len(got))
	}
	types := make(map[string]int)
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	for _, typ := range []string{session.EventCompactionStart, session.EventCompactionSummary, session.EventCompactionEnd} {
		if types[typ] != 1 {
			t.Fatalf("%s count = %d, want 1", typ, types[typ])
		}
	}
	if len(log.DeriveHistory()) == 0 {
		t.Fatal("compaction removed the entire model-visible history")
	}
}
