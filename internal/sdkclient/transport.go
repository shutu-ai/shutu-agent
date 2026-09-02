// DTO and wire transport for the DeepSeek Harness SDK runtime.
//
// This package is deliberately transport-neutral: the same LineTransport can
// speak to an in-memory peer or to the runtime subprocess owned by Client.
package sdkclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const maxWireLineBytes = 4 << 20

// ResponseError preserves a JSON-RPC error response exactly as the runtime
// reported it, including optional structured data.
type ResponseError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *ResponseError) Error() string {
	if len(e.Data) != 0 {
		return fmt.Sprintf("sdk request %d failed: %s: %s", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("sdk request %d failed: %s", e.Code, e.Message)
}

// RequestTimeoutError reports that the configured request bound elapsed. The
// runtime request is not wire-cancellable and may still complete later.
type RequestTimeoutError struct{ Method string }

func (e *RequestTimeoutError) Error() string { return "sdk request timed out: " + e.Method }

// ClosedError reports that no runtime transport is available.
type ClosedError struct{ Reason string }

func (e *ClosedError) Error() string { return "sdk transport closed: " + e.Reason }

// Notification is one server-to-client frame.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *wireRPCError   `json:"error,omitempty"`
}

type wireRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type pendingResponse struct {
	result json.RawMessage
	err    error
}

// LineTransport sends and receives newline-delimited compact JSON-RPC 2.0
// frames. Close detaches the transport and fails pending requests without
// closing caller-owned byte streams.
type LineTransport struct {
	in  io.Reader
	out io.Writer

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[string]chan pendingResponse

	nextID    atomic.Uint64
	startOnce sync.Once
	closeOnce sync.Once
	closed    chan struct{}
	onNotify  func(Notification)
	onRequest RequestHandler
	onClosed  func()
	readDone  chan struct{}
	closeErr  error
}

// RequestHandler answers a server-to-client JSON-RPC request. It runs on the
// transport reader goroutine and must not call back into the transport.
type RequestHandler func(method string, params json.RawMessage) (json.RawMessage, error)

// NewLineTransport creates a transport over a caller-owned read/write pair.
// Notification and closed callbacks are invoked on the transport reader
// goroutine and must not call back into the transport.
func NewLineTransport(in io.Reader, out io.Writer, onNotify func(Notification), onClosed func()) *LineTransport {
	return &LineTransport{
		in:       in,
		out:      out,
		pending:  make(map[string]chan pendingResponse),
		closed:   make(chan struct{}),
		readDone: make(chan struct{}),
		onNotify: onNotify,
		onClosed: onClosed,
	}
}

// OnRequest installs the reverse-request handler. Install it before Start.
func (t *LineTransport) OnRequest(handler RequestHandler) { t.onRequest = handler }

// Start begins reading server frames. It is idempotent.
func (t *LineTransport) Start() {
	t.startOnce.Do(func() { go t.readLoop() })
}

func (t *LineTransport) readLoop() {
	defer close(t.readDone)
	lines := bufio.NewScanner(t.in)
	lines.Buffer(make([]byte, 4096), maxWireLineBytes)
	for lines.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *wireRPCError   `json:"error"`
		}
		if json.Unmarshal(lines.Bytes(), &frame) != nil {
			// Malformed peer lines are transport noise and do not tear down an
			// otherwise usable runtime connection.
			continue
		}
		if frame.Method != "" {
			if len(frame.ID) != 0 {
				t.answerReverseRequest(frame.ID, frame.Method, normalizeRequestParams(frame.Params))
				continue
			}
			params := frame.Params
			if len(params) == 0 || string(params) == "null" {
				params = json.RawMessage("{}")
			}
			if t.onNotify != nil {
				t.onNotify(Notification{Method: frame.Method, Params: params})
			}
			continue
		}
		if len(frame.ID) == 0 {
			continue
		}
		t.settle(requestIDKey(frame.ID), pendingResponse{result: frame.Result, err: wireError(frame.Error)})
	}
	if err := lines.Err(); err != nil {
		_ = t.closeWithError(err)
		return
	}
	_ = t.closeWithError(nil)
}

func requestIDKey(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if id, ok := value.(string); ok {
			return id
		}
	}
	return string(raw)
}

func normalizeRequestParams(params json.RawMessage) json.RawMessage {
	var value any
	if json.Unmarshal(params, &value) != nil {
		return json.RawMessage("{}")
	}
	if _, ok := value.(map[string]any); !ok {
		return json.RawMessage("{}")
	}
	return params
}

func (t *LineTransport) answerReverseRequest(id json.RawMessage, method string, params json.RawMessage) {
	result, err := t.invokeRequestHandler(method, params)
	if err == nil {
		_ = t.writeResponse(wireResponse{JSONRPC: "2.0", ID: id, Result: result})
		return
	}
	responseErr := &ResponseError{Code: -32603, Message: err.Error()}
	var structured *ResponseError
	if errors.As(err, &structured) {
		responseErr = structured
	}
	_ = t.writeResponse(wireResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &wireRPCError{Code: responseErr.Code, Message: responseErr.Message, Data: responseErr.Data},
	})
}

func (t *LineTransport) invokeRequestHandler(method string, params json.RawMessage) (result json.RawMessage, err error) {
	if t.onRequest == nil {
		return nil, &ResponseError{Code: -32601, Message: "method not found: " + method}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = &ResponseError{Code: -32603, Message: fmt.Sprintf("request handler panicked: %v", recovered)}
		}
	}()
	return t.onRequest(method, params)
}

func (t *LineTransport) writeResponse(response wireResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.out.Write(append(encoded, '\n'))
	return err
}

func wireError(err *wireRPCError) error {
	if err == nil {
		return nil
	}
	data := append(json.RawMessage(nil), err.Data...)
	return &ResponseError{Code: err.Code, Message: err.Message, Data: data}
}

func (t *LineTransport) settle(id string, value pendingResponse) {
	t.pendMu.Lock()
	ch := t.pending[id]
	delete(t.pending, id)
	t.pendMu.Unlock()
	if ch != nil {
		ch <- value
	}
}

// Request sends method and waits for its response. A nil params value sends no
// params field. Cancellation and timeout remove the pending entry; a late
// response is discarded rather than resolving a later request.
func (t *LineTransport) Request(ctx context.Context, method string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("go-%d", t.nextID.Add(1))
	ch := make(chan pendingResponse, 1)
	t.pendMu.Lock()
	select {
	case <-t.closed:
		t.pendMu.Unlock()
		return nil, &ClosedError{Reason: t.closeReason()}
	default:
	}
	t.pending[id] = ch
	t.pendMu.Unlock()

	frame := wireRequest{JSONRPC: "2.0", ID: id, Method: method, Params: normalizeParams(params)}
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.removePending(id)
		return nil, err
	}
	t.writeMu.Lock()
	select {
	case <-t.closed:
		t.removePending(id)
		t.writeMu.Unlock()
		return nil, &ClosedError{Reason: t.closeReason()}
	default:
	}
	_, err = t.out.Write(append(encoded, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		t.removePending(id)
		return nil, err
	}

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}
	select {
	case value := <-ch:
		return value.result, value.err
	case <-ctx.Done():
		t.removePending(id)
		return nil, ctx.Err()
	case <-timeoutCh:
		t.removePending(id)
		return nil, &RequestTimeoutError{Method: method}
	}
}

func normalizeParams(params json.RawMessage) json.RawMessage {
	if params == nil || string(params) == "null" {
		return nil
	}
	return params
}

func (t *LineTransport) removePending(id string) {
	t.pendMu.Lock()
	delete(t.pending, id)
	t.pendMu.Unlock()
}

// Notify sends a notification without params.
func (t *LineTransport) Notify(method string, params json.RawMessage) error {
	frame := wireRequest{JSONRPC: "2.0", Method: method, Params: normalizeParams(params)}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	select {
	case <-t.closed:
		return &ClosedError{Reason: t.closeReason()}
	default:
	}
	_, err = t.out.Write(append(encoded, '\n'))
	return err
}

// Close detaches the reader, fails all pending requests, and invokes the
// closed callback exactly once. It intentionally does not close either stream.
func (t *LineTransport) Close() error { return t.closeWithError(nil) }

func (t *LineTransport) closeWithError(err error) error {
	var closeErr error
	t.closeOnce.Do(func() {
		if err != nil {
			closeErr = fmt.Errorf("sdk transport failed: %w", err)
			t.closeErr = err
		}
		close(t.closed)
		t.pendMu.Lock()
		pending := t.pending
		t.pending = make(map[string]chan pendingResponse)
		t.pendMu.Unlock()
		for _, ch := range pending {
			ch <- pendingResponse{err: &ClosedError{Reason: t.closeReason()}}
		}
		if t.onClosed != nil {
			t.onClosed()
		}
	})
	return closeErr
}

func (t *LineTransport) closeReason() string {
	if t.closeErr != nil {
		return t.closeErr.Error()
	}
	return "read loop ended"
}

// Done settles when the read loop has stopped.
func (t *LineTransport) Done() <-chan struct{} { return t.readDone }
