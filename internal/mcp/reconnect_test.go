package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type reconnectClientStub struct {
	mu      sync.Mutex
	starts  int
	closed  bool
	startOK chan struct{}
	callErr error
}

type blockingReconnectStart struct {
	mu      sync.Mutex
	starts  int
	entered chan struct{}
	release chan struct{}
}

type contextBlockingReconnectStart struct {
	entered  chan struct{}
	canceled chan struct{}
}

func (c *contextBlockingReconnectStart) Start(ctx context.Context) error {
	close(c.entered)
	<-ctx.Done()
	close(c.canceled)
	return ctx.Err()
}
func (c *contextBlockingReconnectStart) ListTools(context.Context) ([]Tool, error) { return nil, nil }
func (c *contextBlockingReconnectStart) Call(context.Context, string, map[string]any) (CallResult, error) {
	return CallResult{}, ErrConnection
}
func (c *contextBlockingReconnectStart) Close() error                         { return nil }
func (c *contextBlockingReconnectStart) SetConnectionLostHandler(func(error)) {}

func (c *blockingReconnectStart) Start(context.Context) error {
	c.mu.Lock()
	c.starts++
	n := c.starts
	c.mu.Unlock()
	if n > 1 {
		close(c.entered)
		<-c.release
	}
	return nil
}

func (c *blockingReconnectStart) ListTools(context.Context) ([]Tool, error) { return nil, nil }
func (c *blockingReconnectStart) Call(context.Context, string, map[string]any) (CallResult, error) {
	return CallResult{}, ErrConnection
}
func (c *blockingReconnectStart) Close() error                         { return nil }
func (c *blockingReconnectStart) SetConnectionLostHandler(func(error)) {}

func (c *reconnectClientStub) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	if c.starts == 1 {
		return nil
	}
	select {
	case <-c.startOK:
	default:
		close(c.startOK)
	}
	return nil
}

func (c *reconnectClientStub) ListTools(context.Context) ([]Tool, error) { return nil, nil }

func (c *reconnectClientStub) Call(context.Context, string, map[string]any) (CallResult, error) {
	c.mu.Lock()
	err := c.callErr
	c.mu.Unlock()
	return CallResult{}, err
}

func (c *reconnectClientStub) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *reconnectClientStub) SetConnectionLostHandler(func(error)) {}

func (c *reconnectClientStub) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

type generationClientStub struct {
	mu          sync.Mutex
	started     int
	closed      int
	listCalls   int
	callErr     error
	tools       []Tool
	listHandler func()
}

func (c *generationClientStub) Start(context.Context) error {
	c.mu.Lock()
	c.started++
	c.mu.Unlock()
	return nil
}
func (c *generationClientStub) ListTools(context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	return append([]Tool(nil), c.tools...), nil
}
func (c *generationClientStub) Call(context.Context, string, map[string]any) (CallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CallResult{}, c.callErr
}
func (c *generationClientStub) Close() error {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
	return nil
}
func (c *generationClientStub) SetConnectionLostHandler(func(error)) {}

func (c *generationClientStub) SetToolListChangedHandler(handler func()) {
	c.mu.Lock()
	c.listHandler = handler
	c.mu.Unlock()
}

func (c *generationClientStub) signalToolListChanged() {
	c.mu.Lock()
	handler := c.listHandler
	c.mu.Unlock()
	if handler != nil {
		handler()
	}
}

func (c *generationClientStub) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCalls
}

type blockingDiscoveryClient struct {
	mu           sync.Mutex
	closed       bool
	entered      chan struct{}
	release      chan struct{}
	closeEntered chan struct{}
}

type inFlightReconnectCallClient struct {
	mu            sync.Mutex
	lostHandler   func(error)
	callStarted   chan struct{}
	callRelease   chan struct{}
	callReturned  chan struct{}
	closeEntered  chan struct{}
	callStartOnce sync.Once
	closeOnce     sync.Once
	returnOnce    sync.Once
}

func (c *inFlightReconnectCallClient) Start(context.Context) error { return nil }

func (c *inFlightReconnectCallClient) ListTools(context.Context) ([]Tool, error) {
	return nil, nil
}

func (c *inFlightReconnectCallClient) Call(context.Context, string, map[string]any) (CallResult, error) {
	c.callStartOnce.Do(func() { close(c.callStarted) })
	c.mu.Lock()
	handler := c.lostHandler
	c.mu.Unlock()
	if handler != nil {
		handler(ErrConnection)
	}
	<-c.callRelease
	c.returnOnce.Do(func() { close(c.callReturned) })
	return CallResult{}, ErrConnection
}

func (c *inFlightReconnectCallClient) Close() error {
	c.closeOnce.Do(func() { close(c.closeEntered) })
	return nil
}

func (c *inFlightReconnectCallClient) SetConnectionLostHandler(handler func(error)) {
	c.mu.Lock()
	c.lostHandler = handler
	c.mu.Unlock()
}

func (c *blockingDiscoveryClient) Start(context.Context) error { return nil }
func (c *blockingDiscoveryClient) ListTools(context.Context) ([]Tool, error) {
	c.mu.Lock()
	entered, release := c.entered, c.release
	c.mu.Unlock()
	close(entered)
	<-release
	return nil, nil
}
func (c *blockingDiscoveryClient) Call(context.Context, string, map[string]any) (CallResult, error) {
	return CallResult{}, nil
}
func (c *blockingDiscoveryClient) Close() error {
	c.mu.Lock()
	closeEntered := c.closeEntered
	c.mu.Unlock()
	close(closeEntered)
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *blockingDiscoveryClient) SetConnectionLostHandler(func(error)) {}

func (c *generationClientStub) counts() (started, closed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started, c.closed
}

func TestReconnectingClientUsesFreshGenerationFactory(t *testing.T) {
	first := &generationClientStub{callErr: ErrConnection}
	second := &generationClientStub{}
	created := make(chan *generationClientStub, 1)
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		created <- second
		return second, nil
	}, ReconnectOptions{Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1}).(*ReconnectingClient)
	defer client.Close()
	reconnected := make(chan struct{})
	client.SetReconnectedHandler(func() { close(reconnected) })
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call error = %v, want ErrConnection", err)
	}
	select {
	case got := <-created:
		if got != second {
			t.Fatalf("factory returned %p, want %p", got, second)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh generation was not created")
	}
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("fresh generation did not reconnect")
	}
	firstStarted, firstClosed := first.counts()
	secondStarted, secondClosed := second.counts()
	if firstStarted != 1 || firstClosed != 1 || secondStarted != 1 || secondClosed != 0 {
		t.Fatalf("generations first=%d/%d second=%d/%d, want 1/1 and 1/0", firstStarted, firstClosed, secondStarted, secondClosed)
	}
}

func TestReconnectingClientSupervisesConnectionLoss(t *testing.T) {
	stub := &reconnectClientStub{startOK: make(chan struct{}), callErr: ErrConnection}
	client := NewReconnectingClient(stub, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxAttempts: 3,
	}).(*ReconnectingClient)
	defer client.Close()
	reconnected := make(chan struct{})
	client.SetReconnectedHandler(func() { close(reconnected) })
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call error = %v, want ErrConnection", err)
	}
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("connection supervisor did not reconnect")
	}
	if got := stub.count(); got < 2 {
		t.Fatalf("Start count = %d, want initial start plus reconnect", got)
	}
}

func TestReconnectingClientCloseInterruptsBackoff(t *testing.T) {
	stub := &reconnectClientStub{startOK: make(chan struct{}), callErr: ErrConnection}
	client := NewReconnectingClient(stub, ReconnectOptions{
		Enabled: true, InitialDelay: time.Hour, MaxDelay: time.Hour, MaxAttempts: 0,
	}).(*ReconnectingClient)
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call error = %v, want ErrConnection", err)
	}
	closed := make(chan struct{})
	go func() {
		_ = client.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt reconnect backoff")
	}
	if got := stub.count(); got != 0 {
		t.Fatalf("Start count = %d, want no background attempt before close", got)
	}
}

func TestReconnectingClientDoesNotDuplicateExplicitStart(t *testing.T) {
	stub := &reconnectClientStub{startOK: make(chan struct{}), callErr: ErrConnection}
	client := NewReconnectingClient(stub, ReconnectOptions{
		Enabled: true, InitialDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond, MaxAttempts: 2,
	}).(*ReconnectingClient)
	defer client.Close()
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call error = %v, want ErrConnection", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("explicit Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := stub.count(); got != 1 {
		t.Fatalf("Start count = %d, want one explicit recovery without duplicate supervisor Start", got)
	}
}

func TestReconnectingClientCloseWaitsForInFlightReconnectStart(t *testing.T) {
	stub := &blockingReconnectStart{entered: make(chan struct{}), release: make(chan struct{})}
	client := NewReconnectingClient(stub, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1,
	}).(*ReconnectingClient)
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	client.requestReconnect()
	select {
	case <-stub.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect Start did not begin")
	}
	closed := make(chan struct{})
	go func() {
		_ = client.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while reconnect Start was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(stub.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not drain reconnect Start")
	}
}

func TestReconnectingClientCloseCancelsInFlightFactoryStart(t *testing.T) {
	first := &generationClientStub{}
	candidate := &contextBlockingReconnectStart{entered: make(chan struct{}), canceled: make(chan struct{})}
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		return candidate, nil
	}, ReconnectOptions{Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1}).(*ReconnectingClient)
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	client.requestReconnect()
	select {
	case <-candidate.entered:
	case <-time.After(time.Second):
		t.Fatal("factory generation did not begin")
	}
	closed := make(chan struct{})
	go func() {
		_ = client.Close()
		close(closed)
	}()
	select {
	case <-candidate.canceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel in-flight generation start")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not quiesce after cancelling generation start")
	}
}

func TestReconnectingClientDoesNotResetCrashLoopBudgetAfterBriefReconnect(t *testing.T) {
	stub := &reconnectClientStub{startOK: make(chan struct{}), callErr: ErrConnection}
	client := NewReconnectingClient(stub, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: 20 * time.Millisecond, MaxAttempts: 2,
	}).(*ReconnectingClient)
	defer client.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	reconnected := make(chan struct{}, 4)
	client.SetReconnectedHandler(func() {
		reconnected <- struct{}{}
		client.requestReconnect()
	})
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("first Call error = %v, want ErrConnection", err)
	}
	deadline := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-reconnected:
		case <-deadline:
			t.Fatal("reconnect budget did not reach its cap")
		}
	}
	time.Sleep(80 * time.Millisecond)
	if got := stub.count(); got != 3 {
		t.Fatalf("Start count = %d, want initial plus two reconnect attempts", got)
	}
}

func TestReconnectingClientNotifiesExhaustionOnce(t *testing.T) {
	first := &reconnectClientStub{startOK: make(chan struct{}), callErr: ErrConnection}
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		return nil, ErrStartFailed
	}, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1,
	}).(*ReconnectingClient)
	defer client.Close()

	exhausted := make(chan struct{}, 2)
	client.SetReconnectExhaustedHandler(func() { exhausted <- struct{}{} })
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call error = %v, want ErrConnection", err)
	}
	select {
	case <-exhausted:
	case <-time.After(time.Second):
		t.Fatal("reconnect exhaustion was not reported")
	}
	select {
	case <-exhausted:
		t.Fatal("reconnect exhaustion was reported more than once")
	case <-time.After(20 * time.Millisecond):
	}
}

// A notification emitted by the old generation after replacement must not read
// the old tool set. The wrapper's current-generation boundary routes it to the
// live candidate, preventing stale schemas from re-entering the registry.
func TestReconnectingClientRoutesStaleListChangedToCurrentGeneration(t *testing.T) {
	first := &generationClientStub{}
	second := &generationClientStub{tools: []Tool{{Name: "current", InputSchema: map[string]any{"type": "object"}}}}
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		return second, nil
	}, ReconnectOptions{Enabled: true, InitialDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond, MaxAttempts: 1}).(*ReconnectingClient)
	defer client.Close()
	reconnected := make(chan struct{})
	client.SetReconnectedHandler(func() { close(reconnected) })
	client.SetToolListChangedHandler(func() {
		_, _ = client.ListTools(context.Background())
	})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	client.requestReconnect()
	for !client.RecoveryPending() {
		time.Sleep(time.Millisecond)
	}
	// The old generation is no longer a valid sync source once reconnect owns
	// the outage. A notification arriving during this window must be dropped,
	// exactly as the reference generation guard drops it.
	first.signalToolListChanged()
	time.Sleep(10 * time.Millisecond)
	if first.listCount() != 0 || second.listCount() != 0 {
		t.Fatalf("list-changed during reconnect lists first=%d second=%d, want 0/0", first.listCount(), second.listCount())
	}
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("replacement generation did not connect")
	}

	second.signalToolListChanged()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && second.listCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	if second.listCount() != 1 {
		t.Fatalf("current generation ListTools calls = %d, want 1", second.listCount())
	}
	if first.listCount() != 0 {
		t.Fatalf("retired generation ListTools calls = %d, want 0", first.listCount())
	}
}

// Close owns the same discovery barrier as the reconnect-start barrier: an
// in-flight ListTools must quiesce before the generation is retired.
func TestReconnectingClientCloseWaitsForInFlightListTools(t *testing.T) {
	base := &blockingDiscoveryClient{
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		closeEntered: make(chan struct{}),
	}
	client := NewReconnectingClient(base, ReconnectOptions{Enabled: false}).(*ReconnectingClient)
	listDone := make(chan error, 1)
	go func() { _, err := client.ListTools(context.Background()); listDone <- err }()
	select {
	case <-base.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery did not begin")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-base.closeEntered:
		t.Fatal("Close retired the generation during in-flight discovery")
	default:
	}
	close(base.release)
	select {
	case err := <-listDone:
		if err != nil {
			t.Fatalf("in-flight ListTools: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListTools did not settle")
	}
	select {
	case <-base.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("Close did not reach generation teardown")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not quiesce")
	}
}

// A transport can report loss before its in-flight Call has returned. The
// replacement may start immediately, but the old generation must not be
// closed until that Call has settled.
func TestReconnectingClientRetiresOldGenerationAfterInFlightCall(t *testing.T) {
	first := &inFlightReconnectCallClient{
		callStarted:  make(chan struct{}),
		callRelease:  make(chan struct{}),
		callReturned: make(chan struct{}),
		closeEntered: make(chan struct{}),
	}
	second := &generationClientStub{}
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		return second, nil
	}, ReconnectOptions{Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1}).(*ReconnectingClient)
	defer client.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "x", nil)
		callDone <- err
	}()
	select {
	case <-first.callStarted:
	case <-time.After(time.Second):
		t.Fatal("in-flight Call did not begin")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		started, _ := second.counts()
		if started > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if started, _ := second.counts(); started != 1 {
		t.Fatalf("replacement Start count = %d, want 1", started)
	}
	select {
	case <-first.closeEntered:
		t.Fatal("old generation closed while Call was still in flight")
	default:
	}

	close(first.callRelease)
	select {
	case err := <-callDone:
		if !errors.Is(err, ErrConnection) {
			t.Fatalf("Call error = %v, want ErrConnection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight Call did not settle")
	}
	select {
	case <-first.callReturned:
	case <-time.After(time.Second):
		t.Fatal("old Call did not return")
	}
	select {
	case <-first.closeEntered:
	case <-time.After(time.Second):
		t.Fatal("old generation was not retired after Call settled")
	}
}

// TestVolatileMCPReconnectHelper is the real child process used by the
// reconnect regression below. The first generation exits after acknowledging
// initialize and tools/list but before answering tools/call; the replacement
// generation is stable. The request journal makes cross-process ownership and
// call replay observable.
func TestVolatileMCPReconnectHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=^TestVolatileMCPReconnectHelper$") {
		t.Skip("helper process")
	}
	mode := os.Getenv("PA_MCP_RECONNECT_MODE")
	logPath := os.Getenv("PA_MCP_RECONNECT_LOG")
	effectPath := os.Getenv("PA_MCP_RECONNECT_EFFECT")
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	record := func(method string) {
		if logPath == "" {
			return
		}
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer file.Close()
		_, _ = file.WriteString(mode + " " + method + "\n")
		_ = file.Sync()
	}
	appendEffect := func(value string) {
		if effectPath == "" {
			return
		}
		file, err := os.OpenFile(effectPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer file.Close()
		_, _ = file.WriteString(value + "\n")
		_ = file.Sync()
	}
	for in.Scan() {
		var request struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		switch request.Method {
		case "initialize":
			record(request.Method)
			if mode == "crash-initial" {
				os.Exit(87)
			}
			_ = out.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{
					"protocolVersion": protocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": mode, "version": "1"},
				},
			})
		case "tools/list":
			record(request.Method)
			if mode == "crash-list" {
				os.Exit(87)
			}
			_ = out.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"tools": []any{map[string]any{
					"name": "echo", "inputSchema": map[string]any{"type": "object"},
				}}},
			})
		case "tools/call":
			record(request.Method)
			if mode == "crash" {
				appendEffect("committed")
				os.Exit(87)
			}
			appendEffect("replayed")
			_ = out.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "stable"}}},
			})
		default:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{}})
		}
	}
}

func newRealVolatileReconnectClient(t *testing.T, mode string, logPath string) (*ReconnectingClient, *chan Client, chan struct{}, *McpServer) {
	t.Helper()
	args := []string{"-test.run=^TestVolatileMCPReconnectHelper$"}
	effectPath := logPath + ".effects"
	crashServer := McpServer{
		Name: "volatile", Cmd: os.Args[0], Args: args,
		Env: map[string]string{
			"PA_MCP_RECONNECT_MODE":   mode,
			"PA_MCP_RECONNECT_LOG":    logPath,
			"PA_MCP_RECONNECT_EFFECT": effectPath,
		},
	}
	stableServer := McpServer{
		Name: "volatile", Cmd: os.Args[0], Args: args,
		Env: map[string]string{
			"PA_MCP_RECONNECT_MODE":   "stable",
			"PA_MCP_RECONNECT_LOG":    logPath,
			"PA_MCP_RECONNECT_EFFECT": effectPath,
		},
	}
	first := newConfiguredStdioClient(crashServer)
	replacement := make(chan Client, 1)
	reconnected := make(chan struct{})
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		candidate := newConfiguredStdioClient(stableServer)
		replacement <- candidate
		return candidate, nil
	}, ReconnectOptions{
		Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1,
	}).(*ReconnectingClient)
	t.Cleanup(func() { _ = client.Close() })
	client.SetReconnectedHandler(func() { close(reconnected) })
	return client, &replacement, reconnected, &stableServer
}

func waitRealMCPReplacement(t *testing.T, client *ReconnectingClient, replacement *chan Client, reconnected chan struct{}, ctx context.Context) {
	t.Helper()
	select {
	case <-reconnected:
	case <-ctx.Done():
		t.Fatal("real child failure did not trigger a replacement generation")
	}
	base := client.currentBase()
	select {
	case got := <-*replacement:
		if got == nil || base == nil || base != got {
			t.Fatalf("replacement generation base=%T got=%T", base, got)
		}
	default:
		t.Fatal("fresh generation was not installed")
	}
}

func readRealMCPJournal(t *testing.T, logPath string, want []string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cross-process request journal:\n%v\nwant:\n%v", got, want)
	}
}

// The initial connection belongs to the same reference outage budget. A child
// that dies during initialize must produce the first failure synchronously and
// then recover through a fresh real process.
func TestReconnectingClientRetriesRealStdioStartFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("real child-process lifecycle test")
	}
	logPath := filepath.Join(t.TempDir(), "requests.log")
	client, replacement, reconnected, _ := newRealVolatileReconnectClient(t, "crash-initial", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err == nil {
		t.Fatal("initial Start incorrectly succeeded while the helper crashed")
	}
	waitRealMCPReplacement(t, client, replacement, reconnected, ctx)
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("replacement ListTools = %#v, %v, want one echo tool", tools, err)
	}
	readRealMCPJournal(t, logPath, []string{
		"crash-initial initialize",
		"stable initialize",
		"stable tools/list",
	})
}

// Discovery is another kill point. Losing the child while it owns tools/list
// must fail that call and replace the generation before later discovery.
func TestReconnectingClientRetriesRealStdioListFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("real child-process lifecycle test")
	}
	logPath := filepath.Join(t.TempDir(), "requests.log")
	client, replacement, reconnected, _ := newRealVolatileReconnectClient(t, "crash-list", logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := client.ListTools(ctx); !errors.Is(err, ErrConnection) {
		t.Fatalf("ListTools during child crash = %v, want ErrConnection", err)
	}
	waitRealMCPReplacement(t, client, replacement, reconnected, ctx)
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("replacement ListTools = %#v, %v, want one echo tool", tools, err)
	}
	readRealMCPJournal(t, logPath, []string{
		"crash-list initialize",
		"crash-list tools/list",
		"stable initialize",
		"stable tools/list",
	})
}

// TestReconnectingClientKillsAndReplacesRealStdioGeneration drives the
// supervisor against two real MCP child processes. Killing the first child in
// the middle of tools/call must surface the failed transport call, create a
// fresh process, and later calls must go only to that generation: the failed
// side-effecting call is never replayed to the old child.
func TestReconnectingClientKillsAndReplacesRealStdioGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("real child-process lifecycle test")
	}
	logPath := filepath.Join(t.TempDir(), "requests.log")
	effectPath := logPath + ".effects"
	args := []string{"-test.run=^TestVolatileMCPReconnectHelper$"}
	crashServer := McpServer{
		Name: "volatile", Cmd: os.Args[0], Args: args,
		Env: map[string]string{
			"PA_MCP_RECONNECT_MODE":   "crash",
			"PA_MCP_RECONNECT_LOG":    logPath,
			"PA_MCP_RECONNECT_EFFECT": effectPath,
		},
	}
	stableServer := McpServer{
		Name: "volatile", Cmd: os.Args[0], Args: args,
		Env: map[string]string{
			"PA_MCP_RECONNECT_MODE":   "stable",
			"PA_MCP_RECONNECT_LOG":    logPath,
			"PA_MCP_RECONNECT_EFFECT": effectPath,
		},
	}
	first := newConfiguredStdioClient(crashServer)
	replacement := make(chan Client, 1)
	reconnected := make(chan struct{})
	client := NewReconnectingClientWithFactory(first, func(context.Context) (Client, error) {
		candidate := newConfiguredStdioClient(stableServer)
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
		t.Fatalf("first real generation Start: %v", err)
	}
	if tools, err := client.ListTools(ctx); err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("first ListTools = %#v, %v, want one echo tool", tools, err)
	}
	if _, err := client.Call(ctx, "echo", map[string]any{}); !errors.Is(err, ErrConnection) {
		t.Fatalf("Call during child crash = %v, want ErrConnection", err)
	}
	select {
	case <-reconnected:
	case <-ctx.Done():
		t.Fatal("real child crash did not trigger a replacement generation")
	}
	base := client.currentBase()
	select {
	case got := <-replacement:
		if got == nil || base == nil || base == Client(first) {
			t.Fatalf("replacement generation base=%T got=%T", base, got)
		}
	default:
		t.Fatal("fresh generation was not installed")
	}
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("replacement ListTools = %#v, %v, want one echo tool", tools, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		"crash initialize",
		"crash tools/list",
		"crash tools/call",
		"stable initialize",
		"stable tools/list",
	}
	gotLines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	if !reflect.DeepEqual(gotLines, wantLines) {
		t.Fatalf("cross-process request journal:\n%v\nwant:\n%v", gotLines, wantLines)
	}

	effectData, err := os.ReadFile(effectPath)
	if err != nil {
		t.Fatalf("read external effect journal: %v", err)
	}
	if strings.TrimSpace(strings.ReplaceAll(string(effectData), "\r\n", "\n")) != "committed" {
		t.Fatalf("external effect journal = %q, want exactly one committed effect", string(effectData))
	}
}
