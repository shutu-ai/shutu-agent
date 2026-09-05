package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func makeExitPlanApp() *app {
	a := &app{
		cfg:       config.Config{Plan: config.PlanConfig{Enabled: config.Bool(true)}},
		currentID: "plan-session",
		log:       session.New(),
		reg:       tools.New(),
		interacts: interact.NewEngine(nil),
	}
	return a
}

func waitForInteraction(t *testing.T, e interact.Engine) interact.Request {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, err := e.List(context.Background())
		if err == nil && len(items) == 1 {
			return items[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for interaction request")
	return interact.Request{}
}

func TestExitPlanModeUsesDSHSchemaAndLeavesPlanModeAfterApproval(t *testing.T) {
	a := makeExitPlanApp()
	defer a.interacts.Close()
	if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatalf("activate plan mode: %v", err)
	}
	tool := exitPlanModeTool{app: a}
	schema := tool.Schema()
	if schema["type"] != "object" || schema["additionalProperties"] != nil {
		t.Fatalf("exit_plan_mode schema = %#v, want DSH argument shape", schema)
	}
	if _, ok := schema["properties"].(map[string]any)["plan"]; !ok {
		t.Fatal("exit_plan_mode schema lacks plan")
	}

	result := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		output, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":"# Release\n\n1. Test\n2. Ship"}`))
		result <- struct {
			output string
			err    error
		}{output, err}
	}()
	req := waitForInteraction(t, a.interacts)
	if req.ToolName != exitPlanModeName || len(req.Questions) != 1 || req.Questions[0].Detail == "" {
		t.Fatalf("plan review request = %+v, want DSH review question with plan detail", req)
	}
	answer := `{"answers":[{"id":"plan-review","selected":["Approve"]}]}`
	if _, err := a.interacts.(interact.AnswerResolver).ResolveWithAnswer(context.Background(), req.ID, interact.StatusApproved, answer); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	got := <-result
	if got.err != nil || got.output != `{"approved":true}` {
		t.Fatalf("exit_plan_mode result = %q, err=%v, want approved object", got.output, got.err)
	}
	if session.FoldPlanMode(a.log.Events()) {
		t.Fatal("approved exit_plan_mode must leave plan mode")
	}
}

func TestExitPlanModeRegistryPreservesStructuredOutput(t *testing.T) {
	a := makeExitPlanApp()
	defer a.interacts.Close()
	if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatalf("activate plan mode: %v", err)
	}
	a.reg.SetPolicy(tools.Policy{Enabled: []string{exitPlanModeName}})
	if err := a.reg.Register(exitPlanModeTool{app: a}); err != nil {
		t.Fatalf("register exit_plan_mode: %v", err)
	}
	result := make(chan tools.ToolResult, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := a.reg.Execute(context.Background(), exitPlanModeName, json.RawMessage(`{"plan":"# Release"}`))
		result <- got
		errs <- err
	}()
	req := waitForInteraction(t, a.interacts)
	if _, err := a.interacts.(interact.AnswerResolver).ResolveWithAnswer(context.Background(), req.ID, interact.StatusApproved, `{"answers":[{"id":"plan-review","selected":["Approve"]}]}`); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("registry exit_plan_mode: %v", err)
	}
	got := <-result
	if got.IsError {
		t.Fatalf("registry exit_plan_mode returned error result: %+v", got)
	}
	value, ok := got.Value.(map[string]any)
	if !ok || value["approved"] != true {
		t.Fatalf("registry exit_plan_mode value = %#v, want approved object", got.Value)
	}
}

func TestExitPlanModeRejectsInactiveOrMalformedPlans(t *testing.T) {
	a := makeExitPlanApp()
	defer a.interacts.Close()
	tool := exitPlanModeTool{app: a}
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"plan":"# Plan"}`, "only available in plan mode"},
		{`{"plan":"not markdown"}`, "only available in plan mode"},
	} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("inactive exit_plan_mode(%s) = %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatalf("activate plan mode: %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":"not markdown"}`)); err == nil || !strings.Contains(err.Error(), "starting with a # heading") {
		t.Fatalf("malformed plan error = %v", err)
	}
}

func TestExitPlanModeKeepPlanningReturnsFeedback(t *testing.T) {
	a := makeExitPlanApp()
	defer a.interacts.Close()
	if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
		t.Fatalf("activate plan mode: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := (exitPlanModeTool{app: a}).Execute(context.Background(), json.RawMessage(`{"plan":"# Plan"}`))
		result <- err
	}()
	req := waitForInteraction(t, a.interacts)
	answer := `{"answers":[{"id":"plan-review","selected":["Keep planning"],"custom":"Add rollback steps"}]}`
	if _, err := a.interacts.(interact.AnswerResolver).ResolveWithAnswer(context.Background(), req.ID, interact.StatusApproved, answer); err != nil {
		t.Fatalf("keep planning: %v", err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "Add rollback steps") {
		t.Fatalf("keep-planning result = %v, want feedback", err)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("keep planning must remain in plan mode")
	}
}
