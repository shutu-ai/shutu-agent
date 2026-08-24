package schedule

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func TestDurableSchedulerFoldAndDispatch(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var events []session.Event
	s := NewDurableScheduler(func(change DurableChange) error {
		raw, err := json.Marshal(change)
		if err != nil {
			return err
		}
		events = append(events, session.Event{Type: session.EventScheduleChange, Data: raw})
		return nil
	})
	seconds := int64(1)
	one, err := s.Create(context.Background(), DurableCreateRequest{Prompt: "check logs", AfterSeconds: &seconds}, base)
	if err != nil || one.ID != "schedule-1" {
		t.Fatalf("create = %+v, err=%v", one, err)
	}
	due, ok, err := s.Due(context.Background(), base.Add(2*time.Second))
	if err != nil || !ok {
		t.Fatalf("due = %+v ok=%v err=%v", due, ok, err)
	}
	if err := s.Dispatch(context.Background(), due); err != nil {
		t.Fatal(err)
	}
	views, err := s.List(context.Background(), base.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("one-shot remains active: %+v", views)
	}
	restored := NewDurableScheduler(nil)
	if err := restored.Restore(events); err != nil {
		t.Fatal(err)
	}
	views, err = restored.List(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("restored dispatched one-shot: %+v", views)
	}
}

func TestDurableEverySkipsMissedOccurrencesAndBatches(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var changes []DurableChange
	s := NewDurableScheduler(func(change DurableChange) error { changes = append(changes, change); return nil })
	seconds := int64(300)
	if _, err := s.Create(context.Background(), DurableCreateRequest{Prompt: "metrics", EverySeconds: &seconds}, base); err != nil {
		t.Fatal(err)
	}
	due, ok, err := s.Due(context.Background(), base.Add(901*time.Second))
	if err != nil || !ok || len(due.Records) != 1 {
		t.Fatalf("due=%+v ok=%v err=%v", due, ok, err)
	}
	if err := s.Dispatch(context.Background(), due); err != nil {
		t.Fatal(err)
	}
	views, err := s.List(context.Background(), base.Add(901*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || !views[0].ScheduledAt.Equal(base.Add(1200*time.Second)) {
		t.Fatalf("next=%+v", views)
	}
	if len(changes) != 2 || changes[1].AcceptedAt == nil {
		t.Fatalf("changes=%+v", changes)
	}
}
