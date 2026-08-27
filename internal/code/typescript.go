package code

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultProgramTimeout = 30 * time.Second
	defaultProgramOutput  = 64 * 1024
	maxProgramLine        = 16 * 1024 * 1024
)

// TypeScriptRuntime is the Shutu host implementation of DSH's CodeRuntime.
// Each run is an isolated Node process executing the supplied code as the body
// of an async function. Tool calls travel over a newline-delimited JSON bridge
// to the host binding; the runtime never imports or knows about the registry.
type TypeScriptRuntime struct {
	mu     sync.Mutex
	closed bool
	active map[*exec.Cmd]struct{}
}

// NewTypeScriptRuntime creates the native TypeScript Code Mode runtime. Node is
// resolved at execution time so construction remains cheap and testable.
func NewTypeScriptRuntime() *TypeScriptRuntime {
	return &TypeScriptRuntime{active: map[*exec.Cmd]struct{}{}}
}

func (r *TypeScriptRuntime) RunProgram(ctx context.Context, req ProgramRequest) (ProgramResult, error) {
	if err := ctx.Err(); err != nil {
		return ProgramResult{}, err
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ProgramResult{}, ErrEngineClosed
	}
	if strings.TrimSpace(req.Code) == "" {
		return ProgramResult{}, fmt.Errorf("code runtime: empty program")
	}
	if req.Binding == nil {
		return ProgramResult{}, fmt.Errorf("code runtime: binding callback is required")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultProgramTimeout
	}
	maxOutput := req.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultProgramOutput
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ProgramResult{}, fmt.Errorf("code runtime: resolve cwd: %w", err)
		}
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: create cwd %s: %w", cwd, err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: node executable not found: %w", err)
	}
	script, err := os.CreateTemp(cwd, ".shutu-ptc-*.ts")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: create program: %w", err)
	}
	scriptPath := script.Name()
	defer os.Remove(scriptPath)
	if _, err := script.WriteString(renderTypeScriptProgram(req.Code)); err != nil {
		script.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: write program: %w", err)
	}
	if err := script.Close(); err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: close program: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, node, "--experimental-strip-types", scriptPath)
	cmd.Dir = cwd
	cmd.Env = scrubbedEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: start: %w", err)
	}
	r.track(cmd)
	defer r.untrack(cmd)

	started := time.Now()
	result := ProgramResult{}
	completed := false
	budget := programOutputBudget{limit: maxOutput}
	encoderMu := sync.Mutex{}
	var callsWG sync.WaitGroup
	var bridgeErrMu sync.Mutex
	var bridgeErr error
	setBridgeErr := func(err error) {
		if err == nil {
			return
		}
		bridgeErrMu.Lock()
		if bridgeErr == nil {
			bridgeErr = err
		}
		bridgeErrMu.Unlock()
		cancel()
	}
	writeReply := func(id int64, name string, value any, callErr error) error {
		message := programReply{Type: "reply", ID: id, ToolName: name, OK: callErr == nil}
		if callErr != nil {
			message.Message = callErr.Error()
		} else {
			message.Value = value
		}
		encoderMu.Lock()
		defer encoderMu.Unlock()
		return json.NewEncoder(stdin).Encode(message)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxProgramLine)
	for scanner.Scan() {
		var message programMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			result.Failure = &ProgramFailure{Kind: "protocol", Message: "runtime emitted invalid JSON: " + err.Error()}
			cancel()
			break
		}
		switch message.Type {
		case "log":
			if !budget.append(&result.Logs, message.Text) {
				result.Truncated = true
				result.Failure = &ProgramFailure{Kind: "output-limit", Message: fmt.Sprintf("program output exceeded %d bytes", maxOutput)}
				cancel()
			}
		case "call":
			if strings.TrimSpace(message.Name) == "" {
				if err := writeReply(message.ID, message.Name, nil, errors.New("tool name is required")); err != nil {
					result.Failure = &ProgramFailure{Kind: "protocol", Message: "write tool error: " + err.Error()}
					cancel()
					break
				}
				continue
			}
			var args any
			if len(message.Args) == 0 || string(message.Args) == "null" {
				args = map[string]any{}
			} else if err := json.Unmarshal(message.Args, &args); err != nil {
				if err := writeReply(message.ID, message.Name, nil, fmt.Errorf("invalid tool arguments: %w", err)); err != nil {
					result.Failure = &ProgramFailure{Kind: "protocol", Message: "write argument error: " + err.Error()}
					cancel()
				}
				continue
			}
			callID := fmt.Sprintf("code:%d", message.ID)
			if req.ParentCallID != "" {
				callID = fmt.Sprintf("%s:code:%d", req.ParentCallID, message.ID)
			}
			callsWG.Add(1)
			callID, callName, callArgs := callID, message.Name, args
			callNumber := message.ID
			go func() {
				defer callsWG.Done()
				value, callErr := req.Binding(runCtx, ProgramBindingRequest{CallID: callID, Name: callName, Args: callArgs})
				if err := writeReply(callNumber, callName, value, callErr); err != nil {
					setBridgeErr(fmt.Errorf("write tool reply: %w", err))
				}
			}()
		case "done":
			completed = true
			if message.Error != nil {
				result.Failure = &ProgramFailure{Kind: message.Error.Kind, Message: message.Error.Message}
			} else if len(message.Value) > 0 {
				if err := json.Unmarshal(message.Value, &result.Value); err != nil {
					result.Failure = &ProgramFailure{Kind: "invalid-output", Message: "program completion is not valid JSON: " + err.Error()}
				} else {
					result.HasValue = true
				}
			}
		default:
			result.Failure = &ProgramFailure{Kind: "protocol", Message: fmt.Sprintf("runtime emitted unknown message type %q", message.Type)}
			cancel()
		}
		if result.Failure != nil {
			break
		}
	}
	callsWG.Wait()
	bridgeErrMu.Lock()
	if result.Failure == nil && bridgeErr != nil {
		result.Failure = &ProgramFailure{Kind: "protocol", Message: bridgeErr.Error()}
	}
	bridgeErrMu.Unlock()
	_ = stdin.Close()
	waitErr := cmd.Wait()
	result.Duration = time.Since(started)
	if result.Failure != nil {
		return result, nil
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("code runtime: read output: %w", err)
	}
	if runCtx.Err() == context.DeadlineExceeded {
		result.Failure = &ProgramFailure{Kind: "budget", Message: fmt.Sprintf("program timed out after %s", timeout)}
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if completed {
		return result, nil
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		result.Failure = &ProgramFailure{Kind: "substrate", Message: message}
		return result, nil
	}
	result.Failure = &ProgramFailure{Kind: "substrate", Message: "runtime exited without a completion message"}
	return result, nil
}

func (r *TypeScriptRuntime) track(cmd *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = cmd.Process.Kill()
		return
	}
	r.active[cmd] = struct{}{}
}

func (r *TypeScriptRuntime) untrack(cmd *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, cmd)
}

// Close stops all in-flight program processes and prevents future runs.
func (r *TypeScriptRuntime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	active := make([]*exec.Cmd, 0, len(r.active))
	for cmd := range r.active {
		active = append(active, cmd)
	}
	r.mu.Unlock()
	for _, cmd := range active {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	return nil
}

type programMessage struct {
	Type  string          `json:"type"`
	ID    int64           `json:"id"`
	Name  string          `json:"name"`
	Args  json.RawMessage `json:"args"`
	Text  string          `json:"text"`
	Value json.RawMessage `json:"value"`
	Error *programError   `json:"error"`
}

type programReply struct {
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	ToolName string `json:"toolName,omitempty"`
	OK       bool   `json:"ok"`
	Value    any    `json:"value,omitempty"`
	Message  string `json:"message,omitempty"`
}

type programError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type programOutputBudget struct {
	limit int
	used  int
}

func (b *programOutputBudget) append(logs *[]string, text string) bool {
	remaining := b.limit - b.used
	if remaining <= 0 {
		return false
	}
	data := []byte(text)
	if len(data) > remaining {
		data = data[:remaining]
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
		if len(data) > 0 {
			*logs = append(*logs, string(data))
			b.used += len(data)
		}
		return false
	}
	*logs = append(*logs, text)
	b.used += len(data)
	return true
}

func renderTypeScriptProgram(program string) string {
	return `import * as readline from 'node:readline'
import { inspect } from 'node:util'

const rawStdoutWrite = process.stdout.write.bind(process.stdout)
const rawStderrWrite = process.stderr.write.bind(process.stderr)
const pending = new Map<number, { resolve: (value: unknown) => void, reject: (error: Error) => void }>()
let nextCallId = 0

function emit(message: unknown): void {
  rawStdoutWrite(JSON.stringify(message) + '\n')
}

function render(value: unknown): string {
  return typeof value === 'string' ? value : inspect(value, { depth: 4, maxArrayLength: 100, maxStringLength: 10000 })
}

function log(...values: unknown[]): void {
  emit({ type: 'log', text: values.map(render).join(' ') })
}

const capturedConsole = { log, info: log, warn: log, error: log, debug: log }
globalThis.console = capturedConsole as Console
process.stdout.write = ((chunk: unknown): boolean => { emit({ type: 'log', text: String(chunk) }); return true }) as typeof process.stdout.write
process.stderr.write = ((chunk: unknown): boolean => { emit({ type: 'log', text: String(chunk) }); return true }) as typeof process.stderr.write

class ToolCallError extends Error {
  toolName: string
  constructor(toolName: string, message: string) {
    super(message)
    this.name = 'ToolCallError'
    this.toolName = toolName
  }
}
globalThis.ToolCallError = ToolCallError

const input = readline.createInterface({ input: process.stdin, crlfDelay: Infinity })
input.on('line', (line: string) => {
  try {
    const message = JSON.parse(line) as { type?: string, id?: number, ok?: boolean, value?: unknown, message?: string, toolName?: string }
    if (message.type !== 'reply' || typeof message.id !== 'number') return
    const call = pending.get(message.id)
    if (!call) return
    pending.delete(message.id)
    if (message.ok) call.resolve(message.value)
    else call.reject(new ToolCallError(message.toolName ?? '', message.message ?? 'tool call failed'))
  } catch {
    // Host protocol errors are surfaced when the pending call times out/aborts.
  }
})

function invoke(name: string, args: unknown): Promise<unknown> {
  const id = ++nextCallId
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject })
    emit({ type: 'call', id, name, args: args ?? {} })
  })
}

const tools = new Proxy(Object.create(null), {
  get(_target: object, property: string | symbol): unknown {
    if (typeof property !== 'string') return undefined
    return (args: unknown = {}) => invoke(property, args)
  },
}) as Record<string, (args?: unknown) => Promise<unknown>>

async function __shutuProgram(tools: Record<string, (args?: unknown) => Promise<unknown>>): Promise<unknown> {
` + program + `
}

try {
  const value = await __shutuProgram(tools)
  emit({ type: 'done', value })
} catch (error) {
  const message = error instanceof Error ? error.stack ?? error.message : String(error)
  emit({ type: 'done', error: { kind: 'exception', message } })
}
await new Promise<void>(resolve => rawStdoutWrite('', () => resolve()))
input.close()
process.stdin.pause()
`
}
