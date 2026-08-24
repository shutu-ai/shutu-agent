// workflow.go — the GAP-3 composition-root orchestration
// (docs/dispatch-gap-3.md §6). This is where the task-DAG orchestration seam
// (ADR 2026-08-20-standard-gaps.md D-GAP-2, 用户拍板 JSON DAG 声明式编排) is
// wired into the REPL: registerWorkflow builds the workflow Engine over the
// subagent spawn capability and registers the workflow_run tool when
// workflow.enabled (默认关 D10). config.applyDefaults already whitelisted the
// name. The spawn closure drives a.subagents.Start("spawn", …) with the current
// session id (a.currentID — the same source the subagent wiring uses, read at
// spawn time so a /new or /resume switch is honored), awaits the run, and
// returns the child Output (D2 解耦: workflow 只依赖字符串闭包, 不依赖 subagent
// 类型). The loop's turn/step structure is untouched (D4): each task runs on the
// serial tool path (D5), blocked until the child settles.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/workflow"
	nodeworkflow "github.com/jabing/shutu-agent/internal/workflow/node"
)

// registerWorkflow wires the task-DAG orchestration seam (D-GAP-2) when
// workflow.enabled (默认关 D10): Engine over the subagent spawn capability +
// workflow_run tool. config.applyDefaults already whitelisted the name. The
// spawn closure drives a.subagents.Start("spawn", …) with the current session
// id (a.currentID, read at spawn time like the ralph/subagent wiring), awaits
// the run, and returns the child Output. It holds no closable resources, so
// there is no deferred Close.
func (a *app) registerWorkflow() error {
	if !config.Enabled(a.cfg.Workflow.Enabled) {
		return nil
	}
	spawn := func(ctx context.Context, prompt string) (string, error) {
		run, err := a.subagents.Start(ctx, "spawn", subagent.StartRequest{Prompt: prompt, ParentSessionID: a.currentID})
		if err != nil {
			return "", err
		}
		res, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	}
	eng, err := workflow.NewEngine(spawn, a.cfg.Workflow.MaxConcurrent)
	if err != nil {
		return err
	}
	parentID := a.currentID
	startAgent := func(ctx context.Context, req workflow.AgentRequest) (workflow.AgentResult, error) {
		provider := req.Provider
		if provider == "" {
			provider = a.cfg.Subagent.DefaultProvider
		}
		if provider == "" {
			provider = "spawn"
		}
		run, err := a.subagents.Start(ctx, provider, subagent.StartRequest{
			Label:           req.Label,
			Prompt:          req.Prompt,
			Model:           req.Model,
			OutputSchema:    req.Schema,
			ParentSessionID: parentID,
		})
		if err != nil {
			return workflow.AgentResult{}, err
		}
		res, err := run.Result(ctx)
		if err != nil {
			return workflow.AgentResult{}, err
		}
		return workflow.AgentResult{ID: run.ID, Output: res.Output, StopReason: res.StopReason}, nil
	}
	scriptRunner := nodeworkflow.New(nodeworkflow.Config{
		MaxConcurrent:   a.cfg.Workflow.MaxConcurrent,
		MaxTotalAgents:  a.cfg.Workflow.MaxTotalAgents,
		MaxItemsPerCall: a.cfg.Workflow.MaxItemsPerCall,
		SyncTimeoutMS:   a.cfg.Workflow.SyncTimeoutMS,
	})
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	if err := a.reg.Register(workflow.NewWorkflowRunToolWithScript(eng, scriptRunner, startAgent, func() string { return a.currentID }, onEvent)); err != nil {
		return fmt.Errorf("pa: register %s: %w", workflow.WorkflowRunToolName, err)
	}
	return nil
}
