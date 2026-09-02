package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamableHTTPClientSessionLifecycle(t *testing.T) {
	var mu sync.Mutex
	var session string
	var methods []string
	var auth []string
	var protocolVersions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if len(body) != 0 && json.Unmarshal(body, &req) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods = append(methods, r.Method+":"+req.Method)
		auth = append(auth, r.Header.Get("Authorization"))
		protocolVersions = append(protocolVersions, r.Header.Get("MCP-Protocol-Version"))
		currentSession := session
		mu.Unlock()
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != currentSession {
				http.Error(w, "missing session", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if req.Method == "initialize" {
			mu.Lock()
			session = "session-1"
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", "session-1")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "http-test", "version": "1"}})
			return
		}
		if r.Header.Get("Mcp-Session-Id") != currentSession {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "tools/list":
			if req.Params["cursor"] == nil {
				writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{map[string]any{"name": "first", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "page-2"})
			} else {
				writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{map[string]any{"name": "second", "inputSchema": map[string]any{"type": "object"}, "execution": map[string]any{"taskSupport": "optional"}}}})
			}
		case "tools/call":
			writeHTTPRPC(t, w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}, "structuredContent": map[string]any{"ok": true}})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, map[string]string{"Authorization": "Bearer test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 2 || tools[1].TaskSupport != "optional" {
		t.Fatalf("ListTools = %#v, err=%v", tools, err)
	}
	result, err := client.Call(context.Background(), "ping", map[string]any{"x": 1})
	if err != nil || result.StructuredContent.(map[string]any)["ok"] != true {
		t.Fatalf("Call = %#v, err=%v", result, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !containsString(methods, "POST:initialize") || !containsString(methods, "POST:notifications/initialized") || !containsString(methods, "POST:tools/list") || !containsString(methods, "POST:tools/call") || !containsString(methods, "DELETE:") {
		t.Fatalf("HTTP lifecycle methods = %v", methods)
	}
	for _, value := range auth {
		if value != "Bearer test" {
			t.Fatalf("authorization header = %q, want propagated header", value)
		}
	}
	for i, method := range methods {
		if strings.HasPrefix(method, "POST:") && method != "POST:initialize" && method != "POST:notifications/initialized" && protocolVersions[i] != protocolVersion {
			t.Fatalf("protocol version header for %s = %q, want %q", method, protocolVersions[i], protocolVersion)
		}
	}
}

func TestStreamableHTTPClientCloseInterruptsInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	var enterOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if len(body) != 0 && json.Unmarshal(body, &req) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch {
		case req.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "close-session")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "close-test", "version": "1"}})
		case req.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case req.Method == "tools/list":
			enterOnce.Do(func() { close(entered) })
			<-r.Context().Done()
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := client.ListTools(context.Background())
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server did not receive the in-flight tools/list")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = client.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind an in-flight HTTP request")
	}
	select {
	case err := <-requestDone:
		if err == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, ErrConnection)) {
			t.Fatalf("in-flight HTTP ListTools error = %v, want cancellation/connection error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight HTTP ListTools did not settle after Close")
	}
}

func TestStreamableHTTPClientSSEListChangedAndResponse(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sse-session")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sse-session")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"))
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{}}})
		_, _ = w.Write([]byte("data: " + string(response) + "\n\n"))
	}))
	defer server.Close()
	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 1)
	client.SetToolListChangedHandler(func() { changed <- struct{}{} })
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("SSE tools/list_changed notification was not dispatched")
	}
	if calls != 1 {
		t.Fatalf("SSE request count = %d, want 1", calls)
	}
	_ = client.Close()
}

func TestStreamableHTTPClientAnswersServerPing(t *testing.T) {
	pingReply := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", "ping-session")
			_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":901,\"method\":\"ping\"}\n\n"))
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": protocolVersion}})
			_, _ = w.Write([]byte("data: " + string(response) + "\n\n"))
			return
		}
		if req.Method == "" && req.ID != nil {
			pingReply <- req.Result != nil && len(req.Result) == 0 && req.Error == nil
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start with server ping: %v", err)
	}
	select {
	case ok := <-pingReply:
		if !ok {
			t.Fatal("server ping response was not an empty JSON-RPC result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server ping response was not received")
	}
}

func TestParseSSEResponseMultilineNotificationAndMatchingResponse(t *testing.T) {
	notifications := 0
	stream := strings.Join([]string{
		`event: notification`,
		`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
		``,
		`data: {"jsonrpc":"2.0",`,
		`data: "id":44,"result":{"ignored":true}`,
		`data: }`,
		``,
		`data: {"jsonrpc":"2.0","id":7,"result":{"answer":"matched"}}`,
		``,
	}, "\n")
	result, err := parseSSEResponse([]byte(stream), 7, func() { notifications++ })
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(result, &payload); err != nil || payload.Answer != "matched" {
		t.Fatalf("SSE result = %s (%v)", result, err)
	}
	if notifications != 1 {
		t.Fatalf("notifications = %d, want one list-changed signal", notifications)
	}
}

func TestParseSSEResponseRejectsMalformedData(t *testing.T) {
	if _, err := parseSSEResponse([]byte("data: not-json\n\n"), 1, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("malformed SSE error = %v, want ErrProtocol", err)
	}
}

func TestStreamableHTTPClientMalformedResponseKeepsSession(t *testing.T) {
	var initializes, lists int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			initializes++
			w.Header().Set("Mcp-Session-Id", "stable-session")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			lists++
			if lists == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("not-json\n"))
				return
			}
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{}})
		case "":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("first ListTools = %v, want ErrProtocol", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("second ListTools on retained session = %v", err)
	}
	_ = client.Close()
	if initializes != 1 || lists != 2 {
		t.Fatalf("initialize/list counts = %d/%d, want 1/2 without reconnect", initializes, lists)
	}
}

func TestStreamableHTTPClientCallerCancellationKeepsSession(t *testing.T) {
	entered := make(chan struct{})
	var enterOnce sync.Once
	var lists int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "cancel-session")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			lists++
			if lists == 1 {
				enterOnce.Do(func() { close(entered) })
				<-r.Context().Done()
				return
			}
			if r.Header.Get("Mcp-Session-Id") != "cancel-session" {
				http.Error(w, "session was lost", http.StatusBadRequest)
				return
			}
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{}})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	requestDone := make(chan error, 1)
	go func() {
		_, err := client.ListTools(ctx)
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server did not receive the cancellable tools/list")
	}
	cancel()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled ListTools = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled ListTools did not settle")
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools after caller cancellation = %v, want retained session", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if lists != 2 {
		t.Fatalf("tools/list requests = %d, want cancelled request plus explicit retry", lists)
	}
}

// TestStreamableHTTPAuthFailureKeepsSessionAndAvoidsReplay pins the auth
// kill-point contract: a failed tools/call is a terminal caller-level failure,
// not an automatic retry. The MCP session survives, and a later explicit call
// can perform the external effect exactly once.
func TestStreamableHTTPAuthFailureKeepsSessionAndAvoidsReplay(t *testing.T) {
	var mu sync.Mutex
	var sessionID string
	var callAttempts, effects []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					Action string `json:"action"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			mu.Lock()
			sessionID = "auth-session"
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != sessionID {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "tools/list":
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{map[string]any{
				"name": "publish", "inputSchema": map[string]any{"type": "object"},
			}}})
		case "tools/call":
			mu.Lock()
			callAttempts = append(callAttempts, req.Params.Arguments.Action)
			attempt := len(callAttempts)
			mu.Unlock()
			if attempt == 1 {
				http.Error(w, "authorization expired", http.StatusUnauthorized)
				return
			}
			mu.Lock()
			effects = append(effects, req.Params.Arguments.Action)
			mu.Unlock()
			writeHTTPRPC(t, w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "published"}},
			})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"action": "publish-report"}
	if _, err := client.Call(context.Background(), "publish", args); err == nil {
		t.Fatal("unauthorized tools/call unexpectedly succeeded")
	}
	result, err := client.Call(context.Background(), "publish", args)
	if err != nil || result.IsError {
		t.Fatalf("explicit retry call = %#v, %v", result, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(callAttempts) != 2 || callAttempts[0] != "publish-report" || callAttempts[1] != "publish-report" {
		t.Fatalf("transport call attempts = %v, want two explicit attempts and no replay", callAttempts)
	}
	if len(effects) != 1 || effects[0] != "publish-report" {
		t.Fatalf("external effects = %v, want exactly one after explicit retry", effects)
	}
	if sessionID != "auth-session" {
		t.Fatalf("session = %q, want auth-session", sessionID)
	}
}

func TestNewClientForServerSelectsHTTPTransport(t *testing.T) {
	client, err := NewClientForServer(context.Background(), NewStdioFactory(), McpServer{Name: "remote", Transport: "streamable-http", URL: "http://127.0.0.1:1/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*streamableHTTPClient); !ok {
		t.Fatalf("HTTP client type = %T, want *streamableHTTPClient", client)
	}
	_ = client.Close()
	if _, err := NewClientForServer(context.Background(), NewStdioFactory(), McpServer{Name: "unknown", Transport: "unknown"}); err == nil {
		t.Fatal("unsupported transport was accepted")
	}
}

func TestValidateServerMatchesTransportAndNamespaceContract(t *testing.T) {
	valid := McpServer{Name: "server_1", Cmd: "mcp-server"}
	if err := ValidateServer(valid); err != nil {
		t.Fatalf("valid legacy stdio server: %v", err)
	}
	if err := ValidateServer(McpServer{Name: "bad/name", Cmd: "server"}); err == nil {
		t.Fatal("path separator in server name was accepted")
	}
	if err := ValidateServer(McpServer{Name: "missing-command"}); err == nil {
		t.Fatal("stdio server without command was accepted")
	}
	if err := ValidateServer(McpServer{Name: "remote", Transport: "streamable-http", URL: "https://example.test/mcp"}); err != nil {
		t.Fatalf("valid HTTP server: %v", err)
	}
	if err := ValidateServer(McpServer{Name: "remote", Transport: "streamable-http"}); err == nil {
		t.Fatal("HTTP server without URL was accepted")
	}
	if err := ValidateServer(McpServer{Name: "remote", Transport: "ftp", URL: "ftp://example.test"}); err == nil {
		t.Fatal("unsupported transport was accepted")
	}
}

func TestStreamableHTTPClientReconnectStartsFreshSession(t *testing.T) {
	var initializes, lists int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			initializes++
			w.Header().Set("Mcp-Session-Id", "session-"+string(rune('0'+initializes)))
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			lists++
			if lists == 1 {
				http.Error(w, "temporary outage", http.StatusBadGateway)
				return
			}
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{}})
		case "":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrConnection) {
		t.Fatalf("first ListTools = %v, want ErrConnection", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start after HTTP loss: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("second ListTools: %v", err)
	}
	_ = client.Close()
	if initializes != 2 || lists != 2 {
		t.Fatalf("reconnect initialize/list counts = %d/%d, want 2/2", initializes, lists)
	}
}

// TestReconnectingClientRetiresHTTPGenerationSession proves the transport
// boundary behind a fresh HTTP generation: the old MCP session is explicitly
// deleted during replacement, and later discovery uses only the replacement's
// session. This is the HTTP analogue of the real stdio process-retirement test.
func TestReconnectingClientRetiresHTTPGenerationSession(t *testing.T) {
	var mu sync.Mutex
	var initializes int
	var events []string
	sessions := map[string]string{"alpha": "live", "beta": "live"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		session := r.Header.Get("Mcp-Session-Id")
		if session == "" {
			session = "(new)"
		}
		operation := r.Method
		if req.Method != "" {
			operation += ":" + req.Method
		}

		mu.Lock()
		events = append(events, session+" "+operation)
		mu.Unlock()

		switch {
		case req.Method == "initialize":
			mu.Lock()
			initializes++
			id := ""
			switch initializes {
			case 1:
				id = "alpha"
			case 2:
				id = "beta"
			}
			mu.Unlock()
			if id == "" {
				http.Error(w, "unexpected initialize", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Mcp-Session-Id", id)
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
		case req.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete:
			mu.Lock()
			if _, ok := sessions[session]; ok {
				sessions[session] = "deleted"
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case req.Method == "tools/list":
			mu.Lock()
			live := sessions[session] == "live"
			mu.Unlock()
			if !live {
				http.Error(w, "retired session", http.StatusGone)
				return
			}
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{map[string]any{
				"name": "echo", "inputSchema": map[string]any{"type": "object"},
			}}})
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	first, err := newStreamableHTTPClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := make(chan Client, 1)
	reconnected := make(chan struct{})
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		candidate, err := newStreamableHTTPClient(server.URL, nil)
		if err != nil {
			return nil, err
		}
		replacement <- candidate
		return candidate, nil
	}, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1,
	}).(*ReconnectingClient)
	defer client.Close()
	client.SetReconnectedHandler(func() { close(reconnected) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("alpha generation Start: %v", err)
	}
	// Model an explicit replacement while the old HTTP generation is still
	// connected. This isolates generation retirement from the prior test's
	// request-loss path, where the old session was already invalid.
	client.requestReconnect()
	select {
	case <-reconnected:
	case <-ctx.Done():
		t.Fatal("HTTP generation replacement did not complete")
	}
	base := client.currentBase()
	select {
	case got := <-replacement:
		if got == nil || base == nil || base != got {
			t.Fatalf("replacement generation base=%T got=%T", base, got)
		}
	default:
		t.Fatal("replacement HTTP generation was not installed")
	}
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("replacement ListTools = %#v, %v, want one echo tool", tools, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("replacement Close: %v", err)
	}

	mu.Lock()
	gotEvents := append([]string(nil), events...)
	alpha, beta := sessions["alpha"], sessions["beta"]
	mu.Unlock()
	wantEvents := []string{
		"(new) POST:initialize",
		"alpha POST:notifications/initialized",
		"(new) POST:initialize",
		"beta POST:notifications/initialized",
		"alpha DELETE",
		"beta POST:tools/list",
		"beta DELETE",
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("HTTP generation events:\n%v\nwant:\n%v", gotEvents, wantEvents)
	}
	if alpha != "deleted" || beta != "deleted" {
		t.Fatalf("HTTP session states alpha=%q beta=%q, want both deleted", alpha, beta)
	}
}

func writeHTTPRPC(t *testing.T, w http.ResponseWriter, id *int64, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Errorf("write HTTP JSON-RPC: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}
