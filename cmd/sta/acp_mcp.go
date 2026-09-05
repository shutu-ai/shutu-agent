package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/mcp"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
)

// acpMCP owns the MCP clients for exactly one ACP session. It deliberately
// does not use app.mcp: those clients belong to the REPL's global session and
// may capture its mutable current-session state.
type acpMCP struct {
	mu             sync.Mutex
	owner          string
	log            *session.Log
	clients        map[string]mcp.Client
	advertised     map[string]acpMCPToolRef
	byServer       map[string][]mcp.Tool
	closed         bool
	attachments    *attachment.Store
	maxImageBytes  int
	imageAdmission func(context.Context) error
}

type acpMCPToolRef struct {
	server string
	client mcp.Client
	tool   mcp.Tool
}

func newACPMCP(ctx context.Context, a *app, owner string, log *session.Log) (*acpMCP, error) {
	return newACPMCPWithConfig(ctx, a, owner, log, a.providerConfigSnapshot())
}

func newACPMCPWithConfig(ctx context.Context, a *app, owner string, log *session.Log, cfg config.Config) (*acpMCP, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f := a.mcpFactory
	if f == nil {
		f = mcp.NewStdioFactory()
	}
	s := &acpMCP{
		owner:          owner,
		log:            log,
		clients:        make(map[string]mcp.Client),
		advertised:     make(map[string]acpMCPToolRef),
		byServer:       make(map[string][]mcp.Tool),
		attachments:    a.attachStore,
		maxImageBytes:  cfg.LLM.Multimodal.MaxImageBytes,
		imageAdmission: a.mcpImageAdmission,
	}
	for _, srv := range cfg.Mcp.Servers {
		name := strings.TrimSpace(srv.Name)
		mapped := mcp.McpServer{Name: name, Transport: srv.Transport, Cmd: srv.Cmd, Args: srv.Args, URL: srv.URL, Headers: srv.Headers, Env: srv.Env, Cwd: srv.Cwd, ToolCallTimeout: time.Duration(srv.ToolCallTimeoutMS) * time.Millisecond}
		if err := mcp.ValidateServer(mapped); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP invalid server %q: %w", name, err)
		}
		if _, exists := s.clients[name]; exists {
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP server %q is configured more than once", name)
		}
		client, err := mcp.NewClientForServer(ctx, f, mapped)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP create server %q: %w", name, err)
		}
		if err := client.Start(ctx); err != nil {
			_ = client.Close()
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP start server %q: %w", name, err)
		}
		advertised, err := client.ListTools(ctx)
		if err != nil {
			_ = client.Close()
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP list tools of server %q: %w", name, err)
		}
		s.clients[name] = client
		s.byServer[name] = append([]mcp.Tool(nil), advertised...)
		for _, tool := range advertised {
			if strings.TrimSpace(tool.Name) == "" {
				_ = s.Close()
				return nil, fmt.Errorf("ACP MCP server %q advertised an empty tool name", name)
			}
			fullName := "mcp__" + name + "__" + tool.Name
			if _, exists := s.advertised[fullName]; exists {
				_ = s.Close()
				return nil, fmt.Errorf("ACP MCP tool %q is advertised more than once", fullName)
			}
			s.advertised[fullName] = acpMCPToolRef{
				server: name,
				client: client,
				tool:   tool,
			}
		}
	}
	return s, nil
}

func (s *acpMCP) tools() []agenttools.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	result := []agenttools.Tool{}
	names := make([]string, 0, len(s.advertised))
	for name := range s.advertised {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := s.advertised[name]
		result = append(result, acpMCPTool{
			service: s,
			name:    name,
			tool:    ref.tool,
		})
	}
	return result
}

func (s *acpMCP) list(server string) (string, error) {
	return s.listContext(context.Background(), server)
}

func (s *acpMCP) listContext(ctx context.Context, server string) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("ACP MCP service is closed")
	}
	tools, ok := s.byServer[server]
	tools = append([]mcp.Tool(nil), tools...)
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("mcp_list: unknown server %q", server)
	}
	if err := s.emitEvent(ctx, session.EventMcpList, session.NewMcpList(len(tools))); err != nil {
		return "", fmt.Errorf("mcp_list: persist event: %w", err)
	}
	return formatACPMPCToolList(tools), nil
}

func (s *acpMCP) call(server, name string, args map[string]any) (string, error) {
	return s.callContext(context.Background(), server, name, args)
}

func (s *acpMCP) callContext(ctx context.Context, server, name string, args map[string]any) (string, error) {
	result, err := s.callContextResult(ctx, server, name, args)
	if err != nil {
		return "", err
	}
	return mcp.FormatCallResult(result), nil
}

func (s *acpMCP) callContextResult(ctx context.Context, server, name string, args map[string]any) (mcp.CallResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return mcp.CallResult{}, errors.New("ACP MCP service is closed")
	}
	client, ok := s.clients[server]
	s.mu.Unlock()
	if !ok {
		return mcp.CallResult{}, fmt.Errorf("mcp_call: unknown server %q", server)
	}
	if strings.TrimSpace(name) == "" {
		return mcp.CallResult{}, errors.New("mcp_call: empty tool name")
	}
	result, err := callMCPWithReconnect(ctx, client, name, args)
	if err != nil {
		return mcp.CallResult{}, fmt.Errorf("mcp_call: %s.%s: %w", server, name, err)
	}
	if err := s.emitEvent(ctx, session.EventMcpCall, session.NewMcpCall(name, result.IsError)); err != nil {
		return mcp.CallResult{}, fmt.Errorf("mcp_call: persist event: %w", err)
	}
	return result, nil
}

func (s *acpMCP) callAdvertised(ctx context.Context, fullName string, args map[string]any) (string, error) {
	result, err := s.callAdvertisedResult(ctx, fullName, args)
	if err != nil {
		return "", err
	}
	return mcp.FormatCallResult(result), nil
}

func (s *acpMCP) callAdvertisedResult(ctx context.Context, fullName string, args map[string]any) (mcp.CallResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return mcp.CallResult{}, errors.New("ACP MCP service is closed")
	}
	ref, ok := s.advertised[fullName]
	s.mu.Unlock()
	if !ok {
		return mcp.CallResult{}, fmt.Errorf("unknown MCP tool %q", fullName)
	}
	result, err := callMCPWithReconnect(ctx, ref.client, ref.tool.Name, args)
	if err != nil {
		return mcp.CallResult{}, fmt.Errorf("%s: %w", fullName, err)
	}
	if err := s.emitEvent(ctx, session.EventMcpCall, session.NewMcpCall(ref.tool.Name, result.IsError)); err != nil {
		return mcp.CallResult{}, fmt.Errorf("%s: persist event: %w", fullName, err)
	}
	return result, nil
}

// emitEvent uses the Agent-owned runtime sink when present. Direct ACP calls
// without a runtime context fall back to this session's own log.
func (s *acpMCP) emitEvent(ctx context.Context, typ string, data any) error {
	if err := runtimectx.Emit(ctx, typ, data); err != nil {
		return err
	}
	if _, ok := runtimectx.Get(ctx); ok {
		return nil
	}
	s.mu.Lock()
	log := s.log
	s.mu.Unlock()
	if log == nil {
		return nil
	}
	_, err := log.Append(typ, data)
	return err
}

func (s *acpMCP) closeClients() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	clients := make([]mcp.Client, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	s.clients = nil
	s.advertised = nil
	s.byServer = nil
	s.mu.Unlock()

	var first error
	for _, client := range clients {
		if err := client.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *acpMCP) Close() error { return s.closeClients() }

type acpMCPTool struct {
	service *acpMCP
	name    string
	tool    mcp.Tool
}

func (t acpMCPTool) Name() string           { return t.name }
func (t acpMCPTool) Description() string    { return t.tool.Description }
func (t acpMCPTool) Schema() map[string]any { return normalizeSchema(t.tool.InputSchema) }
func (t acpMCPTool) OutputSchema() map[string]any {
	structured := map[string]any{}
	required := []string{"content"}
	if t.tool.OutputSchema != nil {
		structured = normalizeOutputSchema(t.tool.OutputSchema)
		required = append(required, "structuredContent")
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"content":           map[string]any{"type": "array", "items": map[string]any{}},
			"structuredContent": structured,
		},
		"required": required,
	}
}
func (t acpMCPTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	return result.Output, err
}

func (t acpMCPTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if t.tool.TaskSupport == "required" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: MCP task-based execution is not supported", t.name)
	}
	var values map[string]any
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	result, err := t.service.callAdvertisedResult(ctx, t.name, values)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	value := map[string]any{"content": result.Content}
	if result.StructuredContentSet {
		value["structuredContent"] = result.StructuredContent
	}
	contentStore := t.service.attachments
	contentAdmission := t.service.imageAdmission
	if result.IsError {
		contentStore = nil
		contentAdmission = nil
	}
	content := projectMcpContentWithAdmission(ctx, t.name, result.Content, contentStore, t.service.maxImageBytes, contentAdmission)
	if result.IsError {
		return agenttools.ToolResult{
			Output:  mcp.FormatCallResult(result),
			Content: content,
			IsError: true,
			Error:   &agenttools.ErrorInfo{Name: "MCPToolError", Code: "MCP_TOOL_ERROR"},
		}, nil
	}
	return agenttools.ToolResult{Value: value, Output: mcp.FormatCallResult(result), Content: content}, nil
}

type acpMCPListTool struct{ service *acpMCP }

func (acpMCPListTool) Name() string { return mcp.ToolListName }
func (acpMCPListTool) Description() string {
	return "list the tools already advertised by an MCP server in this ACP session"
}
func (acpMCPListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{"type": "string"},
		},
		"required":             []string{"server"},
		"additionalProperties": false,
	}
}
func (t acpMCPListTool) Execute(ctx context.Context, args any) (string, error) {
	var values struct {
		Server string `json:"server"`
	}
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return "", fmt.Errorf("mcp_list: %w", err)
	}
	return t.service.listContext(ctx, values.Server)
}

type acpMCPCallTool struct{ service *acpMCP }

func (acpMCPCallTool) Name() string { return mcp.ToolCallName }
func (acpMCPCallTool) Description() string {
	return "call a named tool on an MCP server already connected to this ACP session"
}
func (acpMCPCallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{"type": "string"},
			"tool":   map[string]any{"type": "string"},
			"args":   map[string]any{"type": "object"},
		},
		"required":             []string{"server", "tool"},
		"additionalProperties": false,
	}
}
func (t acpMCPCallTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	return result.Output, err
}

func (t acpMCPCallTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var values struct {
		Server string         `json:"server"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
	}
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("mcp_call: %w", err)
	}
	result, err := t.service.callContextResult(ctx, values.Server, values.Tool, values.Args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	value := map[string]any{"content": result.Content}
	if result.StructuredContentSet {
		value["structuredContent"] = result.StructuredContent
	}
	contentStore := t.service.attachments
	contentAdmission := t.service.imageAdmission
	if result.IsError {
		contentStore = nil
		contentAdmission = nil
	}
	content := projectMcpContentWithAdmission(ctx, mcp.ToolCallName, result.Content, contentStore, t.service.maxImageBytes, contentAdmission)
	if result.IsError {
		return agenttools.ToolResult{
			Output:  mcp.FormatCallResult(result),
			Content: content,
			IsError: true,
			Error:   &agenttools.ErrorInfo{Name: "MCPToolError", Code: "MCP_TOOL_ERROR"},
		}, nil
	}
	return agenttools.ToolResult{
		Value:   value,
		Output:  mcp.FormatCallResult(result),
		Content: content,
	}, nil
}

func formatACPMPCToolList(tools []mcp.Tool) string {
	copyTools := append([]mcp.Tool(nil), tools...)
	sort.Slice(copyTools, func(i, j int) bool { return copyTools[i].Name < copyTools[j].Name })
	if len(copyTools) == 0 {
		return "no tools"
	}
	var b strings.Builder
	for i, tool := range copyTools {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(tool.Name)
		if tool.Description != "" {
			b.WriteString(": ")
			b.WriteString(tool.Description)
		}
	}
	return b.String()
}
