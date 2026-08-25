// Package schedule defines the recurring-trigger capability seam (design.md
// §10 D2, ADR 2026-08-19-m6-agent-full.md 决策 M6a): a Registry + Provider seam
// for interval and cron triggers. An Engine (the seam's Service) owns spec
// validation and the pure Tick clock; a Provider is the backend that stores
// the schedule table. Consumers (M6a-2's schedule_* tools and the fire-event
// wiring) depend only on the seam's interfaces (D2), so swapping or
// persisting the backend never touches consumer code.
//
// The default Provider is the in-memory memProvider (mem.go): every schedule
// lives in memory only, so a process restart clears the table by
// construction. Persisting the schedule table to the store layer is
// deliberately deferred to M6a-2 or later — the seam already isolates that
// change behind the Provider interface.
//
// Tick is a pure advancement step (design.md §10 D5, ADR 决策 D5): it returns
// the IDs of the Enabled schedules whose NextFire is due at the given now and
// advances their LastFire/NextFire, but performs no side effects of its own —
// it does not log, enqueue jobs or drive the loop. The wiring layer turns a
// returned ID into a schedule/fire event or a job. There is deliberately no
// background ticker: the wiring calls Tick on its own serial path.
//
// Supported trigger specs:
//   - interval: any time.ParseDuration format with a strictly positive value
//     ("30m", "1h30m", "24h", …). A zero or negative interval is rejected
//     because it cannot advance NextFire.
//   - cron: a 5-field standard cron expression "minute hour day month weekday"
//     (minute 0-59, hour 0-23, day 1-31, month 1-12, weekday 0-6 with
//     Sunday = 0, matching Go's time.Weekday). Fields accept "*", a-b, */n,
//     a-b/n and comma-separated lists of those. When both day and weekday are
//     restricted the date matches if either matches (standard cron OR
//     semantics). Seconds, aliases (@daily, JAN, SUN), "?", "L", "W" and
//     6-field expressions are not supported and are rejected at Add time.
package schedule

import (
	"context"
	"errors"
	"time"
)

// TriggerKind identifies a schedule's trigger family.
type TriggerKind string

const (
	KindInterval TriggerKind = "interval" // spec is a time.ParseDuration string
	KindCron     TriggerKind = "cron"     // spec is a 5-field cron expression
)

// Schedule is one recurring trigger. Callers receive fresh value copies, never
// live registry state.
type Schedule struct {
	ID        string // provider-issued id ("sched-N" under the memory provider)
	Kind      TriggerKind
	Spec      string // interval: "30m"/"1h30m"; cron: "0 9 * * *"
	Payload   string // action text handed to the executor when the trigger fires
	Enabled   bool   // false schedules are never fired by Tick
	CreatedAt time.Time
	LastFire  *time.Time // set once the schedule has fired at least once; nil otherwise
	NextFire  time.Time  // zero means never scheduled
}

// Provider is one schedule backend (design.md §10 D2: Service / Provider /
// Consumer three-piece seam). It is a dumb store: the Engine performs all
// validation and clock advancement and calls through Create/Update/Delete.
type Provider interface {
	Name() string
	// List returns every current schedule, sorted by id.
	List(ctx context.Context) ([]Schedule, error)
	// Create stores s and returns it with a provider-issued id filled in.
	Create(ctx context.Context, s Schedule) (Schedule, error)
	// Update replaces the schedule with s.ID; an unknown id is rejected.
	Update(ctx context.Context, s Schedule) (Schedule, error)
	// Delete removes the schedule with id; an unknown id is rejected.
	Delete(ctx context.Context, id string) error
}

// Engine is the schedule Service (design.md §10 D2, ADR 决策 M6a). Consumers
// depend only on this interface, never on a concrete backend.
//
// Lifecycle: Add validates the trigger and stores a new Enabled schedule;
// Remove deletes one; List observes the table; Tick advances the clock one
// step (a pure function of the table and now); Close releases the backend and
// rejects further operations. Close is idempotent.
type Engine interface {
	// Add validates kind and spec, computes the first NextFire, and stores a new
	// Enabled schedule through the Provider, returning the created schedule with
	// its provider-issued id. Invalid kinds, specs and never-matching cron
	// expressions are rejected before any record is written.
	Add(ctx context.Context, kind TriggerKind, spec, payload string) (Schedule, error)
	// Remove deletes the schedule with id; an unknown id is rejected.
	Remove(ctx context.Context, id string) error
	// List returns every current schedule, sorted by id.
	List(ctx context.Context) ([]Schedule, error)
	// Tick advances the schedule clock once: every Enabled schedule whose
	// NextFire is due at or before now fires exactly once, its LastFire is set
	// to now and NextFire is advanced past now (missed periods are never
	// backfilled — one firing per Tick), and its id is returned. Disabled and
	// deleted schedules are never returned. Tick performs no side effects of its
	// own (D5): the returned ids are for the wiring layer to turn into a
	// schedule/fire event or a job.
	Tick(ctx context.Context, now time.Time) ([]string, error)
	// Close releases the backend and marks the engine closed. It is idempotent;
	// Add/Remove/List/Tick after Close are rejected with ErrEngineClosed.
	Close() error
}

// closer is the optional extension a Provider implements to release its
// resources when the Engine is closed (mirrors the subagent seam's closer).
type closer interface {
	Close() error
}

// Sentinel errors returned by Engine and Provider implementations so callers
// can distinguish failures without parsing message text.
var (
	ErrInvalidSpec     = errors.New("schedule: invalid trigger spec")
	ErrUnknownSchedule = errors.New("schedule: unknown schedule")
	ErrEngineClosed    = errors.New("schedule: engine closed")
	ErrProviderClosed  = errors.New("schedule: provider closed")
)
