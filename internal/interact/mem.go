package interact

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// memProvider is the default in-memory Provider (ADR 决策 M6d). Every request
// lives in memory only — nothing is persisted and no files are touched — so a
// process restart clears the approval table by construction. It is safe for
// concurrent use and performs no validation beyond the seam's invariants: the
// Engine is the validation boundary, and Create/Resolve are called through by
// the Engine. A store-backed Provider can replace it without touching Engine or
// consumer code.
type memProvider struct {
	mu       sync.Mutex
	requests map[string]Request
	nextID   int
	closed   bool
}

// NewMemProvider returns a fresh in-memory Provider — the default backend for
// NewEngine. It is exported so wiring and tests can inject it explicitly or
// preload requests with controlled timestamps.
func NewMemProvider() Provider {
	return newMemProvider()
}

func newMemProvider() *memProvider {
	return &memProvider{requests: map[string]Request{}}
}

// Name identifies the provider in the registry ("memory").
func (m *memProvider) Name() string { return "memory" }

// List returns every current request, sorted by id.
func (m *memProvider) List(ctx context.Context) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrProviderClosed
	}
	out := make([]Request, 0, len(m.requests))
	for _, r := range m.requests {
		out = append(out, cloneRequest(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListForSession is the provider-side ownership filter. Keep it under the
// provider mutex so a scoped answerer observes one consistent snapshot.
func (m *memProvider) ListForSession(ctx context.Context, sessionID string) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrProviderClosed
	}
	out := make([]Request, 0)
	for _, r := range m.requests {
		if r.SessionID == sessionID {
			out = append(out, cloneRequest(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Create stores r under a fresh provider-issued id and returns the stored copy.
func (m *memProvider) Create(ctx context.Context, r Request) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Request{}, ErrProviderClosed
	}
	m.nextID++
	r.ID = fmt.Sprintf("req-%d", m.nextID)
	m.requests[r.ID] = cloneRequest(r)
	return cloneRequest(r), nil
}

// Restore installs durable request snapshots without allocating new ids.
// Existing ids are preserved so a browser that was open across a restart can
// still resolve the same request; the next generated id advances past the
// restored req-N range.
func (m *memProvider) Restore(ctx context.Context, requests []Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	for _, r := range requests {
		if r.ID == "" {
			return fmt.Errorf("interact: restored request id is empty")
		}
		m.requests[r.ID] = cloneRequest(r)
		var n int
		if _, err := fmt.Sscanf(r.ID, "req-%d", &n); err == nil && n > m.nextID {
			m.nextID = n
		}
	}
	return nil
}

// Resolve marks the request with id as resolved with status. An unknown id and
// a request that is no longer pending are rejected; the stored record's
// ResolvedAt is stamped at resolution time.
func (m *memProvider) Resolve(ctx context.Context, id string, status ApprovalStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrProviderClosed
	}
	r, ok := m.requests[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	}
	if r.Status != StatusPending {
		return fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	now := time.Now()
	r.Status = status
	r.ResolvedAt = &now
	m.requests[id] = r
	return nil
}

// Close marks the provider closed so no further operations are accepted. It is
// idempotent and releases nothing else (no goroutines live here).
func (m *memProvider) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// cloneRequest copies a Request so the returned value never aliases the
// record's ResolvedAt pointer.
func cloneRequest(r Request) Request {
	if r.Questions != nil {
		r.Questions = append([]Question(nil), r.Questions...)
		for i := range r.Questions {
			r.Questions[i].Options = append([]QuestionOption(nil), r.Questions[i].Options...)
		}
	}
	if r.ResolvedAt != nil {
		t := *r.ResolvedAt
		r.ResolvedAt = &t
	}
	if r.ExpiresAt != nil {
		t := *r.ExpiresAt
		r.ExpiresAt = &t
	}
	return r
}
