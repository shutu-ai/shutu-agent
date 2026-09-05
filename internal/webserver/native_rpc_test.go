package webserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

func nativeResponse(t *testing.T, recBody []byte) nativeRPCResponse {
	t.Helper()
	var response nativeRPCResponse
	if err := json.Unmarshal(recBody, &response); err != nil {
		t.Fatalf("decode native response: %v; body=%s", err, recBody)
	}
	return response
}

type nativeRawHistoryStore struct {
	store.Store
	events []session.Event
}

func (s nativeRawHistoryStore) LoadSessionRaw(context.Context, string) ([]session.Event, error) {
	return s.events, nil
}

func TestNativeCommandsUseDSHDescriptorsAndGeneratedArgumentShape(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotSession, gotLine string
	srv.SetNativeCommandManager(nativeCommandTestManager{
		list: []NativeCommand{{Name: "help", Description: "show help", InputHint: "optional topic"}},
		execute: func(_ context.Context, sessionID, line string, _ []llm.ImageRef) (NativeCommandExecution, bool, error) {
			gotSession, gotLine = sessionID, line
			return NativeCommandExecution{CommandID: "cmd-1", Result: NativeCommandResult{Kind: "success", Text: "ok"}}, true, nil
		},
	})
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"%s","method":"%s","payload":%s}`, method, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}
	list := call(t, "commands/list", `{"args":{"agentId":"s1"}}`)
	if !list.Result.OK {
		t.Fatalf("commands/list response = %+v", list)
	}
	items, ok := list.Result.Value.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("commands/list value = %#v", list.Result.Value)
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["name"] != "help" || item["description"] != "show help" {
		t.Fatalf("commands/list item = %#v", items[0])
	}
	response := call(t, "commands/execute", `{"args":{"agentId":"s1","line":"/help"}}`)
	if !response.Result.OK {
		t.Fatalf("commands/execute response = %+v", response)
	} else if gotSession != "s1" || gotLine != "/help" {
		t.Fatalf("commands/execute callback=(%q,%q)", gotSession, gotLine)
	}
	execution, ok := response.Result.Value.(map[string]any)
	if !ok || execution["commandId"] != "cmd-1" {
		t.Fatalf("commands/execute value = %#v", response.Result.Value)
	}
}

func TestNativeCommandsAcceptCompactArrayArgumentsAndOmitUnknownValue(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetNativeCommandManager(nativeCommandTestManager{
		execute: func(_ context.Context, sessionID, line string, _ []llm.ImageRef) (NativeCommandExecution, bool, error) {
			if sessionID != "s1" || line != "/status" {
				t.Fatalf("compact command args=(%q,%q)", sessionID, line)
			}
			return NativeCommandExecution{}, false, nil
		},
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/commands/execute", "tok", `{"type":"client-request","rpcId":"array","method":"commands/execute","payload":{"args":["s1","/status",[]]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || response.Result.Value != nil {
		t.Fatalf("unknown command response = %+v", response)
	}
}

func TestNativeRespondBridgesDSHApprovalAndQuestionResponses(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotSession, gotID, gotAnswer string
	var gotStatus interact.ApprovalStatus
	srv.SetInteractionManager(
		func(context.Context, string) ([]interact.Request, error) { return nil, nil },
		func(_ context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error {
			gotSession, gotID, gotStatus, gotAnswer = sessionID, id, status, answer
			return nil
		},
	)
	call := func(t *testing.T, body string) nativeRPCReceipt {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/respond", "tok", body)
		var receipt nativeRPCReceipt
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
			t.Fatalf("decode respond receipt: %v; body=%s", err, rec.Body.Bytes())
		}
		return receipt
	}
	if receipt := call(t, `{"type":"client-response","rpcId":"req-1","result":{"ok":true,"value":{"sessionId":"s1","approvalId":"req-1","outcome":"allowed-once"}}}`); !receipt.Accepted {
		t.Fatalf("approval receipt = %+v", receipt)
	}
	if gotSession != "s1" || gotID != "req-1" || gotStatus != interact.StatusApproved || gotAnswer != "" {
		t.Fatalf("approval callback = %q/%q/%q/%q", gotSession, gotID, gotStatus, gotAnswer)
	}
	if receipt := call(t, `{"type":"client-response","rpcId":"req-2","result":{"ok":true,"value":{"sessionId":"s2","answer":{"answers":[{"id":"mode","selected":["safe"]}]}}}}`); !receipt.Accepted {
		t.Fatalf("question receipt = %+v", receipt)
	}
	if gotSession != "s2" || gotID != "req-2" || gotStatus != interact.StatusApproved || !strings.Contains(gotAnswer, `"selected":["safe"]`) {
		t.Fatalf("question callback = %q/%q/%q/%q", gotSession, gotID, gotStatus, gotAnswer)
	}
	if receipt := call(t, `{"type":"client-response","rpcId":"req-3","result":{"ok":true,"value":{"sessionId":"s3","approvalId":"other","outcome":"rejected"}}}`); receipt.Accepted || receipt.Reason != "bad-response" {
		t.Fatalf("mismatched approval receipt = %+v", receipt)
	}
	if receipt := call(t, `{"type":"client-response","rpcId":"req-4","result":{"ok":false,"error":{"code":"cancelled","message":"closed"}}}`); receipt.Accepted || receipt.Reason != "not-pending" {
		t.Fatalf("failed response receipt = %+v", receipt)
	}
}

func TestNativeRespondBridgesDSHQuestionCancellation(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotSession, gotID, gotAnswer string
	var gotStatus interact.ApprovalStatus
	srv.SetInteractionManager(
		func(context.Context, string) ([]interact.Request, error) { return nil, nil },
		func(_ context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error {
			gotSession, gotID, gotStatus, gotAnswer = sessionID, id, status, answer
			return nil
		},
	)
	// DSH's cancellation envelope has no value/sessionId. The native mux
	// correlation is the only source for the owning session at this endpoint.
	srv.rememberNativeInteraction("question-cancel", "session-1")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/respond", "tok", `{"type":"client-response","rpcId":"question-cancel","result":{"ok":false,"error":{"code":"cancelled","message":"the user closed this question request","details":{}}}}`)
	var receipt nativeRPCReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode cancellation receipt: %v; body=%s", err, rec.Body.Bytes())
	}
	if !receipt.Accepted || gotSession != "session-1" || gotID != "question-cancel" || gotStatus != interact.StatusCanceled || gotAnswer != "" {
		t.Fatalf("question cancellation = receipt=%+v callback=%q/%q/%q/%q", receipt, gotSession, gotID, gotStatus, gotAnswer)
	}
	if got := srv.nativeInteractionSession("question-cancel"); got != "" {
		t.Fatalf("resolved native interaction correlation = %q, want removed", got)
	}
}

func TestNativeRespondRecoversQuestionOwnerAfterReconnect(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotSession, gotID string
	srv.SetInteractionManager(
		func(context.Context, string) ([]interact.Request, error) { return nil, nil },
		func(_ context.Context, sessionID, id string, status interact.ApprovalStatus, _ string) error {
			if status != interact.StatusCanceled {
				t.Fatalf("status = %q, want cancelled", status)
			}
			gotSession, gotID = sessionID, id
			return nil
		},
	)
	srv.SetInteractionSessionResolver(func(context.Context, string) (string, error) {
		return "cold-session", nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/respond", "tok", `{"type":"client-response","rpcId":"cold-question","result":{"ok":false,"error":{"code":"cancelled","message":"closed"}}}`)
	var receipt nativeRPCReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode cancellation receipt: %v", err)
	}
	if !receipt.Accepted || gotSession != "cold-session" || gotID != "cold-question" {
		t.Fatalf("reconnected question cancellation = receipt=%+v callback=%q/%q", receipt, gotSession, gotID)
	}
}

func TestNativePendingInteractionFrameUsesDSHQuestionShape(t *testing.T) {
	method, raw, id, kind, ok := nativePendingInteractionFrame("s1", interact.Request{
		ID: "q-1", Questions: []interact.Question{{
			ID: "mode", Header: "Mode", Question: "Choose?", MultiSelect: true,
			Options: []interact.QuestionOption{{Label: "safe", Description: "No side effects"}},
		}},
	})
	if !ok || method != "question/requested" || id != "q-1" || kind != "question" {
		t.Fatalf("question frame metadata = %q/%q/%q/%v", method, id, kind, ok)
	}
	value, ok := raw.(map[string]any)
	if !ok || value["sessionId"] != "s1" {
		t.Fatalf("question frame = %#v", raw)
	}
	questions, ok := value["questions"].([]map[string]any)
	if !ok || len(questions) != 1 || questions[0]["multiSelect"] != true || questions[0]["multi_select"] != nil {
		t.Fatalf("question shape = %#v", value["questions"])
	}
	if _, present := questions[0]["options"].([]map[string]any)[0]["description"]; !present {
		t.Fatal("question option description should be retained when present")
	}
}

func TestNativeInteractionFrameProjectsCanonicalApprovalEvents(t *testing.T) {
	method, raw, id, kind, ok := nativeInteractionFrame("s1", session.Event{
		Type: session.EventApprovalAsked,
		Data: json.RawMessage(`{"id":"approval-1","toolName":"bash","reason":"run it","questions":[{"id":"confirm","question":"Proceed?"}]}`),
	})
	if !ok || method != "question/requested" || id != "approval-1" || kind != "question" {
		t.Fatalf("canonical asked frame = %q/%q/%q/%v", method, id, kind, ok)
	}
	if raw.(map[string]any)["sessionId"] != "s1" {
		t.Fatalf("canonical asked payload = %#v", raw)
	}
	method, raw, id, kind, ok = nativeInteractionFrame("s1", session.Event{
		Type: session.EventApprovalDecided,
		Data: json.RawMessage(`{"id":"approval-1","outcome":"allowed-once"}`),
	})
	if !ok || method != "approval/resolved" || id != "approval-1" || kind != "" {
		t.Fatalf("canonical decided frame = %q/%q/%q/%v", method, id, kind, ok)
	}
	if raw.(map[string]any)["outcome"] != "allowed-once" {
		t.Fatalf("canonical decided payload = %#v", raw)
	}
}

func TestNativeQueueAndJobsFramesUseDSHShapes(t *testing.T) {
	queue := nativeQueueFrame("s1", []QueueItem{
		{ID: "q-1", Text: "queued prompt", Placement: "queued"},
		{ID: "q-2", Text: "steered prompt", Placement: "steering"},
		{ID: "", Text: "discarded"},
	})
	if queue["type"] != "session/queue" || queue["sessionId"] != "s1" {
		t.Fatalf("queue frame metadata = %#v", queue)
	}
	items, ok := queue["items"].([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("queue frame items = %#v", queue["items"])
	}
	message, ok := items[0]["message"].(map[string]any)
	if !ok || message["role"] != "user" || message["id"] != "q-1" {
		t.Fatalf("queue message = %#v", items[0]["message"])
	}
	source := message["source"].(map[string]any)
	if source["kind"] != "user" || source["rpcId"] != "q-1" {
		t.Fatalf("queue source = %#v", source)
	}

	started := time.UnixMilli(1234).UTC()
	finished := time.UnixMilli(5678).UTC()
	jobs := nativeJobViews([]map[string]any{{
		"id": "job-1", "kind": "bash", "label": "build", "status": "running",
		"started_at": started, "finished_at": &finished, "detail": "done",
	}})
	if len(jobs) != 1 || jobs[0]["startedAt"] != int64(1234) || jobs[0]["finishedAt"] != int64(5678) {
		t.Fatalf("native jobs = %#v", jobs)
	}
	if _, snake := jobs[0]["started_at"]; snake {
		t.Fatalf("native jobs retained snake_case timestamp: %#v", jobs[0])
	}
}

func TestNativeSessionCancelIsIdempotentWhenNoTurnIsRunning(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "cancel-idempotent", nil)
	srv.SetTurnStopper(func(string) error { return errors.New("no turn running") })
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/session.cancel", "tok", `{
		"type":"client-request","rpcId":"cancel-1","method":"session.cancel",
		"payload":{"sessionId":"cancel-idempotent"}
	}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || !response.Result.OK || response.Result.Value == nil {
		t.Fatalf("idempotent cancel = %d %+v", rec.Code, response)
	}

	seedSession(t, st, "cancel-child", []session.Event{{
		Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{"parentSession":"cancel-parent","depth":1}`),
	}})
	if err := st.SetSessionHeader(context.Background(), "cancel-child", store.SessionHeader{
		ID: "cancel-child", Origin: "subagent", DelegationDepth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	rec = doReqBody(t, srv.Handler(), http.MethodPost, "/api/session.cancel", "tok", `{
		"type":"client-request","rpcId":"cancel-child","method":"session.cancel",
		"payload":{"sessionId":"cancel-child"}
	}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "agent-busy" ||
		response.Result.Error.Details["reason"] != "use subagent delivery for this child session" {
		t.Fatalf("subagent ownership cancel = %+v", response)
	}

	rec = doReqBody(t, srv.Handler(), http.MethodPost, "/api/session.cancel", "tok", `{
		"type":"client-request","rpcId":"cancel-2","method":"session.cancel",
		"payload":{"sessionId":"missing-session"}
	}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error.Code != "session-not-found" {
		t.Fatalf("missing cancel = %s", rec.Body.String())
	}
}

type cancelMetadataGateStore struct {
	store.Store
	metadataStarted chan struct{}
	releaseMetadata chan struct{}
}

func (s *cancelMetadataGateStore) GetSessionMeta(ctx context.Context, sessionID string) (store.SessionMeta, error) {
	close(s.metadataStarted)
	<-s.releaseMetadata
	return s.Store.GetSessionMeta(ctx, sessionID)
}

func TestNativeSessionCancelStopsBeforeMetadataRead(t *testing.T) {
	base, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	seedSession(t, base, "cancel-fast", nil)
	wrapped := &cancelMetadataGateStore{
		Store:           base,
		metadataStarted: make(chan struct{}),
		releaseMetadata: make(chan struct{}),
	}
	srv, err := New(wrapped, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	stopCalled := make(chan struct{})
	srv.SetTurnStopper(func(string) error {
		close(stopCalled)
		return nil
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReqBody(t, srv.Handler(), http.MethodPost, "/api/session.cancel", "tok", `{
			"type":"client-request","rpcId":"cancel-fast","method":"session.cancel",
			"payload":{"sessionId":"cancel-fast"}
		}`)
	}()
	select {
	case <-stopCalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel did not reach the in-memory stopper")
	}
	select {
	case rec := <-done:
		response := nativeResponse(t, rec.Body.Bytes())
		if rec.Code != http.StatusOK || !response.Result.OK {
			t.Fatalf("fast cancel = %d %+v", rec.Code, response)
		}
	case <-time.After(500 * time.Millisecond):
		close(wrapped.releaseMetadata)
		t.Fatal("cancel waited for metadata after the stopper accepted it")
	}
	select {
	case <-wrapped.metadataStarted:
		t.Fatal("cancel read metadata after the stopper accepted it")
	default:
	}
}

func TestNativeGenericSessionRoutesFenceSubagentOwnedSessions(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	const sessionID = "subagent-owned"
	seedSession(t, st, sessionID, []session.Event{{
		Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{"parentSession":"parent-session","depth":1}`),
	}})
	if err := st.SetSessionHeader(context.Background(), sessionID, store.SessionHeader{
		ID:              sessionID,
		Origin:          "subagent",
		Parent:          "parent-session",
		DelegationDepth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var callbackCalls atomic.Int64
	callback := func() { callbackCalls.Add(1) }
	srv.SetNativeSessionRenamer(func(context.Context, string, string) (int64, error) {
		callback()
		return 0, nil
	})
	srv.SetQueueManager(
		func(context.Context, string) ([]QueueItem, error) {
			callback()
			return nil, nil
		},
		func(context.Context, string, string, []llm.ContentBlock, PromptMeta) (QueueItem, error) {
			callback()
			return QueueItem{}, nil
		},
		func(context.Context, string, string, string) error {
			callback()
			return nil
		},
	)
	srv.SetSessionModelValidator(func(context.Context, string, string, string, string) error {
		callback()
		return nil
	})
	srv.SetNativeQueueUpdater(func(context.Context, string, string, string, string) error {
		callback()
		return nil
	})
	srv.SetNativeGoalManager(func(context.Context, NativeGoalMutation) (NativeGoalMutationResult, error) {
		callback()
		return NativeGoalMutationResult{}, nil
	})
	srv.SetTurnStopper(func(string) error {
		callback()
		return nil
	})

	cases := []struct {
		name    string
		method  string
		payload string
	}{
		{name: "rename", method: "session.rename", payload: `{"sessionId":"subagent-owned","title":"renamed"}`},
		{name: "prompt", method: "session.prompt", payload: `{"sessionId":"subagent-owned","mode":"queue","content":[{"type":"text","text":"blocked"}]}`},
		{name: "models", method: "session.models", payload: `{"sessionId":"subagent-owned"}`},
		{name: "select-model", method: "session.selectModel", payload: `{"sessionId":"subagent-owned","provider":"provider","model":"model"}`},
		{name: "update-queue", method: "session.updateQueue", payload: `{"sessionId":"subagent-owned","itemId":"item-1","action":{"kind":"remove"}}`},
		{name: "goal", method: "goal.create", payload: `{"sessionId":"subagent-owned","objective":"blocked"}`},
		{name: "agent-preset", method: "agentPreset.select", payload: `{"sessionId":"subagent-owned","agentPreset":"minimal"}`},
		{name: "cancel", method: "session.cancel", payload: `{"sessionId":"subagent-owned"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"type":"client-request","rpcId":"%s","method":"%s","payload":%s}`,
				strings.ReplaceAll(test.name, " ", "-"), test.method, test.payload)
			rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/"+test.method, "tok", body)
			response := nativeResponse(t, rec.Body.Bytes())
			if response.Result.OK || response.Result.Error == nil ||
				response.Result.Error.Code != "agent-busy" ||
				response.Result.Error.Message != `session "subagent-owned" is owned by subagent routing` ||
				response.Result.Error.Details["reason"] != "use subagent delivery for this child session" {
				t.Fatalf("%s response = %+v", test.name, response)
			}
		})
	}
	if callbackCalls.Load() != 0 {
		t.Fatalf("ownership gate leaked %d callback calls", callbackCalls.Load())
	}
}

type nativeCommandTestManager struct {
	list    []NativeCommand
	execute func(context.Context, string, string, []llm.ImageRef) (NativeCommandExecution, bool, error)
}

func (m nativeCommandTestManager) List(context.Context, string) ([]NativeCommand, error) {
	return m.list, nil
}

func (m nativeCommandTestManager) Execute(ctx context.Context, sessionID, line string, images []llm.ImageRef) (NativeCommandExecution, bool, error) {
	return m.execute(ctx, sessionID, line, images)
}

func TestNativeGoalMutationsUseDSHShapesAndForwardOptionalFields(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var calls []NativeGoalMutation
	srv.SetNativeGoalManager(func(_ context.Context, mutation NativeGoalMutation) (NativeGoalMutationResult, error) {
		calls = append(calls, mutation)
		if mutation.Action == "goal.clear" {
			return NativeGoalMutationResult{Cleared: true}, nil
		}
		return NativeGoalMutationResult{GoalID: "goal-1", Revision: len(calls)}, nil
	})

	request := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"%s","method":"%s","payload":%s}`, method, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}

	if response := request(t, "goal.create", `{"sessionId":"s1","objective":"ship the native goal","maxGoalRounds":7}`); !response.Result.OK {
		t.Fatalf("goal.create response = %+v", response)
	}
	if response := request(t, "goal.edit", `{"sessionId":"s1","ref":{"id":"goal-1","revision":1},"maxGoalRounds":9}`); !response.Result.OK {
		t.Fatalf("goal.edit response = %+v", response)
	}
	for _, method := range []string{"goal.pause", "goal.resume", "goal.complete"} {
		if response := request(t, method, fmt.Sprintf(`{"sessionId":"s1","ref":{"id":"goal-1","revision":2}}`)); !response.Result.OK {
			t.Fatalf("%s response = %+v", method, response)
		}
	}
	if response := request(t, "goal.clear", `{"sessionId":"s1","ref":{"id":"goal-1","revision":4}}`); !response.Result.OK {
		t.Fatalf("goal.clear response = %+v", response)
	}
	if len(calls) != 6 || calls[0].Action != "goal.create" || calls[0].Objective == nil || *calls[0].Objective != "ship the native goal" || calls[0].MaxGoalRounds == nil || *calls[0].MaxGoalRounds != 7 {
		t.Fatalf("goal.create callback = %+v", calls)
	}
	if calls[1].Action != "goal.edit" || calls[1].GoalID != "goal-1" || calls[1].Revision != 1 || calls[1].Objective != nil || calls[1].MaxGoalRounds == nil || *calls[1].MaxGoalRounds != 9 {
		t.Fatalf("goal.edit callback = %+v", calls[1])
	}
}

func TestNativeGoalMutationsRejectMalformedReferencesAndEdits(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetNativeGoalManager(func(context.Context, NativeGoalMutation) (NativeGoalMutationResult, error) {
		t.Fatal("goal callback should not run for malformed requests")
		return NativeGoalMutationResult{}, nil
	})
	request := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"bad","method":"%s","payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}
	for _, test := range []struct{ method, payload string }{
		{"goal.create", `{"sessionId":"s1"}`},
		{"goal.edit", `{"sessionId":"s1","ref":{"id":"g","revision":1}}`},
		{"goal.pause", `{"sessionId":"s1","ref":{"id":"g","revision":0}}`},
	} {
		response := request(t, test.method, test.payload)
		if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
			t.Fatalf("%s malformed response = %+v", test.method, response)
		}
	}
}

func TestNativeCredentialMutationsValidateRefsAndNeverReturnValues(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var setRef, setValue, unsetRef string
	srv.SetNativeCredentialManager(func(_ context.Context, ref, value string) error {
		setRef, setValue = ref, value
		return nil
	}, func(_ context.Context, ref string) error {
		unsetRef = ref
		return nil
	})
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"credential","method":"%s","payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}
	if response := call(t, "credentials.set", `{"ref":"TEST_API_KEY","value":"secret-value"}`); !response.Result.OK || setRef != "TEST_API_KEY" || setValue != "secret-value" {
		t.Fatalf("credentials.set response=%+v callback=(%q,%q)", response, setRef, setValue)
	}
	if response := call(t, "credentials.unset", `{"ref":"TEST_API_KEY"}`); !response.Result.OK || unsetRef != "TEST_API_KEY" {
		t.Fatalf("credentials.unset response=%+v callback=%q", response, unsetRef)
	}
	if response := call(t, "credentials.set", `{"ref":"bad-name","value":"secret"}`); response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("invalid credentials.set response=%+v", response)
	}
	if response := call(t, "credentials.set", `{"ref":"TEST_API_KEY","value":""}`); response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("empty credentials.set response=%+v", response)
	}
}

func TestNativeHostOpenPathRejectsMissingPath(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/host.openPath", "tok", `{"type":"client-request","rpcId":"open","method":"host.openPath","payload":{"path":"C:\\does-not-exist-shutu-native"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-unreadable" {
		t.Fatalf("host.openPath response=%+v", response)
	}
}

func TestNativeGoalsRemoteNamespaceUnwrapsDSHArguments(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var got NativeGoalMutation
	srv.SetNativeGoalManager(func(_ context.Context, mutation NativeGoalMutation) (NativeGoalMutationResult, error) {
		got = mutation
		return NativeGoalMutationResult{GoalID: "goal-remote", Revision: 3}, nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/goals/create", "tok", `{"type":"client-request","rpcId":"goals","method":"goals/create","payload":{"args":["s-remote",{"objective":"ship it","maxGoalRounds":5}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || got.Action != "goal.create" || got.SessionID != "s-remote" || got.Objective == nil || *got.Objective != "ship it" || got.MaxGoalRounds == nil || *got.MaxGoalRounds != 5 {
		t.Fatalf("goals/create response=%+v mutation=%+v", response, got)
	}
	var value struct {
		Ref struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
		} `json:"ref"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil || value.Ref.ID != "goal-remote" || value.Ref.Revision != 3 {
		t.Fatalf("goals/create value=%s", encoded)
	}
}

func TestNativeRPCSessionHistoryAndPrompt(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-session", []session.Event{{
		Seq: 0, Type: session.EventUserMessage, At: time.UnixMilli(1234), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"hello native"}`),
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"list-1","method":"session.list","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || response.RPCID != "list-1" {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].SessionID != "native-session" || !list.Items[0].Blank || list.Items[0].Projections == nil {
		t.Fatalf("session.list items = %+v", list.Items)
	}
	metadata, ok := list.Items[0].Projections.Values["sessionListMetadata"].(map[string]any)
	if !ok || metadata["blank"] != true || metadata["lastPromptAt"] != float64(1234) {
		t.Fatalf("session.list projection metadata = %#v", list.Items[0].Projections.Values["sessionListMetadata"])
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"history-1","method":"session.history","payload":{"sessionId":"native-session"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	var history struct {
		Header      nativeSessionHeader   `json:"header"`
		Events      []nativeHistoryEntry  `json:"events"`
		Projections nativeProjectionBlock `json:"projections"`
	}
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Header.Version != 0 || history.Header.ID != "native-session" || history.Header.CreatedAt == 0 || len(history.Events) != 1 || history.Events[0].Event.Time != 1234 || history.Events[0].Event.Type != session.EventUserMessage {
		t.Fatalf("session.history events = %+v", history.Events)
	}
	if history.Projections.AsOfSeq != 0 {
		t.Fatalf("history projection asOfSeq = %d, want 0", history.Projections.AsOfSeq)
	}
	var userData struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(history.Events[0].Event.Data, &userData); err != nil {
		t.Fatal(err)
	}
	if userData.ID == "" || userData.Role != "user" || userData.Source.Kind != "user" || len(userData.Content) != 1 || userData.Content[0].Text != "hello native" {
		t.Fatalf("native user message = %+v", userData)
	}

	gotPrompt := make(chan string, 1)
	srv.SetMessageHandler(func(_ context.Context, sessionID, text string, _ []llm.ImageRef, _ PromptMeta) error {
		gotPrompt <- sessionID + ":" + text
		return nil
	})
	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"prompt-1","method":"session.prompt","payload":{"sessionId":"native-session","mode":"queue","content":[{"type":"text","text":"send me"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.prompt response=%+v", response)
	}
	select {
	case got := <-gotPrompt:
		if got != "native-session:send me" {
			t.Fatalf("session.prompt callback=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("session.prompt was not dispatched after admission")
	}
}

func TestNativeHistoryRejectsUnknownDurableEventsFromRawStore(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-unknown", time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	srv.store = nativeRawHistoryStore{
		Store: st,
		events: []session.Event{{
			Seq: 1, Type: "future/required-event", At: time.UnixMilli(1001), Version: session.EventVersion,
			Data: json.RawMessage(`{"value":true}`),
		}},
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"unknown-history","method":"session.history","payload":{"sessionId":"native-unknown"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "internal" {
		t.Fatalf("unknown raw event response = %+v", response)
	}
	if !strings.Contains(response.Result.Error.Message, "unknown required event") {
		t.Fatalf("unknown raw event error = %q", response.Result.Error.Message)
	}
}

func TestNativeSessionPromptCarriesCanonicalTimeZoneProvenance(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-zone", time.Now()); err != nil {
		t.Fatal(err)
	}
	gotMeta := make(chan PromptMeta, 1)
	srv.SetMessageHandler(func(_ context.Context, _ string, _ string, _ []llm.ImageRef, meta PromptMeta) error {
		gotMeta <- meta
		return nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"zone-request","method":"session.prompt","payload":{"sessionId":"native-zone","mode":"steer","clientTimeZone":"Asia/Shanghai","content":[{"type":"text","text":"schedule this"}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("zone prompt response=%+v", response)
	}
	select {
	case meta := <-gotMeta:
		if meta.RPCID == "" || meta.ClientTimeZone != "Asia/Shanghai" {
			t.Fatalf("prompt meta = %#v", meta)
		}
	case <-time.After(time.Second):
		t.Fatal("zone prompt was not dispatched")
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"bad-zone","method":"session.prompt","payload":{"sessionId":"native-zone","mode":"steer","clientTimeZone":"+08:00","content":[{"type":"text","text":"bad"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "invalid-time-zone" ||
		response.Result.Error.Details["value"] != "+08:00" {
		t.Fatalf("bad zone response=%+v", response)
	}
}

func TestNativeSessionPromptReturnsBeforeTurnCompletes(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-admission", time.Now()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	srv.SetMessageHandler(func(ctx context.Context, _ string, _ string, _ []llm.ImageRef, _ PromptMeta) error {
		close(started)
		if ctx.Done() != nil {
			t.Error("native prompt handler inherited request cancellation")
		}
		<-release
		return nil
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"admission-1","method":"session.prompt","payload":{"sessionId":"native-admission","mode":"steer","content":[{"type":"text","text":"long task"}]}}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native prompt handler did not start")
	}
	select {
	case rec := <-done:
		response := nativeResponse(t, rec.Body.Bytes())
		if !response.Result.OK {
			t.Fatalf("admission response = %+v", response)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("session.prompt waited for the turn to complete")
	}
	close(release)
}

func TestNativeSessionPromptRejectsUnservedProviderBeforeAdmission(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-route", time.Now()); err != nil {
		t.Fatal(err)
	}
	provider := "closed-provider"
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{
			"llm_provider": provider,
			"model":        "closed-model",
			"providers": []any{
				map[string]any{"id": "closed-provider", "configured": false, "available": false},
				map[string]any{"id": "ready-provider", "configured": true, "available": true},
			},
		}
	})
	var admitted atomic.Int64
	srv.SetQueueManager(
		func(context.Context, string) ([]QueueItem, error) { return nil, nil },
		func(context.Context, string, string, []llm.ContentBlock, PromptMeta) (QueueItem, error) {
			admitted.Add(1)
			return QueueItem{}, nil
		},
		func(context.Context, string, string, string) error { return nil },
	)
	call := func() nativeRPCResponse {
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok",
			`{"type":"client-request","rpcId":"route","method":"session.prompt","payload":{"sessionId":"native-route","mode":"queue","content":[{"type":"text","text":"hello"}]}}`)
		return nativeResponse(t, rec.Body.Bytes())
	}
	response := call()
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "model-unavailable" ||
		response.Result.Error.Message != `no adapter serves provider "closed-provider"; select a model for this session` ||
		response.Result.Error.Details["provider"] != "closed-provider" ||
		response.Result.Error.Details["model"] != "closed-model" {
		t.Fatalf("unserved prompt = %+v", response)
	}
	if admitted.Load() != 0 {
		t.Fatal("unserved prompt reached queue admission")
	}

	provider = "ready-provider"
	response = call()
	if !response.Result.OK || response.Result.Value == nil {
		t.Fatalf("served prompt = %+v", response)
	}
	if admitted.Load() != 1 {
		t.Fatalf("served prompt admissions = %d", admitted.Load())
	}
}

func TestNativeSessionPromptQueuesRichImagesAndText(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-prompt", time.Now()); err != nil {
		t.Fatal(err)
	}
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	var directPrompts atomic.Int32
	srv.SetMessageHandler(func(context.Context, string, string, []llm.ImageRef, PromptMeta) error {
		directPrompts.Add(1)
		return nil
	})
	queuedContentCh := make(chan []llm.ContentBlock, 1)
	var queuedItem QueueItem
	srv.SetQueueManager(nil, func(_ context.Context, sessionID, text string, content []llm.ContentBlock, _ PromptMeta) (QueueItem, error) {
		queuedContentCh <- content
		queuedItem = QueueItem{ID: "q-rich", Text: text, Content: content, Placement: "queued"}
		return queuedItem, nil
	}, nil)
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"image-1","method":"session.prompt","payload":{"sessionId":"native-prompt","mode":"queue","content":[{"type":"text","text":"describe this"},{"type":"image","mediaType":"image/png","name":"diagram.png","data":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("image prompt response=%+v", response)
	}
	select {
	case content := <-queuedContentCh:
		if len(content) != 2 || content[0].Kind != llm.BlockText || content[0].Text != "describe this" ||
			content[1].Kind != llm.BlockImage || content[1].Image.MediaType != "image/png" ||
			content[1].Image.Name != "diagram.png" || content[1].Image.Bytes != 68 {
			t.Fatalf("image prompt content=%+v", content)
		}
	case <-time.After(time.Second):
		t.Fatal("image prompt was not queued after admission")
	}
	if directPrompts.Load() != 0 {
		t.Fatalf("queued image prompt bypassed the queue: directPrompts=%d", directPrompts.Load())
	}
	frame := nativeQueueFrame("native-prompt", []QueueItem{queuedItem})
	items, ok := frame["items"].([]map[string]any)
	if !ok || len(items) != 1 {
		t.Fatalf("native queue frame = %#v", frame)
	}
	message, ok := items[0]["message"].(map[string]any)
	if !ok {
		t.Fatalf("native queue message = %#v", items[0])
	}
	content, ok := message["content"].([]llm.ContentBlock)
	if !ok || len(content) != 2 ||
		content[0].Kind != llm.BlockText || content[0].Text != "describe this" ||
		content[1].Kind != llm.BlockImage || content[1].Image.MediaType != "image/png" ||
		content[1].Image.Name != "diagram.png" {
		t.Fatalf("native queue content = %#v", message["content"])
	}

	var queued string
	queuedMeta := make(chan PromptMeta, 1)
	srv.SetQueueManager(nil, func(_ context.Context, sessionID, text string, content []llm.ContentBlock, meta PromptMeta) (QueueItem, error) {
		queued = sessionID + ":" + text
		queuedMeta <- meta
		return QueueItem{ID: "q-native", Text: text}, nil
	}, nil)
	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"queue-1","method":"session.prompt","payload":{"sessionId":"native-prompt","mode":"queue","clientTimeZone":"Asia/Shanghai","content":[{"type":"text","text":"queue this"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || queued != "native-prompt:queue this" {
		t.Fatalf("queue prompt response=%+v queued=%q", response, queued)
	}
	select {
	case meta := <-queuedMeta:
		if meta.RPCID == "" || meta.ClientTimeZone != "Asia/Shanghai" {
			t.Fatalf("queued provenance = %#v", meta)
		}
	case <-time.After(time.Second):
		t.Fatal("queue prompt callback was not invoked")
	}
}

func TestNativeSessionPromptValidatesImageBatchBeforeWriting(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-batch", time.Now()); err != nil {
		t.Fatal(err)
	}
	attachmentDir := filepath.Join(t.TempDir(), "attachments")
	att, err := attachment.NewStore(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	valid := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"batch-1","method":"session.prompt","payload":{"sessionId":"native-batch","mode":"steer","content":[{"type":"text","text":"batch"},{"type":"image","mediaType":"image/png","data":%q},{"type":"image","mediaType":"image/tiff","data":%q}]}}`, "data:image/png;base64,"+valid, valid))
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "attachment-error" || response.Result.Error.Details["reason"] != "UNSUPPORTED_IMAGE_TYPE" {
		t.Fatalf("invalid image batch response = %+v", response)
	}
	entries, err := os.ReadDir(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid native image batch left %d attachment files", len(entries))
	}
}

func TestNativeSessionPromptReportsImageTypeMismatch(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-mismatch", time.Now()); err != nil {
		t.Fatal(err)
	}
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"mismatch-1","method":"session.prompt","payload":{"sessionId":"native-mismatch","mode":"steer","content":[{"type":"text","text":"mismatch"},{"type":"image","mediaType":"image/jpeg","data":%q}]}}`, png))
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "attachment-error" ||
		response.Result.Error.Details["reason"] != "IMAGE_TYPE_MISMATCH" {
		t.Fatalf("image type mismatch response = %+v", response)
	}
}

func TestNativeSessionPromptRequiresCanonicalImageBase64(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-base64", time.Now()); err != nil {
		t.Fatal(err)
	}
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	call := func(id, data string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":%q,"method":"session.prompt","payload":{"sessionId":"native-base64","mode":"steer","content":[{"type":"text","text":"base64"},{"type":"image","mediaType":"image/png","data":%q}]}}`, id, data))
		return nativeResponse(t, rec.Body.Bytes())
	}
	response := call("empty-base64", "")
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "attachment-error" ||
		response.Result.Error.Details["reason"] != "INVALID_IMAGE_BASE64" {
		t.Fatalf("empty image base64 response = %+v", response)
	}
	response = call("noncanonical-base64", "QU\nJD")
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "attachment-error" ||
		response.Result.Error.Details["reason"] != "INVALID_IMAGE_BASE64" {
		t.Fatalf("non-canonical image base64 response = %+v", response)
	}
}

func TestNativeSessionSearchUsesDSHBoundsAndVisibleMessages(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	ctx := context.Background()

	longText := "needle at the beginning " + strings.Repeat("x", 260)
	longData, err := json.Marshal(map[string]string{"text": longText})
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, st, "search-visible", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1000), Version: session.EventVersion,
			Data: longData,
		},
	})
	seedSession(t, st, "search-archived", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
			Data: json.RawMessage(`{"text":"needle archived"}`),
		},
	})
	if err := st.ArchiveSession(ctx, "search-archived", true); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 21; index++ {
		seedSession(t, st, fmt.Sprintf("search-more-%02d", index), []session.Event{
			{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(int64(2000 + index)), Version: session.EventVersion,
				Data: json.RawMessage(`{"text":"needle more"}`),
			},
		})
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.search", "tok", `{"type":"client-request","rpcId":"search-bounds","method":"session.search","payload":{"query":"needle more"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.search response = %+v", response)
	}
	var value struct {
		Items []struct {
			SessionID string `json:"sessionId"`
			Snippet   string `json:"snippet"`
		} `json:"items"`
		HasMore bool `json:"hasMore"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Items) != 20 || !value.HasMore {
		t.Fatalf("session search value = %#v", value)
	}
	if len(value.Items) != 20 {
		t.Fatalf("unexpected visible item in more search = %#v", value.Items)
	}
	for _, item := range value.Items {
		if item.SessionID == "search-visible" || item.SessionID == "search-archived" {
			t.Fatalf("hidden or archived search item = %#v", item)
		}
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.search", "tok", `{"type":"client-request","rpcId":"search-snippet","method":"session.search","payload":{"query":"beginning"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("snippet search response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Items) != 1 || value.Items[0].SessionID != "search-visible" ||
		len([]rune(value.Items[0].Snippet)) != 240 ||
		!strings.HasPrefix(value.Items[0].Snippet, "needle at the beginning ") {
		t.Fatalf("snippet search value = %#v", value)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.search", "tok", `{"type":"client-request","rpcId":"search-nul","method":"session.search","payload":{"query":"bad\u0000ue"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("bad search response = %+v", response)
	}
}

func TestNativeSessionPromptChecksSessionImageCapabilityBeforeWriting(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "native-no-image", time.Now()); err != nil {
		t.Fatal(err)
	}
	attachmentDir := filepath.Join(t.TempDir(), "attachments")
	att, err := attachment.NewStore(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	srv.SetNativeImageCapabilityResolver(func(context.Context, string) bool { return false })
	data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"cap-1","method":"session.prompt","payload":{"sessionId":"native-no-image","mode":"steer","content":[{"type":"text","text":"image"},{"type":"image","mediaType":"image/png","data":%q}]}}`, data))
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "attachment-error" || response.Result.Error.Details["reason"] != "MODEL_DOES_NOT_SUPPORT_IMAGES" {
		t.Fatalf("unsupported native image response = %+v", response)
	}
	entries, err := os.ReadDir(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsupported native image prompt left %d attachment files", len(entries))
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"cap-invalid-base64","method":"session.prompt","payload":{"sessionId":"native-no-image","mode":"steer","content":[{"type":"text","text":"image"},{"type":"image","mediaType":"image/png","data":"not-base64"}]}}`))
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "attachment-error" ||
		response.Result.Error.Details["reason"] != "MODEL_DOES_NOT_SUPPORT_IMAGES" {
		t.Fatalf("unsupported model precedence response = %+v", response)
	}
	entries, err = os.ReadDir(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsupported model image prompt left %d attachment files", len(entries))
	}
}

func TestNativeMessageFeedbackUsesDSHMessageIDsAndVersionCAS(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "feedback-native", []session.Event{{
		Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(2000), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"native reply"}`),
	}})
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", fmt.Sprintf(`{"type":"client-request","rpcId":"feedback","method":"%s","payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}
	messageID := nativeMessageID("feedback-native", 1)
	list := call(t, "messageFeedback/list", `{"args":[{"sessionId":"feedback-native"}]}`)
	if !list.Result.OK {
		t.Fatalf("initial feedback list = %+v", list)
	}
	put := call(t, "messageFeedback/put", fmt.Sprintf(`{"args":[{"sessionId":"feedback-native","messageId":%q,"rating":"positive","ifVersion":null}]}`, messageID))
	if !put.Result.OK {
		t.Fatalf("feedback put transport = %+v", put)
	}
	var putValue struct {
		OK    bool `json:"ok"`
		Value struct {
			MessageID string `json:"messageId"`
			Version   string `json:"version"`
		} `json:"value"`
	}
	encoded, _ := json.Marshal(put.Result.Value)
	if err := json.Unmarshal(encoded, &putValue); err != nil || !putValue.OK || putValue.Value.MessageID != messageID || putValue.Value.Version == "" {
		t.Fatalf("feedback put value = %s", encoded)
	}
	stale := call(t, "messageFeedback/put", fmt.Sprintf(`{"args":[{"sessionId":"feedback-native","messageId":%q,"rating":"negative","ifVersion":"stale"}]}`, messageID))
	encoded, _ = json.Marshal(stale.Result.Value)
	var staleValue struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(encoded, &staleValue); err != nil || staleValue.OK {
		t.Fatalf("stale feedback put = %s", encoded)
	}
	deleted := call(t, "messageFeedback/delete", fmt.Sprintf(`{"args":[{"sessionId":"feedback-native","messageId":%q,"ifVersion":%q}]}`, messageID, putValue.Value.Version))
	encoded, _ = json.Marshal(deleted.Result.Value)
	var deletedValue struct {
		OK    bool `json:"ok"`
		Value struct {
			Absent bool `json:"absent"`
		} `json:"value"`
	}
	if err := json.Unmarshal(encoded, &deletedValue); err != nil || !deletedValue.OK || !deletedValue.Value.Absent {
		t.Fatalf("feedback delete = %s", encoded)
	}
}

func TestNativeFileReferencesListUsesSessionCWDAndRemoteArguments(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSession(t, st, "file-session", nil)
	if err := st.SetSessionCWD(context.Background(), "file-session", root); err != nil {
		t.Fatal(err)
	}
	rec := doReqBody(t, srv.Handler(), "POST", "/api/fileReferences/list", "tok", `{"type":"client-request","rpcId":"file-ref","method":"fileReferences/list","payload":{"args":["file-session","src/",{}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("file reference response = %+v", response)
	}
	var values []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Path != "src/main.go" || values[0].Kind != "file" {
		t.Fatalf("file reference values = %+v", values)
	}
}

func TestNativeSessionReferenceCandidatesReturnCanonicalMentions(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "current-session", nil)
	seedSession(t, st, "release-session", nil)
	if err := st.SetSessionTitle(context.Background(), "release-session", "Release notes", session.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	rec := doReqBody(t, srv.Handler(), "POST", "/api/sessionReferenceResolver/candidates", "tok", `{"type":"client-request","rpcId":"session-ref","method":"sessionReferenceResolver/candidates","payload":{"args":["current-session","release",{}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session reference response = %+v", response)
	}
	var values []struct {
		SessionID string `json:"sessionId"`
		Label     string `json:"label"`
		Mention   string `json:"mention"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SessionID != "release-session" || values[0].Label != "Release notes" || !strings.HasPrefix(values[0].Mention, "@[Release notes](shutu-session:") {
		t.Fatalf("session reference values = %+v", values)
	}
}

func TestNativeSessionReferenceCandidatesRankWorkingDirectoryFirst(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	root := t.TempDir()
	if err := st.CreateSession(context.Background(), "current-session", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionCWD(context.Background(), "current-session", root); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, cwd, title string }{
		{"other-cwd", filepath.Join(root, "other"), "Other"},
		{"same-cwd", root, "Same"},
	} {
		if err := st.CreateSession(context.Background(), item.id, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSessionCWD(context.Background(), item.id, item.cwd); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSessionTitle(context.Background(), item.id, item.title, session.TitleSourceUser); err != nil {
			t.Fatal(err)
		}
	}
	rec := doReqBody(t, srv.Handler(), "POST", "/api/sessionReferenceResolver/candidates", "tok", `{"type":"client-request","rpcId":"rank","method":"sessionReferenceResolver/candidates","payload":{"args":["current-session","",{}]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session reference response = %+v", response)
	}
	var values []struct {
		SessionID string `json:"sessionId"`
		Label     string `json:"label"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].SessionID != "same-cwd" || values[1].SessionID != "other-cwd" {
		t.Fatalf("ranked candidates = %+v", values)
	}
}

func TestNativeSessionListDoesNotCheckpointTailOnlyProjection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	events := []session.Event{{
		Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"turn":1}`),
	}, {
		Seq: 2, Type: session.EventPlanCreate, At: time.UnixMilli(1002), Version: session.EventVersion,
		Data: json.RawMessage(`{"scope":"goal","id":"goal-old","title":"preserve old state","detail":{"objective":"must survive tail replay","status":"pending","revision":1,"maxRounds":2}}`),
	}}
	for seq := uint64(3); seq <= nativeSessionListTailLimit+2; seq++ {
		events = append(events, session.Event{
			Seq: seq, Type: session.EventCompactionStart, At: time.UnixMilli(int64(1000 + seq)), Version: session.EventVersion,
			Data: json.RawMessage(`{"compactionId":"compact-tail"}`),
		})
	}
	seedSession(t, st, "native-list-tail", events)

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"tail-list","method":"session.list","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Projections == nil || list.Items[0].Blank {
		t.Fatalf("session.list items = %+v", list.Items)
	}
	goal, ok := list.Items[0].Projections.Values["goal"].(map[string]any)
	goalDetail, detailOK := goal["goal"].(map[string]any)
	if !ok || !detailOK || goalDetail["id"] != "goal-old" {
		t.Fatalf("session.list lost state outside tail window: %#v", list.Items[0].Projections.Values["goal"])
	}
	row, err := st.GetProjectionCache(context.Background(), "native-list-tail")
	if err != nil {
		t.Fatal(err)
	}
	if row.Revision != nativeSessionListTailLimit+2 {
		t.Fatalf("projection cache revision = %d, want %d", row.Revision, nativeSessionListTailLimit+2)
	}
	var cached nativeProjectionCachePayload
	if err := json.Unmarshal(row.Payload, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Block.Values["goal"] == nil {
		t.Fatalf("tail-only projection was checkpointed: %#v", cached.Block.Values)
	}
}

func TestNativePluginInventoryMatchesNativeManifestShape(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/pluginInventory/list", "tok", `{"type":"client-request","rpcId":"plugins","method":"pluginInventory/list","payload":{"args":[]}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("plugin inventory response = %+v", response)
	}
	var value struct {
		Entries []struct {
			EntryID    string `json:"entryId"`
			ModuleName string `json:"moduleName"`
			Enabled    bool   `json:"enabled"`
			FiberPhase string `json:"fiberPhase"`
		} `json:"entries"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Entries) < 30 || value.Entries[0].EntryID == "" || value.Entries[0].EntryID != value.Entries[0].ModuleName || !value.Entries[0].Enabled || value.Entries[0].FiberPhase != "active" {
		t.Fatalf("plugin inventory value = %+v", value.Entries)
	}
}

func TestNativeHistoryReturnsDSHProjectionBaseline(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetContextWindow(func(string) int { return 128000 })
	seedSession(t, st, "native-projections", []session.Event{
		{Seq: 1, Type: session.EventPlanCreate, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","title":"ship native UI"}`)},
		{Seq: 2, Type: session.EventPlanStatus, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"todo","id":"todo-1","status":"in-progress"}`)},
		{Seq: 3, Type: session.EventPlanMode, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"active":true,"pending":false}`)},
		{Seq: 4, Type: session.EventLLMRequestStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"provider":"deepseek","model":"reasoner"}`)},
		{Seq: 5, Type: session.EventLLMRequestEnd, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"usage":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":40,"cacheWriteTokens":5}}`)},
	})
	if err := st.SetSessionTitle(context.Background(), "native-projections", "Native UI parity", session.TitleSourceUser); err != nil {
		t.Fatalf("set title: %v", err)
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"projection-1","method":"session.history","payload":{"sessionId":"native-projections"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Projections.AsOfSeq != 5 {
		t.Fatalf("projection asOfSeq = %d, want 5", history.Projections.AsOfSeq)
	}
	values := history.Projections.Values
	if values["title"] != "Native UI parity" {
		t.Fatalf("projection title = %#v", values["title"])
	}
	var todos []map[string]any
	encoded, _ = json.Marshal(values["todos"])
	if err := json.Unmarshal(encoded, &todos); err != nil {
		t.Fatal(err)
	}
	if len(todos) != 1 || todos[0]["content"] != "ship native UI" || todos[0]["status"] != "in_progress" {
		t.Fatalf("projection todos = %#v", todos)
	}
	var usage map[string]int64
	encoded, _ = json.Marshal(values["tokenUsage"])
	if err := json.Unmarshal(encoded, &usage); err != nil {
		t.Fatal(err)
	}
	if usage["uncachedInputTokens"] != 60 || usage["outputTokens"] != 20 || usage["cacheReadTokens"] != 40 || usage["cacheWriteTokens"] != 5 {
		t.Fatalf("projection token usage = %#v", usage)
	}
	var contextPressure map[string]int64
	encoded, _ = json.Marshal(values["contextPressure"])
	if err := json.Unmarshal(encoded, &contextPressure); err != nil {
		t.Fatal(err)
	}
	if contextPressure["contextWindow"] != 128000 || contextPressure["pressureTokens"] != 145 {
		t.Fatalf("projection context pressure = %#v", contextPressure)
	}
	var plan map[string]bool
	encoded, _ = json.Marshal(values["plan"])
	if err := json.Unmarshal(encoded, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan["active"] || plan["pending"] {
		t.Fatalf("projection plan = %#v", plan)
	}
}

func TestNativeHistoryUsesCanonicalTitleFallback(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-title-fallback", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"ship the stable base"}`),
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"projection-title","method":"session.history","payload":{"sessionId":"native-title-fallback"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if got := history.Projections.Values["title"]; got != "ship the stable base" {
		t.Fatalf("projection title = %#v, want canonical fallback", got)
	}
}

func TestNativeHistoryProjectionCacheIsUsedOnlyAtCommittedRevision(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "projection-cache", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"hello"}`),
	}})
	request := func(id string) nativeRPCResponse {
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"`+id+`","method":"session.history","payload":{"sessionId":"projection-cache"}}`)
		return nativeResponse(t, rec.Body.Bytes())
	}
	first := request("cache-1")
	if !first.Result.OK {
		t.Fatalf("first history response = %+v", first)
	}
	row, err := st.GetProjectionCache(context.Background(), "projection-cache")
	if err != nil || row.Version != nativeProjectionCacheVersion || row.Revision != 1 {
		t.Fatalf("projection cache row = %+v, err=%v", row, err)
	}
	var cached nativeProjectionCachePayload
	if err := json.Unmarshal(row.Payload, &cached); err != nil {
		t.Fatal(err)
	}
	cached.Block.Values["cacheMarker"] = "committed-checkpoint"
	payload, _ := json.Marshal(cached)
	row.Payload = payload
	if err := st.PutProjectionCache(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	second := request("cache-2")
	if !second.Result.OK {
		t.Fatalf("second history response = %+v", second)
	}
	var value struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(second.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.Projections.Values["cacheMarker"] != "committed-checkpoint" {
		t.Fatalf("exact-revision cache was not used: %#v", value.Projections.Values)
	}
	if err := st.AppendEvents(context.Background(), "projection-cache", []session.Event{{
		Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"world"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if beforeThird, cacheErr := st.GetProjectionCache(context.Background(), "projection-cache"); cacheErr != nil || beforeThird.Revision != 1 {
		t.Fatalf("cache changed before stale-read check: %+v err=%v", beforeThird, cacheErr)
	}
	third := request("cache-3")
	if !third.Result.OK {
		t.Fatalf("third history response = %+v", third)
	}
	value = struct {
		Projections nativeProjectionBlock `json:"projections"`
	}{}
	encoded, _ = json.Marshal(third.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if _, exists := value.Projections.Values["cacheMarker"]; exists {
		after, _ := st.GetProjectionCache(context.Background(), "projection-cache")
		t.Fatalf("stale cache outranked durable event replay: cache=%+v values=%#v", after, value.Projections.Values)
	}
}

func TestNativeHistoryProjectionCacheCorruptionFallsBackAndRepairs(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "projection-cache-repair", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
			Data: json.RawMessage(`{"text":"repair me"}`)},
	})
	request := func(id string) nativeRPCResponse {
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"`+id+`","method":"session.history","payload":{"sessionId":"projection-cache-repair"}}`)
		return nativeResponse(t, rec.Body.Bytes())
	}
	if response := request("repair-1"); !response.Result.OK {
		t.Fatalf("initial history response = %+v", response)
	}
	row, err := st.GetProjectionCache(context.Background(), "projection-cache-repair")
	if err != nil || row.Revision != 1 {
		t.Fatalf("initial cache row = %+v, err=%v", row, err)
	}
	row.Payload = []byte(`{"block":`)
	if err := st.PutProjectionCache(context.Background(), row); err != nil {
		t.Fatalf("write corrupt cache fixture: %v", err)
	}
	if response := request("repair-2"); !response.Result.OK {
		t.Fatalf("rebuild history response = %+v", response)
	}
	repaired, err := st.GetProjectionCache(context.Background(), "projection-cache-repair")
	if err != nil || repaired.Revision != 1 {
		t.Fatalf("repaired cache row = %+v, err=%v", repaired, err)
	}
	var payload nativeProjectionCachePayload
	if err := json.Unmarshal(repaired.Payload, &payload); err != nil || payload.Block.Values == nil {
		t.Fatalf("cache was not repaired from durable events: err=%v payload=%s", err, repaired.Payload)
	}
}

func TestNativeHistoryReturnsGoalAndPermissionProjection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-goal", []session.Event{
		{Seq: 1, Type: session.EventPlanCreate, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","title":"Release","detail":{"objective":"ship the release","status":"pending","revision":1,"maxRounds":3}}`)},
		{Seq: 2, Type: session.EventGoalRoundStart, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"goalId":"goal-1","round":1}`)},
		{Seq: 3, Type: session.EventPlanStatus, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","status":"blocked","reason":"waiting for approval"}`)},
		{Seq: 4, Type: session.EventPlanUpdate, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"scope":"goal","id":"goal-1","objective":"ship the verified release","revision":2}`)},
	})
	if err := st.SetSessionConfig(context.Background(), "native-goal", store.SessionConfig{Permission: "readonly"}); err != nil {
		t.Fatalf("set session permission: %v", err)
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"goal-1","method":"session.history","payload":{"sessionId":"native-goal"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	var goal map[string]any
	encoded, _ = json.Marshal(history.Projections.Values["goal"])
	if err := json.Unmarshal(encoded, &goal); err != nil {
		t.Fatal(err)
	}
	var goalSnapshot map[string]any
	encoded, _ = json.Marshal(goal["goal"])
	if err := json.Unmarshal(encoded, &goalSnapshot); err != nil {
		t.Fatal(err)
	}
	if goalSnapshot["id"] != "goal-1" || goalSnapshot["objective"] != "ship the verified release" || goalSnapshot["phase"] != "blocked" || goalSnapshot["maxGoalRounds"] != float64(3) {
		t.Fatalf("goal snapshot = %#v", goalSnapshot)
	}
	if goal["roundsStarted"] != float64(1) || goal["createdAt"] != float64(1001) || goal["updatedAt"] != float64(1004) {
		t.Fatalf("goal projection metadata = %#v", goal)
	}
	var reason map[string]any
	encoded, _ = json.Marshal(goalSnapshot["blockedReason"])
	if err := json.Unmarshal(encoded, &reason); err != nil {
		t.Fatal(err)
	}
	if reason["code"] != "blocked" || reason["message"] != "waiting for approval" {
		t.Fatalf("goal blocked reason = %#v", reason)
	}
	var permissions map[string]any
	encoded, _ = json.Marshal(history.Projections.Values["permissions"])
	if err := json.Unmarshal(encoded, &permissions); err != nil {
		t.Fatal(err)
	}
	if permissions["currentValue"] != "readonly" {
		t.Fatalf("permissions projection = %#v", permissions)
	}
}

func TestNativeHistoryPermissionUsesSharedProjectionOverConfigFallback(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-permission", []session.Event{
		{Seq: 1, Type: session.EventPermissionPreset, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"preset":"read-only"}`)},
		{Seq: 2, Type: session.EventSandboxMode, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"mode":"read-only"}`)},
	})
	if err := st.SetSessionConfig(context.Background(), "native-permission", store.SessionConfig{Permission: "full"}); err != nil {
		t.Fatalf("set stale session permission: %v", err)
	}

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"permission-1","method":"session.history","payload":{"sessionId":"native-permission"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(history.Projections.Values["permissions"])
	var permissions struct {
		CurrentValue string `json:"currentValue"`
	}
	if err := json.Unmarshal(encoded, &permissions); err != nil {
		t.Fatal(err)
	}
	if permissions.CurrentValue != "readonly" {
		t.Fatalf("permissions projection = %#v, want durable readonly to beat config fallback", permissions)
	}
}

func TestNativeHistoryReturnsDSHHeaderLineageAndSurfaceSnapshot(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-lineage", []session.Event{
		{Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"id":"child","provider":"spawn","parentSession":"root","label":"research","depth":2}`)},
		{Seq: 2, Type: session.EventTurnStart, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"question"}`)},
		{Seq: 4, Type: session.EventAssistantMessage, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer"}`)},
		{Seq: 5, Type: session.EventAssistantMessage, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":3,"end":4}}`)},
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"lineage","method":"session.history","payload":{"sessionId":"native-lineage"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Header  nativeSessionHeader `json:"header"`
		Surface map[string]any      `json:"surface"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if history.Header.ParentSessionID != "root" || history.Header.Origin != "subagent" || history.Header.DelegationDepth != 2 {
		t.Fatalf("session header lineage = %+v", history.Header)
	}
	if history.Surface["replaceGeneration"] != float64(1) {
		t.Fatalf("surface generation = %#v", history.Surface)
	}
	nodes, ok := history.Surface["nodes"].([]any)
	if !ok || len(nodes) != 1 || nodes[0] != float64(5) {
		t.Fatalf("surface nodes = %#v", history.Surface["nodes"])
	}
}

func TestNativeProjectionPreservesDSHContentBlocks(t *testing.T) {
	cursor := newNativeProjectionCursor()
	event := cursor.project("native-content", session.Event{
		Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(1000), Version: session.EventVersion,
		Data: json.RawMessage(`{"content":[{"type":"text","text":"**bold**\n\n    return 1"},{"type":"reasoning","text":"think first"},{"type":"image","attachment":{"attachmentId":"att-1","mediaType":"image/png","bytes":12,"width":2,"height":3,"name":"screen.png"}},{"type":"tool-result","toolCallId":"call-1","content":[{"type":"text","text":"done"}],"isError":true},{"type":"vendor-card","value":{"ok":true}}]}`),
	})
	var projected struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(event.Data, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Message.Content) != 5 {
		t.Fatalf("projected content = %#v", projected.Message.Content)
	}
	if projected.Message.Content[0]["text"] != "**bold**\n\n    return 1" || projected.Message.Content[1]["type"] != "reasoning" {
		t.Fatalf("markdown/reasoning blocks = %#v", projected.Message.Content[:2])
	}
	image, ok := projected.Message.Content[2]["attachment"].(map[string]any)
	if !ok || image["attachmentId"] != "att-1" || image["name"] != "screen.png" {
		t.Fatalf("image attachment block = %#v", projected.Message.Content[2])
	}
	if projected.Message.Content[3]["toolCallId"] != "call-1" || projected.Message.Content[3]["isError"] != true {
		t.Fatalf("tool result block = %#v", projected.Message.Content[3])
	}
	if projected.Message.Content[4]["type"] != "vendor-card" {
		t.Fatalf("extension block = %#v", projected.Message.Content[4])
	}
}

func TestNativeProjectionFoldsSessionStatsFromLifecycleEvents(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(900), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"step":1}`)},
		{Seq: 3, Type: session.EventAssistantChunk, At: time.UnixMilli(1800), Version: session.EventVersion, Data: json.RawMessage(`{"text":"a"}`)},
		{Seq: 4, Type: session.EventAssistantMessage, At: time.UnixMilli(4800), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","usage":{"outputTokens":60}}`)},
		{Seq: 5, Type: session.EventToolCall, At: time.UnixMilli(5000), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","name":"read"}`)},
		{Seq: 6, Type: session.EventToolResult, At: time.UnixMilli(6500), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","output":"ok"}`)},
		{Seq: 7, Type: session.EventStepEnd, At: time.UnixMilli(6600), Version: session.EventVersion, Data: json.RawMessage(`{"step":1}`)},
		{Seq: 8, Type: session.EventTurnEnd, At: time.UnixMilli(6700), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
	}
	cursor := newNativeProjectionCursor()
	for _, ev := range events {
		cursor.project("stats", ev)
	}
	var stats map[string]int64
	encoded, _ := json.Marshal(cursor.projectionBlock("", 8).Values["sessionStats"])
	if err := json.Unmarshal(encoded, &stats); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"turns": 1, "steps": 1, "llmMs": 3800, "toolMs": 1500,
		"ttftMs": 800, "ttftSteps": 1, "decodeMs": 3000, "decodeTokens": 60,
	}
	for key, value := range want {
		if stats[key] != value {
			t.Fatalf("sessionStats[%s] = %d, want %d (all=%#v)", key, stats[key], value, stats)
		}
	}
}

func TestNativeProjectionFoldsDSHSubagentIdentityAndTiming(t *testing.T) {
	tests := []struct {
		name        string
		events      []session.Event
		wantMode    string
		wantLabel   string
		wantSeq     uint64
		wantSettled int64
		wantActive  bool
	}{
		{
			name: "canonical descriptor includes pending turn",
			events: []session.Event{
				{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(100), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
				{Seq: 2, Type: "subagent/descriptor", At: time.UnixMilli(200), Version: session.EventVersion, Data: json.RawMessage(`{"version":2,"mode":"continuable","provider":"spawn","label":"research"}`)},
				{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(500), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
			},
			wantMode: "continuable", wantLabel: "research", wantSeq: 2, wantSettled: 400,
		},
		{
			name: "legacy start defaults to one shot",
			events: []session.Event{
				{Seq: 7, Type: session.EventSubagentStart, At: time.UnixMilli(700), Version: session.EventVersion, Data: json.RawMessage(`{"id":"child-1","label":"quick task"}`)},
			},
			wantMode: "one-shot", wantLabel: "quick task", wantSeq: 7,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cursor := newNativeProjectionCursor()
			for _, ev := range test.events {
				cursor.project("subagent", ev)
			}
			values := cursor.projectionBlock("", int64(test.wantSeq)).Values
			identity, ok := values["subagent"].(map[string]any)
			if !ok {
				t.Fatalf("subagent projection = %#v", values["subagent"])
			}
			if identity["mode"] != test.wantMode || identity["label"] != test.wantLabel || identity["seq"] != test.wantSeq {
				t.Fatalf("subagent identity = %#v", identity)
			}
			timing, ok := values["subagentTiming"].(map[string]any)
			if !ok || timing["settledMs"] != test.wantSettled {
				t.Fatalf("subagent timing = %#v", values["subagentTiming"])
			}
			_, active := timing["active"]
			if active != test.wantActive {
				t.Fatalf("subagent active = %v, want %v", active, test.wantActive)
			}
		})
	}
}

func TestNativeProjectionFoldsDSHContextBreakdown(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: "request/header", At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"header":{"system":"12345678","tools":[{"name":"read"}]}}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"hello"}`)},
		{Seq: 3, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"ok"}`)},
		{Seq: 4, Type: session.EventCompactionSummary, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"shadowedRange":{"start":2,"end":3},"shadowedTokenCount":19}`)},
		{Seq: 5, Type: session.EventAssistantMessage, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"text":"new","surfaceOp":{"op":"replace","start":2,"end":3}}`)},
	}
	cursor := newNativeProjectionCursor()
	for _, ev := range events {
		cursor.project("context", ev)
	}
	value, ok := cursor.projectionBlock("", 5).Values["contextBreakdown"].(map[string]any)
	if !ok {
		t.Fatalf("contextBreakdown projection = %#v", cursor.projectionBlock("", 5).Values["contextBreakdown"])
	}
	if value["systemTokens"] != int64(6) || value["toolsTokens"] != nativeEstimateJSONTokens([]any{map[string]any{"name": "read"}})+4 {
		t.Fatalf("contextBreakdown request envelope = %#v", value)
	}
	if value["messageTokens"] != int64(9) {
		t.Fatalf("contextBreakdown replacement total = %#v, want 9", value)
	}
}

func TestNativeProjectionRejectsMalformedDSHSubagentDescriptor(t *testing.T) {
	cursor := newNativeProjectionCursor()
	cursor.project("subagent", session.Event{
		Seq: 1, Type: "subagent/descriptor", At: time.UnixMilli(100), Version: session.EventVersion,
		Data: json.RawMessage(`{"version":1,"mode":"continuable","label":"research"}`),
	})
	values := cursor.projectionBlock("", 1).Values
	if values["subagent"] != nil {
		t.Fatalf("malformed subagent projection = %#v, want nil", values["subagent"])
	}
	timing, ok := values["subagentTiming"].(map[string]any)
	if !ok || timing["settledMs"] != int64(0) {
		t.Fatalf("malformed subagent timing = %#v", values["subagentTiming"])
	}
}

func TestNativeProjectionIncludesImageLimitsWhenAttachmentsEnabled(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	seedSession(t, st, "native-image-limits", nil)

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"image-limits","method":"session.history","payload":{"sessionId":"native-image-limits"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	limits, ok := history.Projections.Values["imageLimits"].(map[string]any)
	if !ok {
		t.Fatalf("history imageLimits = %#v", history.Projections.Values["imageLimits"])
	}
	if limits["maxImageBytes"] != float64(5*1024*1024) || limits["maxImagesPerMessage"] != float64(20) || limits["maxImageDimension"] != float64(2000) {
		t.Fatalf("history imageLimits = %#v", limits)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"image-list","method":"session.list","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Projections == nil {
		t.Fatalf("session.list image limits items = %+v", list.Items)
	}
	if _, ok := list.Items[0].Projections.Values["imageLimits"].(map[string]any); !ok {
		t.Fatalf("session.list imageLimits = %#v", list.Items[0].Projections.Values["imageLimits"])
	}
}

func TestNativeSessionAttachmentRequiresReferenceAndReturnsDSHData(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := att.SaveImage("image/png", data, maxWebImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	eventData, err := json.Marshal(map[string]any{
		"content": []any{map[string]any{
			"type": "image", "attachment": map[string]any{"attachmentId": ref.ID, "mediaType": ref.MediaType, "bytes": len(data)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, st, "native-attachment", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: eventData,
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.attachment", "tok", `{"type":"client-request","rpcId":"attachment-1","method":"session.attachment","payload":{"sessionId":"native-attachment","attachmentId":"`+ref.ID+`"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.attachment response = %+v", response)
	}
	var value struct {
		Attachment map[string]any `json:"attachment"`
		Data       string         `json:"data"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.Attachment["attachmentId"] != ref.ID || value.Attachment["mediaType"] != "image/png" || value.Attachment["width"] != float64(1) || value.Attachment["height"] != float64(1) {
		t.Fatalf("attachment ref = %#v", value.Attachment)
	}
	if value.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("attachment data = %q", value.Data)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.attachment", "tok", `{"type":"client-request","rpcId":"attachment-2","method":"session.attachment","payload":{"sessionId":"native-attachment","attachmentId":"not-referenced"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "attachment-error" {
		t.Fatalf("unreferenced attachment response = %+v", response)
	}
}

func TestNativeSessionModelsUsesStandardSessionMethod(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"llm_provider": "deepseek-official", "model": "deepseek-v4-flash"}
	})
	seedSession(t, st, "native-models", nil)
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.models", "tok", `{"type":"client-request","rpcId":"models-1","method":"session.models","payload":{"sessionId":"native-models"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.models response = %+v", response)
	}
	var value map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	current, ok := value["current"].(map[string]any)
	if !ok || current["provider"] != "deepseek-official" || current["model"] != "deepseek-v4-flash" || value["routable"] != true {
		t.Fatalf("session.models value = %#v", value)
	}
}

func TestNativeAgentPresetsListSelectAndLock(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any { return map[string]any{"mode": "code"} })

	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("presets-list", "agentPreset.list", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("agentPreset.list response = %+v", response)
	}
	var roster struct {
		Presets     []map[string]any `json:"presets"`
		Authorable  bool             `json:"authorable"`
		HasDocument bool             `json:"hasDocument"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.Presets) != 3 || roster.Authorable || roster.HasDocument {
		t.Fatalf("agent preset roster = %+v", roster)
	}
	for _, preset := range roster.Presets {
		if preset["id"] == "code" && preset["isDefault"] != true {
			t.Fatalf("code preset default flag = %#v", preset["isDefault"])
		}
	}

	if err := st.CreateSession(context.Background(), "blank-preset", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionConfig(context.Background(), "blank-preset", store.SessionConfig{Provider: "openai", Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	response = call("preset-select", "agentPreset.select", map[string]any{"sessionId": "blank-preset", "agentPreset": "minimal"})
	if !response.Result.OK {
		t.Fatalf("agentPreset.select response = %+v", response)
	}
	config, err := st.GetSessionConfig(context.Background(), "blank-preset")
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentPreset != "minimal" || config.Provider != "openai" || config.Model != "gpt-4o" {
		t.Fatalf("selected session config = %+v", config)
	}

	seedSession(t, st, "locked-preset", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"already used"}`),
	}})
	response = call("preset-locked", "agentPreset.select", map[string]any{"sessionId": "locked-preset", "agentPreset": "code"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "agent-preset-locked" {
		t.Fatalf("locked agent preset response = %+v", response)
	}
	response = call("preset-invalid", "agentPreset.select", map[string]any{"sessionId": "blank-preset", "agentPreset": "unknown"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "agent-preset-invalid" {
		t.Fatalf("invalid agent preset response = %+v", response)
	}
}

func TestNativeAgentPresetsHideCodeWhenRuntimeUnavailable(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"mode": "standard", "code_enabled": false, "code_available": false}
	})
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("presets-unavailable-list", "agentPreset.list", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("agentPreset.list response = %+v", response)
	}
	encoded, _ := json.Marshal(response.Result.Value)
	var roster struct {
		Presets []map[string]any `json:"presets"`
	}
	if err := json.Unmarshal(encoded, &roster); err != nil {
		t.Fatal(err)
	}
	for _, preset := range roster.Presets {
		if preset["id"] == "code" {
			t.Fatalf("unavailable code preset was advertised: %#v", roster.Presets)
		}
	}
	response = call("presets-unavailable-select", "agentPreset.select", map[string]any{"sessionId": "blank", "agentPreset": "code"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "agent-preset-invalid" {
		t.Fatalf("unavailable code selection response = %+v", response)
	}
}

type nativeAgentPresetManagerStub struct {
	removed string
}

func (m *nativeAgentPresetManagerStub) List(context.Context) (NativeAgentPresetCatalog, error) {
	return NativeAgentPresetCatalog{
		Presets: []NativeAgentPreset{{ID: "custom", Trust: "user", Name: "Custom"}}, Authorable: true, HasDocument: true,
	}, nil
}

func (m *nativeAgentPresetManagerStub) Read(context.Context, string) (NativeAgentPresetDetails, error) {
	return NativeAgentPresetDetails{AgentPreset: "custom", Trust: "user", Content: "custom prompt", Name: "Custom"}, nil
}

func (m *nativeAgentPresetManagerStub) Copy(context.Context, string, string, string) (string, error) {
	return "copied", nil
}

func (m *nativeAgentPresetManagerStub) OpenDocument(context.Context, string) (NativeAgentPresetDocument, error) {
	return NativeAgentPresetDocument{Path: `C:\preset`}, nil
}

func (m *nativeAgentPresetManagerStub) Remove(_ context.Context, id string) error {
	m.removed = id
	return nil
}

func TestNativeAgentPresetAuthoringRPCs(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	manager := &nativeAgentPresetManagerStub{}
	srv.SetNativeAgentPresetManager(manager)
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}
	if response := call("list", "agentPreset.list", map[string]any{}); !response.Result.OK {
		t.Fatalf("list response=%+v", response)
	}
	for _, item := range []struct {
		id      string
		method  string
		payload map[string]any
		wantKey string
	}{
		{"read", "agentPreset.read", map[string]any{"agentPreset": "custom"}, "content"},
		{"copy", "agentPreset.copy", map[string]any{"from": "standard", "agentPreset": "custom-copy"}, "agentPreset"},
		{"open", "agentPreset.openDocument", map[string]any{"agentPreset": "custom"}, "path"},
	} {
		response := call(item.id, item.method, item.payload)
		if !response.Result.OK {
			t.Fatalf("%s response=%+v", item.method, response)
		}
		value, ok := response.Result.Value.(map[string]any)
		if !ok || value[item.wantKey] == nil {
			t.Fatalf("%s value=%+v", item.method, response.Result.Value)
		}
	}
	if response := call("remove", "agentPreset.remove", map[string]any{"agentPreset": "custom"}); !response.Result.OK || manager.removed != "custom" {
		t.Fatalf("remove response=%+v removed=%q", response, manager.removed)
	}
}

func TestNativeSessionSelectModelPersistsAndProjectsSelection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	var defaultProvider, defaultModel, defaultEffort string
	srv.SetNativeDefaultModelSaver(func(_ context.Context, provider, model, effort string) {
		defaultProvider, defaultModel, defaultEffort = provider, model, effort
	})
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"provider": "deepseek-official", "model": "deepseek-chat", "reasoning_effort": "low"}
	})
	if err := st.CreateSession(context.Background(), "select-model", time.Now()); err != nil {
		t.Fatal(err)
	}
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("select-model", "session.selectModel", map[string]any{
		"sessionId": "select-model", "provider": "openai", "model": "gpt-4o", "reasoningEffort": "high",
	})
	if !response.Result.OK {
		t.Fatalf("session.selectModel response = %+v", response)
	}
	var selected struct {
		Selected map[string]any `json:"selected"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Selected["provider"] != "openai" || selected.Selected["model"] != "gpt-4o" || selected.Selected["reasoningEffort"] != "high" {
		t.Fatalf("selected model = %#v", selected.Selected)
	}
	config, err := st.GetSessionConfig(context.Background(), "select-model")
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider != "openai" || config.Model != "gpt-4o" || config.ReasoningEffort != "high" {
		t.Fatalf("persisted model config = %+v", config)
	}
	if defaultProvider != "openai" || defaultModel != "gpt-4o" || defaultEffort != "high" {
		t.Fatalf("shared default model selection = %q/%q/%q", defaultProvider, defaultModel, defaultEffort)
	}

	response = call("models-after-select", "session.models", map[string]any{"sessionId": "select-model"})
	if !response.Result.OK {
		t.Fatalf("session.models response = %+v", response)
	}
	var models struct {
		Current map[string]any `json:"current"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &models); err != nil {
		t.Fatal(err)
	}
	if models.Current["provider"] != "openai" || models.Current["model"] != "gpt-4o" || models.Current["reasoningEffort"] != "high" {
		t.Fatalf("projected model selection = %#v", models.Current)
	}

	response = call("select-missing", "session.selectModel", map[string]any{
		"sessionId": "missing", "provider": "openai", "model": "gpt-4o",
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "not-found" {
		code, message := "", ""
		if response.Result.Error != nil {
			code, message = response.Result.Error.Code, response.Result.Error.Message
		}
		t.Fatalf("missing session response = %+v (%s: %s)", response, code, message)
	}
	response = call("select-invalid", "session.selectModel", map[string]any{"sessionId": "select-model", "provider": "openai"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("invalid selection response = %+v", response)
	}
}

func TestNativeSessionSelectModelRejectsUnavailableRoute(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "unavailable-model", time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.SetSessionModelValidator(func(context.Context, string, string, string, string) error {
		return fmt.Errorf("%w: dormant", llm.ErrProviderUnavailable)
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.selectModel", "tok", `{
		"type":"client-request","rpcId":"model-1","method":"session.selectModel",
		"payload":{"sessionId":"unavailable-model","provider":"dormant","model":"m"}
	}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "provider-unavailable" {
		t.Fatalf("unavailable model response = %+v", response)
	}
	config, err := st.GetSessionConfig(context.Background(), "unavailable-model")
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider != "" || config.Model != "" {
		t.Fatalf("rejected selection persisted: %+v", config)
	}
}

func TestNativeSessionSelectModelMapsCapabilityFailure(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	if err := st.CreateSession(context.Background(), "image-model", time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.SetSessionModelValidator(func(context.Context, string, string, string, string) error {
		return fmt.Errorf("%w: target is text-only", llm.ErrCapabilityUnavailable)
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.selectModel", "tok", `{
		"type":"client-request","rpcId":"model-capability-1","method":"session.selectModel",
		"payload":{"sessionId":"image-model","provider":"route-gw","model":"text-model"}
	}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "model-unavailable" {
		t.Fatalf("capability rejection response = %+v", response)
	}
}

func TestNativeWorkspaceCreateSerializesSamePath(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	root := t.TempDir()
	canonical, err := nativeWorkspaceCanonicalPath(root)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	ids := make(map[string]int)
	created := 0
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/workspace.create", "tok",
				fmt.Sprintf(`{"type":"client-request","rpcId":"concurrent-create","method":"workspace.create","payload":{"path":%q}}`, root))
			var response nativeRPCResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
				return
			}
			var value struct {
				Created   bool `json:"created"`
				Workspace struct {
					WorkspaceID string `json:"workspaceId"`
					Path        string `json:"path"`
				} `json:"workspace"`
			}
			encoded, _ := json.Marshal(response.Result.Value)
			if err := json.Unmarshal(encoded, &value); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !response.Result.OK || value.Workspace.Path != canonical || value.Workspace.WorkspaceID == "" {
				failures = append(failures, rec.Body.String())
				return
			}
			ids[value.Workspace.WorkspaceID]++
			if value.Created {
				created++
			}
		}()
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("concurrent workspace.create failures = %d, first=%s", len(failures), failures[0])
	}
	if created != 1 || len(ids) != 1 {
		t.Fatalf("concurrent create created=%d ids=%#v, want one id created once", created, ids)
	}
	workspaces, err := st.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].Path != canonical {
		t.Fatalf("workspaces = %#v, want one path %q", workspaces, canonical)
	}
}

func TestNativeWorkspaceLifecycleAndOrdering(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}
	workspace := func(response nativeRPCResponse) nativeWorkspaceView {
		t.Helper()
		var value struct {
			Workspace nativeWorkspaceView `json:"workspace"`
		}
		encoded, _ := json.Marshal(response.Result.Value)
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		return value.Workspace
	}

	rootA, rootB := t.TempDir(), t.TempDir()
	canonicalA, err := nativeWorkspaceCanonicalPath(rootA)
	if err != nil {
		t.Fatal(err)
	}
	createdA := call("workspace-create-a", "workspace.create", map[string]any{"path": rootA})
	if !createdA.Result.OK {
		code, message := "", ""
		if createdA.Result.Error != nil {
			code, message = createdA.Result.Error.Code, createdA.Result.Error.Message
		}
		t.Fatalf("workspace.create A = %+v (%s: %s)", createdA, code, message)
	}
	wsA := workspace(createdA)
	if wsA.WorkspaceID == "" || wsA.Path != canonicalA || wsA.Title != filepath.Base(canonicalA) || wsA.CreatedAt == "" || wsA.UpdatedAt == "" {
		t.Fatalf("workspace A = %+v, want path=%q title=%q", wsA, canonicalA, filepath.Base(canonicalA))
	}
	createdAgain := call("workspace-create-again", "workspace.create", map[string]any{"path": filepath.Join(rootA, ".")})
	if !createdAgain.Result.OK {
		t.Fatalf("idempotent workspace.create = %+v", createdAgain)
	}
	var createdAgainValue struct {
		Created   bool                `json:"created"`
		Workspace nativeWorkspaceView `json:"workspace"`
	}
	encoded, _ := json.Marshal(createdAgain.Result.Value)
	if err := json.Unmarshal(encoded, &createdAgainValue); err != nil {
		t.Fatal(err)
	}
	if createdAgainValue.Created || createdAgainValue.Workspace.WorkspaceID != wsA.WorkspaceID {
		t.Fatalf("idempotent workspace result = %+v", createdAgainValue)
	}

	createdB := call("workspace-create-b", "workspace.create", map[string]any{"path": rootB})
	if !createdB.Result.OK {
		t.Fatalf("workspace.create B = %+v", createdB)
	}
	wsB := workspace(createdB)
	if err := st.CreateSession(context.Background(), "ws-a-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(context.Background(), "ws-a-2", time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(context.Background(), "ws-b-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ws-a-1", "ws-a-2"} {
		if err := st.SetSessionWorkspace(context.Background(), id, wsA.WorkspaceID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSessionWorkspace(context.Background(), "ws-b-1", wsB.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	response := call("session-order", "workspace.insertSessionBefore", map[string]any{
		"workspaceId": wsA.WorkspaceID, "sessionId": "ws-a-2", "beforeSessionId": "ws-a-1",
	})
	if !response.Result.OK {
		t.Fatalf("workspace.insertSessionBefore = %+v", response)
	}
	ordered := workspace(response)
	if len(ordered.SessionIDs) != 2 || ordered.SessionIDs[0] != "ws-a-2" || ordered.SessionIDs[1] != "ws-a-1" {
		t.Fatalf("session order = %+v", ordered.SessionIDs)
	}
	response = call("session-order-invalid", "workspace.insertSessionBefore", map[string]any{
		"workspaceId": wsA.WorkspaceID, "sessionId": "missing-session",
	})
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "workspace-move-invalid" ||
		response.Result.Error.Details["workspaceId"] != wsA.WorkspaceID ||
		response.Result.Error.Details["sessionId"] != "missing-session" ||
		response.Result.Error.Details["beforeSessionId"] != nil {
		t.Fatalf("invalid session move = %+v", response)
	}

	response = call("workspace-order", "workspace.insertBefore", map[string]any{
		"workspaceId": wsB.WorkspaceID, "beforeWorkspaceId": wsA.WorkspaceID,
	})
	if !response.Result.OK {
		t.Fatalf("workspace.insertBefore = %+v", response)
	}
	var workspaceOrder struct {
		WorkspaceIDs []string `json:"workspaceIds"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &workspaceOrder); err != nil {
		t.Fatal(err)
	}
	if len(workspaceOrder.WorkspaceIDs) != 2 || workspaceOrder.WorkspaceIDs[0] != wsB.WorkspaceID {
		t.Fatalf("workspace order = %+v", workspaceOrder.WorkspaceIDs)
	}
	response = call("workspace-order-missing", "workspace.insertBefore", map[string]any{
		"workspaceId": wsA.WorkspaceID, "beforeWorkspaceId": "missing-workspace",
	})
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "workspace-not-found" ||
		response.Result.Error.Details["workspaceId"] != "missing-workspace" {
		t.Fatalf("missing workspace order anchor = %+v", response)
	}

	response = call("workspace-rename", "workspace.rename", map[string]any{"workspaceId": wsA.WorkspaceID, "title": "Project A"})
	if !response.Result.OK || workspace(response).Title != "Project A" {
		t.Fatalf("workspace.rename = %+v", response)
	}
	response = call("workspace-conflict", "workspace.rename", map[string]any{"workspaceId": wsB.WorkspaceID, "title": "Project A"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "workspace-name-conflict" {
		t.Fatalf("workspace rename conflict = %+v", response)
	}
	if response.Result.Error.Details["name"] != "Project A" {
		t.Fatalf("workspace rename conflict details = %#v", response.Result.Error.Details)
	}

	response = call("workspace-archive", "workspace.archiveSession", map[string]any{"sessionId": "ws-a-1"})
	if !response.Result.OK {
		t.Fatalf("workspace.archiveSession = %+v", response)
	}
	var archive struct {
		Archived []string `json:"archivedSessionIds"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &archive); err != nil {
		t.Fatal(err)
	}
	if len(archive.Archived) != 1 || archive.Archived[0] != "ws-a-1" {
		t.Fatalf("archive result = %+v", archive)
	}

	response = call("workspace-list", "workspace.list", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("workspace.list = %+v", response)
	}
	var listing nativeWorkspaceListValue
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	listedA, ok := nativeWorkspaceFind(listing.Items, wsA.WorkspaceID)
	if !ok || len(listedA.SessionIDs) != 2 || len(listing.ArchivedSessionIDs) != 1 {
		t.Fatalf("workspace listing = %+v", listing)
	}

	response = call("workspace-delete", "workspace.delete", map[string]any{"workspaceId": wsB.WorkspaceID})
	if !response.Result.OK {
		t.Fatalf("workspace.delete = %+v", response)
	}
	response = call("workspace-delete-missing", "workspace.delete", map[string]any{"workspaceId": wsB.WorkspaceID})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "workspace-not-found" {
		t.Fatalf("missing workspace.delete = %+v", response)
	}
	if response.Result.Error.Details["workspaceId"] != wsB.WorkspaceID {
		t.Fatalf("missing workspace.delete details = %#v", response.Result.Error.Details)
	}
	response = call("workspace-invalid-path", "workspace.create", map[string]any{"path": filepath.Join(rootA, "missing")})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "workspace-invalid-path" {
		t.Fatalf("invalid workspace.create = %+v", response)
	}
}

func TestNativeSessionCreatePersistsAgentPreset(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetSessionManager(func(ctx context.Context, action, requestedID string) (string, error) {
		if action != "new" {
			t.Fatalf("session action = %q, want new", action)
		}
		id := requestedID
		if id == "" {
			id = "created-preset"
		}
		if err := st.CreateSession(ctx, id, time.Now()); err != nil {
			return "", err
		}
		return id, nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok", `{"type":"client-request","rpcId":"create-preset","method":"session.create","payload":{"sessionId":"created-preset","agentPreset":"code"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.create response = %+v", response)
	}
	config, err := st.GetSessionConfig(context.Background(), "created-preset")
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentPreset != "code" {
		t.Fatalf("created session config = %+v", config)
	}
}

func TestNativeSessionCreateEchoesAdoptedAgentPreset(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	defaultCWD := t.TempDir()
	srv.SetDefaultWorkdir(defaultCWD)
	var got NativeSessionCreateSpec
	srv.SetNativeSessionCreator(func(_ context.Context, spec NativeSessionCreateSpec) (NativeSessionCreateResult, error) {
		got = spec
		return NativeSessionCreateResult{SessionID: "adopted", AgentPreset: "code", CWD: defaultCWD}, nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok",
		`{"type":"client-request","rpcId":"adopt-preset","method":"session.create","payload":{"sessionId":"adopted"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("adopt response = %+v; body=%s", response, rec.Body.String())
	}
	var value map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value["sessionId"] != "adopted" || value["agentPreset"] != "code" {
		t.Fatalf("adopt value = %#v", value)
	}
	if got.AgentPresetRequested {
		t.Fatalf("creator spec = %#v", got)
	}
}

func TestNativeSessionCreateSingleFlightsNamedRetries(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var calls atomic.Int64
	leaderStarted := make(chan struct{})
	release := make(chan struct{})
	srv.SetNativeSessionCreator(func(_ context.Context, spec NativeSessionCreateSpec) (NativeSessionCreateResult, error) {
		if calls.Add(1) == 1 {
			close(leaderStarted)
			<-release
		}
		return NativeSessionCreateResult{
			SessionID: spec.SessionID, AgentPreset: spec.AgentPreset, CWD: spec.CWD,
		}, nil
	})
	inner := srv.Handler()
	followerEntered := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Flight") == "follower" {
			close(followerEntered)
		}
		inner.ServeHTTP(w, r)
	})
	request := func(header string) (*http.Request, *httptest.ResponseRecorder) {
		req := httptest.NewRequest(http.MethodPost, "/api/session.create", strings.NewReader(
			`{"type":"client-request","rpcId":"flight","method":"session.create","payload":{"sessionId":"flight-session"}}`,
		))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("X-Test-Flight", header)
		}
		return req, httptest.NewRecorder()
	}
	firstReq, firstRec := request("")
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstRec, firstReq)
		close(firstDone)
	}()
	select {
	case <-leaderStarted:
	case <-time.After(time.Second):
		t.Fatal("first create did not reach the creator")
	}
	secondReq, secondRec := request("follower")
	secondDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(secondRec, secondReq)
		close(secondDone)
	}()
	select {
	case <-followerEntered:
	case <-time.After(time.Second):
		t.Fatal("second create did not enter the transport")
	}
	// Give the follower one scheduler beat to attach to the in-flight leader.
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first create did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second create did not finish")
	}
	for _, rec := range []*httptest.ResponseRecorder{firstRec, secondRec} {
		response := nativeResponse(t, rec.Body.Bytes())
		if !response.Result.OK || response.Result.Value == nil {
			t.Fatalf("single-flight response = %+v", response)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("creator calls = %d, want 1", calls.Load())
	}
}

func TestNativeSessionCreateSingleFlightChecksFollowerPreset(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	leaderStarted := make(chan struct{})
	release := make(chan struct{})
	srv.SetNativeSessionCreator(func(_ context.Context, spec NativeSessionCreateSpec) (NativeSessionCreateResult, error) {
		close(leaderStarted)
		<-release
		return NativeSessionCreateResult{
			SessionID: spec.SessionID, AgentPreset: "code", CWD: spec.CWD,
		}, nil
	})
	inner := srv.Handler()
	followerEntered := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Flight") == "follower" {
			close(followerEntered)
		}
		inner.ServeHTTP(w, r)
	})
	request := func(rpcID, header, preset string) (*http.Request, *httptest.ResponseRecorder) {
		payload := map[string]any{"sessionId": "flight-preset"}
		if preset != "" {
			payload["agentPreset"] = preset
		}
		encoded, _ := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": rpcID, "method": "session.create", "payload": payload,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/session.create", strings.NewReader(string(encoded)))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("X-Test-Flight", header)
		}
		return req, httptest.NewRecorder()
	}
	leaderReq, leaderRec := request("leader", "", "")
	leaderDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(leaderRec, leaderReq)
		close(leaderDone)
	}()
	select {
	case <-leaderStarted:
	case <-time.After(time.Second):
		t.Fatal("leader create did not reach the creator")
	}
	followerReq, followerRec := request("follower", "follower", "minimal")
	followerDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(followerRec, followerReq)
		close(followerDone)
	}()
	select {
	case <-followerEntered:
	case <-time.After(time.Second):
		t.Fatal("follower create did not enter the transport")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case <-leaderDone:
	case <-time.After(time.Second):
		t.Fatal("leader create did not finish")
	}
	select {
	case <-followerDone:
	case <-time.After(time.Second):
		t.Fatal("follower create did not finish")
	}
	leader := nativeResponse(t, leaderRec.Body.Bytes())
	if !leader.Result.OK || leader.Result.Value == nil {
		t.Fatalf("leader response = %+v", leader)
	}
	follower := nativeResponse(t, followerRec.Body.Bytes())
	if follower.Result.OK || follower.Result.Error == nil ||
		follower.Result.Error.Code != "agent-preset-conflict" ||
		follower.Result.Error.Details["existingPreset"] != "code" ||
		follower.Result.Error.Details["requestedPreset"] != "minimal" {
		t.Fatalf("follower response = %+v", follower)
	}
}

func TestNativeSessionCreateValidatesWorkspaceBeforeCreation(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	called := false
	srv.SetSessionManager(func(ctx context.Context, action, requestedID string) (string, error) {
		called = true
		id := requestedID
		if id == "" {
			id = "created-workspace"
		}
		if err := st.CreateSession(ctx, id, time.Now()); err != nil {
			return "", err
		}
		return id, nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok", `{"type":"client-request","rpcId":"create-missing-workspace","method":"session.create","payload":{"workspaceId":"missing"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "workspace-not-found" ||
		response.Result.Error.Details["workspaceId"] != "missing" {
		t.Fatalf("missing workspace response = %+v", response)
	}
	if called {
		t.Fatal("session manager was called for an unknown workspace")
	}
	metas, err := st.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("invalid workspace created orphan sessions = %+v", metas)
	}

	root := t.TempDir()
	if err := st.CreateWorkspaceWithPath(context.Background(), "workspace-valid", "Valid", root); err != nil {
		t.Fatal(err)
	}
	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok", `{"type":"client-request","rpcId":"create-workspace","method":"session.create","payload":{"workspaceId":"workspace-valid"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("valid workspace response = %+v", response)
	}
	meta, err := st.GetSessionMeta(context.Background(), "created-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if meta.WorkspaceID != "workspace-valid" || !sameWorkspacePath(meta.CWD, root) {
		t.Fatalf("workspace session meta = %#v, want cwd %q", meta, root)
	}
}

func TestNativeSessionCreateRejectsWorkspaceAndCWDTogether(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	called := false
	srv.SetSessionManager(func(context.Context, string, string) (string, error) {
		called = true
		return "should-not-create", nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.create", "tok", `{"type":"client-request","rpcId":"create-conflict","method":"session.create","payload":{"workspaceId":"workspace","cwd":"C:/tmp"}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("workspace and cwd response = %+v", response)
	}
	if called {
		t.Fatal("session manager was called for conflicting create inputs")
	}
}

func TestNativeHostDirectoryBrowseAndCreate(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	root := t.TempDir()
	srv.SetDefaultWorkdir(root)
	for _, name := range []string{"alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": method, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("directory-list", "host.listDirectory", map[string]any{"path": root})
	if !response.Result.OK {
		t.Fatalf("host.listDirectory response = %+v", response)
	}
	var listing workspaceDirectoryListing
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != filepath.Clean(root) || listing.Truncated || len(listing.Crumbs) == 0 {
		t.Fatalf("directory listing metadata = %+v", listing)
	}
	names := make(map[string]workspaceDirectoryEntry, len(listing.Entries))
	for _, entry := range listing.Entries {
		names[entry.Name] = entry
	}
	if names["alpha"].Path != filepath.Join(root, "alpha") || !names[".hidden"].Hidden {
		t.Fatalf("directory entries = %+v", names)
	}
	if _, ok := names["file.txt"]; ok {
		t.Fatalf("file was returned as directory: %+v", names)
	}

	response = call("directory-default", "host.listDirectory", map[string]any{})
	if !response.Result.OK {
		t.Fatalf("default host.listDirectory response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != filepath.Clean(root) {
		t.Fatalf("default directory path = %q, want %q", listing.Path, filepath.Clean(root))
	}

	response = call("directory-create", "host.createDirectory", map[string]any{"path": root, "name": "created"})
	if !response.Result.OK {
		t.Fatalf("host.createDirectory response = %+v", response)
	}
	var created struct {
		Path string `json:"path"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &created); err != nil {
		t.Fatal(err)
	}
	if created.Path != filepath.Join(root, "created") {
		t.Fatalf("created path = %q", created.Path)
	}

	response = call("directory-exists", "host.createDirectory", map[string]any{"path": root, "name": "created"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-exists" {
		t.Fatalf("duplicate directory response = %+v", response)
	}
	response = call("directory-invalid-name", "host.createDirectory", map[string]any{"path": root, "name": "../escape"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-create-failed" {
		t.Fatalf("invalid directory name response = %+v", response)
	}
	response = call("directory-relative", "host.listDirectory", map[string]any{"path": "relative"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-unreadable" {
		t.Fatalf("relative directory response = %+v", response)
	}
}

func TestNativeHostPickDirectoryUnavailableOutsideWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native picker is intentionally interactive on Windows")
	}
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/host.pickDirectory", "tok", `{"type":"client-request","rpcId":"pick","method":"host.pickDirectory","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "directory-picker-unavailable" {
		t.Fatalf("host.pickDirectory response = %+v", response)
	}
}

func TestNativeSessionHistorySeedsProjectionBeforeSelectingMessagePage(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-paged", []session.Event{
		{Seq: 0, Type: session.EventTurnStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"first"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"one"}`)},
		{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
		{Seq: 4, Type: session.EventTurnStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"turn":2}`)},
		{Seq: 5, Type: session.EventUserMessage, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{"text":"second"}`)},
		{Seq: 6, Type: session.EventAssistantMessage, At: time.UnixMilli(1006), Version: session.EventVersion, Data: json.RawMessage(`{"text":"two"}`)},
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"history-page","method":"session.history","payload":{"sessionId":"native-paged","maxMessages":1}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	var history struct {
		Events  []nativeHistoryEntry `json:"events"`
		HasMore bool                 `json:"hasMore"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 || history.Events[0].Event.Seq != 6 || !history.HasMore {
		t.Fatalf("paged history = %+v", history)
	}
	var assistant struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(history.Events[0].Event.Data, &assistant); err != nil {
		t.Fatal(err)
	}
	if assistant.Turn != 2 {
		t.Fatalf("page projection turn = %d, want 2", assistant.Turn)
	}
}

func TestNativeHistoryPageBoundsSkipsReplacementAndKeepsMessageSources(t *testing.T) {
	input := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","sourceEventSeqs":[1,2]}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":1,"end":2}}`)},
	}
	cursor := newNativeProjectionCursor()
	projected := make([]nativeSessionEvent, 0, len(input))
	for _, event := range input {
		projected = append(projected, cursor.project("paged", event))
	}

	start, end := nativeHistoryPageBounds(projected, nil, 1)
	if start != 0 || end != len(projected) {
		t.Fatalf("replacement page bounds = (%d,%d), want (0,%d)", start, end, len(projected))
	}

	start, end = nativeHistoryPageBounds(projected, uint64Ptr(3), 1)
	if start != 0 || end != 2 {
		t.Fatalf("source-group page bounds = (%d,%d), want (0,2)", start, end)
	}
}

func TestNativeHistoryTransportBoundsCapsLargeStreamedMessage(t *testing.T) {
	start, end := nativeHistoryTransportBounds(0, nativeHistoryEventLimit+123)
	if got := end - start; got != nativeHistoryEventLimit {
		t.Fatalf("transport event window = %d, want %d", got, nativeHistoryEventLimit)
	}
	if start != 123 || end != nativeHistoryEventLimit+123 {
		t.Fatalf("transport event bounds = (%d,%d), want (%d,%d)", start, end, 123, nativeHistoryEventLimit+123)
	}

	start, end = nativeHistoryTransportBounds(7, 41)
	if start != 7 || end != 41 {
		t.Fatalf("small transport event bounds = (%d,%d), want (7,41)", start, end)
	}
}

func uint64Ptr(value uint64) *uint64 {
	return &value
}

func TestNativeProjectionUsesOneDSHShapeForReplayAndLive(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"hello"}`)},
		{Seq: 3, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer","toolCalls":[{"ID":"c1","Name":"read","Arguments":"{}"}]}`)},
		{Seq: 4, Type: session.EventToolResult, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"c1","name":"read","output":"ok"}`)},
	}
	replayCursor := newNativeProjectionCursor()
	liveCursor := newNativeProjectionCursor()
	for _, ev := range events {
		replayed := replayCursor.project("s1", ev)
		live := liveCursor.project("s1", ev)
		if string(replayed.Data) != string(live.Data) || replayed.Type != live.Type || replayed.SurfaceOp != live.SurfaceOp {
			t.Fatalf("replay/live projection differs for seq %d: replay=%s live=%s", ev.Seq, replayed.Data, live.Data)
		}
	}
	assistant := replayCursor.project("s1", events[2])
	var assistantData struct {
		Turn    int `json:"turn"`
		Step    int `json:"step"`
		Message struct {
			ID      string `json:"id"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(assistant.Data, &assistantData); err != nil {
		t.Fatal(err)
	}
	if assistantData.Turn != 1 || assistantData.Step != 0 || assistantData.Message.ID == "" || len(assistantData.Message.Content) != 2 {
		t.Fatalf("native assistant projection = %+v", assistantData)
	}
}

func TestNativeProjectionFoldsSurfaceReplacementAndDiagnostics(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1000), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"answer"}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"summary","surfaceOp":{"op":"replace","start":1,"end":2}}`)},
		{Seq: 4, Type: session.EventLLMRetry, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"attempt":2,"maxRetries":3,"delayMs":25,"error":"temporary"}`)},
		{Seq: 5, Type: session.EventJobStart, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"jobId":"j1"}`)},
	}
	cursor := newNativeProjectionCursor()
	projected := make([]nativeSessionEvent, 0, len(events))
	for _, ev := range events {
		projected = append(projected, cursor.project("s1", ev))
	}
	if got := projected[2].SurfaceOp; got == nil {
		t.Fatal("compaction projection is missing surfaceOp")
	}
	var surface struct {
		Op    string `json:"op"`
		Start uint64 `json:"start"`
		End   uint64 `json:"end"`
	}
	if err := json.Unmarshal(nativeJSONBytes(projected[2].SurfaceOp), &surface); err != nil {
		t.Fatal(err)
	}
	if surface.Op != "replace" || surface.Start != 1 || surface.End != 2 || len(projected[2].SourceEventSeqs) != 2 {
		t.Fatalf("surface projection = %+v, sources=%v", surface, projected[2].SourceEventSeqs)
	}
	var retry struct {
		RetryID string `json:"retryId"`
		Retry   int    `json:"retry"`
		Failure struct {
			Message string `json:"message"`
		} `json:"failure"`
	}
	if err := json.Unmarshal(projected[3].Data, &retry); err != nil {
		t.Fatal(err)
	}
	if retry.RetryID == "" || retry.Retry != 2 || retry.Failure.Message != "temporary" {
		t.Fatalf("retry projection = %+v", retry)
	}
	if !projected[4].Ignorable {
		t.Fatal("non-DSH event must be marked ignorable")
	}
}

func TestNativeRPCRejectsMethodMismatchAndAuth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"bad-1","method":"host.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("method mismatch response = %+v", response)
	}
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "", `{"type":"client-request","rpcId":"bad-2","method":"session.list","payload":{}}`); rec.Code != 401 {
		t.Fatalf("missing auth status = %d", rec.Code)
	}
}

func TestNativeSettingsOpenDocument(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	called := false
	srv.SetNativeSettingsDocumentOpener(func(context.Context) error {
		called = true
		return nil
	})
	body := `{"type":"client-request","rpcId":"settings-open","method":"settings.openDocument","payload":{}}`
	rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.openDocument", "tok", body)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || !called {
		t.Fatalf("settings.openDocument response=%+v called=%v", response, called)
	}
	var value map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil || value["opened"] != true {
		t.Fatalf("settings.openDocument value=%+v", response.Result.Value)
	}

	srv2, _ := newTestServer(t, "tok")
	rec = doReqBody(t, srv2.Handler(), "POST", "/api/settings.openDocument", "tok", body)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "not-supported" {
		t.Fatalf("unwired settings.openDocument response=%+v", response)
	}
}

func TestNativeSettingsDescribeAndMutate(t *testing.T) {
	srv, _ := newTestServer(t, "tok")

	rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.describe", "tok", `{"type":"client-request","rpcId":"settings-1","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe response = %+v", response)
	}
	var described struct {
		Namespaces []map[string]any `json:"namespaces"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatal(err)
	}
	if len(described.Namespaces[0]) == 0 || described.Namespaces[0]["ns"] != nativeSettingsOnboarding {
		t.Fatalf("settings namespaces = %+v", described.Namespaces)
	}
	if described.Namespaces[0]["value"].(map[string]any)["welcomeNoticeVersion"] != nativeWelcomeNoticeVersion {
		t.Fatalf("welcome notice acknowledgement = %+v", described.Namespaces[0]["value"])
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/settings.mutate", "tok", `{"type":"client-request","rpcId":"settings-2","method":"settings.mutate","payload":{"ns":"ui-onboarding","ops":[{"op":"set","path":["welcomeNoticeVersion"],"value":"2026-08-13.1"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.mutate response = %+v", response)
	}
	var view struct {
		NS       string         `json:"ns"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view.NS != nativeSettingsOnboarding || view.Value["welcomeNoticeVersion"] != "2026-08-13.1" || view.Revision != 1 {
		t.Fatalf("settings view = %+v", view)
	}
}

func TestNativeSettingsCoreNamespacesDefaultsAndValidation(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.describe", "tok",
		`{"type":"client-request","rpcId":"settings-core","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe = %+v", response.Result.Error)
	}
	encoded, _ := json.Marshal(response.Result.Value)
	var described struct {
		Namespaces []map[string]any `json:"namespaces"`
	}
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatal(err)
	}
	byNS := make(map[string]map[string]any, len(described.Namespaces))
	for _, namespace := range described.Namespaces {
		byNS[namespace["ns"].(string)] = namespace
	}
	for _, namespace := range []string{
		"ui-theme", "locale", "ui-conversation", "permission", "shell",
		"agent-loop", "agent-default-model", "web-search-deepseek",
	} {
		if byNS[namespace] == nil {
			t.Fatalf("%s namespace missing from %#v", namespace, byNS)
		}
	}
	theme := byNS["ui-theme"]["value"].(map[string]any)
	if theme["preference"] != "system" || theme["fontSize"] != float64(14) {
		t.Fatalf("theme defaults = %#v", theme)
	}
	agentLoop := byNS["agent-loop"]["value"].(map[string]any)
	if agentLoop["maxParallelToolCalls"] != float64(10) {
		t.Fatalf("agent loop defaults = %#v", agentLoop)
	}

	rec = doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.update", "tok",
		`{"type":"client-request","rpcId":"settings-invalid","method":"settings.update","payload":{"ns":"ui-theme","patch":{"fontSize":99}}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "settings-rejected" {
		t.Fatalf("invalid theme response = %+v", response)
	}
}

func TestNativeSettingsWebSearchSecretNeverRidesDescribeOrWriteView(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.update", "tok",
		`{"type":"client-request","rpcId":"search-secret","method":"settings.update","payload":{"ns":"web-search-deepseek","patch":{"apiKey":"literal-secret"}}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("web search update = %+v", response.Result.Error)
	}
	encoded, _ := json.Marshal(response.Result.Value)
	var view struct {
		Base  map[string]any `json:"base"`
		User  map[string]any `json:"user"`
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []map[string]any{view.Base, view.User, view.Value} {
		if _, exists := layer["apiKey"]; exists {
			t.Fatalf("api key crossed response boundary: %#v", layer)
		}
	}
	settings, err := st.GetSettings(context.Background())
	if err != nil || !strings.Contains(settings[nativeSettingsKey("web-search-deepseek")], "literal-secret") {
		t.Fatalf("secret was not durably accepted: raw=%q err=%v", settings[nativeSettingsKey("web-search-deepseek")], err)
	}
}

func TestNativeSettingsDocumentUpdatedFanout(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	events := make(chan [2]any, 2)
	unsubscribe := srv.subscribeNativeSettingsDocumentUpdated(func(namespace string, revision int) {
		events <- [2]any{namespace, revision}
	})
	t.Cleanup(unsubscribe)
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.update", "tok",
		`{"type":"client-request","rpcId":"settings-event","method":"settings.update","payload":{"ns":"ui-conversation","patch":{"busyEnter":"steer"}}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.update = %+v", response.Result.Error)
	}
	select {
	case event := <-events:
		if event[0] != "ui-conversation" || event[1] != 1 {
			t.Fatalf("settings event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("settings/document-updated was not emitted")
	}
}

func TestNativeSettingsMutationSurvivesServerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "settings-restart.db")
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(st, "tok", "")
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	body := `{"type":"client-request","rpcId":"settings-persist","method":"settings.update","payload":{"ns":"ui-onboarding","patch":{"welcomeNoticeVersion":"persisted-version"}}}`
	rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.update", "tok", body)
	if response := nativeResponse(t, rec.Body.Bytes()); !response.Result.OK {
		t.Fatalf("settings.update response = %+v", response)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	srv2, err := New(st2, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	rec = doReqBody(t, srv2.Handler(), "POST", "/api/settings.describe", "tok", `{"type":"client-request","rpcId":"settings-reload","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe after restart = %+v", response)
	}
	var described struct {
		Namespaces []struct {
			Value    map[string]any `json:"value"`
			Revision int            `json:"revision"`
		} `json:"namespaces"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatal(err)
	}
	if len(described.Namespaces) < 2 || described.Namespaces[0].Value["welcomeNoticeVersion"] != "persisted-version" || described.Namespaces[0].Revision != 1 {
		t.Fatalf("reloaded settings = %+v", described.Namespaces)
	}
}

type nativeSettingsFailStore struct {
	store.Store
	failKey string
}

func (s *nativeSettingsFailStore) SetSetting(ctx context.Context, key, value string) error {
	if key == s.failKey {
		return errors.New("injected settings write failure")
	}
	return s.Store.SetSetting(ctx, key, value)
}

func TestNativeAgentPresetSettingsRollsBackScalarOnDocumentWriteFailure(t *testing.T) {
	base, err := store.OpenSQLite(filepath.Join(t.TempDir(), "settings-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	srv, err := New(&nativeSettingsFailStore{Store: base, failKey: nativeSettingsKey(nativeSettingsAgentPresets)}, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.update", "tok", `{"type":"client-request","rpcId":"settings-rollback","method":"settings.update","payload":{"ns":"agent-presets","patch":{"default":"minimal"}}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "settings-rejected" {
		t.Fatalf("settings rollback response = %+v", response)
	}
	settings, err := base.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings["agent_preset"] != "" {
		t.Fatalf("scalar setting survived failed document write: %#v", settings)
	}
	srv.nativeSettingsMu.Lock()
	document := srv.nativeSettings[nativeSettingsAgentPresets]
	srv.nativeSettingsMu.Unlock()
	if document.Revision != 0 || len(document.Value) != 0 {
		t.Fatalf("in-memory preset advanced after failed write: %+v", document)
	}
}

func TestNativeLLMCatalogUsesSanitizedConfig(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{
			{
				"id": "deepseek-official", "name": "DeepSeek", "available": true,
				"configured": true, "env_var": "DEEPSEEK_API_KEY", "model": "deepseek-v4-flash", "candidates": []string{"deepseek-v4-flash", "deepseek-v4-pro"},
			},
			{
				"id": "deepseek-new", "name": "DeepSeek New", "available": true,
				"configured": true, "model": "deepseek-v4-flash", "models": []map[string]any{
					{"id": "deepseek-v4-flash"}, {"id": "deepseek-v4-pro"},
				},
			},
			{
				"id": "dormant", "name": "Dormant", "available": false,
				"configured": false, "model": "hidden-model", "candidates": []string{"hidden-candidate"},
			},
		}}
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/llm.providers", "tok", `{"type":"client-request","rpcId":"llm-1","method":"llm.providers","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.providers response = %+v", response)
	}
	var providers struct {
		Providers []map[string]any `json:"providers"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 3 || providers.Providers[0]["provider"] != "deepseek-official" {
		t.Fatalf("llm providers = %+v", providers.Providers)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/llm.models", "tok", `{"type":"client-request","rpcId":"llm-2","method":"llm.models","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.models response = %+v", response)
	}
	var models struct {
		Groups []map[string]any `json:"groups"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Groups) != 2 {
		t.Fatalf("llm model groups = %+v", models.Groups)
	}
	if got := models.Groups[0]["id"]; got != "deepseek-official" {
		t.Fatalf("first model group = %+v", models.Groups)
	}
	firstModels, ok := models.Groups[0]["models"].([]any)
	if !ok || len(firstModels) != 1 {
		t.Fatalf("single configured model group = %+v", models.Groups[0])
	}
	if got := models.Groups[1]["id"]; got != "deepseek-new" {
		t.Fatalf("second model group = %+v", models.Groups)
	}
	secondModels, ok := models.Groups[1]["models"].([]any)
	if !ok || len(secondModels) != 2 {
		t.Fatalf("explicit configured models = %+v", models.Groups[1])
	}
}

func TestNativeLLMCatalogPreservesOwnedModelMetadata(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{{
			"id": "deepseek-official", "name": "DeepSeek", "available": true, "configured": true,
			"catalog_models": []map[string]any{{
				"id": "deepseek-v4-flash", "name": "DeepSeek-V4-Flash", "contextWindow": 1000000,
				"maxTokens": 256000, "reasoning": true, "tools": true, "vision": false, "audio": false,
				"input": []string{"text"}, "reasoningEfforts": map[string]any{"off": nil, "high": "high"},
				"defaultEffort": "high",
			}},
		}}}
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/llm.models", "tok", `{"type":"client-request","rpcId":"llm-owned","method":"llm.models","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.models response = %+v", response)
	}
	var value struct {
		Groups []struct {
			Models []map[string]any `json:"models"`
		} `json:"groups"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Groups) != 1 || len(value.Groups[0].Models) != 1 {
		t.Fatalf("owned model catalog = %+v", value)
	}
	model := value.Groups[0].Models[0]
	if model["contextWindow"] != float64(1000000) || model["maxTokens"] != float64(256000) ||
		model["reasoning"] != true || model["tools"] != true || model["vision"] != false || model["audio"] != false {
		t.Fatalf("owned model metadata = %+v", model)
	}
	if model["defaultEffort"] != "high" {
		t.Fatalf("owned model default effort = %+v", model["defaultEffort"])
	}
	input, ok := model["input"].([]any)
	if !ok || len(input) != 1 || input[0] != "text" {
		t.Fatalf("owned model input metadata = %+v", model["input"])
	}
	efforts, ok := model["reasoningEfforts"].(map[string]any)
	if !ok || efforts["high"] != "high" {
		t.Fatalf("owned model reasoning metadata = %+v", model["reasoningEfforts"])
	}
}

func TestNativeMuxWebSocketSendsSubscriptionBaseline(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-ws", nil)
	queueItems := []QueueItem{{ID: "q-1", Text: "queued prompt", Placement: "queued"}}
	var queueCalls atomic.Int32
	var jobCalls atomic.Int32
	srv.SetQueueManager(func(context.Context, string) ([]QueueItem, error) {
		queueCalls.Add(1)
		return queueItems, nil
	}, func(_ context.Context, _ string, text string, _ []llm.ContentBlock, meta PromptMeta) (QueueItem, error) {
		item := QueueItem{ID: "q-2", Text: text, Placement: "queued"}
		queueItems = append(queueItems, item)
		return item, nil
	}, nil)
	srv.SetJobsProvider(func(context.Context, string) ([]map[string]any, error) {
		jobCalls.Add(1)
		return []map[string]any{{
			"id": "job-1", "kind": "bash", "label": "build", "status": "running",
			"started_at": time.UnixMilli(1234).UTC(),
		}}, nil
	})
	var emit func(session.Event)
	registered := make(chan struct{})
	srv.SetEventSource(func(_ string, callback func(session.Event)) func() {
		emit = callback
		close(registered)
		return func() {}
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUta2V5\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	frame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Payload nativeSubscribedFrame `json:"payload"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.Type != "session/subscribed" || envelope.Payload.SessionID != "native-ws" || envelope.Payload.LastSeq != -1 {
		t.Fatalf("subscription frame = %s", frame)
	}
	queueFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var queueEnvelope struct {
		Payload struct {
			Type      string           `json:"type"`
			SessionID string           `json:"sessionId"`
			Items     []map[string]any `json:"items"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(queueFrame, &queueEnvelope); err != nil {
		t.Fatal(err)
	}
	if queueEnvelope.Payload.Type != "session/queue" || queueEnvelope.Payload.SessionID != "native-ws" || len(queueEnvelope.Payload.Items) != 1 {
		t.Fatalf("queue baseline frame = %s", queueFrame)
	}
	jobFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var jobEnvelope struct {
		Payload struct {
			Type      string           `json:"type"`
			SessionID string           `json:"sessionId"`
			Jobs      []map[string]any `json:"jobs"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(jobFrame, &jobEnvelope); err != nil {
		t.Fatal(err)
	}
	if jobEnvelope.Payload.Type != "session/jobs" || jobEnvelope.Payload.SessionID != "native-ws" || len(jobEnvelope.Payload.Jobs) != 1 || jobEnvelope.Payload.Jobs[0]["startedAt"] != float64(1234) {
		t.Fatalf("jobs baseline frame = %s", jobFrame)
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("mux did not register an event callback")
	}
	queueRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/sessions/native-ws/queue", strings.NewReader(`{"text":"second prompt"}`))
	if err != nil {
		t.Fatal(err)
	}
	queueRequest.Header.Set("Authorization", "Bearer tok")
	queueRequest.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(queueRequest)
	if err != nil {
		t.Fatal(err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("queue enqueue status = %d", response.StatusCode)
	}
	queueUpdateFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var queueUpdateEnvelope struct {
		Payload struct {
			Type  string           `json:"type"`
			Items []map[string]any `json:"items"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(queueUpdateFrame, &queueUpdateEnvelope); err != nil {
		t.Fatal(err)
	}
	if queueUpdateEnvelope.Payload.Type != "session/queue" || len(queueUpdateEnvelope.Payload.Items) != 2 {
		t.Fatalf("queue update frame = %s", queueUpdateFrame)
	}
	queueCallsBeforeEvent := queueCalls.Load()
	jobCallsBeforeEvent := jobCalls.Load()
	emit(session.Event{Seq: 1, Type: session.EventPlanMode, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{"active":true}`)})
	var eventFrame []byte
	for {
		eventFrame, err = readNativeTextFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var methodEnvelope struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(eventFrame, &methodEnvelope); err != nil {
			t.Fatal(err)
		}
		if methodEnvelope.Method == "session/event" {
			break
		}
	}
	projectionFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var eventEnvelope struct {
		Payload nativeSessionEventFrame `json:"payload"`
	}
	if err := json.Unmarshal(eventFrame, &eventEnvelope); err != nil {
		t.Fatal(err)
	}
	if eventEnvelope.Payload.Type != "session/event" || eventEnvelope.Payload.Event.Seq != 1 {
		t.Fatalf("live event frame = %s", eventFrame)
	}
	var projectionEnvelope struct {
		Payload nativeProjectionFrame `json:"payload"`
	}
	if err := json.Unmarshal(projectionFrame, &projectionEnvelope); err != nil {
		t.Fatal(err)
	}
	if projectionEnvelope.Payload.Type != "session/projection" || projectionEnvelope.Payload.Key != "plan" || projectionEnvelope.Payload.Seq != 1 {
		t.Fatalf("live projection frame = %s", projectionFrame)
	}
	if got := queueCalls.Load(); got != queueCallsBeforeEvent {
		t.Fatalf("queue provider called for unrelated plan event: before=%d after=%d", queueCallsBeforeEvent, got)
	}
	if got := jobCalls.Load(); got != jobCallsBeforeEvent {
		t.Fatalf("job provider called for unrelated plan event: before=%d after=%d", jobCallsBeforeEvent, got)
	}
}

func TestNativeMuxWebSocketForwardsConfigurationOwnerEvents(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-owner-events", nil)
	registered := make(chan struct{})
	srv.SetEventSource(func(_ string, _ func(session.Event)) func() {
		close(registered)
		return func() {}
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUta2V5\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("mux did not register an event callback")
	}

	srv.NotifyNativeCredentialUpdated("TEST_API_KEY")
	srv.NotifyNativeLLMAdaptersUpdated()

	seen := map[string][]any{}
	for len(seen) < 2 {
		frame, err := readNativeTextFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			nativeRemoteEventFrame
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatalf("decode remote event frame %s: %v", frame, err)
		}
		if envelope.Type != "host/remote-event" {
			continue
		}
		seen[envelope.Event] = envelope.Args
	}
	if args := seen["credentials/updated"]; len(args) != 1 || args[0] != "TEST_API_KEY" {
		t.Fatalf("credentials/updated args = %#v", args)
	}
	if args := seen["llm/adapters-updated"]; len(args) != 0 {
		t.Fatalf("llm/adapters-updated args = %#v, want payload-free", args)
	}
}

func TestNativeMuxWebSocketSubscribesSessionCreatedAfterConnect(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-existing", nil)
	srv.SetSessionManager(func(ctx context.Context, action, requestedID string) (string, error) {
		if action != "new" {
			t.Fatalf("session action = %q, want new", action)
		}
		if err := st.CreateSession(ctx, requestedID, time.Now().UTC()); err != nil {
			return "", err
		}
		return requestedID, nil
	})
	var callbacksMu sync.Mutex
	callbacks := make(map[string]func(session.Event))
	registered := make(chan string, 2)
	srv.SetEventSource(func(sessionID string, callback func(session.Event)) func() {
		callbacksMu.Lock()
		callbacks[sessionID] = callback
		callbacksMu.Unlock()
		registered <- sessionID
		return func() {
			callbacksMu.Lock()
			delete(callbacks, sessionID)
			callbacksMu.Unlock()
		}
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUta2V5\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	baseline, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var baselineEnvelope struct {
		Payload nativeSubscribedFrame `json:"payload"`
	}
	if err := json.Unmarshal(baseline, &baselineEnvelope); err != nil {
		t.Fatal(err)
	}
	if baselineEnvelope.Payload.SessionID != "native-existing" {
		t.Fatalf("baseline frame = %s", baseline)
	}
	select {
	case sessionID := <-registered:
		if sessionID != "native-existing" {
			t.Fatalf("initial callback session = %q", sessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mux did not register the initial event callback")
	}

	createRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/session.create", strings.NewReader(`{"type":"client-request","rpcId":"create-live","method":"session.create","payload":{"sessionId":"native-live"}}`))
	if err != nil {
		t.Fatal(err)
	}
	createRequest.Header.Set("Authorization", "Bearer tok")
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse, err := http.DefaultClient.Do(createRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusOK {
		t.Fatalf("session.create status = %d", createResponse.StatusCode)
	}
	var createBody nativeRPCResponse
	if err := json.NewDecoder(createResponse.Body).Decode(&createBody); err != nil {
		t.Fatal(err)
	}
	if !createBody.Result.OK {
		t.Fatalf("session.create response = %+v", createBody)
	}

	var subscribed struct {
		Payload nativeSubscribedFrame `json:"payload"`
	}
	for {
		frame, readErr := readNativeTextFrame(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := json.Unmarshal(frame, &subscribed); err != nil {
			t.Fatal(err)
		}
		if subscribed.Payload.Type == "session/subscribed" && subscribed.Payload.SessionID == "native-live" {
			break
		}
	}
	if subscribed.Payload.LastSeq != -1 {
		t.Fatalf("new session baseline = %+v", subscribed.Payload)
	}
	select {
	case sessionID := <-registered:
		if sessionID != "native-live" {
			t.Fatalf("new callback session = %q", sessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mux did not register the new session event callback")
	}
	callbacksMu.Lock()
	emit := callbacks["native-live"]
	callbacksMu.Unlock()
	if emit == nil {
		t.Fatal("new session event callback is nil")
	}
	emit(session.Event{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"hi"}`)})
	var eventEnvelope struct {
		Method  string                  `json:"method"`
		Payload nativeSessionEventFrame `json:"payload"`
	}
	for {
		frame, readErr := readNativeTextFrame(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := json.Unmarshal(frame, &eventEnvelope); err != nil {
			t.Fatal(err)
		}
		if eventEnvelope.Method == "session/event" {
			break
		}
	}
	if eventEnvelope.Payload.SessionID != "native-live" || eventEnvelope.Payload.Event.Seq != 1 || eventEnvelope.Payload.Event.Type != session.EventUserMessage {
		t.Fatalf("new session live event = %+v", eventEnvelope.Payload)
	}
}

func TestNativeMuxWebSocketReplaysPendingApprovalAndQuestionRequests(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-interaction-ws", nil)
	srv.SetEventSource(func(_ string, callback func(session.Event)) func() { return func() {} })
	srv.SetInteractionManager(
		func(_ context.Context, sessionID string) ([]interact.Request, error) {
			if sessionID != "native-interaction-ws" {
				return nil, nil
			}
			return []interact.Request{
				{ID: "approval-1", ToolName: "bash", Prompt: "Allow bash", Status: interact.StatusPending},
				{ID: "question-1", Status: interact.StatusPending, Questions: []interact.Question{{
					ID: "mode", Question: "Which mode?", Options: []interact.QuestionOption{{Label: "safe"}},
				}}},
			}, nil
		},
		func(context.Context, string, string, interact.ApprovalStatus, string) error { return nil },
	)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1pbnRlcmFjdGlvbg==\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q, err=%v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if _, err := readNativeTextFrame(reader); err != nil {
		t.Fatal(err)
	}
	readRequest := func() (string, map[string]any) {
		t.Helper()
		frame, err := readNativeTextFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Type    string         `json:"type"`
			RPCID   string         `json:"rpcId"`
			Method  string         `json:"method"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Type != nativeRPCTypeServerRequest {
			t.Fatalf("interaction envelope = %s", frame)
		}
		return envelope.Method + ":" + envelope.RPCID, envelope.Payload
	}
	approval, approvalPayload := readRequest()
	if !strings.HasPrefix(approval, "approval/requested:approval-1") || approvalPayload["approvalId"] != "approval-1" {
		t.Fatalf("approval replay = %q / %#v", approval, approvalPayload)
	}
	question, questionPayload := readRequest()
	if !strings.HasPrefix(question, "question/requested:question-1") || questionPayload["sessionId"] != "native-interaction-ws" {
		t.Fatalf("question replay = %q / %#v", question, questionPayload)
	}
}

func TestNativeHostWebSocketSendsHostBaselineAndStatus(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-host", nil)
	var emit func(session.Event)
	registered := make(chan struct{})
	srv.SetEventSource(func(_ string, callback func(session.Event)) func() {
		emit = callback
		close(registered)
		return func() {}
	})
	srv.SetSessionStatusProvider(func(_ context.Context, _ store.SessionMeta) SessionStatus {
		return SessionStatus{State: "ongoing"}
	})
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.host HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUtaG9zdA==\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	frame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Method  string         `json:"method"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/session-added" || envelope.Payload["sessionId"] != "native-host" || envelope.Payload["blank"] != true {
		t.Fatalf("host session baseline = %s", frame)
	}
	statusFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(statusFrame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/session-status" || envelope.Payload["running"] != true {
		t.Fatalf("host status baseline = %s", statusFrame)
	}
	archivedFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(archivedFrame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/archived-sessions-changed" {
		t.Fatalf("host archive baseline = %s", archivedFrame)
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not register an event callback")
	}
	emit(session.Event{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{}`)})
	transition, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(transition, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/session-status" || envelope.Payload["running"] != true {
		t.Fatalf("host status transition = %s", transition)
	}
	emit(session.Event{Seq: 2, Type: session.EventToolResult, At: time.UnixMilli(2002), Version: session.EventVersion, Data: json.RawMessage(`{"isError":true,"error":"tool failed"}`)})
	errorFrame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(errorFrame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/agent-error" || envelope.Payload["message"] != "tool failed" {
		t.Fatalf("host agent error = %s", errorFrame)
	}
}

func TestNativeHostDescribeUsesLiveAgentsAndDefaultSelection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "durable-but-not-live", nil)
	srv.SetSessionStatusProvider(func(_ context.Context, _ store.SessionMeta) SessionStatus {
		return SessionStatus{State: "idle"}
	})
	srv.SetLiveAgentCounter(func() int { return 7 })
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"llm_provider": "provider-live", "model": "model-live"}
	})

	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/host.describe", "tok",
		`{"type":"client-request","rpcId":"host-1","method":"host.describe","payload":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("host.describe = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Result struct {
			Value struct {
				Provider         string `json:"provider"`
				Model            string `json:"model"`
				AttachedSessions int    `json:"attachedSessions"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	view := response.Result.Value
	if view.Provider != "provider-live" || view.Model != "model-live" {
		t.Fatalf("model selection = %#v, want provider-live/model-live", view)
	}
	if view.AttachedSessions != 7 {
		t.Fatalf("attachedSessions = %d, want live registry count 7", view.AttachedSessions)
	}
}

func TestNativeHostWebSocketReconcilesSessionsAfterConnect(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	defer srv.Close()
	seedSession(t, st, "native-host-reconcile", nil)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.host HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: cmVjb25jaWxlLWhvc3Q=\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	for range 2 {
		if _, err := readNativeTextFrame(reader); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSession(context.Background(), "native-host-new", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	frame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal("new session frame: ", err)
	}
	var envelope struct {
		Method  string         `json:"method"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/session-added" || envelope.Payload["sessionId"] != "native-host-new" {
		t.Fatalf("new session frame = %s", frame)
	}
	if err := st.ArchiveSession(context.Background(), "native-host-new", true); err != nil {
		t.Fatal(err)
	}
	removed, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal("removed session frame: ", err)
	}
	if err := json.Unmarshal(removed, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/session-removed" || envelope.Payload["sessionId"] != "native-host-new" {
		t.Fatalf("removed session frame = %s", removed)
	}
	archived, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal("archive frame: ", err)
	}
	if err := json.Unmarshal(archived, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "host/archived-sessions-changed" {
		t.Fatalf("archive frame = %s", archived)
	}
}

func TestNativeSessionForkCopiesCompletedTurnPrefixAndMetadata(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	forkAdded := make(chan string, 2)
	removeForkListener := srv.subscribeNativeMuxSessionAdded(func(sessionID string) {
		forkAdded <- sessionID
	})
	defer removeForkListener()
	workspacePath := t.TempDir()
	if err := st.CreateWorkspaceWithPath(context.Background(), "fork-workspace", "Fork workspace", workspacePath); err != nil {
		t.Fatal(err)
	}
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"first"}`)},
		{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 4, Type: session.EventSessionTitle, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{"title":"Boundary title","messageSeqs":[],"source":{"kind":"fallback"}}`)},
		{Seq: 5, Type: session.EventTurnStart, At: time.UnixMilli(1005), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 6, Type: session.EventUserMessage, At: time.UnixMilli(1006), Version: session.EventVersion, Data: json.RawMessage(`{"text":"open"}`)},
	}
	seedSession(t, st, "fork-source", events)
	ctx := context.Background()
	if err := st.SetSessionTitle(ctx, "fork-source", "Fork title", session.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionWorkspace(ctx, "fork-source", "fork-workspace"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionCWD(ctx, "fork-source", workspacePath); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionConfig(ctx, "fork-source", store.SessionConfig{AgentPreset: "code", Provider: "openai", Model: "gpt-4o", Permission: "full"}); err != nil {
		t.Fatal(err)
	}

	call := func(id string, payload map[string]any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": "session.fork", "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.fork", "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	anchor := uint64(2)
	response := call("fork-prefix", map[string]any{"sessionId": "fork-source", "atSeq": anchor})
	if !response.Result.OK {
		t.Fatalf("session.fork response = %+v", response)
	}
	var value struct {
		SessionID string `json:"sessionId"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.SessionID == "" || value.SessionID == "fork-source" {
		t.Fatalf("fork session id = %q", value.SessionID)
	}
	select {
	case addedID := <-forkAdded:
		if addedID != value.SessionID {
			t.Fatalf("native fork notification = %q, want %q", addedID, value.SessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native fork did not notify the native mux")
	}
	cloned, err := st.LoadSession(ctx, value.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cloned) != 4 || cloned[0].Seq != 1 || cloned[2].Type != session.EventTurnEnd || cloned[3].Type != session.EventSessionTitle {
		t.Fatalf("forked events = %+v", cloned)
	}
	meta, err := st.GetSessionMeta(ctx, value.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Boundary title" || meta.TitleSource != session.TitleSourceFallback ||
		meta.WorkspaceID != "fork-workspace" || meta.CWD != workspacePath {
		t.Fatalf("forked metadata = %+v", meta)
	}
	config, err := st.GetSessionConfig(ctx, value.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentPreset != "code" || config.Provider != "openai" || config.Model != "gpt-4o" || config.Permission != "full" {
		t.Fatalf("forked config = %+v", config)
	}

	response = call("fork-latest-completed", map[string]any{"sessionId": "fork-source", "atSeq": 99})
	if !response.Result.OK {
		t.Fatalf("fallback fork response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	cloned, err = st.LoadSession(ctx, value.SessionID)
	if err != nil || len(cloned) != 4 {
		t.Fatalf("fallback forked events = %d, err=%v", len(cloned), err)
	}

	seedSession(t, st, "fork-empty", nil)
	response = call("fork-empty", map[string]any{"sessionId": "fork-empty"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "fork-unavailable" ||
		response.Result.Error.Message != `session "fork-empty" has no completed turn to fork from` {
		t.Fatalf("empty fork response = %+v", response)
	}
	seedSession(t, st, "fork-open", []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(2002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"open"}`)},
	})
	response = call("fork-open", map[string]any{"sessionId": "fork-open", "atSeq": 2})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "fork-unavailable" ||
		response.Result.Error.Message != `session "fork-open" has not completed the turn containing event 2` {
		t.Fatalf("open fork response = %+v", response)
	}
	response = call("fork-missing", map[string]any{"sessionId": "missing"})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "session-not-found" {
		t.Fatalf("missing fork response = %+v", response)
	}
}

func TestNativeSessionForkAttachesSubagentSourceToNearestAncestorWorkspace(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "ordinary-root", nil)
	workspacePath := t.TempDir()
	if err := st.CreateWorkspaceWithPath(context.Background(), "ancestor-workspace", "Ancestor", workspacePath); err != nil {
		t.Fatal(err)
	}
	directPath := t.TempDir()
	if err := st.CreateWorkspaceWithPath(context.Background(), "direct-workspace", "Direct", directPath); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionWorkspace(context.Background(), "ordinary-root", "ancestor-workspace"); err != nil {
		t.Fatal(err)
	}
	seedSession(t, st, "subagent-source", []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(3001), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(3002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"child work"}`)},
		{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(3003), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
	})
	if err := st.SetSessionHeader(context.Background(), "subagent-source", store.SessionHeader{
		ID: "subagent-source", Parent: "ordinary-root", Origin: "subagent", DelegationDepth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var value struct {
		SessionID string `json:"sessionId"`
	}

	fork := func(id string) store.SessionMeta {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": "session.fork",
			"payload": map[string]any{"sessionId": "subagent-source"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.fork", "tok", string(body))
		response := nativeResponse(t, rec.Body.Bytes())
		if !response.Result.OK {
			t.Fatalf("subagent fork response = %+v", response)
		}
		encoded, err := json.Marshal(response.Result.Value)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		meta, err := st.GetSessionMeta(context.Background(), value.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		header, err := st.GetSessionHeader(context.Background(), value.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if header.Parent != "subagent-source" || header.Origin != "fork" {
			t.Fatalf("subagent fork header = %#v", header)
		}
		return meta
	}

	if err := st.SetSessionWorkspace(context.Background(), "subagent-source", "direct-workspace"); err != nil {
		t.Fatal(err)
	}
	if meta := fork("fork-direct-subagent"); meta.WorkspaceID != "direct-workspace" {
		t.Fatalf("direct subagent fork workspace = %q", meta.WorkspaceID)
	}

	if err := st.SetSessionWorkspace(context.Background(), "subagent-source", ""); err != nil {
		t.Fatal(err)
	}
	if meta := fork("fork-ancestor-subagent"); meta.WorkspaceID != "ancestor-workspace" {
		t.Fatalf("subagent fork workspace = %q", meta.WorkspaceID)
	}
}

func TestNativeSessionUpdateQueueUsesDSHActionUnion(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotAction, gotText, gotSession, gotItem string
	srv.SetNativeQueueUpdater(func(_ context.Context, sessionID, itemID, action, text string) error {
		gotSession, gotItem, gotAction, gotText = sessionID, itemID, action, text
		return nil
	})
	call := func(id string, payload map[string]any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": "session.updateQueue", "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.updateQueue", "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("queue-edit", map[string]any{
		"sessionId": "session-1", "itemId": "message-1",
		"action": map[string]any{"kind": "edit", "content": []any{map[string]any{"type": "text", "text": " revised "}}},
	})
	if !response.Result.OK || gotSession != "session-1" || gotItem != "message-1" || gotAction != "edit" || gotText != "revised" {
		t.Fatalf("edit response=%+v callback=%s/%s/%s/%q", response, gotSession, gotItem, gotAction, gotText)
	}

	response = call("queue-remove", map[string]any{
		"sessionId": "session-1", "itemId": "message-1", "action": map[string]any{"kind": "remove"},
	})
	if !response.Result.OK || gotAction != "remove" || gotText != "" {
		t.Fatalf("remove response=%+v callback=%s/%q", response, gotAction, gotText)
	}
	response = call("queue-steer", map[string]any{
		"sessionId": "session-1", "itemId": "message-1", "action": map[string]any{"kind": "steer"},
	})
	if !response.Result.OK || gotAction != "steer" || gotText != "" {
		t.Fatalf("steer response=%+v callback=%s/%q", response, gotAction, gotText)
	}

	response = call("queue-image-edit", map[string]any{
		"sessionId": "session-1", "itemId": "message-1",
		"action": map[string]any{"kind": "edit", "content": []any{map[string]any{"type": "image"}}},
	})
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "attachment-error" ||
		response.Result.Error.Details["reason"] != "QUEUE_EDIT_NON_TEXT" {
		t.Fatalf("image edit response = %+v", response)
	}
	queueErr := ErrQueueItemNotFound
	srv.SetNativeQueueUpdater(func(context.Context, string, string, string, string) error {
		return queueErr
	})
	response = call("queue-missing", map[string]any{
		"sessionId": "session-1", "itemId": "message-1", "action": map[string]any{"kind": "remove"},
	})
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "queue-item-not-found" ||
		response.Result.Error.Details["itemId"] != "message-1" {
		t.Fatalf("missing queue item response = %+v", response)
	}
	queueErr = ErrSteerUnavailable
	response = call("queue-steer-unavailable", map[string]any{
		"sessionId": "session-1", "itemId": "message-1", "action": map[string]any{"kind": "steer"},
	})
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Code != "steer-unavailable" ||
		response.Result.Error.Details["itemId"] != "message-1" {
		t.Fatalf("steer unavailable response = %+v", response)
	}
	srv.SetNativeQueueUpdater(func(_ context.Context, sessionID, itemID, action, text string) error {
		gotSession, gotItem, gotAction, gotText = sessionID, itemID, action, text
		return nil
	})
	response = call("queue-invalid", map[string]any{
		"sessionId": "session-1", "itemId": "message-1", "action": map[string]any{"kind": "unknown"},
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("invalid action response = %+v", response)
	}
}

func TestNativeSettingsUpdateReplaceAndLLMDiscovery(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var discovery ProviderDiscover
	srv.SetProviderDiscover(func(_ context.Context, request ProviderDiscover) ([]ProviderModel, error) {
		discovery = request
		return []ProviderModel{{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000, MaxTokens: 16384}, {ID: ""}}, nil
	})
	call := func(id, method string, payload any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{"type": "client-request", "rpcId": id, "method": method, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("settings-update", "settings.update", map[string]any{
		"ns": "ui-onboarding", "patch": map[string]any{"completed": true}, "expectedRevision": 0,
	})
	if !response.Result.OK {
		t.Fatalf("settings.update response = %+v", response)
	}
	var view map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view["revision"] != float64(1) || view["value"].(map[string]any)["completed"] != true {
		t.Fatalf("settings.update view = %#v", view)
	}
	response = call("settings-conflict", "settings.update", map[string]any{
		"ns": "ui-onboarding", "patch": map[string]any{"other": true}, "expectedRevision": 0,
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "settings-conflict" {
		t.Fatalf("settings conflict response = %+v", response)
	}
	response = call("settings-replace", "settings.replace", map[string]any{
		"ns": "ui-onboarding", "section": map[string]any{"replacement": "yes"}, "expectedRevision": 1,
	})
	if !response.Result.OK {
		t.Fatalf("settings.replace response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	values := view["value"].(map[string]any)
	if view["revision"] != float64(2) || values["replacement"] != "yes" || values["completed"] != nil {
		t.Fatalf("settings.replace view = %#v", view)
	}

	response = call("discover", "llm.discoverModels", map[string]any{
		"settingsNs": "llm-pi-ai", "provider": "custom", "baseURL": "https://gateway.example/v1",
		"api": "openai-completions", "apiKey": "secret-value",
	})
	if !response.Result.OK || discovery.BaseURL != "https://gateway.example/v1" || discovery.Protocol != "openai-completions" || discovery.APIKey != "secret-value" {
		t.Fatalf("llm.discoverModels response=%+v request=%+v", response, discovery)
	}
	var discovered struct {
		Models []map[string]any `json:"models"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered.Models) != 1 || discovered.Models[0]["id"] != "gpt-4o" || discovered.Models[0]["contextWindow"] != float64(128000) || discovered.Models[0]["maxTokens"] != float64(16384) {
		t.Fatalf("discovered models = %#v", discovered.Models)
	}
}

func TestNativeSettingsDescribeExposesAgentPresetBaseLayer(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any { return map[string]any{"mode": "code"} })
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.describe", "tok",
		`{"type":"client-request","rpcId":"settings-describe","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe = %+v", response.Result.Error)
	}
	encoded, _ := json.Marshal(response.Result.Value)
	var value struct {
		Namespaces []struct {
			Ns       string         `json:"ns"`
			Base     map[string]any `json:"base"`
			User     map[string]any `json:"user"`
			Value    map[string]any `json:"value"`
			Revision int            `json:"revision"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	var preset *struct {
		Ns       string         `json:"ns"`
		Base     map[string]any `json:"base"`
		User     map[string]any `json:"user"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}
	for i := range value.Namespaces {
		if value.Namespaces[i].Ns == "agent-presets" {
			preset = &value.Namespaces[i]
			break
		}
	}
	if preset == nil {
		t.Fatalf("agent-presets namespace missing from %#v", value.Namespaces)
	}
	if preset.Base["default"] != "code" || preset.Value["default"] != "code" || preset.User == nil || len(preset.User) != 0 || preset.Revision != 0 {
		t.Fatalf("agent-presets view = %#v, want code base with empty user layer", preset)
	}
}

func TestNativeSettingsLLMBaseAndUserLayers(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{
			{
				"id": "deepseek-official", "name": "DeepSeek", "configured": true, "available": true,
				"env_var": "DEEPSEEK_API_KEY", "base_url": "https://api.deepseek.com",
				"model": "deepseek-v4-flash",
			},
			{
				"id": "custom", "name": "Custom", "custom": true, "configured": true, "available": true,
				"env_var": "CUSTOM_API_KEY", "base_url": "https://custom.example/v1",
				"protocol": "openai-completions", "model": "custom-model",
			},
		}}
	})
	describe := func() map[string]map[string]any {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.describe", "tok",
			`{"type":"client-request","rpcId":"settings-describe","method":"settings.describe","payload":{}}`)
		response := nativeResponse(t, rec.Body.Bytes())
		if !response.Result.OK {
			t.Fatalf("settings.describe = %+v", response.Result.Error)
		}
		encoded, _ := json.Marshal(response.Result.Value)
		var value struct {
			Namespaces []map[string]any `json:"namespaces"`
		}
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		byNS := make(map[string]map[string]any, len(value.Namespaces))
		for _, namespace := range value.Namespaces {
			byNS[namespace["ns"].(string)] = namespace
		}
		return byNS
	}

	views := describe()
	deepseek := views["llm-deepseek"]
	if deepseek == nil {
		t.Fatal("llm-deepseek namespace missing")
	}
	deepseekBase := deepseek["base"].(map[string]any)
	if deepseekBase["apiKeyEnv"] != "DEEPSEEK_API_KEY" || deepseekBase["baseURL"] != "https://api.deepseek.com" {
		t.Fatalf("deepseek base = %#v", deepseekBase)
	}
	if len(deepseek["user"].(map[string]any)) != 0 {
		t.Fatalf("deepseek user = %#v, want empty", deepseek["user"])
	}

	custom := views["llm-pi-ai"]
	if custom == nil {
		t.Fatal("llm-pi-ai namespace missing")
	}
	customBase := custom["base"].(map[string]any)["providers"].(map[string]any)["custom"].(map[string]any)
	if customBase["baseURL"] != "https://custom.example/v1" || customBase["api"] != "openai-completions" {
		t.Fatalf("custom base profile = %#v", customBase)
	}

	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.update", "tok",
		`{"type":"client-request","rpcId":"settings-update","method":"settings.update","payload":{"ns":"llm-pi-ai","patch":{"providers":{"custom":{"baseURL":"https://override.example/v1"}}},"expectedRevision":0}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.update = %+v", response.Result.Error)
	}
	encoded, _ := json.Marshal(response.Result.Value)
	var updated struct {
		Base     map[string]any `json:"base"`
		User     map[string]any `json:"user"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}
	if err := json.Unmarshal(encoded, &updated); err != nil {
		t.Fatal(err)
	}
	customUser := updated.User["providers"].(map[string]any)["custom"].(map[string]any)
	customValue := updated.Value["providers"].(map[string]any)["custom"].(map[string]any)
	if customUser["baseURL"] != "https://override.example/v1" || customValue["baseURL"] != "https://override.example/v1" || customValue["apiKeyEnv"] != "CUSTOM_API_KEY" {
		t.Fatalf("custom merged profile = user %#v value %#v", customUser, customValue)
	}

	rec = doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.replace", "tok",
		`{"type":"client-request","rpcId":"settings-replace","method":"settings.replace","payload":{"ns":"llm-pi-ai","section":{},"expectedRevision":1}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.replace = %+v", response.Result.Error)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	updated = struct {
		Base     map[string]any `json:"base"`
		User     map[string]any `json:"user"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}{}
	if err := json.Unmarshal(encoded, &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.User) != 0 || updated.Value["providers"].(map[string]any)["custom"].(map[string]any)["baseURL"] != "https://custom.example/v1" {
		t.Fatalf("custom reset = %#v, want user reset and base restored", updated)
	}
}

func TestNativeAgentPresetDefaultSettingsUpdate(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	call := func(id string, preset string, expected *int) nativeRPCResponse {
		t.Helper()
		payload := map[string]any{
			"ns": "agent-presets", "patch": map[string]any{"default": preset},
		}
		if expected != nil {
			payload["expectedRevision"] = *expected
		}
		body, err := json.Marshal(map[string]any{
			"type": "client-request", "rpcId": id, "method": "settings.update", "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.update", "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("agent-preset-minimal", "minimal", nil)
	if !response.Result.OK {
		t.Fatalf("minimal default update response = %+v", response)
	}
	var view map[string]any
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view["ns"] != "agent-presets" || view["revision"] != float64(1) || view["value"].(map[string]any)["default"] != "minimal" {
		t.Fatalf("minimal default view = %#v", view)
	}
	settings, err := st.GetSettings(context.Background())
	if err != nil || settings["agent_preset"] != "minimal" {
		t.Fatalf("persisted agent preset = %#v, err=%v", settings, err)
	}

	response = call("agent-preset-code", "code", nil)
	if !response.Result.OK {
		t.Fatalf("code default update response = %+v", response)
	}
	staleRevision := 0
	response = call("agent-preset-stale", "minimal", &staleRevision)
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "settings-conflict" {
		t.Fatalf("stale agent preset update response = %+v", response)
	}

	unsetBody, err := json.Marshal(map[string]any{
		"type": "client-request", "rpcId": "agent-preset-unset", "method": "settings.mutate",
		"payload": map[string]any{
			"ns": "agent-presets", "ops": []map[string]any{
				{"op": "unset", "path": []string{"default"}},
			}, "expectedRevision": 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/settings.mutate", "tok", string(unsetBody))
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("agent preset unset response = %+v", response.Result.Error)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view["revision"] != float64(3) ||
		view["value"].(map[string]any)["default"] != "standard" ||
		len(view["user"].(map[string]any)) != 0 {
		t.Fatalf("agent preset reset view = %#v, want base standard with empty user", view)
	}
	settings, err = st.GetSettings(context.Background())
	if err != nil || settings["agent_preset"] != "standard" {
		t.Fatalf("persisted agent preset after reset = %#v, err=%v", settings, err)
	}
}

func TestNativeSkillListProjectsUserInvocableCatalog(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetSkillManager(func(_ context.Context, action string, _ SkillRequest) (map[string]any, error) {
		if action != "list" {
			t.Fatalf("skill action = %q, want list", action)
		}
		return map[string]any{"skills": []map[string]any{
			{"name": "zeta", "description": "last", "when_to_use": "later", "model_invocable": true, "user_invocable": true},
			{"name": "internal", "description": "hidden", "model_invocable": true, "user_invocable": false},
			{"name": "alpha", "description": "first", "model_invocable": false, "user_invocable": true},
		}}, nil
	})
	body := `{"type":"client-request","rpcId":"skill-1","method":"skill.list","payload":{"sessionId":"session-1"}}`
	rec := doReqBody(t, srv.Handler(), "POST", "/api/skill.list", "tok", body)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("skill.list response = %+v", response)
	}
	var value struct {
		Skills []map[string]any `json:"skills"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Skills) != 2 || value.Skills[0]["name"] != "alpha" || value.Skills[1]["name"] != "zeta" {
		t.Fatalf("skill list = %#v", value.Skills)
	}
	if value.Skills[1]["whenToUse"] != "later" || value.Skills[0]["modelInvocable"] != false {
		t.Fatalf("skill projections = %#v", value.Skills)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/skill.list", "tok", `{"type":"client-request","rpcId":"skill-2","method":"skill.list","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("missing session skill.list response = %+v", response)
	}
}

func TestNativeSkillListUsesSessionCWD(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "skill-alpha", nil)
	seedSession(t, st, "skill-beta", nil)
	seedSession(t, st, "skill-no-cwd", nil)
	if err := st.SetSessionCWD(context.Background(), "skill-no-cwd", ""); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	if err := st.SetSessionCWD(context.Background(), "skill-alpha", first); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionCWD(context.Background(), "skill-beta", second); err != nil {
		t.Fatal(err)
	}
	catalogs := map[string][]SkillCatalogEntry{
		first:  {{Name: "first-only", Description: "first"}},
		second: {{Name: "second-only", Description: "second", WhenToUse: "when second"}},
	}
	srv.SetSkillCatalogProvider(func(_ context.Context, cwd string) ([]SkillCatalogEntry, error) {
		rows, ok := catalogs[cwd]
		if !ok {
			return nil, fmt.Errorf("unexpected cwd %q", cwd)
		}
		return rows, nil
	})
	call := func(sessionID string) nativeRPCResponse {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"sessionId": sessionID})
		body, _ := json.Marshal(map[string]any{
			"type":    "client-request",
			"rpcId":   "skill-" + sessionID,
			"method":  "skill.list",
			"payload": json.RawMessage(payload),
		})
		rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/skill.list", "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}
	for sessionID, want := range map[string]string{"skill-alpha": "first-only", "skill-beta": "second-only"} {
		response := call(sessionID)
		if !response.Result.OK {
			t.Fatalf("%s skill.list = %+v", sessionID, response.Result.Error)
		}
		encoded, _ := json.Marshal(response.Result.Value)
		var value struct {
			Skills []map[string]any `json:"skills"`
		}
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		if len(value.Skills) != 1 || value.Skills[0]["name"] != want {
			t.Fatalf("%s skills = %#v, want %s", sessionID, value.Skills, want)
		}
	}
	response := call("skill-missing")
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "session-not-found" {
		t.Fatalf("missing session response = %+v", response.Result.Error)
	}
	response = call("skill-no-cwd")
	if response.Result.OK || response.Result.Error == nil ||
		response.Result.Error.Message != `session "skill-no-cwd" has no project cwd` {
		t.Fatalf("cwd-less session response = %+v", response.Result.Error)
	}
}

func TestNativeSubagentListProjectsParentScopedCatalog(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "parent-session", nil)
	srv.SetSubagentProvider(func(_ context.Context, sessionID string) ([]map[string]any, error) {
		if sessionID != "parent-session" {
			t.Fatalf("parent session = %q", sessionID)
		}
		return []map[string]any{
			{"id": "child-z", "label": "Z task", "running": false, "mode": "one-shot", "has_children": true},
			{"id": "child-a", "label": "A task", "running": true, "mode": "continuable", "has_children": false},
		}, nil
	})
	body := `{"type":"client-request","rpcId":"subagent-1","method":"subagent.list","payload":{"parentSessionId":"parent-session"}}`
	rec := doReqBody(t, srv.Handler(), "POST", "/api/subagent.list", "tok", body)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("subagent.list response = %+v", response)
	}
	var value struct {
		Entries         []map[string]any `json:"entries"`
		ParentAvailable bool             `json:"parentAvailable"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if !value.ParentAvailable || len(value.Entries) != 2 || value.Entries[0]["id"] != "child-a" || value.Entries[1]["id"] != "child-z" {
		t.Fatalf("subagent entries = %#v", value)
	}
	if value.Entries[0]["activity"] != "running" || value.Entries[0]["mode"] != "continuable" || value.Entries[1]["hasChildren"] != true {
		t.Fatalf("subagent projections = %#v", value.Entries)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/subagent.list", "tok", `{"type":"client-request","rpcId":"subagent-2","method":"subagent.list","payload":{"parentSessionId":"missing"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "subagent-parent-not-found" {
		t.Fatalf("missing parent response = %+v", response)
	}
}

func TestNativeSubagentHistoryEnforcesParentLineageAndReusesProjection(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "parent-history", nil)
	seedSession(t, st, "child-history", []session.Event{
		{Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"parentSession":"parent-history","depth":1}`)},
		{Seq: 2, Type: session.EventTurnStart, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"child request"}`)},
		{Seq: 4, Type: session.EventTurnEnd, At: time.UnixMilli(1004), Version: session.EventVersion, Data: json.RawMessage(`{}`)},
	})
	call := func(id string, payload map[string]any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{"type": "client-request", "rpcId": id, "method": "subagent.history", "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/subagent.history", "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}

	response := call("subagent-history", map[string]any{
		"parentSessionId": "parent-history", "childSessionId": "child-history", "mode": "continuable", "maxMessages": 1,
	})
	if !response.Result.OK {
		t.Fatalf("subagent.history response = %+v", response)
	}
	var value struct {
		Header struct {
			ID              string `json:"id"`
			ParentSessionID string `json:"parentSession"`
			Origin          string `json:"origin"`
		} `json:"header"`
		Events      []nativeHistoryEntry  `json:"events"`
		Projections nativeProjectionBlock `json:"projections"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.Header.ID != "child-history" || value.Header.ParentSessionID != "parent-history" || value.Header.Origin != "subagent" || len(value.Events) == 0 {
		t.Fatalf("subagent history value = %+v", value)
	}
	if value.Projections.AsOfSeq != 4 {
		t.Fatalf("subagent history projection watermark = %d", value.Projections.AsOfSeq)
	}
	if _, ok := value.Projections.Values["contextBreakdown"]; !ok {
		t.Fatalf("subagent history projections = %#v", value.Projections.Values)
	}

	response = call("subagent-history-unauthorized", map[string]any{
		"parentSessionId": "other-parent", "childSessionId": "child-history", "mode": "one-shot",
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "subagent-unauthorized" {
		t.Fatalf("unauthorized history response = %+v", response)
	}
	response = call("subagent-history-missing", map[string]any{
		"parentSessionId": "parent-history", "childSessionId": "missing", "mode": "one-shot",
	})
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "subagent-not-found" {
		t.Fatalf("missing history response = %+v", response)
	}
}

func TestNativeSubagentPromptAndInterruptRequireLineageAndWireLiveRuntime(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "parent-control", nil)
	seedSession(t, st, "child-control", []session.Event{
		{Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"parentSession":"parent-control","depth":1}`)},
	})
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAttachmentStore(att)
	var prompted struct {
		ID      string
		Text    string
		Content []llm.ContentBlock
		Meta    PromptMeta
	}
	var interrupted string
	srv.SetNativeSubagentManager(
		func(_ context.Context, childID string, content []llm.ContentBlock, meta PromptMeta) error {
			prompted.ID, prompted.Content, prompted.Meta = childID, content, meta
			return nil
		},
		func(childID, _ string) error {
			interrupted = childID
			return nil
		},
	)
	call := func(id, method string, payload map[string]any) nativeRPCResponse {
		t.Helper()
		body, err := json.Marshal(map[string]any{"type": "client-request", "rpcId": id, "method": method, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		rec := doReqBody(t, srv.Handler(), "POST", "/api/"+method, "tok", string(body))
		return nativeResponse(t, rec.Body.Bytes())
	}
	badZoneWrongParent := call("subagent-bad-zone-wrong-parent", "subagent.prompt", map[string]any{
		"parentSessionId": "missing-parent", "childSessionId": "child-control", "mode": "continuable",
		"clientTimeZone": "+08:00",
		"content":        []map[string]any{{"type": "text", "text": "validate metadata first"}},
	})
	if badZoneWrongParent.Result.OK || badZoneWrongParent.Result.Error == nil ||
		badZoneWrongParent.Result.Error.Code != "invalid-time-zone" ||
		badZoneWrongParent.Result.Error.Details["value"] != "+08:00" {
		t.Fatalf("subagent bad zone before authorization = %+v", badZoneWrongParent)
	}
	if interrupted != "" {
		t.Fatalf("metadata validation triggered subagent callback = %q", interrupted)
	}
	prompt := call("subagent-prompt", "subagent.prompt", map[string]any{
		"parentSessionId": "parent-control", "childSessionId": "child-control", "mode": "continuable",
		"clientTimeZone": "Asia/Shanghai",
		"content": []map[string]any{
			{"type": "image", "mediaType": "image/png", "name": "follow-up.png", "data": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
			{"type": "text", "text": "follow up"},
		},
	})
	if !prompt.Result.OK || prompted.ID != "child-control" || len(prompted.Content) != 2 ||
		prompted.Content[0].Kind != llm.BlockImage || prompted.Content[0].Image.Name != "follow-up.png" ||
		prompted.Content[1].Kind != llm.BlockText || prompted.Content[1].Text != "follow up" ||
		prompted.Meta.RPCID == "" || prompted.Meta.ClientTimeZone != "Asia/Shanghai" {
		t.Fatalf("subagent prompt = %+v, callback = %+v", prompt, prompted)
	}
	var receipt map[string]any
	encoded, _ := json.Marshal(prompt.Result.Value)
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["messageId"] == "" {
		t.Fatalf("subagent prompt receipt = %#v", receipt)
	}
	badZone := call("subagent-bad-zone", "subagent.prompt", map[string]any{
		"parentSessionId": "parent-control", "childSessionId": "child-control", "mode": "continuable",
		"clientTimeZone": "+08:00",
		"content":        []map[string]any{{"type": "text", "text": "bad zone"}},
	})
	if badZone.Result.OK || badZone.Result.Error == nil ||
		badZone.Result.Error.Code != "invalid-time-zone" ||
		badZone.Result.Error.Details["value"] != "+08:00" {
		t.Fatalf("subagent bad zone = %+v", badZone)
	}
	srv.SetNativeImageCapabilityResolver(func(context.Context, string) bool { return false })
	imageBlocked := call("subagent-prompt-image-blocked", "subagent.prompt", map[string]any{
		"parentSessionId": "parent-control", "childSessionId": "child-control", "mode": "continuable",
		"content": []map[string]any{
			{"type": "image", "mediaType": "image/png", "data": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
			{"type": "text", "text": "blocked"},
		},
	})
	if imageBlocked.Result.OK || imageBlocked.Result.Error == nil ||
		imageBlocked.Result.Error.Code != "attachment-error" ||
		imageBlocked.Result.Error.Details["reason"] != "MODEL_DOES_NOT_SUPPORT_IMAGES" {
		t.Fatalf("blocked subagent image prompt = %+v", imageBlocked)
	}
	srv.SetNativeImageCapabilityResolver(func(context.Context, string) bool { return true })
	interrupt := call("subagent-interrupt", "subagent.interrupt", map[string]any{
		"parentSessionId": "parent-control", "childSessionId": "child-control", "mode": "continuable",
	})
	if !interrupt.Result.OK || interrupted != "child-control" {
		t.Fatalf("subagent interrupt = %+v, callback child = %q", interrupt, interrupted)
	}
	wrongParent := call("subagent-prompt-wrong-parent", "subagent.prompt", map[string]any{
		"parentSessionId": "other-parent", "childSessionId": "child-control", "mode": "continuable",
		"content": []map[string]any{{"type": "text", "text": "must reject"}},
	})
	if wrongParent.Result.OK || wrongParent.Result.Error == nil || wrongParent.Result.Error.Code != "subagent-parent-not-found" {
		t.Fatalf("wrong parent prompt = %+v", wrongParent)
	}
	badMode := call("subagent-prompt-bad-mode", "subagent.prompt", map[string]any{
		"parentSessionId": "parent-control", "childSessionId": "child-control", "mode": "one-shot",
		"content": []map[string]any{{"type": "text", "text": "must reject"}},
	})
	if badMode.Result.OK || badMode.Result.Error == nil || badMode.Result.Error.Code != "bad-request" {
		t.Fatalf("bad mode prompt = %+v", badMode)
	}
}

func readNativeTextFrame(reader *bufio.Reader) ([]byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first&0x0f != 1 {
		return nil, fmt.Errorf("opcode = %d", first&0x0f)
	}
	second, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	length := int(second & 0x7f)
	if length == 126 {
		var bytesLength [2]byte
		if _, err := io.ReadFull(reader, bytesLength[:]); err != nil {
			return nil, err
		}
		length = int(bytesLength[0])<<8 | int(bytesLength[1])
	} else if length == 127 {
		return nil, fmt.Errorf("unexpected large frame")
	}
	if second&0x80 != 0 {
		return nil, fmt.Errorf("server frame is masked")
	}
	payload := make([]byte, length)
	_, err = io.ReadFull(reader, payload)
	return payload, err
}

func TestNativeSessionRenameUsesCanonicalEventCallbackAndRejectsEmptyTitle(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "rename-session", nil)
	var calls int
	srv.SetNativeSessionRenamer(func(_ context.Context, sessionID, title string) (int64, error) {
		calls++
		if sessionID != "rename-session" || title != "New title" {
			t.Fatalf("rename callback = %q/%q", sessionID, title)
		}
		return 7, nil
	})
	call := func(id, title string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), "POST", "/api/session.rename", "tok", fmt.Sprintf(`{"type":"client-request","rpcId":%q,"method":"session.rename","payload":{"sessionId":"rename-session","title":%q}}`, id, title))
		return nativeResponse(t, rec.Body.Bytes())
	}
	response := call("rename-1", "  New title  ")
	if !response.Result.OK || response.Result.Value.(map[string]any)["seq"] != float64(7) {
		t.Fatalf("rename response = %+v", response)
	}
	if calls != 1 {
		t.Fatalf("rename callback calls = %d, want 1", calls)
	}
	empty := call("rename-2", " \t\n ")
	if empty.Result.OK || empty.Result.Error == nil || empty.Result.Error.Code != "title-invalid" {
		t.Fatalf("empty rename response = %+v", empty)
	}
	if calls != 1 {
		t.Fatalf("empty rename reached callback: calls=%d", calls)
	}
}
