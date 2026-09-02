package schedule

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// engine is the default schedule Service implementation (ADR 决策 M6a): it
// owns validation and the pure Tick clock, delegating storage to a Provider.
// Add/Remove/List call through to the Provider; Tick additionally advances the
// due schedules' LastFire/NextFire via the Provider. It is safe for concurrent
// use; Close is idempotent and releases the Provider. The unexported concrete
// type (mirroring the subagent seam's unexported runtime) keeps the Engine
// interface the only public shape; NewEngine returns it as a concrete *engine
// that satisfies Engine.
type engine struct {
	prov Provider

	mu        sync.Mutex
	exprs     map[string]*cronExpr // parsed cron expressions by schedule id (interval needs no cache)
	closed    bool
	closeDone chan struct{}
}

// NewEngine returns an engine backed by prov; a nil prov selects the default
// in-memory Provider (NewMemProvider). Each engine should own its provider:
// Close releases it.
func NewEngine(prov Provider) *engine {
	if prov == nil {
		prov = newMemProvider()
	}
	return &engine{
		prov:      prov,
		exprs:     map[string]*cronExpr{},
		closeDone: make(chan struct{}),
	}
}

// Add validates kind and spec, computes the first NextFire, and stores the new
// Enabled schedule through the Provider, returning the created schedule with
// its Provider-issued id. Invalid kinds, specs and never-matching cron
// expressions are rejected before any record is written.
func (e *engine) Add(ctx context.Context, kind TriggerKind, spec, payload string) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Schedule{}, err
	}

	now := time.Now()
	var next time.Time
	var expr *cronExpr
	switch kind {
	case KindInterval:
		d, err := parseIntervalSpec(spec)
		if err != nil {
			return Schedule{}, err
		}
		next = nextInterval(now, d)
	case KindCron:
		var err error
		expr, err = parseCronSpec(spec)
		if err != nil {
			return Schedule{}, err
		}
		next, err = expr.next(now)
		if err != nil {
			return Schedule{}, fmt.Errorf("%w: %s: %v", ErrInvalidSpec, spec, err)
		}
	default:
		return Schedule{}, fmt.Errorf("schedule: unknown trigger kind %q", kind)
	}

	created, err := e.prov.Create(ctx, Schedule{
		Kind:      kind,
		Spec:      spec,
		Payload:   payload,
		Enabled:   true,
		CreatedAt: now,
		NextFire:  next,
	})
	if err != nil {
		return Schedule{}, err
	}
	if expr != nil {
		e.mu.Lock()
		e.exprs[created.ID] = expr
		e.mu.Unlock()
	}
	return created, nil
}

// Remove deletes the schedule with id; an unknown id is rejected.
func (e *engine) Remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkOpen(); err != nil {
		return err
	}
	if err := e.prov.Delete(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.exprs, id)
	e.mu.Unlock()
	return nil
}

// List returns every current schedule, sorted by id.
func (e *engine) List(ctx context.Context) ([]Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}
	return e.prov.List(ctx)
}

// Tick advances the schedule clock once (pure advancement, design.md §10 D5):
// every Enabled schedule whose NextFire is due at or before now fires exactly
// once — LastFire is set to now, NextFire is advanced past now, and the id is
// returned. Missed periods are never backfilled (one firing per Tick). Disabled
// and deleted schedules are never returned. No side effects of its own: the
// returned ids are for the wiring layer to act on.
func (e *engine) Tick(ctx context.Context, now time.Time) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}
	all, err := e.prov.List(ctx)
	if err != nil {
		return nil, err
	}
	var fired []string
	for _, s := range all {
		if !s.Enabled {
			continue
		}
		if s.NextFire.IsZero() || s.NextFire.After(now) {
			continue
		}
		next, err := e.nextAfter(s, now)
		if err != nil {
			return nil, err
		}
		updated := s
		updated.LastFire = &now
		updated.NextFire = next
		if _, err := e.prov.Update(ctx, updated); err != nil {
			return nil, err
		}
		fired = append(fired, s.ID)
	}
	sort.Strings(fired)
	return fired, nil
}

// nextAfter computes the next fire time strictly after `after` for s. For
// interval the next fire is after+spec; for cron it is the next matching
// occurrence (parsed via the cache, falling back to parsing the stored spec —
// which covers schedules preloaded into the Provider without going through
// Add).
func (e *engine) nextAfter(s Schedule, after time.Time) (time.Time, error) {
	switch s.Kind {
	case KindInterval:
		d, err := parseIntervalSpec(s.Spec)
		if err != nil {
			return time.Time{}, err
		}
		return nextInterval(after, d), nil
	case KindCron:
		expr, err := e.exprFor(s.ID, s.Spec)
		if err != nil {
			return time.Time{}, err
		}
		return expr.next(after)
	default:
		return time.Time{}, fmt.Errorf("schedule: unknown trigger kind %q", s.Kind)
	}
}

// exprFor returns the cached parsed expression for id, parsing and caching the
// stored spec on a miss (e.g. a schedule preloaded into the Provider directly).
func (e *engine) exprFor(id, spec string) (*cronExpr, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if expr, ok := e.exprs[id]; ok {
		return expr, nil
	}
	expr, err := parseCronSpec(spec)
	if err != nil {
		return nil, err
	}
	e.exprs[id] = expr
	return expr, nil
}

// checkOpen rejects operations on a closed engine.
func (e *engine) checkOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEngineClosed
	}
	return nil
}

// Close releases the backend (if it implements closer) and marks the engine
// closed so Add/Remove/List/Tick are rejected. It is idempotent.
func (e *engine) Close() error {
	e.mu.Lock()
	if e.closed {
		done := e.closeDone
		e.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	e.closed = true
	prov := e.prov
	e.mu.Unlock()
	if c, ok := prov.(closer); ok {
		err := c.Close()
		close(e.closeDone)
		return err
	}
	close(e.closeDone)
	return nil
}
