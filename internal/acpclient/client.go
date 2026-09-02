// Package acpclient is an independent newline-delimited JSON-RPC client for
// the ACP wire. It deliberately does not import internal/acp: the server owns
// transport frames, while this client owns its own peer vocabulary, pending
// requests, notifications, and reverse permission routing.
package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxFrameBytes = 4 << 20

// ProtocolError reports a frame that violates the peer contract.
type ProtocolError struct{ Detail string }

func (e *ProtocolError) Error() string { return "acp client protocol violation: " + e.Detail }

// ClosedError reports that the peer transport is no longer live.
type ClosedError struct{ Reason string }

func (e *ClosedError) Error() string { return "acp client transport closed: " + e.Reason }

// RPCError preserves a JSON-RPC error returned by the peer.
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	if len(e.Data) != 0 {
		return fmt.Sprintf("acp request failed: %s: %s", e.Message, e.Data)
	}
	return "acp request failed: " + e.Message
}

// Notification is a server-to-client notification.
type Notification struct {
	Method string
	Params json.RawMessage
}

// ContentBlock is the client-side prompt wire block.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Update is one agent_message_chunk delivered by session/update.
type Update struct {
	SessionID string       `json:"sessionId"`
	Type      string       `json:"sessionUpdate"`
	Content   ContentBlock `json:"content"`
}

type wireUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string       `json:"sessionUpdate"`
		Content       ContentBlock `json:"content"`
	} `json:"update"`
}

// PermissionRequest is the typed server-to-client approval request.
type PermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string `json:"toolCallId"`
		Name       string `json:"name"`
	} `json:"toolCall"`
	Reason  string           `json:"reason"`
	Options []PermissionItem `json:"options"`
}

// PermissionItem is one option offered by the server.
type PermissionItem struct {
	ID    string `json:"optionId"`
	Label string `json:"name"`
}

// PermissionOutcome is the client response to a permission request.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// PermissionHandler answers a server-initiated permission request.
type PermissionHandler func(PermissionRequest) (PermissionOutcome, error)

// StopReason is the prompt settlement reason.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled"
)

type wireRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type wireFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireRPCError   `json:"error,omitempty"`
}

type pending struct {
	result json.RawMessage
	err    error
}

// Client is one independent client-side peer over caller-owned streams.
type Client struct {
	in        io.Reader
	out       io.Writer
	requests  chan<- clientRequest
	notify    chan Notification
	onRequest PermissionHandler

	startOnce sync.Once
	closeOnce sync.Once
	started   atomic.Bool
	closed    chan struct{}
	readDone  chan struct{}
	closeErrMu sync.Mutex
	closeErr  error

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[string]chan pending
	id      atomic.Uint64
}

type clientRequest struct {
	ctx      context.Context
	method   string
	params   json.RawMessage
	timeout  time.Duration
	response chan pending
}

// New creates a client over stdin/stdout halves owned by the caller. Closing
// the Client detaches pending requests but does not close these streams.
func New(in io.Reader, out io.Writer) *Client {
	return &Client{
		in:       in,
		out:      out,
		closed:   make(chan struct{}),
		readDone: make(chan struct{}),
		pending:  make(map[string]chan pending),
	}
}

// OnPermission installs the reverse permission handler before Start.
func (c *Client) OnPermission(handler PermissionHandler) error {
	if c.started.Load() {
		return errors.New("acp permission handler must be installed before start")
	}
	c.onRequest = handler
	return nil
}

// Start begins the independent read/dispatch loop.
func (c *Client) Start() error {
	if c.started.Swap(true) {
		return errors.New("acp client is already started")
	}
	requests := make(chan clientRequest, 16)
	notifications := make(chan Notification, 256)
	c.requests = requests
	c.notify = notifications
	go c.readLoop()
	go c.writeLoop(requests)
	return nil
}

// Notifications exposes notifications not consumed by a typed method.
func (c *Client) Notifications() <-chan Notification {
	return c.notify
}

// Close fails pending work and stops the client's goroutines. It does not
// close caller-owned byte streams.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.pendMu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			ch <- pending{err: &ClosedError{Reason: "client closed"}}
		}
		c.pendMu.Unlock()
	})
	c.closeErrMu.Lock()
	err := c.closeErr
	c.closeErrMu.Unlock()
	return err
}

func (c *Client) readLoop() {
	defer close(c.readDone)
	lines := bufio.NewScanner(c.in)
	lines.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		if !lines.Scan() {
			c.closeWithError(lines.Err())
			return
		}
		var frame wireFrame
		if err := json.Unmarshal(lines.Bytes(), &frame); err != nil {
			c.closeWithError(&ProtocolError{Detail: "invalid JSON frame"})
			return
		}
		switch {
		case frame.Method != "" && len(frame.ID) != 0:
			c.answerPermission(string(frame.ID), frame.Method, frame.Params)
		case frame.Method != "":
			c.deliverNotification(frame.Method, frame.Params)
		case len(frame.ID) != 0:
			c.settle(frame.ID, frame.Result, frame.Error)
		default:
			c.closeWithError(&ProtocolError{Detail: "frame has neither id nor method"})
			return
		}
	}
}

func (c *Client) writeLoop(requests <-chan clientRequest) {
	for {
		select {
		case <-c.closed:
			return
		case request := <-requests:
			if err := request.ctx.Err(); err != nil {
				request.response <- pending{err: err}
				continue
			}
			id := fmt.Sprintf("acp-client-%d", c.id.Add(1))
			ch := make(chan pending, 1)
			c.pendMu.Lock()
			c.pending[id] = ch
			c.pendMu.Unlock()
			encoded, err := json.Marshal(wireFrame{
				JSONRPC: "2.0",
				ID:      json.RawMessage(`"` + id + `"`),
				Method:  request.method,
				Params:  request.params,
			})
			if err != nil {
				c.removePending(id)
				request.response <- pending{err: err}
				continue
			}
			c.writeMu.Lock()
			_, err = c.out.Write(append(encoded, '\n'))
			c.writeMu.Unlock()
			if err != nil {
				c.removePending(id)
				request.response <- pending{err: err}
				continue
			}
			timeout := request.timeout
			if timeout <= 0 {
				timeout = 10 * time.Second
			}
			timer := time.NewTimer(timeout)
			var value pending
			select {
			case value = <-ch:
			case <-request.ctx.Done():
				value = pending{err: request.ctx.Err()}
			case <-timer.C:
				value = pending{err: &ProtocolError{Detail: "request timed out: " + request.method}}
			case <-c.closed:
				value = pending{err: &ClosedError{Reason: "client closed during request"}}
			}
			timer.Stop()
			c.removePending(id)
			request.response <- value
		}
	}
}

func (c *Client) answerPermission(id, method string, raw json.RawMessage) {
	if method != "session/request_permission" || c.onRequest == nil {
		c.writeFrame(wireFrame{JSONRPC: "2.0", ID: json.RawMessage(id), Error: &wireRPCError{Code: -32601, Message: "method not found"}})
		return
	}
	var request PermissionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		c.writeFrame(wireFrame{JSONRPC: "2.0", ID: json.RawMessage(id), Error: &wireRPCError{Code: -32602, Message: "invalid permission request"}})
		return
	}
	outcome, err := c.onRequest(request)
	if err != nil {
		c.writeFrame(wireFrame{JSONRPC: "2.0", ID: json.RawMessage(id), Error: &wireRPCError{Code: -32603, Message: err.Error()}})
		return
	}
	result, err := json.Marshal(outcome)
	if err != nil {
		c.writeFrame(wireFrame{JSONRPC: "2.0", ID: json.RawMessage(id), Error: &wireRPCError{Code: -32603, Message: err.Error()}})
		return
	}
	c.writeFrame(wireFrame{JSONRPC: "2.0", ID: json.RawMessage(id), Result: result})
}

func (c *Client) deliverNotification(method string, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	select {
	case c.notify <- Notification{Method: method, Params: raw}:
	case <-c.closed:
	}
}

func (c *Client) settle(id, result json.RawMessage, rpcErr *wireRPCError) {
	value := pending{result: result}
	if rpcErr != nil {
		value.err = &RPCError{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data}
	}
	key := wireID(id)
	c.pendMu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.pendMu.Unlock()
	if ch != nil {
		ch <- value
	}
}

func (c *Client) removePending(id string) {
	c.pendMu.Lock()
	delete(c.pending, id)
	c.pendMu.Unlock()
}

func (c *Client) writeFrame(frame wireFrame) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.out.Write(append(encoded, '\n'))
}

func (c *Client) closeWithError(err error) {
	if err != nil {
		c.closeErrMu.Lock()
		c.closeErr = err
		c.closeErrMu.Unlock()
	}
	_ = c.Close()
}

func wireID(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return string(raw)
}

// Request sends one JSON-RPC request and unmarshals a nonempty result.
func (c *Client) Request(ctx context.Context, method string, params, result any) error {
	if !c.started.Load() {
		return &ClosedError{Reason: "client is not started"}
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	response := make(chan pending, 1)
	select {
	case c.requests <- clientRequest{ctx: ctx, method: method, params: encoded, response: response}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return &ClosedError{Reason: "client closed"}
	}
	select {
	case value := <-response:
		if value.err != nil {
			return value.err
		}
		if result == nil || len(value.result) == 0 || string(value.result) == "null" {
			return nil
		}
		return json.Unmarshal(value.result, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return &ClosedError{Reason: "client closed"}
	}
}

// Initialize performs the ACP initialize handshake.
func (c *Client) Initialize(ctx context.Context) (map[string]any, error) {
	result := map[string]any{}
	if err := c.Request(ctx, "initialize", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Authenticate performs the optional ACP authenticate handshake.
func (c *Client) Authenticate(ctx context.Context) (map[string]any, error) {
	result := map[string]any{}
	if err := c.Request(ctx, "authenticate", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// NewSession creates a session and returns its wire identity.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var result struct {
		SessionID string `json:"sessionId"`
	}
	params := map[string]any{"cwd": cwd}
	if err := c.Request(ctx, "session/new", params, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.SessionID) == "" {
		return "", &ProtocolError{Detail: "session/new omitted sessionId"}
	}
	return result.SessionID, nil
}

// Reconnect replaces the addressed session's runtime from durable state.
func (c *Client) Reconnect(ctx context.Context, sessionID string) (map[string]any, error) {
	result := map[string]any{}
	params := map[string]any{"sessionId": sessionID}
	if err := c.Request(ctx, "session/reconnect", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Prompt sends one prompt, collects agent message chunks, and waits for its
// settlement. It must not be called concurrently for the same session.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, onUpdate func(Update)) (StopReason, error) {
	var result struct {
		StopReason StopReason `json:"stopReason"`
	}
	params := map[string]any{"sessionId": sessionID, "prompt": blocks}
	updates := make(chan Update, 256)
	response := make(chan error, 1)
	go func() {
		response <- c.Request(ctx, "session/prompt", params, &result)
	}()
collect:
	for {
		select {
		case notification := <-c.notify:
			if notification.Method != "session/update" {
				continue
			}
			var update wireUpdate
			if err := json.Unmarshal(notification.Params, &update); err != nil {
				return "", err
			}
			updates <- Update{SessionID: update.SessionID, Type: update.Update.SessionUpdate, Content: update.Update.Content}
		case err := <-response:
			if err != nil {
				return "", err
			}
			break collect
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	for {
		select {
		case notification := <-c.notify:
			if notification.Method != "session/update" {
				continue
			}
			var update wireUpdate
			if err := json.Unmarshal(notification.Params, &update); err != nil {
				return "", err
			}
			updates <- Update{SessionID: update.SessionID, Type: update.Update.SessionUpdate, Content: update.Update.Content}
			continue
		default:
		}
		break
	}
	close(updates)
	for update := range updates {
		if onUpdate != nil {
			onUpdate(update)
		}
	}
	if result.StopReason == "" {
		return "", &ProtocolError{Detail: "prompt response omitted stopReason"}
	}
	return result.StopReason, nil
}

// Cancel sends the idempotent cancellation notification.
func (c *Client) Cancel(sessionID string) error {
	return c.writeFrameAndWaitNothing("session/cancel", map[string]any{"sessionId": sessionID})
}

func (c *Client) writeFrameAndWaitNothing(method string, params any) error {
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(wireFrame{JSONRPC: "2.0", Method: method, Params: encodedParams})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.out.Write(append(encoded, '\n'))
	return err
}
