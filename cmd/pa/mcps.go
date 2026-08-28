// mcps.go — the M6f-2 composition-root orchestration (dispatch-m6f-2 §4-5).
// This is where the MCP tool-ecosystem seam is wired into the REPL:
// registerMcps creates the stdio Factory, registers the mcp_list/mcp_call
// tools (internal/mcp, the dynamic name-addressed path), and — when mcp.enabled
// and servers are configured — starts a stdio client per server and bridges
// every advertised tool into the registry as mcp.<server>.<tool> with its input
// schema passed through (the static path). The wiring sits entirely in the tool
// registration layer — the loop's turn/step structure is untouched (D4) — and
// every client call is foreground and serial on the tool path (D5, no
// background goroutine). It must run before registerInteracts so the
// sensitive-tool gate can wrap the mcp tools too.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/mcp"
)

// registerMcps creates the stdio Factory, registers the mcp_* tools and bridges
// every configured server's tools into the registry when mcp.enabled. When mcp
// is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerPlans/…/registerCode).
func (a *app) registerMcps() error {
	if !config.Enabled(a.cfg.Mcp.Enabled) {
		return nil
	}
	f := a.mcpFactory
	if f == nil {
		f = mcp.NewStdioFactory()
	}
	// D3 event sink: mcp/list and mcp/call are appended to the active session
	// log. The callback only ever runs inside a mcp_* tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the other register* wiring.
	ctx := context.Background()
	for _, srv := range a.cfg.Mcp.Servers {
		if err := a.bridgeMcpServer(ctx, f, srv); err != nil {
			return err
		}
	}
	return nil
}

// bridgeMcpServer starts one configured server's stdio client (Factory.New +
// Start + ListTools), then registers each advertised tool as a first-class
// registry tool named mcp.<server>.<tool> with its input schema passed through;
// executing such a tool calls back into the server via tools/call
// (dispatch-m6f-2 §4). The client is kept in a.mcp so main's deferred Close
// terminates every bridged server at shutdown. A server that fails to start or
// list fails startup loudly (the whole registerMcps errors) — mcp is opt-in
// (D10), so a misconfigured server is a real configuration error, not a silent
// degradation.
func (a *app) bridgeMcpServer(ctx context.Context, f mcp.Factory, srv config.McpServer) error {
	client, err := f.New(ctx, srv.Cmd, srv.Args)
	if err != nil {
		return fmt.Errorf("pa: create mcp server %q: %w", srv.Name, err)
	}
	if err := client.Start(ctx); err != nil {
		_ = client.Close()
		return fmt.Errorf("pa: start mcp server %q: %w", srv.Name, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("pa: list tools of mcp server %q: %w", srv.Name, err)
	}
	a.mcp = append(a.mcp, client)
	for _, tl := range tools {
		name := mcp.PublicToolName(srv.Name, tl.Name)
		bt := bridgedMcpTool{
			client:        client,
			name:          name,
			tool:          tl.Name,
			desc:          tl.Description,
			schema:        normalizeSchema(tl.InputSchema),
			output:        normalizeOutputSchema(tl.OutputSchema),
			taskSupport:   tl.TaskSupport,
			attachments:   a.attachStore,
			maxImageBytes: a.cfg.LLM.Multimodal.MaxImageBytes,
		}
		if err := a.reg.Register(bt); err != nil {
			return fmt.Errorf("pa: register bridged mcp tool %q: %w", name, err)
		}
		// Bridged names are dynamic — config.applyDefaults cannot whitelist
		// them (their names are only known at runtime), so the whitelist is
		// extended here as each tool is registered (dispatch-m6f-2 §4).
		a.reg.Allow(name)
	}
	return nil
}

// bridgedMcpTool is one MCP server tool registered by registerMcps as
// mcp.<server>.<tool> (dispatch-m6f-2 §4). It implements the tools.Tool method
// set structurally (Go structural typing) — cmd/pa is the composition root, so
// importing internal/tools here is fine. Executing it calls back into the live
// server client via tools/call with the model's arguments passed through
// verbatim; the registry's D7 gate validates those arguments against the
// server's own input schema, which is passed through to Schema().
type bridgedMcpTool struct {
	client        mcp.Client
	name          string
	tool          string
	desc          string
	schema        map[string]any
	output        map[string]any
	taskSupport   string
	attachments   *attachment.Store
	maxImageBytes int
}

func (t bridgedMcpTool) Name() string           { return t.name }
func (t bridgedMcpTool) Description() string    { return t.desc }
func (t bridgedMcpTool) Schema() map[string]any { return t.schema }
func (t bridgedMcpTool) OutputSchema() map[string]any {
	structured := map[string]any{}
	required := []string{"content"}
	if t.output != nil {
		structured = t.output
		required = append(required, "structuredContent")
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"content": map[string]any{"type": "array", "items": map[string]any{}}, "structuredContent": structured},
		"required":             required,
		"additionalProperties": false,
	}
}

func (t bridgedMcpTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	return result.Output, err
}

func (t bridgedMcpTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if t.taskSupport == "required" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: MCP task-based execution is not supported", t.name)
	}
	var a map[string]any
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	res, err := t.client.Call(ctx, t.tool, a)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if res.IsError {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %s", t.name, mcp.FormatCallResult(res))
	}
	value := map[string]any{"content": res.Content}
	if res.StructuredContentSet {
		value["structuredContent"] = res.StructuredContent
	}
	// Same rendering as mcp_call (mcp.FormatCallResult), so a server tool's
	// result reads identically through either path.
	return agenttools.ToolResult{Value: value, Output: mcp.FormatCallResult(res), Content: projectMcpContent(t.name, res.Content, t.attachments, t.maxImageBytes)}, nil
}

// projectMcpContent preserves DSH's rich result boundary: text remains text,
// supported MCP image blocks are durably stored as ImageRef values, and
// unsupported or unadmitted blocks become explicit diagnostic text while the
// canonical Value above still retains the raw protocol content.
func projectMcpContent(toolName string, items []any, store *attachment.Store, maxImageBytes int) []llm.ContentBlock {
	blocks := make([]llm.ContentBlock, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			blocks = append(blocks, llm.Text("[unsupported MCP content block: expected an object]"))
			continue
		}
		typ, _ := obj["type"].(string)
		switch typ {
		case "text":
			text, _ := obj["text"].(string)
			blocks = append(blocks, llm.Text(text))
		case "image":
			blocks = append(blocks, projectMcpImage(obj, store, maxImageBytes))
		case "resource_link":
			name, _ := obj["name"].(string)
			uri, _ := obj["uri"].(string)
			if name == "" || uri == "" {
				blocks = append(blocks, llm.Text("[resource link unavailable: the MCP block is missing its name or URI]"))
			} else {
				blocks = append(blocks, llm.Text(fmt.Sprintf("Resource link: %s (%s)", name, uri)))
			}
		case "audio":
			mime, _ := obj["mimeType"].(string)
			blocks = append(blocks, llm.Text(fmt.Sprintf("[audio result unsupported: %s; raw audio data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"))))
		case "resource":
			blocks = append(blocks, llm.Text("[embedded resource unsupported; raw resource data remains available to programmatic callers]"))
		default:
			blocks = append(blocks, llm.Text(fmt.Sprintf("[unsupported MCP content type: %s]", typ)))
		}
	}
	if len(blocks) == 0 {
		return []llm.ContentBlock{llm.Text(fmt.Sprintf("(%s returned no model-visible content)", toolName))}
	}
	return blocks
}

func projectMcpImage(obj map[string]any, store *attachment.Store, maxImageBytes int) llm.ContentBlock {
	mime, _ := obj["mimeType"].(string)
	data, _ := obj["data"].(string)
	if store == nil {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; no attachment store is mounted; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	if attachment.MediaTypeForExtension("."+strings.TrimPrefix(mime, "image/")) == "" {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; the declared media type is not PNG, JPEG, WebP, or GIF; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || data == "" || base64.StdEncoding.EncodeToString(decoded) != data {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; the image data is not canonical base64; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	ref, err := store.SaveImage(mime, decoded, maxImageBytes)
	if err != nil {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; image admission rejected the result: %v; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"), err))
	}
	return llm.ContentBlock{Kind: llm.BlockImage, Image: ref}
}

func fallbackMcpString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// normalizeSchema adapts a server-provided inputSchema for registration in the
// tools registry (dispatch-m6f-2 §4: schema passthrough): the nil schema
// becomes an empty object (the registry compiles it as accept-anything) and a
// schema missing "type" is assumed to be an object (MCP tool arguments are
// always a JSON object), so the D7 gate rejects non-object arguments cleanly.
// Everything else is passed through verbatim.
func normalizeSchema(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

func normalizeOutputSchema(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// toMcpServers maps the configured config.McpServer entries onto the mcp
// package's own McpServer shape (identical fields), keeping the seam decoupled
// from config (D2).
func toMcpServers(servers []config.McpServer) []mcp.McpServer {
	out := make([]mcp.McpServer, 0, len(servers))
	for _, s := range servers {
		out = append(out, mcp.McpServer{Name: s.Name, Cmd: s.Cmd, Args: s.Args})
	}
	return out
}

// mcpServerNames returns the configured server names for the /help status line.
func mcpServerNames(servers []config.McpServer) string {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}
