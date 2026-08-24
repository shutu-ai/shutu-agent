package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/workflow"
)

func TestRunnerExecutesDynamicWorkflowAndAgentRPC(t *testing.T) {
	runner := New(Config{Command: "node", MaxConcurrent: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var events []workflow.ScriptEvent
	result, err := runner.RunScript(ctx, workflow.ScriptRequest{
		Meta: map[string]any{"name": "test-flow", "description": "test"},
		Args: map[string]any{"topic": "go"},
		Script: `phase("research")
const values = await parallel([() => agent("one"), () => agent("two")])
return {topic: args.topic, values, final: await agent("three")}`,
	}, func(_ context.Context, req workflow.AgentRequest) (workflow.AgentResult, error) {
		return workflow.AgentResult{ID: "child-" + req.Prompt, Output: "out-" + req.Prompt, StopReason: "completed"}, nil
	}, func(ev workflow.ScriptEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if result.StopReason != "completed" || result.AgentsStarted != 3 {
		t.Fatalf("result = %+v, want completed with three agents", result)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["topic"] != "go" || value["final"] != "out-three" {
		t.Fatalf("value = %#v, want args and final output", result.Value)
	}
	var names []string
	for _, ev := range events {
		names = append(names, ev.Type)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"workflow/start", "workflow/phase", "workflow/agent-start", "workflow/agent-end", "workflow/end"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events = %v, missing %q", names, want)
		}
	}
}

func TestRunnerReturnsStructuredAgentResult(t *testing.T) {
	runner := New(Config{Command: "node"})
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "structured", "description": "structured"},
		Script: `return await agent("report", {schema: {type: "object", properties: {answer: {type: "string"}}, required: ["answer"], additionalProperties: false}})`,
	}, func(_ context.Context, req workflow.AgentRequest) (workflow.AgentResult, error) {
		if req.Schema == nil || req.Schema["type"] != "object" {
			t.Fatalf("agent schema = %#v, want object schema", req.Schema)
		}
		return workflow.AgentResult{ID: "child-structured", StopReason: "completed", Structured: map[string]any{"answer": req.Prompt}}, nil
	}, nil)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["answer"] != "report" {
		t.Fatalf("structured result = %#v, want captured object", result.Value)
	}
}

func TestRunnerCancellationStopsNodeProcess(t *testing.T) {
	runner := New(Config{Command: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.RunScript(ctx, workflow.ScriptRequest{Meta: map[string]any{"name": "cancel", "description": "cancel"}, Script: `await new Promise(() => {})`}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
			return workflow.AgentResult{}, nil
		}, nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("cancel error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunScript did not settle after cancellation")
	}
}
