package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// helperFactory is a Factory whose New returns a stdioClient pointed at this
// test binary running as the fake MCP server (the same helper-process pattern
// as newFakeClient in mcp_test.go, reached through the Factory seam the mcp_*
// tools use). A 5s per-request bound makes a broken test fail fast.
type helperFactory struct{ mode string }

func (f helperFactory) New(ctx context.Context, cmd string, args []string) (Client, error) {
	c := newStdioClient(cmd, args)
	c.env = []string{
		helperServerEnv + "=1",
		helperServerModeEnv + "=" + f.mode,
	}
	c.timeout = 5 * time.Second
	return c, nil
}

type reconnectTestClient struct {
	startCalls int
	listCalls  int
	callCalls  int
	err        error
	tools      []Tool
}

func (c *reconnectTestClient) Start(context.Context) error {
	c.startCalls++
	return nil
}

func (c *reconnectTestClient) ListTools(context.Context) ([]Tool, error) {
	c.listCalls++
	return c.tools, nil
}

func (c *reconnectTestClient) Call(context.Context, string, map[string]any) (CallResult, error) {
	c.callCalls++
	if c.callCalls == 1 {
		return CallResult{}, c.err
	}
	return CallResult{Content: []any{map[string]any{"type": "text", "text": "reconnected"}}}, nil
}

func (c *reconnectTestClient) Close() error { return nil }

type reconnectTestFactory struct{ client *reconnectTestClient }

func (f reconnectTestFactory) New(context.Context, string, []string) (Client, error) {
	return f.client, nil
}

// eventRec is one event emitted through the McpTools onEvent sink.
type eventRec struct {
	typ  string
	data any
}

// newMcpToolsWithEvents returns an McpTools bundle wired to the fake server in
// the given mode and to a slice that records every emitted mcp/* event (the
// composition root wires the same sink to the session log in cmd/sta, D3).
func newMcpToolsWithEvents(t *testing.T, mode string) (*McpTools, *[]eventRec) {
	t.Helper()
	recs := &[]eventRec{}
	mt := NewMcpTools(helperFactory{mode: mode}, []McpServer{
		{Name: "fake", Cmd: os.Args[0], Args: []string{"-test.run=^TestHelperServer$"}},
	}, func(typ string, data any) {
		*recs = append(*recs, eventRec{typ: typ, data: data})
	})
	return mt, recs
}

// eventTypes returns the emitted event types in order.
func eventTypes(recs []eventRec) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.typ)
	}
	return out
}

// decodeEvent unmarshals a captured event payload into T (the session payloads
// are plain JSON-serializable data).
func decodeEvent[T any](t *testing.T, ev eventRec) T {
	t.Helper()
	raw, err := json.Marshal(ev.data)
	if err != nil {
		t.Fatalf("marshal %s event data: %v", ev.typ, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s event data %s: %v", ev.typ, raw, err)
	}
	return out
}

// TestMcpListToolSchema verifies the D7 shape the registry compiles and sends
// to the model (dispatch-m6f-2 §3): additionalProperties false and server
// required.
func TestMcpListToolSchema(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	sch := mt.List().Schema()
	if sch["type"] != "object" {
		t.Fatalf("schema type = %v, want object", sch["type"])
	}
	if sch["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", sch["additionalProperties"])
	}
	req, _ := sch["required"].([]string)
	if len(req) != 1 || req[0] != "server" {
		t.Fatalf("required = %v, want [server]", req)
	}
}

// TestMcpCallToolSchema verifies the D7 shape of mcp_call: additionalProperties
// false, server and tool required, and args a free-form object.
func TestMcpCallToolSchema(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	sch := mt.Call().Schema()
	if sch["type"] != "object" {
		t.Fatalf("schema type = %v, want object", sch["type"])
	}
	if sch["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", sch["additionalProperties"])
	}
	req, _ := sch["required"].([]string)
	if len(req) != 2 || req[0] != "server" || req[1] != "tool" {
		t.Fatalf("required = %v, want [server tool]", req)
	}
	props, _ := sch["properties"].(map[string]any)
	args, _ := props["args"].(map[string]any)
	if args["type"] != "object" {
		t.Fatalf("args type = %v, want object", args["type"])
	}
}

// TestMcpListToolListsAndEmits covers the happy path: mcp_list runs the fake
// server's tools/list over a real stdio JSON-RPC round trip, returns the sorted
// tool table, and lands an mcp/list event with the count.
func TestMcpListToolListsAndEmits(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	out, err := mt.List().Execute(context.Background(), json.RawMessage(`{"server":"fake"}`))
	if err != nil {
		t.Fatalf("mcp_list: %v", err)
	}
	if !strings.Contains(out, "- echo") || !strings.Contains(out, "- noschema") {
		t.Fatalf("mcp_list output = %q, want both fake tools", out)
	}
	if strings.Index(out, "- echo") > strings.Index(out, "- noschema") {
		t.Fatalf("mcp_list output = %q, want sorted tool names", out)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "mcp/list" {
		t.Fatalf("emitted types = %v, want [mcp/list]", got)
	}
	d := decodeEvent[struct {
		Count int `json:"count"`
	}](t, (*recs)[0])
	if d.Count != 2 {
		t.Fatalf("mcp/list payload = %+v, want count 2", d)
	}
}

// TestMcpListToolUnknownServer verifies an unknown server is an error (never a
// panic) naming the configured servers, and that no event is emitted.
func TestMcpListToolUnknownServer(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	_, err := mt.List().Execute(context.Background(), json.RawMessage(`{"server":"nope"}`))
	if err == nil || !strings.Contains(err.Error(), `unknown server "nope"`) {
		t.Fatalf("mcp_list error = %v, want unknown server nope", err)
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed list, got %v", eventTypes(*recs))
	}
}

// TestMcpListToolStartFailure verifies a server whose process cannot start is
// an error (not a panic), and no event is emitted.
func TestMcpListToolStartFailure(t *testing.T) {
	var recs []eventRec
	mt := NewMcpTools(helperFactory{mode: "echo"}, []McpServer{
		{Name: "ghost", Cmd: "sta-mcp-no-such-command-xyz-54321", Args: nil},
	}, func(typ string, data any) {
		recs = append(recs, eventRec{typ: typ, data: data})
	})
	_, err := mt.List().Execute(context.Background(), json.RawMessage(`{"server":"ghost"}`))
	if err == nil || !strings.Contains(err.Error(), "start server") {
		t.Fatalf("mcp_list error = %v, want a start-server error", err)
	}
	if len(recs) != 0 {
		t.Fatalf("no event may be emitted on a failed list, got %v", eventTypes(recs))
	}
}

// TestMcpCallToolCallsAndEmits covers the happy path: mcp_call runs a real
// tools/call over stdio, returns the server content, and lands an mcp/call
// event with the tool name and isError=false.
func TestMcpCallToolCallsAndEmits(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	out, err := mt.Call().Execute(context.Background(), json.RawMessage(
		`{"server":"fake","tool":"echo","args":{"text":"hi"}}`))
	if err != nil {
		t.Fatalf("mcp_call: %v", err)
	}
	if out != "echo:hi" {
		t.Fatalf("mcp_call output = %q, want echo:hi", out)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "mcp/call" {
		t.Fatalf("emitted types = %v, want [mcp/call]", got)
	}
	d := decodeEvent[struct {
		Name    string `json:"name"`
		IsError bool   `json:"isError"`
	}](t, (*recs)[0])
	if d.Name != "echo" || d.IsError {
		t.Fatalf("mcp/call payload = %+v, want echo / not isError", d)
	}
}

func TestMcpCallToolResultPreservesCanonicalContent(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	result, err := mt.Call().ExecuteResult(context.Background(), json.RawMessage(`{"server":"fake","tool":"echo","args":{"text":"rich"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Kind != llm.BlockText || result.Content[0].Text != "echo:rich" {
		t.Fatalf("MCP rich content = %+v", result.Content)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["content"] == nil {
		t.Fatalf("MCP canonical value = %#v", result.Value)
	}
}

func TestMcpDynamicCallDoesNotReplayUnknownCommit(t *testing.T) {
	for _, failure := range []error{ErrConnection, ErrTimeout} {
		t.Run(failure.Error(), func(t *testing.T) {
			client := &reconnectTestClient{err: failure}
			mt := NewMcpTools(reconnectTestFactory{client: client}, []McpServer{{Name: "fake", Cmd: "fixture"}}, nil)
			_, err := mt.Call().ExecuteResult(context.Background(), json.RawMessage(`{"server":"fake","tool":"echo"}`))
			if err == nil || !errors.Is(err, failure) {
				t.Fatalf("dynamic call error = %v, want %v", err, failure)
			}
			if client.startCalls != 1 || client.callCalls != 1 {
				t.Fatalf("calls = start:%d call:%d, want start:1 call:1", client.startCalls, client.callCalls)
			}
		})
	}
}

func TestMcpDynamicCallRejectsRequiredTaskBeforeCall(t *testing.T) {
	client := &reconnectTestClient{tools: []Tool{{Name: "async", TaskSupport: "required"}}}
	mt := NewMcpTools(reconnectTestFactory{client: client}, []McpServer{{Name: "fake", Cmd: "fixture"}}, nil)
	_, err := mt.Call().ExecuteResult(context.Background(), json.RawMessage(`{"server":"fake","tool":"async"}`))
	if err == nil || !strings.Contains(err.Error(), "task-based execution is not supported") {
		t.Fatalf("required task dynamic call error = %v", err)
	}
	if client.startCalls != 1 || client.listCalls != 1 || client.callCalls != 0 {
		t.Fatalf("calls = start:%d list:%d call:%d, want start:1 list:1 call:0", client.startCalls, client.listCalls, client.callCalls)
	}
}

func TestMcpCallToolRegistryAcceptsCanonicalRichValue(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	registry := tools.New()
	if err := registry.Register(mt.Call()); err != nil {
		t.Fatal(err)
	}
	registry.Allow(ToolCallName)
	result, err := registry.Execute(context.Background(), ToolCallName, json.RawMessage(`{"server":"fake","tool":"echo","args":{"text":"registry"}}`))
	if err != nil {
		t.Fatalf("registry mcp_call: %v", err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["content"] == nil || result.IsError {
		t.Fatalf("registry result = %#v, want canonical non-error value", result)
	}
}

func TestMcpCallToolRegistryClassifiesToolLevelError(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	registry := tools.New()
	registry.Allow(ToolCallName)
	if err := registry.Register(mt.Call()); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), ToolCallName, json.RawMessage(`{"server":"fake","tool":"erris"}`))
	if err != nil {
		t.Fatalf("registry mcp_call error: %v", err)
	}
	if !result.IsError || result.Error == nil || result.Error.Code != "MCP_TOOL_ERROR" {
		t.Fatalf("tool-level error = %+v, want structured MCP_TOOL_ERROR", result)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "mcp/call" {
		t.Fatalf("events = %v, want completed mcp/call fact", got)
	}
}

// TestMcpCallToolIsError verifies a tool-level failure inside a successful
// result: the output carries the [isError] marker and the mcp/call event's
// isError flag is set (the call itself completed, so the fact is logged).
func TestMcpCallToolIsError(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	out, err := mt.Call().Execute(context.Background(), json.RawMessage(
		`{"server":"fake","tool":"erris"}`))
	if err != nil {
		t.Fatalf("mcp_call: %v", err)
	}
	if !strings.Contains(out, "[isError]") || !strings.Contains(out, "operation failed") {
		t.Fatalf("mcp_call output = %q, want an isError marker plus the text", out)
	}
	d := decodeEvent[struct {
		Name    string `json:"name"`
		IsError bool   `json:"isError"`
	}](t, (*recs)[0])
	if d.Name != "erris" || !d.IsError {
		t.Fatalf("mcp/call payload = %+v, want erris / isError", d)
	}
}

func TestMcpCallToolIsErrorDoesNotPersistRichImages(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	root := t.TempDir()
	store, err := attachment.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	mt.SetAttachmentStore(store, 1024)
	registry := tools.New()
	registry.Allow(ToolCallName)
	if err := registry.Register(mt.Call()); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), ToolCallName, json.RawMessage(`{"server":"fake","tool":"errimage"}`))
	if err != nil || !result.IsError {
		t.Fatalf("isError image result = %+v err=%v", result, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isError projection wrote %d attachment files", len(entries))
	}
}

// TestMcpCallToolUnknownServer verifies an unknown server is an error (never a
// panic) naming the configured servers, and that no event is emitted.
func TestMcpCallToolUnknownServer(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	_, err := mt.Call().Execute(context.Background(), json.RawMessage(
		`{"server":"nope","tool":"echo"}`))
	if err == nil || !strings.Contains(err.Error(), `unknown server "nope"`) {
		t.Fatalf("mcp_call error = %v, want unknown server nope", err)
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed call, got %v", eventTypes(*recs))
	}
}

// TestMcpCallToolEmptyTool verifies an empty tool name is rejected.
func TestMcpCallToolEmptyTool(t *testing.T) {
	mt, _ := newMcpToolsWithEvents(t, "echo")
	_, err := mt.Call().Execute(context.Background(), json.RawMessage(`{"server":"fake","tool":"  "}`))
	if err == nil || !strings.Contains(err.Error(), "empty tool") {
		t.Fatalf("mcp_call error = %v, want an empty-tool error", err)
	}
}

// TestMcpCallToolServerErrorFrame verifies a server error frame (tools/call
// for an unknown tool) surfaces as an error and logs nothing.
func TestMcpCallToolServerErrorFrame(t *testing.T) {
	mt, recs := newMcpToolsWithEvents(t, "echo")
	_, err := mt.Call().Execute(context.Background(), json.RawMessage(
		`{"server":"fake","tool":"nope"}`))
	if err == nil {
		t.Fatal("mcp_call must error on a server error frame")
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed call, got %v", eventTypes(*recs))
	}
}
