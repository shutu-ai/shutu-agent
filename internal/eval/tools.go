// tools.go — the Eval-3a Consumer half of the eval seam (ADR D-EVAL-5):
// eval_run, eval_result and eval_list are registered into the tools.Registry by
// the composition root (cmd/pa) when eval.enabled, and auto-whitelisted by
// config.applyDefaults the same way the job_* tools are. They implement the
// tools.Tool method set structurally (Go structural typing), so this package
// never imports the tools package — the seam stays decoupled. D7 is enforced by
// the registry. D3 event logging lives here: eval_run emits eval/run.
package eval

import (
	"context"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"

	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

// Tool names (whitelisted when eval.enabled; see config.evalToolNames).
const (
	ToolRunName    = "eval_run"
	ToolResultName = "eval_result"
	ToolListName   = "eval_list"
)

// listDefaultLimit is eval_list's page size when limit is absent.
const listDefaultLimit = 20

// EvalTools bundles the shared state of the three eval_* tools.
type EvalTools struct {
	eng     Engine
	onEvent func(typ string, data any)
}

// NewEvalTools returns the shared eval-tool bundle bound to an Engine. onEvent,
// when non-nil, receives the eval/* event payloads; the composition root wires
// it to the session log (D3).
func NewEvalTools(eng Engine, onEvent func(typ string, data any)) *EvalTools {
	return &EvalTools{eng: eng, onEvent: onEvent}
}

func (t *EvalTools) Run() EvalRunTool       { return EvalRunTool{t: t} }
func (t *EvalTools) Result() EvalResultTool { return EvalResultTool{t: t} }
func (t *EvalTools) List() EvalListTool     { return EvalListTool{t: t} }

func (t *EvalTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *EvalTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// EvalRunTool judges a deliverable output against acceptance criteria and
// returns the stored evaluation record (rule → llm → manual dispatch, D-EVAL-3).
type EvalRunTool struct {
	t *EvalTools
}

func (EvalRunTool) Name() string { return ToolRunName }

func (EvalRunTool) Description() string {
	return "judge a deliverable output text against acceptance criteria and return the evaluation record (id, verdict, reason)"
}

func (EvalRunTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the subagent id or plan todo id being evaluated",
			},
			"output": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the deliverable output text to judge",
			},
			"criteria": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "minLength": 1},
				"description": "acceptance criteria; entries may carry a mode prefix (contains:/not:/llm:/manual:)",
			},
		},
		"required":             []string{"output", "criteria"},
		"additionalProperties": false,
	}
}

func (t EvalRunTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		TaskID   string   `json:"task_id"`
		Output   string   `json:"output"`
		Criteria []string `json:"criteria"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("eval_run: %w", err)
	}
	if strings.TrimSpace(a.Output) == "" {
		return "", fmt.Errorf("eval_run: empty output")
	}
	if len(a.Criteria) == 0 {
		return "", fmt.Errorf("eval_run: empty criteria")
	}
	rec, err := t.t.eng.Evaluate(ctx, a.TaskID, a.Output, a.Criteria)
	if err != nil {
		return "", fmt.Errorf("eval_run: %w", err)
	}
	if err := t.t.emitContext(ctx, session.EventEvalRun, session.NewEvalRun(rec.ID, rec.TaskID, string(rec.Verdict), rec.Reason, rec.EvaluatorKind, len(rec.Criteria))); err != nil {
		return "", fmt.Errorf("eval_run: persist event: %w", err)
	}
	return formatRecord(rec), nil
}

// EvalResultTool returns one stored evaluation record by id.
type EvalResultTool struct {
	t *EvalTools
}

func (EvalResultTool) Name() string { return ToolResultName }

func (EvalResultTool) Description() string {
	return "return one stored evaluation record by id (verdict, reason, criteria count)"
}

func (EvalResultTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the evaluation record id returned by eval_run",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t EvalResultTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("eval_result: %w", err)
	}
	rec, err := t.t.eng.Get(ctx, a.ID)
	if err != nil {
		return "", fmt.Errorf("eval_result: %w", err)
	}
	return formatRecord(rec), nil
}

// EvalListTool lists the evaluation history, most recent first.
type EvalListTool struct {
	t *EvalTools
}

func (EvalListTool) Name() string { return ToolListName }

func (EvalListTool) Description() string {
	return "list the evaluation history, most recent first"
}

func (EvalListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "max records (default 20)",
			},
		},
		"additionalProperties": false,
	}
}

func (t EvalListTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("eval_list: %w", err)
	}
	recs, err := t.t.eng.List(ctx)
	if err != nil {
		return "", fmt.Errorf("eval_list: %w", err)
	}
	if len(recs) == 0 {
		return "no evaluation records yet", nil
	}
	if a.Limit <= 0 {
		a.Limit = listDefaultLimit
	}
	return formatRecords(recs, a.Limit), nil
}

func formatRecord(rec EvalRecord) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "eval %s: %s (kind=%s)\n", rec.ID, rec.Verdict, rec.EvaluatorKind)
	if rec.TaskID != "" {
		fmt.Fprintf(&sb, "  task: %s\n", rec.TaskID)
	}
	if rec.Reason != "" {
		fmt.Fprintf(&sb, "  reason: %s\n", rec.Reason)
	}
	fmt.Fprintf(&sb, "  criteria: %d\n", len(rec.Criteria))
	return strings.TrimSuffix(sb.String(), "\n")
}

func formatRecords(recs []EvalRecord, limit int) string {
	if len(recs) == 0 {
		return "no evaluation records yet"
	}
	if limit <= 0 || limit > len(recs) {
		limit = len(recs)
	}
	var b strings.Builder
	for i, r := range recs[:limit] {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(formatRecord(r))
	}
	return b.String()
}
