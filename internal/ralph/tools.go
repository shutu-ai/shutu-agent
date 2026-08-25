// tools.go — the GAP-2 Consumer half of the ralph seam (ADR
// 2026-08-20-standard-gaps.md D-GAP-3, 对齐 dsh tool-ralph): the ralph tool is
// registered into the tools.Registry by the composition root (cmd/pa) when
// ralph.enabled, and auto-whitelisted by config.applyDefaults the same way the
// eval_*/fs_search tools are. It implements the tools.Tool method set
// structurally (Go structural typing), so this package never imports the tools
// package — the seam stays decoupled. D7 is enforced by the registry. D3 event
// logging lives here: a settled ralph run emits ralph/run.
package ralph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
)

// RalphToolName is the fresh-agent loop tool name (D-GAP-3).
const RalphToolName = "ralph"

// RalphTool bundles the ralph tool's shared state: the Engine it drives and the
// D3 event sink.
type RalphTool struct {
	eng     *Engine
	onEvent func(typ string, data any)
}

// NewRalphTool returns the ralph tool bound to an Engine. onEvent, when
// non-nil, receives the ralph/run payload; the composition root wires it to the
// session log (D3).
func NewRalphTool(eng *Engine, onEvent func(typ string, data any)) *RalphTool {
	return &RalphTool{eng: eng, onEvent: onEvent}
}

func (t *RalphTool) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (RalphTool) Name() string { return RalphToolName }

func (RalphTool) Description() string {
	return "对不可变目标运行多轮 fresh-agent 循环，返回最终报告（完成/阻塞/轮上限）"
}

func (RalphTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "immutable objective to drive the loop",
			},
			"max_rounds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "loop cap (default 256; deployment maximum 256)",
			},
		},
		"required":             []string{"objective"},
		"additionalProperties": false,
	}
}

// Execute drives the loop over the model-provided objective and returns the
// bounded final report. An empty objective is rejected; engine-level failures
// (spawn errors) are wrapped.
func (t *RalphTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Objective string `json:"objective"`
		MaxRounds int    `json:"max_rounds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("ralph: %w", err)
	}
	if strings.TrimSpace(a.Objective) == "" {
		return "", fmt.Errorf("ralph: empty objective")
	}
	rep, err := t.eng.Run(ctx, a.Objective, a.MaxRounds)
	if err != nil {
		return "", fmt.Errorf("ralph: %w", err)
	}
	t.emit(session.EventRalphRun, session.NewRalphRun(rep.Objective, rep.Rounds, rep.Done, rep.Blocked))
	return formatReport(rep), nil
}

// formatReport renders the bounded final report: the objective head, the rounds
// spent, the outcome (done | blocked | round-limit), the final deliverable /
// block reason, and one line per round brief.
func formatReport(rep Report) string {
	outcome := "round-limit"
	switch {
	case rep.Done:
		outcome = "done"
	case rep.Blocked:
		outcome = "blocked"
	}
	final := rep.Final
	if rep.Blocked {
		final = rep.BlockReason
	}
	if final == "" {
		final = "—"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "ralph: %s\n", boundRunes(rep.Objective, 80))
	fmt.Fprintf(&sb, "  rounds: %d/%d\n", rep.Rounds, rep.MaxRounds)
	fmt.Fprintf(&sb, "  outcome: %s\n", outcome)
	fmt.Fprintf(&sb, "  final: %s\n", final)
	sb.WriteString("  briefs:\n")
	for i, b := range rep.RoundBriefs {
		fmt.Fprintf(&sb, "    round %d: %s\n", i+1, b)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
