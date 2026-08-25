package schedule

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Compile-time assertions: engine implements the Engine Service and memProvider
// implements the Provider interface.
var _ Engine = (*engine)(nil)
var _ Provider = (*memProvider)(nil)

// findSchedule returns the schedule with id from a List result.
func findSchedule(list []Schedule, id string) (Schedule, bool) {
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return Schedule{}, false
}

// craftInterval inserts an interval schedule directly through the provider
// (bypassing Add) with controlled timestamps, returning the created schedule.
func craftInterval(t *testing.T, e *engine, spec string, createdAt, nextFire time.Time) Schedule {
	t.Helper()
	s, err := e.prov.Create(context.Background(), Schedule{
		Kind:      KindInterval,
		Spec:      spec,
		Payload:   "ping",
		Enabled:   true,
		CreatedAt: createdAt,
		NextFire:  nextFire,
	})
	if err != nil {
		t.Fatalf("provider Create: %v", err)
	}
	return s
}

// --- Add / Remove / List ----------------------------------------------------

func TestEngineAddListRemove(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	before := time.Now()
	s1, err := e.Add(context.Background(), KindInterval, "30m", "ping")
	if err != nil {
		t.Fatalf("Add(interval): %v", err)
	}
	s2, err := e.Add(context.Background(), KindCron, "0 9 * * *", "morning")
	if err != nil {
		t.Fatalf("Add(cron): %v", err)
	}
	after := time.Now()

	if s1.ID == "" || s2.ID == "" || s1.ID == s2.ID {
		t.Fatalf("Add must issue distinct non-empty ids, got %q and %q", s1.ID, s2.ID)
	}
	if !s1.Enabled || !s2.Enabled {
		t.Fatal("new schedules must be enabled")
	}

	// Interval NextFire must be ≈ now + spec.
	lo, hi := before.Add(30*time.Minute), after.Add(30*time.Minute)
	if s1.NextFire.Before(lo) || s1.NextFire.After(hi) {
		t.Errorf("interval NextFire = %v, want within [%v, %v]", s1.NextFire, lo, hi)
	}
	// Cron NextFire must be in the future.
	if !s2.NextFire.After(before) {
		t.Errorf("cron NextFire = %v, want after %v", s2.NextFire, before)
	}
	// LastFire is nil until the schedule fires.
	if s1.LastFire != nil || s2.LastFire != nil {
		t.Fatal("new schedules must have nil LastFire")
	}

	list, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d schedules, want 2", len(list))
	}
	if got, ok := findSchedule(list, s1.ID); !ok || got.Spec != "30m" || got.Kind != KindInterval {
		t.Errorf("List missing interval schedule: %+v (found=%v)", got, ok)
	}
	if got, ok := findSchedule(list, s2.ID); !ok || got.Spec != "0 9 * * *" || got.Kind != KindCron {
		t.Errorf("List missing cron schedule: %+v (found=%v)", got, ok)
	}

	if err := e.Remove(context.Background(), s1.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, err = e.List(context.Background())
	if err != nil {
		t.Fatalf("List after Remove: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List after Remove returned %d schedules, want 1", len(list))
	}
	if list[0].ID != s2.ID {
		t.Errorf("List after Remove = %q, want %q", list[0].ID, s2.ID)
	}

	if err := e.Remove(context.Background(), "sched-missing"); !errors.Is(err, ErrUnknownSchedule) {
		t.Errorf("Remove of unknown id: err = %v, want ErrUnknownSchedule", err)
	}
}

func TestEngineAddRejectsInvalid(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	bad := []struct {
		kind TriggerKind
		spec string
	}{
		{TriggerKind("bogus"), "30m"}, // unknown kind
		{KindInterval, "0s"},          // zero interval
		{KindInterval, "-5m"},         // negative interval
		{KindInterval, "nope"},        // non-duration
		{KindCron, "0 0 31 2 *"},      // never matches
		{KindCron, "61 * * * *"},      // out of range
		{KindCron, "@daily"},          // unsupported alias
	}
	for _, c := range bad {
		if s, err := e.Add(context.Background(), c.kind, c.spec, "x"); err == nil {
			t.Errorf("Add(%q, %q) = %+v, want error", c.kind, c.spec, s)
		}
	}

	list, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("rejected Adds must not be stored, got %d schedules", len(list))
	}
}

// --- Tick -------------------------------------------------------------------

func TestEngineTickFiresDueOnce(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	s, err := e.Add(context.Background(), KindInterval, "30m", "ping")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	due := s.NextFire

	// A tick before NextFire must fire nothing and change nothing.
	fired, err := e.Tick(context.Background(), due.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Tick(before due): %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("Tick(before due) fired %v, want none", fired)
	}
	list, _ := e.List(context.Background())
	got, _ := findSchedule(list, s.ID)
	if got.LastFire != nil {
		t.Error("Tick(before due) must not set LastFire")
	}
	if !got.NextFire.Equal(due) {
		t.Errorf("Tick(before due) must not change NextFire, got %v want %v", got.NextFire, due)
	}

	// A tick at NextFire fires exactly once and advances the clock.
	fired, err = e.Tick(context.Background(), due)
	if err != nil {
		t.Fatalf("Tick(at due): %v", err)
	}
	if !reflect.DeepEqual(fired, []string{s.ID}) {
		t.Fatalf("Tick(at due) fired %v, want [%s]", fired, s.ID)
	}
	list, _ = e.List(context.Background())
	got, _ = findSchedule(list, s.ID)
	if got.LastFire == nil || !got.LastFire.Equal(due) {
		t.Errorf("LastFire = %v, want %v", got.LastFire, due)
	}
	wantNext := due.Add(30 * time.Minute)
	if !got.NextFire.Equal(wantNext) {
		t.Errorf("NextFire = %v, want %v", got.NextFire, wantNext)
	}

	// A tick just after firing must not fire again (exactly once per period).
	fired, err = e.Tick(context.Background(), due.Add(time.Minute))
	if err != nil {
		t.Fatalf("Tick(after due): %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("Tick(after due) fired %v, want none", fired)
	}

	// A tick at the advanced NextFire fires again.
	fired, err = e.Tick(context.Background(), wantNext)
	if err != nil {
		t.Fatalf("Tick(at next due): %v", err)
	}
	if !reflect.DeepEqual(fired, []string{s.ID}) {
		t.Fatalf("Tick(at next due) fired %v, want [%s]", fired, s.ID)
	}
}

func TestEngineTickSkipsDisabled(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	s, err := e.Add(context.Background(), KindInterval, "30m", "ping")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	disabled := s
	disabled.Enabled = false
	if _, err := e.prov.Update(context.Background(), disabled); err != nil {
		t.Fatalf("provider Update: %v", err)
	}

	fired, err := e.Tick(context.Background(), disabled.NextFire)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("disabled schedule fired %v, want none", fired)
	}
}

func TestEngineTickSkipsDeleted(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	s, err := e.Add(context.Background(), KindInterval, "30m", "ping")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Remove(context.Background(), s.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	fired, err := e.Tick(context.Background(), s.NextFire.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("deleted schedule fired %v, want none", fired)
	}
}

func TestEngineTickMissedPeriodsFireOnce(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	// A schedule whose NextFire is 48h in the past: many periods were missed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := craftInterval(t, e, "30m", past.Add(-24*time.Hour), past)
	now := past.Add(48 * time.Hour)

	fired, err := e.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !reflect.DeepEqual(fired, []string{s.ID}) {
		t.Fatalf("Tick fired %v, want exactly [%s]", fired, s.ID)
	}

	list, _ := e.List(context.Background())
	got, _ := findSchedule(list, s.ID)
	if got.LastFire == nil || !got.LastFire.Equal(now) {
		t.Errorf("LastFire = %v, want %v", got.LastFire, now)
	}
	// NextFire advances past now by exactly one interval — no backfill of the
	// 96 missed periods.
	wantNext := now.Add(30 * time.Minute)
	if !got.NextFire.Equal(wantNext) {
		t.Errorf("NextFire = %v, want %v", got.NextFire, wantNext)
	}

	// And a subsequent tick before the new NextFire must not fire again.
	fired, err = e.Tick(context.Background(), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Tick(+1m): %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("Tick(+1m) fired %v, want none", fired)
	}
}

func TestEngineTickMultipleDue(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	// Both due at now: an interval and a cron (cron is preloaded into the
	// provider, exercising the parse-from-spec cache fallback).
	a := craftInterval(t, e, "30m", now.Add(-time.Hour), now)
	b, err := e.prov.Create(context.Background(), Schedule{
		Kind:      KindCron,
		Spec:      "0 9 * * *",
		Payload:   "b",
		Enabled:   true,
		CreatedAt: now.Add(-time.Hour),
		NextFire:  now,
	})
	if err != nil {
		t.Fatalf("provider Create(cron): %v", err)
	}

	fired, err := e.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !reflect.DeepEqual(fired, []string{a.ID, b.ID}) {
		t.Fatalf("Tick fired %v, want [%s %s] (sorted by id)", fired, a.ID, b.ID)
	}

	list, _ := e.List(context.Background())
	ga, _ := findSchedule(list, a.ID)
	if !ga.NextFire.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("interval NextFire = %v, want %v", ga.NextFire, now.Add(30*time.Minute))
	}
	gb, _ := findSchedule(list, b.ID)
	wantCronNext := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	if !gb.NextFire.Equal(wantCronNext) {
		t.Errorf("cron NextFire = %v, want %v", gb.NextFire, wantCronNext)
	}
}

// --- Close ------------------------------------------------------------------

func TestEngineCloseIdempotent(t *testing.T) {
	e := NewEngine(nil)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close must be idempotent, got %v", err)
	}

	if _, err := e.Add(context.Background(), KindInterval, "30m", "x"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Add after Close: err = %v, want ErrEngineClosed", err)
	}
	if err := e.Remove(context.Background(), "x"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Remove after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.List(context.Background()); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("List after Close: err = %v, want ErrEngineClosed", err)
	}
	if _, err := e.Tick(context.Background(), time.Now()); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Tick after Close: err = %v, want ErrEngineClosed", err)
	}
}

func TestEngineUsesInjectedProvider(t *testing.T) {
	p := NewMemProvider()
	e := NewEngine(p)
	defer e.Close()

	s, err := e.Add(context.Background(), KindInterval, "30m", "x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	all, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("provider List: %v", err)
	}
	if len(all) != 1 || all[0].ID != s.ID {
		t.Errorf("injected provider holds %+v, want the added schedule", all)
	}
}
