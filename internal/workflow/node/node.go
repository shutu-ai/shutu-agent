// Package node runs dsh-shaped JavaScript workflows through an external Node
// process. The Go workflow package owns the host protocol; this package owns
// process lifecycle and JSONL transport.
package node

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jabing/shutu-agent/internal/workflow"
)

//go:embed runner.mjs
var runnerSource string

const (
	defaultCommand         = "node"
	defaultMaxConcurrent   = 4
	defaultMaxTotalAgents  = 1000
	defaultMaxItemsPerCall = 4096
	defaultSyncTimeoutMS   = 5000
)

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
	nextID          uint64
}

// New returns a runner with dsh-aligned defaults.
func New(cfg Config) *Runner {
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = defaultCommand
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
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

type startMessage struct {
	Type   string         `json:"type"`
	RunID  string         `json:"run_id"`
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
	runID := fmt.Sprintf("workflow-node-%d", atomic.AddUint64(&r.nextID, 1))
	cmd := exec.CommandContext(ctx, r.command, "--input-type=module", "-e", runnerSource)
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
		return workflow.ScriptResult{}, fmt.Errorf("workflow node start (%s): %w", r.command, err)
	}
	defer stdin.Close()
	var stderrDone sync.WaitGroup
	var stderrBuf bytes.Buffer
	stderrDone.Add(1)
	go func() {
		defer stderrDone.Done()
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	var writeMu sync.Mutex
	write := func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := stdin.Write(append(data, byte(10))); err != nil {
			return err
		}
		return nil
	}
	if err := write(startMessage{Type: "start", RunID: runID, Meta: req.Meta, Script: req.Script, Args: req.Args, Limits: limitsMessage{MaxConcurrentAgents: r.maxConcurrent, MaxTotalAgents: r.maxTotalAgents, MaxItemsPerCall: r.maxItemsPerCall, SyncTimeoutMS: r.syncTimeoutMS}}); err != nil {
		_ = cmd.Process.Kill()
		return workflow.ScriptResult{}, fmt.Errorf("workflow node send start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var result *workflow.ScriptResult
	lastLine := ""
	for scanner.Scan() {
		lastLine = scanner.Text()
		var msg hostMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "event":
			var data any
			if len(msg.Data) != 0 {
				_ = json.Unmarshal(msg.Data, &data)
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
			go func(m hostMessage, o agentOptions) {
				ar, callErr := agent(ctx, workflow.AgentRequest{Prompt: m.Prompt, Label: o.Label, Phase: o.Phase, Provider: o.Provider, Model: o.Model, Schema: o.Schema})
				response := agentResultMessage{Type: "agent_result", ID: m.ID}
				if callErr != nil {
					response.Error = callErr.Error()
				} else {
					response.ChildID = ar.ID
					response.Output = ar.Output
					response.StopReason = ar.StopReason
					response.Structured = ar.Structured
				}
				_ = write(response)
			}(msg, opts)
		case "result":
			var value any
			if len(msg.Value) != 0 {
				_ = json.Unmarshal(msg.Value, &value)
			}
			result = &workflow.ScriptResult{RunID: runID, Value: value, StopReason: msg.StopReason, Error: msg.Error, AgentsStarted: msg.Agents}
		}
		if result != nil {
			break
		}
	}
	if err := scanner.Err(); err != nil && result == nil {
		_ = cmd.Process.Kill()
	}
	if result == nil && ctx.Err() != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	stderrDone.Wait()
	if result != nil {
		return *result, nil
	}
	if ctx.Err() != nil {
		return workflow.ScriptResult{}, ctx.Err()
	}
	if waitErr != nil {
		return workflow.ScriptResult{}, fmt.Errorf("workflow node exited: %w: %s", waitErr, strings.TrimSpace(stderrBuf.String()))
	}
	return workflow.ScriptResult{}, fmt.Errorf("workflow node exited without a result: stdout=%s stderr=%s", lastLine, strings.TrimSpace(stderrBuf.String()))
}
