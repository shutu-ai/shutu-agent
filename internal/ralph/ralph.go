// Package ralph runs a bounded fresh-agent loop over an immutable objective
// (D-GAP-3, 对齐 dsh tool-ralph). Each round spawns a brand-new subagent with
// no session inheritance; only a bounded structured report crosses rounds, and
// the shared workspace (disk/project files) is the long-term memory. A worker
// reports completion, a concrete blocker, or round-limit exhaustion. The engine
// only orchestrates — the actual work happens inside the spawned subagents.
package ralph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMaxRounds is the loop cap when max_rounds is absent (D-GAP-3).
const DefaultMaxRounds = 256

// MaxRoundsLimit is the deployment safety ceiling used by the dsh tool.
const MaxRoundsLimit = 256

// Round protocol markers (D-GAP-3): a worker's final reply starting with
// "DONE: " means the objective is met; "BLOCKED: " means an impassable
// concrete blocker; otherwise the reply is treated as a progress report and
// the loop continues.
const (
	MarkerDone    = "DONE: "
	MarkerBlocked = "BLOCKED: "
)

// Spawn is the subagent-launch capability the engine depends on (D2: 组合根注
// 入闭包). It starts one fresh subagent with the given prompt and returns its
// terminal output text. Prompts and outputs are plain strings; the engine never
// sees subagent internals.
type Spawn func(ctx context.Context, prompt string) (string, error)

// Report is the bounded result of one loop run (D-GAP-3: 只携带摘要, 不携带
// 全量对话).
type Report struct {
	Objective    string
	MaxRounds    int
	Rounds       int      // rounds actually spawned
	Done         bool     // a worker reported DONE
	Blocked      bool     // a worker reported BLOCKED
	BlockReason  string   // set when Blocked
	Final        string   // final deliverable (when Done)
	RoundBriefs  []string // one bounded brief per round (progress or final)
	RoundReports []RoundReport
}

// RoundReport is the restricted handoff crossing one fresh-agent boundary.
type RoundReport struct {
	Status    string   `json:"status"`
	Summary   string   `json:"summary,omitempty"`
	Result    string   `json:"result,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
	NextSteps []string `json:"nextSteps,omitempty"`
	Blocker   string   `json:"blocker,omitempty"`
	Handoff   []string `json:"handoff,omitempty"` // legacy compatibility
}

// Engine runs the loop.
type Engine struct {
	spawn     Spawn
	maxRounds int
}

// NewEngine returns an engine bound to a spawn capability. A nil spawn is
// rejected at use time (NewEngine errors).
func NewEngine(spawn Spawn) (*Engine, error) {
	return NewEngineWithLimit(spawn, MaxRoundsLimit)
}

// NewEngineWithLimit binds a deployment-owned round ceiling. Model-provided
// max_rounds values above it are rejected instead of silently weakening the
// host policy.
func NewEngineWithLimit(spawn Spawn, limit int) (*Engine, error) {
	if spawn == nil {
		return nil, fmt.Errorf("ralph: engine requires a spawn capability")
	}
	if limit <= 0 {
		limit = MaxRoundsLimit
	}
	return &Engine{spawn: spawn, maxRounds: limit}, nil
}

// Run executes the fresh-agent loop up to maxRounds (<=0 → DefaultMaxRounds).
// It returns an error only for engine-level failures (spawn capability errors
// that are not worker reports); worker BLOCKED/DONE are normal outcomes.
func (e *Engine) Run(ctx context.Context, objective string, maxRounds int) (Report, error) {
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	if e.maxRounds > 0 && maxRounds > e.maxRounds {
		return Report{}, fmt.Errorf("ralph: max_rounds %d exceeds deployment limit %d", maxRounds, e.maxRounds)
	}
	rep := Report{Objective: objective, MaxRounds: maxRounds}
	prevBrief := ""
	for round := 1; round <= maxRounds; round++ {
		out, err := e.spawn(ctx, buildWorkerPrompt(objective, round, prevBrief))
		if err != nil {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			return Report{}, err
		}
		trimmed := strings.TrimSpace(out)
		if structured, ok := parseWorkerReport(trimmed); ok {
			rep.RoundReports = append(rep.RoundReports, structured)
			switch structured.Status {
			case "complete":
				rep.Rounds, rep.Done = round, true
				rep.Final = boundRunes(firstNonEmpty(structured.Result, structured.Summary), 4000)
				rep.RoundBriefs = append(rep.RoundBriefs, rep.Final)
				return rep, nil
			case "blocked":
				rep.Rounds, rep.Blocked = round, true
				rep.BlockReason = boundRunes(firstNonEmpty(structured.Blocker, structured.Summary), 2000)
				rep.RoundBriefs = append(rep.RoundBriefs, rep.BlockReason)
				return rep, nil
			case "continue":
				brief := boundRunes(firstNonEmpty(structured.Summary, structured.Result), 4000)
				rep.Rounds = round
				rep.RoundBriefs = append(rep.RoundBriefs, brief)
				prevBrief = brief
				continue
			default:
				rep.RoundReports = rep.RoundReports[:len(rep.RoundReports)-1]
			}
		}
		switch {
		case strings.HasPrefix(trimmed, MarkerDone):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, MarkerDone))
			rep.Rounds = round
			rep.Done = true
			rep.Final = boundRunes(rest, 4000)
			rep.RoundBriefs = append(rep.RoundBriefs, rep.Final)
			return rep, nil
		case strings.HasPrefix(trimmed, MarkerBlocked):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, MarkerBlocked))
			rep.Rounds = round
			rep.Blocked = true
			rep.BlockReason = boundRunes(rest, 2000)
			rep.RoundBriefs = append(rep.RoundBriefs, rep.BlockReason)
			return rep, nil
		default:
			brief := boundRunes(trimmed, 4000)
			rep.Rounds = round
			rep.RoundBriefs = append(rep.RoundBriefs, brief)
			prevBrief = brief
		}
	}
	return rep, nil
}

func parseWorkerReport(text string) (RoundReport, bool) {
	if !strings.HasPrefix(text, "{") {
		return RoundReport{}, false
	}
	var report RoundReport
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		return RoundReport{}, false
	}
	if report.Status != "continue" && report.Status != "complete" && report.Status != "blocked" {
		return RoundReport{}, false
	}
	// Keep the original summary/result/handoff envelope readable for old
	// workers. When any dsh-compatible field is present, validate its stricter
	// status-specific contract instead of silently accepting malformed data.
	if report.Evidence == nil && report.NextSteps == nil && report.Blocker == "" {
		return report, true
	}
	if strings.TrimSpace(report.Summary) == "" || report.Summary != strings.TrimSpace(report.Summary) ||
		!normalizedList(report.Evidence) || !normalizedList(report.NextSteps) || report.Blocker != strings.TrimSpace(report.Blocker) {
		return RoundReport{}, false
	}
	switch report.Status {
	case "continue":
		if len(report.NextSteps) == 0 || report.Blocker != "" {
			return RoundReport{}, false
		}
	case "complete":
		if len(report.Evidence) == 0 || len(report.NextSteps) != 0 || report.Blocker != "" {
			return RoundReport{}, false
		}
	case "blocked":
		if report.Blocker == "" {
			return RoundReport{}, false
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil || len([]rune(string(encoded))) > 16384 {
		return RoundReport{}, false
	}
	return report, true
}

func normalizedList(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// buildWorkerPrompt renders the round prompt: the immutable objective + the
// previous round's bounded brief (or "（无）" for round one). Fresh agents share
// no session, so only this text carries history across rounds; the workspace on
// disk is the shared long-term memory.
func buildWorkerPrompt(objective string, round int, prevBrief string) string {
	prev := strings.TrimSpace(prevBrief)
	if prev == "" {
		prev = "（无）"
	}
	instruction := `When possible, finish with one JSON object using this dsh-compatible schema: {"status":"continue|complete|blocked","summary":"...","evidence":["..."],"nextSteps":["..."],"blocker":"..."}. For continue, summary and nextSteps must be non-empty and blocker empty. For complete, summary and evidence must be non-empty, nextSteps and blocker empty. For blocked, summary and blocker must be non-empty. Legacy DONE:/BLOCKED: replies remain accepted.`
	return instruction + "\n\n" + fmt.Sprintf(`你是数驼 AI Agent 的 fresh 工作代理。目标（不可变）：
%s

这是第 %d 轮。上一轮进展摘要（第一轮为「无」）：
%s

你与前几轮没有共享会话——你只能看到上面的目标与上一轮摘要。工作区（磁盘/项目
文件）是跨轮共享的长期记忆，请基于它的当前状态继续推进。

最终回复的判定规则：
- 若你已达成目标：第一行以 "DONE: " 开头，随后给出最终交付摘要。
- 若你遇到无法逾越的具体阻塞（缺凭证、需要人工、外部依赖不可用等）：第一行以
  "BLOCKED: " 开头，随后给出原因。
- 否则：给出本轮进展报告（做了什么、发现什么、下一步），继续推进。
`, objective, round, prev)
}

// boundRunes truncates s to at most max runes (append "…" when cut).
func boundRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}
