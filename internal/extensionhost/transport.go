package extensionhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

var (
	ErrConnectionLost = errors.New("extension: connection lost")
	ErrProtocol       = errors.New("extension: protocol error")
)

type connection interface {
	Call(ctx context.Context, method string, params, result any) error
	Close() error
}

type stdioConnection struct {
	command string
	args    []string
	env     []string
	workdir string

	startMu   sync.Mutex
	started   bool
	closed    bool
	process   *exec.Cmd
	owned     ownedProcess
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stdioMu   sync.Mutex
	requests  map[uint64]chan stdioResponse
	nextID    uint64
	exited    chan struct{}
	waitErr   error
	exitCode  int
	stderr    *stderrTail
	closeOnce sync.Once
}

type stdioResponse struct {
	response extension.RPCResponse
	err      error
}

func newConnection(transport extension.Transport) (connection, error) {
	switch strings.ToLower(strings.TrimSpace(transport.Type)) {
	case "stdio":
		return &stdioConnection{
			command:  transport.Command,
			args:     append([]string(nil), transport.Args...),
			env:      append([]string(nil), transport.Env...),
			workdir:  transport.Workdir,
			requests: make(map[uint64]chan stdioResponse),
			exited:   make(chan struct{}),
		}, nil
	case "http":
		return &httpConnection{endpoint: transport.Endpoint, client: &http.Client{Timeout: 30 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("extension: unsupported transport %q", transport.Type)
	}
}

func (c *stdioConnection) Call(ctx context.Context, method string, params, result any) error {
	c.startMu.Lock()
	if c.closed {
		c.startMu.Unlock()
		return ErrConnectionClosed
	}
	if !c.started {
		if err := c.startLocked(ctx); err != nil {
			c.startMu.Unlock()
			return err
		}
		c.started = true
	}
	c.startMu.Unlock()

	c.stdioMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan stdioResponse, 1)
	c.requests[id] = ch
	c.stdioMu.Unlock()
	defer func() {
		c.stdioMu.Lock()
		delete(c.requests, id)
		c.stdioMu.Unlock()
	}()

	request := extension.RPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if params == nil {
		request.Params = nil
	}
	encoded, err := marshalJSONLine(request)
	if err != nil {
		return err
	}
	c.stdioMu.Lock()
	if c.process == nil || c.stdin == nil {
		c.stdioMu.Unlock()
		return ErrConnectionLost
	}
	_, writeErr := c.stdin.Write(encoded)
	c.stdioMu.Unlock()
	if writeErr != nil {
		return fmt.Errorf("%w: write %s: %v", ErrConnectionLost, method, writeErr)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("extension: call %s: %w", method, ctx.Err())
	case <-c.exited:
		return c.processExitError(method)
	case pending := <-ch:
		if pending.err != nil {
			if errors.Is(pending.err, errProcessExited) {
				return c.processExitError(method)
			}
			return pending.err
		}
		response := pending.response
		if response.Error != nil {
			return fmt.Errorf("%w: %s: %s", ErrProtocol, method, response.Error.Message)
		}
		if result == nil {
			return nil
		}
		if len(response.Result) == 0 {
			return fmt.Errorf("%w: %s returned no result", ErrProtocol, method)
		}
		if err := decodeStrict(response.Result, result); err != nil {
			return fmt.Errorf("%w: decode %s result: %v", ErrProtocol, method, err)
		}
		return nil
	}
}

func (c *stdioConnection) startLocked(ctx context.Context) error {
	command, err := exec.LookPath(c.command)
	if err != nil {
		return fmt.Errorf("extension: find command %q: %w", c.command, err)
	}
	cmd := exec.Command(command, c.args...)
	prepareOwnedProcess(cmd)
	cmd.Env = childEnvironment(c.env)
	if c.workdir != "" {
		cmd.Dir = c.workdir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("extension: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("extension: stdout: %w", err)
	}
	c.stderr = &stderrTail{limit: maxStderrTailBytes}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("extension: start process: %w", err)
	}
	owned, err := attachOwnedProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stdin.Close()
		return fmt.Errorf("extension: attach process tree: %w", err)
	}
	c.process = cmd
	c.owned = owned
	c.stdin = stdin
	c.stdout = stdout
	go c.read(stdout)
	go func() {
		waitErr := cmd.Wait()
		c.startMu.Lock()
		c.waitErr = waitErr
		if cmd.ProcessState != nil {
			c.exitCode = cmd.ProcessState.ExitCode()
		} else {
			c.exitCode = -1
		}
		c.startMu.Unlock()
		close(c.exited)
		c.failPending(errProcessExited)
	}()
	return nil
}

func (c *stdioConnection) read(stdout io.ReadCloser) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var response extension.RPCResponse
		if json.Unmarshal(line, &response) != nil || response.ID == 0 {
			continue
		}
		c.stdioMu.Lock()
		ch := c.requests[response.ID]
		delete(c.requests, response.ID)
		c.stdioMu.Unlock()
		if ch != nil {
			ch <- stdioResponse{response: response}
		}
	}
	_ = stdout.Close()
	c.startMu.Lock()
	if c.stdout == stdout {
		c.stdout = nil
	}
	c.startMu.Unlock()
}

func (c *stdioConnection) failPending(err error) {
	c.stdioMu.Lock()
	pending := make([]chan stdioResponse, 0, len(c.requests))
	for id, ch := range c.requests {
		pending = append(pending, ch)
		delete(c.requests, id)
	}
	c.stdioMu.Unlock()
	for _, ch := range pending {
		ch <- stdioResponse{err: err}
	}
}

func (c *stdioConnection) Close() error {
	var first error
	c.closeOnce.Do(func() {
		c.startMu.Lock()
		c.closed = true
		cmd := c.process
		owned := c.owned
		stdin := c.stdin
		stdout := c.stdout
		c.startMu.Unlock()
		if stdin != nil {
			c.stdioMu.Lock()
			first = stdin.Close()
			c.stdin = nil
			c.stdioMu.Unlock()
		}
		if cmd != nil && cmd.Process != nil {
			select {
			case <-c.exited:
			case <-time.After(3 * time.Second):
				if stdout != nil {
					_ = stdout.Close()
				}
				if owned != nil {
					if err := owned.Close(); err != nil && first == nil {
						first = err
					}
				} else if err := cmd.Process.Kill(); err != nil && first == nil {
					first = err
				}
				select {
				case <-c.exited:
				case <-time.After(3 * time.Second):
					if first == nil {
						first = fmt.Errorf("extension: process cleanup timed out")
					}
				}
			}
			if owned != nil {
				if err := owned.Close(); err != nil && first == nil {
					first = err
				}
			}
		}
		c.failPending(ErrConnectionClosed)
	})
	return first
}

var errProcessExited = errors.New("extension: process exited")

const maxStderrTailBytes = 8 * 1024

type stderrTail struct {
	mu     sync.Mutex
	limit  int
	buffer []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= t.limit {
		t.buffer = append(t.buffer[:0], p[len(p)-t.limit:]...)
		return len(p), nil
	}
	t.buffer = append(t.buffer, p...)
	if len(t.buffer) > t.limit {
		t.buffer = append([]byte(nil), t.buffer[len(t.buffer)-t.limit:]...)
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(append([]byte(nil), t.buffer...))
}

func (c *stdioConnection) processExitError(method string) error {
	c.startMu.Lock()
	waitErr := c.waitErr
	exitCode := c.exitCode
	stderr := c.stderr
	c.startMu.Unlock()
	reason := fmt.Sprintf("exit code %d", exitCode)
	if waitErr != nil && exitCode < 0 {
		reason = waitErr.Error()
	}
	message := fmt.Sprintf("process exited during %s (%s)", method, reason)
	if tail := sanitizeDiagnostic(stderr.String()); tail != "" {
		message += "; stderr tail: " + tail
	}
	return fmt.Errorf("%w: %s", ErrConnectionLost, message)
}

func sanitizeDiagnostic(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		for _, marker := range []string{"token", "secret", "password", "cookie", "authorization", "api_key", "apikey"} {
			if strings.Contains(lower, marker) {
				if index := strings.IndexAny(line, "=:"); index >= 0 {
					line = line[:index+1] + "[redacted]"
				} else {
					line = "[redacted diagnostic line]"
				}
				break
			}
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

type httpConnection struct {
	endpoint string
	client   *http.Client
	mu       sync.Mutex
	nextID   uint64
	closed   bool
}

func (c *httpConnection) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConnectionClosed
	}
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	body, err := marshalJSON(extension.RPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("extension: call %s: %w", method, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("extension: call %s: HTTP %d", method, response.StatusCode)
	}
	var wire extension.RPCResponse
	if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
		return fmt.Errorf("%w: decode %s response: %v", ErrProtocol, method, err)
	}
	if wire.Error != nil {
		return fmt.Errorf("%w: %s: %s", ErrProtocol, method, wire.Error.Message)
	}
	if result == nil {
		return nil
	}
	return decodeStrict(wire.Result, result)
}

func (c *httpConnection) Close() error {
	c.mu.Lock()
	c.closed = true
	client := c.client
	c.mu.Unlock()
	client.CloseIdleConnections()
	return nil
}

func childEnvironment(extra []string) []string {
	allow := map[string]struct{}{
		"PATH": {}, "PATHEXT": {}, "SYSTEMROOT": {}, "SYSTEMDRIVE": {},
		"COMSPEC": {}, "TEMP": {}, "TMP": {}, "HOME": {}, "USERPROFILE": {}, "LANG": {}, "LC_ALL": {},
	}
	var out []string
	for _, entry := range os.Environ() {
		name := strings.ToUpper(entry[:strings.IndexByte(entry, '=')])
		if _, ok := allow[name]; ok && !sensitiveEnvironmentName(name) {
			out = append(out, entry)
		}
	}
	for _, entry := range extra {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || strings.ContainsAny(parts[0], "= \t\r\n") || sensitiveEnvironmentName(parts[0]) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func sensitiveEnvironmentName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	return strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "PASSWORD") ||
		strings.Contains(name, "CREDENTIAL") || strings.Contains(name, "API_KEY") || strings.Contains(name, "AUTH") ||
		strings.HasSuffix(name, "_KEY")
}
