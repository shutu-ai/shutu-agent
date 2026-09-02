package sdkclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// ProtocolError reports a response that cannot be interpreted as the
// documented SDK runtime protocol.
type ProtocolError struct{ Detail string }

func (e *ProtocolError) Error() string { return "sdk protocol violation: " + e.Detail }

// ClientClosedError reports that a subprocess-owning client has no live
// runtime. It retains the best-known exit disposition and bounded stderr tail.
type ClientClosedError struct {
	Reason     string
	ExitCode   *int
	StderrTail []string
}

func (e *ClientClosedError) Error() string {
	parts := []string{e.Reason}
	if e.ExitCode != nil {
		parts = append(parts, "exit code: "+strconv.Itoa(*e.ExitCode))
	}
	if len(e.StderrTail) != 0 {
		parts = append(parts, append([]string{"stderr tail:"}, e.StderrTail...)...)
	}
	return joinLines(parts)
}

// ClientOptions is the complete launch and timeout specification for one
// runtime subprocess.
type ClientOptions struct {
	Command         string
	Args            []string
	Dir             string
	Env             []string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	EOFGrace        time.Duration
	TerminateGrace  time.Duration
}

func (o ClientOptions) withDefaults() ClientOptions {
	options := o
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = time.Second
	}
	if options.EOFGrace == 0 {
		options.EOFGrace = 6 * time.Second
	}
	if options.TerminateGrace == 0 {
		options.TerminateGrace = 3 * time.Second
	}
	return options
}

// Client owns the SDK runtime subprocess, its stdio transport, and all
// notification subscriptions. Close is idempotent and terminal.
type Client struct {
	options ClientOptions

	startMu    sync.Mutex
	stateMu    sync.Mutex
	started    bool
	closing    bool
	closed     bool
	spawnErr   error
	exitCode   *int
	stderrTail []string

	cmd            *exec.Cmd
	processStdin   io.WriteCloser
	transport      *LineTransport
	requestHandler RequestHandler
	processDone    chan processResult
	processExited  chan struct{}
	processSettled chan struct{}
	closeOnce      sync.Once
	closeErr       error

	subMu          sync.Mutex
	subscriptions  map[int]*subscription
	sessionParents map[string]string
	subscriptionID int
}

type processResult struct {
	code int
	err  error
}

// NewClient creates an unstarted process client.
func NewClient(options ClientOptions) *Client {
	return &Client{
		options:        options.withDefaults(),
		subscriptions:  make(map[int]*subscription),
		sessionParents: make(map[string]string),
	}
}

// Start spawns the runtime and begins reading notifications. It is idempotent
// while live and refuses reuse after Close.
func (c *Client) Start() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.stateMu.Lock()
	if c.closed || c.closing {
		c.stateMu.Unlock()
		return c.closedError("DeepSeek Harness runtime client is closed")
	}
	if c.started {
		c.stateMu.Unlock()
		return c.spawnErr
	}
	// A previous failed spawn is recoverable; this attempt owns a new
	// process and must not inherit the old failure disposition.
	c.spawnErr = nil
	c.started = true
	c.stateMu.Unlock()

	cmd := exec.Command(c.options.Command, c.options.Args...)
	cmd.Dir = c.options.Dir
	if c.options.Env != nil {
		cmd.Env = c.options.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.recordSpawnFailure(err)
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		c.recordSpawnFailure(err)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		c.recordSpawnFailure(err)
		return err
	}
	c.cmd = cmd
	c.processStdin = stdin
	c.processDone = make(chan processResult, 1)
	processExited := make(chan struct{})
	processSettled := make(chan struct{})
	stderrDone := make(chan struct{})
	c.processExited = processExited
	c.processSettled = processSettled
	c.transport = NewLineTransport(stdout, stdin, c.dispatchNotification, c.handleTransportClosed)
	if c.requestHandler != nil {
		c.transport.OnRequest(c.requestHandler)
	}
	if err := cmd.Start(); err != nil {
		failedTransport := c.transport
		failedProcessDone := c.processDone
		c.stateMu.Lock()
		c.spawnErr = err
		// A failed spawn did not establish a runtime. Keep this Client
		// reusable so callers can correct a transient launch condition and
		// retry Start on a fresh process, matching the reference SDK.
		c.started = false
		c.cmd = nil
		c.processStdin = nil
		c.transport = nil
		c.processDone = nil
		c.processExited = nil
		c.processSettled = nil
		c.stateMu.Unlock()
		_ = failedTransport.Close()
		failedProcessDone <- processResult{code: -1, err: err}
		c.failSubscriptions(c.closedError("DeepSeek Harness runtime failed to start"))
		return err
	}
	go func() {
		defer close(stderrDone)
		c.readStderr(stderr)
	}()
	go c.waitProcess(processExited)
	go func(processExited <-chan struct{}) {
		<-processExited
		<-stderrDone
		close(processSettled)
	}(processExited)
	c.transport.Start()
	return nil
}

// OnRequest installs the server-to-client callback handler before the runtime
// starts. Nil leaves the transport's JSON-RPC `-32601` default response.
func (c *Client) OnRequest(handler RequestHandler) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed || c.closing {
		return c.closedError("DeepSeek Harness runtime client is closed")
	}
	if c.started {
		return errors.New("SDK callback handler must be installed before runtime start")
	}
	c.requestHandler = handler
	return nil
}

func (c *Client) recordSpawnFailure(err error) {
	c.stateMu.Lock()
	c.spawnErr = err
	c.started = false
	c.cmd = nil
	c.processStdin = nil
	c.transport = nil
	c.processDone = nil
	c.processExited = nil
	c.processSettled = nil
	c.stateMu.Unlock()
}

func (c *Client) readStderr(reader io.Reader) {
	lines := bufio.NewScanner(reader)
	lines.Buffer(make([]byte, 4096), 64<<10)
	var pending string
	for lines.Scan() {
		pending += lines.Text()
		c.appendStderr(pending)
		pending = ""
	}
	if pending != "" {
		c.appendStderr(pending)
	}
}

func (c *Client) appendStderr(line string) {
	if line == "" {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.stderrTail = append(c.stderrTail, line)
	if len(c.stderrTail) > 400 {
		c.stderrTail = c.stderrTail[len(c.stderrTail)-400:]
	}
}

func (c *Client) waitProcessSettled(timeout time.Duration) {
	c.stateMu.Lock()
	settled := c.processSettled
	c.stateMu.Unlock()
	if settled == nil {
		return
	}
	if timeout <= 0 {
		<-settled
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-settled:
	case <-timer.C:
	}
}

func (c *Client) waitProcess(exited chan struct{}) {
	defer close(exited)
	err := c.cmd.Wait()
	result := processResult{code: -1, err: err}
	if c.cmd.ProcessState != nil {
		result.code = c.cmd.ProcessState.ExitCode()
	}
	c.stateMu.Lock()
	code := result.code
	c.exitCode = &code
	c.stateMu.Unlock()
	c.processDone <- result
}

func (c *Client) handleTransportClosed() {
	go func() {
		deadline := time.Now().Add(100 * time.Millisecond)
		for time.Now().Before(deadline) {
			c.stateMu.Lock()
			settled := c.spawnErr != nil || c.exitCode != nil
			c.stateMu.Unlock()
			if settled {
				break
			}
			time.Sleep(time.Millisecond)
		}
		c.failSubscriptions(c.closedError("DeepSeek Harness runtime transport closed"))
	}()
}

// Request sends a raw SDK JSON-RPC request.
func (c *Client) Request(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if err := c.Start(); err != nil {
		return nil, err
	}
	c.stateMu.Lock()
	closing := c.closing
	c.stateMu.Unlock()
	if closing {
		return nil, c.closedError("DeepSeek Harness runtime client is closed")
	}
	raw, err := c.transport.Request(ctx, method, params, c.options.RequestTimeout)
	if err == nil {
		return raw, nil
	}
	// Protocol errors, caller cancellation and the explicit request timeout
	// are already meaningful wire/client outcomes. Transport/process loss is
	// different: match dsh by enriching it with the best-known exit code and
	// bounded stderr context so callers can distinguish a dead runtime from a
	// rejected request.
	var responseErr *ResponseError
	var timeoutErr *RequestTimeoutError
	if errors.As(err, &responseErr) || errors.As(err, &timeoutErr) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	c.waitProcessSettled(100 * time.Millisecond)
	return nil, c.closedError("DeepSeek Harness runtime transport closed")
}

// Initialize performs the process-wide handshake and validates server identity.
func (c *Client) Initialize(ctx context.Context, params InitializeParams) (InitializeResult, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return InitializeResult{}, err
	}
	raw, err := c.Request(ctx, "initialize", encoded)
	if err != nil {
		return InitializeResult{}, err
	}
	var result InitializeResult
	if json.Unmarshal(raw, &result) != nil || result.ServerInfo.Name == "" || result.ServerInfo.Version == "" {
		return InitializeResult{}, &ProtocolError{Detail: fmt.Sprintf("initialize returned no server identity: %s", raw)}
	}
	return result, nil
}

// Prompt queues one durable user turn and returns its inbox receipt identity.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock) (string, error) {
	params := SessionPromptParams{SessionID: sessionID, ContentBlocks: blocks}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	raw, err := c.Request(ctx, "session/prompt", encoded)
	if err != nil {
		return "", err
	}
	var result SessionPromptResult
	if json.Unmarshal(raw, &result) != nil || result.MessageID == "" {
		return "", &ProtocolError{Detail: fmt.Sprintf("session/prompt returned no message id: %s", raw)}
	}
	return result.MessageID, nil
}

// Snapshot queries the runtime's canonical durable projection for one
// existing session. The server may expose this as an optional extension while
// the core prompt/event wire remains reference-compatible.
func (c *Client) Snapshot(ctx context.Context, sessionID string) (SessionSnapshot, error) {
	encoded, err := json.Marshal(map[string]string{"sessionId": sessionID})
	if err != nil {
		return SessionSnapshot{}, err
	}
	raw, err := c.Request(ctx, "session/snapshot", encoded)
	if err != nil {
		return SessionSnapshot{}, err
	}
	var result SessionSnapshot
	if json.Unmarshal(raw, &result) != nil || result.SessionID == "" {
		return SessionSnapshot{}, &ProtocolError{Detail: fmt.Sprintf("session/snapshot returned no session id: %s", raw)}
	}
	return result, nil
}

// Subscribe returns a detachable notification stream. The filter runs on the
// transport reader goroutine and must be fast and side-effect free.
func (c *Client) Subscribe(filter NotificationFilter) *SubscriptionHandle {
	state := &subscription{filter: filter}
	c.subMu.Lock()
	c.subscriptionID++
	id := c.subscriptionID
	state.detach = func() {
		c.subMu.Lock()
		delete(c.subscriptions, id)
		c.subMu.Unlock()
	}
	c.stateMu.Lock()
	dead := c.closed || c.closing || c.spawnErr != nil || c.exitCode != nil
	c.stateMu.Unlock()
	if dead {
		state.fail(c.closedError("DeepSeek Harness runtime closed"))
	} else {
		c.subscriptions[id] = state
	}
	c.subMu.Unlock()
	return &SubscriptionHandle{state: state}
}

// SubscribeSessionTree scopes notifications to a root session plus descendants
// discovered from subagent.started lineage edges.
func (c *Client) SubscribeSessionTree(sessionID string) *SubscriptionHandle {
	return c.Subscribe(func(notification Notification) bool {
		if notification.Method == "subagent.started" || notification.Method == "subagent.finished" {
			values := notificationString(notification, "parentSessionId", "childSessionId")
			parent, child := values["parentSessionId"], values["childSessionId"]
			if parent != "" && c.isDescendantOf(parent, sessionID) {
				return true
			}
			return child == sessionID
		}
		values := notificationString(notification, "sessionId")
		related := values["sessionId"]
		return related != "" && c.isDescendantOf(related, sessionID)
	})
}

func (c *Client) isDescendantOf(sessionID, rootID string) bool {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	visited := make(map[string]struct{})
	for {
		if sessionID == rootID {
			return true
		}
		if _, ok := visited[sessionID]; ok {
			return false
		}
		visited[sessionID] = struct{}{}
		parent, ok := c.sessionParents[sessionID]
		if !ok {
			return false
		}
		sessionID = parent
	}
}

func (c *Client) dispatchNotification(notification Notification) {
	if notification.Method == "subagent.started" {
		values := notificationString(notification, "parentSessionId", "childSessionId")
		parent, child := values["parentSessionId"], values["childSessionId"]
		if parent != "" && child != "" && parent != child {
			c.subMu.Lock()
			c.sessionParents[child] = parent
			c.subMu.Unlock()
		}
	}
	c.subMu.Lock()
	subscriptions := make([]*subscription, 0, len(c.subscriptions))
	for _, state := range c.subscriptions {
		subscriptions = append(subscriptions, state)
	}
	c.subMu.Unlock()
	for _, state := range subscriptions {
		state.push(notification)
	}
}

func (c *Client) failSubscriptions(err error) {
	c.subMu.Lock()
	subscriptions := make([]*subscription, 0, len(c.subscriptions))
	for _, state := range c.subscriptions {
		subscriptions = append(subscriptions, state)
	}
	c.subMu.Unlock()
	for _, state := range subscriptions {
		state.fail(err)
	}
}

// Close requests protocol shutdown, then closes stdin and escalates through
// graceful termination and kill until the child has actually exited.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.performClose() })
	return c.closeErr
}

func (c *Client) performClose() error {
	c.startMu.Lock()
	c.stateMu.Lock()
	if c.closed || c.cmd == nil {
		c.closed = true
		c.stateMu.Unlock()
		c.startMu.Unlock()
		// Subscriptions are allowed before Start so callers can establish
		// their observation boundary first. Closing an unstarted client (or a
		// client whose spawn already failed) must still settle those handles;
		// otherwise Next would wait forever with no producer left.
		c.failSubscriptions(c.closedError("DeepSeek Harness runtime client is closed"))
		return nil
	}
	c.closing = true
	transport := c.transport
	c.stateMu.Unlock()
	if !c.started {
		c.stateMu.Lock()
		c.closed = true
		c.stateMu.Unlock()
		c.startMu.Unlock()
		return nil
	}
	c.startMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.options.ShutdownTimeout)
	_, _ = transport.Request(ctx, "shutdown", nil, c.options.ShutdownTimeout)
	cancel()

	if err := c.disposeProcess(); err != nil {
		c.finishClose(transport)
		return err
	}
	c.finishClose(transport)
	return nil
}

func (c *Client) finishClose(transport *LineTransport) {
	if transport != nil {
		_ = transport.Close()
	}
	c.stateMu.Lock()
	c.closed = true
	c.stateMu.Unlock()
	c.failSubscriptions(c.closedError("DeepSeek Harness runtime closed"))
}

func (c *Client) disposeProcess() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	select {
	case <-c.processDone:
		return nil
	default:
	}
	_ = c.processStdin.Close()
	if waitResult(c.processDone, c.options.EOFGrace) {
		return nil
	}
	if runtime.GOOS != "windows" {
		_ = c.cmd.Process.Signal(os.Interrupt)
		if waitResult(c.processDone, c.options.TerminateGrace) {
			return nil
		}
	}
	if err := c.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("force-terminate SDK runtime: %w", err)
	}
	if !waitResult(c.processDone, c.options.TerminateGrace) {
		return fmt.Errorf("SDK runtime did not exit within %s after force termination", c.options.TerminateGrace)
	}
	return nil
}

func waitResult(done <-chan processResult, timeout time.Duration) bool {
	if timeout <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (c *Client) closedError(reason string) error {
	c.stateMu.Lock()
	value := &ClientClosedError{Reason: reason}
	if c.spawnErr != nil {
		value.Reason += ": spawn error: " + c.spawnErr.Error()
	}
	if c.exitCode != nil {
		code := *c.exitCode
		value.ExitCode = &code
	}
	if len(c.stderrTail) != 0 {
		value.StderrTail = append([]string(nil), c.stderrTail...)
	}
	c.stateMu.Unlock()
	return value
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i != 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
