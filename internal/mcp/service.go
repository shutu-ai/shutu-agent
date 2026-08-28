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
// deadline) enforced through a read deadline on the stdout pipe. The reader is
// a synchronous newline scanner — there are no background goroutines, so Close
// cannot leak one (the sole goroutine exec.Cmd may spawn for stderr capture is
// drained by Wait inside Close).
package mcp

import (
	"context"
	"errors"
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

// Factory builds Clients from a command line (design.md §10 D2). The default
// factory (NewStdioFactory) returns stdioClient instances; a different MCP
// transport implements Factory to swap the backend without touching consumers.
type Factory interface {
	// New returns a Client for the given command and args, ready for Start.
	// The returned client is idle until Start is called.
	New(ctx context.Context, cmd string, args []string) (Client, error)
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
