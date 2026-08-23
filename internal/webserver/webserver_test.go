// webserver_test.go — the M10 portal tests (docs/dispatch-m10.md §3): New
// validation, bearer auth, sessions/events JSON API, static hosting, the
// bounded event summary, and the /api/stats dashboard rollup (M10c §3). The
// store is a real SQLite backend on a temp dir (the same backend the REPL
// uses), seeded through CreateSession + AppendEvents.
package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

// newTestServer builds a portal over a fresh temp SQLite store.
func newTestServer(t *testing.T, token string) (*Server, *store.SQLiteStore) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(st, token, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, st
}

func doReq(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doReqBody issues a request carrying a JSON body (used by the message API).
func doReqBody(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func seedSession(t *testing.T, st *store.SQLiteStore, id string, events []session.Event) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, id, now); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.AppendEvents(ctx, id, events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := New(nil, "tok", ""); err == nil {
		t.Fatal("New with nil store must fail")
	}
	// Empty token is now valid: the portal serves open to the local machine
	// (D-WEB-2 change, user decision 2026-08-20) — no longer fail-closed.
	if _, err := New(st, "", ""); err != nil {
		t.Fatalf("New with empty token err = %v, want open portal", err)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	h := srv.Handler()
	if rec := doReq(t, h, "GET", "/api/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token → %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, "GET", "/api/sessions", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token → %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, "GET", "/api/sessions", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("right token → %d, want 200", rec.Code)
	}
	// The static shell is public so the login view can load (D-WEB-2): it
	// holds no data; only the API routes are gated.
	if rec := doReq(t, h, "GET", "/", ""); rec.Code != http.StatusOK {
		t.Fatalf("static / without token → %d, want 200 (login shell must load)", rec.Code)
	}
}

// TestNoAuthOpen verifies the D-WEB-2 change: with no token configured the API
// serves open (dsh-style local machine trust) — no login, no bearer required.
func TestNoAuthOpen(t *testing.T) {
	srv, _ := newTestServer(t, "")
	h := srv.Handler()
	if rec := doReq(t, h, "GET", "/api/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("no token configured, anonymous API → %d, want 200 (open portal)", rec.Code)
	}
	if rec := doReq(t, h, "GET", "/api/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("no token configured, anonymous health → %d, want 200", rec.Code)
	}
}

func TestSessionsList(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, "s-1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "s-2", now); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions → %d, want 200", rec.Code)
	}
	var list []sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("sessions = %d, want 2", len(list))
	}
	if list[0].ID != "s-2" {
		t.Fatalf("first session = %q, want s-2 (most recently updated first)", list[0].ID)
	}
}

func TestSessionEvents(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "你好"})},
		{Seq: 2, Type: "assistant/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "你好！"})},
		{Seq: 3, Type: "tool/result", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Name": "get_time", "Output": "2026-08-20"})},
	})
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/events", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("events → %d, want 200", rec.Code)
	}
	var evs []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[0].Type != "user/message" || evs[0].Summary != "你好" {
		t.Fatalf("ev[0] = %+v, want user/message summary 你好", evs[0])
	}
	if !strings.Contains(evs[2].Summary, "get_time") {
		t.Fatalf("tool/result summary = %q, want it to mention get_time", evs[2].Summary)
	}
	// Unknown session → 404.
	if rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-nope/events", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session → %d, want 404", rec.Code)
	}
}

func TestStaticServed(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	h := srv.Handler()
	if rec := doReq(t, h, "GET", "/", "tok"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / → %d %q, want 200 text/html", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := doReq(t, h, "GET", "/static/app.js", "tok"); rec.Code != http.StatusOK {
		t.Fatalf("GET /static/app.js → %d, want 200", rec.Code)
	}
	if rec := doReq(t, h, "GET", "/nope", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope → %d, want 404", rec.Code)
	}
}

func TestSummaryBound(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	long := strings.Repeat("字", 500)
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": long})},
	})
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/events", "tok")
	var evs []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatal(err)
	}
	s := evs[0].Summary
	if len([]rune(s)) != maxSummary+1 { // 200 runes + "…"
		t.Fatalf("summary runes = %d, want %d+1 (bounded + ellipsis)", len([]rune(s)), maxSummary)
	}
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("summary %q should end with …", s)
	}
}

func TestHealth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReq(t, srv.Handler(), "GET", "/api/health", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("health → %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["ok"] != true {
		t.Fatalf("health body = %v, want ok:true", body)
	}
}

func TestStats(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	base := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: base, Version: 1, Data: mustData(t, map[string]any{"Text": "hi"})},
		{Seq: 2, Type: "tool/result", At: base.Add(time.Minute), Version: 1, Data: mustData(t, map[string]any{"Name": "get_time", "Output": "now"})},
		{Seq: 3, Type: "tool/result", At: base.Add(2 * time.Minute), Version: 1, Data: mustData(t, map[string]any{"Name": "web_search", "Output": "ok"})},
	})
	seedSession(t, st, "s-2", []session.Event{
		{Seq: 1, Type: "assistant/message", At: base.Add(30 * time.Minute), Version: 1, Data: mustData(t, map[string]any{"Text": "hi there"})},
		{Seq: 2, Type: "tool/error", At: base.Add(31 * time.Minute), Version: 1, Data: mustData(t, map[string]any{"Name": "read", "Err": "denied"})},
		{Seq: 3, Type: "user/message", At: base.Add(32 * time.Minute), Version: 1, Data: mustData(t, map[string]any{"Text": "again"})},
	})
	// Auth: /api/stats without a token → 401 (same middleware as the rest).
	if rec := doReq(t, srv.Handler(), "GET", "/api/stats", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stats without token → %d, want 401", rec.Code)
	}
	rec := doReq(t, srv.Handler(), "GET", "/api/stats", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats → %d, want 200", rec.Code)
	}
	var stv statsView
	if err := json.Unmarshal(rec.Body.Bytes(), &stv); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stv.SessionsTotal != 2 || stv.EventsTotal != 6 || stv.ToolCalls != 2 {
		t.Fatalf("stats totals = s%d e%d t%d, want s2 e6 t2", stv.SessionsTotal, stv.EventsTotal, stv.ToolCalls)
	}
	want := map[string]int{"user/message": 2, "assistant/message": 1, "tool/result": 2, "tool/error": 1}
	if len(stv.EventTypeCounts) != len(want) {
		t.Fatalf("event_type_counts = %v, want %v", stv.EventTypeCounts, want)
	}
	for k, v := range want {
		if stv.EventTypeCounts[k] != v {
			t.Fatalf("event_type_counts[%q] = %d, want %d", k, stv.EventTypeCounts[k], v)
		}
	}
	wantActive := base.Add(32 * time.Minute)
	if !stv.LastActive.Equal(wantActive) {
		t.Fatalf("last_active = %v, want %v", stv.LastActive, wantActive)
	}
}

func TestStatsEmpty(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReq(t, srv.Handler(), "GET", "/api/stats", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats → %d, want 200", rec.Code)
	}
	var stv statsView
	if err := json.Unmarshal(rec.Body.Bytes(), &stv); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stv.SessionsTotal != 0 || stv.EventsTotal != 0 || stv.ToolCalls != 0 {
		t.Fatalf("empty stats = s%d e%d t%d, want all 0", stv.SessionsTotal, stv.EventsTotal, stv.ToolCalls)
	}
	if len(stv.EventTypeCounts) != 0 {
		t.Fatalf("event_type_counts = %v, want empty", stv.EventTypeCounts)
	}
	if !stv.LastActive.IsZero() {
		t.Fatalf("last_active = %v, want zero", stv.LastActive)
	}
}

// TestKBAdminStub verifies the M10b placeholder (ADR D-WEB-6): the /api/kb/*
// routes sit behind auth and answer 501 until KB 全量 lands.
func TestKBAdminStub(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	h := srv.Handler()
	if rec := doReq(t, h, "GET", "/api/kb", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("kb without token → %d, want 401", rec.Code)
	}
	for _, p := range []string{"/api/kb", "/api/kb/bases", "/api/kb/bases/b1/entries"} {
		rec := doReq(t, h, "GET", p, "tok")
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("GET %s → %d, want 501", p, rec.Code)
		}
	}
}

// TestMessageRequiresAuth verifies the M10 W1 message API sits behind the same
// bearer middleware as the rest (dispatch-m10-web2 §5).
func TestMessageRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "", `{"text":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("message without token → %d, want 401", rec.Code)
	}
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "wrong", `{"text":"hi"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("message with wrong token → %d, want 401", rec.Code)
	}
}

// TestMessageHandlerInvoked verifies the injected message handler: a POST with
// a non-empty text invokes msgFn with the right (sessionID, text) and answers
// 200 {"ok":true}; empty text answers 400 without invoking the handler; an
// unwired handler answers 501.
func TestMessageHandlerInvoked(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotID, gotText string
	var gotImages []llm.ImageRef
	srv.SetMessageHandler(func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error {
		gotID, gotText, gotImages = sessionID, text, images
		return nil
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "tok", `{"text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("message → %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("body = %v, want ok:true", out)
	}
	if gotID != "s-1" || gotText != "hello" || len(gotImages) != 0 {
		t.Fatalf("handler got (%q, %q, %v), want (s-1, hello, nil)", gotID, gotText, gotImages)
	}

	// Empty text → 400 and the handler is not invoked.
	gotID, gotText = "", ""
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "tok", `{"text":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text → %d, want 400", rec.Code)
	}
	if gotID != "" || gotText != "" {
		t.Fatalf("handler must not be invoked for empty text, got (%q, %q)", gotID, gotText)
	}

	// Unwired handler → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReqBody(t, srv2.Handler(), "POST", "/api/sessions/s-1/message", "tok", `{"text":"hi"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("message with nil handler → %d, want 501", rec.Code)
	}
}

// TestSessionNewResume verifies the injected session manager: POST /api/sessions
// forwards ("new", "") and returns the new id; POST /api/sessions/{id}/resume
// forwards ("resume", id); an unwired manager answers 501.
func TestSessionNewResume(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotAction, gotID string
	srv.SetSessionManager(func(ctx context.Context, action, id string) (string, error) {
		gotAction, gotID = action, id
		return "s-new", nil
	})

	rec := doReq(t, srv.Handler(), "POST", "/api/sessions", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("create → %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "s-new" {
		t.Fatalf("create body = %v, want id s-new", out)
	}
	if gotAction != "new" || gotID != "" {
		t.Fatalf("create action = (%q, %q), want (new, )", gotAction, gotID)
	}

	rec = doReq(t, srv.Handler(), "POST", "/api/sessions/s-9/resume", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume → %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "s-new" {
		t.Fatalf("resume body = %v, want id s-new", out)
	}
	if gotAction != "resume" || gotID != "s-9" {
		t.Fatalf("resume action = (%q, %q), want (resume, s-9)", gotAction, gotID)
	}

	// Unwired manager → 501 for both.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReq(t, srv2.Handler(), "POST", "/api/sessions", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("create with nil manager → %d, want 501", rec.Code)
	}
	if rec := doReq(t, srv2.Handler(), "POST", "/api/sessions/s-1/resume", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("resume with nil manager → %d, want 501", rec.Code)
	}
}

// TestSessionConfigAPI exercises the per-session override endpoints (Phase 2;
// dsh ModelSelection 对齐): POST /api/sessions stores agent_preset/model/
// permission; GET and PATCH {id}/config read and rewrite provider/model/
// reasoning_effort/permission (mode stays locked). Invalid mode/permission/
// effort are rejected; an unknown id answers 404.
func TestSessionConfigAPI(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	srv.SetSessionManager(func(ctx context.Context, action, id string) (string, error) {
		if err := st.CreateSession(ctx, "s-cfg", time.Now().UTC()); err != nil {
			return "", err
		}
		return "s-cfg", nil
	})

	// Create with per-session overrides.
	rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions", "tok",
		`{"agent_preset":"minimal","model":"deepseek-chat","permission":"readonly"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create with config → %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["agent_preset"] != "minimal" || out["model"] != "deepseek-chat" || out["permission"] != "readonly" {
		t.Fatalf("create body = %v, want minimal/deepseek-chat/readonly", out)
	}

	// GET config returns the stored overrides.
	rec = doReq(t, srv.Handler(), "GET", "/api/sessions/s-cfg/config", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("get config → %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["agent_preset"] != "minimal" || out["model"] != "deepseek-chat" || out["permission"] != "readonly" {
		t.Fatalf("get config body = %v, want minimal/deepseek-chat/readonly", out)
	}

	// PATCH rewrites the dsh selection (provider+model+effort) + permission;
	// mode stays locked.
	rec = doReqBody(t, srv.Handler(), "PATCH", "/api/sessions/s-cfg/config", "tok",
		`{"provider":"openai","model":"gpt-4o","reasoning_effort":"max","permission":"full"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch config → %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["agent_preset"] != "minimal" || out["provider"] != "openai" || out["model"] != "gpt-4o" || out["reasoning_effort"] != "max" || out["permission"] != "full" {
		t.Fatalf("patch config body = %v, want minimal/openai/gpt-4o/max/full", out)
	}

	// Unknown id → 404.
	if rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-nope/config", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("get config missing → %d, want 404", rec.Code)
	}
	if rec := doReqBody(t, srv.Handler(), "PATCH", "/api/sessions/s-nope/config", "tok", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch config missing → %d, want 404", rec.Code)
	}

	// Invalid mode / permission / effort → 400.
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/sessions", "tok", `{"agent_preset":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create invalid mode → %d, want 400", rec.Code)
	}
	if rec := doReqBody(t, srv.Handler(), "PATCH", "/api/sessions/s-cfg/config", "tok", `{"permission":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid permission → %d, want 400", rec.Code)
	}
	if rec := doReqBody(t, srv.Handler(), "PATCH", "/api/sessions/s-cfg/config", "tok", `{"reasoning_effort":"turbo"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid effort → %d, want 400", rec.Code)
	}
}

// TestSessionContextAPI verifies GET /api/sessions/{id}/context (dsh
// ContextMeter): a seeded session returns 200 with a non-zero used_tokens and a
// context_window (the wired budget), while an unknown session is 404.
func TestSessionContextAPI(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "你好，这是一个较长的消息，用于估算上下文 token 用量。"})},
		{Seq: 2, Type: "assistant/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "回你一样长度的消息，让估算非零。"})},
	})
	srv.SetContextWindow(func(sessionID string) int { return 128000 })
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/context", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("context → %d, want 200", rec.Code)
	}
	var d struct {
		UsedTokens    int     `json:"used_tokens"`
		ContextWindow int     `json:"context_window"`
		Percent       float64 `json:"percent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.UsedTokens <= 0 {
		t.Fatalf("used_tokens = %d, want > 0", d.UsedTokens)
	}
	if d.ContextWindow != 128000 {
		t.Fatalf("context_window = %d, want 128000", d.ContextWindow)
	}
	if d.Percent <= 0 {
		t.Fatalf("percent = %v, want > 0", d.Percent)
	}
	// Unknown session → 404.
	if rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-nope/context", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session → %d, want 404", rec.Code)
	}
}

// TestTurnStopAPI verifies POST /api/sessions/{id}/stop (dsh 停止按钮): a wired
// stopper forwards the session id and returns 200, an unwired server answers
// 501, and the route is auth-guarded.
func TestTurnStopAPI(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var stopped string
	srv.SetTurnStopper(func(sessionID string) error { stopped = sessionID; return nil })
	rec := doReq(t, srv.Handler(), "POST", "/api/sessions/s-9/stop", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop → %d, want 200", rec.Code)
	}
	if stopped != "s-9" {
		t.Fatalf("stopper got %q, want s-9", stopped)
	}
	// Unwired stopper → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReq(t, srv2.Handler(), "POST", "/api/sessions/s-9/stop", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("unwired stop → %d, want 501", rec.Code)
	}
	// Auth required.
	if rec := doReq(t, srv.Handler(), "POST", "/api/sessions/s-9/stop", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stop without auth → %d, want 401", rec.Code)
	}
}

// TestEventsStreamSSE verifies the SSE stream: with a seeded session and an
// injected fake event source the response is text/event-stream and the body
// carries the snapshot frames plus a synchronously pushed live frame and the
// retry hint; an unwired event source answers 501. The handler is run in a
// goroutine and the request context cancelled once the fake push lands, since a
// real SSE handler only returns on client disconnect.
func TestEventsStreamSSE(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "hi"})},
		{Seq: 2, Type: "assistant/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "hello"})},
	})
	pushed := make(chan struct{})
	srv.SetEventSource(func(sessionID string, sink func(session.Event)) func() {
		if sessionID != "s-1" {
			t.Errorf("subscribe id = %q, want s-1", sessionID)
		}
		sink(session.Event{Seq: 3, Type: "assistant/chunk", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "!"})})
		close(pushed)
		return func() {}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/sessions/s-1/events/stream", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the fake event source push")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the stream handler to exit")
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"seq":1`) || !strings.Contains(body, `"seq":2`) {
		t.Fatalf("stream body missing snapshot frames: %q", body)
	}
	if !strings.Contains(body, `data: {"seq":3`) {
		t.Fatalf("stream body missing the pushed live frame: %q", body)
	}
	if !strings.Contains(body, "retry: 3000") {
		t.Fatalf("stream body missing the retry hint: %q", body)
	}

	// Unwired event source → 501 (no stream).
	srv2, _ := newTestServer(t, "tok")
	if rec := doReq(t, srv2.Handler(), "GET", "/api/sessions/s-1/events/stream", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("stream with nil source → %d, want 501", rec.Code)
	}
}

// TestConfigAPI verifies GET /api/config (M10 W2, ADR D-WEB2-D): the route sits
// behind auth, invokes the injected config provider and serves its sanitized
// map verbatim (the redaction itself is cmd/pa's webConfig — the token key is
// served only as "***", never a plaintext); an unwired provider answers 501.
func TestConfigAPI(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	called := false
	// The fake mirrors cmd/pa's webConfig: web_server.token is redacted to
	// "***" (never the plaintext), so the boundary carries no secret.
	srv.SetConfigProvider(func() map[string]any {
		called = true
		return map[string]any{
			"model":            "deepseek-chat",
			"llm_provider":     "deepseek-official",
			"mode":             "standard",
			"web_server_addr":  "127.0.0.1:8080",
			"web_server.token": "***",
			"web_enabled":      true,
		}
	})

	// Auth gate.
	if rec := doReq(t, srv.Handler(), "GET", "/api/config", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("config without token → %d, want 401", rec.Code)
	}

	rec := doReq(t, srv.Handler(), "GET", "/api/config", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("config → %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("cfgFn must be invoked")
	}
	body := rec.Body.String()
	// Redaction shape at the boundary: the token key is masked and the
	// plaintext never appears (a buggy provider that served it would fail here).
	if !strings.Contains(body, `"web_server.token":"***"`) {
		t.Fatalf("config body must carry the redacted token marker: %s", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("config body leaks a plaintext token: %s", body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "deepseek-chat" || out["web_enabled"] != true {
		t.Fatalf("config = %v, want model deepseek-chat and web_enabled true", out)
	}

	// Unwired provider → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReq(t, srv2.Handler(), "GET", "/api/config", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("config with nil provider → %d, want 501", rec.Code)
	}
}

func mustData(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return b
}

// TestEventsExtendedFields verifies the M10 W4 (D-WEB2-H) event fields: the
// assistant reasoning chain and the tool-card title/output are extracted from
// the event Data (bounded), and absent on other types.
func TestEventsExtendedFields(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "assistant/message", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"Text": "答案", "Reasoning": "先想两步再回答"})},
		{Seq: 2, Type: "tool/result", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"Name": "get_time", "Output": "2026-08-20 12:00"})},
		{Seq: 3, Type: "user/message", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"Text": "hi"})},
	})
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/events", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("events → %d, want 200", rec.Code)
	}
	var evs []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[0].Reasoning != "先想两步再回答" {
		t.Fatalf("assistant/message reasoning = %q, want the chain", evs[0].Reasoning)
	}
	if evs[1].ToolName != "get_time" || evs[1].ToolOutput != "2026-08-20 12:00" {
		t.Fatalf("tool/result tool_name/tool_output = %q/%q, want get_time/2026-08-20 12:00", evs[1].ToolName, evs[1].ToolOutput)
	}
	if evs[2].Reasoning != "" || evs[2].ToolName != "" || evs[2].ToolOutput != "" {
		t.Fatalf("user/message must carry no extended fields, got %+v", evs[2])
	}
}

// TestEventViewToolArgs verifies the details-panel input field: the events API
// attaches a tool/result's arguments from the preceding assistant/message
// toolCalls (read-only view field; the session log format is unchanged).
func TestEventViewToolArgs(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "assistant/message", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"Text": "答案", "toolCalls": []llm.ToolCall{
				{ID: "call_1", Name: "get_time", Arguments: `{"zone":"UTC"}`},
			}})},
		{Seq: 2, Type: "tool/result", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"callId": "call_1", "Name": "get_time", "Output": "12:00"})},
		{Seq: 3, Type: "tool/result", At: time.Now(), Version: 1,
			Data: mustData(t, map[string]any{"callId": "call_2", "Name": "unknown_tool", "Output": "x"})},
	})
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/events", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("events → %d, want 200", rec.Code)
	}
	var evs []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[1].ToolArgs != `{"zone":"UTC"}` {
		t.Fatalf("tool/result tool_args = %q, want the call arguments", evs[1].ToolArgs)
	}
	if evs[2].ToolArgs != "" {
		t.Fatalf("unmatched tool/result tool_args = %q, want empty", evs[2].ToolArgs)
	}
	if evs[0].ToolArgs != "" {
		t.Fatalf("assistant/message tool_args = %q, want empty", evs[0].ToolArgs)
	}
}

// TestSubagentsJobsAPI verifies the M10 W4 (D-WEB2-H) status panels: a wired
// provider answers its sanitized list; a nil provider answers 501; with a
// configured token the route stays behind requireAuth.
func TestSubagentsJobsAPI(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetSubagentProvider(func(ctx context.Context, sessionID string) ([]map[string]any, error) {
		if sessionID != "" && sessionID != "s-1" {
			return []map[string]any{}, nil
		}
		return []map[string]any{{"id": "a-1", "label": "task", "running": true}}, nil
	})
	srv.SetJobsProvider(func(ctx context.Context, sessionID string) ([]map[string]any, error) {
		return []map[string]any{{"id": "job-1", "kind": "workflow", "label": "wf", "status": "running"}}, nil
	})
	h := srv.Handler()

	rec := doReq(t, h, "GET", "/api/subagents?session_id=s-1", "tok")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a-1") {
		t.Fatalf("subagents → %d %q, want 200 with a-1", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/subagents?session_id=s-2", "tok")
	if !strings.Contains(rec.Body.String(), `"subagents":[]`) {
		t.Fatalf("subagents other session → %q, want empty", rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/jobs", "tok")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "job-1") {
		t.Fatalf("jobs → %d %q, want 200 with job-1", rec.Code, rec.Body.String())
	}

	// Unwired providers → 501; auth still applies when a token is configured.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReq(t, srv2.Handler(), "GET", "/api/subagents", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("subagents nil → %d, want 501", rec.Code)
	}
	if rec := doReq(t, srv2.Handler(), "GET", "/api/jobs", "tok"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("jobs nil → %d, want 501", rec.Code)
	}
	if rec := doReq(t, h, "GET", "/api/subagents", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("subagents no token → %d, want 401", rec.Code)
	}
}

// TestSessionTitleDelete covers the P2 sidebar management API: rename
// (PATCH /api/sessions/{id}/title) with override + clear, and delete
// (DELETE /api/sessions/{id}) with cascade + 404 for unknown ids.
func TestSessionTitleDelete(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1, Data: mustData(t, map[string]any{"Text": "帮我写首诗"})},
	})
	h := srv.Handler()

	// Inference title before any override.
	rec := doReq(t, h, "GET", "/api/sessions", "tok")
	var list []sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].Title != "帮我写首诗" {
		t.Fatalf("inferred title = %q, want 帮我写首诗", list[0].Title)
	}

	// Rename overrides the inferred title.
	body := strings.NewReader(`{"title":"我的新名字"}`)
	req := httptest.NewRequest("PATCH", "/api/sessions/s-1/title", body)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH title → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/sessions", "tok")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list 2: %v", err)
	}
	if list[0].Title != "我的新名字" {
		t.Fatalf("renamed title = %q, want 我的新名字", list[0].Title)
	}

	// Unknown id → 404.
	req = httptest.NewRequest("PATCH", "/api/sessions/s-nope/title", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown → %d, want 404", rec.Code)
	}

	// Delete removes the session and its events.
	req = httptest.NewRequest("DELETE", "/api/sessions/s-1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE → %d, want 200", rec.Code)
	}
	rec = doReq(t, h, "GET", "/api/sessions", "tok")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list 3: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("sessions after delete = %d, want 0", len(list))
	}
	// Events cascade-deleted: loading the session is now a 404.
	if rec := doReq(t, h, "GET", "/api/sessions/s-1/events", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("events after delete → %d, want 404", rec.Code)
	}
	req = httptest.NewRequest("DELETE", "/api/sessions/s-1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown → %d, want 404", rec.Code)
	}
}

// png1x1 is a minimal valid 1x1 PNG byte stream (media type image/png).
var png1x1 = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49,
	0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02,
	0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44,
	0x41, 0x54, 0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x03, 0x00,
	0x01, 0x6D, 0x26, 0x0B, 0xBC, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44,
	0xAE, 0x42, 0x60, 0x82,
}

// TestWorkspaceAPI covers the P6 grouping API: create (with empty-title
// rejection), list (workspaces + ungrouped ids), rename, session creation into
// a group, and delete returning sessions to the ungrouped bucket.
func TestWorkspaceAPI(t *testing.T) {
	srv, st := newTestServer(t, "")
	h := srv.Handler()

	// Empty title → 400.
	if rec := doReqBody(t, h, "POST", "/api/workspaces", "", `{"title":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty title → %d, want 400", rec.Code)
	}

	// Create two workspaces.
	rec := doReqBody(t, h, "POST", "/api/workspaces", "", `{"title":"研究"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create → %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" || created.Title != "研究" {
		t.Fatalf("create resp = %s", rec.Body.String())
	}
	rec = doReqBody(t, h, "POST", "/api/workspaces", "", `{"title":"日常"}`)
	var w2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &w2); err != nil {
		t.Fatalf("create w2: %v", err)
	}

	// Seed sessions: one into the first workspace, one ungrouped.
	seedSession(t, st, "s1", []session.Event{{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"hi"}`)}})
	seedSession(t, st, "s2", []session.Event{{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"yo"}`)}})
	if err := st.SetSessionWorkspace(context.Background(), "s1", created.ID); err != nil {
		t.Fatalf("assign s1: %v", err)
	}

	rec = doReq(t, h, "GET", "/api/workspaces", "")
	var list struct {
		Workspaces []struct {
			ID         string   `json:"id"`
			Title      string   `json:"title"`
			SessionIDs []string `json:"session_ids"`
			CreatedAt  int64    `json:"created_at"`
		} `json:"workspaces"`
		UngroupedIDs []string `json:"ungrouped_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v (%s)", err, rec.Body.String())
	}
	if len(list.Workspaces) != 2 || list.Workspaces[0].Title != "研究" {
		t.Fatalf("workspaces = %+v", list.Workspaces)
	}
	if list.Workspaces[0].CreatedAt <= 0 {
		t.Fatalf("workspace created_at = %d, want > 0 (dsh workspace hover card)", list.Workspaces[0].CreatedAt)
	}
	if len(list.Workspaces[0].SessionIDs) != 1 || list.Workspaces[0].SessionIDs[0] != "s1" {
		t.Fatalf("w1 sessions = %v, want [s1]", list.Workspaces[0].SessionIDs)
	}
	if len(list.UngroupedIDs) != 1 || list.UngroupedIDs[0] != "s2" {
		t.Fatalf("ungrouped = %v, want [s2]", list.UngroupedIDs)
	}

	// Rename.
	rec = doReqBody(t, h, "PATCH", "/api/workspaces/"+created.ID, "", `{"title":"研究·改"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename → %d: %s", rec.Code, rec.Body.String())
	}
	rec = doReqBody(t, h, "PATCH", "/api/workspaces/nope", "", `{"title":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rename unknown → %d, want 404", rec.Code)
	}

	// Create a session directly into a group via POST /api/sessions (the real
	// session manager materializes the row; the mock mirrors that).
	srv.SetSessionManager(func(ctx context.Context, action, id string) (string, error) {
		if err := st.CreateSession(ctx, "s3", time.Now().UTC()); err != nil {
			return "", err
		}
		return "s3", nil
	})
	rec = doReqBody(t, h, "POST", "/api/sessions", "", `{"workspace_id":"`+created.ID+`"}`)
	var sc struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sc); err != nil || sc.WorkspaceID != created.ID {
		t.Fatalf("session create resp = %s", rec.Body.String())
	}

	// Delete w1 → s1 and s3 return to ungrouped.
	rec = doReq(t, h, "DELETE", "/api/workspaces/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete → %d: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "DELETE", "/api/workspaces/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete again → %d, want 404", rec.Code)
	}
	rec = doReq(t, h, "GET", "/api/workspaces", "")
	list = struct {
		Workspaces []struct {
			ID         string   `json:"id"`
			Title      string   `json:"title"`
			SessionIDs []string `json:"session_ids"`
			CreatedAt  int64    `json:"created_at"`
		} `json:"workspaces"`
		UngroupedIDs []string `json:"ungrouped_ids"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list.Workspaces) != 1 {
		t.Fatalf("workspaces after delete = %d, want 1", len(list.Workspaces))
	}
	if len(list.UngroupedIDs) != 3 {
		t.Fatalf("ungrouped after delete = %v, want all 3 sessions", list.UngroupedIDs)
	}
}

// TestSessionForkArchiveOrder covers P6.2: fork clones the event log, archive
// leaves the active list, unarchive restores it, and drag order moves/orders
// sessions and workspaces.
func TestSessionForkArchiveOrder(t *testing.T) {
	srv, st := newTestServer(t, "")
	h := srv.Handler()
	seedSession(t, st, "s1", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"hi"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"yo"}`)},
	})
	if err := st.CreateWorkspace(context.Background(), "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}

	// Fork clones the log into a new session in the same workspace.
	rec := doReq(t, h, "POST", "/api/sessions/s1/fork", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("fork → %d: %s", rec.Code, rec.Body.String())
	}
	var fork struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fork); err != nil || fork.ID == "" {
		t.Fatalf("fork resp = %s", rec.Body.String())
	}
	events, err := st.LoadSession(context.Background(), fork.ID)
	if err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 || events[0].Type != session.EventUserMessage {
		t.Fatalf("fork events = %+v", events)
	}
	// unknown → 404
	if rec := doReq(t, h, "POST", "/api/sessions/nope/fork", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("fork unknown → %d, want 404", rec.Code)
	}

	// Archive removes from the active list; unarchive brings it back.
	if rec := doReq(t, h, "POST", "/api/sessions/s1/archive", ""); rec.Code != http.StatusOK {
		t.Fatalf("archive → %d", rec.Code)
	}
	rec = doReq(t, h, "GET", "/api/sessions", "")
	var sl []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sl); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	for _, s := range sl {
		if s.ID == "s1" {
			t.Fatal("archived s1 still in active list")
		}
	}
	if rec := doReq(t, h, "POST", "/api/sessions/s1/unarchive", ""); rec.Code != http.StatusOK {
		t.Fatalf("unarchive → %d", rec.Code)
	}
	rec = doReq(t, h, "GET", "/api/sessions", "")
	sl = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &sl); err != nil {
		t.Fatalf("list decode 2: %v", err)
	}
	found := false
	for _, s := range sl {
		if s.ID == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatal("s1 not restored after unarchive")
	}

	// Manual order moves sessions into w1 and reorders; then workspace order.
	if rec := doReqBody(t, h, "PATCH", "/api/sessions/order", "", `{"workspace_id":"w1","session_ids":["s1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("order sessions → %d: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/workspaces", "")
	var groups struct {
		Workspaces []struct {
			ID         string   `json:"id"`
			SessionIDs []string `json:"session_ids"`
		} `json:"workspaces"`
		UngroupedIDs []string `json:"ungrouped_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("groups decode: %v", err)
	}
	if len(groups.Workspaces[0].SessionIDs) != 1 || groups.Workspaces[0].SessionIDs[0] != "s1" {
		t.Fatalf("group after order = %+v", groups.Workspaces[0])
	}
	if rec := doReqBody(t, h, "PATCH", "/api/workspaces/order", "", `{"ids":["w1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("order workspaces → %d", rec.Code)
	}
}

// TestSearchAPI covers GET /api/search (P6.3): body-text hits with snippets.
func TestSearchAPI(t *testing.T) {
	srv, st := newTestServer(t, "")
	h := srv.Handler()
	seedSession(t, st, "s1", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"部署 dsh 网关"}`)},
	})
	seedSession(t, st, "s2", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"无关内容"}`)},
	})

	rec := doReq(t, h, "GET", "/api/search?q="+url.QueryEscape("网关"), "")
	var res struct {
		Hits []struct {
			ID      string `json:"id"`
			Snippet string `json:"snippet"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != "s1" || res.Hits[0].Snippet == "" {
		t.Fatalf("hits = %+v, want one s1 with snippet", res.Hits)
	}
	// Empty query → empty hits.
	rec = doReq(t, h, "GET", "/api/search?q=", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Hits) != 0 {
		t.Fatalf("empty query hits = %+v", res.Hits)
	}
}

// TestModelSwitch covers the P5.1 live model switch (POST /api/config/model):
// an injected switcher receives provider/model and answers 200; an empty body
// answers 400; a rejected switch (switcher error) answers 400; an unwired
// switcher answers 501.
func TestModelSwitch(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotP, gotM, gotE string
	srv.SetModelSwitcher(func(ctx context.Context, provider, model, effort string) error {
		gotP, gotM, gotE = provider, model, effort
		return nil
	})
	rec := doReqBody(t, srv.Handler(), "POST", "/api/config/model", "tok",
		`{"provider":"deepseek-official","model":"deepseek-reasoner"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("model switch → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if gotP != "deepseek-official" || gotM != "deepseek-reasoner" {
		t.Fatalf("switcher got (%q, %q), want (deepseek, deepseek-reasoner)", gotP, gotM)
	}

	// reasoning_effort rides the same switch (dsh 思考强度).
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/model", "tok",
		`{"reasoning_effort":"high"}`); rec.Code != http.StatusOK {
		t.Fatalf("effort switch → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if gotE != "high" {
		t.Fatalf("effort = %q, want high", gotE)
	}

	// Empty body → 400 (neither provider, model nor effort).
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/model", "tok", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty switch → %d, want 400", rec.Code)
	}

	// Switcher rejection → 400.
	srv.SetModelSwitcher(func(ctx context.Context, provider, model, effort string) error { return errors.New("unknown provider") })
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/model", "tok", `{"model":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected switch → %d, want 400", rec.Code)
	}

	// Unwired switcher → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReqBody(t, srv2.Handler(), "POST", "/api/config/model", "tok", `{"model":"x"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("switch without wire → %d, want 501", rec.Code)
	}
}

// TestProviderManager covers the M11 provider-management API: POST
// /api/config/provider dispatches a save (custom provider profile or built-in
// key override) to the injected manager and answers 200; an empty id answers
// 400; a rejected save answers 400; DELETE dispatches a delete; an unwired
// manager answers 501.
func TestProviderManager(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var gotAction string
	var gotEdit ProviderEdit
	srv.SetProviderManager(func(ctx context.Context, action string, edit ProviderEdit) error {
		gotAction, gotEdit = action, edit
		return nil
	})

	// Save a custom provider.
	rec := doReqBody(t, srv.Handler(), "POST", "/api/config/provider", "tok",
		`{"id":"ollama","name":"Ollama","base_url":"http://localhost:11434/v1","model":"llama3.1","api_key":"k","custom":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom save → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if gotAction != "save" || gotEdit.ID != "ollama" || !gotEdit.Custom || gotEdit.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("manager got %q %#v, want save custom ollama", gotAction, gotEdit)
	}

	// Empty id → 400.
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/provider", "tok", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty id save → %d, want 400", rec.Code)
	}

	// Manager rejection → 400.
	srv.SetProviderManager(func(ctx context.Context, action string, edit ProviderEdit) error { return errors.New("bad route") })
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/provider", "tok", `{"id":"x","name":"X","base_url":"http://x/v1","model":"m"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected save → %d, want 400", rec.Code)
	}

	// DELETE dispatches a delete for the custom provider.
	srv.SetProviderManager(func(ctx context.Context, action string, edit ProviderEdit) error {
		gotAction, gotEdit = action, edit
		return nil
	})
	if rec := doReqBody(t, srv.Handler(), "DELETE", "/api/config/provider", "tok", `{"id":"ollama"}`); rec.Code != http.StatusOK {
		t.Fatalf("delete → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if gotAction != "delete" || gotEdit.ID != "ollama" {
		t.Fatalf("manager got %q %#v, want delete ollama", gotAction, gotEdit)
	}

	// Unwired manager → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReqBody(t, srv2.Handler(), "POST", "/api/config/provider", "tok", `{"id":"x"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("save without wire → %d, want 501", rec.Code)
	}
}

// TestProviderDiscover covers POST /api/config/provider/discover (M11-pi-ai):
// a wired discoverer passes the probe payload and returns candidate models;
// a rejected probe answers 400; an unwired discoverer answers 501.
func TestProviderDiscover(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	var got ProviderDiscover
	srv.SetProviderDiscover(func(ctx context.Context, request ProviderDiscover) ([]ProviderModel, error) {
		got = request
		return []ProviderModel{{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000, MaxTokens: 16384}, {ID: "gpt-4o-mini"}}, nil
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/config/provider/discover", "tok",
		`{"provider":"","base_url":"https://gw.example/v1","protocol":"openai-completions","api_key":"k"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.BaseURL != "https://gw.example/v1" || got.Protocol != "openai-completions" || got.APIKey != "k" {
		t.Fatalf("discover got %#v", got)
	}
	var body struct {
		Models []ProviderModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Models) != 2 || body.Models[0].ID != "gpt-4o" || body.Models[0].ContextWindow != 128000 {
		t.Fatalf("models = %#v", body.Models)
	}

	// Rejected probe → 400.
	srv.SetProviderDiscover(func(ctx context.Context, request ProviderDiscover) ([]ProviderModel, error) {
		return nil, errors.New("协议 \"anthropic-messages\" 无模型列表可读")
	})
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/config/provider/discover", "tok",
		`{"base_url":"https://x","protocol":"anthropic-messages"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected discover → %d, want 400", rec.Code)
	}

	// Unwired discoverer → 501.
	srv2, _ := newTestServer(t, "tok")
	if rec := doReqBody(t, srv2.Handler(), "POST", "/api/config/provider/discover", "tok", `{"base_url":"https://x"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("discover without wire → %d, want 501", rec.Code)
	}
}

// TestAttachmentUploadGet covers the P5 attachment APIs: a multipart upload
// (POST) returns the attachment view, the byte echo (GET) returns the stored
// bytes with the right Content-Type, unknown ids answer 404, and an unwired
// store answers 501.
func TestAttachmentUploadGet(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "att"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv.SetAttachmentStore(att)
	if err := st.CreateSession(context.Background(), "s-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Upload a PNG through a multipart form.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "pic.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(png1x1); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/sessions/s-1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload → %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var av attachmentView
	if err := json.Unmarshal(rec.Body.Bytes(), &av); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if av.ID == "" || av.MediaType != "image/png" || av.Bytes != int64(len(png1x1)) {
		t.Fatalf("attachment view = %+v, want png %d bytes", av, len(png1x1))
	}

	// Echo the bytes back.
	rec = doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/attachments/"+av.ID, "tok")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("echo → %d %q, want 200 image/png", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), png1x1) {
		t.Fatalf("echo bytes differ from upload")
	}

	// Unknown attachment id → 404; upload to unknown session → 404.
	if rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/attachments/nope", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown attachment → %d, want 404", rec.Code)
	}
	if rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-nope/attachments/"+av.ID, "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session echo → %d, want 404", rec.Code)
	}

	// Unwired store → 501.
	srv2, _ := newTestServer(t, "tok")
	req = httptest.NewRequest("POST", "/api/sessions/s-1/attachments", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("upload without store → %d, want 501", rec.Code)
	}
}

// TestMessageWithImages covers the P5 images field of POST /api/sessions/
// {id}/message: uploaded ids are resolved to ImageRefs and passed to the
// injected handler; an unknown id answers 400; images without a store answer
// 501.
func TestMessageWithImages(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	att, err := attachment.NewStore(filepath.Join(t.TempDir(), "att"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv.SetAttachmentStore(att)
	if err := st.CreateSession(context.Background(), "s-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	var gotText string
	var gotImages []llm.ImageRef
	srv.SetMessageHandler(func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error {
		gotText, gotImages = text, images
		return nil
	})

	// Upload, then send a message carrying the image id.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "pic.png")
	_, _ = fw.Write(png1x1)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/sessions/s-1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var av attachmentView
	_ = json.Unmarshal(rec.Body.Bytes(), &av)

	rec = doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "tok",
		`{"text":"描述","images":["`+av.ID+`"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("message with image → %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if gotText != "描述" || len(gotImages) != 1 || gotImages[0].ID != av.ID || gotImages[0].MediaType != "image/png" {
		t.Fatalf("handler got (%q, %+v), want (描述, [png %s])", gotText, gotImages, av.ID)
	}

	// Unknown image id → 400.
	rec = doReqBody(t, srv.Handler(), "POST", "/api/sessions/s-1/message", "tok",
		`{"text":"x","images":["nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown image → %d, want 400", rec.Code)
	}

	// Images without a wired store → 501.
	srv2, _ := newTestServer(t, "tok")
	srv2.SetMessageHandler(func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error { return nil })
	rec = doReqBody(t, srv2.Handler(), "POST", "/api/sessions/s-1/message", "tok",
		`{"text":"x","images":["any"]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("images without store → %d, want 501", rec.Code)
	}
}

// TestEventViewImages verifies the events API exposes the image refs carried by
// a user/message event (only ref metadata — bytes stay in the attachment store).
func TestEventViewImages(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	ref := llm.ImageRef{ID: "img-1", MediaType: "image/png"}
	seedSession(t, st, "s-1", []session.Event{
		{Seq: 1, Type: "user/message", At: time.Now(), Version: 1,
			Data: mustData(t, session.NewUserMessageWithBlocks("看图", []llm.ContentBlock{{Kind: llm.BlockImage, Image: ref}}))},
	})
	rec := doReq(t, srv.Handler(), "GET", "/api/sessions/s-1/events", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("events → %d, want 200", rec.Code)
	}
	var evs []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &evs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 1 || len(evs[0].Images) != 1 {
		t.Fatalf("images = %d, want 1 (event %+v)", len(evs[0].Images), evs[0])
	}
	if evs[0].Images[0].ID != "img-1" || evs[0].Images[0].MediaType != "image/png" {
		t.Fatalf("image view = %+v, want img-1/png", evs[0].Images[0])
	}
}
