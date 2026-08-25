// Package interact defines the human-approval capability seam (design.md §10
// D2, ADR 2026-08-19-m6-agent-full.md 决策 M6d): an ApprovalStatus + Request
// model with a Provider + Engine seam for pending approval requests. An Engine
// (the seam's Service) owns validation — prompt legality, args bound, status
// legality, duplicate-resolution rejection, the pending cap — and a Provider is
// the dumb backend that stores requests. Consumers (M6d-2's interact_* tools,
// the interact/* event wiring and the sensitive-tool gate) depend only on the
// seam's interfaces (D2), so swapping or persisting the backend never touches
// consumer code.
//
// The default Provider is the in-memory memProvider (mem.go): every request
// lives in memory only — nothing is persisted and no files are touched — so a
// process restart clears the approval table by construction. Persisting
// requests to the store layer is deliberately deferred to M6d-2 or later: the
// seam already isolates that change behind the Provider interface.
//
// Await is a pure poll loop (design.md §10 D5, ADR 决策 D5): it repeatedly
// lists the request until it leaves pending, a context cancellation or an
// error ends the wait. There is deliberately no background goroutine and no
// shared wait state — the caller drives Await on its own serial path (the CLI
// interaction flow), so a resolution made in another goroutine simply becomes
// visible on the next poll.
package interact

import (
	"context"
	"errors"
	"time"
)

// ApprovalStatus is the lifecycle of one approval request.
type ApprovalStatus string

const (
	StatusPending  ApprovalStatus = "pending"   // created, waiting for the user
	StatusApproved ApprovalStatus = "approved"  // the user said yes
	StatusRejected ApprovalStatus = "rejected"  // the user said no
	StatusExpired  ApprovalStatus = "expired"   // abandoned (reserved for later expiry)
	StatusCanceled ApprovalStatus = "cancelled" // the question UI was dismissed
)

// QuestionOption is one selectable answer offered by a structured user
// question. It mirrors DSH's wire-safe question option without coupling the
// interaction seam to a UI package.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one structured question carried by an interaction request.
// Empty Questions keeps the legacy approval-only request shape intact.
type Question struct {
	ID          string           `json:"id"`
	Question    string           `json:"question"`
	Detail      string           `json:"detail,omitempty"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
}

// Request is one pending-or-resolved human-approval item. Callers receive fresh
// value copies, never live provider state.
type Request struct {
	ID         string         // provider-issued id ("req-N" under the memory provider)
	Prompt     string         // user-facing explanation of the sensitive action
	ToolName   string         // tool whose execution triggered the approval
	Args       string         // tool args JSON at trigger time (bounded, ≤ maxArgsLen runes)
	Questions  []Question     `json:"questions,omitempty"`
	Answer     string         `json:"answer,omitempty"` // structured answer JSON, set after resolution
	Status     ApprovalStatus // pending by default
	CreatedAt  time.Time
	ResolvedAt *time.Time // set once the request leaves pending; nil otherwise
}

// Provider is one approval backend (design.md §10 D2: Service / Provider /
// Consumer three-piece seam). It is a dumb store: the Engine performs all
// validation and calls through List/Create/Resolve.
type Provider interface {
	Name() string
	// List returns every current request, sorted by id.
	List(ctx context.Context) ([]Request, error)
	// Create stores r and returns it with a provider-issued id filled in.
	Create(ctx context.Context, r Request) (Request, error)
	// Resolve marks the request with id as resolved with status. An unknown id
	// and a request that is no longer pending are rejected.
	Resolve(ctx context.Context, id string, status ApprovalStatus) error
}

// Engine is the approval Service (design.md §10 D2, ADR 决策 M6d). Consumers
// depend only on this interface, never on a concrete backend.
//
// Lifecycle: Request creates a pending request; Resolve records the user's
// decision; Await blocks until a request is resolved; List observes the table;
// Close releases the backend and rejects further operations. Close is
// idempotent.
type Engine interface {
	// Request validates the prompt and args and creates a pending request,
	// returning it with its provider-issued id. An empty prompt and args longer
	// than the bound are rejected; when the pending count is at the cap
	// (default pendingLimit) or the provider is closed, Request reports an
	// error.
	Request(ctx context.Context, prompt, toolName, args string) (Request, error)
	// Resolve records the user's decision for the request with id: approved or
	// rejected. An unknown id, a request already resolved and an invalid status
	// are rejected. The resolved request is returned.
	Resolve(ctx context.Context, id string, status ApprovalStatus) (Request, error)
	// Await blocks until the request with id leaves pending (Resolve), the
	// context is cancelled, or the request disappears. v1 has no background
	// wait: Await polls the Provider on a short interval on the caller's serial
	// path (D5), so a resolution made elsewhere becomes visible on the next
	// poll. An unknown id fails fast with ErrUnknownRequest.
	Await(ctx context.Context, id string) (Request, error)
	// List returns every current request, sorted by id.
	List(ctx context.Context) ([]Request, error)
	// Close releases the backend and marks the engine closed. It is idempotent;
	// every other operation after Close is rejected with ErrEngineClosed.
	Close() error
}

// StructuredRequester is an optional extension implemented by the in-memory
// engine. Keeping it outside Engine preserves compatibility with existing
// approval consumers and test doubles while allowing interact_ask to carry DSH
// style question batches when the concrete engine supports them.
type StructuredRequester interface {
	RequestWithQuestions(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, error)
}

// AnswerResolver is an optional extension used by the Web transport to retain
// the structured answer while preserving the normal approved/rejected status.
type AnswerResolver interface {
	ResolveWithAnswer(ctx context.Context, id string, status ApprovalStatus, answer string) (Request, error)
}

// Canceler is the optional user-question cancellation extension. Cancellation
// is separate from rejection so a blocked ask_user_question can distinguish a
// dismissed UI from a deliberate negative answer.
type Canceler interface {
	Cancel(ctx context.Context, id string) (Request, error)
}

// RequestRestorer is an optional startup hook for providers that can restore
// approval snapshots from a durable session log. It intentionally stays
// outside Engine so existing consumers and test doubles remain source-
// compatible.
type RequestRestorer interface {
	Restore(ctx context.Context, requests []Request) error
}

// closer is the optional extension a Provider implements to release its
// resources when the Engine is closed (mirrors the schedule and plan seams'
// closer).
type closer interface {
	Close() error
}

// Sentinel errors returned by Engine and Provider implementations so callers
// can distinguish failures without parsing message text.
var (
	ErrInvalidPrompt   = errors.New("interact: invalid prompt")
	ErrInvalidArgs     = errors.New("interact: invalid args")
	ErrInvalidStatus   = errors.New("interact: invalid approval status")
	ErrUnknownRequest  = errors.New("interact: unknown request")
	ErrAlreadyResolved = errors.New("interact: request already resolved")
	ErrPendingLimit    = errors.New("interact: pending limit reached")
	ErrEngineClosed    = errors.New("interact: engine closed")
	ErrProviderClosed  = errors.New("interact: provider closed")
)
