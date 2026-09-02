package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

// eventCapture records one emitted event's type and payload.
type eventCapture struct {
	typ  string
	data any
}

type fakeScriptRunner struct {
	request ScriptRequest
}

// TestWorkflowCancellationClassification pins the synchronous workflow tool's
// context forwarding; execution-level cancellation is covered by the engine.
func TestWorkflowCancellationClassification(t *testing.T) {
	tool := NewWorkflowRunToolWithScript(mustEngine(t, &fakeSpawn{}, 0), &fakeScriptRunner{}, nil, func() string { return "parent-1" }, nil)
	classified, ok := any(tool).(interface{ CancellationAware() bool })
	if !ok || !classified.CancellationAware() {
		t.Fatal("workflow must classify cooperative cancellation")
	}
}

func (f *fakeScriptRunner) RunScript(_ context.Context, req ScriptRequest, _ AgentStart, emit func(ScriptEvent)) (ScriptResult, error) {
	f.request = req
	if emit != nil {
		emit(ScriptEvent{Type: session.EventWorkflowStart, Data: map[string]any{"name": req.Meta["name"]}})
	}
	return ScriptResult{Value: map[string]any{"ok": true}, StopReason: "completed", AgentsStarted: 1}, nil
}

type failFastScriptRunner struct {
	agentCalls int
	sawCancel  bool
}

func (f *failFastScriptRunner) RunScript(ctx context.Context, _ ScriptRequest, agent AgentStart, emit func(ScriptEvent)) (ScriptResult, error) {
	emit(ScriptEvent{Type: session.EventWorkflowStart, Data: map[string]any{"name": "fail-fast"}})
	if ctx.Err() != nil {
		f.sawCancel = true
		return ScriptResult{RunID: "fail-fast", StopReason: "cancelled", Error: ctx.Err().Error()}, nil
	}
	// A worker must not get another external admission after a failed receipt.
	_, _ = agent(ctx, AgentRequest{Prompt: "must not start"})
	f.agentCalls++
	return ScriptResult{RunID: "fail-fast", StopReason: "completed", AgentsStarted: 1}, nil
}

type recordScriptRunner struct{}

func (recordScriptRunner) RunScript(_ context.Context, _ ScriptRequest, _ AgentStart, emit func(ScriptEvent)) (ScriptResult, error) {
	meta := map[string]any{"name": "recorded"}
	emit(ScriptEvent{Type: session.EventWorkflowStart, Data: map[string]any{"run_id": "run-1", "meta": meta}})
	emit(ScriptEvent{Type: session.EventWorkflowAgentStart, Data: map[string]any{
		"run_id": "run-1", "meta": meta, "seq": 1, "label": "member", "phase": "audit", "child_id": "child-1",
	}})
	emit(ScriptEvent{Type: session.EventWorkflowAgentEnd, Data: map[string]any{
		"run_id": "run-1", "meta": meta, "seq": 1, "label": "member", "phase": "audit", "child_id": "child-1",
		"outcome": "completed",
	}})
	emit(ScriptEvent{Type: session.EventWorkflowEnd, Data: map[string]any{
		"run_id": "run-1", "meta": meta, "stop_reason": "completed", "agents_started": 1,
	}})
	return ScriptResult{RunID: "run-1", StopReason: "completed", AgentsStarted: 1}, nil
}

func TestWorkflowToolSchemaMatchesDSHWorkflowContract(t *testing.T) {
	description := WorkflowRunTool{}.Description()
	if !strings.Contains(description, "JavaScript workflow") || !strings.Contains(description, "pipeline") || !strings.Contains(description, "parallel") {
		t.Fatal("workflow description must document the DSH JavaScript workflow hooks")
	}
	schema := WorkflowRunTool{}.Schema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("workflow schema properties missing")
	}
	for _, name := range []string{"meta", "args", "script"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("workflow schema missing %q", name)
		}
	}
	if _, ok := properties["tasks"]; ok {
		t.Fatal("workflow model-facing schema must not expose the legacy tasks DAG")
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 2 || required[0] != "meta" || required[1] != "script" {
		t.Fatalf("required = %#v, want meta and script", schema["required"])
	}
}

func TestWorkflowToolExecuteScriptPathValidatesMetaAndForwardsEvents(t *testing.T) {
	fs := &fakeSpawn{}
	eng := mustEngine(t, fs, 0)
	runner := &fakeScriptRunner{}
	var events []eventCapture
	tool := NewWorkflowRunToolWithScript(eng, runner, nil, func() string { return "parent-1" }, func(typ string, data any) {
		events = append(events, eventCapture{typ: typ, data: data})
	})
	tool.agent = func(context.Context, AgentRequest) (AgentResult, error) { return AgentResult{}, nil }
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"meta":{"name":"audit","description":"check files"},"args":{"scope":"repo"},"script":"return 1"}`))
	if err != nil {
		t.Fatalf("Execute script: %v", err)
	}
	if !strings.Contains(out, `"ok": true`) || !strings.Contains(out, `workflow "audit" completed`) {
		t.Fatalf("script result = %q, want JSON result", out)
	}
	if runner.request.ParentSessionID != "parent-1" || runner.request.Script != "return 1" {
		t.Fatalf("request = %+v, want parent and script forwarded", runner.request)
	}
	if len(events) != 2 || events[0].typ != session.EventWorkflowStart || events[1].typ != session.EventWorkflowRun {
		t.Fatalf("events = %+v, want workflow/start then workflow/run", events)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"script":"return 1"}`)); err == nil || !strings.Contains(err.Error(), "meta is required") {
		t.Fatalf("missing meta error = %v, want meta validation", err)
	}
}

func TestWorkflowToolRejectsUnknownAndMalformedMetaFields(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{}, 0)
	runner := &fakeScriptRunner{}
	tool := NewWorkflowRunToolWithScript(eng, runner, func(context.Context, AgentRequest) (AgentResult, error) {
		return AgentResult{}, nil
	}, nil, nil)
	for _, raw := range []string{
		`{"meta":{"name":"x","description":"y","unknown":true},"script":"return 1"}`,
		`{"meta":{"name":"x","description":"y","whenToUse":1},"script":"return 1"}`,
		`{"meta":{"name":"x","description":"y","phases":[{"title":"p","provider":1}]},"script":"return 1"}`,
		`{"meta":{"name":"x","description":"y","phases":[{"title":"p","extra":true}]},"script":"return 1"}`,
	} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("Execute(%s) accepted malformed meta", raw)
		}
	}
}

func TestWorkflowToolDurableEventFailureIsReturned(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{}, 0)
	runner := &fakeScriptRunner{}
	tool := NewWorkflowRunToolWithScript(eng, runner, func(context.Context, AgentRequest) (AgentResult, error) {
		return AgentResult{}, nil
	}, nil, nil)
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "session-1",
		Emit:      func(string, any) error { return errors.New("durable sink unavailable") },
	})
	_, err := tool.Execute(ctx, json.RawMessage(`{"meta":{"name":"audit","description":"check"},"script":"return 1"}`))
	if err == nil || !strings.Contains(err.Error(), "persist event") {
		t.Fatalf("workflow durable event error = %v, want persist event failure", err)
	}
}

func TestWorkflowContextEventSinkFailureIsReturnedWithoutRuntime(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{}, 0)
	runner := &fakeScriptRunner{}
	tool := NewWorkflowRunToolWithScript(eng, runner, func(context.Context, AgentRequest) (AgentResult, error) {
		return AgentResult{}, nil
	}, nil, nil)
	var emitted []string
	tool.SetEmitContext(func(_ context.Context, typ string, _ any) error {
		emitted = append(emitted, typ)
		return errors.New("durable sink unavailable")
	})
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"meta":{"name":"audit","description":"check"},"script":"return 1"}`))
	if err == nil || !strings.Contains(err.Error(), "persist event") {
		t.Fatalf("workflow context sink error = %v, want persist event failure", err)
	}
	if len(emitted) != 1 || emitted[0] != session.EventWorkflowStart {
		t.Fatalf("emitted events = %v, want failure at the first workflow event", emitted)
	}
}

func TestWorkflowReceiptFailureStopsExternalAgentAdmission(t *testing.T) {
	runner := &failFastScriptRunner{}
	tool := NewWorkflowRunToolWithScript(nil, runner, func(ctx context.Context, _ AgentRequest) (AgentResult, error) {
		if err := ctx.Err(); err != nil {
			return AgentResult{}, err
		}
		return AgentResult{ID: "must-not-exist", StopReason: "completed"}, nil
	}, nil, nil)
	tool.SetEmitContext(func(context.Context, string, any) error {
		return errors.New("durable sink unavailable")
	})
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"meta":{"name":"fail-fast","description":"check"},"script":"return 1"}`))
	if err == nil || !strings.Contains(err.Error(), "persist event") {
		t.Fatalf("receipt failure error = %v, want persist event failure", err)
	}
	if runner.agentCalls != 0 || !runner.sawCancel {
		t.Fatalf("runner agentCalls=%d sawCancel=%v, want cancellation before external admission", runner.agentCalls, runner.sawCancel)
	}
}

func TestWorkflowToolProjectsReferenceDurableRecords(t *testing.T) {
	runner := recordScriptRunner{}
	tool := NewWorkflowRunToolWithScript(nil, runner, func(context.Context, AgentRequest) (AgentResult, error) {
		return AgentResult{}, nil
	}, func() string { return "parent-1" }, nil)
	var events []eventCapture
	tool.SetEmitContext(func(_ context.Context, typ string, data any) error {
		events = append(events, eventCapture{typ: typ, data: data})
		return nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"meta":{"name":"recorded","description":"check"},"script":"return 1"}`)); err != nil {
		t.Fatalf("workflow execute: %v", err)
	}
	var records []eventCapture
	for _, event := range events {
		switch event.typ {
		case session.EventToolWorkflowRunStart, session.EventToolWorkflowAgentStart,
			session.EventToolWorkflowAgentEnd, session.EventToolWorkflowRunEnd:
			records = append(records, event)
		}
	}
	wantTypes := []string{
		session.EventToolWorkflowRunStart, session.EventToolWorkflowAgentStart,
		session.EventToolWorkflowAgentEnd, session.EventToolWorkflowRunEnd,
	}
	if len(records) != len(wantTypes) {
		t.Fatalf("records = %#v, want %d events", records, len(wantTypes))
	}
	for index, record := range records {
		if record.typ != wantTypes[index] {
			t.Fatalf("record %d = %s, want %s", index, record.typ, wantTypes[index])
		}
		value := record.data.(map[string]any)
		if value["runId"] != "run-1" {
			t.Fatalf("record %#v has wrong run identity", record)
		}
	}
	if got := records[1].data.(map[string]any)["childId"]; got != "child-1" {
		t.Fatalf("agent start childId = %#v, want child-1", got)
	}
	if got := records[3].data.(map[string]any)["stopReason"]; got != "completed" {
		t.Fatalf("run end stopReason = %#v, want completed", got)
	}
}

func TestWorkflowRecorderFailureDoesNotAffectToolResult(t *testing.T) {
	runner := recordScriptRunner{}
	tool := NewWorkflowRunToolWithScript(nil, runner, func(context.Context, AgentRequest) (AgentResult, error) {
		return AgentResult{}, nil
	}, func() string { return "parent-1" }, nil)
	var recordFailures int
	var runtimeEvents int
	tool.SetEmitContext(func(_ context.Context, typ string, _ any) error {
		switch {
		case typ == session.EventToolWorkflowRunStart:
			recordFailures++
			return errors.New("record journal unavailable")
		case !strings.HasPrefix(typ, "tool-workflow/"):
			runtimeEvents++
		}
		return nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"meta":{"name":"recorded","description":"check"},"script":"return 1"}`)); err != nil {
		t.Fatalf("recorder failure changed workflow result: %v", err)
	}
	// Four worker lifecycle events plus the tool's final workflow/run summary.
	if recordFailures != 1 || runtimeEvents != 5 {
		t.Fatalf("recordFailures=%d runtimeEvents=%d, want one disabled record and five runtime events", recordFailures, runtimeEvents)
	}
}

// TestCrashOpenWorkflowRestartDoesNotReplayExternalChild restores the durable
// crash-open prefix left by a dead worker, then runs an explicit new workflow.
// Restart materialization and the new run must not re-invoke the old child;
// the new run is a distinct receipt with its own admission.
func TestCrashOpenWorkflowRestartDoesNotReplayExternalChild(t *testing.T) {
	crashed := session.New()
	emit := func(typ string, data any) {
		t.Helper()
		if _, err := crashed.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	emit(session.EventToolWorkflowRunStart, map[string]any{"runId": "old-run", "name": "old"})
	emit(session.EventToolWorkflowAgentStart, map[string]any{
		"runId": "old-run", "seq": 1, "label": "old member", "childId": "old-child",
	})

	restored := session.New()
	if err := restored.Restore(crashed.Events()); err != nil {
		t.Fatalf("restore crash-open workflow log: %v", err)
	}

	runner := &restartScriptRunner{}
	tool := NewWorkflowRunToolWithScript(nil, runner, func(_ context.Context, req AgentRequest) (AgentResult, error) {
		return AgentResult{ID: req.Prompt, StopReason: "completed"}, nil
	}, func() string { return "restored-parent" }, nil)
	tool.SetEmitContext(func(_ context.Context, typ string, data any) error {
		_, err := restored.Append(typ, data)
		return err
	})

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{
		"meta":{"name":"new-run","description":"after restart"},
		"script":"return \"new work\""
	}`)); err != nil {
		t.Fatalf("new workflow after restart: %v", err)
	}
	if len(runner.agentRequests) != 0 {
		t.Fatalf("restart replayed external children: %+v", runner.agentRequests)
	}
	if err := session.ValidateLifecycle(restored.Events()); err != nil {
		t.Fatalf("restored log with old crash prefix and new run: %v", err)
	}
	var runIDs []string
	for _, event := range restored.Events() {
		if event.Type != session.EventToolWorkflowRunStart {
			continue
		}
		var data struct {
			RunID string `json:"runId"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("decode run start: %v", err)
		}
		runIDs = append(runIDs, data.RunID)
	}
	if len(runIDs) != 2 || runIDs[0] != "old-run" || runIDs[1] == "old-run" {
		t.Fatalf("run IDs after restart = %#v, want old crash prefix plus one new run", runIDs)
	}
}

type restartScriptRunner struct {
	agentRequests []AgentRequest
}

func (r *restartScriptRunner) RunScript(_ context.Context, req ScriptRequest, agent AgentStart, emit func(ScriptEvent)) (ScriptResult, error) {
	emit(ScriptEvent{Type: session.EventWorkflowStart, Data: map[string]any{
		"run_id": "new-run", "meta": req.Meta,
	}})
	return ScriptResult{RunID: "new-run", Value: "new work", StopReason: "completed"}, nil
}

// TestWorkflowToolExecuteFormatsReport verifies Execute drives the engine,
// renders the per-task summary (header + completed tasks + bounded output),
// and emits the workflow/run event with the lean counts payload (D3).
func TestWorkflowToolExecuteFormatsReport(t *testing.T) {
	fs := &fakeSpawn{}
	eng := mustEngine(t, fs, 0)
	var events []eventCapture
	tool := NewWorkflowRunTool(eng, func(typ string, data any) {
		events = append(events, eventCapture{typ: typ, data: data})
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[
		{"id":"A","prompt":"任务A"},
		{"id":"B","prompt":"任务B"}
	]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"workflow: 2 tasks", "A: completed", "B: completed", "output: 默认输出"} {
		if !strings.Contains(out, want) {
			t.Errorf("report %q lacks %q", out, want)
		}
	}
	if len(events) != 1 || events[0].typ != session.EventWorkflowRun {
		t.Fatalf("events = %+v, want exactly one %s event", events, session.EventWorkflowRun)
	}
	raw, err := json.Marshal(events[0].data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Total != 2 || p.Completed != 2 || p.Failed != 0 {
		t.Errorf("workflow/run payload = %+v, want total=2 completed=2 failed=0", p)
	}
}

// TestWorkflowToolExecuteCycleError: a cyclic DAG surfaces as an error
// containing the cycle marker (ErrCycle passes through, wrapped).
func TestWorkflowToolExecuteCycleError(t *testing.T) {
	fs := &fakeSpawn{}
	eng := mustEngine(t, fs, 0)
	tool := NewWorkflowRunTool(eng, nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[
		{"id":"A","prompt":"p","depends_on":["B"]},
		{"id":"B","prompt":"p","depends_on":["A"]}
	]}`))
	if err == nil {
		t.Fatal("Execute: want an error for a cyclic DAG")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to contain cycle", err)
	}
}

// TestWorkflowToolExecuteRejectsEmptyTasks: an absent or empty tasks array is
// rejected before any engine work.
func TestWorkflowToolExecuteRejectsEmptyTasks(t *testing.T) {
	fs := &fakeSpawn{}
	eng := mustEngine(t, fs, 0)
	tool := NewWorkflowRunTool(eng, nil)
	for _, args := range []string{`{}`, `{"tasks":[]}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Execute(%s) must be rejected (empty tasks)", args)
		}
	}
}

// TestWorkflowToolExecuteFailureRendersError: a failed task renders the error
// line instead of the output line, and the workflow/run event counts it.
func TestWorkflowToolExecuteFailureRendersError(t *testing.T) {
	fs := &fakeSpawn{fn: func(ctx context.Context, prompt string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if strings.Contains(prompt, "任务B") {
			return "", errors.New("boom")
		}
		return "ok", nil
	}}
	eng := mustEngine(t, fs, 0)
	var events []eventCapture
	tool := NewWorkflowRunTool(eng, func(typ string, data any) {
		events = append(events, eventCapture{typ: typ, data: data})
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[
		{"id":"A","prompt":"任务A"},
		{"id":"B","prompt":"任务B"}
	]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "B: failed") || !strings.Contains(out, "error: boom") {
		t.Errorf("report %q must render B as failed with the error", out)
	}
	if strings.Contains(out, "A: failed") {
		t.Errorf("report %q marks A failed, want completed", out)
	}
	raw, err := json.Marshal(events[0].data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Total != 2 || p.Completed != 1 || p.Failed != 1 {
		t.Errorf("workflow/run payload = %+v, want total=2 completed=1 failed=1", p)
	}
}
