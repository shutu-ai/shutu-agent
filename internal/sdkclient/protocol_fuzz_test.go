package sdkclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLineTransportHostileFramesDoNotPoisonPendingRequest(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	notifications := make(chan Notification, 4)
	transport := NewLineTransport(conn, conn, func(notification Notification) {
		notifications <- notification
	}, nil)
	transport.Start()
	resultCh := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := transport.Request(context.Background(), "ping", json.RawMessage(`{}`), 3*time.Second)
		resultCh <- struct {
			raw json.RawMessage
			err error
		}{raw, err}
	}()

	serverResult := make(chan error, 1)
	go func() {
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(server).Decode(&request) != nil {
			serverResult <- errors.New("server failed to read ping")
			return
		}
		_ = server.SetDeadline(time.Now().Add(3 * time.Second))
		for _, line := range []string{
			"not-json",
			"null",
			`{"jsonrpc":"2.0","params":{}}`,
			`{"jsonrpc":"2.0","id":"unknown","result":"wrong"}`,
			`{"jsonrpc":"2.0","method":"tick"}`,
			`{"jsonrpc":"2.0","id":12,"method":"reverse","params":[]}`,
		} {
			_, _ = server.Write([]byte(line + "\n"))
		}
		var callback struct {
			ID     *int            `json:"id"`
			Error  *wireRPCError   `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(server).Decode(&callback) != nil || callback.ID == nil || *callback.ID != 12 ||
			callback.Error == nil || callback.Error.Code != -32601 || !strings.Contains(callback.Error.Message, "reverse") {
			serverResult <- errors.New("reverse request was not answered with method-not-found")
			return
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "correct"})
		_, _ = server.Write(append(response, '\n'))
		serverResult <- nil
	}()

	select {
	case notification := <-notifications:
		if notification.Method != "tick" || string(notification.Params) != "{}" {
			t.Fatalf("hostile notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("valid notification was lost among hostile frames")
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if result.err != nil || string(result.raw) != `"correct"` {
		t.Fatalf("pending result = (%s, %v)", result.raw, result.err)
	}
	_ = transport.Close()
}

func TestLineTransportReverseRequestHandlerNormalizesParamsAndPreservesError(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.OnRequest(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "callback" || string(params) != "{}" {
			return nil, &ResponseError{Code: 8, Message: "bad params", Data: json.RawMessage(`{"seen":true}`)}
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	transport.Start()
	done := make(chan error, 1)
	go func() {
		defer conn.Close()
		_, _ = server.Write([]byte(`{"jsonrpc":"2.0","id":77,"method":"callback","params":[]}` + "\n"))
		var normalized struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
		}
		if json.NewDecoder(server).Decode(&normalized) != nil || normalized.ID != 77 || normalized.Result["ok"] != true {
			encoded, _ := json.Marshal(normalized)
			done <- errors.New("reverse request params were not normalized: " + string(encoded))
			return
		}
		_, _ = server.Write([]byte(`{"jsonrpc":"2.0","id":78,"method":"callback","params":{"bad":true}}` + "\n"))
		var structured struct {
			ID    int            `json:"id"`
			Error *wireRPCError  `json:"error"`
			Raw   map[string]any `json:"-"`
		}
		if json.NewDecoder(server).Decode(&structured) != nil || structured.ID != 78 || structured.Error == nil ||
			structured.Error.Code != 8 || string(structured.Error.Data) != `{"seen":true}` {
			encoded, _ := json.Marshal(structured)
			done <- errors.New("structured reverse-request error was not preserved: " + string(encoded))
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reverse request handler did not answer")
	}
	_ = transport.Close()
}

func TestLineTransportReverseRequestHandlerPanicMapsToInternalError(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.OnRequest(func(string, json.RawMessage) (json.RawMessage, error) {
		panic("callback boom")
	})
	transport.Start()
	done := make(chan error, 1)
	go func() {
		defer conn.Close()
		_, _ = server.Write([]byte(`{"jsonrpc":"2.0","id":9,"method":"panic/callback","params":{}}` + "\n"))
		var response struct {
			ID    int           `json:"id"`
			Error *wireRPCError `json:"error"`
		}
		if json.NewDecoder(server).Decode(&response) != nil || response.ID != 9 || response.Error == nil ||
			response.Error.Code != -32603 || !strings.Contains(response.Error.Message, "callback boom") {
			done <- errors.New("panic callback was not mapped to -32603")
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reverse request panic was not answered")
	}
	_ = transport.Close()
}

func TestLineTransportDeterministicMutatedResponseFuzz(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.Start()
	resultCh := make(chan error, 1)
	go func() {
		_, err := transport.Request(context.Background(), "fuzz", json.RawMessage(`{}`), 3*time.Second)
		resultCh <- err
	}()
	done := make(chan error, 1)
	go func() {
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(server).Decode(&request) != nil {
			done <- errors.New("fuzz peer failed to read request")
			return
		}
		_ = server.SetDeadline(time.Now().Add(3 * time.Second))
		random := rand.New(rand.NewSource(29))
		for i := 0; i < 300; i++ {
			hostile := []byte(`{"jsonrpc":"2.0","id":"hostile-` + strings.Repeat("x", random.Intn(8)) + `","result":{"seed":` + strings.Repeat("1", 1+random.Intn(8)) + `}}`)
			for mutation := 0; mutation <= random.Intn(4); mutation++ {
				hostile[random.Intn(len(hostile))] = byte(1 + random.Intn(255))
			}
			_, _ = server.Write(append(hostile, '\n'))
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": nil})
		_, _ = server.Write(append(response, '\n'))
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	_ = transport.Close()
}

func TestLineTransportContextCancellationSendsNoFrame(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	transport := NewLineTransport(conn, conn, nil, nil)
	transport.Start()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.Request(ctx, "cancelled", json.RawMessage(`{}`), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request error = %v", err)
	}

	responseCh := make(chan error, 1)
	go func() {
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(server).Decode(&request) != nil || request.ID != "go-1" {
			responseCh <- errors.New("cancellation left a frame or consumed a request id")
			return
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "ok"})
		_, _ = server.Write(append(response, '\n'))
		responseCh <- nil
	}()
	if _, err := transport.Request(context.Background(), "real", json.RawMessage(`{}`), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-responseCh; err != nil {
		t.Fatal(err)
	}
	_ = transport.Close()
}

func FuzzLineTransportFrame(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","method":"tick","params":{}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":7,"method":"reverse","params":[]}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":"go-1","result":{"x":true}}`))
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, frame []byte) {
		reader, writer := io.Pipe()
		transport := NewLineTransport(reader, io.Discard, func(Notification) {}, nil)
		transport.Start()
		_, _ = writer.Write(frame)
		_ = writer.Close()
		select {
		case <-transport.Done():
		case <-time.After(time.Second):
			t.Fatal("reader goroutine did not settle after EOF")
		}
		_ = transport.Close()
	})
}

func TestLineTransportClosesPendingRequestOnOverlongFrame(t *testing.T) {
	reader, writer := io.Pipe()
	transport := NewLineTransport(reader, io.Discard, nil, nil)
	transport.Start()
	requestDone := make(chan error, 1)
	go func() {
		_, err := transport.Request(context.Background(), "too-long", json.RawMessage(`{}`), 2*time.Second)
		requestDone <- err
	}()

	go func() {
		defer writer.Close()
		_, _ = writer.Write(bytes.Repeat([]byte{0x78}, maxWireLineBytes+2))
	}()
	select {
	case err := <-requestDone:
		var closed *ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("overlong frame error = %v, want ClosedError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending request survived an overlong frame")
	}
	select {
	case <-transport.Done():
	case <-time.After(time.Second):
		t.Fatal("reader goroutine survived an overlong frame")
	}
	_ = transport.Close()
}
