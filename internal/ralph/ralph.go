// Package ralph runs a bounded fresh-agent loop over an immutable objective.
// Each round starts a clean child and carries only one bounded structured
// report forward; the shared workspace is the durable memory.
package ralph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const DefaultMaxRounds = 256
const MaxRoundsLimit = 256

type Spawn func(ctx context.Context, prompt string) (string, error)

type Report struct {
	Objective    string
	MaxRounds    int
	Rounds       int
	Done         bool
	Blocked      bool
	BlockReason  string
	Final        string
	RoundBriefs  []string
	RoundReports []RoundReport
}

type RoundReport struct {
	Status    string   `json:"status"`
	Summary   string   `json:"summary"`
	Evidence  []string `json:"evidence"`
	NextSteps []string `json:"nextSteps"`
	Blocker   string   `json:"blocker"`
}

type Engine struct {
	spawn     Spawn
	maxRounds int
}

func NewEngine(spawn Spawn) (*Engine, error) {
	return NewEngineWithLimit(spawn, MaxRoundsLimit)
}

func NewEngineWithLimit(spawn Spawn, limit int) (*Engine, error) {
	if spawn == nil {
		return nil, fmt.Errorf("ralph: engine requires a spawn capability")
	}
	if limit <= 0 {
		limit = MaxRoundsLimit
	}
	return &Engine{spawn: spawn, maxRounds: limit}, nil
}

func (e *Engine) Run(ctx context.Context, objective string, maxRounds int) (Report, error) {
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	if e.maxRounds > 0 && maxRounds > e.maxRounds {
		return Report{}, fmt.Errorf("ralph: maxRounds %d exceeds deployment limit %d", maxRounds, e.maxRounds)
	}
	rep := Report{Objective: objective, MaxRounds: maxRounds}
	previous := ""
	for round := 1; round <= maxRounds; round++ {
		out, err := e.spawn(ctx, buildWorkerPrompt(objective, round, previous))
		if err != nil {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			return Report{}, err
		}
		report, err := parseWorkerReport(strings.TrimSpace(out))
		if err != nil {
			return Report{}, fmt.Errorf("ralph: worker round %d: %w", round, err)
		}
		rep.RoundReports = append(rep.RoundReports, report)
		rep.Rounds = round
		switch report.Status {
		case "complete":
			rep.Done = true
			rep.Final = boundRunes(report.Summary, 4000)
			rep.RoundBriefs = append(rep.RoundBriefs, rep.Final)
			return rep, nil
		case "blocked":
			rep.Blocked = true
			rep.BlockReason = boundRunes(report.Blocker, 2000)
			rep.RoundBriefs = append(rep.RoundBriefs, rep.BlockReason)
			return rep, nil
		case "continue":
			rep.RoundBriefs = append(rep.RoundBriefs, boundRunes(report.Summary, 4000))
			encoded, _ := json.Marshal(report)
			previous = string(encoded)
		}
	}
	return rep, nil
}

func parseWorkerReport(text string) (RoundReport, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil || raw == nil {
		return RoundReport{}, fmt.Errorf("worker must return one JSON object")
	}
	for key := range raw {
		switch key {
		case "status", "summary", "evidence", "nextSteps", "blocker":
		default:
			return RoundReport{}, fmt.Errorf("worker report contains unknown field %q", key)
		}
	}
	if len(raw) != 5 {
		return RoundReport{}, fmt.Errorf("worker report must contain status, summary, evidence, nextSteps, and blocker")
	}
	var report RoundReport
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		return RoundReport{}, fmt.Errorf("worker report is invalid: %w", err)
	}
	if report.Status != "continue" && report.Status != "complete" && report.Status != "blocked" {
		return RoundReport{}, fmt.Errorf("worker report status is invalid")
	}
	if strings.TrimSpace(report.Summary) == "" || report.Summary != strings.TrimSpace(report.Summary) {
		return RoundReport{}, fmt.Errorf("worker report summary must be non-empty and normalized")
	}
	if report.Evidence == nil || report.NextSteps == nil || !normalizedList(report.Evidence) || !normalizedList(report.NextSteps) {
		return RoundReport{}, fmt.Errorf("worker report evidence and nextSteps must be arrays of normalized strings")
	}
	if report.Blocker != strings.TrimSpace(report.Blocker) {
		return RoundReport{}, fmt.Errorf("worker report blocker must be normalized")
	}
	switch report.Status {
	case "continue":
		if len(report.NextSteps) == 0 || report.Blocker != "" {
			return RoundReport{}, fmt.Errorf("continue report needs nextSteps and an empty blocker")
		}
	case "complete":
		if len(report.Evidence) == 0 || len(report.NextSteps) != 0 || report.Blocker != "" {
			return RoundReport{}, fmt.Errorf("complete report needs evidence, no nextSteps, and an empty blocker")
		}
	case "blocked":
		if report.Blocker == "" {
			return RoundReport{}, fmt.Errorf("blocked report needs a concrete blocker")
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil || len([]rune(string(encoded))) > 16384 {
		return RoundReport{}, fmt.Errorf("worker report exceeds 16384 characters")
	}
	return report, nil
}

func normalizedList(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func buildWorkerPrompt(objective string, round int, previous string) string {
	if strings.TrimSpace(previous) == "" {
		previous = "(none — this is the first round)"
	}
	return fmt.Sprintf(`You are one fresh worker in a foreground Ralph loop. Return exactly one JSON object with this schema: {"status":"continue|complete|blocked","summary":"...","evidence":["..."],"nextSteps":["..."],"blocker":"..."}. All five fields are required and strings must be trimmed. For continue, summary and nextSteps must be non-empty and blocker empty. For complete, summary and evidence must be non-empty, nextSteps and blocker empty. For blocked, summary and blocker must be non-empty. Do not return prose or legacy DONE/BLOCKED markers.

Immutable objective:
%s

Ralph round: %d.
Previous structured handoff:
%s

The shared workspace is durable memory. Inspect it, perform concrete in-scope work, and verify changes. You have no parent conversation or prior child session.`, objective, round, previous)
}

func boundRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
