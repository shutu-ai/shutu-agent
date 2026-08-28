package interact

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// eventRecord captures one (type, payload) pair forwarded through the onEvent
// sink of NewInteractTools.
type eventRecord struct {
	typ string
	raw string
}

// collectEvents builds an onEvent sink that records every forwarded payload.
func collectEvents(recs *[]eventRecord) func(string, any) {
	return func(typ string, data any) {
		raw, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		*recs = append(*recs, eventRecord{typ: typ, raw: string(raw)})
	}
}

// countEventType returns how many recorded events carry typ.
func countEventType(recs []eventRecord, typ string) int {
	n := 0
	for _, r := range recs {
		if r.typ == typ {
			n++
		}
	}
	return n
}

// --- D7 schema shape ----------------------------------------------------------

// TestInteractToolsSchemaShape verifies the D7 argument schemas shipped with
// the tools (dispatch-m6d-2 §3): interact_ask requires a non-empty prompt and
// rejects unknown properties; interact_status requires a non-empty id. The
// registry compiles and enforces these (cmd/pa), so the shape is asserted here.
func TestInteractToolsSchemaShape(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	its := NewInteractTools(e, nil)

	ask := its.Ask().Schema()
	if ask["type"] != "object" || ask["additionalProperties"] != false {
		t.Fatalf("interact_ask schema = %v, want object + additionalProperties:false", ask)
	}
	askProps, _ := ask["properties"].(map[string]any)
	if askProps == nil {
		t.Fatalf("interact_ask properties missing: %v", ask)
	}
	prompt, _ := askProps["prompt"].(map[string]any)
	if prompt == nil || prompt["type"] != "string" || prompt["minLength"] != 1 {
		t.Fatalf("interact_ask prompt property = %v, want string minLength 1", prompt)
	}
	askReq, _ := ask["required"].([]string)
	if !reflect.DeepEqual(askReq, []string{"prompt"}) {
		t.Fatalf("interact_ask required = %v, want [prompt]", askReq)
	}

	status := its.Status().Schema()
	if status["type"] != "object" || status["additionalProperties"] != false {
		t.Fatalf("interact_status schema = %v, want object + additionalProperties:false", status)
	}
	statusProps, _ := status["properties"].(map[string]any)
	if statusProps == nil {
		t.Fatalf("interact_status properties missing: %v", status)
	}
	id, _ := statusProps["id"].(map[string]any)
	if id == nil || id["type"] != "string" || id["minLength"] != 1 {
		t.Fatalf("interact_status id property = %v, want string minLength 1", id)
	}
	statusReq, _ := status["required"].([]string)
	if !reflect.DeepEqual(statusReq, []string{"id"}) {
		t.Fatalf("interact_status required = %v, want [id]", statusReq)
	}
	question := its.AskUserQuestion().Schema()
	if question["type"] != "object" || question["additionalProperties"] != nil {
		t.Fatalf("ask_user_question schema = %v", question)
	}
	questionReq, _ := question["required"].([]string)
	if !reflect.DeepEqual(questionReq, []string{"questions"}) {
		t.Fatalf("ask_user_question required = %v", questionReq)
	}
	questionProps, _ := question["properties"].(map[string]any)
	questions, _ := questionProps["questions"].(map[string]any)
	if questions["description"] != "Questions to ask the user before continuing." {
		t.Fatalf("ask_user_question questions description = %v", questions["description"])
	}
	if questions["minItems"] != nil {
		t.Fatalf("ask_user_question must leave empty-batch rejection to the service, schema = %v", questions)
	}
	items, _ := questions["items"].(map[string]any)
	if items["additionalProperties"] != true {
		t.Fatalf("question items must permit forward-compatible fields, schema = %v", items)
	}
	itemProps, _ := items["properties"].(map[string]any)
	if _, advertised := itemProps["detail"]; advertised {
		t.Fatal("detail is not part of the DSH ask_user_question model surface")
	}
}

// --- interact_ask -------------------------------------------------------------

// TestInteractAskCreatesRequestAndEmitsEvent verifies interact_ask (dispatch
// M6d-2 §3): a valid prompt creates a pending request through the Engine,
// returns its id + current status, and lands an interact/request event (D3)
// carrying the request id and the triggering tool name.
func TestInteractAskCreatesRequestAndEmitsEvent(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	its := NewInteractTools(e, collectEvents(&recs))

	res, err := its.Ask().Execute(context.Background(), json.RawMessage(`{"prompt":"ok to run the report?"}`))
	if err != nil {
		t.Fatalf("interact_ask: %v", err)
	}
	if !strings.Contains(res, "req-1") || !strings.Contains(res, string(StatusPending)) {
		t.Fatalf("interact_ask output = %q, want it to carry req-1 + pending status", res)
	}
	if got := countEventType(recs, session.EventInteractRequest); got != 1 {
		t.Fatalf("interact/request events = %d, want 1 (events: %+v)", got, recs)
	}
	var ir struct {
		ID       string `json:"id"`
		ToolName string `json:"toolName"`
	}
	if err := json.Unmarshal([]byte(recs[0].raw), &ir); err != nil {
		t.Fatalf("unmarshal interact/request payload: %v", err)
	}
	if ir.ID != "req-1" || ir.ToolName != ToolAskName {
		t.Fatalf("interact/request payload = %+v, want req-1 / interact_ask", ir)
	}
	// The request is actually pending in the engine.
	all, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].Status != StatusPending || all[0].Prompt != "ok to run the report?" {
		t.Fatalf("engine table = %+v, want one pending request with the prompt", all)
	}
}

func TestInteractAskStructuredQuestionsRoundTrip(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	its := NewInteractTools(e, nil)
	input := `{"prompt":"Choose the release track","questions":[{"id":"track","question":"Which track?","detail":"Pick one","options":[{"label":"stable","description":"Production"},{"label":"canary"}]}]}`
	if _, err := its.Ask().Execute(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatalf("structured interact_ask: %v", err)
	}
	items, err := e.List(context.Background())
	if err != nil || len(items) != 1 || len(items[0].Questions) != 1 {
		t.Fatalf("structured request = %+v, err=%v", items, err)
	}
	if items[0].Questions[0].Options[0].Label != "stable" {
		t.Fatalf("structured options = %+v", items[0].Questions[0].Options)
	}
	resolver, ok := any(e).(AnswerResolver)
	if !ok {
		t.Fatal("engine does not expose structured answer resolver")
	}
	answer := `{"answers":[{"id":"track","selected":["stable"]}]}`
	if _, err := resolver.ResolveWithAnswer(context.Background(), items[0].ID, StatusApproved, `{"answers":[{"id":"track","selected":["unknown"]}]}`); err == nil {
		t.Fatal("invalid structured option must be rejected")
	}
	if _, err := resolver.ResolveWithAnswer(context.Background(), items[0].ID, StatusApproved, answer); err != nil {
		t.Fatalf("ResolveWithAnswer: %v", err)
	}
	resolved, err := e.Await(context.Background(), items[0].ID)
	if err != nil || resolved.Answer != answer {
		t.Fatalf("resolved answer = %q, err=%v", resolved.Answer, err)
	}
}

func TestAskUserQuestionBlocksAndCanBeCancelled(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	its := NewInteractTools(e, collectEvents(&recs))
	result := make(chan error, 1)
	go func() {
		_, err := its.AskUserQuestion().Execute(context.Background(), json.RawMessage(`{"questions":[{"id":"mode","question":"Mode?","options":[{"label":"safe"}]}]}`))
		result <- err
	}()
	var req Request
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := e.List(context.Background())
		if len(items) == 1 {
			req = items[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.ID == "" {
		t.Fatal("ask_user_question did not create a request")
	}
	if _, err := e.ResolveWithAnswer(context.Background(), req.ID, StatusApproved, `{"answers":[{"id":"mode","selected":["safe"]}]}`); err != nil {
		t.Fatalf("resolve question: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("ask_user_question result: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result = make(chan error, 1)
	go func() {
		_, err := its.AskUserQuestion().Execute(ctx, json.RawMessage(`{"questions":[{"id":"cancel","question":"Cancel me"}]}`))
		result <- err
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := e.List(context.Background())
		if len(items) == 2 && items[1].Status == StatusPending {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; err == nil {
		t.Fatal("cancelled ask_user_question must return an error")
	} else if err.Error() != "ask_user_question was aborted before the user answered" {
		t.Fatalf("aborted ask_user_question error = %q, want DSH abort text", err)
	}
	items, _ := e.List(context.Background())
	if len(items) != 2 || items[1].Status != StatusCanceled {
		t.Fatalf("cancelled request = %+v", items)
	}
	if countEventType(recs, session.EventInteractCancel) != 1 {
		t.Fatalf("cancel event count = %d, want 1", countEventType(recs, session.EventInteractCancel))
	}
}

func TestAskUserQuestionUserCancellationUsesDSHCancelText(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	its := NewInteractTools(e, nil)
	result := make(chan error, 1)
	go func() {
		_, err := its.AskUserQuestion().Execute(context.Background(), json.RawMessage(`{"questions":[{"id":"cancel","question":"Cancel me"}]}`))
		result <- err
	}()

	var req Request
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := e.List(context.Background())
		if len(items) == 1 {
			req = items[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.ID == "" {
		t.Fatal("ask_user_question did not create a request")
	}
	if _, err := e.Cancel(context.Background(), req.ID); err != nil {
		t.Fatalf("cancel question: %v", err)
	}
	if err := <-result; err == nil || err.Error() != "the user cancelled ask_user_question" {
		t.Fatalf("user-cancelled ask_user_question error = %v, want DSH cancel text", err)
	}
}

func TestAskUserQuestionTimeoutUsesDSHAbortAndCancelsRequest(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	its := NewInteractTools(e, collectEvents(&recs))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := its.AskUserQuestion().Execute(ctx, json.RawMessage(`{"questions":[{"id":"timeout","question":"Wait?"}]}`))
	if err == nil || err.Error() != "ask_user_question was aborted before the user answered" {
		t.Fatalf("timed-out ask_user_question error = %v, want DSH abort text", err)
	}
	items, listErr := e.List(context.Background())
	if listErr != nil || len(items) != 1 || items[0].Status != StatusCanceled {
		t.Fatalf("timed-out request = %+v, err=%v, want cancelled request", items, listErr)
	}
	if countEventType(recs, session.EventInteractCancel) != 1 {
		t.Fatalf("timeout cancel event count = %d, want 1", countEventType(recs, session.EventInteractCancel))
	}
}

func TestAskUserQuestionProjectsDSHAnswerAndIgnoresExtraQuestionFields(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	its := NewInteractTools(e, nil)
	result := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		output, err := its.AskUserQuestion().Execute(context.Background(), json.RawMessage(`{"questions":[{"id":"mode","question":"Mode?","detail":"not a DSH field","options":[{"label":"safe"}],"future":"ignored"}]}`))
		result <- struct {
			output string
			err    error
		}{output, err}
	}()

	var req Request
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := e.List(context.Background())
		if len(items) == 1 {
			req = items[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.ID == "" {
		t.Fatal("ask_user_question did not create a request")
	}
	if req.Questions[0].Detail != "" {
		t.Fatalf("DSH-only model projection leaked detail = %q", req.Questions[0].Detail)
	}
	if _, err := e.ResolveWithAnswer(context.Background(), req.ID, StatusApproved, `{"answers":[{"id":"mode","selected":["safe"],"custom":"","future":"ignored"}]}`); err != nil {
		t.Fatalf("resolve question: %v", err)
	}
	got := <-result
	if got.err != nil {
		t.Fatalf("ask_user_question: %v", got.err)
	}
	if got.output != `{"answers":[{"id":"mode","selected":["safe"],"custom":""}]}` {
		t.Fatalf("answer projection = %s, want DSH fields only", got.output)
	}
}

func TestAskUserQuestionRegistryPreservesStructuredOutput(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	its := NewInteractTools(e, nil)
	reg := tools.New()
	reg.SetPolicy(tools.Policy{Enabled: []string{ToolAskUserQuestionName}})
	if err := reg.Register(its.AskUserQuestion()); err != nil {
		t.Fatalf("register ask_user_question: %v", err)
	}
	result := make(chan tools.ToolResult, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := reg.Execute(context.Background(), ToolAskUserQuestionName, json.RawMessage(`{"questions":[{"id":"mode","question":"Mode?"}]}`))
		result <- got
		errs <- err
	}()
	var req Request
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		items, _ := e.List(context.Background())
		if len(items) == 1 {
			req = items[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.ID == "" {
		t.Fatal("ask_user_question did not create a request")
	}
	if _, err := e.ResolveWithAnswer(context.Background(), req.ID, StatusApproved, `{"answers":[{"id":"mode","selected":[],"custom":"safe"}]}`); err != nil {
		t.Fatalf("resolve question: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("registry ask_user_question: %v", err)
	}
	got := <-result
	if got.IsError {
		t.Fatalf("registry ask_user_question returned error result: %+v", got)
	}
	value, ok := got.Value.(map[string]any)
	if !ok || value["answers"] == nil {
		t.Fatalf("registry ask_user_question value = %#v, want structured object", got.Value)
	}
}

// TestInteractAskRejectsEmptyPrompt verifies the repeated D7 guard: a blank
// prompt is rejected even when a direct call bypasses the registry gate.
func TestInteractAskRejectsEmptyPrompt(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	its := NewInteractTools(e, collectEvents(&recs))
	if _, err := its.Ask().Execute(context.Background(), json.RawMessage(`{"prompt":"   "}`)); err == nil {
		t.Fatal("interact_ask with a blank prompt must error")
	}
	if got := countEventType(recs, session.EventInteractRequest); got != 0 {
		t.Fatalf("interact/request events = %d, want 0 after a rejected prompt", got)
	}
}

// --- interact_status ----------------------------------------------------------

// TestInteractStatusReportsAndEmitsEvent verifies interact_status (dispatch
// M6d-2 §3): for a known request it returns the current status and lands an
// interact/status event (D3); for an unknown id it errors and emits nothing.
func TestInteractStatusReportsAndEmitsEvent(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	its := NewInteractTools(e, collectEvents(&recs))

	req, err := e.Request(context.Background(), "allow delete", "delete_file", `{}`)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := e.Resolve(context.Background(), req.ID, StatusApproved); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	res, err := its.Status().Execute(context.Background(), json.RawMessage(`{"id":"`+req.ID+`"}`))
	if err != nil {
		t.Fatalf("interact_status: %v", err)
	}
	if !strings.Contains(res, req.ID) || !strings.Contains(res, string(StatusApproved)) {
		t.Fatalf("interact_status output = %q, want it to carry %s + approved", res, req.ID)
	}
	if got := countEventType(recs, session.EventInteractStatus); got != 1 {
		t.Fatalf("interact/status events = %d, want 1", got)
	}
	var ist struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(recs[0].raw), &ist); err != nil {
		t.Fatalf("unmarshal interact/status payload: %v", err)
	}
	if ist.ID != req.ID || ist.Status != string(StatusApproved) {
		t.Fatalf("interact/status payload = %+v, want %s approved", ist, req.ID)
	}

	// Unknown id: error, no event.
	if _, err := its.Status().Execute(context.Background(), json.RawMessage(`{"id":"req-missing"}`)); err == nil {
		t.Fatal("interact_status with an unknown id must error")
	}
	if got := countEventType(recs, session.EventInteractStatus); got != 1 {
		t.Fatalf("interact/status events = %d, want still 1 after an unknown lookup", got)
	}
}
