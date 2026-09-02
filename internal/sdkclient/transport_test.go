package sdkclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestLineTransportClosedBeforeWriteRemovesPending(t *testing.T) {
	transport := NewLineTransport(bytes.NewReader(nil), &bytes.Buffer{}, nil, nil)
	transport.writeMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := transport.Request(context.Background(), "blocked", json.RawMessage(`{}`), time.Second)
		result <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		transport.pendMu.Lock()
		pending := len(transport.pending)
		transport.pendMu.Unlock()
		if pending == 1 {
			break
		}
		if !time.Now().Before(deadline) {
			transport.writeMu.Unlock()
			t.Fatal("request was not registered before the close race")
		}
		time.Sleep(time.Millisecond)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	transport.writeMu.Unlock()

	select {
	case err := <-result:
		var closed *ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("request error = %v, want ClosedError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not settle after transport close")
	}
	transport.pendMu.Lock()
	defer transport.pendMu.Unlock()
	if len(transport.pending) != 0 {
		t.Fatalf("pending requests after close-before-write = %d, want 0", len(transport.pending))
	}
}

func TestLineTransportRequestNotificationAndErrors(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.Start()

	go func() {
		defer conn.Close()
		var request struct {
			ID     *string         `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		decoder := json.NewDecoder(server)
		if decoder.Decode(&request) != nil || request.ID == nil || request.Method != "echo" {
			return
		}
		_ = server.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = server.Write([]byte("not-json\n"))
		notification, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session.status", "params": map[string]string{"status": "idle"}})
		_, _ = server.Write(append(notification, '\n'))
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"echo": request.Params}})
		_, _ = server.Write(append(response, '\n'))
	}()

	result, err := transport.Request(context.Background(), "echo", json.RawMessage(`{"x":42}`), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Echo struct{ X int } `json:"echo"`
	}
	if json.Unmarshal(result, &decoded) != nil || decoded.Echo.X != 42 {
		t.Fatalf("response = %s", result)
	}
	_ = transport.Close()
}

func TestLineTransportTimeoutAbandonsLateResponse(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.Start()
	responses := make(chan string, 2)

	go func() {
		defer conn.Close()
		var first, second struct {
			ID string `json:"id"`
		}
		decoder := json.NewDecoder(server)
		if decoder.Decode(&first) != nil || decoder.Decode(&second) != nil {
			return
		}
		responses <- first.ID
		responses <- second.ID
		late, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": first.ID, "result": "late"})
		_, _ = server.Write(append(late, '\n'))
		answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": second.ID, "result": "current"})
		_, _ = server.Write(append(answer, '\n'))
	}()

	_, err := transport.Request(context.Background(), "first", json.RawMessage(`{}`), 20*time.Millisecond)
	var timeout *RequestTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("first error = %v, want RequestTimeoutError", err)
	}
	result, err := transport.Request(context.Background(), "second", json.RawMessage(`{}`), 2*time.Second)
	if err != nil || string(result) != `"current"` {
		t.Fatalf("second = (%s, %v), want current result", result, err)
	}
	if firstID, secondID := <-responses, <-responses; secondID <= firstID {
		t.Fatalf("request ids = %q then %q", firstID, secondID)
	}
	_ = transport.Close()
}

func TestLineTransportPreservesStructuredResponseError(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.Start()
	go func() {
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(server).Decode(&request) != nil {
			return
		}
		response, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   map[string]any{"code": 7, "message": "structured", "data": map[string]string{"detail": "x"}},
		})
		_, _ = server.Write(append(response, '\n'))
	}()

	_, err := transport.Request(context.Background(), "fail", json.RawMessage(`{}`), time.Second)
	var wireErr *ResponseError
	if !errors.As(err, &wireErr) || wireErr.Code != 7 || wireErr.Message != "structured" || string(wireErr.Data) != `{"detail":"x"}` {
		t.Fatalf("error = %#v, want preserved structured response error", err)
	}
	_ = transport.Close()
}
