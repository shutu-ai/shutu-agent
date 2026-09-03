package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

// scriptedLLM returns a fixed single text block (or none) then stops; used to
// exercise the title call without a real provider.
type scriptedLLM struct {
	text string
}

func (l *scriptedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	events := make([]llm.StreamEvent, 0, 2)
	if l.text != "" {
		events = append(events, llm.StreamEvent{Kind: llm.StreamTextDelta, Text: l.text})
	}
	events = append(events, llm.StreamEvent{Kind: llm.StreamFinish, FinishReason: "stop"})
	return &scriptedReader{events: events}, nil
}

type scriptedReader struct {
	events []llm.StreamEvent
	i      int
}

func (r *scriptedReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

func seedUserMessage(t *testing.T, st store.Store, id, text string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, id, []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.Now().UTC(), Version: 1, Data: mustTitleData(t, map[string]any{"Text": text})},
	}); err != nil {
		t.Fatal(err)
	}
}

func mustTitleData(t *testing.T, v map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestEnsureSessionTitleMaterializesFallback asserts the deterministic fallback
// is stored (source "fallback") the first time a session has an eligible user
// message and no accepted title yet.
func TestEnsureSessionTitleMaterializesFallback(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedUserMessage(t, st, "s-a", "帮我写一首关于春天的诗")

	a := makeTurnApp()
	a.store = st
	a.ensureSessionTitle(context.Background(), "s-a")

	meta, err := st.GetSessionMeta(context.Background(), "s-a")
	if err != nil {
		t.Fatal(err)
	}
	// "帮我写一首关于春天的诗" has no spaces, so the word cap yields the whole
	// sentence within the 40-byte fallback budget.
	if meta.Title != "帮我写一首关于春天的诗" {
		t.Fatalf("fallback title = %q, want the sentence", meta.Title)
	}
	if meta.TitleSource != session.TitleSourceFallback {
		t.Fatalf("title source = %q, want fallback", meta.TitleSource)
	}
}

// TestEnsureSessionTitleRespectsUserPin asserts a user-renamed session is never
// auto-revised by the title service.
func TestEnsureSessionTitleRespectsUserPin(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedUserMessage(t, st, "s-a", "帮我写一首诗")
	if err := st.SetSessionTitle(context.Background(), "s-a", "我的名字", session.TitleSourceUser); err != nil {
		t.Fatal(err)
	}

	a := makeTurnApp()
	a.store = st
	a.ensureSessionTitle(context.Background(), "s-a")

	meta, err := st.GetSessionMeta(context.Background(), "s-a")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "我的名字" || meta.TitleSource != session.TitleSourceUser {
		t.Fatalf("pinned title changed: title=%q source=%q", meta.Title, meta.TitleSource)
	}
}

// TestLlmTitleNormalizesModelOutput asserts the model title call normalizes the
// provider output to a one-line, byte-bounded title.
func TestLlmTitleNormalizesModelOutput(t *testing.T) {
	a := &app{cfg: config.Config{Model: "m"}, llm: &scriptedLLM{text: "  短标题\n第二行  \n"}}
	title, err := a.llmTitle(context.Background(), "帮我写一首诗")
	if err != nil {
		t.Fatal(err)
	}
	if title != "短标题 第二行" {
		t.Fatalf("llmTitle = %q, want 短标题 第二行", title)
	}
}

// TestLlmTitleNoModelOutputIsError asserts an empty model output yields an
// error so the caller keeps the fallback.
func TestLlmTitleNoModelOutputIsError(t *testing.T) {
	a := &app{cfg: config.Config{Model: "m"}, llm: &scriptedLLM{text: "  \u001b[0m"}}
	if _, err := a.llmTitle(context.Background(), "hi"); err == nil {
		t.Fatal("llmTitle(empty output) = nil error, want error")
	}
}

func TestNativeRenameSessionAppendsCanonicalTitleEvent(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "rename.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "rename-app", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	a := makeTurnApp()
	a.store = st
	seq, err := a.nativeRenameSession(ctx, "rename-app", "A canonical title")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("rename seq = %d, want 1", seq)
	}
	events, err := st.LoadSession(ctx, "rename-app")
	if err != nil || len(events) != 1 || events[0].Type != session.EventSessionTitle {
		t.Fatalf("rename events = %+v, err=%v", events, err)
	}
	meta, err := st.GetSessionMeta(ctx, "rename-app")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "A canonical title" || meta.TitleSource != session.TitleSourceUser {
		t.Fatalf("rename metadata = %+v", meta)
	}
}
