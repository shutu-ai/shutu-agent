package timecontext_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/timecontext"
)

var base = time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

func event(seq uint64, kind string, at time.Time, data any) session.Event {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return session.Event{Seq: seq, Type: kind, At: at, Version: session.EventVersion, Data: raw}
}

func userMessage(text string, rpcID, timeZone string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(text)},
		SourceKind: "user", SourceRPCID: rpcID, SourceClientTimeZone: timeZone,
	}
}

func dataEvent(seq uint64, kind string, at time.Time, data any) session.Event {
	return event(seq, kind, at, data)
}

func injectionEvent(seq uint64, at time.Time) session.Event {
	return dataEvent(seq, session.EventUserMessage, at, map[string]any{
		"source": map[string]any{"kind": "plugin", "plugin": "time-context"},
	})
}

func TestReadingUsesRequestZoneAndModelVisibleBaseline(t *testing.T) {
	service, err := timecontext.New(timecontext.Config{TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	service.Now(func() time.Time { return base.Add(25*time.Hour + time.Minute + time.Second) })
	events := []session.Event{
		dataEvent(1, session.EventTurnStart, base, map[string]any{"turn": 1}),
		dataEvent(2, session.EventUserMessage, base, map[string]any{"text": "turn 1"}),
	}
	reading, err := service.Reading(events, 1, 1, []llm.Message{
		userMessage("turn 1", "request-1", "Asia/Shanghai"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Time sampled while preparing turn 1, step 1: 2026-07-15T09:01:01+08:00[Asia/Shanghai]\n" +
		"Browser time zone for this request: Asia/Shanghai. Interpret otherwise-unqualified dates and times in this zone.\n" +
		"Elapsed since the preceding model-visible message: 1d 1h 1m 1s."
	if reading.Text() != want {
		t.Fatalf("reading = %q, want %q", reading.Text(), want)
	}
	if reading.SourceKind != "plugin" || reading.SourcePlugin != "time-context" ||
		reading.SourceForm != "snapshot" || len(reading.SourceSections) != 1 ||
		reading.SourceSections[0].Name != "time-context" || reading.SourceSections[0].Text != want {
		t.Fatalf("source = %+v", reading)
	}
}

func TestLaterStepUsesSameTurnTimeContextBaseline(t *testing.T) {
	service, err := timecontext.New(timecontext.Config{TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	service.Now(func() time.Time { return base.Add(61 * time.Second) })
	events := []session.Event{
		dataEvent(1, session.EventTurnStart, base, map[string]any{"turn": 3}),
		dataEvent(2, session.EventUserMessage, base, map[string]any{"text": "turn 3"}),
		injectionEvent(3, base),
		dataEvent(4, session.EventTurnStart, base, map[string]any{"turn": 4}),
	}
	reading, err := service.Reading(events, 3, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reading.Text(), "Elapsed since the preceding step context: 1m 1s.") {
		t.Fatalf("reading = %q", reading.Text())
	}
	if !strings.Contains(reading.Text(), "Browser time zone for this request: unavailable.") {
		t.Fatalf("browser policy = %q", reading.Text())
	}
}

func TestRefreshIntervalUsesLatestInjectionIncludingShadowedRows(t *testing.T) {
	service, err := timecontext.New(timecontext.Config{TimeZone: "UTC", RefreshIntervalMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	events := []session.Event{
		dataEvent(1, session.EventUserMessage, base, map[string]any{"text": "request"}),
		injectionEvent(2, base.Add(500*time.Millisecond)),
	}
	service.Now(func() time.Time { return base.Add(1499 * time.Millisecond) })
	if reading, err := service.Reading(events, 1, 2, nil); err != nil || reading != nil {
		t.Fatalf("reading before threshold = %#v, %v, want nil", reading, err)
	}
	service.Now(func() time.Time { return base.Add(1500 * time.Millisecond) })
	reading, err := service.Reading(events, 1, 2, nil)
	if err != nil || reading == nil {
		t.Fatalf("reading at threshold = %#v, %v", reading, err)
	}
}

func TestBackwardWallClockClampsElapsedToZero(t *testing.T) {
	service, err := timecontext.New(timecontext.Config{TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	service.Now(func() time.Time { return base })
	events := []session.Event{
		dataEvent(1, session.EventTurnStart, base, map[string]any{"turn": 1}),
		injectionEvent(2, base),
	}
	service.Now(func() time.Time { return base.Add(-5 * time.Second) })
	reading, err := service.Reading(events, 1, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reading.Text(), "Elapsed since the preceding step context: 0s.") {
		t.Fatalf("reading = %q", reading.Text())
	}
}

func TestMixedBrowserZonesAreSortedAndRejectedWhenInvalid(t *testing.T) {
	service, err := timecontext.New(timecontext.Config{TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	service.Now(func() time.Time { return base })
	messages := []llm.Message{
		userMessage("first", "rpc-1", "Asia/Shanghai"),
		userMessage("second", "rpc-2", "America/New_York"),
	}
	reading, err := service.Reading(nil, 1, 1, messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reading.Text(), `mixed ["America/New_York","Asia/Shanghai"]`) {
		t.Fatalf("mixed reading = %q", reading.Text())
	}
	if _, err := service.Reading(nil, 1, 1, []llm.Message{userMessage("bad", "rpc", "+08:00")}); err == nil ||
		!strings.Contains(err.Error(), "canonical UTC or IANA Area/Location") {
		t.Fatalf("invalid zone error = %v", err)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	if _, err := timecontext.New(timecontext.Config{RefreshIntervalMS: -1}); err == nil ||
		!strings.Contains(err.Error(), "non-negative safe integer") {
		t.Fatalf("interval error = %v", err)
	}
	if _, err := timecontext.New(timecontext.Config{TimeZone: "Not/A_Real_Zone"}); err == nil ||
		!strings.Contains(err.Error(), "invalid IANA timeZone") {
		t.Fatalf("zone error = %v", err)
	}
}
