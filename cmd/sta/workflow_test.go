// workflow_test.go — the GAP-3 wiring tests (docs/dispatch-gap-3.md §6):
// registerWorkflow D10 gate + tool registration + whitelist, the workflow E2E
// through the subagent provider with a scripted LLM, and the cycle
// rejection. The fakes mirror the ralph_test/subagent_test pattern:
// makeWorkflowApp builds a minimal app, the whitelist policy lets the registry
// Execute the workflow_run tool, and ralphFakeLLM (defined in ralph_test.go,
// same package) is the scripted child answerer.
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/workflow"
)

// makeWorkflowApp builds a minimal app for registerWorkflow tests: only the
// fields registerWorkflow and the registerSubagent it depends on touch
// (cfg.Workflow, cfg.Subagent, cfg.Model, reg, log, llm, prompt, currentID)
// are set. The subagent config is always enabled so registerSubagent can build
// the Runtime registerWorkflow spawns through.
func makeWorkflowApp(enabled bool, fakeLLM llm.LLM) *app {
	return &app{
		cfg: config.Config{
			Model:    "m",
			Subagent: config.SubagentConfig{Enabled: config.Bool(true), MaxDepth: 8, DefaultProvider: "spawn"},
			Workflow: config.WorkflowConfig{Enabled: config.Bool(enabled), MaxConcurrent: 4},
		},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-test",
		llm:       fakeLLM,
		prompt:    prompt.New("You are a subagent."),
	}
}

// workflowPolicy whitelists the workflow tool so registry Execute can run
// it (in production config.applyDefaults + PolicyFromConfig do this).
func workflowPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"workflow"},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

// TestRegisterWorkflowDisabledRegistersNothing verifies the D10 gate: with
// workflow.enabled=false the composition root registers no workflow tool
// (dispatch-gap-3 §6).
func TestRegisterWorkflowDisabledRegistersNothing(t *testing.T) {
	app := makeWorkflowApp(false, nil)
	if err := app.registerWorkflow(); err != nil {
		t.Fatalf("registerWorkflow: %v", err)
	}
	for _, spec := range app.reg.Specs() {
		if spec.Name == workflow.WorkflowRunToolName {
			t.Fatalf("workflow_run tool %q registered while workflow disabled", spec.Name)
		}
	}
}

// TestRegisterWorkflowEnabled verifies the enabled path: the workflow_run tool
// is registered and whitelisted (Execute succeeds through the registry on a
// single JavaScript workflow).
func TestRegisterWorkflowEnabled(t *testing.T) {
	app := makeWorkflowApp(true, ralphFakeLLM{text: "任务A 完成"})
	app.reg.SetPolicy(workflowPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerWorkflow(); err != nil {
		t.Fatalf("registerWorkflow: %v", err)
	}
	names := make([]string, 0, len(app.reg.Specs()))
	for _, s := range app.reg.Specs() {
		names = append(names, s.Name)
	}
	if !containsStr(names, workflow.WorkflowRunToolName) {
		t.Fatalf("registered tools %v lack %q", names, workflow.WorkflowRunToolName)
	}
	// Whitelist: Execute succeeds only when workflow is both registered and
	// whitelisted (the D10-gated whitelist in production comes from
	// config.applyDefaults).
	if _, err := app.reg.Execute(context.Background(), workflow.WorkflowRunToolName,
		json.RawMessage(`{"meta":{"name":"smoke","description":"smoke"},"script":"return 42"}`)); err != nil {
		t.Fatalf("workflow must be registered and whitelisted when enabled: %v", err)
	}
}

// TestWorkflowRunE2E drives a JavaScript workflow with an agent call through
// the registry and verifies the structured DSH envelope.
func TestWorkflowRunE2E(t *testing.T) {
	app := makeWorkflowApp(true, ralphFakeLLM{text: "子代理输出"})
	app.reg.SetPolicy(workflowPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerWorkflow(); err != nil {
		t.Fatalf("registerWorkflow: %v", err)
	}
	res, err := app.reg.Execute(context.Background(), workflow.WorkflowRunToolName,
		json.RawMessage(`{"meta":{"name":"e2e-flow","description":"test"},"script":"return await agent(\"x\")"}`))
	if err != nil {
		t.Fatalf("workflow via registry: %v", err)
	}
	if !strings.Contains(res.Output, `workflow "e2e-flow" completed`) ||
		!strings.Contains(res.Output, `Return value:`) {
		t.Fatalf("workflow output = %q, want DSH workflow envelope", res.Output)
	}
	envelope, ok := res.Value.(map[string]any)
	if !ok || envelope["runId"] == "" || envelope["agentsStarted"] != 1 || envelope["result"] != "子代理输出" {
		t.Fatalf("workflow value = %#v, want runId/agentsStarted/result envelope", res.Value)
	}
	if !hasEvent(app.log, session.EventWorkflowRun) {
		t.Fatal("workflow/run event missing from the session log after workflow Execute")
	}
}

// TestWorkflowRunScriptError submits a workflow with an unsupported hook
// option; the DSH runner fails the whole script instead of returning partial
// output.
func TestWorkflowRunScriptError(t *testing.T) {
	app := makeWorkflowApp(true, ralphFakeLLM{text: "子代理输出"})
	app.reg.SetPolicy(workflowPolicy())
	if err := app.registerSubagent(); err != nil {
		t.Fatalf("registerSubagent: %v", err)
	}
	defer app.subagents.Close()
	if err := app.registerWorkflow(); err != nil {
		t.Fatalf("registerWorkflow: %v", err)
	}
	res, err := app.reg.Execute(context.Background(), workflow.WorkflowRunToolName,
		json.RawMessage(`{"meta":{"name":"bad-flow","description":"test"},"script":"return await agent(\"x\", {effort: \"high\"})"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Output, "workflow run failed") {
		t.Fatalf("invalid workflow result = %+v, err=%v, want structured workflow error", res, err)
	}
}
