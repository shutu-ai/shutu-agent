package sdkclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

const runtimeModeEnv = "SHUTU_SDKCLIENT_TEST_RUNTIME"
const referenceNotificationsEnv = "SHUTU_SDKCLIENT_REFERENCE_NOTIFICATIONS"
const launchMarkerEnv = "SHUTU_SDKCLIENT_LAUNCH_MARKER"
const parentLeakEnv = "SHUTU_SDKCLIENT_PARENT_LEAK"

func TestMain(m *testing.M) {
	if mode := os.Getenv(runtimeModeEnv); mode != "" {
		if err := fakeRuntime(mode); err != nil {
			fmt.Fprintln(os.Stderr, "fake SDK runtime:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func launchOptions(mode string, mutate func(*ClientOptions)) ClientOptions {
	options := ClientOptions{
		Command:         os.Args[0],
		Dir:             ".",
		RequestTimeout:  2 * time.Second,
		ShutdownTimeout: 250 * time.Millisecond,
		EOFGrace:        250 * time.Millisecond,
		TerminateGrace:  time.Second,
	}
	options.Env = append(os.Environ(), runtimeModeEnv+"="+mode)
	if mutate != nil {
		mutate(&options)
	}
	return options
}

func TestHarnessRunCollectsReceiptThroughIdleAndSessionTree(t *testing.T) {
	harness := NewHarness(HarnessOptions{
		Launch:    launchOptions("normal", nil),
		CWD:       ".",
		MaxTokens: 123,
	})
	result, err := harness.Run(context.Background(), "hello", "root-session")
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "root-session" || result.FinalResponse != "hello from fake runtime" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Events) != 2 || result.Events[0].Type != "agent/inbox/spliced" || result.Events[1].Type != "assistant/message" {
		t.Fatalf("root events = %#v", result.Events)
	}
	methods := make([]string, 0, len(result.Notifications))
	for _, notification := range result.Notifications {
		methods = append(methods, notification.Method)
	}
	want := "session.event session.event subagent.started session.event session.status"
	if strings.Join(methods, " ") != want {
		t.Fatalf("notification methods = %q, want %q", strings.Join(methods, " "), want)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionEventPreservesReferenceSurfaceMetadata(t *testing.T) {
	var event SessionEvent
	if err := json.Unmarshal([]byte(`{"seq":7,"type":"user/message","time":12,"data":{"content":[]},"sourceEventSeqs":[3,5],"surfaceOp":{"op":"replace","start":3,"end":5},"ignorable":true}`), &event); err != nil {
		t.Fatal(err)
	}
	if !event.Ignorable || len(event.SourceEventSeqs) != 2 || event.SourceEventSeqs[1] != 5 {
		t.Fatalf("event metadata = %+v", event)
	}
	if string(event.SurfaceOp) != `{"op":"replace","start":3,"end":5}` {
		t.Fatalf("surfaceOp = %s", event.SurfaceOp)
	}
}

func TestHarnessRetriesAfterRuntimeSpawnFailure(t *testing.T) {
	harness := NewHarness(HarnessOptions{Launch: ClientOptions{
		Command:  "definitely-not-a-real-shutu-sdk-runtime",
		Dir:      t.TempDir(),
		EOFGrace: 50 * time.Millisecond,
	}})
	if err := harness.Start(context.Background()); err == nil {
		t.Fatal("missing runtime unexpectedly started")
	}

	// The first failed spawn must not poison the high-level wrapper. Replace
	// the launch spec with a valid local test runtime and retry the handshake.
	harness.options.Launch = launchOptions("normal", nil)
	if err := harness.Start(context.Background()); err != nil {
		t.Fatalf("retry after spawn failure: %v", err)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientRetriesAfterRuntimeSpawnFailure(t *testing.T) {
	client := NewClient(ClientOptions{
		Command:  "definitely-not-a-real-shutu-sdk-runtime",
		Dir:      t.TempDir(),
		EOFGrace: 50 * time.Millisecond,
	})
	if err := client.Start(); err == nil {
		t.Fatal("missing runtime unexpectedly started")
	}
	client.options = launchOptions("normal", nil)
	if err := client.Start(); err != nil {
		t.Fatalf("low-level retry after spawn failure: %v", err)
	}
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCloseBeforeStartFailsSubscriptions(t *testing.T) {
	client := NewClient(ClientOptions{})
	subscription := client.Subscribe(nil)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := subscription.Next(context.Background())
	var closed *ClientClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("subscription after unstarted close = %v, want ClientClosedError", err)
	}

	failed := NewClient(ClientOptions{Command: "definitely-not-a-real-shutu-sdk-runtime"})
	failedSubscription := failed.Subscribe(nil)
	if err := failed.Start(); err == nil {
		t.Fatal("missing runtime unexpectedly started")
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = failedSubscription.Next(context.Background())
	if !errors.As(err, &closed) {
		t.Fatalf("subscription after failed-spawn close = %v, want ClientClosedError", err)
	}
}

func TestClientRequestWithoutTimeoutWaitsForResponse(t *testing.T) {
	client := NewClient(launchOptions("normal", func(options *ClientOptions) {
		options.RequestTimeout = 0
	}))
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err != nil {
		t.Fatalf("request without timeout: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCloseFallsBackToEOFWhenShutdownIsIgnored(t *testing.T) {
	client := NewClient(launchOptions("eof", nil))
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCloseForceReapsRuntimeThatIgnoresEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("process escalation test uses real grace windows")
	}
	client := NewClient(launchOptions("block", nil))
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second close = %v, want idempotent nil", err)
	}
	if _, err := client.Request(context.Background(), "initialize", json.RawMessage(`{"cwd":"."}`)); err == nil {
		t.Fatal("closed client accepted reuse")
	}
}

func TestClientUnexpectedExitFailsSubscriptionWithDispositionAndStderr(t *testing.T) {
	client := NewClient(launchOptions("die", nil))
	subscription := client.Subscribe(nil)
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err == nil {
		t.Fatal("dying runtime unexpectedly answered initialize")
	} else {
		var closed *ClientClosedError
		if !errors.As(err, &closed) || closed.ExitCode == nil || *closed.ExitCode != 9 || len(closed.StderrTail) == 0 {
			t.Fatalf("initialize error = %v, want settled exit code/stderr context", err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for {
		_, err := subscription.Next(context.Background())
		var closed *ClientClosedError
		if errors.As(err, &closed) {
			if closed.ExitCode == nil || *closed.ExitCode != 9 {
				t.Fatalf("closed exit code = %#v, want 9", closed.ExitCode)
			}
			if len(closed.StderrTail) == 0 || closed.StderrTail[len(closed.StderrTail)-1] != "fake runtime died intentionally" {
				t.Fatalf("closed stderr tail = %#v", closed.StderrTail)
			}
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("subscription error = %v, want ClientClosedError", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientLaunchBindsCwdEnvAndRequestTimeout(t *testing.T) {
	t.Setenv(parentLeakEnv, "must-not-leak")
	cwd := t.TempDir()
	options := launchOptions("inspect", nil)
	options.Dir = cwd
	options.Env = filterEnv(options.Env, parentLeakEnv)
	options.Env = append(options.Env, launchMarkerEnv+"=bound")
	client := NewClient(options)

	params, err := json.Marshal(InitializeParams{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.Request(context.Background(), "initialize", params)
	if err != nil {
		t.Fatal(err)
	}
	var launch struct {
		LaunchCwd    string `json:"launchCwd"`
		EnvMarker    string `json:"envMarker"`
		ParentLeaked string `json:"parentLeaked"`
	}
	if json.Unmarshal(raw, &launch) != nil || launch.LaunchCwd != cwd || launch.EnvMarker != "bound" || launch.ParentLeaked != "" {
		t.Fatalf("launch inspection = %s", raw)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	hung := NewClient(launchOptions("hang", func(options *ClientOptions) {
		options.RequestTimeout = 40 * time.Millisecond
		options.ShutdownTimeout = 30 * time.Millisecond
		options.EOFGrace = 30 * time.Millisecond
		options.TerminateGrace = time.Second
	}))
	_, err = hung.Initialize(context.Background(), InitializeParams{CWD: cwd})
	var timeout *RequestTimeoutError
	if !errors.As(err, &timeout) || timeout.Method != "initialize" {
		t.Fatalf("hung initialize error = %v, want RequestTimeoutError", err)
	}
	if err := hung.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientServesReverseRequestCallbackFromRuntimeProcess(t *testing.T) {
	calls := make(chan struct {
		method string
		params json.RawMessage
	}, 1)
	client := NewClient(launchOptions("callback", nil))
	if err := client.OnRequest(func(method string, params json.RawMessage) (json.RawMessage, error) {
		calls <- struct {
			method string
			params json.RawMessage
		}{method: method, params: params}
		return json.RawMessage(`{"accepted":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Initialize(context.Background(), InitializeParams{CWD: "."}); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-calls:
		if call.method != "runtime/callback" || string(call.params) != "{}" {
			t.Fatalf("callback = (%q, %s), want normalized runtime/callback", call.method, call.params)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime callback was not delivered")
	}
	if err := client.OnRequest(nil); err == nil {
		t.Fatal("callback handler replacement after start was accepted")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientSurvivesPrematureStdoutLossAndReapsLiveChild(t *testing.T) {
	client := NewClient(launchOptions("stream-close", func(options *ClientOptions) {
		options.ShutdownTimeout = 30 * time.Millisecond
		options.EOFGrace = 30 * time.Millisecond
		options.TerminateGrace = time.Second
	}))
	_, err := client.Initialize(context.Background(), InitializeParams{CWD: "."})
	var closed *ClientClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("initialize after stdout loss = %v, want ClientClosedError", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientChildBadFramesDoNotPoisonLaterResponse(t *testing.T) {
	client := NewClient(launchOptions("bad-frame", nil))
	result, err := client.Initialize(context.Background(), InitializeParams{CWD: "."})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerInfo.Name != "deepseek-harness-sdk-runtime" {
		t.Fatalf("initialize result = %#v", result)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientChildOversizeFrameFailsClosedAndIsReaped(t *testing.T) {
	client := NewClient(launchOptions("oversize", func(options *ClientOptions) {
		options.ShutdownTimeout = 30 * time.Millisecond
		options.EOFGrace = 30 * time.Millisecond
		options.TerminateGrace = time.Second
	}))
	_, err := client.Initialize(context.Background(), InitializeParams{CWD: "."})
	var closed *ClientClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("oversize child initialize = %v, want ClientClosedError", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func filterEnv(env []string, removePrefix string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, removePrefix+"=") {
			out = append(out, entry)
		}
	}
	return out
}

func fakeRuntime(mode string) error {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), maxWireLineBytes)
	for in.Scan() {
		var request struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			if mode == "die" {
				_, _ = fmt.Fprintln(os.Stderr, "fake runtime died intentionally")
				os.Exit(9)
			}
			if mode == "hang" {
				time.Sleep(5 * time.Second)
			}
			if mode == "bad-frame" {
				_, _ = os.Stdout.WriteString("not-json\n{\"jsonrpc\":\"2.0\",\"params\":{}}\n")
			}
			if mode == "oversize" {
				_, _ = os.Stdout.WriteString(strings.Repeat("x", maxWireLineBytes+2) + "\n")
				time.Sleep(5 * time.Second)
			}
			if mode == "stream-close" {
				_ = os.Stdout.Close()
				time.Sleep(5 * time.Second)
			}
			if mode == "callback" {
				if err := out.Encode(map[string]any{
					"jsonrpc": "2.0", "id": "server-1", "method": "runtime/callback", "params": []string{},
				}); err != nil {
					return err
				}
			}
			if mode == "inspect" {
				launchCwd, err := os.Getwd()
				if err != nil {
					return err
				}
				if err := out.Encode(map[string]any{
					"jsonrpc": "2.0", "id": request.ID,
					"result": map[string]any{
						"serverInfo":   map[string]string{"name": "deepseek-harness-sdk-runtime", "version": "test"},
						"launchCwd":    launchCwd,
						"envMarker":    os.Getenv(launchMarkerEnv),
						"parentLeaked": os.Getenv(parentLeakEnv),
					},
				}); err != nil {
					return err
				}
				continue
			}
			if err := out.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"serverInfo": map[string]string{"name": "deepseek-harness-sdk-runtime", "version": "test"}},
			}); err != nil {
				return err
			}
			if mode == "eof" || mode == "block" {
				if mode == "block" {
					time.Sleep(5 * time.Second)
				}
				return nil
			}
		case "session/prompt":
			var params SessionPromptParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return err
			}
			if mode == "reference" {
				path := os.Getenv(referenceNotificationsEnv)
				if path == "" {
					return errors.New("reference notifications path is empty")
				}
				if err := replayReferenceNotifications(out, path, params.SessionID); err != nil {
					return err
				}
				if err := out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"messageId": params.SessionID}}); err != nil {
					return err
				}
				continue
			}
			if err := writeFakeRun(out, params.SessionID); err != nil {
				return err
			}
			if err := out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"messageId": "message-1"}}); err != nil {
				return err
			}
		case "shutdown":
			if err := out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{}}); err != nil {
				return err
			}
			return nil
		}
	}
	if err := in.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func replayReferenceNotifications(out *json.Encoder, path, sessionID string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 4096), maxWireLineBytes)
	inChild := false
	for lines.Scan() {
		frame := materializeReferenceFrame(lines.Text(), sessionID, sessionID+"-child", &inChild)
		if _, err := fmt.Fprintln(os.Stdout, string(frame)); err != nil {
			return err
		}
	}
	if err := lines.Err(); err != nil {
		return err
	}
	return nil
}

func materializeReferenceFrame(line, rootSession, childSession string, inChild *bool) json.RawMessage {
	line = strings.ReplaceAll(line, "{{sessionId}}", rootSession)
	var frame struct {
		Method string                 `json:"method"`
		Params map[string]interface{} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil || frame.Params == nil {
		return json.RawMessage(line)
	}
	materialized := make(map[string]any, len(frame.Params))
	for key, value := range frame.Params {
		materialized[key] = value
	}
	if *inChild {
		if _, ok := materialized["sessionId"].(string); ok {
			materialized["sessionId"] = childSession
		}
	}
	switch frame.Method {
	case "subagent.started":
		materialized["parentSessionId"] = rootSession
		materialized["childSessionId"] = childSession
		*inChild = true
	case "subagent.finished":
		materialized["parentSessionId"] = rootSession
		materialized["childSessionId"] = childSession
		materialized["agentId"] = childSession
		*inChild = false
	}
	encoded, err := json.Marshal(map[string]any{"method": frame.Method, "params": materialized})
	if err != nil {
		return json.RawMessage(line)
	}
	return encoded
}

func writeFakeRun(out *json.Encoder, root string) error {
	notifications := []map[string]any{
		{
			"jsonrpc": "2.0", "method": "session.event",
			"params": map[string]any{"sessionId": root, "event": map[string]any{
				"seq": 1, "type": "agent/inbox/spliced",
				"data": map[string]any{"inserted": []map[string]string{{"id": "message-1"}}},
			}},
		},
		{
			"jsonrpc": "2.0", "method": "session.event",
			"params": map[string]any{"sessionId": root, "event": map[string]any{
				"seq": 2, "type": "assistant/message",
				"data": map[string]any{"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "hello from fake runtime"}}}},
			}},
		},
		{
			"jsonrpc": "2.0", "method": "subagent.started",
			"params": map[string]string{"parentSessionId": root, "childSessionId": "child-session"},
		},
		{
			"jsonrpc": "2.0", "method": "session.event",
			"params": map[string]any{"sessionId": "child-session", "event": map[string]any{
				"seq": 3, "type": "assistant/message",
				"data": map[string]any{"message": map[string]any{"content": []map[string]string{{"type": "text", "text": "child"}}}},
			}},
		},
		{
			"jsonrpc": "2.0", "method": "session.status",
			"params": map[string]string{"sessionId": root, "status": "idle"},
		},
	}
	for _, notification := range notifications {
		if err := out.Encode(notification); err != nil {
			return err
		}
	}
	return nil
}
