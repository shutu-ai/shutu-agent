package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

func openSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// buildLog appends a representative mini-conversation through a session.Log
// whose sink forwards to the store, returning the log for later comparison.
func buildLog(t *testing.T, st Store, id string, wantDerived int) *session.Log {
	t.Helper()
	log := session.New()
	log.SetSink(func(ev session.Event) error {
		return st.AppendEvents(context.Background(), id, []session.Event{ev})
	})
	must := func(typ string, data any) {
		if _, err := log.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	must(session.EventUserMessage, session.NewUserMessage("what time is it"))
	must(session.EventAssistantChunk, session.NewAssistantChunk("Let "))
	must(session.EventAssistantChunk, session.NewAssistantChunk("me check"))
	must(session.EventAssistantMessage, session.NewAssistantMessage("Let me check", nil, "stop"))
	if wantDerived > 0 && len(log.DeriveHistory()) != wantDerived {
		t.Fatalf("derived %d messages, want %d", len(log.DeriveHistory()), wantDerived)
	}
	return log
}

func assertEventsEqual(t *testing.T, want, got []session.Event) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Seq != g.Seq {
			t.Errorf("event %d: seq = %d, want %d", i, g.Seq, w.Seq)
		}
		if w.Type != g.Type {
			t.Errorf("event %d: type = %q, want %q", i, g.Type, w.Type)
		}
		if w.Version != g.Version {
			t.Errorf("event %d: version = %d, want %d", i, g.Version, w.Version)
		}
		if w.At.UnixNano() != g.At.UnixNano() {
			t.Errorf("event %d: at = %v, want %v", i, g.At, w.At)
		}
		if !bytes.Equal(w.Data, g.Data) {
			t.Errorf("event %d: data = %s, want %s", i, g.Data, w.Data)
		}
	}
}

// TestReplayEventsConsistent persists events, closes the store, reopens it,
// and verifies the reloaded events match one-by-one and derive the same
// history (dispatch-m2: "事件逐条一致、派生历史一致").
func TestReplayEventsConsistent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pa.db")
	ctx := context.Background()

	st1, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const id = "s-replay"
	if err := st1.CreateSession(ctx, id, time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	want := buildLog(t, st1, id, 2) // user + assistant = 2 derived messages
	st1.Close()

	st2, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	events, err := st2.LoadSession(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assertEventsEqual(t, want.Events(), events)

	// Derived history must be identical after replay.
	replayed := session.New()
	if err := replayed.Restore(events); err != nil {
		t.Fatalf("restore: %v", err)
	}
	h1, h2 := want.DeriveHistory(), replayed.DeriveHistory()
	if len(h1) != len(h2) {
		t.Fatalf("derived history len = %d, want %d", len(h2), len(h1))
	}
	for i := range h1 {
		if h1[i].Role != h2[i].Role || h1[i].Text() != h2[i].Text() {
			t.Errorf("history %d: got %+v, want %+v", i, h2[i], h1[i])
		}
	}
}

// TestMultiSessionRestore verifies two sessions coexist: each loads only its
// own events and /list reports both (dispatch-m2: "多会话恢复").
func TestMultiSessionRestore(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()

	a := session.New()
	a.SetSink(func(ev session.Event) error { return st.AppendEvents(ctx, "s-a", []session.Event{ev}) })
	if _, err := a.Append(session.EventUserMessage, session.NewUserMessage("hello A")); err != nil {
		t.Fatalf("append A: %v", err)
	}

	b := session.New()
	b.SetSink(func(ev session.Event) error { return st.AppendEvents(ctx, "s-b", []session.Event{ev}) })
	if _, err := b.Append(session.EventUserMessage, session.NewUserMessage("hello B")); err != nil {
		t.Fatalf("append B: %v", err)
	}

	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("listed %d sessions, want 2: %+v", len(metas), metas)
	}

	evA, err := st.LoadSession(ctx, "s-a")
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	evB, err := st.LoadSession(ctx, "s-b")
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if len(evA) != 1 || len(evB) != 1 {
		t.Fatalf("event counts A=%d B=%d, want 1 each", len(evA), len(evB))
	}
	if !bytes.Contains(evA[0].Data, []byte("hello A")) {
		t.Errorf("session A data = %s", evA[0].Data)
	}
	if !bytes.Contains(evB[0].Data, []byte("hello B")) {
		t.Errorf("session B data = %s", evB[0].Data)
	}

	// Restoring each into a fresh log yields the correct history.
	la := session.New()
	if err := la.Restore(evA); err != nil {
		t.Fatalf("restore A: %v", err)
	}
	h := la.DeriveHistory()
	if len(h) != 1 || h[0].Role != llm.RoleUser || h[0].Text() != "hello A" {
		t.Errorf("session A history = %+v", h)
	}
}

// TestLoadNotFound verifies ErrNotFound for an unknown session id.
func TestLoadNotFound(t *testing.T) {
	st := openSQLite(t)
	if _, err := st.LoadSession(context.Background(), "s-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadSession = %v, want ErrNotFound", err)
	}
}

// TestKBRecallEventPersistsAndReplays verifies the M4a kb/recall event type
// travels the durable append path end to end: session.Log sink → SQLiteStore →
// replay (design.md §3 / D8, D3 机制在 M4a 就位).
func TestKBRecallEventPersistsAndReplays(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	const id = "s-kb"
	log := session.New()
	log.SetSink(func(ev session.Event) error {
		return st.AppendEvents(ctx, id, []session.Event{ev})
	})
	if _, err := log.Append(session.EventKBRecall, session.NewKBRecall("架构", []session.RecallHit{
		{ID: "kb-1", Title: "架构决策记录", Snippet: "我们决定采用 SQLite FTS5…", Type: "decision", Score: 0.9},
	})); err != nil {
		t.Fatalf("append: %v", err)
	}
	events, err := st.LoadSession(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(events) != 1 || events[0].Type != session.EventKBRecall {
		t.Fatalf("replayed = %+v, want one %q event", events, session.EventKBRecall)
	}
	if !bytes.Contains(events[0].Data, []byte("架构决策记录")) {
		t.Errorf("payload lost in round trip: %s", events[0].Data)
	}
}

// TestAppendMaterializesSession verifies appending to a never-created session
// materializes its row (defensive) and it then appears in /list.
func TestAppendMaterializesSession(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	ev := session.Event{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"hi"}`)}
	if err := st.AppendEvents(ctx, "s-auto", []session.Event{ev}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := st.LoadSession(ctx, "s-auto")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
}

// TestWorkspaceLifecycle covers the P6 workspace store: create (idempotent,
// appended sort), list order, rename, session membership + ungroup, and delete
// returning sessions to the ungrouped bucket.
func TestWorkspaceLifecycle(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()

	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil { // idempotent
		t.Fatalf("re-create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w2", "日常"); err != nil {
		t.Fatalf("create w2: %v", err)
	}
	ws, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ws) != 2 || ws[0].ID != "w1" || ws[1].ID != "w2" {
		t.Fatalf("workspaces = %+v, want w1,w2 in creation order", ws)
	}
	if ws[1].Sort <= ws[0].Sort {
		t.Fatalf("sorts not ascending: %d then %d", ws[0].Sort, ws[1].Sort)
	}

	if err := st.SetWorkspaceTitle(ctx, "w1", "研究·改"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := st.SetWorkspaceTitle(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename unknown: %v, want ErrNotFound", err)
	}

	// Sessions join a workspace and read back through ListSessions.
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := st.SetSessionWorkspace(ctx, "s1", "w1"); err != nil {
		t.Fatalf("set s1->w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s2", "w1"); err != nil {
		t.Fatalf("set s2->w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s2", ""); err != nil { // back to ungrouped
		t.Fatalf("ungroup s2: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "nope", "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set unknown session: %v, want ErrNotFound", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s1"].WorkspaceID != "w1" || byID["s2"].WorkspaceID != "" || byID["s3"].WorkspaceID != "" {
		t.Fatalf("workspace ids = %q/%q/%q, want w1/''/''", byID["s1"].WorkspaceID, byID["s2"].WorkspaceID, byID["s3"].WorkspaceID)
	}

	// Deleting a workspace returns its sessions to the ungrouped bucket.
	if err := st.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatalf("delete w1: %v", err)
	}
	if err := st.DeleteWorkspace(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v, want ErrNotFound", err)
	}
	metas, err = st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions after delete: %v", err)
	}
	for _, m := range metas {
		if m.WorkspaceID != "" {
			t.Fatalf("session %s still in workspace %q after delete", m.ID, m.WorkspaceID)
		}
	}
}

// TestArchiveAndOrder covers the P6.2 additions: archive/unarchive toggling,
// manual session order (with cross-workspace move) and manual workspace order.
func TestArchiveAndOrder(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w2", "日常"); err != nil {
		t.Fatalf("create w2: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// Archive hides a session from the active set (ArchivedAt set, cleared on
	// unarchive).
	if err := st.ArchiveSession(ctx, "s2", true); err != nil {
		t.Fatalf("archive s2: %v", err)
	}
	if err := st.ArchiveSession(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive unknown: %v, want ErrNotFound", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range metas {
		if m.ID == "s2" && m.ArchivedAt.IsZero() {
			t.Fatal("s2 should be archived")
		}
		if m.ID != "s2" && !m.ArchivedAt.IsZero() {
			t.Fatalf("%s should not be archived", m.ID)
		}
	}
	if err := st.ArchiveSession(ctx, "s2", false); err != nil {
		t.Fatalf("unarchive s2: %v", err)
	}

	// Manual session order moves sessions across workspaces and assigns Sort.
	if err := st.ReorderSessions(ctx, "w1", []string{"s2", "s1", "s3"}); err != nil {
		t.Fatalf("reorder sessions: %v", err)
	}
	metas, _ = st.ListSessions(ctx)
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	for i, id := range []string{"s2", "s1", "s3"} {
		if byID[id].WorkspaceID != "w1" || byID[id].Sort != i {
			t.Fatalf("%s = ws %q sort %d, want w1 sort %d", id, byID[id].WorkspaceID, byID[id].Sort, i)
		}
	}
	// Moving back to ungrouped with a new order.
	if err := st.ReorderSessions(ctx, "", []string{"s3", "s2"}); err != nil {
		t.Fatalf("reorder to ungrouped: %v", err)
	}
	metas, _ = st.ListSessions(ctx)
	byID = map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s3"].WorkspaceID != "" || byID["s3"].Sort != 0 {
		t.Fatalf("s3 after move = ws %q sort %d, want ungrouped sort 0", byID["s3"].WorkspaceID, byID["s3"].Sort)
	}

	// Manual workspace order.
	if err := st.ReorderWorkspaces(ctx, []string{"w2", "w1"}); err != nil {
		t.Fatalf("reorder workspaces: %v", err)
	}
	ws, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("list ws: %v", err)
	}
	if ws[0].ID != "w2" || ws[1].ID != "w1" {
		t.Fatalf("workspace order = %s,%s, want w2,w1", ws[0].ID, ws[1].ID)
	}
}

// TestSearchAndFlatOrder covers P6.3: body-text search returns a snippet per
// matching session, and flat-order rewrites flat_sort without touching
// workspace membership.
func TestSearchAndFlatOrder(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	mk := func(id, text string) {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := st.AppendEvents(ctx, id, []session.Event{
			{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"` + text + `"}`)},
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	mk("s1", "部署 dsh 网关的配置说明")
	mk("s2", "今天天气很好")
	mk("s3", "再次部署网关")
	// Set a title for one hit to confirm it is carried.
	if err := st.SetSessionTitle(ctx, "s1", "网关部署手册", session.TitleSourceUser); err != nil {
		t.Fatalf("set title: %v", err)
	}

	hits, err := st.SearchSessions(ctx, "部署")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("search hits = %d, want 2 (s1, s3)", len(hits))
	}
	got := map[string]SearchHit{}
	for _, h := range hits {
		got[h.SessionID] = h
	}
	if got["s1"].Snippet == "" || got["s1"].Title != "网关部署手册" {
		t.Fatalf("s1 hit = %+v, want snippet + title", got["s1"])
	}
	if _, ok := got["s2"]; ok {
		t.Fatal("s2 should not match 部署")
	}
	// A literal % must not act as a wildcard.
	if hits, _ := st.SearchSessions(ctx, "%"); len(hits) != 0 {
		t.Fatalf("literal %% matched %d sessions, want 0", len(hits))
	}

	// Flat order: s1/s3 keep membership, take flat_sort.
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s3", "w1"); err != nil {
		t.Fatalf("set s3->w1: %v", err)
	}
	if err := st.ReorderSessionsFlat(ctx, []string{"s3", "s1"}); err != nil {
		t.Fatalf("flat order: %v", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s3"].FlatSort != 0 || byID["s1"].FlatSort != 1 {
		t.Fatalf("flat sorts = %d/%d, want 0/1", byID["s3"].FlatSort, byID["s1"].FlatSort)
	}
	if byID["s3"].WorkspaceID != "w1" {
		t.Fatalf("flat reorder changed membership: s3 ws = %q", byID["s3"].WorkspaceID)
	}
}

// TestMigrateLegacyTitleBecomesUserPin verifies that a pre-title-source row
// carrying a non-empty title (written only by the old sidebar rename) is pinned
// as user-sourced after migrating, so automatic title revision never overwrites
// a pre-existing rename.
func TestMigrateLegacyTitleBecomesUserPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pa.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, created_at INTEGER, updated_at INTEGER, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, title) VALUES ('s-old', 1, 1, '老标题')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	meta, err := st.GetSessionMeta(context.Background(), "s-old")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "老标题" || meta.TitleSource != session.TitleSourceUser {
		t.Fatalf("legacy pin: title=%q source=%q, want user", meta.Title, meta.TitleSource)
	}
}

// TestMarkSessionViewedAndLastViewedAt verifies the finished-but-unviewed
// tracking: LastViewedAt surfaces through ListSessions/GetSessionMeta and
// MarkSessionViewed clears it.
func TestMarkSessionViewedAndLastViewedAt(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "s-v", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Hour)

	// Unset initially.
	meta, err := st.GetSessionMeta(ctx, "s-v")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.LastViewedAt.IsZero() {
		t.Fatalf("new session LastViewedAt = %v, want zero", meta.LastViewedAt)
	}

	if err := st.MarkSessionViewed(ctx, "s-v", before); err != nil {
		t.Fatal(err)
	}
	meta, err = st.GetSessionMeta(ctx, "s-v")
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastViewedAt.IsZero() || !meta.LastViewedAt.Equal(before) {
		t.Fatalf("LastViewedAt = %v, want %v", meta.LastViewedAt, before)
	}

	// ListSessions surfaces it too.
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range metas {
		if m.ID == "s-v" && !m.LastViewedAt.IsZero() {
			found = true
		}
	}
	if !found {
		t.Fatal("ListSessions did not surface LastViewedAt for s-v")
	}

	// Unknown id → ErrNotFound.
	if err := st.MarkSessionViewed(ctx, "s-missing", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkSessionViewed(missing) err = %v, want ErrNotFound", err)
	}
}

// TestSessionConfig verifies the per-session override read/write surface
// (Phase 2 按会话切换; dsh ModelSelection 对齐): default zeros,
// SetSessionConfig stores the full override set (mode/provider/model/effort/
// permission), UpdateSessionConfig rewrites provider/model/effort/permission
// (mode stays locked), and a missing id returns ErrNotFound.
func TestSessionConfig(t *testing.T) {
	ctx := context.Background()
	st := openSQLite(t)
	if err := st.CreateSession(ctx, "s-cfg", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Default: all empty (fall back to the globals).
	got, err := st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got != (SessionConfig{}) {
		t.Fatalf("default session config = %+v, want zero", got)
	}

	// Set the full override set (dsh selection: provider+model+effort).
	if err := st.SetSessionConfig(ctx, "s-cfg", SessionConfig{AgentPreset: "minimal", Provider: "openai", Model: "gpt-4o", ReasoningEffort: "high", Permission: "readonly"}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPreset != "minimal" || got.Provider != "openai" || got.Model != "gpt-4o" || got.ReasoningEffort != "high" || got.Permission != "readonly" {
		t.Fatalf("session config = %+v, want minimal/openai/gpt-4o/high/readonly", got)
	}

	// UpdateSessionConfig rewrites provider/model/effort/permission; the mode
	// is untouched.
	if err := st.UpdateSessionConfig(ctx, "s-cfg", "anthropic", "claude-3-5", "max", "full"); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPreset != "minimal" || got.Provider != "anthropic" || got.Model != "claude-3-5" || got.ReasoningEffort != "max" || got.Permission != "full" {
		t.Fatalf("session config after update = %+v, want minimal/anthropic/claude-3-5/max/full", got)
	}

	// Clearing provider/model/effort/permission returns to global fallback,
	// mode still locked.
	if err := st.UpdateSessionConfig(ctx, "s-cfg", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "" || got.Model != "" || got.ReasoningEffort != "" || got.Permission != "" || got.AgentPreset != "minimal" {
		t.Fatalf("session config after clear = %+v, want ''/''/''/''/minimal", got)
	}

	// Empty mode clears the lock too (SetSessionConfig with empty set).
	if err := st.SetSessionConfig(ctx, "s-cfg", SessionConfig{}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got != (SessionConfig{}) {
		t.Fatalf("session config after full clear = %+v, want zero", got)
	}

	// Unknown id → ErrNotFound.
	if _, err := st.GetSessionConfig(ctx, "s-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSessionConfig(missing) err = %v, want ErrNotFound", err)
	}
	if err := st.UpdateSessionConfig(ctx, "s-missing", "m", "", "m", "full"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSessionConfig(missing) err = %v, want ErrNotFound", err)
	}
}

func TestMessageFeedbackCRUD(t *testing.T) {
	ctx := context.Background()
	st := openSQLite(t)
	if err := st.CreateSession(ctx, "s-feedback", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := st.GetMessageFeedback(ctx, "s-feedback", 2); err != nil || ok || got != (MessageFeedback{}) {
		t.Fatalf("initial feedback = %+v, %v, %v; want zero,false,nil", got, ok, err)
	}
	created, err := st.PutMessageFeedback(ctx, "s-feedback", 2, "positive", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID != "s-feedback" || created.Seq != 2 || created.Rating != "positive" {
		t.Fatalf("created feedback = %+v", created)
	}
	items, err := st.ListMessageFeedback(ctx, "s-feedback")
	if err != nil || len(items) != 1 || items[0].Rating != "positive" {
		t.Fatalf("listed feedback = %+v, err=%v", items, err)
	}
	updated, err := st.PutMessageFeedback(ctx, "s-feedback", 2, "negative", "changed")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Rating != "negative" || updated.Note != "changed" || updated.UpdatedAt.Before(updated.CreatedAt) {
		t.Fatalf("updated feedback = %+v", updated)
	}
	if err := st.DeleteMessageFeedback(ctx, "s-feedback", 2); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetMessageFeedback(ctx, "s-feedback", 2); err != nil || ok {
		t.Fatalf("feedback after delete = ok=%v err=%v, want false,nil", ok, err)
	}
	if _, err := st.ListMessageFeedback(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListMessageFeedback(missing) err=%v, want ErrNotFound", err)
	}
}
