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
// Each mcp_* invocation builds a fresh client through the injected Factory,
// performs discovery plus (for mcp_call) one side-effecting call on the same
// foreground connection, and closes it in a defer, so a tool call can never
// leak a subprocess or a goroutine. The composition root's static server
// bridging (M6f-2 §4) holds long-lived clients separately; these tools are the
// dynamic, name-addressed path.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
)

// McpServer is one configured MCP server: a unique Name (used as the
// mcp_list/mcp_call selector and the mcp.<server>.<tool> bridge prefix) and a
// stdio command line (Cmd plus Args) the Factory spawns. It mirrors
// config.McpServer field-for-field; the composition root maps between the two
// so this seam never imports config (D2).
type McpServer struct {
	Name      string
	Transport string
	Cmd       string
	Args      []string
	URL       string
	Headers   map[string]string
	// Env and Cwd apply to stdio child processes. Explicit Env entries are
	// added after the scrubbed ambient environment.
	Env map[string]string
	Cwd string
	// ToolCallTimeout bounds tools/call only.
	ToolCallTimeout time.Duration
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
	f              Factory
	servers        []McpServer
	onEvent        func(typ string, data any)
	onEventErr     func(typ string, data any) error
	attachments    *attachment.Store
	maxImageBytes  int
	imageAdmission func(context.Context) error
}

// NewMcpTools returns the mcp_* tool bundle bound to a Factory. onEvent, when
// non-nil, receives the mcp/* event payloads; the composition root wires it to
// the session log (D3).
func NewMcpTools(f Factory, servers []McpServer, onEvent func(typ string, data any)) *McpTools {
	return &McpTools{f: f, servers: servers, onEvent: onEvent}
}

// SetErrorSink lets a composition root fail a tool call when its durable
// lifecycle event cannot be appended. The legacy callback remains supported.
func (t *McpTools) SetErrorSink(sink func(typ string, data any) error) { t.onEventErr = sink }

// SetAttachmentStore enables canonical image references for dynamic mcp_call
// results. Without a store, image blocks are represented by bounded diagnostic
// text rather than leaking base64 into the model transcript.
func (t *McpTools) SetAttachmentStore(store *attachment.Store, maxBytes int) {
	if t != nil {
		t.attachments = store
		t.maxImageBytes = maxBytes
	}
}

// SetImageAdmission installs the host's exact session/model capability gate
// for image-bearing MCP results. A nil gate preserves the standalone package
// behavior for embedders that intentionally own admission themselves.
func (t *McpTools) SetImageAdmission(admission func(context.Context) error) {
	if t != nil {
		t.imageAdmission = admission
	}
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

func (t *McpTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	if t.onEventErr != nil {
		return t.onEventErr(typ, data)
	}
	t.emit(typ, data)
	return nil
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

// CancellationAware is explicit because client creation, process start,
// tools/list, and event emission all receive the registry-supplied context.
func (McpListTool) CancellationAware() bool { return true }

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
	client, err := NewClientForServer(ctx, t.t.f, srv)
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
	if err := t.t.emitContext(ctx, session.EventMcpList, session.NewMcpList(len(tools))); err != nil {
		return "", fmt.Errorf("mcp_list: persist event: %w", err)
	}
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

// CancellationAware is explicit because process start and tools/call receive
// the registry-supplied context; caller cancellation is never retried.
func (McpCallTool) CancellationAware() bool { return true }

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
	result, err := t.ExecuteResult(ctx, args)
	return result.Output, err
}

func (t McpCallTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Server string         `json:"server"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: %w", err)
	}
	srv, ok := t.t.findServer(a.Server)
	if !ok {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: unknown server %q (configured: %s)", a.Server, t.t.serverNames())
	}
	if strings.TrimSpace(a.Tool) == "" {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: empty tool name")
	}
	client, err := NewClientForServer(ctx, t.t.f, srv)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: create client for %s: %w", srv.Name, err)
	}
	defer client.Close()
	if err := client.Start(ctx); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: start server %q: %w", srv.Name, err)
	}
	// Dynamic calls do not have a generation-bound Tool definition carrying
	// execution.taskSupport, so discover the advertised metadata before
	// crossing the side-effecting tools/call boundary. This is the same
	// fail-closed rule as the static bridge: task-only execution is not silently
	// downgraded to a foreground call. A missing name is left to tools/call so
	// the server retains ownership of its normal unknown-tool diagnostic.
	advertised, err := client.ListTools(ctx)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: discover tool %q on %s: %w", a.Tool, srv.Name, err)
	}
	for _, tool := range advertised {
		if tool.Name == a.Tool && strings.EqualFold(strings.TrimSpace(tool.TaskSupport), "required") {
			return agenttools.ToolResult{}, fmt.Errorf("mcp_call: %s: MCP task-based execution is not supported", a.Tool)
		}
	}
	// A tools/call request is at-most-once. A connection error or timeout can
	// arrive after the server committed the side effect, so retrying here would
	// duplicate an unknown-commit request. The supervisor may recover a
	// long-lived bridge separately; a later model-level retry is explicit.
	res, err := client.Call(ctx, a.Tool, a.Args)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: %s.%s: %w", srv.Name, a.Tool, err)
	}
	// mcp/call is a log-only fact (D3) carrying the tool name and whether the
	// server reported a tool-level failure. The completed call is logged before
	// projecting the result, including the failure case.
	if err := t.t.emitContext(ctx, session.EventMcpCall, session.NewMcpCall(a.Tool, res.IsError)); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: persist event: %w", err)
	}
	value := map[string]any{"content": res.Content}
	if res.StructuredContentSet {
		value["structuredContent"] = res.StructuredContent
	}
	// A tool-level MCP error is still a completed call, but its untrusted rich
	// blocks must not be admitted into the durable attachment store. Keep the
	// raw protocol value below for programmatic callers and render the error
	// content without persistence.
	contentTools := t.t
	if res.IsError {
		copyTools := *t.t
		copyTools.attachments = nil
		copyTools.imageAdmission = nil
		contentTools = &copyTools
	}
	content := projectCallContentContext(contentTools, ctx, res.Content)
	if res.IsError {
		// The reference MCP bridge throws for a successful JSON-RPC response
		// whose result carries isError=true. Preserve that distinction from a
		// transport/protocol failure: the call fact is durable, while the tool
		// result is a structured model-visible error.
		return agenttools.ToolResult{
			Output:  FormatCallResult(res),
			Content: content,
			IsError: true,
			Error:   &agenttools.ErrorInfo{Name: "MCPToolError", Code: "MCP_TOOL_ERROR"},
		}, nil
	}
	return agenttools.ToolResult{Value: value, Output: FormatCallResult(res), Content: content}, nil
}

func projectCallContent(t *McpTools, items []any) []llm.ContentBlock {
	return projectCallContentContext(t, context.Background(), items)
}

func projectCallContentContext(t *McpTools, ctx context.Context, items []any) []llm.ContentBlock {
	imageInputs := make([]attachment.ImageInput, 0)
	imageIndexes := make([]int, 0)
	imageReasons := make(map[int]string)
	for index, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := obj["type"].(string)
		if typ != "image" {
			continue
		}
		mediaType, _ := obj["mimeType"].(string)
		encoded, _ := obj["data"].(string)
		if attachment.MediaTypeForExtension("."+strings.TrimPrefix(mediaType, "image/")) == "" {
			imageReasons[index] = "declared media type is unsupported"
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || encoded == "" || base64.StdEncoding.EncodeToString(data) != encoded {
			imageReasons[index] = "image data is not canonical base64"
			continue
		}
		imageIndexes = append(imageIndexes, index)
		imageInputs = append(imageInputs, attachment.ImageInput{MediaType: mediaType, Data: data})
	}
	imageRefs := make(map[int]llm.ImageRef)
	if len(imageReasons) == 0 && len(imageInputs) > 0 {
		reason := ""
		if t == nil || t.attachments == nil {
			reason = "no durable attachment store is mounted"
		} else if t.imageAdmission != nil {
			if err := t.imageAdmission(ctx); err != nil {
				reason = err.Error()
			}
		}
		if reason == "" {
			refs, err := t.attachments.SaveImages(imageInputs, t.maxImageBytes)
			if err != nil {
				reason = fmt.Sprintf("image admission rejected the result: %v", err)
			} else {
				for i, index := range imageIndexes {
					imageRefs[index] = refs[i]
				}
			}
		}
		if reason != "" {
			for _, index := range imageIndexes {
				imageReasons[index] = reason
			}
		}
	}
	blocks := make([]llm.ContentBlock, 0, len(items))
	textRun := make([]string, 0)
	flushText := func() {
		if len(textRun) == 0 {
			return
		}
		blocks = append(blocks, llm.Text(strings.Join(textRun, "\n")))
		textRun = nil
	}
	for index, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			textRun = append(textRun, "[unsupported MCP content block: expected an object]")
			continue
		}
		typ, _ := obj["type"].(string)
		switch typ {
		case "text":
			if text, exists := obj["text"]; exists {
				if value, ok := text.(string); ok {
					textRun = append(textRun, value)
				}
			}
		case "image":
			flushText()
			if ref, ok := imageRefs[index]; ok {
				blocks = append(blocks, llm.ContentBlock{Kind: llm.BlockImage, Image: ref})
				continue
			}
			reason := imageReasons[index]
			if reason == "" {
				reason = "another image in the same result was invalid or was not admitted"
			}
			mediaType, _ := obj["mimeType"].(string)
			blocks = append(blocks, llm.Text(fmt.Sprintf("[image unavailable: %s; %s; raw image data remains available to programmatic callers]", fallbackMcpString(mediaType, "unknown media type"), reason)))
		case "resource_link":
			name, _ := obj["name"].(string)
			uri, _ := obj["uri"].(string)
			if name == "" || uri == "" {
				textRun = append(textRun, "[resource link unavailable: the MCP block is missing its name or URI]")
			} else {
				textRun = append(textRun, fmt.Sprintf("Resource link: %s (%s)", name, uri))
			}
		case "audio":
			mediaType, _ := obj["mimeType"].(string)
			textRun = append(textRun, fmt.Sprintf("[audio result unsupported: %s; raw audio data remains available to programmatic callers]", fallbackMcpString(mediaType, "unknown media type")))
		case "resource":
			textRun = append(textRun, "[embedded resource unsupported; raw resource data remains available to programmatic callers]")
		default:
			textRun = append(textRun, fmt.Sprintf("[unsupported MCP content type: %s]", typ))
		}
	}
	flushText()
	if len(blocks) == 0 {
		return []llm.ContentBlock{llm.Text("(MCP returned no model-visible content)")}
	}
	return blocks
}

func fallbackMcpString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
