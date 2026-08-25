// tools.go — the M6f-2 Consumer half of the MCP seam (design.md §8 Consumer /
// D2, dispatch-m6f-2 §3): mcp_list and mcp_call are registered into the
// tools.Registry by the composition root (cmd/pa) when mcp.enabled, and
// auto-whitelisted by config.applyDefaults the same way the job_*/subagent_*/
// skill_*/schedule_*/plan_*/spill_*/interact_*/code_* tools are. The returned
// tools implement the tools.Tool method set structurally (Go structural
// typing), so this package never imports the tools package — the seam stays
// decoupled (D2). They also never import the config package: the configured
// servers arrive as the seam's own McpServer shape, which the composition root
// maps from config.McpServer (identical fields, D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; server required; mcp_call's args is a free-form object the server tool
// receives verbatim) before this code runs; the checks are repeated here so a
// direct call can never bypass them.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 M6f /
// dispatch-m6f-2 §3): mcp_list emits mcp/list on a completed tools/list (the
// count) and mcp_call emits mcp/call on a completed tools/call (the tool name
// plus whether the server reported a tool-level failure inside a successful
// result), through the injected onEvent sink (the composition root wires it to
// the session log), inside a tool Execute on the serial main-loop path (D5). A
// call that did not complete — unknown server, start/handshake failure,
// protocol error, cancelled context — returns an error and logs nothing; the
// loop surfaces it as tool/error.
//
// Each mcp_* invocation builds a fresh stdio client through the injected
// Factory, uses it for the one round trip (D5, foreground and serial) and
// closes it in a defer, so a tool call can never leak a subprocess or a
// goroutine. The composition root's static server bridging (M6f-2 §4) holds
// long-lived clients separately; these tools are the dynamic, name-addressed
// path.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"sort"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
)

// McpServer is one configured MCP server: a unique Name (used as the
// mcp_list/mcp_call selector and the mcp.<server>.<tool> bridge prefix) and a
// stdio command line (Cmd plus Args) the Factory spawns. It mirrors
// config.McpServer field-for-field; the composition root maps between the two
// so this seam never imports config (D2).
type McpServer struct {
	Name string
	Cmd  string
	Args []string
}

// ToolListName and ToolCallName are the MCP consumer tools (whitelisted when
// mcp.enabled; see config.mcpToolNames).
const (
	ToolListName = "mcp_list"
	ToolCallName = "mcp_call"
)

// McpTools bundles the shared state of the mcp_* tools: the Factory that
// builds a client per configured server, the configured servers, and the D3
// event sink.
type McpTools struct {
	f       Factory
	servers []McpServer
	onEvent func(typ string, data any)
}

// NewMcpTools returns the mcp_* tool bundle bound to a Factory. onEvent, when
// non-nil, receives the mcp/* event payloads; the composition root wires it to
// the session log (D3).
func NewMcpTools(f Factory, servers []McpServer, onEvent func(typ string, data any)) *McpTools {
	return &McpTools{f: f, servers: servers, onEvent: onEvent}
}

// List returns the mcp_list tool.
func (t *McpTools) List() McpListTool { return McpListTool{t: t} }

// Call returns the mcp_call tool.
func (t *McpTools) Call() McpCallTool { return McpCallTool{t: t} }

// emit forwards one mcp/* event payload to the injected sink (D3).
func (t *McpTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// findServer returns the configured server with the given name.
func (t *McpTools) findServer(name string) (McpServer, bool) {
	for _, s := range t.servers {
		if s.Name == name {
			return s, true
		}
	}
	return McpServer{}, false
}

// serverNames returns the sorted configured server names for error messages
// ("" only when none are configured).
func (t *McpTools) serverNames() string {
	names := make([]string, 0, len(t.servers))
	for _, s := range t.servers {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none configured)"
	}
	return strings.Join(names, ", ")
}

// McpListTool lists the tools a configured MCP server advertises (tools/list)
// and returns them as model-facing text. It is the dynamic sibling of the
// composition root's static bridging (M6f-2 §4): the model names a server to
// discover its current tool table.
type McpListTool struct {
	t *McpTools
}

func (McpListTool) Name() string { return ToolListName }

func (McpListTool) Description() string {
	return "list the tools a configured MCP server advertises (tools/list): returns the sorted tool names with their descriptions, logging mcp/list"
}

func (McpListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "name of the configured MCP server to list (see mcp.servers)",
			},
		},
		"required":             []string{"server"},
		"additionalProperties": false,
	}
}

func (t McpListTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Server string `json:"server"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("mcp_list: %w", err)
	}
	srv, ok := t.t.findServer(a.Server)
	if !ok {
		return "", fmt.Errorf("mcp_list: unknown server %q (configured: %s)", a.Server, t.t.serverNames())
	}
	client, err := t.t.f.New(ctx, srv.Cmd, srv.Args)
	if err != nil {
		return "", fmt.Errorf("mcp_list: create client for %s: %w", srv.Name, err)
	}
	defer client.Close()
	if err := client.Start(ctx); err != nil {
		return "", fmt.Errorf("mcp_list: start server %q: %w", srv.Name, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return "", fmt.Errorf("mcp_list: list tools of %q: %w", srv.Name, err)
	}
	t.t.emit(session.EventMcpList, session.NewMcpList(len(tools)))
	return formatToolList(tools), nil
}

// McpCallTool invokes a named tool on a configured MCP server (tools/call)
// with a free-form arguments object and returns the server's content as
// model-facing text. A tool that reports execution failure inside a successful
// result is returned normally with an [isError] marker (mcp/call is logged
// with isError); a transport/protocol failure is an error and logs nothing.
type McpCallTool struct {
	t *McpTools
}

func (McpCallTool) Name() string { return ToolCallName }

func (McpCallTool) Description() string {
	return "call a named tool on a configured MCP server (tools/call): passes the free-form args object through and returns the server's content, logging mcp/call"
}

func (McpCallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "name of the configured MCP server hosting the tool (see mcp.servers)",
			},
			"tool": map[string]any{
				"type":        "string",
				"description": "name of the tool to invoke on that server",
			},
			"args": map[string]any{
				"type":        "object",
				"description": "arguments to pass to the tool, following its input schema (optional)",
			},
		},
		"required":             []string{"server", "tool"},
		"additionalProperties": false,
	}
}

func (t McpCallTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Server string         `json:"server"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("mcp_call: %w", err)
	}
	srv, ok := t.t.findServer(a.Server)
	if !ok {
		return "", fmt.Errorf("mcp_call: unknown server %q (configured: %s)", a.Server, t.t.serverNames())
	}
	if strings.TrimSpace(a.Tool) == "" {
		return "", fmt.Errorf("mcp_call: empty tool name")
	}
	client, err := t.t.f.New(ctx, srv.Cmd, srv.Args)
	if err != nil {
		return "", fmt.Errorf("mcp_call: create client for %s: %w", srv.Name, err)
	}
	defer client.Close()
	if err := client.Start(ctx); err != nil {
		return "", fmt.Errorf("mcp_call: start server %q: %w", srv.Name, err)
	}
	res, err := client.Call(ctx, a.Tool, a.Args)
	if err != nil {
		return "", fmt.Errorf("mcp_call: %s.%s: %w", srv.Name, a.Tool, err)
	}
	// mcp/call is a log-only fact (D3) carrying the tool name and whether the
	// server reported a tool-level failure; the full content lives in the
	// tool/result the loop logs.
	t.t.emit(session.EventMcpCall, session.NewMcpCall(a.Tool, res.IsError))
	return FormatCallResult(res), nil
}

// formatToolList renders a tools/list result as model-facing text: the sorted
// tool names with their descriptions, one per line.
func formatToolList(tools []Tool) string {
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	if len(tools) == 0 {
		return "no tools"
	}
	var sb strings.Builder
	for _, tl := range tools {
		fmt.Fprintf(&sb, "- %s", tl.Name)
		if tl.Description != "" {
			fmt.Fprintf(&sb, ": %s", tl.Description)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// FormatCallResult renders a tools/call result as model-facing text: an
// [isError] marker when the server reported a tool-level failure, then the
// content items (each item's "text" when it is a text block, else its JSON).
// It is shared by mcp_call and by cmd/pa's bridged mcp.<server>.<tool> tools so
// a server tool's result reads identically through either path.
func FormatCallResult(res CallResult) string {
	var sb strings.Builder
	if res.IsError {
		sb.WriteString("[isError]\n")
	}
	for i, item := range res.Content {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(contentItemText(item))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// contentItemText renders one tools/call content item as text: a plain string
// item verbatim, the item's "text" field for the common
// {"type":"text","text":…} block, else the item's JSON.
func contentItemText(item any) string {
	if s, ok := item.(string); ok {
		return s
	}
	if m, ok := item.(map[string]any); ok {
		if text, ok := m["text"].(string); ok {
			return text
		}
	}
	b, err := json.Marshal(item)
	if err != nil {
		return fmt.Sprintf("%v", item)
	}
	return string(b)
}
