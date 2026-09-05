package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// fakeSensitiveTool is a stand-in for a whitelisted tool whose name a wiring
// test lists in interact.sensitive_tools. It records whether it executed, so
// the gate tests can prove approved → the tool runs and rejected → it never
// does.
type fakeSensitiveTool struct {
	name     string
	executed bool
}

func (f *fakeSensitiveTool) Name() string        { return f.name }
func (f *fakeSensitiveTool) Description() string { return "fake sensitive tool" }
func (f *fakeSensitiveTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f *fakeSensitiveTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (f *fakeSensitiveTool) Execute(ctx context.Context, args any) (string, error) {
	f.executed = true
	return "ran", nil
}

// makeInteractApp builds a minimal app for interact wiring tests: only the
// fields registerInteracts touches (cfg.Interact, reg, log, currentID,
// approveInput) are set.
func makeInteractApp(enabled bool, sensitive []string) *app {
	return &app{
		cfg: config.Config{
			Interact: config.InteractConfig{Enabled: config.Bool(enabled), SensitiveTools: sensitive},
		},
		reg:          tools.New(),
		log:          session.New(),
		currentID:    "s-interact",
		approveInput: strings.NewReader("y\n"),
	}
}

// interactPolicy whitelists the tools the test executes so the registry Execute
// gate can run them (in production config.applyDefaults + PolicyFromConfig do
// this).
func interactPolicy(extra ...string) tools.Policy {
	return tools.Policy{Enabled: append([]string{"ask_user_question"}, extra...)}
}

// TestRegisterInteractsDisabledRegistersNothing verifies the D10 gate: with
// interact.enabled=false the composition root creates no Engine, registers no
// interact_* tool, and installs no sensitive-tool gate (dispatch-m6d-2 §5).
func TestRegisterInteractsDisabledRegistersNothing(t *testing.T) {
	a := makeInteractApp(false, nil)
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	if a.interacts != nil {
		t.Fatal("interact engine must be nil when interact.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "interact_") {
			t.Fatalf("interact tool %q registered while interact disabled", spec.Name)
		}
	}
	// No gate installed: a whitelisted tool runs without any approval read.
	// approveInput would block or feed junk if a gate existed; here the tool
	// executes untouched.
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	a.reg.SetPolicy(interactPolicy("bash"))
	if _, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("run_command with interact disabled: %v", err)
	}
	if !ft.executed {
		t.Fatal("run_command must run with no gate when interact is disabled")
	}
}

func TestRegisterInteractsDoesNotReplayUnrelatedDamagedSession(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "damaged-history", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, event := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventTurnStart, session.NewTurnStart()},
	} {
		if _, err := log.Append(event.typ, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvents(ctx, "damaged-history", log.Events()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadSession(ctx, "damaged-history"); err == nil {
		t.Fatal("fixture must fail strict session replay")
	}

	a := makeInteractApp(true, nil)
	a.store = st
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("interaction startup should ignore unrelated transcript damage: %v", err)
	}
	defer a.interacts.Close()
}

// TestAskUserQuestionCatalogCancellationClassification verifies that the
// long-running interactive tool exposes the same cancellation contract that
// its Await(ctx) implementation provides.
func TestAskUserQuestionCatalogCancellationClassification(t *testing.T) {
	a := makeInteractApp(true, nil)
	if err := a.registerInteracts(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range a.reg.Catalog() {
		if entry.Name == "ask_user_question" && !entry.Cancellable {
			t.Fatal("ask_user_question must declare cooperative cancellation")
		}
	}
}

// TestRegisterInteractsEnabledRegistersToolsAndEvents verifies the enabled
// path: the Provider + Engine are created, the DSH question tool is
// registered, D7 rejects bad arguments at the Execute gate, valid calls flow
// through (ask → status), and the interact/* events land in the session log
// (D3).
func TestRegisterInteractsEnabledRegistersToolsAndEvents(t *testing.T) {
	a := makeInteractApp(true, nil)
	a.reg.SetPolicy(interactPolicy())
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()
	if a.interacts == nil {
		t.Fatal("interact engine must be created when interact.enabled=true")
	}
	names := make([]string, 0, len(a.reg.Specs()))
	for _, s := range a.reg.Specs() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"ask_user_question"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"ask_user_question", `{}`},                                        // missing required questions
		{"ask_user_question", `{"questions":[{"id":123,"question":"p"}]}`}, // id must be a string
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}
	// DSH leaves the empty-batch rejection to the user-questions service rather
	// than advertising a minItems constraint in the model schema.
	if result, err := a.reg.Execute(context.Background(), "ask_user_question", json.RawMessage(`{"questions":[]}`)); err != nil || !result.IsError {
		t.Fatalf("ask_user_question with an empty batch = result=%+v err=%v, want execution error", result, err)
	}

	// A valid question blocks until the human response is resolved, matching DSH.
	result := make(chan error, 1)
	go func() {
		_, err := a.reg.Execute(context.Background(), "ask_user_question", json.RawMessage(`{"questions":[{"id":"confirm","question":"Proceed?","options":[{"label":"yes"}]}]}`))
		result <- err
	}()
	var req interact.Request
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := a.interacts.List(context.Background())
		if len(items) == 1 {
			req = items[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.ID == "" {
		t.Fatal("ask_user_question did not create a request")
	}
	resolver, ok := a.interacts.(interact.AnswerResolver)
	if !ok {
		t.Fatal("interaction engine does not support structured answers")
	}
	if _, err := resolver.ResolveWithAnswer(context.Background(), req.ID, interact.StatusApproved, `{"answers":[{"id":"confirm","selected":["yes"]}]}`); err != nil {
		t.Fatalf("resolve question: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("ask_user_question via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventInteractRequest) {
		t.Fatal("interact/request event missing from the session log after ask_user_question")
	}
	for _, removed := range []string{"interact_ask", "interact_status"} {
		if _, err := a.reg.Execute(context.Background(), removed, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("removed legacy tool %q is still executable", removed)
		}
	}
	// The interact/* rows never derive into model messages (log-only).
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("interact/* events must not derive into messages: %+v", msgs)
	}
}

// TestSensitiveGateApprovedRuns verifies the ADR 决策 M6d gate approved path
// (dispatch-m6d-2 §4): a tool listed in sensitive_tools first creates a pending
// approval request (interact/request), reads the user's y answer on the CLI
// serial path, records the decision (interact/resolve), and only then executes
// the underlying tool — whose output is returned.
func TestSensitiveGateApprovedRuns(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.approveInput = strings.NewReader("y\n")
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.ExecuteWithCallID(context.Background(), "call-sensitive-1", "bash", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("run_command through the approved gate: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (approved → the tool runs)", res.Output, ft.executed)
	}
	// The gate recorded request + resolve, and no deny.
	if !hasEvent(a.log, session.EventInteractRequest) || !hasEvent(a.log, session.EventInteractResolve) {
		t.Fatalf("log = %+v, want interact/request + interact/resolve", a.log.Events())
	}
	if hasEvent(a.log, session.EventInteractDeny) {
		t.Fatal("interact/deny must not fire on an approved execution")
	}
	var asked struct {
		CallID string `json:"callId"`
	}
	if err := json.Unmarshal(a.log.Events()[0].Data, &asked); err != nil || asked.CallID != "call-sensitive-1" {
		t.Fatalf("approval request call id = %q, err=%v", asked.CallID, err)
	}
}

func TestSensitiveGateFailsClosedWhenApprovalEventCannotPersist(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.approveInput = strings.NewReader("y\n")
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	a.log.SetSink(func(session.Event) error { return errors.New("disk full") })
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()
	if _, err := a.reg.ExecuteWithCallID(context.Background(), "call-no-durable-approval", "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatal("approval persistence failure must deny the sensitive tool")
	}
	if ft.executed {
		t.Fatal("sensitive tool executed after approval event persistence failure")
	}
}

func TestResolveInteractionDurablyRollsBackOnDecisionAppendFailure(t *testing.T) {
	a := makeInteractApp(true, nil)
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()
	requester, ok := a.interacts.(interact.SessionRequester)
	if !ok {
		t.Fatal("interaction engine lacks session requester")
	}
	req, err := requester.RequestForSession(context.Background(), a.currentID, "Approve?", "bash", "{}")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	a.rememberInteraction(req.ID, a.currentID, req.CallID)
	if _, err := a.log.Append(session.EventApprovalAsked, map[string]any{"id": req.ID, "toolName": req.ToolName}); err != nil {
		t.Fatalf("append asked: %v", err)
	}
	a.log.SetSink(func(session.Event) error { return errors.New("disk full") })
	if err := a.resolveInteractionDurably(context.Background(), a.currentID, req.ID, interact.StatusApproved, ""); err == nil {
		t.Fatal("decision append failure must be returned")
	}
	items, err := a.interacts.List(context.Background())
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(items) != 1 || items[0].Status != interact.StatusPending {
		t.Fatalf("request after rollback = %+v, want one pending request", items)
	}
}

// TestSensitiveGateRejectedReturnsDenial verifies the rejected path: the user's
// n answer records the decision, appends interact/deny, and the gate returns a
// denial to the model — the underlying tool never executes.
func TestSensitiveGateRejectedReturnsDenial(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.approveInput = strings.NewReader("n\n")
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	_, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`))
	if err == nil {
		t.Fatal("run_command through a rejected gate must return a denial")
	}
	if !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("denial error = %v, want it to mention the user rejection", err)
	}
	if ft.executed {
		t.Fatal("run_command must NOT execute after a rejection")
	}
	for _, typ := range []string{session.EventInteractRequest, session.EventInteractResolve, session.EventInteractDeny} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log = %+v, want %s recorded", a.log.Events(), typ)
		}
	}
}

// TestSensitiveGateMissedDoesNotIntercept verifies a whitelisted tool that is
// NOT listed in sensitive_tools passes through untouched: it executes with no
// approval request, no events and no terminal read.
func TestSensitiveGateMissedDoesNotIntercept(t *testing.T) {
	a := makeInteractApp(true, []string{"bash"})
	a.reg.SetPolicy(interactPolicy("read"))
	ft := &fakeSensitiveTool{name: "read"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"x"}`))
	if err != nil {
		t.Fatalf("non-sensitive read: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (no interception)", res.Output, ft.executed)
	}
	if len(a.log.Events()) != 0 {
		t.Fatalf("non-sensitive execution must log no interact event: %+v", a.log.Events())
	}
}

// TestSensitiveGateEmptyListNoGate verifies that an enabled interact with an
// empty sensitive_tools registers the interact_* tools but installs no gate
// (dispatch-m6d-2 §2/§5): a whitelisted tool runs with no approval.
func TestSensitiveGateEmptyListNoGate(t *testing.T) {
	a := makeInteractApp(true, nil)
	a.reg.SetPolicy(interactPolicy("bash"))
	ft := &fakeSensitiveTool{name: "bash"}
	if err := a.reg.Register(ft); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := a.registerInteracts(); err != nil {
		t.Fatalf("registerInteracts: %v", err)
	}
	defer a.interacts.Close()

	res, err := a.reg.Execute(context.Background(), "bash", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("run_command with an empty sensitive list: %v", err)
	}
	if res.Output != "ran" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ran/true (no gate with empty sensitive_tools)", res.Output, ft.executed)
	}
	if len(a.log.Events()) != 0 {
		t.Fatalf("empty sensitive_tools must log no interact event: %+v", a.log.Events())
	}
}
