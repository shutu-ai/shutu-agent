// Package node runs dsh-shaped JavaScript workflows through an external Node
// process. The Go workflow package owns the host protocol; this package owns
// process lifecycle and JSONL transport.
package node

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/workflow"
)

//go:embed runner.mjs
var runnerSource string

const (
	defaultCommand         = "node"
	defaultMaxTotalAgents  = 1000
	defaultMaxItemsPerCall = 4096
	defaultSyncTimeoutMS   = 5000
	agentDrainGrace        = 2 * time.Second
)

func defaultMaxConcurrent() int {
	cores := runtime.GOMAXPROCS(0)
	if cores < 1 {
		cores = 1
	}
	cores -= 2
	if cores < 1 {
		cores = 1
	}
	if cores > 16 {
		cores = 16
	}
	return cores
}

// scrubbedEnv mirrors the reference worker's containment boundary. Workflow
// code is model-authored and vm isolation is not a security boundary; the
// child must therefore not inherit API keys, loader flags, or arbitrary host
// environment values even if a script escapes the vm context.
func scrubbedEnv() []string {
	if runtime.GOOS == "windows" {
		tmp := os.TempDir()
		return []string{"TMP=" + tmp, "TEMP=" + tmp}
	}
	return []string{}
}

// Config controls the external Node runner. It intentionally mirrors the
// dsh worker-thread limits while keeping Node outside the Go binary.
type Config struct {
	Command         string
	MaxConcurrent   int
	MaxTotalAgents  int
	MaxItemsPerCall int
	SyncTimeoutMS   int
}

// Runner is a one-process-per-workflow external Node provider.
type Runner struct {
	command         string
	maxConcurrent   int
	maxTotalAgents  int
	maxItemsPerCall int
	syncTimeoutMS   int
	// writeFailures counts responses rejected by the host transport. After the
	// terminal frame is observed this is the late-response rejection signal.
	writeFailures atomic.Int64
}

// New returns a runner with dsh-aligned defaults.
func New(cfg Config) *Runner {
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = defaultCommand
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent()
	}
	if cfg.MaxTotalAgents <= 0 {
		cfg.MaxTotalAgents = defaultMaxTotalAgents
	}
	if cfg.MaxItemsPerCall <= 0 {
		cfg.MaxItemsPerCall = defaultMaxItemsPerCall
	}
	if cfg.SyncTimeoutMS <= 0 {
		cfg.SyncTimeoutMS = defaultSyncTimeoutMS
	}
	return &Runner{command: cfg.Command, maxConcurrent: cfg.MaxConcurrent, maxTotalAgents: cfg.MaxTotalAgents, maxItemsPerCall: cfg.MaxItemsPerCall, syncTimeoutMS: cfg.SyncTimeoutMS}
}

// newRunID returns an opaque per-run identity. It deliberately does not restart
// at one: a process restart after a crash-open durable prefix must not mint the
// same run ID and make a new workflow look like replay of the old run.
func newRunID() (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	return "workflow-node-" + hex.EncodeToString(raw[:]), nil
}

type startMessage struct {
	Type   string         `json:"type"`
	RunID  string         `json:"run_id"`
	PID    int            `json:"workerPid"`
	Meta   map[string]any `json:"meta"`
	Script string         `json:"script"`
	Args   any            `json:"args,omitempty"`
	Limits limitsMessage  `json:"limits"`
}

type limitsMessage struct {
	MaxConcurrentAgents int `json:"maxConcurrentAgents"`
	MaxTotalAgents      int `json:"maxTotalAgents"`
	MaxItemsPerCall     int `json:"maxItemsPerCall"`
	SyncTimeoutMS       int `json:"syncTimeoutMs"`
}

type hostMessage struct {
	Type       string          `json:"type"`
	Event      string          `json:"event"`
	Data       json.RawMessage `json:"data"`
	ID         uint64          `json:"id"`
	Prompt     string          `json:"prompt"`
	Options    json.RawMessage `json:"options"`
	Value      json.RawMessage `json:"value"`
	StopReason string          `json:"stop_reason"`
	Error      string          `json:"error"`
	Output     string          `json:"output"`
	Structured json.RawMessage `json:"structured"`
	Agents     int             `json:"agents_started"`
}

type agentOptions struct {
	Label    string         `json:"label"`
	Phase    string         `json:"phase"`
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Schema   map[string]any `json:"schema"`
}

type agentResultMessage struct {
	Type       string `json:"type"`
	ID         uint64 `json:"id"`
	ChildID    string `json:"child_id,omitempty"`
	Output     string `json:"output,omitempty"`
	StopReason string `json:"stop_reason"`
	Error      string `json:"error,omitempty"`
	Structured any    `json:"structured,omitempty"`
}

// RunScript starts a fresh Node process and serves agent() calls over JSONL.
func (r *Runner) RunScript(ctx context.Context, req workflow.ScriptRequest, agent workflow.AgentStart, emit func(workflow.ScriptEvent)) (workflow.ScriptResult, error) {
	if strings.TrimSpace(req.Script) == "" {
		return workflow.ScriptResult{}, errors.New("workflow: script is empty")
	}
	if agent == nil {
		return workflow.ScriptResult{}, errors.New("workflow: script runner requires an agent capability")
	}
	runID, err := newRunID()
	if err != nil {
		return workflow.ScriptResult{}, fmt.Errorf("workflow: generate run id: %w", err)
	}
	emitWorkflowEnd := func(stopReason, message string, agentsStarted int) {
		if emit == nil {
			return
		}
		data := map[string]any{
			"run_id": runID, "meta": req.Meta, "stop_reason": stopReason,
			"agents_started": agentsStarted,
		}
		if message != "" {
			data["error"] = message
		}
		emit(workflow.ScriptEvent{Type: "workflow/end", Data: data})
	}
	if emit != nil {
		emit(workflow.ScriptEvent{Type: "workflow/start", Data: map[string]any{
			"run_id": runID, "meta": req.Meta,
		}})
	}
	cmd := exec.CommandContext(ctx, r.command, "--input-type=module", "-e", runnerSource)
	cmd.Env = scrubbedEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workflow.ScriptResult{}, fmt.Errorf("workflow node stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workflow.ScriptResult{}, fmt.Errorf("workflow node stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return workflow.ScriptResult{}, fmt.Errorf("workflow node stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			emitWorkflowEnd("cancelled", ctx.Err().Error(), 0)
			return workflow.ScriptResult{RunID: runID, StopReason: "cancelled", Error: ctx.Err().Error()}, nil
		}
		emitWorkflowEnd("error", err.Error(), 0)
		return workflow.ScriptResult{}, fmt.Errorf("workflow node start (%s): %w", r.command, err)
	}
	var stderrDone sync.WaitGroup
	var stderrBuf bytes.Buffer
	stderrDone.Add(1)
	go func() {
		defer stderrDone.Done()
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	var writeMu sync.Mutex
	writeClosed := false
	write := func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeClosed {
			return io.ErrClosedPipe
		}
		if _, err := stdin.Write(append(data, byte(10))); err != nil {
			return err
		}
		return nil
	}
	closeWrite := func() {
		writeMu.Lock()
		if !writeClosed {
			writeClosed = true
			_ = stdin.Close()
		}
		writeMu.Unlock()
	}
	defer closeWrite()
	start := startMessage{Type: "start", RunID: runID, PID: cmd.Process.Pid, Meta: req.Meta, Script: req.Script, Args: req.Args, Limits: limitsMessage{MaxConcurrentAgents: r.maxConcurrent, MaxTotalAgents: r.maxTotalAgents, MaxItemsPerCall: r.maxItemsPerCall, SyncTimeoutMS: r.syncTimeoutMS}}
	if err := write(start); err != nil {
		_ = cmd.Process.Kill()
		emitWorkflowEnd("error", err.Error(), 0)
		return workflow.ScriptResult{}, fmt.Errorf("workflow node send start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	agentCtx, cancelAgents := context.WithCancel(ctx)
	defer cancelAgents()
	var agentWG sync.WaitGroup
	liveAgents := make(map[int]map[string]any)
	var result *workflow.ScriptResult
	lastLine := ""
	var protocolErr error
	terminalClaimed := false
	claimTerminal := func() {
		if terminalClaimed {
			return
		}
		terminalClaimed = true
		// Close admission immediately when the run owns a terminal receipt.
		// In-flight agent callbacks may drain, but a response arriving after
		// this boundary is late output and must be rejected.
		closeWrite()
	}
	emitStrandedAgentEnds := func() {
		if emit == nil || len(liveAgents) == 0 {
			return
		}
		seqs := make([]int, 0, len(liveAgents))
		for seq := range liveAgents {
			seqs = append(seqs, seq)
		}
		sort.Ints(seqs)
		for _, seq := range seqs {
			data := make(map[string]any, len(liveAgents[seq])+1)
			for key, value := range liveAgents[seq] {
				data[key] = value
			}
			data["outcome"] = "cancelled"
			emit(workflow.ScriptEvent{Type: "workflow/agent-end", Data: data})
			delete(liveAgents, seq)
		}
	}
	for scanner.Scan() {
		lastLine = scanner.Text()
		msg, decodeErr := decodeHostMessage(scanner.Bytes())
		if decodeErr != nil {
			protocolErr = fmt.Errorf("workflow worker protocol: %w", decodeErr)
			claimTerminal()
			_ = cmd.Process.Kill()
			break
		}
		switch msg.Type {
		case "event":
			var data any
			if len(msg.Data) != 0 {
				_ = json.Unmarshal(msg.Data, &data)
			}
			if eventData, ok := data.(map[string]any); ok {
				switch msg.Event {
				case "workflow/agent-start":
					if seq, ok := workflowAgentSeq(eventData["seq"]); ok {
						liveAgents[seq] = eventData
					}
				case "workflow/agent-end":
					if seq, ok := workflowAgentSeq(eventData["seq"]); ok {
						delete(liveAgents, seq)
					}
					if ctx.Err() != nil {
						eventData["outcome"] = "cancelled"
					}
				}
			}
			if emit != nil {
				emit(workflow.ScriptEvent{Type: msg.Event, Data: data})
			}
		case "agent":
			var opts agentOptions
			if len(msg.Options) != 0 {
				if err := json.Unmarshal(msg.Options, &opts); err != nil {
					_ = write(agentResultMessage{Type: "agent_result", ID: msg.ID, Error: err.Error()})
					continue
				}
			}
			agentWG.Add(1)
			go func(m hostMessage, o agentOptions) {
				defer agentWG.Done()
				ar, callErr := agent(agentCtx, workflow.AgentRequest{Prompt: m.Prompt, Label: o.Label, Phase: o.Phase, Provider: o.Provider, Model: o.Model, Schema: o.Schema})
				response := agentResultMessage{Type: "agent_result", ID: m.ID}
				if callErr != nil {
					response.Error = callErr.Error()
				} else {
					response.ChildID = ar.ID
					response.Output = ar.Output
					response.StopReason = ar.StopReason
					response.Structured = ar.Structured
				}
				if err := write(response); err != nil {
					r.writeFailures.Add(1)
				}
			}(msg, opts)
		case "result":
			var value any
			if len(msg.Value) != 0 {
				_ = json.Unmarshal(msg.Value, &value)
			}
			result = &workflow.ScriptResult{RunID: runID, Value: value, StopReason: msg.StopReason, Error: msg.Error, AgentsStarted: msg.Agents}
			if ctx.Err() != nil {
				result.Value = nil
				result.StopReason = "cancelled"
				result.Error = ctx.Err().Error()
			}
			claimTerminal()
		}
		if result != nil {
			cancelAgents()
			break
		}
	}
	cancelAgents()
	callbacksDone := make(chan struct{})
	go func() {
		agentWG.Wait()
		close(callbacksDone)
	}()
	select {
	case <-callbacksDone:
	case <-time.After(agentDrainGrace):
		// A provider that ignores cancellation must not wedge workflow
		// settlement; late callback writes are rejected by closeWrite.
	}
	if err := scanner.Err(); err != nil && result == nil {
		claimTerminal()
		_ = cmd.Process.Kill()
	}
	if result == nil && ctx.Err() != nil {
		claimTerminal()
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	stderrDone.Wait()
	if protocolErr != nil {
		emitStrandedAgentEnds()
		emitWorkflowEnd("error", protocolErr.Error(), 0)
		return workflow.ScriptResult{}, fmt.Errorf("%w (last line: %q; stderr: %s)", protocolErr, lastLine, strings.TrimSpace(stderrBuf.String()))
	}
	if result != nil {
		if result.StopReason == "cancelled" {
			emitStrandedAgentEnds()
		}
		emitWorkflowEnd(result.StopReason, result.Error, result.AgentsStarted)
		return *result, nil
	}
	if ctx.Err() != nil {
		// The reference worker settles cancellation as a workflow result. The
		// external process is already terminated here, so synthesize the same
		// terminal event and preserve the run id for callers that record it.
		emitStrandedAgentEnds()
		emitWorkflowEnd("cancelled", ctx.Err().Error(), 0)
		return workflow.ScriptResult{RunID: runID, StopReason: "cancelled", Error: ctx.Err().Error()}, nil
	}
	if waitErr != nil {
		emitStrandedAgentEnds()
		emitWorkflowEnd("error", waitErr.Error(), 0)
		return workflow.ScriptResult{}, fmt.Errorf("workflow node exited: %w: %s", waitErr, strings.TrimSpace(stderrBuf.String()))
	}
	emitStrandedAgentEnds()
	emitWorkflowEnd("error", "workflow node exited without a result", 0)
	return workflow.ScriptResult{}, fmt.Errorf("workflow node exited without a result: stdout=%s stderr=%s", lastLine, strings.TrimSpace(stderrBuf.String()))
}

// decodeHostMessage is the worker-to-host protocol boundary. A malformed or
// unknown frame is never silently ignored: the worker state machine may already
// be inconsistent, so the host must terminate the run with one terminal receipt.
func decodeHostMessage(raw []byte) (hostMessage, error) {
	if !json.Valid(raw) {
		return hostMessage{}, errors.New("frame is not valid JSON")
	}
	var msg hostMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return hostMessage{}, err
	}
	switch msg.Type {
	case "event":
		if strings.TrimSpace(msg.Event) == "" {
			return hostMessage{}, errors.New("event frame has no event type")
		}
		if len(msg.Data) == 0 || !json.Valid(msg.Data) {
			return hostMessage{}, fmt.Errorf("event %q has invalid data", msg.Event)
		}
		var data any
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			return hostMessage{}, fmt.Errorf("event %q data: %w", msg.Event, err)
		}
		if _, ok := data.(map[string]any); !ok {
			return hostMessage{}, fmt.Errorf("event %q data must be an object", msg.Event)
		}
	case "agent":
		if msg.ID == 0 {
			return hostMessage{}, errors.New("agent frame has no request id")
		}
		if strings.TrimSpace(msg.Prompt) == "" {
			return hostMessage{}, errors.New("agent frame has no prompt")
		}
	case "result":
		if msg.StopReason == "" {
			return hostMessage{}, errors.New("result frame has no stop reason")
		}
	default:
		return hostMessage{}, fmt.Errorf("unknown frame type %q", msg.Type)
	}
	return msg, nil
}

func workflowAgentSeq(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number >= 1 && number == float64(int(number)) {
			return int(number), true
		}
	case int:
		if number >= 1 {
			return number, true
		}
	case json.Number:
		parsed, err := number.Int64()
		if err == nil && parsed >= 1 && parsed <= int64(^uint(0)>>1) {
			return int(parsed), true
		}
	}
	return 0, false
}
