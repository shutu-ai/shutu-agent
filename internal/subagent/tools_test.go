package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// Compile-time assertions: the four subagent_* tools implement the tool method
// set the composition root boxes into a tools.Registry.
var (
	_ = (*SubagentSpawnTool)(nil)
	_ = (*SubagentTeammateTool)(nil)
	_ = (*SubagentMessageTool)(nil)
	_ = (*SubagentWaitTool)(nil)
	_ = (*SubagentStatusTool)(nil)
	_ = (*SubagentCancelTool)(nil)
	_ = (*SubagentListTool)(nil)
)

func TestDSHAgentControlSurface(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	ctx := context.Background()

	created, err := st.SpawnTeammate().ExecuteResult(ctx, json.RawMessage(`{"name":"researcher","description":"research","prompt":"inspect the docs"}`))
	if err != nil {
		t.Fatalf("spawn_teammate: %v", err)
	}
	member, ok := created.Value.(map[string]any)["member"].(map[string]any)
	if !ok || member["name"] != "researcher" || member["role"] != "teammate" {
		t.Fatalf("spawn_teammate value = %#v", created.Value)
	}
	id, _ := member["id"].(string)
	if id == "" {
		t.Fatal("spawn_teammate did not return a child id")
	}

	sent, err := st.DshSend().ExecuteResult(ctx, json.RawMessage(`{"target":"researcher","message":"please continue"}`))
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if sent.Value.(map[string]any)["status"] != "queued" {
		t.Fatalf("send_message value = %#v, want queued", sent.Value)
	}

	empty := testBundle(t, model, 8, nil)
	waited, err := empty.WaitAgent().ExecuteResult(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("wait_agent: %v", err)
	}
	value := waited.Value.(map[string]any)
	if value["timedOut"] != false || value["noProgress"] == nil {
		t.Fatalf("wait_agent no-progress value = %#v", waited.Value)
	}
}

// TestSubagentCancellationClassification makes the lifecycle distinction
// explicit: wait observes the caller context, while foreground spawn returns
// from a child that intentionally has an independent lifecycle.
func TestSubagentCancellationClassification(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	st := testBundle(t, model, 8, nil)
	for _, tool := range []any{st.WaitAgent()} {
		classified, ok := tool.(interface{ CancellationAware() bool })
		if !ok || !classified.CancellationAware() {
			t.Fatalf("%T must classify its bounded wait as cancellable", tool)
		}
	}
	for _, tool := range []any{st.Spawn(), st.Fork()} {
		if _, ok := tool.(interface{ CancellationAware() bool }); ok {
			t.Fatalf("%T must not claim caller-context cancellation for an independently owned child", tool)
		}
	}
}

type restoredTeammateDirectory struct {
	sent, quiet, followed, cancelled bool
}

func (d *restoredTeammateDirectory) List(context.Context, string) ([]Teammate, error) {
	return []Teammate{{ID: "team:parent:researcher", Label: "researcher", Running: true, Continuable: true}}, nil
}

func (d *restoredTeammateDirectory) Direct(_ context.Context, parent, target string) (Teammate, error) {
	_ = parent
	if target != "team:parent:researcher" && target != "researcher" {
		return Teammate{}, errors.New("unknown teammate")
	}
	return Teammate{
		ID: "team:parent:researcher", Label: "researcher", Running: true, Continuable: true,
		Send:      func(context.Context, string, map[string]string) error { d.sent = true; return nil },
		SendQuiet: func(context.Context, string) error { d.quiet = true; return nil },
		Followup:  func(context.Context, string) error { d.followed = true; return nil },
		Cancel:    func(string) error { d.cancelled = true; return nil },
	}, nil
}

func (*restoredTeammateDirectory) Parent(_ context.Context, id string) (string, bool, error) {
	if id == "team:parent:researcher" {
		return "sess-1", true, nil
	}
	return "", false, nil
}

func TestRestoredTeammateDirectoryFeedsControlPlane(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{{Kind: llm.StreamFinish, FinishReason: "stop"}}}}
	st := testBundle(t, model, 8, nil)
	directory := &restoredTeammateDirectory{}
	st.SetTeammateDirectory(directory)
	ctx := context.Background()

	list, err := st.ListAgents().ExecuteResult(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_agents restored: %v", err)
	}
	entries := list.Value.([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["id"] != "team:parent:researcher" {
		t.Fatalf("restored list = %#v", list.Value)
	}
	if _, err := st.DshSend().ExecuteResult(ctx, json.RawMessage(`{"target":"researcher","message":"quiet update"}`)); err != nil {
		t.Fatalf("send_message restored: %v", err)
	}
	if !directory.quiet {
		t.Fatal("send_message did not use the quiet restored-member callback")
	}
	if _, err := st.FollowupTask().ExecuteResult(ctx, json.RawMessage(`{"target":"researcher","message":"wake"}`)); err != nil {
		t.Fatalf("followup_task restored: %v", err)
	}
	if !directory.followed {
		t.Fatal("followup_task did not use the restored-member callback")
	}
	if _, err := st.Interrupt().ExecuteResult(ctx, json.RawMessage(`{"agent_id":"team:parent:researcher"}`)); err != nil {
		t.Fatalf("interrupt_agent restored: %v", err)
	}
	if !directory.cancelled {
		t.Fatal("interrupt_agent did not use the restored-member callback")
	}
}

func TestChildReportToolUsesCanonicalChildScopedContract(t *testing.T) {
	var got struct{ child, parent, output string }
	tool := newChildReportTool("child-1", "parent-1", func(child, parent, output string) (string, error) {
		got = struct{ child, parent, output string }{child, parent, output}
		return "child-1-message-1", nil
	})
	if tool.Name() != "report" {
		t.Fatalf("report tool name = %q", tool.Name())
	}
	properties := tool.Schema()["properties"].(map[string]any)
	if len(properties) != 1 || properties["output"] == nil {
		t.Fatalf("report schema properties = %#v, want output only", properties)
	}
	result, err := tool.ExecuteResult(context.Background(), json.RawMessage(`{"output":"actionable finding"}`))
	if err != nil {
		t.Fatalf("report execute: %v", err)
	}
	if result.Value.(map[string]any)["messageId"] != "child-1-message-1" {
		t.Fatalf("report result = %#v", result.Value)
	}
	if got.child != "child-1" || got.parent != "parent-1" || got.output != "actionable finding" {
		t.Fatalf("report delivery = %#v", got)
	}
}

// eventLog records emitted subagent/* event types (and payloads) for tool
// tests.
type eventLog struct {
	mu   sync.Mutex
	evts []string
	data []any
}

func (e *eventLog) record(typ string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evts = append(e.evts, typ)
	e.data = append(e.data, data)
}

func (e *eventLog) counts() map[string]int {
	e.mu.Lock()
	m := map[string]int{}
	for _, t := range e.evts {
		m[t]++
	}
	e.mu.Unlock()
	return m
}

func (e *eventLog) payload(i int) any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.data[i]
}

// decodeEventPayload unmarshals the i-th recorded event payload into a plain
// struct, since the session payload types are unexported.
func decodeEventPayload[T any](t *testing.T, e *eventLog, i int) T {
	t.Helper()
	var d T
	raw, err := json.Marshal(e.payload(i))
	if err != nil {
		t.Fatalf("marshal event %d payload: %v", i, err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal event %d payload: %v", i, err)
	}
	return d
}

// testBundle wires a fresh Runtime + spawn provider + tool bundle against the
// given model, with owner "sess-1". The runtime is closed at test cleanup so
// no background goroutine leaks.
func testBundle(t *testing.T, model llm.LLM, maxDepth int, onEvent func(string, any)) *SubagentTools {
	t.Helper()
	prov := NewSpawnProvider(Deps{
		LLM:    model,
		Tools:  tools.New(),
		Prompt: prompt.New("x"),
		Model:  "m",
	})
	rt := NewRuntime()
	if err := rt.RegisterProvider(prov); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return NewSubagentToolsWithContinuable(rt, maxDepth, func() string { return "sess-1" }, onEvent, true)
}

// waitSettled polls the bundle's settle cache until the child's terminal
// Result is recorded (the eventual-consistency window after settlement).
func waitSettled(t *testing.T, st *SubagentTools, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		_, ok := st.settled[id]
		st.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subagent %s did not settle within %v", id, timeout)
}

// TestSubagentSpawnReturnsChildID verifies subagent_spawn delegates to the
// spawn provider, returns the child id without blocking (the child runs in the
// background), and emits subagent/start with the lineage (child id / provider /
// parent session / label).
func TestSubagentSpawnReturnsChildID(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child "},
		{Kind: llm.StreamTextDelta, Text: "answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	log := &eventLog{}
	st := testBundle(t, model, 8, log.record)

	out, err := st.Spawn().Execute(context.Background(), json.RawMessage(`{"description":"researcher","prompt":"summarize the docs"}`))
	if err != nil {
		t.Fatalf("subagent_spawn: %v", err)
	}
	if !strings.Contains(out, "started subagent spawn-1") {
		t.Fatalf("subagent_spawn output = %q, want started subagent spawn-1 ...", out)
	}
	c := log.counts()
	if c["subagent/start"] != 1 {
		t.Fatalf("subagent/start count = %d, want 1", c["subagent/start"])
	}
	if c["subagent/end"] != 0 {
		t.Fatalf("subagent/end count = %d, want 0 (spawn only starts, never settles)", c["subagent/end"])
	}
	start := decodeEventPayload[struct {
		ID            string `json:"id"`
		Provider      string `json:"provider"`
		ParentSession string `json:"parentSession"`
		Label         string `json:"label"`
	}](t, log, 0)
	if start.ID != "spawn-1" || start.Provider != "spawn" || start.ParentSession != "sess-1" || start.Label != "researcher" {
		t.Fatalf("subagent/start payload = %+v", start)
	}
	// The spawn event records the injected owner as the parent.
}

func TestSubagentSpawnForwardsAgentOptions(t *testing.T) {
	model := &captureMaxTokensLLM{}
	st := testBundle(t, model, 8, nil)
	result, err := st.Spawn().ExecuteResult(context.Background(), json.RawMessage(`{"description":"specialist","prompt":"inspect","run_in_background":false,"agentOptions":{"model":"child-model","maxTokens":222}}`))
	if err != nil {
		t.Fatalf("subagent with agentOptions: %v", err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("spawn value = %#v", result.Value)
	}
	id, _ := value["runId"].(string)
	if id == "" {
		t.Fatalf("spawn value has no runId: %#v", result.Value)
	}
	waitSettled(t, st, id, 5*time.Second)
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) != 1 || model.requests[0].Model != "child-model" || model.requests[0].MaxTokens != 222 {
		t.Fatalf("child request = %+v, want model child-model/maxTokens 222", model.requests)
	}
}

func TestSubagentAsyncSettlementUsesParentSessionEventSink(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child result"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	type event struct {
		session string
		typ     string
	}
	events := make(chan event, 4)
	st.SetSessionEventSink(func(sessionID, typ string, _ any) error {
		events <- event{session: sessionID, typ: typ}
		return nil
	})

	result, err := st.Spawn().Execute(context.Background(), json.RawMessage(`{"description":"worker","prompt":"finish"}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := st.Cancel().Execute(context.Background(), json.RawMessage(`{"id":"spawn-1","reason":"test"}`)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitSettled(t, st, "spawn-1", time.Second)
	select {
	case got := <-events:
		if got.session != "sess-1" || got.typ != "subagent/end" {
			t.Fatalf("async event = %+v, want parent-scoped subagent/end", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for parent-scoped settlement event")
	}
	if !strings.Contains(result, "started subagent spawn-1") {
		t.Fatalf("spawn result = %q", result)
	}
}

func TestSubagentAsyncSettlementInvokesParentCompletionWake(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child result"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	type wake struct {
		session string
		child   string
		result  Result
	}
	wakes := make(chan wake, 1)
	st.SetCompletionWake(func(_ context.Context, sessionID, childID string, result Result) error {
		wakes <- wake{session: sessionID, child: childID, result: result}
		return nil
	})

	if _, err := st.Spawn().Execute(context.Background(), json.RawMessage(`{"description":"worker","prompt":"finish","run_in_background":false}`)); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	waitSettled(t, st, "spawn-1", 5*time.Second)
	select {
	case got := <-wakes:
		if got.session != "sess-1" || got.child != "spawn-1" {
			t.Fatalf("completion wake = %+v, want parent session and child id", got)
		}
		if got.result.StopReason != StopCompleted || got.result.Output != "child result" {
			t.Fatalf("completion result = %+v, want completed child result", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for parent completion wake")
	}
}

func TestSubagentReportDeliversDSHRelayAfterDurableEvent(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child result"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	type reportEvent struct {
		id     string
		parent string
	}
	events := make(chan reportEvent, 8)
	st.SetSessionEventSink(func(_ string, _ string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal report event: %v", err)
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode report event: %v", err)
		}
		var start struct {
			ParentSession string `json:"parentSession"`
		}
		_ = json.Unmarshal(raw, &start)
		events <- reportEvent{id: payload.ID, parent: start.ParentSession}
		return nil
	})
	type delivery struct {
		parent, child, messageID, output string
	}
	deliveries := make(chan delivery, 1)
	st.SetReportDelivery(func(_ context.Context, parent, child, messageID, output string) error {
		deliveries <- delivery{parent: parent, child: child, messageID: messageID, output: output}
		return nil
	})

	if _, err := st.Spawn().Execute(context.Background(), json.RawMessage(`{"description":"worker","prompt":"finish"}`)); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	messageID, err := st.ReportFromChildContext(context.Background(), "spawn-1", "selected findings")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	// Spawn emits start before report; consume that lifecycle row while still
	// requiring the report row to exist before ReportFromChildContext returns.
	sawReport := false
	for deadline := time.After(time.Second); !sawReport; {
		select {
		case got := <-events:
			if got.id != "spawn-1" {
				continue
			}
			if got.parent != "sess-1" {
				t.Fatalf("report event parent = %q, want sess-1", got.parent)
			}
			sawReport = true
		case <-deadline:
			t.Fatal("timed out waiting for durable report event")
		}
	}
	select {
	case got := <-deliveries:
		if got.parent != "sess-1" || got.child != "spawn-1" ||
			got.messageID != messageID || got.output != "selected findings" {
			t.Fatalf("report delivery = %+v, want parent/child/message/output identity", got)
		}
	default:
		t.Fatal("timed out waiting for parent report delivery")
	}
}

func TestSubagentCompletionWakeWaitsForDurableEvent(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	st.SetSessionEventSink(func(string, string, any) error {
		return errors.New("durable sink unavailable")
	})
	wakes := make(chan struct{}, 1)
	st.SetCompletionWake(func(context.Context, string, string, Result) error {
		wakes <- struct{}{}
		return nil
	})
	if _, err := st.Spawn().Execute(context.Background(), json.RawMessage(`{"description":"worker","prompt":"finish","run_in_background":false}`)); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	waitSettled(t, st, "spawn-1", 5*time.Second)
	select {
	case <-wakes:
		t.Fatal("completion wake was sent after durable event failure")
	default:
	}
}

// TestSubagentSpawnAcceptanceCriteria verifies subagent_spawn's schema exposes
// the acceptance_criteria array and Execute passes it through (Eval-2b): the
// spawned child's first user message contains the injected acceptance section
// with both criteria.
func TestSubagentSpawnAcceptanceCriteria(t *testing.T) {
	t.Skip("acceptance criteria is intentionally removed from the DSH model-facing schema")
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	ctx := context.Background()

	schema := st.Spawn().Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("spawn schema properties = %T, want map[string]any", schema["properties"])
	}
	if _, ok := props["run_in_background"]; !ok {
		t.Fatalf("spawn schema must expose run_in_background, got properties %v", props)
	}

	out, err := st.Spawn().Execute(ctx, json.RawMessage(
		`{"prompt":"do X","acceptance_criteria":["contains:输出含报告","llm:结论合理"]}`))
	if err != nil {
		t.Fatalf("subagent_spawn with acceptance_criteria: %v", err)
	}
	if !strings.Contains(out, "started subagent spawn-1") {
		t.Fatalf("subagent_spawn output = %q, want started subagent spawn-1", out)
	}
	// The child settles in the background; wait for it before inspecting the
	// model request it made.
	waitSettled(t, st, "spawn-1", 5*time.Second)
	if len(model.calls) != 1 {
		t.Fatalf("child llm calls = %d, want 1", len(model.calls))
	}
	user := userMessageText(model.calls[0])
	if !strings.Contains(user, "验收标准") || !strings.Contains(user, "contains:输出含报告") || !strings.Contains(user, "llm:结论合理") {
		t.Fatalf("child user message = %q, want the injected acceptance section", user)
	}
}

// TestSubagentStatusReflectsResultAndEmitsEndOnce verifies subagent_status
// reflects a settled child's Result (output + stop reason) and emits
// subagent/end exactly once across repeated observations.
func TestSubagentStatusReflectsResultAndEmitsEndOnce(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "child answer"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	log := &eventLog{}
	st := testBundle(t, model, 8, log.record)
	ctx := context.Background()

	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"r","prompt":"go","run_in_background":false}`)); err != nil {
		t.Fatalf("subagent_spawn: %v", err)
	}
	waitSettled(t, st, "spawn-1", 5*time.Second)

	out, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`))
	if err != nil {
		t.Fatalf("subagent_status: %v", err)
	}
	if !strings.Contains(out, "settled") || !strings.Contains(out, "stop_reason=completed") || !strings.Contains(out, "child answer") {
		t.Fatalf("subagent_status output = %q, want settled completed with output", out)
	}
	if c := log.counts(); c["subagent/end"] != 1 {
		t.Fatalf("subagent/end count = %d, want exactly 1", c["subagent/end"])
	}
	end := decodeEventPayload[struct {
		ID            string `json:"id"`
		Provider      string `json:"provider"`
		StopReason    string `json:"stopReason"`
		OutputSummary string `json:"outputSummary"`
	}](t, log, 1)
	if end.ID != "spawn-1" || end.Provider != "spawn" || end.StopReason != "completed" || end.OutputSummary != "child answer" {
		t.Fatalf("subagent/end payload = %+v", end)
	}
	// Repeated observations must not re-emit subagent/end.
	if _, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`)); err != nil {
		t.Fatalf("subagent_status (2nd): %v", err)
	}
	if _, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`)); err != nil {
		t.Fatalf("subagent_status (3rd): %v", err)
	}
	if c := log.counts(); c["subagent/end"] != 1 {
		t.Fatalf("subagent/end count after repeats = %d, want exactly 1", c["subagent/end"])
	}
}

// TestSubagentStatusRunningBeforeSettle verifies a still-live child reports
// "running" without blocking and without emitting subagent/end.
func TestSubagentStatusRunningBeforeSettle(t *testing.T) {
	started := make(chan struct{})
	st := testBundle(t, &blockingLLM{started: started}, 8, nil)
	ctx := context.Background()

	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"slow","prompt":"work"}`)); err != nil {
		t.Fatalf("subagent_spawn: %v", err)
	}
	<-started // the child is live inside its first model request
	out, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`))
	if err != nil {
		t.Fatalf("subagent_status: %v", err)
	}
	if !strings.Contains(out, "running") || strings.Contains(out, "settled") {
		t.Fatalf("subagent_status output = %q, want running (not settled)", out)
	}
}

// TestSubagentCancelRequestsCancellation verifies subagent_cancel returns
// "requested" for a live child (which then settles aborted) and
// "already-finished" for a settled one, and that a cancelled child surfaces
// stop_reason=aborted through subagent_status.
func TestSubagentCancelRequestsCancellation(t *testing.T) {
	started := make(chan struct{})
	log := &eventLog{}
	st := testBundle(t, &blockingLLM{started: started}, 8, log.record)
	ctx := context.Background()

	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"slow","prompt":"work"}`)); err != nil {
		t.Fatalf("subagent_spawn: %v", err)
	}
	<-started
	out, err := st.Cancel().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`))
	if err != nil {
		t.Fatalf("subagent_cancel: %v", err)
	}
	if out != "requested" {
		t.Fatalf("subagent_cancel = %q, want requested", out)
	}
	waitSettled(t, st, "spawn-1", 5*time.Second)
	if out, err := st.Cancel().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`)); err != nil || out != "already-finished" {
		t.Fatalf("subagent_cancel (settled) = %q, err = %v, want already-finished", out, err)
	}
	statusOut, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"spawn-1"}`))
	if err != nil {
		t.Fatalf("subagent_status: %v", err)
	}
	if !strings.Contains(statusOut, "stop_reason=aborted") {
		t.Fatalf("subagent_status after cancel = %q, want stop_reason=aborted", statusOut)
	}
	if c := log.counts(); c["subagent/end"] != 1 {
		t.Fatalf("subagent/end count = %d, want exactly 1 for the aborted child", c["subagent/end"])
	}
}

// TestSubagentListProjectsChildren verifies subagent_list projects the
// children spawned under a parent session (defaulted to the owner, or given
// explicitly within the caller's scope).
func TestSubagentListProjectsChildren(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "stop"}},
		{{Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	st := testBundle(t, model, 8, nil)
	ctx := context.Background()

	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"one","prompt":"a"}`)); err != nil {
		t.Fatalf("spawn one: %v", err)
	}
	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"two","prompt":"b"}`)); err != nil {
		t.Fatalf("spawn two: %v", err)
	}
	// Default parent = the injected owner session.
	out, err := st.List().Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("subagent_list: %v", err)
	}
	if !strings.Contains(out, "spawn-1") || !strings.Contains(out, "spawn-2") || !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("subagent_list output = %q, want both children under sess-1", out)
	}
	// Explicit parent is honored.
	out2, err := st.List().Execute(ctx, json.RawMessage(`{"parent_session":"sess-1"}`))
	if err != nil {
		t.Fatalf("subagent_list (explicit parent): %v", err)
	}
	if !strings.Contains(out2, "spawn-1") {
		t.Fatalf("subagent_list (explicit parent) output = %q, want spawn-1", out2)
	}
	// An unrelated parent is not an observable empty result; it is an
	// authorization failure so callers cannot probe another Agent's children.
	if _, err := st.List().Execute(ctx, json.RawMessage(`{"parent_session":"other"}`)); err == nil || !strings.Contains(err.Error(), "outside the calling agent scope") {
		t.Fatalf("subagent_list (other parent) error = %v, want parent-scope rejection", err)
	}
}

func TestDSHControlToolsUseCanonicalSchemasAndValues(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	st := testBundle(t, model, 8, nil)
	ctx := context.Background()
	if _, err := st.Spawn().Execute(ctx, json.RawMessage(`{"description":"research","prompt":"work"}`)); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	list, err := st.ListAgents().ExecuteResult(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}
	entries, ok := list.Value.([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("list_agents value = %#v, want one projected child", list.Value)
	}
	entry := entries[0].(map[string]any)
	if entry["kind"] != "child" || entry["id"] != "spawn-1" || entry["status"] != "running" {
		t.Fatalf("list_agents entry = %#v", entry)
	}
	sent, err := st.Send().ExecuteResult(ctx, json.RawMessage(`{"subagent_id":"spawn-1","message":"continue"}`))
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if sent.Value.(map[string]any)["messageId"] == "" || !strings.Contains(sent.Output, "next turn") {
		t.Fatalf("send_message result = %#v / %q", sent.Value, sent.Output)
	}
	interrupted, err := st.Interrupt().ExecuteResult(ctx, json.RawMessage(`{"agent_id":"spawn-1"}`))
	if err != nil {
		t.Fatalf("interrupt_agent: %v", err)
	}
	if interrupted.Value.(map[string]any)["accepted"] != true {
		t.Fatalf("interrupt_agent value = %#v", interrupted.Value)
	}
}

// TestSubagentToolsSchemaValidation verifies the D7 gate: the four subagent
// tools' schemas reject missing required fields, wrong argument types, and
// unknown (additional) properties at the registry's Execute gate.
func TestSubagentToolsSchemaValidation(t *testing.T) {
	st := testBundle(t, &scriptedLLM{}, 8, nil)
	reg := tools.New()
	reg.SetPolicy(tools.Policy{
		Enabled:     []string{ToolSpawnName, ToolStatusName, ToolCancelName, ToolListName, ToolListAgentsName},
		Timeout:     0,
		OutputLimit: 0,
	})
	for _, tool := range []tools.Tool{st.Spawn(), st.Status(), st.Cancel(), st.List(), st.ListAgents()} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	for _, tc := range []struct {
		name string
		args string
	}{
		{"subagent", `{}`}, // missing required description/prompt
		{"subagent", `{"description":"x","prompt":""}`},                     // empty prompt
		{"subagent", `{"description":"x","prompt":"x","extra":1}`},          // additional properties rejected
		{"subagent", `{"description":"x","prompt":"x","provider":"spawn"}`}, // provider is bound by composition
		{"subagent_status", `{}`},                                           // missing required id
		{"subagent_status", `{"id":123}`},                                   // id must be a string
		{"subagent_cancel", `{}`},                                           // missing required id
		{"subagent_cancel", `{"id":false}`},                                 // wrong id type
		{"subagent_list", `{"parent_session":123}`},                         // wrong parent type
		{"list_agents", `{"scope":"invalid"}`},                              // closed enum
		{"send_message", `{"subagent_id":123,"message":"x"}`},
		{"interrupt_agent", `{"agent_id":123}`},
	} {
		if _, err := reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}
	// A valid spawn flows through the registry and returns the child id.
	res, err := reg.Execute(context.Background(), "subagent", json.RawMessage(`{"description":"go","prompt":"go"}`))
	if err != nil {
		t.Fatalf("subagent via registry: %v", err)
	}
	if !strings.Contains(res.Output, "started subagent spawn-1") {
		t.Fatalf("subagent_spawn output = %q, want started subagent spawn-1", res.Output)
	}
}

// TestSubagentUnknownChild verifies status/cancel on an unknown child id are
// surfaced as errors.
func TestSubagentUnknownChild(t *testing.T) {
	st := testBundle(t, &scriptedLLM{}, 8, nil)
	ctx := context.Background()
	if _, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"nope-1"}`)); err == nil {
		t.Fatal("subagent_status on an unknown id must fail")
	}
	if _, err := st.Cancel().Execute(ctx, json.RawMessage(`{"id":"nope-1"}`)); err == nil {
		t.Fatal("subagent_cancel on an unknown id must fail")
	}
}

func TestSubagentResumeEnforcesCallingAgentLineage(t *testing.T) {
	st := testBundle(t, &scriptedLLM{}, 8, nil)
	st.mu.Lock()
	st.children["child-1"] = &childInfo{run: &Run{}, provider: defaultProviderName, parent: "sess-1"}
	st.mu.Unlock()

	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "other-session"})
	if _, err := st.Resume().Execute(ctx, json.RawMessage(`{"id":"child-1","message":"resume"}`)); err == nil || !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("cross-agent resume error = %v, want lineage rejection", err)
	}
	if _, err := st.Status().Execute(ctx, json.RawMessage(`{"id":"child-1"}`)); err == nil || !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("cross-agent status error = %v, want lineage rejection", err)
	}
	if _, err := st.Cancel().Execute(ctx, json.RawMessage(`{"id":"child-1"}`)); err == nil || !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("cross-agent cancel error = %v, want lineage rejection", err)
	}
	if _, err := st.List().Execute(ctx, json.RawMessage(`{"parent_session":"sess-1"}`)); err == nil || !strings.Contains(err.Error(), "outside the calling agent scope") {
		t.Fatalf("cross-agent list error = %v, want parent-scope rejection", err)
	}
}

// TestSubagentControlPlaneOwnershipMatrix consolidates the A5.1 authority
// boundary across the caller-addressed control actions. A child registered to
// one parent must be invisible to another caller, and none of its callbacks may
// run for status, resume, send, followup, interrupt, cancel, list or wait.
func TestSubagentControlPlaneOwnershipMatrix(t *testing.T) {
	st := testBundle(t, &scriptedLLM{}, 8, nil)
	var callbackRuns atomic.Int32
	callback := func() { callbackRuns.Add(1) }
	st.mu.Lock()
	st.children["owned-child"] = &childInfo{
		run: &Run{
			ID:   "owned-child",
			Send: func(context.Context, string) error { callback(); return nil },
			SendQuiet: func(context.Context, string) error {
				callback()
				return nil
			},
			Cancel: func(string) error { callback(); return nil },
		},
		provider: defaultProviderName, label: "owned", parent: "sess-1",
	}
	st.mu.Unlock()

	other := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "other-session"})
	rejections := []struct {
		name string
		call func() error
	}{
		{"status", func() error {
			_, err := st.Status().Execute(other, json.RawMessage(`{"id":"owned-child"}`))
			return err
		}},
		{"resume", func() error {
			_, err := st.Resume().Execute(other, json.RawMessage(`{"id":"owned-child","message":"no"}`))
			return err
		}},
		{"send_message", func() error {
			_, err := st.DshSend().ExecuteResult(other, json.RawMessage(`{"target":"owned-child","message":"no"}`))
			return err
		}},
		{"followup_task", func() error {
			_, err := st.FollowupTask().ExecuteResult(other, json.RawMessage(`{"target":"owned-child","message":"no"}`))
			return err
		}},
		{"interrupt_agent", func() error {
			_, err := st.Interrupt().Execute(other, json.RawMessage(`{"agent_id":"owned-child"}`))
			return err
		}},
		{"subagent_cancel", func() error {
			_, err := st.Cancel().Execute(other, json.RawMessage(`{"id":"owned-child"}`))
			return err
		}},
		{"subagent_list", func() error {
			_, err := st.List().Execute(other, json.RawMessage(`{"parent_session":"sess-1"}`))
			return err
		}},
	}
	for _, rejection := range rejections {
		if err := rejection.call(); err == nil {
			t.Fatalf("%s crossed the ownership boundary", rejection.name)
		}
	}

	// Discovery/wait are caller-scoped rather than target-addressed. They may
	// return an empty result, but they must not project or invoke the foreign
	// child.
	wait, err := st.WaitAgent().ExecuteResult(other, json.RawMessage(`{"timeout_ms":10000}`))
	if err != nil {
		t.Fatalf("wait_agent cross-owner: %v", err)
	}
	if strings.Contains(wait.Output, "owned-child") {
		t.Fatalf("wait_agent leaked foreign child: %q", wait.Output)
	}
	list, err := st.ListAgents().ExecuteResult(other, json.RawMessage(`{"scope":"descendants"}`))
	if err != nil {
		t.Fatalf("list_agents cross-owner: %v", err)
	}
	if entries, ok := list.Value.([]any); !ok || len(entries) != 0 {
		t.Fatalf("list_agents leaked foreign children: %#v", list.Value)
	}
	if got := callbackRuns.Load(); got != 0 {
		t.Fatalf("foreign control callbacks ran %d times; want 0", got)
	}
}

// TestSpawnToolProviderField verifies the D-GAP-4 provider field on
// subagent_spawn: "spawn" passed explicitly resolves to the same provider as
// the default, and an unregistered external provider name fails closed with an
// "unknown provider" error (no silent fallback). The test runtime registers
// only the local spawn provider.
func TestSpawnToolProviderField(t *testing.T) {
	t.Skip("provider selection is composition-bound in the DSH tool contract")
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	st := testBundle(t, model, 8, nil)
	ctx := context.Background()

	// Explicit "spawn" is equivalent to the omitted (default) provider.
	out, err := st.Spawn().Execute(ctx, json.RawMessage(`{"prompt":"x","provider":"spawn"}`))
	if err != nil {
		t.Fatalf("subagent_spawn with provider=spawn: %v", err)
	}
	if !strings.Contains(out, "started subagent spawn-1") || !strings.Contains(out, "provider=spawn") {
		t.Fatalf("subagent_spawn output = %q, want started subagent spawn-1 (provider=spawn)", out)
	}

	// An unregistered external provider must fail closed with an unknown-provider
	// error, never fall back to the local provider.
	out, err = st.Spawn().Execute(ctx, json.RawMessage(`{"prompt":"x","provider":"codex"}`))
	if err == nil {
		t.Fatalf("subagent_spawn with unregistered provider=codex must fail, got output %q", out)
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("subagent_spawn error = %v, want it to mention \"unknown provider\"", err)
	}
}
