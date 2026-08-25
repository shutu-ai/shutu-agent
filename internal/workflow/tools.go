// tools.go — the GAP-3 Consumer half of the workflow seam (ADR
// 2026-08-20-standard-gaps.md D-GAP-2, 用户拍板 JSON DAG 声明式编排): the
// workflow_run tool is registered into the tools.Registry by the composition
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

	"github.com/jabing/shutu-agent/internal/session"
)

// WorkflowRunToolName is the task-DAG orchestration tool name (D-GAP-2).
const WorkflowRunToolName = "workflow_run"

// WorkflowRunTool bundles both workflow execution paths: the legacy Go DAG and
// the optional dsh-shaped JavaScript runner. The D3 event sink is shared.
type WorkflowRunTool struct {
	eng     *Engine
	script  ScriptRunner
	agent   AgentStart
	parent  func() string
	onEvent func(typ string, data any)
}

// NewWorkflowRunTool returns the workflow_run tool bound to an Engine. onEvent,
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
	return "Run a dsh-compatible JavaScript workflow script (meta/script/args with agent, parallel, pipeline, phase, and log) or a Go-native task DAG over subagents. JavaScript is the dynamic path; tasks is the fixed DAG compatibility path."
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
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":     map[string]any{"type": "string", "minLength": 1},
						"prompt": map[string]any{"type": "string", "minLength": 1},
						"depends_on": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
					"required":             []string{"id", "prompt"},
					"additionalProperties": false,
				},
				"minItems":    1,
				"description": "task DAG nodes (unique ids; depends_on lists prerequisite ids)",
			},
		},
		"anyOf": []any{
			map[string]any{"required": []string{"meta", "script"}},
			map[string]any{"required": []string{"tasks"}},
		},
		"additionalProperties": false,
	}
}

// Execute runs the model-submitted task DAG through the engine and renders the
// per-task summary. An empty tasks array is rejected; engine-level errors
// (ErrCycle, validation) are wrapped and passed through. The workflow/run
// event carries only the counts (D3 — 只记元数据, 不落输出全文).
func (t *WorkflowRunTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Meta   map[string]any `json:"meta"`
		Args   any            `json:"args"`
		Script string         `json:"script"`
		Tasks  []Task         `json:"tasks"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("workflow_run: %w", err)
	}
	if a.Script != "" {
		if t.script == nil {
			return "", fmt.Errorf("workflow_run: JavaScript provider is unavailable")
		}
		if err := validateScriptMeta(a.Meta); err != nil {
			return "", fmt.Errorf("workflow_run: %w", err)
		}
		parent := ""
		if t.parent != nil {
			parent = t.parent()
		}
		res, err := t.script.RunScript(ctx, ScriptRequest{Meta: a.Meta, Script: a.Script, Args: a.Args, ParentSessionID: parent}, t.agent, func(ev ScriptEvent) {
			t.emit(ev.Type, ev.Data)
		})
		if err != nil {
			return "", fmt.Errorf("workflow_run: JavaScript: %w", err)
		}
		if res.StopReason != "completed" {
			if res.Error == "" {
				res.Error = "workflow stopped with " + res.StopReason
			}
			return "", fmt.Errorf("workflow_run: JavaScript: %s", res.Error)
		}
		t.emit(session.EventWorkflowRun, map[string]any{"mode": "script", "stopReason": res.StopReason, "agentsStarted": res.AgentsStarted})
		return formatScriptResult(res), nil
	}
	if len(a.Tasks) == 0 {
		return "", fmt.Errorf("workflow_run: empty tasks")
	}
	rep, err := t.eng.Run(ctx, Spec{Tasks: a.Tasks})
	if err != nil {
		return "", fmt.Errorf("workflow_run: %w", err)
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
	return formatReport(rep), nil
}

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

func formatScriptResult(res ScriptResult) string {
	value, err := json.Marshal(res.Value)
	if err != nil {
		value = []byte("null")
	}
	var text string
	if string(value) != "null" {
		var s string
		if json.Unmarshal(value, &s) == nil {
			text = s
		} else {
			text = string(value)
		}
	}
	if text == "" {
		text = "null"
	}
	return fmt.Sprintf("workflow_run: JavaScript workflow (%d agents)\n  result: %s", res.AgentsStarted, text)
}

// formatReport renders the per-task summary: a header with the task count and
// one indented block per task — id, status, and a bounded output (completed)
// or error (failed) line.
func formatReport(rep Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "workflow_run: %d tasks", len(rep.Tasks))
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
