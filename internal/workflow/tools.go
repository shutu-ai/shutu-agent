// tools.go — the GAP-3 Consumer half of the workflow seam (ADR
// 2026-08-20-standard-gaps.md D-GAP-2, 用户拍板 JSON DAG 声明式编排): the
// workflow tool is registered into the tools.Registry by the composition
// root (cmd/pa) when workflow.enabled, and auto-whitelisted by
// config.applyDefaults the same way the ralph/fs_search tools are. It
// implements the tools.Tool method set structurally (Go structural typing), so
// this package never imports the tools package — the seam stays decoupled. D7
// is enforced by the registry. D3 event logging lives here: a settled workflow
// run emits workflow/run.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/jabing/shutu-agent/internal/session"
)

// WorkflowRunToolName is the task-DAG orchestration tool name (D-GAP-2).
const WorkflowRunToolName = "workflow"

// WorkflowRunTool bundles both workflow execution paths: the legacy Go DAG and
// the optional dsh-shaped JavaScript runner. The D3 event sink is shared.
type WorkflowRunTool struct {
	eng     *Engine
	script  ScriptRunner
	agent   AgentStart
	parent  func() string
	onEvent func(typ string, data any)
}

// NewWorkflowRunTool returns the workflow tool bound to an Engine. onEvent,
// when non-nil, receives the workflow/run payload; the composition root wires
// it to the session log (D3).
func NewWorkflowRunTool(eng *Engine, onEvent func(typ string, data any)) *WorkflowRunTool {
	return &WorkflowRunTool{eng: eng, onEvent: onEvent}
}

// NewWorkflowRunToolWithScript adds the dsh-compatible JavaScript path while
// preserving the existing Go DAG path. parent supplies the active session id
// at execution time so /new and /resume switches are honored.
func NewWorkflowRunToolWithScript(eng *Engine, script ScriptRunner, agent AgentStart, parent func() string, onEvent func(typ string, data any)) *WorkflowRunTool {
	return &WorkflowRunTool{eng: eng, script: script, agent: agent, parent: parent, onEvent: onEvent}
}

func (t *WorkflowRunTool) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (WorkflowRunTool) Name() string { return WorkflowRunToolName }

func (WorkflowRunTool) Description() string {
	return "Run a JavaScript workflow script that orchestrates subagents at scale. Provide meta (required name and description, optional whenToUse and phases), a plain JavaScript script body with top-level await, and optional args. The script exposes agent, pipeline, parallel, phase, and log; agent supports label, phase, provider, model, and object-rooted JSON schema options. Ordinary child or stage failures return null; invalid hooks, unsupported options/schemas, and cap violations fail the whole workflow. The call runs in the foreground and returns only after the script completes."
}

func (WorkflowRunTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"meta": map[string]any{
				"type":        "object",
				"description": "Workflow metadata for the dsh JavaScript script path.",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"whenToUse":   map[string]any{"type": "string"},
					"phases": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":    map[string]any{"type": "string", "minLength": 1},
							"detail":   map[string]any{"type": "string"},
							"provider": map[string]any{"type": "string"},
							"model":    map[string]any{"type": "string"},
						},
						"required":             []string{"title"},
						"additionalProperties": true,
					}},
				},
				"additionalProperties": true,
			},
			"args": map[string]any{
				"type":        "object",
				"description": "JSON object exposed to the JavaScript workflow as args.",
			},
			"script": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "JavaScript workflow body; top-level return and await are supported.",
			},
		},
		"required":             []string{"meta", "script"},
		"additionalProperties": false,
	}
}

// Execute runs the model-submitted task DAG through the engine and renders the
// per-task summary. An empty tasks array is rejected; engine-level errors
// (ErrCycle, validation) are wrapped and passed through. The workflow/run
// event carries only the counts (D3 — 只记元数据, 不落输出全文).
func (t *WorkflowRunTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Meta   map[string]any `json:"meta"`
		Args   any            `json:"args"`
		Script string         `json:"script"`
		Tasks  []Task         `json:"tasks"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("workflow: %w", err)
	}
	if a.Script != "" {
		if t.script == nil {
			return agenttools.ToolResult{}, fmt.Errorf("workflow: JavaScript provider is unavailable")
		}
		if t.agent == nil {
			return agenttools.ToolResult{}, fmt.Errorf("workflow tool requires a calling agent")
		}
		if err := validateScriptMeta(a.Meta); err != nil {
			return agenttools.ToolResult{}, fmt.Errorf("workflow: %w", err)
		}
		parent := ""
		if t.parent != nil {
			parent = t.parent()
		}
		res, err := t.script.RunScript(ctx, ScriptRequest{Meta: a.Meta, Script: a.Script, Args: a.Args, ParentSessionID: parent}, t.agent, func(ev ScriptEvent) {
			t.emit(ev.Type, ev.Data)
		})
		if err != nil {
			return agenttools.ToolResult{}, fmt.Errorf("workflow: JavaScript: %w", err)
		}
		if res.StopReason != "completed" {
			if res.Error == "" {
				res.Error = "workflow stopped with " + res.StopReason
			}
			return agenttools.ToolResult{}, scriptStopError(res)
		}
		if res.RunID == "" {
			res.RunID = fmt.Sprintf("workflow-%d", atomic.AddUint64(&runSequence, 1))
		}
		t.emit(session.EventWorkflowRun, map[string]any{"mode": "script", "stopReason": res.StopReason, "agentsStarted": res.AgentsStarted})
		value := map[string]any{"runId": res.RunID, "agentsStarted": res.AgentsStarted, "result": res.Value}
		return agenttools.ToolResult{Value: value, Output: formatScriptResult(a.Meta["name"].(string), res)}, nil
	}
	if len(a.Tasks) == 0 {
		return agenttools.ToolResult{}, fmt.Errorf("workflow: empty tasks")
	}
	if t.eng == nil {
		return agenttools.ToolResult{}, fmt.Errorf("workflow: task DAG provider is unavailable")
	}
	rep, err := t.eng.Run(ctx, Spec{Tasks: a.Tasks})
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("workflow: %w", err)
	}
	completed, failed := 0, 0
	for _, tr := range rep.Tasks {
		if tr.Status == StatusCompleted {
			completed++
		} else {
			failed++
		}
	}
	t.emit(session.EventWorkflowRun, session.NewWorkflowRun(len(rep.Tasks), completed, failed))
	text := formatReport(rep)
	return agenttools.ToolResult{Value: text, Output: text}, nil
}

// Execute is the text projection required by the base Tool interface.
func (t *WorkflowRunTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

var runSequence uint64

const maxWorkflowResultChars = 50000

func validateScriptMeta(meta map[string]any) error {
	if meta == nil {
		return fmt.Errorf("JavaScript workflow meta is required")
	}
	for _, key := range []string{"name", "description"} {
		value, ok := meta[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("JavaScript workflow meta.%s must be a non-empty string", key)
		}
	}
	if phases, ok := meta["phases"]; ok {
		items, ok := phases.([]any)
		if !ok {
			return fmt.Errorf("JavaScript workflow meta.phases must be an array")
		}
		for i, raw := range items {
			phase, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("JavaScript workflow meta.phases[%d] must be an object", i)
			}
			title, ok := phase["title"].(string)
			if !ok || strings.TrimSpace(title) == "" {
				return fmt.Errorf("JavaScript workflow meta.phases[%d].title must be a non-empty string", i)
			}
		}
	}
	return nil
}

func scriptStopError(res ScriptResult) error {
	switch res.StopReason {
	case "cancelled":
		if res.Error != "" {
			return fmt.Errorf("workflow run was cancelled (%s)", res.Error)
		}
		return fmt.Errorf("workflow run was cancelled")
	case "error":
		if res.Error != "" {
			return fmt.Errorf("workflow run failed: %s", res.Error)
		}
		return fmt.Errorf("workflow run failed: unknown error")
	default:
		return fmt.Errorf("workflow run ended abnormally (%s)", res.StopReason)
	}
}

func formatScriptResult(name string, res ScriptResult) string {
	value, err := json.MarshalIndent(res.Value, "", "  ")
	if err != nil {
		value = []byte("null")
	}
	text := string(value)
	if text == "" {
		text = "null"
	}
	if utf8.RuneCountInString(text) > maxWorkflowResultChars {
		runes := []rune(text)
		text = string(runes[:maxWorkflowResultChars]) + fmt.Sprintf("\n—[truncated: %d more characters]", len(runes)-maxWorkflowResultChars)
	}
	word := "agents"
	if res.AgentsStarted == 1 {
		word = "agent"
	}
	return fmt.Sprintf("workflow %q completed (%d %s).\nReturn value:\n%s", name, res.AgentsStarted, word, text)
}

// formatReport renders the per-task summary: a header with the task count and
// one indented block per task — id, status, and a bounded output (completed)
// or error (failed) line.
func formatReport(rep Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "workflow: %d tasks", len(rep.Tasks))
	for _, tr := range rep.Tasks {
		fmt.Fprintf(&sb, "\n  %s: %s", tr.ID, tr.Status)
		if tr.Status == StatusCompleted {
			fmt.Fprintf(&sb, "\n    output: %s", boundRunes(tr.Output, 400))
		} else {
			fmt.Fprintf(&sb, "\n    error: %s", boundRunes(tr.Error, 400))
		}
	}
	return sb.String()
}
