package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Helper-process env markers: the fake MCP server runs inside the test binary
// itself, re-executed as a subprocess (the Go "helper process" idiom — the same
// binary, a -test.run filter and an env marker). This sidesteps any cmd /C
// quoting boundary (mirrors the M6e-1 note) and keeps the fake server on the
// exact newline-delimited JSON-RPC framing the client speaks.
const (
	helperServerEnv     = "PA_MCP_HELPER_SERVER"
	helperServerModeEnv = "PA_MCP_HELPER_SERVER_MODE"
)

// TestHelperServer is the fake MCP server. Spawned by the tests as
// os.Args[0] -test.run=^TestHelperServer$ with helperServerEnv set, it reads
// newline-delimited JSON-RPC frames on stdin and answers on stdout. In the
// parent run (no env marker) it is a normal skipped test. The "timeout" mode
// answers initialize but never answers tools/list or tools/call, so the client
// must time out and terminate the server via Close.
func TestHelperServer(t *testing.T) {
	if os.Getenv(helperServerEnv) != "1" {
		t.Skip("helper-process mode: spawned by the mcp tests")
	}
	mode := os.Getenv(helperServerModeEnv)
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return // stdin closed → exit cleanly
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return
		}
		if len(msg.ID) == 0 {
			continue // a notification → no id, no response
		}
		switch msg.Method {
		case "initialize":
			if mode == "handshake-fail" {
				fakeError(out, msg.ID, -32000, "handshake rejected by fake server")
				continue
			}
			if mode == "notify" {
				fakeFrame(out, map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": map[string]any{}})
			}
			if mode == "ping" {
				fakeFrame(out, map[string]any{"jsonrpc": "2.0", "id": 900, "method": "ping", "params": map[string]any{}})
				pingLine, pingErr := in.ReadBytes('\n')
				var pingResponse struct {
					ID     json.RawMessage `json:"id"`
					Result map[string]any  `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if pingErr != nil || json.Unmarshal(pingLine, &pingResponse) != nil || string(pingResponse.ID) != "900" || pingResponse.Result == nil || len(pingResponse.Error) != 0 {
					return
				}
			}
			fakeResult(out, msg.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0.0"},
			})
		case "tools/list":
			if mode == "timeout" || mode == "timeout-close" {
				if mode == "timeout-close" {
					fakeFrame(out, map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": map[string]any{}})
				}
				// Stay alive without responding: a pending timer keeps the Go
				// runtime's deadlock detector (which kills a test binary on
				// `select {}` with all goroutines asleep) from firing. The
				// client times out, then kills us via Close.
				time.Sleep(time.Hour)
			}
			if mode == "env-check" {
				fakeResult(out, msg.ID, map[string]any{"tools": []any{map[string]any{
					"name": "env", "description": os.Getenv("DSH_API_KEY") + "|" + os.Getenv("SAFE_MODE"),
					"inputSchema": map[string]any{"type": "object"},
				}}})
				continue
			}
			if mode == "cwd-check" {
				fakeResult(out, msg.ID, map[string]any{"tools": []any{map[string]any{
					"name": "cwd", "description": mustGetwd() + "|" + os.Getenv("DSH_API_KEY") + "|" + os.Getenv("SAFE_MODE"), "inputSchema": map[string]any{"type": "object"},
				}}})
				continue
			}
			if mode == "pages" {
				var params struct {
					Cursor string `json:"cursor"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if params.Cursor == "page-2" {
					fakeResult(out, msg.ID, map[string]any{"tools": []any{map[string]any{"name": "second", "inputSchema": map[string]any{"type": "object"}}}})
				} else {
					fakeResult(out, msg.ID, map[string]any{"tools": []any{map[string]any{"name": "first", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "page-2"})
				}
				continue
			}
			fakeResult(out, msg.ID, map[string]any{
				"tools": []any{
					map[string]any{
						"name":        "echo",
						"description": "Echo the given text back",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"text": map[string]any{"type": "string"}},
							"required":   []any{"text"},
						},
						"outputSchema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}},
						"execution":    map[string]any{"taskSupport": "optional"},
					},
					map[string]any{
						"name":        "noschema",
						"description": "A tool with no inputSchema",
					},
				},
			})
		case "tools/call":
			if mode == "timeout" {
				time.Sleep(time.Hour) // see tools/list above
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				fakeError(out, msg.ID, -32602, "invalid params")
				continue
			}
			switch params.Name {
			case "echo":
				text, _ := params.Arguments["text"].(string)
				fakeResult(out, msg.ID, map[string]any{
					"content":           []any{map[string]any{"type": "text", "text": "echo:" + text}},
					"structuredContent": map[string]any{"answer": text},
					"isError":           false,
				})
			case "erris":
				fakeResult(out, msg.ID, map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "operation failed"}},
					"isError": true,
				})
			case "errimage":
				fakeResult(out, msg.ID, map[string]any{
					"content": []any{map[string]any{
						"type": "image", "mimeType": "image/png",
						"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
					}},
					"isError": true,
				})
			case "boom":
				fakeError(out, msg.ID, -32602, "boom requested")
			case "nope":
				fakeError(out, msg.ID, -32601, "Method not found")
			default:
				fakeError(out, msg.ID, -32602, "unknown tool "+params.Name)
			}
		default:
			fakeError(out, msg.ID, -32601, "method not found")
		}
	}
}

func TestClientCloseInterruptsInFlightRequest(t *testing.T) {
	c := newFakeClient(t, "timeout-close", 30*time.Second)
	changed := make(chan struct{}, 1)
	c.SetToolListChangedHandler(func() { changed <- struct{}{} })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	requestDone := make(chan error, 1)
	go func() {
		_, err := c.ListTools(context.Background())
		requestDone <- err
	}()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout helper did not receive the in-flight tools/list")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind an in-flight stdio request")
	}
	select {
	case err := <-requestDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("in-flight ListTools error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight ListTools did not settle after Close")
	}
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

func fakeResult(w *bufio.Writer, id json.RawMessage, result any) {
	fakeFrame(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func fakeError(w *bufio.Writer, id json.RawMessage, code int, message string) {
	fakeFrame(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func fakeFrame(w *bufio.Writer, frame map[string]any) {
	b, _ := json.Marshal(frame)
	_, _ = w.Write(b)
	_ = w.WriteByte('\n')
	_ = w.Flush()
}

// newFakeClient builds a stdioClient pointed at this test binary running as the
// fake MCP server in the given mode. A non-positive timeout uses a 5s
// per-request bound so a broken test fails fast instead of hanging on the 30s
// default.
func newFakeClient(t *testing.T, mode string, timeout time.Duration) *stdioClient {
	t.Helper()
	c := newStdioClient(os.Args[0], []string{"-test.run=^TestHelperServer$"})
	c.env = []string{
		helperServerEnv + "=1",
		helperServerModeEnv + "=" + mode,
	}
	c.timeout = timeout
	if timeout <= 0 {
		c.timeout = 5 * time.Second
	}
	return c
}

// TestClientStartHandshake covers the happy-path Start (MCP initialize
// handshake) and its idempotence.
func TestClientStartHandshake(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("second Start (idempotent): %v", err)
	}
}

func TestClientAnswersServerPing(t *testing.T) {
	c := newFakeClient(t, "ping", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start with server ping: %v", err)
	}
}

func TestScrubEnvRemovesCredentialShapedNamesOnly(t *testing.T) {
	got := scrubEnv([]string{
		"PATH=/bin",
		"DSH_API_KEY=secret",
		"SERVICE_TOKEN=secret",
		"SAFE_MODE=1",
		"BROKEN_NO_EQUALS",
	})
	if strings.Join(got, "\x00") != "PATH=/bin\x00SAFE_MODE=1" {
		t.Fatalf("scrubbed MCP environment = %#v", got)
	}
}

func TestStdioChildDoesNotInheritAmbientCredentials(t *testing.T) {
	t.Setenv("DSH_API_KEY", "ambient-secret")
	t.Setenv("SAFE_MODE", "1")
	c := newFakeClient(t, "env-check", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Description != "|1" {
		t.Fatalf("child environment description = %#v, want ambient credential removed and safe value retained", tools)
	}
}

func TestConfiguredServerPreservesEnvCwdAndCallTimeout(t *testing.T) {
	t.Setenv("DSH_API_KEY", "ambient-secret")
	dir := t.TempDir()
	client, err := NewClientForServer(context.Background(), NewStdioFactory(), McpServer{
		Name: "configured", Cmd: os.Args[0],
		Args: []string{"-test.run=^TestHelperServer$"},
		Env:  map[string]string{helperServerEnv: "1", helperServerModeEnv: "cwd-check", "SAFE_MODE": "1", "DSH_API_KEY": "explicit-secret"},
		Cwd:  dir, ToolCallTimeout: 1234 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClientForServer: %v", err)
	}
	defer client.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Description != dir+"|explicit-secret|1" {
		t.Fatalf("configured child boundary = %#v, want cwd/env", tools)
	}
	configured, ok := client.(*stdioClient)
	if !ok || configured.callTimeout != 1234*time.Millisecond {
		t.Fatalf("configured call timeout = %#v, want 1234ms", configured)
	}
}

func TestClientDispatchesToolListChangedNotification(t *testing.T) {
	c := newFakeClient(t, "notify", 0)
	defer c.Close()
	changed := make(chan struct{}, 1)
	c.SetToolListChangedHandler(func() { changed <- struct{}{} })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("tool-list-changed notification was not dispatched")
	}
}

// TestClientStartFailure covers a spawn failure mapping to ErrStartFailed.
func TestClientStartFailure(t *testing.T) {
	c := NewStdioClient("sta-mcp-no-such-command-xyz-12345", nil)
	defer c.Close()

	err := c.Start(context.Background())
	if err == nil || !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start error = %v, want ErrStartFailed", err)
	}
}

// TestClientHandshakeFailure covers a server that rejects initialize (error
// frame) mapping to ErrHandshake.
func TestClientHandshakeFailure(t *testing.T) {
	c := newFakeClient(t, "handshake-fail", 0)
	defer c.Close()

	err := c.Start(context.Background())
	if err == nil || !errors.Is(err, ErrHandshake) {
		t.Fatalf("Start error = %v, want ErrHandshake", err)
	}
}

// TestClientListTools covers tools/list: tools, descriptions and the
// inputSchema passthrough, plus a tool with no inputSchema yielding an empty
// non-nil map.
func TestClientListTools(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	echo, ok := byName["echo"]
	if !ok {
		t.Fatalf("tools = %v, want an 'echo' tool", tools)
	}
	if echo.Description == "" {
		t.Fatal("echo.Description is empty")
	}
	if ty, _ := echo.InputSchema["type"].(string); ty != "object" {
		t.Fatalf("echo.InputSchema['type'] = %v, want 'object'", echo.InputSchema["type"])
	}
	if echo.OutputSchema == nil || echo.OutputSchema["type"] != "object" || echo.TaskSupport != "optional" {
		t.Fatalf("echo output/task metadata = %#v/%q, want output schema and optional task support", echo.OutputSchema, echo.TaskSupport)
	}
	ns, ok := byName["noschema"]
	if !ok {
		t.Fatalf("tools = %v, want a 'noschema' tool", tools)
	}
	if ns.InputSchema == nil {
		t.Fatal("noschema.InputSchema is nil, want empty non-nil map")
	}
	if len(ns.InputSchema) != 0 {
		t.Fatalf("noschema.InputSchema = %v, want empty", ns.InputSchema)
	}
}

func TestClientListToolsDrainsPagination(t *testing.T) {
	c := newFakeClient(t, "pages", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "first" || tools[1].Name != "second" {
		t.Fatalf("paginated tools = %#v, want first and second", tools)
	}
}

// TestClientCallSuccess covers a happy tools/call: content passes through and
// IsError is false.
func TestClientCallSuccess(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := c.Call(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatal("IsError = true, want false")
	}
	if len(res.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(res.Content))
	}
	item, ok := res.Content[0].(map[string]any)
	if !ok {
		t.Fatalf("Content[0] = %#v, want a map", res.Content[0])
	}
	if got, _ := item["text"].(string); got != "echo:hi" {
		t.Fatalf("Content[0]['text'] = %q, want %q", got, "echo:hi")
	}
	if !res.StructuredContentSet || res.StructuredContent.(map[string]any)["answer"] != "hi" {
		t.Fatalf("StructuredContent = %#v (set=%v), want answer=hi", res.StructuredContent, res.StructuredContentSet)
	}
}

// TestClientCallIsError covers a tool that reports execution failure inside a
// successful result: IsError is passed through with nil error.
func TestClientCallIsError(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := c.Call(context.Background(), "erris", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false, want true")
	}
	if len(res.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(res.Content))
	}
}

// TestClientCallErrorFrame covers a server error frame mapping to ErrServer.
func TestClientCallErrorFrame(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := c.Call(context.Background(), "boom", nil)
	if err == nil || !errors.Is(err, ErrServer) {
		t.Fatalf("Call error = %v, want ErrServer", err)
	}
}

// TestClientCallUnknownMethod covers a -32601 method-not-found frame mapping to
// ErrUnknownMethod (tools/call for a tool the server does not know).
func TestClientCallUnknownMethod(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := c.Call(context.Background(), "nope", nil)
	if err == nil || !errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("Call error = %v, want ErrUnknownMethod", err)
	}
}

// TestClientTimeout covers the per-request timeout: a server that never answers
// tools/list yields ErrTimeout promptly, and Close still terminates the hung
// server promptly (no goroutine holds the connection open). Start intentionally
// uses the normal handshake bound; process launch latency must not turn this
// timeout test into a flaky initialize failure.
func TestClientTimeout(t *testing.T) {
	c := newFakeClient(t, "timeout", 0)
	defer c.Close()

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := c.ListTools(context.Background())
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrTimeout) {
		t.Fatalf("ListTools error = %v, want ErrTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout did not return promptly: %v", elapsed)
	}

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on the hung server (goroutine/process leak)")
	}
}

// TestClientReconnectAfterTimeout covers the recoverable connection state:
// request timeout tears down only the current subprocess, while Start performs
// a fresh initialize handshake. Explicit Close remains terminal afterwards.
func TestClientReconnectAfterTimeout(t *testing.T) {
	c := newFakeClient(t, "timeout", 0)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.timeout = 300 * time.Millisecond
	if _, err := c.ListTools(context.Background()); err == nil || !errors.Is(err, ErrTimeout) {
		t.Fatalf("ListTools error = %v, want ErrTimeout", err)
	}
	c.timeout = DefaultTimeout
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start after timeout: %v", err)
	}
	if !c.started {
		t.Fatal("client is not started after reconnect")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after explicit Close = %v, want ErrClosed", err)
	}
}

// TestClientCloseIdempotent covers Close twice returning nil, the child process
// being killed and reaped, and Start/ListTools/Call being rejected with
// ErrClosed afterwards.
func TestClientCloseIdempotent(t *testing.T) {
	c := newFakeClient(t, "echo", 0)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The child must have been killed and reaped (no zombie, no leak). On
	// Linux ProcessState can become visible just after Wait wakes the Close
	// caller; bound this observation instead of assuming a cross-runtime
	// instruction ordering.
	reaped := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		// SIGKILL termination is represented by Sys().WaitStatus.Signaled,
		// not ProcessState.Exited; ProcessState non-nil is the reap boundary.
		if c.proc != nil && c.proc.ProcessState != nil {
			reaped = true
			break
		}
	}
	if !reaped {
		t.Fatal("child process not reaped after Close")
	}
	// Post-close operations are rejected.
	if _, err := c.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}
	if err := c.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
	if _, err := c.Call(context.Background(), "echo", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close = %v, want ErrClosed", err)
	}
}

// TestClientNoGoroutineLeak asserts a full client lifecycle (Start, ListTools,
// Call, Close) leaves the goroutine count at its baseline — the client spawns
// no background goroutines and Close drains the exec-internal stderr copier.
// The baseline is taken after a warm-up cycle plus forced GC so lazily started
// runtime goroutines (poller/finalizer/GC workers) are already counted.
func TestClientNoGoroutineLeak(t *testing.T) {
	warm := newFakeClient(t, "echo", 0)
	if err := warm.Start(context.Background()); err != nil {
		t.Fatalf("warmup Start: %v", err)
	}
	if _, err := warm.ListTools(context.Background()); err != nil {
		t.Fatalf("warmup ListTools: %v", err)
	}
	_ = warm.Close()
	runtime.GC()
	runtime.GC()

	before := runtime.NumGoroutine()
	c := newFakeClient(t, "echo", 0)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if _, err := c.Call(context.Background(), "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestStdioCloseWaitsForNotificationCallback(t *testing.T) {
	c := newStdioClient("unused", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	c.SetToolListChangedHandler(func() {
		close(entered)
		<-release
	})

	c.mu.Lock()
	c.startNotificationDispatcherLocked()
	c.mu.Unlock()
	c.signalToolListChanged()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("notification callback did not start")
	}

	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while notification callback was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not drain notification callback")
	}
}
