package sessionquery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

type fakeBackend struct {
	hits  []store.SearchHit
	logs  map[string][]session.Event
	metas []store.SessionMeta
}

func (f fakeBackend) SearchSessions(context.Context, string) ([]store.SearchHit, error) {
	return f.hits, nil
}
func (f fakeBackend) LoadSession(_ context.Context, id string) ([]session.Event, error) {
	return f.logs[id], nil
}
func (f fakeBackend) ListSessions(context.Context) ([]store.SessionMeta, error) { return f.metas, nil }

func testTools() *Tools {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("deploy the service"))
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessage("deployed", nil, "stop"))
	return NewTools(fakeBackend{
		hits: []store.SearchHit{
			{SessionID: "old", Title: "Deploy", UpdatedAt: time.Unix(10, 0), Snippet: "deploy the service"},
			{SessionID: "private", Title: "Private", UpdatedAt: time.Unix(11, 0), Snippet: "deploy the service"},
		},
		logs: map[string][]session.Event{"current": log.Events(), "old": log.Events()},
		metas: []store.SessionMeta{
			{ID: "current", CreatedAt: time.Unix(1, 0), WorkspaceID: "w1"},
			{ID: "old", CreatedAt: time.Unix(2, 0), WorkspaceID: "w1"},
			{ID: "private", CreatedAt: time.Unix(3, 0), WorkspaceID: "w2"},
		},
	}, func() string { return "current" })
}

func TestSearchAndEventSearch(t *testing.T) {
	ts := testTools()
	out, err := ts.Search().Execute(context.Background(), json.RawMessage(`{"query":"deploy"}`))
	if err != nil || !strings.Contains(out, "old") {
		t.Fatalf("session search = %q, err=%v", out, err)
	}
	out, err = ts.EventSearch().Execute(context.Background(), json.RawMessage(`{"query":"service"}`))
	if err != nil || !strings.Contains(out, "seq 1") {
		t.Fatalf("event search = %q, err=%v", out, err)
	}
}

func TestEventReadBoundsAndTrace(t *testing.T) {
	ts := testTools()
	out, err := ts.Read().Execute(context.Background(), json.RawMessage(`{"seq":1,"after":1}`))
	if err != nil || !strings.Contains(out, "Target event seq 1") || !strings.Contains(out, "After:") {
		t.Fatalf("event read = %q, err=%v", out, err)
	}
	out, err = ts.EventTrace().Execute(context.Background(), json.RawMessage(`{"seq":1}`))
	if err != nil || !strings.Contains(out, "Replaced by: none") {
		t.Fatalf("event trace = %q, err=%v", out, err)
	}
}

func TestWorkspaceScopeAndDerivedLineage(t *testing.T) {
	parent := session.New()
	_, _ = parent.Append(session.EventSubagentStart, session.NewSubagentStart("child", "spawn", "parent", "worker"))
	backend := fakeBackend{
		hits: []store.SearchHit{
			{SessionID: "child", Title: "Child", UpdatedAt: time.Unix(2, 0), Snippet: "deploy"},
			{SessionID: "private", Title: "Private", UpdatedAt: time.Unix(3, 0), Snippet: "deploy"},
		},
		logs: map[string][]session.Event{"parent": parent.Events(), "child": nil},
		metas: []store.SessionMeta{
			{ID: "parent", CreatedAt: time.Unix(1, 0), WorkspaceID: "w1"},
			{ID: "child", CreatedAt: time.Unix(2, 0), WorkspaceID: "w1"},
			{ID: "private", CreatedAt: time.Unix(3, 0), WorkspaceID: "w2"},
		},
	}
	ts := NewTools(backend, func() string { return "parent" })
	out, err := ts.Search().Execute(context.Background(), json.RawMessage(`{"query":"deploy"}`))
	if err != nil || !strings.Contains(out, "child") || strings.Contains(out, "private") {
		t.Fatalf("workspace-scoped search = %q, err=%v", out, err)
	}
	out, err = ts.Trace().Execute(context.Background(), json.RawMessage(`{"session_id":"parent"}`))
	if err != nil || !strings.Contains(out, "Descendants:") || !strings.Contains(out, "child") {
		t.Fatalf("parent lineage = %q, err=%v", out, err)
	}
	out, err = ts.Trace().Execute(context.Background(), json.RawMessage(`{"session_id":"child"}`))
	if err != nil || !strings.Contains(out, "Ancestors") || !strings.Contains(out, "parent") {
		t.Fatalf("child lineage = %q, err=%v", out, err)
	}
	if _, err := ts.Read().Execute(context.Background(), json.RawMessage(`{"session_id":"private","seq":1}`)); err == nil || !strings.Contains(err.Error(), "outside the caller workspace") {
		t.Fatalf("private session read err = %v", err)
	}
}

func TestNormalizeQueryRejectsBlankAndNUL(t *testing.T) {
	for _, query := range []string{" ", "a\x00b"} {
		if _, err := normalizeQuery(query); err == nil {
			t.Fatalf("normalizeQuery(%q) accepted invalid query", query)
		}
	}
}

func TestCWDAndToolRelations(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessage("", []llm.ToolCall{{ID: "call-1", Name: "lookup"}}, "tool-calls"))
	_, _ = log.Append(session.EventToolStart, session.NewToolStart("call-1", "lookup", "{}"))
	_, _ = log.Append(session.EventToolResult, session.NewToolResult("call-1", "lookup", "done", nil))
	backend := fakeBackend{
		logs:  map[string][]session.Event{"current": log.Events()},
		metas: []store.SessionMeta{{ID: "current", WorkspaceID: "w", CWD: "D:/project"}},
	}
	ts := NewTools(backend, func() string { return "current" })
	out, err := ts.EventTrace().Execute(context.Background(), json.RawMessage(`{"seq":2}`))
	if err != nil || !strings.Contains(out, "Events cited directly as sources: 1") || !strings.Contains(out, "Direct derived events: 3") {
		t.Fatalf("tool relation trace = %q, err=%v", out, err)
	}
	backend.metas = append(backend.metas, store.SessionMeta{ID: "other-cwd", WorkspaceID: "w", CWD: "D:/other"})
	if _, err := NewTools(backend, func() string { return "current" }).Read().Execute(context.Background(), json.RawMessage(`{"session_id":"other-cwd","seq":1}`)); err == nil || !strings.Contains(err.Error(), "outside the caller workspace") {
		t.Fatalf("cwd isolation err = %v", err)
	}
}
