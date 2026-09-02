package session

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

func TestCanonicalApprovalProjectionUsesClosedOutcomeVocabulary(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		data    any
		wantTyp string
		want    string
	}{
		{"asked", EventInteractRequest, NewInteractRequestDetail("a1", "bash", "danger", "{}", nil), EventApprovalAsked, ""},
		{"allowed", EventInteractResolve, NewInteractResolve("a1", true), EventApprovalDecided, "allowed-once"},
		{"rejected", EventInteractResolve, NewInteractResolve("a1", false), EventApprovalDecided, "rejected"},
		{"cancelled", EventInteractCancel, NewInteractCancel("a1"), EventApprovalDecided, "cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			typ, value, ok := CanonicalApprovalEvent(test.typ, test.data)
			if !ok || typ != test.wantTyp {
				t.Fatalf("projection = %q, %#v, %v", typ, value, ok)
			}
			if test.want == "" {
				return
			}
			object, ok := value.(map[string]any)
			if !ok || object["outcome"] != test.want {
				t.Fatalf("outcome = %#v", value)
			}
		})
	}
}

func TestCanonicalApprovalProjectionPreservesApprovalCard(t *testing.T) {
	input := NewInteractRequestDetail("a1", "bash", "run it", `{"command":"pwd"}`, []map[string]any{{"id": "confirm", "question": "Proceed?"}})
	typ, value, ok := CanonicalApprovalEvent(EventInteractRequest, input)
	if !ok || typ != EventApprovalAsked {
		t.Fatalf("projection = %q, %#v, %v", typ, value, ok)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"id": "a1", "toolName": "bash", "prompt": "run it", "reason": "run it", "args": `{"command":"pwd"}`} {
		if got[key] != want {
			t.Fatalf("%s = %#v, want %q", key, got[key], want)
		}
	}
	if _, ok := got["questions"]; !ok {
		t.Fatal("canonical approval asked lost questions")
	}

	typ, value, ok = CanonicalApprovalEvent(EventInteractResolve, map[string]any{"id": "a1", "approved": true, "answer": `{"answers":[]}`})
	if !ok || typ != EventApprovalDecided {
		t.Fatalf("decision projection = %q, %#v, %v", typ, value, ok)
	}
	decision, ok := value.(map[string]any)
	if !ok || decision["answer"] != `{"answers":[]}` {
		t.Fatalf("decision lost answer: %#v", value)
	}
}

func TestCanonicalApprovalProjectionPreservesToolCallCorrelation(t *testing.T) {
	typ, value, ok := CanonicalApprovalEvent(EventInteractRequest,
		NewInteractRequestDetailWithCallID("a1", "call-1", "bash", "run it", "{}", nil))
	if !ok || typ != EventApprovalAsked {
		t.Fatalf("asked projection = %q, %#v, %v", typ, value, ok)
	}
	asked := value.(map[string]any)
	if asked["callId"] != "call-1" {
		t.Fatalf("asked callId = %#v", asked["callId"])
	}
	typ, value, ok = CanonicalApprovalEvent(EventInteractResolve,
		NewInteractResolveWithCallID("a1", "call-1", true))
	if !ok || typ != EventApprovalDecided {
		t.Fatalf("decision projection = %q, %#v, %v", typ, value, ok)
	}
	decision := value.(map[string]any)
	if decision["callId"] != "call-1" {
		t.Fatalf("decision callId = %#v", decision["callId"])
	}
}

func TestValidateWireEventCoreAndUnknownMatrix(t *testing.T) {
	valid := []string{
		`{"type":"turn/start","seq":1,"time":1,"data":{"turn":1}}`,
		`{"type":"step/start","seq":2,"time":1,"data":{"turn":1,"step":1}}`,
		`{"type":"user/message","seq":3,"time":1,"data":{"role":"user","content":[],"source":{"kind":"user"}},"surfaceOp":"append"}`,
		`{"type":"assistant/chunk","seq":4,"time":1,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta"}}}`,
		`{"type":"assistant/message","seq":5,"time":1,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model"}}},"surfaceOp":"append"}`,
		`{"type":"step/end","seq":6,"time":1,"data":{"turn":1,"step":1}}`,
		`{"type":"turn/end","seq":7,"time":1,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
		`{"type":"command/run","seq":8,"time":1,"data":{"commandId":"c1","name":"feedback","args":"note","source":{"kind":"user"}}}`,
		`{"type":"command/done","seq":9,"time":1,"data":{"commandId":"c1","kind":"success","text":"recorded"}}`,
		`{"type":"request/context","seq":10,"time":1,"data":{"provider":"mock","model":"m","contextWindow":128000}}`,
		`{"type":"feedback/record","seq":11,"time":1,"data":{"text":"useful"}}`,
		`{"type":"plan/create","seq":12,"time":1,"data":{"scope":"goal","id":"g1","title":"Ship"}}`,
		`{"type":"plan/status","seq":13,"time":1,"data":{"scope":"goal","id":"g1","status":"in-progress"}}`,
		`{"type":"plan/mode","seq":14,"time":1,"data":{"active":true}}`,
		`{"type":"session/title","seq":15,"time":1,"data":{"title":"Ship","messageSeqs":[],"source":{"kind":"user"}}}`,
	}
	for _, raw := range valid {
		if err := ValidateWireEvent([]byte(raw)); err != nil {
			t.Errorf("valid event rejected: %v (%s)", err, raw)
		}
	}
	if err := ValidateWireEvent([]byte(`{"type":"plugin/custom","seq":1,"time":1,"data":{}}`)); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("unknown required event error = %v", err)
	}
	if err := ValidateWireEvent([]byte(`{"type":"plugin/custom","seq":1,"time":1,"data":{},"ignorable":true}`)); err != nil {
		t.Fatalf("ignorable unknown rejected: %v", err)
	}
}

func TestLegacyRequestVocabularyIsRejectedAcrossLiveAndReplayPaths(t *testing.T) {
	legacy := json.RawMessage(`{"config":{"provider":"mock","model":"m"}}`)
	for _, typ := range []string{"request/header-delta", "mode/set"} {
		if err := ValidateEventVocabulary(typ, legacy); !errors.Is(err, ErrUnsupportedEvent) {
			t.Fatalf("%s vocabulary error = %v", typ, err)
		}
	}
	if err := ValidateEventVocabulary(EventRequestHeader, json.RawMessage(`{"reason":"fallback"}`)); !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("fallback request header error = %v", err)
	}

	log := New()
	appendLegacy := log.Append
	if _, err := appendLegacy("request/header-delta", map[string]any{"config": map[string]any{"provider": "mock", "model": "m"}}); !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("live append error = %v", err)
	}
	if log.NextSeq() != 1 {
		t.Fatal("rejected live event advanced the sequence")
	}
	if err := log.Restore([]Event{{Seq: 1, Type: "mode/set", Version: EventVersion, Data: legacy}}); !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestUnknownDurableEventRejectedAcrossAppendPaths(t *testing.T) {
	unknown := "future/required-event"
	raw := json.RawMessage(`{}`)

	live := New()
	if _, err := live.Append(unknown, map[string]any{}); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("live append = %v, want ErrUnknownRequiredEvent", err)
	}
	if got := live.NextSeq(); got != 1 {
		t.Fatalf("live sequence after rejection = %d, want 1", got)
	}

	atomicLog := New()
	committed := false
	if err := AppendAtomic([]AtomicAppend{{Log: atomicLog, Type: unknown, Data: map[string]any{}}}, func([]Event) error {
		committed = true
		return nil
	}); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("atomic append = %v, want ErrUnknownRequiredEvent", err)
	}
	if committed || atomicLog.NextSeq() != 1 {
		t.Fatalf("atomic rejection committed=%v nextSeq=%d, want false/1", committed, atomicLog.NextSeq())
	}

	persisted := New()
	if err := persisted.AppendPersisted(Event{Seq: 1, Type: unknown, Version: EventVersion, Data: raw}); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("persisted incorporation = %v, want ErrUnknownRequiredEvent", err)
	}
	if got := persisted.NextSeq(); got != 1 {
		t.Fatalf("persisted sequence after rejection = %d, want 1", got)
	}

	replayed := New()
	if err := replayed.Restore([]Event{{Seq: 1, Type: unknown, Version: EventVersion, Data: raw}}); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("replay = %v, want ErrUnknownRequiredEvent", err)
	}
	if got := replayed.NextSeq(); got != 1 {
		t.Fatalf("replay sequence after rejection = %d, want 1", got)
	}
	if err := ValidateLifecycle([]Event{{Seq: 1, Type: unknown, Version: EventVersion, Data: raw}}); !errors.Is(err, ErrUnknownRequiredEvent) {
		t.Fatalf("lifecycle validation = %v, want ErrUnknownRequiredEvent", err)
	}
}

func TestLegacySteeringReplaysAsUserSurface(t *testing.T) {
	raw, err := json.Marshal(NewSteeringMessage("focus", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	log := New()
	if err := log.Restore([]Event{{Seq: 1, Type: EventSteeringMessage, Version: EventVersion, Data: raw}}); err != nil {
		t.Fatal(err)
	}
	history := log.DeriveHistory()
	if len(history) != 1 || history[0].Role != llm.RoleUser || history[0].Text() != "focus" {
		t.Fatalf("legacy steering history = %+v", history)
	}
}

func TestValidateWireEventRejectsMalformedCorePayload(t *testing.T) {
	cases := []string{
		`{"type":"turn/end","seq":1,"time":1,"data":{"turn":1}}`,
		`{"type":"assistant/chunk","seq":1,"time":1,"data":{"turn":1,"step":1}}`,
		`{"type":"user/message","seq":1,"time":1,"data":{"role":"user","content":[]}}`,
		`{"type":"request/context","seq":1,"time":1,"data":{"provider":"mock"}}`,
		`{"type":"request/context","seq":1,"time":1,"data":{"provider":"mock","model":"m","contextWindow":0}}`,
		`{"type":"feedback/record","seq":1,"time":1,"data":{"text":""}}`,
		`{"type":"plan/mode","seq":1,"time":1,"data":{"active":"true"}}`,
	}
	for _, raw := range cases {
		if err := ValidateWireEvent([]byte(raw)); !errors.Is(err, ErrMalformedWireEvent) {
			t.Errorf("malformed event error = %v (%s)", err, raw)
		}
	}
}

func TestValidateWireEventAcceptsRetryLifecycleEvents(t *testing.T) {
	for _, raw := range []string{
		`{"type":"llm/retry","seq":1,"time":1,"data":{"retryId":"r1","turn":1,"step":1,"provider":"mock","mode":"normal","policyKey":"p1","retry":1,"maxRetries":2,"delayMs":500,"failure":{"code":"SERVER","message":"temporary"}}}`,
		`{"type":"llm/retry-started","seq":2,"time":2,"data":{"retryId":"r1","turn":1,"step":1,"retry":1}}`,
	} {
		if err := ValidateWireEvent([]byte(raw)); err != nil {
			t.Fatalf("retry lifecycle rejected: %v", err)
		}
	}
}

func TestValidateWireEventRejectsMalformedRetryPayload(t *testing.T) {
	cases := []string{
		`{"type":"llm/retry","seq":1,"time":1,"data":{"retry":1,"delayMs":500,"failure":{"code":"SERVER","message":"temporary"}}}`,
		`{"type":"llm/retry","seq":1,"time":1,"data":{"retryId":"r1","turn":1,"step":1,"provider":"mock","mode":"always","policyKey":"p1","retry":1,"maxRetries":2,"delayMs":500,"failure":{"code":"SERVER","message":"temporary"}}}`,
		`{"type":"llm/retry","seq":1,"time":1,"data":{"retryId":"r1","turn":1,"step":1,"provider":"mock","mode":"normal","policyKey":"p1","retry":2,"maxRetries":1,"delayMs":500,"failure":{"code":"SERVER","message":"temporary"}}}`,
		`{"type":"llm/retry-started","seq":1,"time":1,"data":{"retryId":"r1","turn":1,"step":1,"retry":0}}`,
	}
	for _, raw := range cases {
		if err := ValidateWireEvent([]byte(raw)); !errors.Is(err, ErrMalformedWireEvent) {
			t.Errorf("malformed retry accepted: %v (%s)", err, raw)
		}
	}
}

func TestValidateLifecycleChecksCanonicalCoordinatesAndToolPairing(t *testing.T) {
	log := New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{EventTurnStart, NewTurnStartAt(1)},
		{EventStepStart, NewStepStartAt(1, 1)},
		{EventToolCall, NewToolCall(1, 1, "call-1", "read", "{}")},
		{EventToolResult, NewToolResultAt(1, 1, "call-1", "read", "ok", nil)},
		{EventStepEnd, NewStepEndAt(1, 1, "completed", "")},
		{EventTurnEnd, NewTurnEndAt(1, "completed", "")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateLifecycle(log.Events()); err != nil {
		t.Fatalf("canonical lifecycle rejected: %v", err)
	}
	bad := append([]Event(nil), log.Events()...)
	bad[3].Data = json.RawMessage(`{"turn":1,"step":1,"message":{"source":{"callId":"ghost"}}}`)
	if err := ValidateLifecycle(bad); err == nil {
		t.Fatal("unpaired canonical tool result was accepted")
	}
	bad = append([]Event(nil), log.Events()...)
	bad[1].Data = json.RawMessage(`{"turn":1,"step":2}`)
	if err := ValidateLifecycle(bad); err == nil {
		t.Fatal("non-sequential canonical step was accepted")
	}
}

func TestValidateLifecycleChecksCommandRunDoneContract(t *testing.T) {
	log := New()
	if _, err := log.Append(EventFeedbackRecord, NewFeedbackRecord("source")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(EventCommandRun, NewCommandRun("cmd-1", "feedback", "source")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(EventCommandDone, NewCommandDone("cmd-1", "success", "recorded", 1)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLifecycle(log.Events()); err != nil {
		t.Fatalf("valid command lifecycle rejected: %v", err)
	}

	base := log.Events()
	cases := []struct {
		name   string
		events []Event
	}{
		{
			name: "orphan done",
			events: []Event{
				{Seq: 1, Type: EventCommandDone, Version: EventVersion, Data: json.RawMessage(`{"commandId":"missing","kind":"success"}`)},
			},
		},
		{
			name: "duplicate run",
			events: []Event{
				base[1],
				{Seq: 3, Type: EventCommandRun, Version: EventVersion, Data: base[1].Data},
			},
		},
		{
			name: "duplicate done",
			events: []Event{
				base[1], base[2],
				{Seq: 4, Type: EventCommandDone, Version: EventVersion, Data: base[2].Data},
			},
		},
		{
			name: "source on error",
			events: []Event{
				base[1],
				{Seq: 3, Type: EventCommandDone, Version: EventVersion, Data: json.RawMessage(`{"commandId":"cmd-1","kind":"error","sourceEventSeq":1}`)},
			},
		},
		{
			name: "missing source",
			events: []Event{
				base[1],
				{Seq: 3, Type: EventCommandDone, Version: EventVersion, Data: json.RawMessage(`{"commandId":"cmd-1","kind":"success","sourceEventSeq":9}`)},
			},
		},
		{
			name: "command source",
			events: []Event{
				base[1],
				{Seq: 3, Type: EventCommandRun, Version: EventVersion, Data: json.RawMessage(`{"commandId":"cmd-2","name":"other","source":{"kind":"user"}}`)},
				{Seq: 4, Type: EventCommandDone, Version: EventVersion, Data: json.RawMessage(`{"commandId":"cmd-1","kind":"success","sourceEventSeq":3}`)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateLifecycle(tc.events); err == nil {
				t.Fatal("invalid command lifecycle was accepted")
			}
		})
	}

	if _, err := log.Append(EventCommandDone, NewCommandDone("cmd-1", "success", "again")); err == nil {
		t.Fatal("duplicate command/done append was accepted")
	}
	if got := log.NextSeq(); got != 4 {
		t.Fatalf("sequence advanced after rejected command/done: %d", got)
	}
}

func TestValidateLifecycleChecksCanonicalRetryInvariant(t *testing.T) {
	log := New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := log.Append(typ, data); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(EventTurnStart, NewTurnStartAt(1))
	appendEvent(EventStepStart, NewStepStartAt(1, 1))
	appendEvent(EventRequestHeader, NewRequestHeader("turn:1:step:1", llm.ChatRequest{Provider: "mock", Model: "m"}, "initial"))
	retry := llm.RetryEvent{
		RetryID: "retry-1", Attempt: 1, MaxRetries: 2, DelayMS: 5,
		Mode: "normal", PolicyKey: "policy-1",
		Failure: &llm.Failure{Code: "SERVER", Message: "temporary"},
	}
	appendEvent(EventLLMRetry, NewLLMRetryAt(1, 1, "mock", "m", retry))
	appendEvent(EventLLMRetryStarted, NewLLMRetryStarted(retry, 1, 1))
	appendEvent(EventStepEnd, NewStepEndAt(1, 1, "failed", "temporary"))
	appendEvent(EventTurnEnd, NewTurnEndAt(1, "failed", "temporary"))
	if err := ValidateLifecycle(log.Events()); err != nil {
		t.Fatalf("valid retry lifecycle rejected: %v", err)
	}

	cases := []struct {
		name string
		edit func([]Event)
	}{
		{"missing retry id", func(events []Event) {
			var data map[string]any
			_ = json.Unmarshal(events[3].Data, &data)
			delete(data, "retryId")
			events[3].Data, _ = json.Marshal(data)
		}},
		{"started without schedule", func(events []Event) {
			// Replace the scheduled row with a different retry ID so the started
			// transition no longer has a matching durable owner.
			var data map[string]any
			_ = json.Unmarshal(events[4].Data, &data)
			data["retryId"] = "other"
			events[4].Data, _ = json.Marshal(data)
		}},
		{"provider mismatch", func(events []Event) {
			var data map[string]any
			_ = json.Unmarshal(events[3].Data, &data)
			data["provider"] = "other"
			events[3].Data, _ = json.Marshal(data)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]Event(nil), log.Events()...)
			tc.edit(events)
			if err := ValidateLifecycle(events); err == nil {
				t.Fatal("invalid retry lifecycle was accepted")
			}
		})
	}
}

func TestAppendRejectsCanonicalRetryInvariantWithoutAdvancingLog(t *testing.T) {
	log := New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{EventTurnStart, NewTurnStartAt(1)},
		{EventStepStart, NewStepStartAt(1, 1)},
		{EventRequestHeader, NewRequestHeader("turn:1:step:1", llm.ChatRequest{Provider: "mock", Model: "m"}, "initial")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	valid := llm.RetryEvent{
		RetryID: "retry-1", Attempt: 1, MaxRetries: 1, DelayMS: 5,
		Mode: "normal", PolicyKey: "policy-1",
		Failure: &llm.Failure{Code: "SERVER", Message: "temporary"},
	}
	if _, err := log.Append(EventLLMRetry, NewLLMRetryAt(1, 1, "mock", "m", valid)); err != nil {
		t.Fatalf("valid retry append: %v", err)
	}
	wantSeq := log.NextSeq()
	invalid := valid
	invalid.RetryID = "retry-2"
	if _, err := log.Append(EventLLMRetry, NewLLMRetryAt(1, 1, "other", "m", invalid)); err == nil {
		t.Fatal("provider-mismatched retry append was accepted")
	}
	if got := log.NextSeq(); got != wantSeq {
		t.Fatalf("next seq after rejected retry = %d, want %d", got, wantSeq)
	}
	if got := len(log.Events()); got != 4 {
		t.Fatalf("event count after rejected retry = %d, want 4", got)
	}
}
