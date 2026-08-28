// ralph.go — the GAP-2 composition-root orchestration (docs/dispatch-gap-2.md
// §6). This is where the fresh-agent loop seam (ADR 2026-08-20-standard-gaps.md
// D-GAP-3, 对齐 dsh tool-ralph) is wired into the REPL: registerRalph builds
// the ralph Engine over the subagent spawn capability and registers the ralph
// tool when ralph.enabled (默认关 D10). config.applyDefaults already whitelisted
// the name. The spawn closure drives a.subagents.Start("spawn", …) with the
// current session id, awaits the run, and returns the child Output (D2 解耦:
// ralph 只依赖字符串闭包, 不依赖 subagent 类型). The loop's turn/step structure is
// untouched (D4): each round runs on the serial tool path (D5), blocking until
// the child settles.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/ralph"
	"github.com/jabing/shutu-agent/internal/subagent"
)

// registerRalph wires the fresh-agent loop seam (D-GAP-3) when ralph.enabled
// (默认关 D10): it builds the ralph Engine over the subagent spawn capability
// and registers the ralph tool. config.applyDefaults already whitelisted the
// name. The spawn closure drives a.subagents.Start("spawn", …) with the current
// session id (a.currentID — the same source the subagent wiring uses, read at
// spawn time so a /new or /resume switch is honored), awaits the run, and
// returns the child Output (D2 解耦: ralph 只依赖字符串闭包, 不依赖 subagent 类型).
func (a *app) registerRalph() error {
	if !config.Enabled(a.cfg.Ralph.Enabled) {
		return nil
	}
	spawn := func(ctx context.Context, prompt string) (string, error) {
		run, err := a.subagents.Start(ctx, "spawn", subagent.StartRequest{
			Prompt: prompt, ParentSessionID: a.currentID, OutputSchema: ralph.RoundReportSchema(),
		})
		if err != nil {
			return "", err
		}
		res, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		if res.Structured != nil {
			encoded, encodeErr := json.Marshal(res.Structured)
			if encodeErr != nil {
				return "", fmt.Errorf("encode structured Ralph report: %w", encodeErr)
			}
			return string(encoded), nil
		}
		return res.Output, nil
	}
	limit := a.cfg.Ralph.MaxRounds
	if limit <= 0 {
		limit = ralph.MaxRoundsLimit
	}
	eng, err := ralph.NewEngineWithLimit(spawn, limit)
	if err != nil {
		return err
	}
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	if err := a.reg.Register(ralph.NewRalphTool(eng, onEvent)); err != nil {
		return fmt.Errorf("pa: register %s: %w", ralph.RalphToolName, err)
	}
	return nil
}
