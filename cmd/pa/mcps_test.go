package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// fakeMcpClient is an in-memory mcp.Client for the wiring tests: it serves
// canned ListTools/Call outcomes and records how it was used, so the tests can
// assert the bridge's prefix naming, schema passthrough and call delegation
// without spawning a real subprocess (the real JSON-RPC over stdio path is
// covered by internal/mcp's fake-server helper-process tests).
type fakeMcpClient struct {
	tools          []mcp.Tool
	startErr       error
	listErr        error
	callFn         func(name string, args map[string]any) (mcp.CallResult, error)
	started        int
	closed         int
	callCount      int
	lastTool       string
	lastArgs       map[string]any
	connectionLost func(error)
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

func (c *fakeMcpClient) SetConnectionLostHandler(handler func(error)) { c.connectionLost = handler }

func (c *fakeMcpClient) lose(err error) {
	if c.connectionLost != nil {
		c.connectionLost(err)
	}
}

// fakeMcpFactory hands out a fresh fakeMcpClient per New call, configured by
// the command line (cmd acts as the server identity), and records every client
// it created.
type fakeMcpFactory struct {
	toolsByCmd       map[string][]mcp.Tool
	callByCmd        map[string]func(name string, args map[string]any) (mcp.CallResult, error)
	startErrByCreate []error
	created          []*fakeMcpClient
}

func newFakeMcpFactory() *fakeMcpFactory {
	return &fakeMcpFactory{toolsByCmd: map[string][]mcp.Tool{}, callByCmd: map[string]func(string, map[string]any) (mcp.CallResult, error){}}
}

func (f *fakeMcpFactory) New(ctx context.Context, cmd string, args []string) (mcp.Client, error) {
	c := &fakeMcpClient{tools: f.toolsByCmd[cmd], callFn: f.callByCmd[cmd]}
	if n := len(f.created); n < len(f.startErrByCreate) {
		c.startErr = f.startErrByCreate[n]
	}
	f.created = append(f.created, c)
	return c, nil
}

func TestBridgedMcpRequiredTaskSupportFailsBeforeTransportCall(t *testing.T) {
	client := &fakeMcpClient{}
	tool := bridgedMcpTool{
		client: client, name: "mcp.test.task-only", tool: "task-only",
		schema: map[string]any{"type": "object"}, taskSupport: "required",
	}
	_, err := tool.ExecuteResult(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "MCP task-based execution is not supported") {
		t.Fatalf("required taskSupport error = %v", err)
	}
	if client.callCount != 0 {
		t.Fatalf("transport calls = %d, want rejection before tools/call", client.callCount)
	}
}

// TestBridgedMCPRequiredTaskRejectsRealHTTPBeforeSideEffect proves the same
// pre-transport contract against a real Streamable HTTP server. The required
// task declaration is rejected after discovery but before tools/call, so no
// external side-effect request can reach the protocol peer.
func TestBridgedMCPRequiredTaskRejectsRealHTTPBeforeSideEffect(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		methods = append(methods, req.Method)
		if req.Method == "tools/call" {
			calls++
		}
		mu.Unlock()
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "required-task-session")
			writeMCPHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": "2024-11-05"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeMCPHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{map[string]any{
				"name": "external-task", "inputSchema": map[string]any{"type": "object"},
				"execution": map[string]any{"taskSupport": "required"},
			}}})
		case "tools/call":
			http.Error(w, "external side effect must not be called", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := mcp.NewStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].TaskSupport != "required" {
		t.Fatalf("discovery = %#v, %v; want one required-task tool", tools, err)
	}
	tool := bridgedMcpTool{
		client: client, name: "mcp.required.external-task", tool: "external-task",
		schema: map[string]any{"type": "object"}, taskSupport: tools[0].TaskSupport,
	}
	_, err = tool.ExecuteResult(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "MCP task-based execution is not supported") {
		t.Fatalf("required task error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("tools/call requests = %d, want 0 before rejection", calls)
	}
	found := false
	for _, method := range methods {
		if method == "tools/call" {
			found = true
		}
	}
	if found {
		t.Fatal("tools/call reached the real HTTP transport")
	}
}

func writeMCPHTTPRPC(t *testing.T, w http.ResponseWriter, id *int64, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	}); err != nil {
		t.Errorf("encode MCP HTTP response: %v", err)
	}
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

// TestRegisterMcpsUsesConfigDerivedSelectorPolicy verifies the production
// composition path: config.Load supplies the normalized mcp allowlist and
// PolicyFromConfig installs it before registerMcps. The test intentionally
// does not call SetPolicy(mcpPolicy()), so a missing config allowlist fails at
// the same Registry admission boundary as a real process.
func TestRegisterMcpsUsesConfigDerivedSelectorPolicy(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	cfg.Mcp.Servers = []config.McpServer{{Name: "demo", Cmd: "fake-demo"}}
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-demo"] = []mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	a := &app{
		cfg:        cfg,
		reg:        tools.New(),
		log:        session.New(),
		mcpFactory: f,
	}
	a.reg.SetPolicy(tools.PolicyFromConfig(cfg.Tools, cfg.DataDir))
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	for _, name := range []string{"mcp_list", "mcp_call"} {
		if !a.reg.Policy().Allows(name) {
			t.Fatalf("config-derived policy rejects %q: %+v", name, a.reg.Policy())
		}
		found := false
		for _, spec := range a.reg.VisibleSpecs() {
			if spec.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q is registered but not visible: %v", name, specNames(a.reg))
		}
	}
	if _, err := a.reg.Execute(context.Background(), "mcp_list", json.RawMessage(`{"server":"demo"}`)); err != nil {
		t.Fatalf("mcp_list through config-derived production policy: %v", err)
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
	for _, want := range []string{"mcp_list", "mcp_call"} {
		if _, ok := specs[want]; !ok {
			t.Fatalf("enabled MCP registry lacks dynamic selector %q (have %v)", want, specNames(a.reg))
		}
	}
	entries := map[string]tools.CatalogEntry{}
	for _, entry := range a.reg.Catalog() {
		entries[entry.Name] = entry
	}
	for name, want := range map[string]tools.RegistrationInfo{
		"mcp_list":        {Owner: "mcp", Plugin: "builtin-mcp"},
		"mcp_call":        {Owner: "mcp", Plugin: "builtin-mcp"},
		"mcp__fs__read":   {Owner: "mcp:fs", Plugin: "mcp"},
		"mcp__echo__echo": {Owner: "mcp:echo", Plugin: "mcp"},
	} {
		entry, ok := entries[name]
		if !ok {
			t.Fatalf("catalog lacks %q", name)
		}
		if entry.Registration.Owner != want.Owner || entry.Registration.Plugin != want.Plugin || entry.Provenance != "plugin" {
			t.Fatalf("%q catalog ownership = %+v/%q, want %+v/plugin", name, entry.Registration, entry.Provenance, want)
		}
		if !entry.Cancellable {
			t.Fatalf("%q must declare cooperative cancellation", name)
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
	// Bridged MCP calls are namespaced first-class tools; selector tools are not
	// exposed in dsh's model catalog.
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("mcp/* events must not derive into messages: %+v", msgs)
	}
	mcpCalls := 0
	for _, event := range a.log.Events() {
		if event.Type == session.EventMcpCall {
			mcpCalls++
		}
	}
	if mcpCalls != 1 {
		t.Fatalf("static MCP bridge mcp/call events = %d, want exactly one", mcpCalls)
	}
}

// TestRegisterMcpsBridgeFailureFailsLoudly verifies a server that fails to
// start at bridge time fails startup loudly (mcp is opt-in, D10) rather than
// silently degrading, and leaves no dangling client.
func TestRegisterMcpsBridgeFailureFailsLoudly(t *testing.T) {
	inner := newFakeMcpFactory()
	inner.toolsByCmd["fake-broken"] = []mcp.Tool{{Name: "x", Description: "x", InputSchema: map[string]any{}}}
	a := makeMcpApp(true, []config.McpServer{{Name: "broken", Cmd: "fake-broken", FailOnStartupError: true}}, &startFailFactory{inner: inner})
	if err := a.registerMcps(); err == nil || !strings.Contains(err.Error(), "start mcp server") {
		t.Fatalf("registerMcps error = %v, want a start-server error", err)
	}
	if len(a.mcp) != 0 {
		t.Fatalf("a.mcp = %v, want no clients after a failed bridge", a.mcp)
	}
}

func TestRegisterMcpsStartupFailureIsRecoverableByDefault(t *testing.T) {
	inner := newFakeMcpFactory()
	a := makeMcpApp(true, []config.McpServer{{Name: "temporarily-down", Cmd: "fake-down"}}, &startFailFactory{inner: inner})
	if err := a.registerMcps(); err != nil {
		t.Fatalf("default recoverable MCP startup failure: %v", err)
	}
	// The fake now exposes the connection-lost seam, so the bridge promotes it
	// to the same supervised client used by production stdio transports. The
	// initial failure may report no tools, but the supervisor remains owned by
	// the composition root for retry and shutdown.
	if len(a.mcp) != 1 {
		t.Fatalf("recovering MCP clients = %d, want the supervised wrapper retained", len(a.mcp))
	}
	defer func() { _ = a.mcp[0].Close() }()
	if _, ok := a.reg.Registration("mcp_list"); !ok {
		t.Fatal("dynamic mcp_list tool was lost after optional server failure")
	}
}

// TestRegisterMcpsRetainsInitialRecoveryAndPublishesAfterRetry aligns the
// composition layer with the reference supervisor: a transient initial-start
// failure reports no tools yet, but retains the supervised client for retry and
// shutdown. The reconnected callback publishes one complete generation after a
// fresh client succeeds.
func TestRegisterMcpsRetainsInitialRecoveryAndPublishesAfterRetry(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-recover"] = []mcp.Tool{{Name: "echo", Description: "echo", InputSchema: map[string]any{}}}
	a := makeMcpApp(true, []config.McpServer{{Name: "recover", Cmd: "fake-recover"}}, &flakyStartFactory{inner: f, failures: 1})
	a.cfg.Mcp.ReconnectEnabled = config.Bool(true)
	a.cfg.Mcp.ReconnectInitialDelayMS = 1
	a.cfg.Mcp.ReconnectMaxDelayMS = 1
	a.cfg.Mcp.ReconnectMaxAttempts = 3
	if err := a.registerMcps(); err != nil {
		t.Fatalf("recoverable initial MCP failure: %v", err)
	}
	if len(a.mcp) != 1 {
		t.Fatalf("recovering MCP clients = %d, want the supervised wrapper retained", len(a.mcp))
	}
	if _, ok := a.reg.Registration("mcp__recover__echo"); ok {
		t.Fatal("failed initial generation published a tool")
	}
	defer func() { _ = a.mcp[0].Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.reg.Registration("mcp__recover__echo"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("MCP initial-start retry did not publish the replacement generation")
}

func TestRegisterMcpsWithdrawsToolsAfterReconnectBudgetExhaustion(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-lost"] = []mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	f.startErrByCreate = []error{nil, mcp.ErrStartFailed}
	a := makeMcpApp(true, []config.McpServer{{Name: "lost", Cmd: "fake-lost"}}, f)
	a.cfg.Mcp.ReconnectEnabled = config.Bool(true)
	a.cfg.Mcp.ReconnectInitialDelayMS = 1
	a.cfg.Mcp.ReconnectMaxDelayMS = 1
	a.cfg.Mcp.ReconnectMaxAttempts = 1
	if err := a.registerMcps(); err != nil {
		t.Fatalf("registerMcps: %v", err)
	}
	if _, ok := a.reg.Registration("mcp__lost__echo"); !ok {
		t.Fatal("initial MCP tool was not published")
	}
	f.created[0].lose(mcp.ErrConnection)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.reg.Registration("mcp__lost__echo"); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stale MCP tool remained registered after reconnect exhaustion")
}

func TestWebMCPStatusUsesServerIdentityNotConnectionOrder(t *testing.T) {
	connected := &fakeMcpClient{}
	a := &app{
		cfg: config.Config{Mcp: config.McpConfig{Servers: []config.McpServer{
			{Name: "down", Cmd: "down"}, {Name: "up", Cmd: "up"},
		}}},
		mcp:         []mcp.Client{connected},
		mcpByServer: map[string]mcp.Client{"up": connected},
	}
	rows := a.webMCPServers()
	if len(rows) != 2 {
		t.Fatalf("MCP rows = %#v", rows)
	}
	if rows[0]["connected"] != false || rows[1]["connected"] != true {
		t.Fatalf("MCP connection status = %#v, want down=false/up=true", rows)
	}
}

func TestRegisterMcpsRollsBackPartialGenerationOnRegistryConflict(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-conflict"] = []mcp.Tool{
		{Name: "free", Description: "free", InputSchema: map[string]any{}},
		{Name: "taken", Description: "taken", InputSchema: map[string]any{}},
	}
	a := makeMcpApp(true, []config.McpServer{{Name: "conflict", Cmd: "fake-conflict", FailOnStartupError: true}}, f)
	if err := a.reg.Register(bridgedMcpTool{name: "mcp__conflict__taken", schema: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.registerMcps(); err == nil {
		t.Fatal("registry conflict was accepted")
	}
	if _, ok := a.reg.Registration("mcp__conflict__free"); ok {
		t.Fatal("partial MCP generation remained registered")
	}
	if len(a.mcp) != 0 || len(f.created) != 1 || f.created[0].closed != 1 {
		t.Fatalf("rollback clients=%d created=%d closed=%d, want no live client and one close", len(a.mcp), len(f.created), f.created[0].closed)
	}
}

func TestRegisterMcpsRejectsDuplicateAdvertisedToolGeneration(t *testing.T) {
	f := newFakeMcpFactory()
	f.toolsByCmd["fake-duplicate"] = []mcp.Tool{
		{Name: "same", InputSchema: map[string]any{}},
		{Name: "same", InputSchema: map[string]any{}},
	}
	a := makeMcpApp(true, []config.McpServer{{Name: "duplicate", Cmd: "fake-duplicate", FailOnStartupError: true}}, f)
	if err := a.registerMcps(); err == nil || !strings.Contains(err.Error(), "advertised tools") {
		t.Fatalf("duplicate generation error = %v", err)
	}
	if len(a.mcp) != 0 || len(f.created) != 1 || f.created[0].closed != 1 {
		t.Fatalf("duplicate rollback clients=%d created=%d closed=%d", len(a.mcp), len(f.created), f.created[0].closed)
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

func TestBridgedMcpToolUsesAgentRuntimeEventSink(t *testing.T) {
	target := session.New()
	client := &fakeMcpClient{}
	tool := bridgedMcpTool{
		client: client, name: "mcp__agent__echo", tool: "echo", schema: map[string]any{},
		onEvent: func(string, any) error { return errors.New("legacy sink must not be used") },
	}
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "agent-session",
		Emit: func(typ string, data any) error {
			_, err := target.Append(typ, data)
			return err
		},
	})
	if _, err := tool.ExecuteResult(ctx, map[string]any{}); err != nil {
		t.Fatalf("agent bridged MCP call: %v", err)
	}
	if len(target.Events()) != 1 || target.Events()[0].Type != session.EventMcpCall {
		t.Fatalf("agent runtime MCP events = %+v, want one mcp/call", target.Events())
	}
}

func TestMcpRichImageProjectionRequiresRouteAndAdmitsAsBatch(t *testing.T) {
	root := t.TempDir()
	store, err := attachment.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	items := []any{
		map[string]any{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
		map[string]any{"type": "text", "text": "between"},
		map[string]any{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
	}
	denied := projectMcpContentWithAdmission(context.Background(), "demo", items, store, 1024, func(context.Context) error {
		return errors.New("the current model route does not declare image input")
	})
	for _, block := range denied {
		if block.Kind == llm.BlockImage {
			t.Fatal("route-denied MCP images must not enter model content")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("route-denied projection wrote %d files", len(entries))
	}
	accepted := projectMcpContentWithAdmission(context.Background(), "demo", items, store, 1024, func(context.Context) error { return nil })
	imageCount := 0
	for _, block := range accepted {
		if block.Kind == llm.BlockImage {
			imageCount++
		}
	}
	if imageCount != 2 {
		t.Fatalf("accepted MCP image blocks = %d, want 2: %+v", imageCount, accepted)
	}
	entries, err = os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("accepted batch wrote %d files, want one content-addressed file", len(entries))
	}
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

func TestBridgedMcpToolDoesNotReplayLostConnection(t *testing.T) {
	client := &fakeMcpClient{}
	var calls int
	client.callFn = func(name string, args map[string]any) (mcp.CallResult, error) {
		calls++
		return mcp.CallResult{}, mcp.ErrConnection
	}
	tool := bridgedMcpTool{client: client, name: "mcp__demo__echo", tool: "echo", schema: map[string]any{}}
	result, err := tool.ExecuteResult(context.Background(), map[string]any{})
	if err == nil || !errors.Is(err, mcp.ErrConnection) {
		t.Fatalf("lost connection error = %v, want ErrConnection", err)
	}
	if result.Output != "" || client.started != 0 || calls != 1 {
		t.Fatalf("result=%+v started=%d calls=%d, want one non-replayed call", result, client.started, calls)
	}
}

func TestMCPReconnectResyncReplacesPublishedGeneration(t *testing.T) {
	client := &fakeMcpClient{tools: []mcp.Tool{{Name: "new", InputSchema: map[string]any{"type": "object"}}}}
	a := &app{
		reg:          tools.New(),
		mcpToolNames: map[string][]string{"demo": {"mcp__demo__old"}},
	}
	if err := a.reg.RegisterWithInfo(bridgedMcpTool{name: "mcp__demo__old", tool: "old", client: client, schema: map[string]any{}}, mcpToolRegistration("demo")); err != nil {
		t.Fatal(err)
	}
	if err := a.resyncMCPServer(context.Background(), "demo", client); err != nil {
		t.Fatalf("resyncMCPServer: %v", err)
	}
	if _, ok := a.reg.Registration("mcp__demo__old"); ok {
		t.Fatal("old MCP generation remained registered")
	}
	if _, ok := a.reg.Registration("mcp__demo__new"); !ok {
		t.Fatal("new MCP generation was not registered")
	}
}

func TestMCPReconnectResyncKeepsPreviousGenerationOnConflict(t *testing.T) {
	client := &fakeMcpClient{tools: []mcp.Tool{
		{Name: "fresh", InputSchema: map[string]any{"type": "object"}},
		{Name: "taken", InputSchema: map[string]any{"type": "object"}},
	}}
	a := &app{
		reg:          tools.New(),
		mcpToolNames: map[string][]string{"demo": {"mcp__demo__stable"}},
	}
	if err := a.reg.RegisterWithInfo(bridgedMcpTool{name: "mcp__demo__stable", tool: "stable", client: client, schema: map[string]any{}}, mcpToolRegistration("demo")); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.Register(bridgedMcpTool{name: "mcp__demo__taken", tool: "taken", client: client, schema: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.resyncMCPServer(context.Background(), "demo", client); err == nil {
		t.Fatal("registry conflict was accepted")
	}
	if _, ok := a.reg.Registration("mcp__demo__stable"); !ok {
		t.Fatal("previous MCP generation was withdrawn after failed resync")
	}
	if _, ok := a.reg.Registration("mcp__demo__fresh"); ok {
		t.Fatal("partial replacement generation remained registered")
	}
	if got := a.mcpToolNames["demo"]; len(got) != 1 || got[0] != "mcp__demo__stable" {
		t.Fatalf("published MCP names = %#v, want previous generation", got)
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

// flakyStartFactory fails only the configured number of initial starts, unlike
// startFailFactory, which models a permanently unavailable server.
type flakyStartFactory struct {
	inner    mcp.Factory
	failures int
	mu       sync.Mutex
}

func (s *flakyStartFactory) New(ctx context.Context, cmd string, args []string) (mcp.Client, error) {
	c, err := s.inner.New(ctx, cmd, args)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures > 0 {
		s.failures--
		c.(*fakeMcpClient).startErr = mcp.ErrStartFailed
	}
	return c, nil
}

// specNames returns the sorted registered tool names (for error messages).
func specNames(reg *tools.Registry) []string {
	out := make([]string, 0, len(reg.Specs()))
	for _, s := range reg.Specs() {
		out = append(out, s.Name)
	}
	return out
}
