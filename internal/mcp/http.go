package mcp

// Streamable HTTP MCP transport. This is intentionally implemented with the
// standard library so it has the same dependency and trust boundary as the
// stdio client. One client owns one MCP session generation: initialize creates
// the session, every request carries Mcp-Session-Id, and Close best-effort
// deletes the session before making the client terminal. Reconnect therefore
// creates a fresh protocol session instead of replaying a tool call into an
// old server generation.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxHTTPResponseBytes = 32 << 20

type streamableHTTPClient struct {
	mu          sync.Mutex
	endpoint    string
	headers     map[string]string
	http        *http.Client
	timeout     time.Duration
	callTimeout time.Duration

	started   bool
	closed    bool
	sessionID string
	// protocolVersion is the version negotiated by initialize and is sent on
	// every subsequent Streamable HTTP request.
	protocolVersion string
	nextID          int64

	notifyMu          sync.Mutex
	notifyHandler     func()
	connectionHandler func(error)
	callbackWG        sync.WaitGroup
	closeDone         chan struct{}
	closeCtx          context.Context
	closeCancel       context.CancelFunc
	closeOnce         sync.Once
}

// NewStreamableHTTPClient returns an idle MCP client for a Streamable HTTP
// endpoint. The endpoint is validated at construction; network I/O starts
// only when Start performs initialize.
func NewStreamableHTTPClient(endpoint string, headers map[string]string) (Client, error) {
	return newStreamableHTTPClient(endpoint, headers)
}

func newStreamableHTTPClient(endpoint string, headers map[string]string) (*streamableHTTPClient, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("mcp: invalid Streamable HTTP endpoint %q", endpoint)
	}
	copyHeaders := make(map[string]string, len(headers))
	for key, value := range headers {
		copyHeaders[key] = value
	}
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &streamableHTTPClient{
		endpoint:    endpoint,
		headers:     copyHeaders,
		http:        &http.Client{},
		timeout:     DefaultTimeout,
		nextID:      idStart,
		closeDone:   make(chan struct{}),
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
	}, nil
}

func newConfiguredStreamableHTTPClient(server McpServer) (*streamableHTTPClient, error) {
	c, err := newStreamableHTTPClient(server.URL, server.Headers)
	if err != nil {
		return nil, err
	}
	c.callTimeout = server.ToolCallTimeout
	if c.callTimeout <= 0 {
		c.callTimeout = 60 * time.Second
	}
	if c.callTimeout > c.timeout {
		c.timeout = c.callTimeout
	}
	return c, nil
}

func (c *streamableHTTPClient) SetToolListChangedHandler(handler func()) {
	c.notifyMu.Lock()
	c.notifyHandler = handler
	c.notifyMu.Unlock()
}

func (c *streamableHTTPClient) SetConnectionLostHandler(handler func(error)) {
	c.notifyMu.Lock()
	c.connectionHandler = handler
	c.notifyMu.Unlock()
}

func (c *streamableHTTPClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.started {
		return nil
	}
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}
	result, sessionID, err := c.postRequestLocked(ctx, &request{JSONRPC: "2.0", ID: ptrInt64(c.nextID), Method: "initialize", Params: params})
	c.nextID++
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &initRes); err != nil {
		c.disconnectLocked(fmt.Errorf("%w: initialize result is not a JSON object: %v", ErrHandshake, err))
		return fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	if sessionID != "" {
		c.sessionID = sessionID
	}
	if initRes.ProtocolVersion != "" {
		c.protocolVersion = initRes.ProtocolVersion
	} else {
		c.protocolVersion = protocolVersion
	}
	c.started = true
	if err := c.postNotificationLocked(ctx, "notifications/initialized", map[string]any{}); err != nil {
		c.disconnectLocked(err)
		return fmt.Errorf("%w: initialized notification: %v", ErrHandshake, err)
	}
	return nil
}

func ptrInt64(value int64) *int64 { return &value }

func (c *streamableHTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return nil, err
	}
	tools := make([]Tool, 0)
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, _, err := c.postRequestLocked(ctx, &request{JSONRPC: "2.0", ID: ptrInt64(c.nextID), Method: "tools/list", Params: params})
		c.nextID++
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
		if err := json.Unmarshal(result, &list); err != nil {
			return nil, fmt.Errorf("%w: invalid tools/list result: %v", ErrProtocol, err)
		}
		for _, tool := range list.Tools {
			schema := tool.InputSchema
			if schema == nil {
				schema = map[string]any{}
			}
			tools = append(tools, Tool{Name: tool.Name, Description: tool.Description, InputSchema: schema, OutputSchema: tool.OutputSchema, TaskSupport: tool.Execution.TaskSupport})
		}
		if list.NextCursor == "" || list.NextCursor == cursor {
			return tools, nil
		}
		cursor = list.NextCursor
	}
}

func (c *streamableHTTPClient) Call(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.readyLocked(); err != nil {
		return CallResult{}, err
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
	result, _, err := c.postRequestLocked(callCtx, &request{JSONRPC: "2.0", ID: ptrInt64(c.nextID), Method: "tools/call", Params: map[string]any{"name": name, "arguments": args}})
	c.nextID++
	if err != nil {
		return CallResult{}, err
	}
	var call struct {
		Content           []any           `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(result, &call); err != nil {
		return CallResult{}, fmt.Errorf("%w: invalid tools/call result: %v", ErrProtocol, err)
	}
	var structured any
	set := len(call.StructuredContent) != 0
	if set {
		if err := json.Unmarshal(call.StructuredContent, &structured); err != nil {
			return CallResult{}, fmt.Errorf("%w: invalid structuredContent: %v", ErrProtocol, err)
		}
	}
	return CallResult{Content: call.Content, StructuredContent: structured, StructuredContentSet: set, IsError: call.IsError}, nil
}

func (c *streamableHTTPClient) Close() error {
	// Cancel in-flight HTTP requests before taking mu. Request methods are
	// serialized under mu, so locking first would make Close wait for an
	// unresponsive server until the request timeout.
	c.closeOnce.Do(func() {
		if c.closeCancel != nil {
			c.closeCancel()
		}
	})
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		return nil
	}
	// Retire the client before the DELETE. A racing caller must not start a new
	// request or reconnect while the session is being closed.
	c.closed = true
	sessionID := c.sessionID
	started := c.started
	if started && sessionID != "" {
		_ = c.deleteSessionLocked()
	}
	c.started = false
	c.sessionID = ""
	c.mu.Unlock()
	c.callbackWG.Wait()
	close(c.closeDone)
	return nil
}

func (c *streamableHTTPClient) readyLocked() error {
	if c.closed {
		return ErrClosed
	}
	if !c.started {
		return ErrNotStarted
	}
	return nil
}

func (c *streamableHTTPClient) postNotificationLocked(ctx context.Context, method string, params any) error {
	_, _, err := c.roundTripLocked(ctx, http.MethodPost, &request{JSONRPC: "2.0", Method: method, Params: params}, false)
	return err
}

func (c *streamableHTTPClient) postRequestLocked(ctx context.Context, req *request) (json.RawMessage, string, error) {
	result, sessionID, err := c.roundTripLocked(ctx, http.MethodPost, req, true)
	if err != nil && httpConnectionError(err) {
		c.disconnectLocked(err)
	}
	return result, sessionID, err
}

func (c *streamableHTTPClient) deleteSessionLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Close has already canceled the client-wide request context so any
	// in-flight operation can unwind. The best-effort DELETE is a new cleanup
	// operation and must use its own bounded context, otherwise it would be
	// canceled before it is sent.
	_, _, err := c.roundTripWithoutCloseLocked(ctx, http.MethodDelete, nil, false)
	return err
}

func (c *streamableHTTPClient) roundTripLocked(ctx context.Context, method string, req *request, wantResponse bool) (json.RawMessage, string, error) {
	return c.roundTripWithCloseLocked(ctx, method, req, wantResponse, true)
}

func (c *streamableHTTPClient) roundTripWithoutCloseLocked(ctx context.Context, method string, req *request, wantResponse bool) (json.RawMessage, string, error) {
	return c.roundTripWithCloseLocked(ctx, method, req, wantResponse, false)
}

func (c *streamableHTTPClient) roundTripWithCloseLocked(ctx context.Context, method string, req *request, wantResponse bool, interruptOnClose bool) (json.RawMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stopClose := func() bool { return true }
	if interruptOnClose && c.closeCtx != nil {
		stopClose = context.AfterFunc(c.closeCtx, cancel)
		defer stopClose()
	}
	var body io.Reader
	if req != nil {
		encoded, err := json.Marshal(req)
		if err != nil {
			return nil, "", fmt.Errorf("%w: marshal request: %v", ErrProtocol, err)
		}
		body = bytes.NewReader(encoded)
	}
	requestHTTP, err := http.NewRequestWithContext(ctx, method, c.endpoint, body)
	if err != nil {
		return nil, "", fmt.Errorf("%w: create request: %v", ErrConnection, err)
	}
	for key, value := range c.headers {
		requestHTTP.Header.Set(key, value)
	}
	if req != nil {
		requestHTTP.Header.Set("Content-Type", "application/json")
		requestHTTP.Header.Set("Accept", "application/json, text/event-stream")
	}
	if c.sessionID != "" {
		requestHTTP.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocolVersion != "" && req != nil && req.Method != "initialize" {
		requestHTTP.Header.Set("MCP-Protocol-Version", c.protocolVersion)
	}
	response, err := c.http.Do(requestHTTP)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", ErrTimeout
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: HTTP request: %v", ErrConnection, err)
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	if readErr != nil {
		return nil, response.Header.Get("Mcp-Session-Id"), fmt.Errorf("%w: read HTTP response: %v", ErrConnection, readErr)
	}
	if len(data) > maxHTTPResponseBytes {
		return nil, response.Header.Get("Mcp-Session-Id"), fmt.Errorf("%w: HTTP response exceeds %d bytes", ErrProtocol, maxHTTPResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errType := ErrServer
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone || response.StatusCode >= 500 {
			errType = ErrConnection
		}
		return nil, response.Header.Get("Mcp-Session-Id"), fmt.Errorf("%w: HTTP status %s: %s", errType, response.Status, strings.TrimSpace(string(data)))
	}
	if !wantResponse {
		return nil, response.Header.Get("Mcp-Session-Id"), nil
	}
	if req == nil {
		return nil, response.Header.Get("Mcp-Session-Id"), nil
	}
	result, err := parseHTTPRPCResponse(data, response.Header.Get("Content-Type"), *req.ID, c.signalToolListChanged, c.handleServerRequest)
	return result, response.Header.Get("Mcp-Session-Id"), err
}

func parseHTTPRPCResponse(data []byte, contentType string, id int64, onNotification func(), onRequest func(int64, string, json.RawMessage)) (json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty HTTP response", ErrProtocol)
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("data:")) {
		return parseSSEResponseWithRequest(data, id, onNotification, onRequest)
	}
	return parseRPCMessageWithRequest(bytes.TrimSpace(data), id, onNotification, onRequest)
}

func parseSSEResponse(data []byte, id int64, onNotification func()) (json.RawMessage, error) {
	return parseSSEResponseWithRequest(data, id, onNotification, nil)
}

func parseSSEResponseWithRequest(data []byte, id int64, onNotification func(), onRequest func(int64, string, json.RawMessage)) (json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxHTTPResponseBytes)
	var event strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event.Len() > 0 {
				result, err := parseRPCMessageWithRequest([]byte(event.String()), id, onNotification, onRequest)
				if err == nil {
					return result, nil
				}
				if !errors.Is(err, errRPCMessageNotForRequest) {
					return nil, err
				}
				event.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if event.Len() > 0 {
				event.WriteByte('\n')
			}
			event.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scan event stream: %v", ErrProtocol, err)
	}
	if event.Len() > 0 {
		return parseRPCMessageWithRequest([]byte(event.String()), id, onNotification, onRequest)
	}
	return nil, fmt.Errorf("%w: event stream contained no response", ErrProtocol)
}

var errRPCMessageNotForRequest = errors.New("mcp: response belongs to another request")

func parseRPCMessage(data []byte, id int64, onNotification func()) (json.RawMessage, error) {
	return parseRPCMessageWithRequest(data, id, onNotification, nil)
}

func parseRPCMessageWithRequest(data []byte, id int64, onNotification func(), onRequest func(int64, string, json.RawMessage)) (json.RawMessage, error) {
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("%w: invalid HTTP JSON-RPC frame: %v", ErrProtocol, err)
	}
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		return nil, fmt.Errorf("%w: jsonrpc version %q", ErrProtocol, msg.JSONRPC)
	}
	if msg.Method != "" {
		// HTTP server->client notifications are delivered inside the response
		// stream. Requests cannot be answered asynchronously by this foreground
		// client, so only the supported list-changed notification is consumed.
		if msg.Method == "notifications/tools/list_changed" && onNotification != nil {
			onNotification()
		}
		if msg.ID != nil && onRequest != nil {
			onRequest(*msg.ID, msg.Method, msg.Params)
		}
		return nil, errRPCMessageNotForRequest
	}
	if msg.ID == nil || *msg.ID != id {
		return nil, errRPCMessageNotForRequest
	}
	if msg.Error != nil {
		if msg.Error.Code == jsonRPCErrMethodNotFound {
			return nil, fmt.Errorf("%w: %s", ErrUnknownMethod, msg.Error.Message)
		}
		return nil, fmt.Errorf("%w: code %d: %s", ErrServer, msg.Error.Code, msg.Error.Message)
	}
	if len(msg.Result) == 0 {
		return nil, fmt.Errorf("%w: response has neither result nor error", ErrProtocol)
	}
	return msg.Result, nil
}

func httpConnectionError(err error) bool {
	// A caller cancelling one HTTP request does not retire the MCP session.
	// The request context is intentionally narrower than the transport
	// generation; only an actual transport error or the client's own bounded
	// timeout makes the generation unusable. This also prevents the reconnect
	// wrapper from replacing a healthy session after an ordinary user abort.
	return errors.Is(err, ErrConnection) || errors.Is(err, ErrTimeout)
}

func (c *streamableHTTPClient) disconnectLocked(reason error) {
	if !c.started {
		return
	}
	c.started = false
	c.sessionID = ""
	c.notifyMu.Lock()
	handler := c.connectionHandler
	c.notifyMu.Unlock()
	if handler != nil && !c.closed {
		c.callbackWG.Add(1)
		go func() {
			defer c.callbackWG.Done()
			handler(reason)
		}()
	}
}

// signalToolListChanged is intentionally best-effort and asynchronous: the
// caller may be holding the serialized request mutex while parsing an SSE
// response, while the consumer callback may immediately call ListTools.
func (c *streamableHTTPClient) signalToolListChanged() {
	c.notifyMu.Lock()
	handler := c.notifyHandler
	c.notifyMu.Unlock()
	if handler == nil || c.closed {
		return
	}
	c.callbackWG.Add(1)
	go func() {
		defer c.callbackWG.Done()
		handler()
	}()
}

// handleServerRequest answers the protocol-level requests that arrive inside
// a Streamable HTTP response stream. MCP ping is required for transport
// liveness; unsupported requests receive the same explicit -32601 response as
// the stdio transport. The reply is a separate POST because the original HTTP
// response is already being consumed.
func (c *streamableHTTPClient) handleServerRequest(id int64, method string, _ json.RawMessage) {
	c.callbackWG.Add(1)
	go func() {
		defer c.callbackWG.Done()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed || !c.started {
			return
		}
		response := &request{JSONRPC: "2.0", ID: ptrInt64(id)}
		if method == "ping" {
			response.Result = map[string]any{}
		} else {
			response.Error = map[string]any{"code": jsonRPCErrMethodNotFound, "message": "method not found"}
		}
		_, _, _ = c.roundTripLocked(context.Background(), http.MethodPost, response, false)
	}()
}

// stdio and HTTP share the wire message parser, but HTTP's SSE parser needs
// to observe notifications. Keep a tiny scanner-aware wrapper for the only
// supported notification rather than turning the foreground client into a
// permanently running reader goroutine.
var _ Client = (*streamableHTTPClient)(nil)
