// Package spill defines the long-term-memory capability seam (design.md §10
// D2, ADR 2026-08-19-m6-agent-full.md 决策 M6c): a Provider + Engine seam for
// conversation-derived memories. An Engine (the seam's Service) owns id
// issuance (content-hash, so the same content always maps to the same memo),
// dedup and the AutoSpill policy; a Provider is the dumb backend that stores
// Memo rows. Consumers (M6c-2's spill_* tools and the spill/* event wiring)
// depend only on the seam's interfaces (D2), so swapping or persisting the
// backend never touches consumer code.
//
// Spill is automatic conversation-derived memory. Its seam stays independent
// from the other storage-backed capabilities (D9).
//
// The default Provider is the in-memory memProvider (mem.go): every memo lives
// in memory only — nothing is persisted and no files are touched — so a
// process restart clears the memory by construction. Persisting the memo table
// to the store layer is deliberately deferred to M6c-2 or later: the seam
// already isolates that change behind the Provider interface, so a
// store-backed Provider can be added without touching Engine or consumer code.
package spill

import (
	"context"
	"errors"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

// Memo is one conversation-derived memory row.
type Memo struct {
	ID        string // content-hash id ("memo-<hex>"); same content → same id
	Content   string // memo body (a durable fact derived from conversation)
	Source    string // provenance ("session:<seq>", "session:<seq>:tool:<name>", "auto", …)
	CreatedAt time.Time
}

// Provider is one spill backend (design.md §10 D2: Service / Provider /
// Consumer three-piece seam). It is a dumb store: the Engine performs all id
// issuance and policy and calls through Add/Get/Delete/Search. Callers receive
// fresh value copies, never live registry state.
type Provider interface {
	Name() string
	// List returns every current memo, sorted by id.
	List(ctx context.Context) ([]Memo, error)
	// Add is an idempotent upsert by ID (an empty id is rejected).
	Add(ctx context.Context, m Memo) (Memo, error)
	// Get returns the memo with id; an unknown id is rejected with
	// ErrUnknownMemo.
	Get(ctx context.Context, id string) (Memo, error)
	// Delete removes the memo with id; an unknown id is rejected with
	// ErrUnknownMemo.
	Delete(ctx context.Context, id string) error
	// Search returns up to limit memos whose content matches query. v1 uses
	// case-insensitive substring matching with zero dependencies; a later FTS
	// or vector Provider can swap in without touching consumers.
	Search(ctx context.Context, query string, limit int) ([]Memo, error)
}

// Engine is the spill Service (design.md §10 D2, ADR 决策 M6c). Consumers
// depend only on this interface, never on a concrete backend.
//
// Lifecycle: Spill writes one memo idempotently; Recall retrieves by query;
// AutoSpill runs the v1 auto-sedimentation policy over a session event log and
// reports how many new memos it stored; List observes the store; Remove deletes
// one; Close releases the backend and rejects further operations. Close is
// idempotent.
type Engine interface {
	// Spill writes content as a memo with source and returns it. The id is the
	// content hash, so spilling the same content twice is idempotent — the
	// second call returns the existing memo unchanged and never duplicates.
	Spill(ctx context.Context, content, source string) (Memo, error)
	// Recall returns up to limit memos whose content matches query. v1 matches
	// by case-insensitive substring. limit <= 0 means the default of 5.
	Recall(ctx context.Context, query string, limit int) ([]Memo, error)
	// AutoSpill runs the v1 automatic-sedimentation policy (policy.go): it
	// reads the conversation event log, extracts each turn's final assistant
	// text (tool-call frames excluded) and a bounded summary of each
	// tool/result, keeps the ones worth remembering, and writes them
	// deduplicated. It returns the number of NEW memos added (re-spilling an
	// already-stored memo counts 0). The policy kernel is a pure function — it
	// performs no side effects; the wiring layer calls AutoSpill on its own
	// serial path (D5).
	AutoSpill(ctx context.Context, events []session.Event) (int, error)
	// List returns every current memo, sorted by id.
	List(ctx context.Context) ([]Memo, error)
	// Remove deletes the memo with id; an unknown id is rejected with
	// ErrUnknownMemo.
	Remove(ctx context.Context, id string) error
	// Close releases the backend and marks the engine closed. It is idempotent;
	// every other operation after Close is rejected with ErrEngineClosed.
	Close() error
}

// closer is the optional extension a Provider implements to release its
// resources when the Engine is closed (mirrors the schedule/plan seams).
type closer interface {
	Close() error
}

// Sentinel errors returned by Engine and Provider implementations so callers
// can distinguish failures without parsing message text.
var (
	ErrUnknownMemo    = errors.New("spill: unknown memo")
	ErrEngineClosed   = errors.New("spill: engine closed")
	ErrProviderClosed = errors.New("spill: provider closed")
)
