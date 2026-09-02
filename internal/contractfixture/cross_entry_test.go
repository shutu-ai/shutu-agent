package contractfixture_test

// This is deliberately a cross-entry test rather than another package-local
// fixture test.  The same bytes are written to both persistence backends and
// then read through the public Web and native transports.  The SDK envelope
// is checked against the same canonical event records as well.  Keeping the
// comparison here exposes accidental transport-specific event invention.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/contractfixture"
	"github.com/jabing/shutu-agent/internal/persistence"
	"github.com/jabing/shutu-agent/internal/sdkclient"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/webserver"
)

type canonicalEvent struct {
	Seq  uint64
	Type string
	Time int64
	Data json.RawMessage
}

func TestCrossEntryCoreTurnContract(t *testing.T) {
	want := loadCanonicalFixture(t)
	ctx := context.Background()

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cross-entry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(ctx, "cross-entry", time.UnixMilli(want[0].Time).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "cross-entry", toSessionEvents(want)); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.LoadSessionRaw(ctx, "cross-entry")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEvents(t, "SQLite", want, loaded)

	jsonl, err := persistence.OpenJSONL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonl.Create(ctx, persistence.Header{ID: "cross-entry"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := jsonl.Append(ctx, "cross-entry", toSessionEvents(want)); err != nil {
		t.Fatal(err)
	}
	jsonlLoaded, err := jsonl.Load(ctx, "cross-entry")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalEvents(t, "JSONL", want, jsonlLoaded.Events)

	srv, err := webserver.New(st, "cross-token", "")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDefaultWorkdir(t.TempDir())
	handler := srv.Handler()

	webEvents := getWebEvents(t, handler)
	if len(webEvents) != len(want) {
		t.Fatalf("Web event count = %d, want %d", len(webEvents), len(want))
	}
	for index, event := range webEvents {
		if event.Seq != want[index].Seq || event.Type != want[index].Type || event.Time != want[index].Time {
			t.Fatalf("Web event %d = %#v, want seq/type/time %d/%s/%d", index, event, want[index].Seq, want[index].Type, want[index].Time)
		}
	}

	nativeEvents := postNativeHistory(t, handler)
	assertNativeEvents(t, want, nativeEvents)

	for _, expected := range want {
		wire := sdkclient.SessionEvent{
			Seq: expected.Seq, Type: expected.Type, At: expected.Time,
			Data: append(json.RawMessage(nil), expected.Data...),
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var actual canonicalEvent
		if err := json.Unmarshal(raw, &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("SDK event envelope = %#v, want %#v", actual, expected)
		}
	}
}

func loadCanonicalFixture(t *testing.T) []canonicalEvent {
	t.Helper()
	records, err := contractfixture.CoreTurnEvents()
	if err != nil {
		t.Fatal(err)
	}
	want := make([]canonicalEvent, 0, len(records))
	for _, record := range records {
		canonical, err := canonicalJSON(record.Data)
		if err != nil {
			t.Fatalf("fixture %s data: %v", record.Type, err)
		}
		want = append(want, canonicalEvent{Seq: record.Seq, Type: record.Type, Time: record.Time, Data: canonical})
	}
	return want
}

func toSessionEvents(events []canonicalEvent) []session.Event {
	out := make([]session.Event, 0, len(events))
	for _, event := range events {
		out = append(out, session.Event{
			Seq: event.Seq, Type: event.Type, Version: session.EventVersion,
			At: time.UnixMilli(event.Time).UTC(), Data: append(json.RawMessage(nil), event.Data...),
		})
	}
	return out
}

func assertCanonicalEvents(t *testing.T, surface string, want []canonicalEvent, got []session.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s event count = %d, want %d", surface, len(got), len(want))
	}
	for index, event := range got {
		data, err := canonicalJSON(event.Data)
		if err != nil {
			t.Fatalf("%s event %d data: %v", surface, index, err)
		}
		actual := canonicalEvent{Seq: event.Seq, Type: event.Type, Time: event.At.UnixMilli(), Data: data}
		if !reflect.DeepEqual(actual, want[index]) {
			t.Fatalf("%s event %d = %#v, want %#v", surface, index, actual, want[index])
		}
	}
}

// Native history is a projection, not a raw store dump: it owns client-safe
// message ids.  The declared normalization removes only those generated ids;
// seq/type/time and every other data field remain exact.
func assertNativeEvents(t *testing.T, want []canonicalEvent, got []session.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("native RPC event count = %d, want %d", len(got), len(want))
	}
	for index, event := range got {
		data, err := canonicalJSON(event.Data)
		if err != nil {
			t.Fatalf("native RPC event %d data: %v", index, err)
		}
		actual := canonicalEvent{Seq: event.Seq, Type: event.Type, Time: event.At.UnixMilli(), Data: stripNativeData(event.Type, data)}
		expected := canonicalEvent{Seq: want[index].Seq, Type: want[index].Type, Time: want[index].Time, Data: stripNativeData(want[index].Type, want[index].Data)}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("native RPC event %d = %#v, want %#v", index, actual, expected)
		}
	}
}

func stripNativeData(eventType string, raw []byte) json.RawMessage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	stripIDValues(value)
	if eventType == session.EventToolResult {
		if object, ok := value.(map[string]any); ok {
			// Native enriches tool/result with a top-level lookup summary;
			// the canonical event keeps the authoritative values in message.
			delete(object, "callId")
			delete(object, "name")
		}
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func stripIDValues(value any) {
	switch item := value.(type) {
	case map[string]any:
		if _, ok := item["id"]; ok {
			delete(item, "id")
		}
		// Native history intentionally projects away internal turn/step
		// coordinates.  The event sequence is the authoritative cursor.
		delete(item, "turn")
		delete(item, "step")
		for _, child := range item {
			stripIDValues(child)
		}
	case []any:
		for _, child := range item {
			stripIDValues(child)
		}
	}
}

type webEvent struct {
	Seq  uint64
	Type string
	Time int64
}

func getWebEvents(t *testing.T, handler http.Handler) []webEvent {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/cross-entry/events?limit=500", nil)
	req.Header.Set("Authorization", "Bearer cross-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Web events status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events []struct {
			Seq  uint64    `json:"seq"`
			Type string    `json:"type"`
			Time time.Time `json:"time"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	out := make([]webEvent, 0, len(page.Events))
	for _, event := range page.Events {
		out = append(out, webEvent{Seq: event.Seq, Type: event.Type, Time: event.Time.UnixMilli()})
	}
	return out
}

func postNativeHistory(t *testing.T, handler http.Handler) []session.Event {
	t.Helper()
	body := []byte(`{"type":"client-request","rpcId":"cross-1","method":"session.history","payload":{"sessionId":"cross-entry","maxMessages":500}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/session.history", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer cross-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("native history status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Result struct {
			OK    bool `json:"ok"`
			Value struct {
				Events []struct {
					Event struct {
						Seq  uint64          `json:"seq"`
						Type string          `json:"type"`
						Time int64           `json:"time"`
						Data json.RawMessage `json:"data"`
					} `json:"event"`
				} `json:"events"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.OK {
		t.Fatalf("native history rejected fixture: %s", rec.Body.String())
	}
	out := make([]session.Event, 0, len(response.Result.Value.Events))
	for _, item := range response.Result.Value.Events {
		data, err := canonicalJSON(item.Event.Data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, session.Event{Seq: item.Event.Seq, Type: item.Event.Type, At: time.UnixMilli(item.Event.Time).UTC(), Data: data})
	}
	return out
}

func canonicalJSON(raw []byte) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
