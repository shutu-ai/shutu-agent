// Black-box tests for the M4b composition-root orchestration (dispatch-m4b
// §2/§3/§4): the per-turn recall injects bounded snippets and logs the kb/recall
// event before the model sees them, fail-open never blocks, the kb_add tool
// logs the kb/add event through its onAdded wiring, and the /kb-status /
// /kb-reindex CLI commands behave under enabled and disabled states.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/kb"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/spill"
	"github.com/jabing/shutu-agent/internal/tools"
)

// failingKB is a KB whose Recall always fails, for the fail-open test.
type failingKB struct{ kb.KB }

func (failingKB) Recall(ctx context.Context, query string, limit int) ([]kb.Hit, error) {
	return nil, errors.New("recall exploded")
}

// TestRecallContextInjectsAndLogs verifies a successful recall returns one
// context message carrying the bounded snippet, and the kb/recall event is
// appended to the log before the message is handed out (D3: 模型可见 ⇒ 已落日志).
func TestRecallContextInjectsAndLogs(t *testing.T) {
	k := kb.NewMemProvider()
	if _, err := k.Add(context.Background(), kb.Entry{
		Title: "架构决策记录", Body: "我们决定采用 SQLite FTS5 作为知识库检索方案", Type: kb.TypeDecision,
		Tags: []string{"架构"}, Source: "session:s1:turn:1", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log := session.New()
	msgs, err := recallContext(context.Background(), k, log, "架构决策", 3)
	if err != nil {
		t.Fatalf("recallContext: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user context message", msgs)
	}
	if !strings.Contains(msgs[0].Text(), "架构决策记录") || !strings.Contains(msgs[0].Text(), "id: ") {
		t.Fatalf("recall content = %q, want title + id", msgs[0].Text())
	}
	// The kb/recall event must already be in the log.
	var recallEvents int
	for _, ev := range log.Events() {
		if ev.Type == session.EventKBRecall {
			recallEvents++
			if !strings.Contains(string(ev.Data), "架构决策记录") {
				t.Fatalf("kb/recall payload lacks the entry: %s", ev.Data)
			}
		}
	}
	if recallEvents != 1 {
		t.Fatalf("kb/recall events = %d, want 1", recallEvents)
	}
}

// TestRecallContextFailOpen verifies retrieval failure never blocks: an error
// from Recall returns (nil, err) and appends nothing, a nil provider and a
// zero limit are silent no-ops, and an empty result injects nothing.
func TestRecallContextFailOpen(t *testing.T) {
	log := session.New()
	if msgs, err := recallContext(context.Background(), failingKB{}, log, "anything", 3); err == nil || msgs != nil {
		t.Fatalf("failing recall: msgs=%v err=%v, want nil msgs + error", msgs, err)
	}
	if msgs, err := recallContext(context.Background(), nil, log, "anything", 3); err != nil || msgs != nil {
		t.Fatalf("nil provider: msgs=%v err=%v, want nil/nil", msgs, err)
	}
	if msgs, err := recallContext(context.Background(), kb.NewMemProvider(), log, "anything", 0); err != nil || msgs != nil {
		t.Fatalf("limit 0 (off): msgs=%v err=%v, want nil/nil", msgs, err)
	}
	if msgs, err := recallContext(context.Background(), kb.NewMemProvider(), log, "nothing here", 3); err != nil || msgs != nil {
		t.Fatalf("no hits: msgs=%v err=%v, want nil/nil", msgs, err)
	}
	for _, ev := range log.Events() {
		if ev.Type == session.EventKBRecall {
			t.Fatalf("kb/recall must not be appended on fail-open paths: %s", ev.Data)
		}
	}
}

func TestSpillRecallContextInjectsAndLogs(t *testing.T) {
	mem := spill.NewMemProvider()
	engine := spill.NewEngine(mem)
	defer engine.Close()
	if _, err := engine.Spill(context.Background(), "The user prefers durable Go memory", "session:7"); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	msgs, err := spillRecallContext(context.Background(), engine, log, "durable Go")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text(), "durable Go memory") {
		t.Fatalf("memory context = %+v", msgs)
	}
	if countEvents(log, session.EventSpillRecall) != 1 {
		t.Fatalf("spill/recall events = %d, want 1", countEvents(log, session.EventSpillRecall))
	}
}

// TestKBAddToolLogsKBAddEvent proves the onAdded wiring: executing kb_add
// through the registry appends a kb/add event carrying the entry summary, and
// the written entry is immediately retrievable via kb_search (dispatch-m4b §3).
func TestKBAddToolLogsKBAddEvent(t *testing.T) {
	k := kb.NewMemProvider()
	log := session.New()
	reg := tools.New()
	if err := reg.Register(kb.NewAddTool(k, func(e kb.Entry) {
		if _, err := log.Append(session.EventKBAdd, session.NewKBAdd(e.ID, e.Title, e.Type, e.Tags, e.Source, e.Version)); err != nil {
			t.Fatalf("append kb/add: %v", err)
		}
	})); err != nil {
		t.Fatalf("register kb_add: %v", err)
	}
	if err := reg.Register(kb.NewSearchTool(k, 5)); err != nil {
		t.Fatalf("register kb_search: %v", err)
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{"kb_add", "kb_search"}, Timeout: time.Hour})

	res, err := reg.Execute(context.Background(), "kb_add", json.RawMessage(`{"title":"记住的约定","body":"unique-term-qwerty 每周五同步进度","type":"fact"}`))
	if err != nil {
		t.Fatalf("kb_add: %v", err)
	}
	if !strings.Contains(res.Output, "added knowledge entry") {
		t.Fatalf("kb_add output = %q", res.Output)
	}

	var addEvents int
	for _, ev := range log.Events() {
		if ev.Type != session.EventKBAdd {
			continue
		}
		addEvents++
		var d struct {
			EntryID string   `json:"entryId"`
			Title   string   `json:"title"`
			Type    string   `json:"type"`
			Tags    []string `json:"tags"`
			Source  string   `json:"source"`
			Version int      `json:"version"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal kb/add: %v", err)
		}
		if d.Title != "记住的约定" || d.Type != "fact" || d.Version != 1 || d.EntryID == "" || !strings.HasPrefix(d.Source, "manual:") {
			t.Fatalf("kb/add payload = %+v", d)
		}
	}
	if addEvents != 1 {
		t.Fatalf("kb/add events = %d, want 1", addEvents)
	}

	search, err := reg.Execute(context.Background(), "kb_search", json.RawMessage(`{"query":"unique-term-qwerty"}`))
	if err != nil {
		t.Fatalf("kb_search after kb_add: %v", err)
	}
	if !strings.Contains(search.Output, "记住的约定") {
		t.Fatalf("kb_add'd entry not retrievable: %q", search.Output)
	}
}

// TestKBStatusAndReindexDisabled verifies both CLI commands behave when kb is
// disabled: /kb-status reports disabled, /kb-reindex errors.
func TestKBStatusAndReindexDisabled(t *testing.T) {
	a := &app{}
	if err := a.kbStatus(context.Background()); err != nil {
		t.Fatalf("kbStatus disabled: %v", err)
	}
	if err := a.kbReindex(context.Background()); err == nil {
		t.Fatal("kbReindex must fail when kb is disabled")
	}
}

// TestKBStatusAndReindexOnSQLite exercises the CLI against a real SQLite
// provider: /kb-status reports the entry count, /kb-reindex rebuilds without
// error and search still finds the entries afterwards.
func TestKBStatusAndReindexOnSQLite(t *testing.T) {
	dir := t.TempDir()
	k, err := kb.NewFromConfig(kbConfigFor(t, dir, true))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	defer k.Close()
	if _, err := k.Add(context.Background(), kb.Entry{
		Title: "架构决策记录", Body: "使用 FTS5 检索", Type: kb.TypeDecision, Source: "manual:1", Confidence: 0.8,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	a := &app{kb: k}
	if err := a.kbStatus(context.Background()); err != nil {
		t.Fatalf("kbStatus: %v", err)
	}
	if err := a.kbReindex(context.Background()); err != nil {
		t.Fatalf("kbReindex: %v", err)
	}
	hits, err := k.Search(context.Background(), "FTS5", kb.SearchOpts{TopK: 5})
	if err != nil {
		t.Fatalf("search after reindex: %v", err)
	}
	if len(hits) != 1 || hits[0].Entry.Title != "架构决策记录" {
		t.Fatalf("after reindex search = %+v, want the seeded entry", hits)
	}
}

// kbConfigFor builds a KBConfig pointing at a temp sqlite file.
func kbConfigFor(t *testing.T, dir string, enabled bool) config.KBConfig {
	t.Helper()
	return config.KBConfig{Enabled: config.Bool(enabled), DBPath: filepath.Join(dir, "kb.sqlite")}
}
