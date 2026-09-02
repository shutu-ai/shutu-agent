package node

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

func TestRunnerEnforcesTotalAgentCapBeforeConcurrentQueueing(t *testing.T) {
	runner := New(Config{Command: "node", MaxConcurrent: 1, MaxTotalAgents: 1})
	var calls atomic.Int32
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "total-cap", "description": "total-cap"},
		Script: `return await parallel([() => agent("one"), () => agent("two")])`,
	}, func(_ context.Context, req workflow.AgentRequest) (workflow.AgentResult, error) {
		calls.Add(1)
		return workflow.AgentResult{ID: req.Prompt, Output: req.Prompt, StopReason: "completed"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "error" || !strings.Contains(result.Error, "maxTotalAgents") {
		t.Fatalf("result = %+v, want total-agent-cap error", result)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("agent callback calls = %d, want exactly one", got)
	}
}

func TestRunnerCancelsUnawaitedAgentCallbacksAfterWorkflowResult(t *testing.T) {
	runner := New(Config{Command: "node"})
	callbackStarted := make(chan struct{})
	callbackCancelled := make(chan struct{})
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "orphan-agent", "description": "orphan-agent"},
		Script: `agent("fire and forget"); return "workflow done"`,
	}, func(ctx context.Context, _ workflow.AgentRequest) (workflow.AgentResult, error) {
		close(callbackStarted)
		<-ctx.Done()
		close(callbackCancelled)
		return workflow.AgentResult{}, ctx.Err()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "completed" || result.Value != "workflow done" {
		t.Fatalf("result = %+v, want completed workflow result", result)
	}
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("unawaited agent callback did not start")
	}
	select {
	case <-callbackCancelled:
	case <-time.After(time.Second):
		t.Fatal("unawaited agent callback was not cancelled after workflow result")
	}
}

func TestRunnerCancellationStopsNodeProcess(t *testing.T) {
	runner := New(Config{Command: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		result, err := runner.RunScript(ctx, workflow.ScriptRequest{Meta: map[string]any{"name": "cancel", "description": "cancel"}, Script: `await new Promise(() => {})`}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
			return workflow.AgentResult{}, nil
		}, nil)
		if err == nil && result.StopReason == "cancelled" {
			done <- nil
			return
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel error = %v, want resolved cancellation result", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunScript did not settle after cancellation")
	}
}

// TestRunnerRejectsLateResponseAfterTerminalResult proves result ownership is
// immediate: an in-flight callback draining after workflow/result may settle,
// but its late worker response is rejected instead of being accepted.
func TestRunnerRejectsLateResponseAfterTerminalResult(t *testing.T) {
	runner := New(Config{Command: "node"})
	started := make(chan struct{})
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "late-response", "description": "late-response"},
		Script: `globalThis.late = agent("late response"); return "terminal"`,
	}, func(_ context.Context, _ workflow.AgentRequest) (workflow.AgentResult, error) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		return workflow.AgentResult{ID: "late-child", Output: "late", StopReason: "completed"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "completed" || result.Value != "terminal" {
		t.Fatalf("result = %+v, want terminal workflow result", result)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("late callback did not start")
	}
	if runner.writeFailures.Load() == 0 {
		t.Fatal("late worker response was accepted after terminal result")
	}
}

// TestRunIDsDoNotRestartAfterRunnerRecreation simulates a process restart by
// using a fresh Runner. Opaque run IDs prevent a new run from reusing the ID of
// an older crash-open durable workflow prefix.
func TestRunIDsDoNotRestartAfterRunnerRecreation(t *testing.T) {
	runOnce := func() string {
		runner := New(Config{Command: "node"})
		var events []workflow.ScriptEvent
		result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
			Meta:   map[string]any{"name": "restart-id", "description": "restart-id"},
			Script: `return "done"`,
		}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
			return workflow.AgentResult{}, nil
		}, func(event workflow.ScriptEvent) { events = append(events, event) })
		if err != nil || result.StopReason != "completed" {
			t.Fatalf("workflow run = %+v, err=%v", result, err)
		}
		for _, event := range events {
			if event.Type != "workflow/start" {
				continue
			}
			data, ok := event.Data.(map[string]any)
			if !ok {
				t.Fatalf("workflow/start data = %#v", event.Data)
			}
			runID, _ := data["run_id"].(string)
			return runID
		}
		t.Fatal("workflow/start was not emitted")
		return ""
	}

	first := runOnce()
	second := runOnce()
	if first == "" || second == "" {
		t.Fatalf("run IDs = %q/%q, want two opaque IDs", first, second)
	}
	if first == second {
		t.Fatalf("restarted Runner reused run ID %q", first)
	}
}

// TestRunnerWorkerDeathAfterMemberPublication closes the real Node process
// after its one child receipt is durable. Death must not re-enter the provider,
// duplicate the member, or omit the terminal workflow receipt.
func TestRunnerWorkerDeathAfterMemberPublication(t *testing.T) {
	runner := New(Config{Command: "node"})
	var calls int
	var events []workflow.ScriptEvent
	var eventsMu sync.Mutex
	done := make(chan error, 1)
	go func() {
		_, runErr := runner.RunScript(context.Background(), workflow.ScriptRequest{
			Meta:   map[string]any{"name": "worker-death", "description": "worker-death"},
			Script: `log("PID="+workerPid); const result=await agent("published then die"); await new Promise(()=>{})`,
		}, func(_ context.Context, req workflow.AgentRequest) (workflow.AgentResult, error) {
			calls++
			return workflow.AgentResult{ID: "child-death", Output: "published", StopReason: "completed"}, nil
		}, func(event workflow.ScriptEvent) {
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
		})
		done <- runErr
	}()

	workerPID := 0
	memberPublished := false
	waitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitDeadline) {
		eventsMu.Lock()
		memberPublished = false
		for _, event := range events {
			switch event.Type {
			case "workflow/log":
				data, ok := event.Data.(map[string]any)
				if ok {
					message, _ := data["message"].(string)
					var pid int
					if _, err := fmt.Sscanf(message, "PID=%d", &pid); err == nil {
						workerPID = pid
					}
				}
			case "workflow/agent-end":
				memberPublished = true
			}
		}
		eventsMu.Unlock()
		if workerPID != 0 && memberPublished {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if workerPID == 0 {
		eventsMu.Lock()
		t.Logf("workflow events = %#v", events)
		eventsMu.Unlock()
		t.Fatal("worker did not publish its PID")
	}
	worker, err := os.FindProcess(workerPID)
	if err != nil {
		t.Fatalf("find worker process: %v", err)
	}
	if err := worker.Kill(); err != nil {
		t.Fatalf("kill worker process: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("worker death unexpectedly produced no runner error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunScript did not settle after worker death")
	}
	if calls != 1 {
		t.Fatalf("external provider calls = %d, want exactly one", calls)
	}
	starts, ends := 0, 0
	endIndex := -1
	eventsMu.Lock()
	for index, event := range events {
		switch event.Type {
		case "workflow/agent-start":
			starts++
		case "workflow/agent-end":
			ends++
			endIndex = index
		case "workflow/end":
			if endIndex >= 0 && index < endIndex {
				t.Fatal("workflow/end preceded stranded member end")
			}
		}
	}
	eventsMu.Unlock()
	if starts != 1 || ends != 1 {
		t.Fatalf("member starts/ends = %d/%d, want exactly one published pair", starts, ends)
	}
	last := events[len(events)-1]
	if last.Type != "workflow/end" {
		t.Fatalf("last event = %#v, want workflow/end", last)
	}
	data, ok := last.Data.(map[string]any)
	if !ok || data["stop_reason"] != "error" {
		t.Fatalf("workflow/end = %#v, want error terminal receipt", last.Data)
	}
}

func TestRunnerOmitsUnpublishedMemberWhenCancellationKillsWorker(t *testing.T) {
	runner := New(Config{Command: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make([]workflow.ScriptEvent, 0, 8)
	callbackStarted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		result, err := runner.RunScript(ctx, workflow.ScriptRequest{
			Meta:   map[string]any{"name": "cancel-agent", "description": "cancel-agent"},
			Script: `await agent("cancel me")`,
		}, func(agentCtx context.Context, _ workflow.AgentRequest) (workflow.AgentResult, error) {
			close(callbackStarted)
			<-agentCtx.Done()
			return workflow.AgentResult{}, agentCtx.Err()
		}, func(event workflow.ScriptEvent) { events = append(events, event) })
		if err == nil && result.StopReason == "cancelled" {
			done <- nil
			return
		}
		done <- err
	}()
	select {
	case <-callbackStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("agent callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunScript did not settle after cancellation")
	}
	for index, event := range events {
		if event.Type == "workflow/agent-start" || event.Type == "workflow/agent-end" {
			t.Fatalf("event %d = %#v, want no member pair before a child id is published", index, event)
		}
	}
}

func TestRunnerUsesPortableLabelsAndRejectsLossyResults(t *testing.T) {
	runner := New(Config{Command: "node"})
	var events []workflow.ScriptEvent
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "label", "description": "label"},
		Script: `await agent("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz\nsecond line"); return new Date(0)`,
	}, func(_ context.Context, _ workflow.AgentRequest) (workflow.AgentResult, error) {
		return workflow.AgentResult{ID: "child", Output: "ok", StopReason: "completed"}, nil
	}, func(event workflow.ScriptEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "error" || !strings.Contains(result.Error, "plain JSON") {
		t.Fatalf("lossy result = %+v, want RESULT_UNSERIALIZABLE-style error", result)
	}
	var label string
	for _, event := range events {
		if event.Type != "workflow/agent-start" {
			continue
		}
		if data, ok := event.Data.(map[string]any); ok {
			label, _ = data["label"].(string)
		}
	}
	if len([]rune(label)) != 48 || strings.Contains(label, "second") {
		t.Fatalf("agent label = %q, want first-line 48-rune truncation", label)
	}
}

func TestRunnerOwnsLifecycleWhenNodeCannotStart(t *testing.T) {
	runner := New(Config{Command: "definitely-not-a-node-binary"})
	events := make([]workflow.ScriptEvent, 0, 2)
	_, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "launch-failure", "description": "launch-failure"},
		Script: `return 1`,
	}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
		return workflow.AgentResult{}, nil
	}, func(event workflow.ScriptEvent) { events = append(events, event) })
	if err == nil {
		t.Fatal("RunScript unexpectedly succeeded with a missing Node command")
	}
	if len(events) != 2 || events[0].Type != "workflow/start" || events[1].Type != "workflow/end" {
		t.Fatalf("lifecycle events = %#v, want start then end", events)
	}
	if data, ok := events[1].Data.(map[string]any); !ok || data["stop_reason"] != "error" {
		t.Fatalf("workflow/end = %#v, want error stop reason", events[1].Data)
	}
}

func TestDecodeHostMessageRejectsMalformedAndUnknownFrames(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed json", raw: `{`, want: "valid JSON"},
		{name: "missing type", raw: `{}`, want: "unknown frame type"},
		{name: "unknown type", raw: `{"type":"late"}`, want: `unknown frame type "late"`},
		{name: "event without type", raw: `{"type":"event","data":{}}`, want: "no event type"},
		{name: "non-object event", raw: `{"type":"event","event":"workflow/log","data":[]}`, want: "must be an object"},
		{name: "agent without prompt", raw: `{"type":"agent","id":1}`, want: "no prompt"},
		{name: "result without stop reason", raw: `{"type":"result"}`, want: "no stop reason"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeHostMessage([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodeHostMessageAcceptsProtocolFrames(t *testing.T) {
	for _, raw := range []string{
		`{"type":"event","event":"workflow/log","data":{"message":"ok"}}`,
		`{"type":"agent","id":1,"prompt":"run"}`,
		`{"type":"result","stop_reason":"completed","agents_started":0}`,
	} {
		if _, err := decodeHostMessage([]byte(raw)); err != nil {
			t.Fatalf("decode(%s) = %v", raw, err)
		}
	}
}

func TestRunnerDoesNotTreatForgedFatalObjectAsWorkflowFatal(t *testing.T) {
	runner := New(Config{Command: "node"})
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "fatal-marker", "description": "fatal-marker"},
		Script: `return await parallel([() => { throw { fatal: true, message: "forged" } }, () => "ok"])`,
	}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
		return workflow.AgentResult{}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "completed" {
		t.Fatalf("result = %+v, want completed combinator result", result)
	}
	value, ok := result.Value.([]any)
	if !ok || len(value) != 2 || value[0] != nil || value[1] != "ok" {
		t.Fatalf("forged fatal result = %#v, want [null ok]", result.Value)
	}
}

func TestRunnerDoesNotInheritHostEnvironment(t *testing.T) {
	const key = "SHUTU_WORKFLOW_SECRET_SENTINEL"
	old, existed := os.LookupEnv(key)
	if err := os.Setenv(key, "must-not-cross"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	}()

	runner := New(Config{Command: "node"})
	result, err := runner.RunScript(context.Background(), workflow.ScriptRequest{
		Meta:   map[string]any{"name": "env", "description": "env"},
		Script: `return globalThis.constructor.constructor("return process")().env.SHUTU_WORKFLOW_SECRET_SENTINEL ?? null`,
	}, func(context.Context, workflow.AgentRequest) (workflow.AgentResult, error) {
		return workflow.AgentResult{}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "completed" || result.Value != nil {
		t.Fatalf("environment probe = %+v, want completed with no inherited secret", result)
	}
}
