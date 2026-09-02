package webserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

// TestWebAuthorizationReplayAndOwnerMatrix pins the browser-facing security
// boundary in one place: bearer admission, cross-origin mutation fencing,
// addressed-owner callbacks, fail-closed secrets, deterministic SSE replay,
// and stable unknown-interaction handling. Production queue ownership and
// shutdown admission are covered by cmd/pa's matrix test.
func TestWebAuthorizationReplayAndOwnerMatrix(t *testing.T) {
	srv, st := newTestServer(t, "secret-token")
	seedSession(t, st, "true-parent", nil)
	seedSession(t, st, "other-parent", nil)
	seedSession(t, st, "owned-child", []session.Event{{
		Seq: 1, Type: session.EventSubagentStart, At: time.UnixMilli(1001), Version: session.EventVersion,
		Data: json.RawMessage(`{"parentSession":"true-parent","depth":1}`),
	}})

	queue := map[string][]QueueItem{}
	srv.SetQueueManager(
		func(_ context.Context, sessionID string) ([]QueueItem, error) {
			items := queue[sessionID]
			if items == nil {
				items = []QueueItem{}
			}
			return items, nil
		},
		func(_ context.Context, sessionID, text string) (QueueItem, error) {
			item := QueueItem{ID: sessionID + "-item", Text: text, CreatedAt: time.Unix(1, 0), Placement: "queued"}
			queue[sessionID] = append(queue[sessionID], item)
			return item, nil
		},
		func(_ context.Context, sessionID, itemID, action string) error {
			for _, item := range queue[sessionID] {
				if item.ID == itemID {
					return nil
				}
			}
			return fmt.Errorf("queue item %q is not owned by session %q", itemID, sessionID)
		},
	)
	srv.SetInteractionManager(
		func(context.Context, string) ([]interact.Request, error) { return nil, nil },
		func(context.Context, string, string, interact.ApprovalStatus, string) error {
			return interact.ErrUnknownRequest
		},
	)
	srv.SetNativeCredentialManager(
		func(context.Context, string, string) error { return fmt.Errorf("credential store rejected the change") },
		func(context.Context, string) error { return fmt.Errorf("credential store rejected the delete") },
	)
	srv.SetNativeSubagentManager(
		func(context.Context, string, string) error {
			t.Error("unauthorized subagent prompt reached the runtime")
			return nil
		},
		func(string, string) error {
			t.Error("unauthorized subagent interrupt reached the runtime")
			return nil
		},
	)
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{{
			"id": "provider-one", "available": true, "configured": true,
			"api_key": "catalog-plaintext-secret", "model": "configured-model",
		}}}
	})
	srv.SetEventSource(func(string, func(session.Event)) func() {
		t.Error("malformed cursor test started a live stream")
		return func() {}
	})

	h := srv.Handler()

	if rec := doReq(t, h, http.MethodGet, "/api/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer token = %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/api/sessions", "wrong-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer token = %d, want 401", rec.Code)
	}

	crossOrigin := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/sessions", `{}`},
		{http.MethodPost, "/api/sessions/true-parent/queue", `{"text":"must reject"}`},
		{http.MethodPatch, "/api/settings", `{"language":"en"}`},
	}
	for _, mutation := range crossOrigin {
		req := httptest.NewRequest(mutation.method, mutation.path, strings.NewReader(mutation.body))
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://attacker.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin %s %s = %d %s, want 403", mutation.method, mutation.path, rec.Code, rec.Body.String())
		}
	}
	if len(queue["true-parent"]) != 0 {
		t.Fatalf("cross-origin queue mutation changed state: %#v", queue["true-parent"])
	}

	local := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"language":"en"}`))
	local.Header.Set("Authorization", "Bearer secret-token")
	local.Header.Set("Content-Type", "application/json")
	localRec := httptest.NewRecorder()
	h.ServeHTTP(localRec, local)
	if localRec.Code != http.StatusOK {
		t.Fatalf("non-browser local mutation = %d %s, want 200", localRec.Code, localRec.Body.String())
	}

	rec := doReqBody(t, h, http.MethodPost, "/api/sessions/true-parent/queue", "secret-token", `{"text":"owned work"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owned queue enqueue = %d %s, want 202", rec.Code, rec.Body.String())
	}
	rec = doReqBody(t, h, http.MethodPatch, "/api/sessions/other-parent/queue/true-parent-item", "secret-token", `{"action":"delete"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("foreign queue item update was admitted: %s", rec.Body.String())
	}
	if len(queue["true-parent"]) != 1 || len(queue["other-parent"]) != 0 {
		t.Fatalf("foreign queue update changed ownership state: %#v", queue)
	}

	rec = doReqBody(t, h, http.MethodPost, "/api/interactions/unknown/resolve?session_id=true-parent", "secret-token", `{"status":"approved"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown interaction = %d %s, want 409", rec.Code, rec.Body.String())
	}

	nativeCall := func(rpcID, method, payload string) nativeRPCResponse {
		t.Helper()
		body := fmt.Sprintf(`{"type":"client-request","rpcId":%q,"method":%q,"payload":%s}`, rpcID, method, payload)
		rec := doReqBody(t, h, http.MethodPost, "/api/"+method, "secret-token", body)
		return nativeResponse(t, rec.Body.Bytes())
	}
	unauthorizedHistory := nativeCall("history-owner", "subagent.history", `{"parentSessionId":"other-parent","childSessionId":"owned-child","mode":"one-shot"}`)
	if unauthorizedHistory.Result.OK || unauthorizedHistory.Result.Error == nil || unauthorizedHistory.Result.Error.Code != "subagent-unauthorized" {
		t.Fatalf("foreign subagent history = %+v", unauthorizedHistory)
	}
	unauthorizedPrompt := nativeCall("prompt-owner", "subagent.prompt", `{"parentSessionId":"other-parent","childSessionId":"owned-child","mode":"continuable","content":[{"type":"text","text":"must reject"}]}`)
	if unauthorizedPrompt.Result.OK || unauthorizedPrompt.Result.Error == nil || unauthorizedPrompt.Result.Error.Code != "subagent-unauthorized" {
		t.Fatalf("foreign subagent prompt = %+v", unauthorizedPrompt)
	}
	unauthorizedInterrupt := nativeCall("interrupt-owner", "subagent.interrupt", `{"parentSessionId":"other-parent","childSessionId":"owned-child","mode":"continuable"}`)
	if unauthorizedInterrupt.Result.OK || unauthorizedInterrupt.Result.Error == nil || unauthorizedInterrupt.Result.Error.Code != "subagent-unauthorized" {
		t.Fatalf("foreign subagent interrupt = %+v", unauthorizedInterrupt)
	}

	rec = doReqBody(t, h, http.MethodPost, "/api/credentials.set", "secret-token", `{"type":"client-request","rpcId":"credential-owner","method":"credentials.set","payload":{"ref":"TEST_API_KEY","value":"credential-plaintext-secret"}}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credential-plaintext-secret") {
		t.Fatalf("credential set response leaked or mishandled the secret: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReqBody(t, h, http.MethodPost, "/api/llm.providers", "secret-token", `{"type":"client-request","rpcId":"provider-owner","method":"llm.providers","payload":{}}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "catalog-plaintext-secret") {
		t.Fatalf("provider catalog leaked a secret: %d %s", rec.Code, rec.Body.String())
	}

	badCursor := httptest.NewRequest(http.MethodGet, "/api/sessions/true-parent/events/stream", nil)
	badCursor.Header.Set("Authorization", "Bearer secret-token")
	badCursor.Header.Set("Last-Event-ID", "not-a-seq")
	badCursorRec := httptest.NewRecorder()
	h.ServeHTTP(badCursorRec, badCursor)
	if badCursorRec.Code != http.StatusBadRequest {
		t.Fatalf("malformed stream cursor = %d, want 400", badCursorRec.Code)
	}
	assertSSEReplaySuffix(t, srv, st)
}

func assertSSEReplaySuffix(t *testing.T, srv *Server, st *store.SQLiteStore) {
	t.Helper()
	if err := st.AppendEvents(context.Background(), "true-parent", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(2001), Version: session.EventVersion, Data: json.RawMessage(`{"text":"old"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(2002), Version: session.EventVersion, Data: json.RawMessage(`{"text":"current"}`)},
	}); err != nil {
		t.Fatalf("seed replay events: %v", err)
	}
	pushed := make(chan struct{})
	srv.SetEventSource(func(sessionID string, sink func(session.Event)) func() {
		if sessionID != "true-parent" {
			t.Errorf("replay subscribed %q, want true-parent", sessionID)
		}
		sink(session.Event{Seq: 3, Type: session.EventAssistantChunk, At: time.UnixMilli(2003), Version: session.EventVersion, Data: json.RawMessage(`{"text":"live"}`)})
		close(pushed)
		return func() {}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/true-parent/events/stream", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Last-Event-ID", "1")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for replay live event")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for replay stream")
	}
	body := rec.Body.String()
	if strings.Contains(body, `"seq":1`) || !strings.Contains(body, `"seq":2`) || !strings.Contains(body, `"seq":3`) {
		t.Fatalf("Last-Event-ID replay body = %q, want only suffix seq 2 and 3", body)
	}
}
