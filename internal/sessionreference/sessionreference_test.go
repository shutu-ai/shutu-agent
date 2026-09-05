package sessionreference

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

func TestParseAndFormatCanonicalSessionMention(t *testing.T) {
	mention, err := FormatMention(`weird"]id`, "Release notes")
	if err != nil {
		t.Fatal(err)
	}
	text, references, err := ParseText("review " + mention + " now")
	if err != nil {
		t.Fatal(err)
	}
	if text != "review @Release notes now" || len(references) != 1 ||
		references[0].SessionID != `weird"]id` || references[0].Label != "Release notes" {
		t.Fatalf("parse result = %q %#v", text, references)
	}
}

func TestParseRejectsMalformedCanonicalMention(t *testing.T) {
	if _, _, err := ParseText("@[Bad](shutu-session:not-base64)"); err == nil {
		t.Fatal("malformed mention unexpectedly passed")
	}
}

func seedReference(t *testing.T, backend store.Store, id, title, text string) {
	t.Helper()
	if err := backend.CreateSession(context.Background(), id, time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		if err := backend.SetSessionTitle(context.Background(), id, title, session.TitleSourceUser); err != nil {
			t.Fatal(err)
		}
	}
	if err := backend.AppendEvents(context.Background(), id, []session.Event{
		{Seq: 1, Type: session.EventUserMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: json.RawMessage(`{"source":{"kind":"user"},"content":[{"type":"text","text":"source question"}]}`)},
		{Seq: 2, Type: session.EventAssistantMessage, At: time.UnixMilli(1002), Version: session.EventVersion, Data: json.RawMessage(`{"message":{"content":[{"type":"text","text":"` + text + `"}]}}`)},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(1003), Version: session.EventVersion, Data: json.RawMessage(`{"source":{"kind":"plugin","plugin":"session-reference"},"content":[{"type":"text","text":"must not propagate"}]}`)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareTextBuildsRecallContext(t *testing.T) {
	backend, err := store.OpenSQLite(t.TempDir() + "/sessions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	seedReference(t, backend, "target", "Target", "")
	seedReference(t, backend, "source", "Source", "source answer")
	mention, err := FormatMention("source", "Source")
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareText(context.Background(), backend, "target", "compare with "+mention)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Text != "compare with @Source" || prepared.Context == nil ||
		prepared.Context.SourceKind != "session-reference" || prepared.Context.SourceForm != "recall" {
		t.Fatalf("prepared = %+v", prepared)
	}
	body := prepared.Context.Text()
	if !strings.Contains(body, `"label":"Source"`) || !strings.Contains(body, "source answer") ||
		!strings.Contains(body, `"role":"assistant"`) || strings.Contains(body, "must not propagate") {
		t.Fatalf("recall body = %q", body)
	}
}

func TestPrepareRejectsSelfAndTooManyReferences(t *testing.T) {
	backend, err := store.OpenSQLite(t.TempDir() + "/sessions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	mentions := make([]string, 0, MaxReferences+1)
	for i := 0; i <= MaxReferences; i++ {
		id := "source-" + string(rune('a'+i))
		seedReference(t, backend, id, "", "")
		mention, err := FormatMention(id, id)
		if err != nil {
			t.Fatal(err)
		}
		mentions = append(mentions, mention)
	}
	if _, err := PrepareText(context.Background(), backend, "target", strings.Join(mentions, " ")); err == nil {
		t.Fatal("too many references unexpectedly passed")
	}
	selfMention, err := FormatMention("target", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareText(context.Background(), backend, "target", selfMention); err == nil {
		t.Fatal("self reference unexpectedly passed")
	}
}

func TestSmallBudgetTruncatesAndRecordsFacts(t *testing.T) {
	backend, err := store.OpenSQLite(t.TempDir() + "/sessions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.CreateSession(context.Background(), "target", time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 5000)
	raw, err := json.Marshal(map[string]any{
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": long}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.AppendEvents(context.Background(), "source", []session.Event{
		{Seq: 1, Type: session.EventAssistantMessage, At: time.UnixMilli(1001), Version: session.EventVersion, Data: raw},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := backend.LoadSession(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	data, stats, err := retainReferencedSession(events, snapshot.Surface, store.SessionMeta{ID: "source"}, "Source", 512)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Truncated || stats.OmittedBytes <= 0 || len(data.Conversation) != 1 ||
		!strings.Contains(data.Conversation[0].Text, "omitted") {
		t.Fatalf("truncated data=%+v stats=%+v", data, stats)
	}
}

func TestContentBlockTextHelper(t *testing.T) {
	if got := textOf([]llm.ContentBlock{llm.Text("a"), llm.Text("b")}); got != "a\nb" {
		t.Fatalf("textOf = %q", got)
	}
}
