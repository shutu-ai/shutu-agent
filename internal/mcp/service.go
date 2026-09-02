// Package mcp defines the Model Context Protocol (MCP) capability seam
// (design.md §10 D2, ADR 2026-08-19-m6-agent-full.md 决策 M6f): a Client +
// Factory seam for talking to external MCP servers that expose tools. An MCP
// server is a separate process speaking JSON-RPC 2.0 over stdio (newline-
// delimited frames). The default implementation stdioClient (stdio.go) is
// self-implemented on the standard library alone — this is the first declared
// zero-new-dependency exception of M6 (ADR 决策 M6f: self-implement the MCP
// client rather than pulling an SDK; the SDK is revisited only if the protocol
// outgrows what the stdio client can express).
//
// Consumers (M6f-2's mcp_* tools and the composition-root wiring) depend only
// on the seam's interfaces (D2): the Client presents Start/ListTools/Call/
// Close, and the Factory builds clients from a command line. Swapping in a
// different MCP transport (HTTP, in-process, …) never touches consumer code.
//
// Lifecycle: a Client is created by NewStdioClient (or a Factory) and starts
// idle. Start launches the stdio subprocess and performs the MCP initialize
// handshake (idempotent). ListTools and Call are JSON-RPC round trips against
// the live server. Close kills the subprocess and releases every pipe and is
// idempotent; operations after Close are rejected with ErrClosed.
//
// Requests are foreground and serial (design.md §10 D5): exactly one JSON-RPC
// round trip is in flight at a time, and each round trip is bounded by a
// per-request timeout (DefaultTimeout when the caller gives no earlier
// deadline) enforced through a read deadline on the stdout pipe. The raw
// stdio client has no lifecycle supervisor; NewReconnectingClient adds one
// explicitly for long-lived bridges and Close drains it. The reader is
// a synchronous newline scanner. The raw client's request reader is drained
// by Close; the optional reconnect wrapper owns and joins its separate
// supervisor goroutine.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultTimeout bounds a single JSON-RPC round trip (Start/ListTools/Call)
// when the caller's context carries no earlier deadline (30s).
const DefaultTimeout = 30 * time.Second

// Tool is one tool advertised by the MCP server via tools/list. InputSchema is
// the tool's JSON Schema (a "type":"object" schema with a "properties" map),
// passed through verbatim from the server.
type Tool struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	// TaskSupport is the MCP execution.taskSupport declaration. Shutu's
	// foreground bridge intentionally refuses "required", matching DSH's
	// explicit unsupported-task error instead of silently changing semantics.
	TaskSupport string
}

// CallResult is the outcome of one tools/call. Content is the server's content
// list (each item is typically {"type":"text","text":…}); IsError is the
// server's isError flag — a normal tool execution that reports failure, not a
// transport/protocol failure (those are returned as errors).
type CallResult struct {
	Content              []any
	StructuredContent    any
	StructuredContentSet bool
	IsError              bool
}

// Client is one MCP server connection (design.md §10 D2). Implementations are
// used on the agent's serial path (D5), so every method is a foreground,
// blocking round trip with no background goroutines.
type Client interface {
	// Start launches the server process and completes the MCP initialize
	// handshake. It is idempotent: a started client returns nil; a closed
	// client returns ErrClosed; a spawn failure returns ErrStartFailed; a
	// rejected or malformed initialize returns ErrHandshake.
	Start(ctx context.Context) error
	// ListTools returns the tools the server advertises (tools/list).
	ListTools(ctx context.Context) ([]Tool, error)
	// Call invokes the named tool with args (tools/call) and returns the
	// server's result. A server error frame is surfaced as ErrServer (or
	// ErrUnknownMethod for a -32601 method-not-found frame); a tool that
	// reports execution failure inside a successful result is returned
	// normally with IsError set.
	Call(ctx context.Context, name string, args map[string]any) (CallResult, error)
	// Close kills the server process and releases all pipes. It is idempotent;
	// further operations are rejected with ErrClosed.
	Close() error
}

// ConfiguredFactory is an optional extension for factories that preserve the
// complete per-server configuration. The original New seam remains
// source-compatible for factories that only support command and args.
type ConfiguredFactory interface {
	NewConfigured(ctx context.Context, server McpServer) (Client, error)
}

// ToolListChangedHandler is an optional live-discovery seam. MCP servers may
// notify clients that their advertised tool set changed; consumers that
// publish bridged tools can then perform a fresh tools/list and replace the
// published generation. Clients without this optional interface retain the
// snapshot-at-start behavior.
type ToolListChangedHandler interface {
	SetToolListChangedHandler(func())
}

// ConnectionLostHandler is an optional transport lifecycle seam. A client
// invokes it after a live connection becomes unusable; implementations must
// not invoke the callback while holding their request mutex. The reconnect
// supervisor uses this signal to restore a connection without waiting for the
// next model tool call.
type ConnectionLostHandler interface {
	SetConnectionLostHandler(func(error))
}

// ReconnectedHandler is implemented by the reconnecting wrapper. Consumers
// use it to refresh dynamic tool schemas after a successful reconnect.
type ReconnectedHandler interface {
	SetReconnectedHandler(func())
}

// ReconnectExhaustedHandler is implemented by the reconnecting wrapper. The
// callback fires once when the outage budget is exhausted or the generation
// close barrier fails. Consumers that publish generation-scoped tools should
// withdraw them at this point; keeping stale tools callable would diverge from
// the reference supervisor's terminal-failure contract.
type ReconnectExhaustedHandler interface {
	SetReconnectExhaustedHandler(func())
}

// Factory builds Clients from a command line (design.md §10 D2). The default
// factory (NewStdioFactory) returns stdioClient instances; a different MCP
// transport implements Factory to swap the backend without touching consumers.
type Factory interface {
	// New returns a Client for the given command and args, ready for Start.
	// The returned client is idle until Start is called.
	New(ctx context.Context, cmd string, args []string) (Client, error)
}

// HTTPFactory is the optional transport extension implemented by factories
// that can build MCP Streamable HTTP clients. Keeping it separate from Factory
// preserves the existing stdio test seam and lets embedders deliberately
// reject transports they do not support.
type HTTPFactory interface {
	NewHTTP(ctx context.Context, endpoint string, headers map[string]string) (Client, error)
}

// ValidateServer enforces the reference MCP namespace and transport contract
// before a client is created. Empty Transport is the legacy stdio spelling.
func ValidateServer(server McpServer) error {
	if len(server.Name) < 1 || len(server.Name) > 32 {
		return fmt.Errorf("mcp: server name must contain 1-32 ASCII letters, digits, '_' or '-'")
	}
	for _, char := range server.Name {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return fmt.Errorf("mcp: server name %q contains an invalid character", server.Name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(server.Transport)) {
	case "", "stdio":
		if strings.TrimSpace(server.Cmd) == "" {
			return fmt.Errorf("mcp: stdio server %q command is required", server.Name)
		}
	case "streamable-http", "http", "https":
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("mcp: Streamable HTTP server %q URL is required", server.Name)
		}
	default:
		return fmt.Errorf("mcp: unsupported transport %q", server.Transport)
	}
	return nil
}

// NewClientForServer selects the configured MCP transport. An empty transport
// is the legacy stdio spelling. The helper is shared by the REPL and ACP so a
// server cannot accidentally use HTTP in one surface and stdio in another.
func NewClientForServer(ctx context.Context, f Factory, server McpServer) (Client, error) {
	if f == nil {
		return nil, errors.New("mcp: nil factory")
	}
	if err := ValidateServer(server); err != nil {
		return nil, err
	}
	if configured, ok := f.(ConfiguredFactory); ok {
		return configured.NewConfigured(ctx, server)
	}
	switch strings.ToLower(strings.TrimSpace(server.Transport)) {
	case "", "stdio":
		return f.New(ctx, server.Cmd, server.Args)
	case "streamable-http", "http", "https":
		hf, ok := f.(HTTPFactory)
		if !ok {
			return nil, fmt.Errorf("mcp: factory does not support Streamable HTTP")
		}
		return hf.NewHTTP(ctx, server.URL, server.Headers)
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q", server.Transport)
	}
}

// NewStdioClient returns the default Client for a stdio MCP server: the given
// command is spawned as a subprocess speaking newline-delimited JSON-RPC 2.0
// on stdin/stdout (zero new dependencies). The returned client is idle until
// Start launches the process and performs the initialize handshake.
func NewStdioClient(cmd string, args []string) Client {
	return newStdioClient(cmd, args)
}

// NewStdioFactory returns the default Factory that builds stdioClient
// instances.
func NewStdioFactory() Factory { return stdioFactory{} }

// Sentinel errors returned by the seam so callers can distinguish failures
// without parsing message text.
var (
	ErrNotStarted    = errors.New("mcp: client not started")
	ErrClosed        = errors.New("mcp: client closed")
	ErrStartFailed   = errors.New("mcp: process start failed")
	ErrHandshake     = errors.New("mcp: initialize handshake failed")
	ErrProtocol      = errors.New("mcp: protocol error")
	ErrTimeout       = errors.New("mcp: request timed out")
	ErrConnection    = errors.New("mcp: connection closed")
	ErrUnknownMethod = errors.New("mcp: unknown method")
	ErrServer        = errors.New("mcp: server error")
)
