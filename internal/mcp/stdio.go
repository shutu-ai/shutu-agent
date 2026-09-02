package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// JSON-RPC 2.0 / MCP protocol constants.
const (
	// protocolVersion is the MCP protocol version negotiated at initialize.
	protocolVersion = "2024-11-05"
	// clientName and clientVersion identify this client to the server.
	clientName    = "shutu-agent"
	clientVersion = "0.1.0"
	// jsonRPCErrMethodNotFound is the JSON-RPC 2.0 code for an unknown method;
	// a server answers tools/call for an unknown tool (or an unsupported
	// method) with it, which maps to ErrUnknownMethod.
	jsonRPCErrMethodNotFound = -32601
	// idStart is the first sequential request id.
	idStart = 1
)

// request is one outbound JSON-RPC frame. A nil ID makes it a notification
// (no response is expected); a non-nil ID is a request awaiting a matching
// response.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	// Result and Error are used only when Streamable HTTP replies to a
	// server-initiated request through a separate POST. Keeping them on the
	// shared wire envelope avoids a second JSON-RPC representation.
	Result any `json:"result,omitempty"`
	Error  any `json:"error,omitempty"`
}

// message is one inbound JSON-RPC frame: a response to our request (ID set,
// Method empty), a server error frame (Error set), or a server→client
// request/notification (Method set).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcError is a JSON-RPC 2.0 error object returned by the server.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// stdioClient is the default MCP Client: a child process speaking newline-
// delimited JSON-RPC 2.0 over stdin/stdout. All mutable state is guarded by
// mu. Requests are foreground and serial (D5) — one round trip at a time —
// each bounded by a per-request timeout. The response read is a synchronous
// newline scanner that runs inside a short-lived per-request reader goroutine
// (see readResponseLocked); that goroutine never outlives the client.
type stdioClient struct {
	mu          sync.Mutex
	cmd         string
	args        []string
	timeout     time.Duration // per-request timeout; 0 → DefaultTimeout
	env         []string      // extra environment entries (test helper mode)
	cwd         string
	callTimeout time.Duration

	proc    *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	reader  *bufio.Reader
	stderr  bytes.Buffer
	started bool
	closed  bool
	nextID  int64

	notifyMu          sync.Mutex
	notifyHandler     func()
	notifyCh          chan struct{}
	notifyStop        chan struct{}
	connectionHandler func(error)
	callbackWG        sync.WaitGroup
	closeDone         chan struct{}
	closeSignal       chan struct{}
	closeOnce         sync.Once
}

// newStdioClient returns an idle stdioClient for the given command.
func newStdioClient(cmd string, args []string) *stdioClient {
	return &stdioClient{
		cmd:         cmd,
		args:        args,
		timeout:     DefaultTimeout,
		nextID:      idStart,
		closeDone:   make(chan struct{}),
		closeSignal: make(chan struct{}),
	}
}

func newConfiguredStdioClient(server McpServer) *stdioClient {
	c := newStdioClient(server.Cmd, server.Args)
	c.cwd = server.Cwd
	c.callTimeout = server.ToolCallTimeout
	if c.callTimeout <= 0 {
		c.callTimeout = 60 * time.Second
	}
	if c.callTimeout > c.timeout {
		c.timeout = c.callTimeout
	}
	keys := make([]string, 0, len(server.Env))
	for key := range server.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c.env = append(c.env, key+"="+server.Env[key])
	}
	return c
}

// SetToolListChangedHandler installs the best-effort callback used for the
// MCP notifications/tools/list_changed notification. The callback is never
// run by the JSON-RPC reader while the client mutex is held; it is dispatched
// after the current response is consumed so a callback may safely call
// ListTools to refresh the advertised generation.
func (c *stdioClient) SetToolListChangedHandler(handler func()) {
	c.notifyMu.Lock()
	c.notifyHandler = handler
	c.notifyMu.Unlock()
}

func (c *stdioClient) SetConnectionLostHandler(handler func(error)) {
	c.notifyMu.Lock()
	c.connectionHandler = handler
	c.notifyMu.Unlock()
}

// Start launches the child process and performs the MCP initialize handshake.
// It is idempotent. A failed or lost connection is recoverable by calling Start
// again; only an explicit Close permanently retires the client.
func (c *stdioClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.started {
		return nil
	}
	if strings.TrimSpace(c.cmd) == "" {
		return fmt.Errorf("%w: empty command", ErrStartFailed)
	}

	// Wire the child's stdio with os.Pipe pairs we own so Close can release
	// every handle deterministically (no exec-internal copy goroutines on the
	// data paths). Windows os.Pipe handles do not support read deadlines (see
	// readResponseLocked), so timeouts are enforced by the request path, not by
	// pipe deadlines.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%w: create stdin pipe: %v", ErrStartFailed, err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return fmt.Errorf("%w: create stdout pipe: %v", ErrStartFailed, err)
	}

	proc := exec.Command(c.cmd, c.args...)
	proc.Dir = c.cwd
	proc.Stdin = stdinR
	proc.Stdout = stdoutW
	proc.Stderr = &c.stderr
	prepareProcessGroup(proc)
	// MCP servers are untrusted external processes.  Match the reference
	// transport boundary: remove credential-shaped ambient variables first,
	// then add only the explicit test/embedding environment requested by the
	// caller.  This prevents a server from inheriting the host's provider keys
	// merely because it was configured in the same process.
	proc.Env = append(scrubbedEnv(), c.env...)

	if err := proc.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("%w: %v", ErrStartFailed, err)
	}
	// The child owns its read ends from here on; we keep the write ends.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	c.proc = proc
	c.stdin = stdinW
	c.stdout = stdoutR
	c.reader = bufio.NewReader(stdoutR)
	c.started = true
	c.startNotificationDispatcherLocked()

	// MCP initialize handshake.
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}
	res, err := c.doRequestLocked(ctx, "initialize", params)
	if err != nil {
		c.disconnectLocked()
		return fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(res, &initRes); err != nil {
		c.disconnectLocked()
		return fmt.Errorf("%w: initialize result is not a JSON object: %v", ErrHandshake, err)
	}
	// Best-effort initialized notification (JSON-RPC notifications receive no
	// response; a write failure here only means the connection is gone).
	_ = c.sendNotificationLocked("notifications/initialized", map[string]any{})
	return nil
}

// ListTools returns the tools advertised by the server (tools/list). A tool
// with no inputSchema yields an empty, non-nil InputSchema.
func (c *stdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if !c.started {
		return nil, ErrNotStarted
	}
	tools := make([]Tool, 0)
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.doRequestLocked(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var list struct {
			Tools []struct {
				Name         string         `json:"name"`
				Description  string         `json:"description"`
				InputSchema  map[string]any `json:"inputSchema"`
				OutputSchema map[string]any `json:"outputSchema"`
				Execution    struct {
					TaskSupport string `json:"taskSupport"`
				} `json:"execution"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &list); err != nil {
			return nil, fmt.Errorf("%w: invalid tools/list result: %v", ErrProtocol, err)
		}
		for _, t := range list.Tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{}
			}
			tools = append(tools, Tool{
				Name: t.Name, Description: t.Description, InputSchema: schema,
				OutputSchema: t.OutputSchema, TaskSupport: t.Execution.TaskSupport,
			})
		}
		if list.NextCursor == "" || list.NextCursor == cursor {
			break
		}
		cursor = list.NextCursor
	}
	return tools, nil
}

// Call invokes the named tool with args (tools/call). A server error frame is
// surfaced as ErrServer (or ErrUnknownMethod for -32601); a tool reporting
// execution failure inside a successful result is returned normally with
// IsError set.
func (c *stdioClient) Call(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CallResult{}, ErrClosed
	}
	if !c.started {
		return CallResult{}, ErrNotStarted
	}
	if args == nil {
		args = map[string]any{}
	}
	callCtx := ctx
	cancel := func() {}
	if c.callTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, c.callTimeout)
	}
	defer cancel()
	res, err := c.doRequestLocked(callCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return CallResult{}, err
	}
	var callRes struct {
		Content           []any           `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &callRes); err != nil {
		return CallResult{}, fmt.Errorf("%w: invalid tools/call result: %v", ErrProtocol, err)
	}
	var structured any
	set := len(callRes.StructuredContent) != 0
	if set {
		if err := json.Unmarshal(callRes.StructuredContent, &structured); err != nil {
			return CallResult{}, fmt.Errorf("%w: invalid structuredContent: %v", ErrProtocol, err)
		}
	}
	return CallResult{Content: callRes.Content, StructuredContent: structured, StructuredContentSet: set, IsError: callRes.IsError}, nil
}

// Close kills the server process and releases every pipe. It is idempotent:
// the first call terminates and reaps the process, later calls return nil.
// After Close, Start/ListTools/Call are rejected with ErrClosed.
func (c *stdioClient) Close() error {
	// Signal cancellation before taking mu. Requests intentionally hold mu while
	// reading a serial stdio response; waiting for mu first would leave Close
	// unable to interrupt a server that never answers.
	c.closeOnce.Do(func() { close(c.closeSignal) })
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	c.cleanupLocked()
	done := c.closeDone
	c.mu.Unlock()
	// Notification and connection callbacks are deliberately dispatched outside
	// c.mu. Drain them before reporting Close complete: a list-changed callback
	// may still be refreshing a bridged tool generation, while a connection
	// callback may still be notifying the reconnect supervisor. Without this
	// barrier a composition root can close its registries while one of those
	// callbacks is still using them.
	c.callbackWG.Wait()
	if done != nil {
		close(done)
	}
	return nil
}

// cleanupLocked marks the client closed and releases the subprocess and all
// pipes. The caller must hold c.mu. It is idempotent; the process is killed
// (tree-kill on Unix, direct kill on Windows) and reaped via Wait, so no child
// or exec-internal goroutine is left behind.
func (c *stdioClient) cleanupLocked() {
	c.closed = true
	c.stopNotificationDispatcherLocked()
	c.teardownProcess()
	c.stdin = nil
	c.stdout = nil
	c.reader = nil
	c.started = false
}

func (c *stdioClient) startNotificationDispatcherLocked() {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	if c.notifyCh != nil {
		return
	}
	c.notifyCh = make(chan struct{}, 1)
	c.notifyStop = make(chan struct{})
	ch, stop := c.notifyCh, c.notifyStop
	c.callbackWG.Add(1)
	go func() {
		defer c.callbackWG.Done()
		for {
			select {
			case <-ch:
				c.notifyMu.Lock()
				handler := c.notifyHandler
				c.notifyMu.Unlock()
				if handler != nil {
					handler()
				}
			case <-stop:
				return
			}
		}
	}()
}

func (c *stdioClient) stopNotificationDispatcherLocked() {
	c.notifyMu.Lock()
	if c.notifyStop != nil {
		close(c.notifyStop)
	}
	c.notifyStop = nil
	c.notifyCh = nil
	c.notifyMu.Unlock()
}

func (c *stdioClient) signalToolListChanged() {
	c.notifyMu.Lock()
	ch := c.notifyCh
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	c.notifyMu.Unlock()
}

// disconnectLocked tears down a failed connection but leaves the client
// restartable. It is distinct from cleanupLocked: the latter is the explicit
// lifecycle terminal state exposed by Close.
func (c *stdioClient) disconnectLocked() {
	wasStarted := c.started
	c.teardownProcess()
	c.stdin = nil
	c.stdout = nil
	c.reader = nil
	c.started = false
	if wasStarted {
		c.notifyMu.Lock()
		handler := c.connectionHandler
		c.notifyMu.Unlock()
		if handler != nil {
			// The request path owns c.mu here. Dispatch outside the client lock
			// so a supervisor can call Start without deadlocking the failed call.
			c.callbackWG.Add(1)
			go func() {
				defer c.callbackWG.Done()
				handler(ErrConnection)
			}()
		}
	}
}

// teardownProcess kills the server process and closes the pipes without taking
// c.mu. It is called by the request path (on timeout or cancellation) to
// unblock the reader goroutine's synchronous read — the killed child's closed
// stdout makes the read return EOF — and by cleanupLocked. It is idempotent.
func (c *stdioClient) teardownProcess() {
	if c.proc != nil && c.proc.Process != nil {
		killTree(c.proc)
		_ = c.proc.Wait()
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
}

// doRequestLocked performs one JSON-RPC round trip. The caller must hold c.mu.
// The request context bounds the whole round trip: it is wrapped with the
// client's per-request timeout (DefaultTimeout when the caller gives none) and
// readResponseLocked returns as soon as the context expires, so a request never
// blocks past its deadline. When the connection is gone (timeout, cancellation,
// closed pipe) the client is disconnected so a later Start can establish a
// fresh session; only an explicit Close makes further calls fail with ErrClosed.
func (c *stdioClient) doRequestLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id := c.nextID
	c.nextID++
	payload, err := json.Marshal(request{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal request: %v", ErrProtocol, err)
	}
	payload = append(payload, '\n')
	if _, err := c.stdin.Write(payload); err != nil {
		c.disconnectLocked()
		return nil, fmt.Errorf("%w: write request: %v", ErrConnection, err)
	}

	res, err := c.readResponseLocked(ctx, id)
	if err != nil && connectionDead(err) {
		c.disconnectLocked()
	}
	return res, err
}

// connectionDead reports whether err means the stdio connection is gone (as
// opposed to a protocol-level failure on a still-live connection, after which
// the client stays usable).
func connectionDead(err error) bool {
	return errors.Is(err, ErrTimeout) ||
		errors.Is(err, ErrConnection) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// readResponseLocked waits for the response to id, bounded by ctx. The caller
// must hold c.mu.
//
// The response read is synchronous and newline-scanned (D5) but runs in a
// short-lived reader goroutine so the request goroutine can select on the
// context. Windows os.Pipe handles are not pollable — SetReadDeadline returns
// "file type does not support deadline" — so a blocking read cannot be
// interrupted by a deadline; instead, when ctx expires the connection is torn
// down (teardownProcess kills the server, whose closed stdout then unblocks
// the reader with EOF), the reader goroutine sends its outcome to the
// buffered channel and exits. The reader goroutine therefore never leaks: on a
// normal response it has already exited, and on a timeout it is unblocked by
// the teardown before readResponseLocked returns.
func (c *stdioClient) readResponseLocked(ctx context.Context, id int64) (json.RawMessage, error) {
	type outcome struct {
		res json.RawMessage
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := c.scanResponse(id)
		ch <- outcome{res: res, err: err}
	}()

	select {
	case o := <-ch:
		return o.res, o.err
	case <-ctx.Done():
		c.teardownProcess()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, ctx.Err()
	case <-c.closeSignal:
		c.teardownProcess()
		return nil, ErrClosed
	}
}

// scanResponse reads frames until the response for id arrives (or the
// connection dies), replying to server→client requests with a -32601 (method
// not found) error frame — v1 implements no server→client methods — and
// ignoring server notifications plus responses for other ids. It runs inside
// the reader goroutine and always sends exactly one outcome on its caller's
// buffered channel.
func (c *stdioClient) scanResponse(id int64) (json.RawMessage, error) {
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return nil, c.readErr(err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("%w: invalid frame: %v", ErrProtocol, err)
		}
		if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
			return nil, fmt.Errorf("%w: jsonrpc version %q", ErrProtocol, msg.JSONRPC)
		}
		if msg.Method != "" {
			// A server→client request or notification.
			if msg.Method == "notifications/tools/list_changed" {
				c.signalToolListChanged()
			}
			if msg.ID != nil {
				if msg.Method == "ping" {
					c.replyResultLocked(*msg.ID, map[string]any{})
				} else {
					c.replyErrorLocked(*msg.ID, jsonRPCErrMethodNotFound, "method not found")
				}
			}
			continue
		}
		if msg.ID == nil || *msg.ID != id {
			continue // a stale response or a response to nothing outstanding
		}
		if msg.Error != nil {
			return nil, c.rpcErr(msg.Error)
		}
		if len(msg.Result) == 0 {
			return nil, fmt.Errorf("%w: response has neither result nor error", ErrProtocol)
		}
		return msg.Result, nil
	}
}

// readErr maps a stdout read failure to a sentinel: EOF or any other pipe error
// means the connection is gone (ErrConnection). The per-request timeout is
// surfaced separately by readResponseLocked, not through the read error itself.
func (c *stdioClient) readErr(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: server closed stdout: %v", ErrConnection, err)
	}
	return fmt.Errorf("%w: read response: %v", ErrConnection, err)
}

// rpcErr converts a server error frame into a sentinel error: -32601 (method
// not found) maps to ErrUnknownMethod (e.g. tools/call for a tool the server
// does not know); every other code is wrapped as ErrServer with the server's
// code and message.
func (c *stdioClient) rpcErr(e *rpcError) error {
	if e.Code == jsonRPCErrMethodNotFound {
		return fmt.Errorf("%w: %s", ErrUnknownMethod, e.Message)
	}
	return fmt.Errorf("%w: code %d: %s", ErrServer, e.Code, e.Message)
}

// sendNotificationLocked writes a JSON-RPC notification (no id, no response
// expected). The caller must hold c.mu.
func (c *stdioClient) sendNotificationLocked(method string, params any) error {
	payload, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = c.stdin.Write(payload)
	return err
}

// replyErrorLocked answers a server→client request with a JSON-RPC error frame
// (v1 implements no server→client methods). Best-effort: a write failure means
// the connection is already gone. The caller must hold c.mu.
func (c *stdioClient) replyErrorLocked(id int64, code int, message string) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	_, _ = c.stdin.Write(payload)
}

// replyResultLocked answers a supported server-to-client request. The caller
// must hold c.mu so the response cannot interleave with a foreground request.
func (c *stdioClient) replyResultLocked(id int64, result any) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	_, _ = c.stdin.Write(payload)
}

// stdioFactory is the default Factory implementation.
type stdioFactory struct{}

// New returns a stdioClient for the given command, ready for Start. The ctx is
// accepted for signature parity with the Factory seam; construction itself is
// synchronous and cannot fail for a well-formed command line.
func (stdioFactory) New(ctx context.Context, cmd string, args []string) (Client, error) {
	return newStdioClient(cmd, args), nil
}

// NewHTTP implements the optional HTTPFactory seam. StdioFactory is the
// default factory for both transports so production wiring does not need a
// second composition-root implementation.
func (stdioFactory) NewHTTP(ctx context.Context, endpoint string, headers map[string]string) (Client, error) {
	return NewStreamableHTTPClient(endpoint, headers)
}

// NewConfigured is the default factory's complete transport-aware constructor.
func (stdioFactory) NewConfigured(ctx context.Context, server McpServer) (Client, error) {
	switch strings.ToLower(strings.TrimSpace(server.Transport)) {
	case "", "stdio":
		return newConfiguredStdioClient(server), nil
	case "streamable-http", "http", "https":
		return newConfiguredStreamableHTTPClient(server)
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q", server.Transport)
	}
}
