package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/session"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

// acpMCP owns the MCP clients for exactly one ACP session. It deliberately
// does not use app.mcp: those clients belong to the REPL's global session and
// may capture its mutable current-session state.
type acpMCP struct {
	mu         sync.Mutex
	owner      string
	log        *session.Log
	clients    map[string]mcp.Client
	advertised map[string]acpMCPToolRef
	byServer   map[string][]mcp.Tool
	closed     bool
}

type acpMCPToolRef struct {
	server string
	client mcp.Client
	tool   mcp.Tool
}

func newACPMCP(ctx context.Context, a *app, owner string, log *session.Log) (*acpMCP, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f := a.mcpFactory
	if f == nil {
		f = mcp.NewStdioFactory()
	}
	s := &acpMCP{
		owner:      owner,
		log:        log,
		clients:    make(map[string]mcp.Client),
		advertised: make(map[string]acpMCPToolRef),
		byServer:   make(map[string][]mcp.Tool),
	}
	for _, srv := range a.cfg.Mcp.Servers {
		name := strings.TrimSpace(srv.Name)
		if name == "" {
			_ = s.Close()
			return nil, errors.New("ACP MCP server name must not be empty")
		}
		if strings.TrimSpace(srv.Cmd) == "" {
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP server %q command must not be empty", name)
		}
		if _, exists := s.clients[name]; exists {
			_ = s.Close()
			return nil, fmt.Errorf("ACP MCP server %q is configured more than once", name)
		}
		client, err := f.New(ctx, srv.Cmd, srv.Args)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("ACP MCP service is closed")
	}
	tools, ok := s.byServer[server]
	if !ok {
		return "", fmt.Errorf("mcp_list: unknown server %q", server)
	}
	if s.log != nil {
		_, _ = s.log.Append(session.EventMcpList, session.NewMcpList(len(tools)))
	}
	return formatACPMPCToolList(tools), nil
}

func (s *acpMCP) call(server, name string, args map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("ACP MCP service is closed")
	}
	client, ok := s.clients[server]
	if !ok {
		return "", fmt.Errorf("mcp_call: unknown server %q", server)
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("mcp_call: empty tool name")
	}
	result, err := client.Call(context.Background(), name, args)
	if err != nil {
		return "", fmt.Errorf("mcp_call: %s.%s: %w", server, name, err)
	}
	if s.log != nil {
		_, _ = s.log.Append(session.EventMcpCall, session.NewMcpCall(name, result.IsError))
	}
	return mcp.FormatCallResult(result), nil
}

func (s *acpMCP) callContext(ctx context.Context, server, name string, args map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("ACP MCP service is closed")
	}
	client, ok := s.clients[server]
	if !ok {
		return "", fmt.Errorf("mcp_call: unknown server %q", server)
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("mcp_call: empty tool name")
	}
	result, err := client.Call(ctx, name, args)
	if err != nil {
		return "", fmt.Errorf("mcp_call: %s.%s: %w", server, name, err)
	}
	if s.log != nil {
		_, _ = s.log.Append(session.EventMcpCall, session.NewMcpCall(name, result.IsError))
	}
	return mcp.FormatCallResult(result), nil
}

func (s *acpMCP) callAdvertised(ctx context.Context, fullName string, args map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("ACP MCP service is closed")
	}
	ref, ok := s.advertised[fullName]
	if !ok {
		return "", fmt.Errorf("unknown MCP tool %q", fullName)
	}
	result, err := ref.client.Call(ctx, ref.tool.Name, args)
	if err != nil {
		return "", fmt.Errorf("%s: %w", fullName, err)
	}
	if s.log != nil {
		_, _ = s.log.Append(session.EventMcpCall, session.NewMcpCall(ref.tool.Name, result.IsError))
	}
	return mcp.FormatCallResult(result), nil
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
func (t acpMCPTool) Execute(ctx context.Context, args any) (string, error) {
	var values map[string]any
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return "", fmt.Errorf("%s: %w", t.name, err)
	}
	return t.service.callAdvertised(ctx, t.name, values)
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
func (t acpMCPListTool) Execute(_ context.Context, args any) (string, error) {
	var values struct {
		Server string `json:"server"`
	}
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return "", fmt.Errorf("mcp_list: %w", err)
	}
	return t.service.list(values.Server)
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
	var values struct {
		Server string         `json:"server"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
	}
	if err := agenttools.DecodeArgs(args, &values); err != nil {
		return "", fmt.Errorf("mcp_call: %w", err)
	}
	return t.service.callContext(ctx, values.Server, values.Tool, values.Args)
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
