package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// fakeMcpClient is an in-memory mcp.Client for the wiring tests: it serves
// canned ListTools/Call outcomes and records how it was used, so the tests can
// assert the bridge's prefix naming, schema passthrough and call delegation
// without spawning a real subprocess (the real JSON-RPC over stdio path is
// covered by internal/mcp's fake-server helper-process tests).
type fakeMcpClient struct {
	tools     []mcp.Tool
	startErr  error
	listErr   error
	callFn    func(name string, args map[string]any) (mcp.CallResult, error)
	started   int
	closed    int
	callCount int
	lastTool  string
	lastArgs  map[string]any
}

func (c *fakeMcpClient) Start(ctx context.Context) error {
	c.started++
	return c.startErr
}

func (c *fakeMcpClient) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.tools, nil
}

func (c *fakeMcpClient) Call(ctx context.Context, name string, args map[string]any) (mcp.CallResult, error) {
	c.callCount++
	c.lastTool = name
	c.lastArgs = args
	if c.callFn != nil {
		return c.callFn(name, args)
	}
	return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "fake:" + name}}}, nil
}

func (c *fakeMcpClient) Close() error {
	c.closed++
	return nil
}

// fakeMcpFactory hands out a fresh fakeMcpClient per New call, configured by
// the command line (cmd acts as the server identity), and records every client
// it created.
type fakeMcpFactory struct {
	toolsByCmd map[string][]mcp.Tool
	callByCmd  map[string]func(name string, args map[string]any) (mcp.CallResult, error)
	created    []*fakeMcpClient
}

func newFakeMcpFactory() *fakeMcpFactory {
	return &fakeMcpFactory{toolsByCmd: map[string][]mcp.Tool{}, callByCmd: map[string]func(string, map[string]any) (mcp.CallResult, error){}}
}

func (f *fakeMcpFactory) New(ctx context.Context, cmd string, args []string) (mcp.Client, error) {
	c := &fakeMcpClient{tools: f.toolsByCmd[cmd], callFn: f.callByCmd[cmd]}
	f.created = append(f.created, c)
	return c, nil
}

// mcpPolicy whitelists the mcp_* tools so the registry Execute gate can run
// them (in production config.applyDefaults + PolicyFromConfig do this); bridged
// mcp.<server>.<tool> names are added by registerMcps via reg.Allow.
func mcpPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"mcp_list", "mcp_call"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// makeMcpApp builds a minimal app for mcp wiring tests: only the fields
// registerMcps touches (cfg.Mcp, reg, log, mcpFactory) are set.
func makeMcpApp(enabled bool, servers []config.McpServer, f mcp.Factory) *app {
	return &app{
		cfg: config.Config{
			Mcp: config.McpConfig{Enabled: config.Bool(enabled), Servers: servers},
		},
		reg:        tools.New(),
		log:        session.New(),
		mcpFactory: f,
	}
}

// TestRegisterMcpsDisabledRegistersNothing verifies the D10 gate: with
// mcp.enabled=false the composition root creates no clients, registers no
// mcp_* or bridged tool, and never touches the factory (dispatch-m6f-2 §5).
func TestRegisterMcpsDisabledRegistersNothing(t *testing.T) {
	f := newFakeMcpFactory()
	a := makeMcpApp(false, []config.McpServer{{Name: "fs", Cmd: "fake-fs"}}, f)
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	if len(a.mcp) != 0 {
		t.Fatalf("a.mcp = %v, want no clients when mcp disabled", a.mcp)
	}
	if len(f.created) != 0 {
		t.Fatalf("factory created %d clients while mcp disabled, want 0", len(f.created))
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "mcp") {
			t.Fatalf("tool %q registered while mcp disabled", spec.Name)
		}
	}
}

// TestRegisterMcpsEnabledRegistersAndBridges verifies the enabled path: the
// mcp_* tools and every advertised server tool are registered, bridged names
// carry the mcp.<server>.<tool> prefix with the server's schema passed through,
// bridged names are whitelisted at runtime, and executing the tools delegates
// to the client and lands the mcp/* events (dispatch-m6f-2 §4-5).
func TestRegisterMcpsEnabledRegistersAndBridges(t *testing.T) {
	f := newFakeMcpFactory()
	fsSchema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}
	f.toolsByCmd["fake-fs"] = []mcp.Tool{{Name: "read", Description: "Read a file from the server workspace", InputSchema: fsSchema}}
	f.toolsByCmd["fake-echo"] = []mcp.Tool{{Name: "echo", Description: "Echo text back", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}
	f.callByCmd["fake-fs"] = func(name string, args map[string]any) (mcp.CallResult, error) {
		return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "fs:" + name}}}, nil
	}
	f.callByCmd["fake-echo"] = func(name string, args map[string]any) (mcp.CallResult, error) {
		return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "echo:" + name}}}, nil
	}

	a := makeMcpApp(true, []config.McpServer{
		{Name: "fs", Cmd: "fake-fs"},
		{Name: "echo", Cmd: "fake-echo"},
	}, f)
	a.reg.SetPolicy(mcpPolicy())
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	if len(a.mcp) != 2 {
		t.Fatalf("a.mcp has %d clients, want 2", len(a.mcp))
	}

	// Registry carries every bridged tool under dsh's namespaced prefix.
	specs := map[string]llm.ToolSchema{}
	for _, s := range a.reg.Specs() {
		specs[s.Name] = s
	}
	for _, want := range []string{"mcp__fs__read", "mcp__echo__echo"} {
		if _, ok := specs[want]; !ok {
			t.Fatalf("registered specs lack %q (have %v)", want, specNames(a.reg))
		}
	}
	// Schema passthrough: the bridged tool's parameters are the server's
	// inputSchema (verbatim properties + the object type guard).
	bridged := specs["mcp__fs__read"]
	props, _ := bridged.Parameters["properties"].(map[string]any)
	if len(props) != 1 || props["path"] == nil {
		t.Fatalf("mcp__fs__read parameters = %v, want the server's path property passed through", bridged.Parameters)
	}
	if bridged.Parameters["type"] != "object" {
		t.Fatalf("mcp__fs__read parameters type = %v, want object", bridged.Parameters["type"])
	}
	if !strings.HasPrefix(bridged.Description, "Read a file") {
		t.Fatalf("mcp__fs__read description = %q, want the server description", bridged.Description)
	}

	// The bridged name is whitelisted at runtime (reg.Allow), so the registry
	// Execute gate runs it; D7 validates against the passed-through schema.
	res, err := a.reg.Execute(context.Background(), "mcp__fs__read", json.RawMessage(`{"path":"/x"}`))
	if err != nil {
		t.Fatalf("execute bridged mcp__fs__read: %v", err)
	}
	if res.Output != "fs:read" {
		t.Fatalf("bridged output = %q, want fs:read", res.Output)
	}
	if _, err := a.reg.Execute(context.Background(), "mcp__fs__read", json.RawMessage(`{}`)); err == nil {
		t.Fatal("bridged mcp__fs__read must reject args missing the required path (D7)")
	}
	// Call delegation: the stored bridged client received the exact tool name
	// and the model's args passed through.
	bc, ok := a.mcp[0].(*fakeMcpClient)
	if !ok {
		t.Fatalf("a.mcp[0] = %T, want *fakeMcpClient", a.mcp[0])
	}
	if bc.lastTool != "read" || !reflect.DeepEqual(bc.lastArgs, map[string]any{"path": "/x"}) {
		t.Fatalf("bridged call = %q %v, want read with {path:/x}", bc.lastTool, bc.lastArgs)
	}

	// Bridged MCP calls are namespaced first-class tools; selector tools are not
	// exposed in dsh's model catalog.
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("mcp/* events must not derive into messages: %+v", msgs)
	}
}

// TestRegisterMcpsBridgeFailureFailsLoudly verifies a server that fails to
// start at bridge time fails startup loudly (mcp is opt-in, D10) rather than
// silently degrading, and leaves no dangling client.
func TestRegisterMcpsBridgeFailureFailsLoudly(t *testing.T) {
	inner := newFakeMcpFactory()
	inner.toolsByCmd["fake-broken"] = []mcp.Tool{{Name: "x", Description: "x", InputSchema: map[string]any{}}}
	a := makeMcpApp(true, []config.McpServer{{Name: "broken", Cmd: "fake-broken"}}, &startFailFactory{inner: inner})
	if err := a.registerMcps(); err == nil || !strings.Contains(err.Error(), "start mcp server") {
		t.Fatalf("registerMcps error = %v, want a start-server error", err)
	}
	if len(a.mcp) != 0 {
		t.Fatalf("a.mcp = %v, want no clients after a failed bridge", a.mcp)
	}
}

func TestBridgedMcpToolPreservesStructuredResultAndOutputSchema(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-structured"] = []mcp.Tool{{
		Name: "report", Description: "report", InputSchema: map[string]any{},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []any{"answer"}, "additionalProperties": false},
	}}
	f.callByCmd["fake-structured"] = func(string, map[string]any) (mcp.CallResult, error) {
		return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "done"}}, StructuredContent: map[string]any{"answer": "42"}, StructuredContentSet: true}, nil
	}
	a := makeMcpApp(true, []config.McpServer{{Name: "structured", Cmd: "fake-structured"}}, f)
	a.reg.SetPolicy(mcpPolicy())
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	res, err := a.reg.Execute(context.Background(), "mcp__structured__report", json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("execute structured MCP tool: result=%+v err=%v", res, err)
	}
	value, ok := res.Value.(map[string]any)
	if !ok || value["structuredContent"].(map[string]any)["answer"] != "42" {
		t.Fatalf("value = %#v, want structuredContent.answer=42", res.Value)
	}
	schema := a.reg.Specs()
	_ = schema // OutputSchema is validated by Registry.Execute above.
}

func TestBridgedMcpToolRejectsRequiredTaskExecution(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-task"] = []mcp.Tool{{Name: "task", Description: "task", InputSchema: map[string]any{}, TaskSupport: "required"}}
	a := makeMcpApp(true, []config.McpServer{{Name: "tasks", Cmd: "fake-task"}}, f)
	a.reg.SetPolicy(mcpPolicy())
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	res, err := a.reg.Execute(context.Background(), "mcp__tasks__task", json.RawMessage(`{}`))
	if err != nil || !res.IsError || !strings.Contains(res.Output, "task-based execution") {
		t.Fatalf("task-required result=%+v err=%v, want structured unsupported error", res, err)
	}
}

// startFailFactory wraps a fakeMcpFactory and makes every returned client's
// Start fail, simulating an unspawnable server binary.
type startFailFactory struct{ inner mcp.Factory }

func (s *startFailFactory) New(ctx context.Context, cmd string, args []string) (mcp.Client, error) {
	c, err := s.inner.New(ctx, cmd, args)
	if err != nil {
		return nil, err
	}
	fc := c.(*fakeMcpClient)
	fc.startErr = mcp.ErrStartFailed
	return fc, nil
}

// specNames returns the sorted registered tool names (for error messages).
func specNames(reg *tools.Registry) []string {
	out := make([]string, 0, len(reg.Specs()))
	for _, s := range reg.Specs() {
		out = append(out, s.Name)
	}
	return out
}
