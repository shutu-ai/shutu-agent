// Package store defines the persistence abstraction for the session log
// (design.md D8) and its SQLite backend. The store appends events durably and
// replays them at startup; the in-memory log is always rebuilt from the store,
// never the other way around (D1: history is a derived value).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

// ErrNotFound is returned when a session id has no row in the store.
var ErrNotFound = errors.New("store: session not found")

// SessionMeta is the durable metadata of one session.
type SessionMeta struct {
	ID         string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	EventCount int
	// Title is the accepted display title (fallback / LLM / user rename).
	// When empty the UI falls back to the first-user-message inference.
	Title string
	// TitleSource is the producer of the accepted title: "" | fallback | llm
	// | user. "user" pins the title so automatic work never re-revises it.
	TitleSource string
	// WorkspaceID is the owning workspace (P6 grouping), empty for the
	// ungrouped bucket.
	WorkspaceID string
	// CWD is the working directory captured when the session was created.
	// Empty means the session predates the session-header migration.
	CWD string
	// ArchivedAt is non-zero once the session is archived (P6.2 dsh archive):
	// archived sessions leave the active sidebar list.
	ArchivedAt time.Time
	// Sort is the manual drag order (P6.2): zero means "fall back to updated
	// activity"; a drag sets the whole bucket's order.
	Sort int
	// FlatSort is the manual drag order for the flat (ungrouped) view
	// (P6.3): zero means "fall back to updated activity". It is independent
	// from the per-workspace Sort.
	FlatSort int
	// LastViewedAt is when the session was last opened/messaged in the UI.
	// Zero means never viewed; its presence lets the sidebar's finished-
	// but-unviewed reminder (dsh status.completed) distinguish a session that
	// finished work the user has not opened yet.
	LastViewedAt time.Time
}

// SessionHeaderStore exposes the small durable header projection used by
// read-only session-query consumers. It is deliberately optional so existing
// Store implementations and test doubles remain source-compatible.
type SessionHeaderStore interface {
	SetSessionCWD(ctx context.Context, sessionID, cwd string) error
}

// SessionSearchPager is an optional bounded page reader for session search.
// The model-facing tool collects these pages internally; the cursor/offset is
// an implementation detail of the local provider.
type SessionSearchPager interface {
	SearchSessionsPage(ctx context.Context, q string, offset, limit int) ([]SearchHit, bool, error)
}

// SessionConfig is the per-session override for the mode preset, LLM provider /
// model / reasoning effort and permission tier (Phase 2: 按会话切换; dsh
// ModelSelection 对齐: the session owns its full provider+model+effort
// selection). Empty fields fall back to the global config. Mode is locked at
// session creation (agent_preset); provider, model, effort and permission stay
// editable per session.
type SessionConfig struct {
	AgentPreset     string // "" | minimal | standard | code (locked at creation)
	Provider        string // "" → fall back to the global provider (dsh selection.provider)
	Model           string // "" → fall back to the global model
	ReasoningEffort string // "" | off | low | high | max ("" → provider default)
	Permission      string // "" | readonly | standard | full
}

// SessionConfigStore is the per-session-config read/write surface. It is NOT on
// the Store interface (so existing stubs and callers stay untouched); consumers
// that need it (the web portal's per-session endpoints, the agent loop) assert
// this interface against their Store value. Databases created before these
// columns exist return empty configs (all fields fall back to the globals).
type SessionConfigStore interface {
	// GetSessionConfig reads a session's overrides; a missing or legacy
	// session returns zero values (ErrNotFound only when the id has no row).
	GetSessionConfig(ctx context.Context, sessionID string) (SessionConfig, error)
	// SetSessionConfig writes the full override set (used at creation and by
	// the loop's mode lock). ErrNotFound when the id has no row.
	SetSessionConfig(ctx context.Context, sessionID string, cfg SessionConfig) error
	// UpdateSessionConfig rewrites provider, model, reasoning effort and
	// permission (mode stays locked). ErrNotFound when the id has no row.
	UpdateSessionConfig(ctx context.Context, sessionID, provider, model, reasoningEffort, permission string) error
}

// MessageFeedback is the durable rating attached to one assistant/message
// event. Seq is scoped by SessionID and identifies the assistant response.
type MessageFeedback struct {
	SessionID string    `json:"session_id"`
	Seq       uint64    `json:"seq"`
	Rating    string    `json:"rating"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MaxMessageFeedbackNoteBytes bounds optional notes accepted by the feedback
// API. The current Web UI only sends the rating, but the field is kept for
// parity with dsh's feedback model.
const MaxMessageFeedbackNoteBytes = 4096

// MessageFeedbackStore is the optional persistence surface used by the Web
// portal's assistant thumbs-up/thumbs-down actions.
type MessageFeedbackStore interface {
	ListMessageFeedback(ctx context.Context, sessionID string) ([]MessageFeedback, error)
	GetMessageFeedback(ctx context.Context, sessionID string, seq uint64) (MessageFeedback, bool, error)
	PutMessageFeedback(ctx context.Context, sessionID string, seq uint64, rating, note string) (MessageFeedback, error)
	DeleteMessageFeedback(ctx context.Context, sessionID string, seq uint64) error
}

// SearchHit is one session that matched a body-text query, with the first
// matching line snippet (P6.3 remote search, dsh searchAcrossSessions).
type SearchHit struct {
	SessionID string
	UpdatedAt time.Time
	Title     string
	Snippet   string
}

// WorkspaceMeta is the durable metadata of one workspace (P6, dsh workspace
// grouping): a named bucket that sessions are created into. Sort is the
// display order in the sidebar's grouped view (ascending).
type WorkspaceMeta struct {
	ID    string
	Title string
	// Path is the canonical directory backing this workspace. Empty means a
	// legacy title-only workspace and callers should use their default cwd.
	Path string
	Sort int
	// CreatedAt is the workspace creation time (dsh workspace hover card); the
	// zero value means it predates the column and is omitted from the UI.
	CreatedAt time.Time
}

// WorkspacePathStore is the optional path-aware workspace surface. It keeps
// the base Store interface source-compatible with older test doubles while
// allowing the Web composition to persist dsh-style directory workspaces.
type WorkspacePathStore interface {
	CreateWorkspaceWithPath(ctx context.Context, id, title, path string) error
}

// Store is the durable append-only event backend. The agent loop is strictly
// serial (D5), so callers never need their own locking, but implementations
// must not corrupt on concurrent use either.
type Store interface {
	// CreateSession materializes a session row (idempotent). createdAt is
	// recorded verbatim; updatedAt starts at createdAt.
	CreateSession(ctx context.Context, id string, createdAt time.Time) error
	// AppendEvents durably appends events to one session. Events must carry
	// strictly increasing Seq values not already present for that session. A
	// missing session row is materialized on first append.
	AppendEvents(ctx context.Context, sessionID string, events []session.Event) error
	// LoadSession replays all of a session's events in Seq order. It returns
	// ErrNotFound when the session id has no row.
	LoadSession(ctx context.Context, sessionID string) ([]session.Event, error)
	// LoadSessionPage reads one bounded event window. before and after are
	// exclusive sequence cursors; zero cursors select the newest page.
	LoadSessionPage(ctx context.Context, sessionID string, before, after uint64, limit int) (events []session.Event, hasMore bool, err error)
	// ListSessions returns every session's metadata, most recently updated
	// first.
	ListSessions(ctx context.Context) ([]SessionMeta, error)
	// GetSessionMeta returns one session's metadata. ErrNotFound when the id
	// has no row.
	GetSessionMeta(ctx context.Context, sessionID string) (SessionMeta, error)
	// SetSessionTitle stores (or clears) the accepted title for a session and
	// records its producer ("" | session.TitleSourceFallback |
	// session.TitleSourceLLM | session.TitleSourceUser). An empty title clears
	// the stored title and its source and returns to inference. ErrNotFound
	// when the id has no row.
	SetSessionTitle(ctx context.Context, sessionID, title, source string) error
	// MarkSessionViewed records that a session was opened or messaged at the
	// given time, clearing any finished-but-unviewed reminder. ErrNotFound
	// when the id has no row.
	MarkSessionViewed(ctx context.Context, sessionID string, at time.Time) error
	// SetSessionWorkspace moves a session into a workspace; an empty
	// workspaceID returns it to the ungrouped bucket. ErrNotFound when the
	// session id has no row.
	SetSessionWorkspace(ctx context.Context, sessionID, workspaceID string) error
	// ArchiveSession marks a session archived (dsh archive: it leaves the
	// active sidebar list). Unarchive clears the mark. ErrNotFound when the id
	// has no row.
	ArchiveSession(ctx context.Context, sessionID string, archived bool) error
	// ReorderSessions applies a manual order: every listed session is moved
	// into workspaceID (empty = ungrouped) and assigned sort 0..n-1 in list
	// order, so the grouped sidebar follows the drag. The group's other
	// sessions keep their sort untouched.
	ReorderSessions(ctx context.Context, workspaceID string, sessionIDs []string) error
	// ReorderSessionsFlat applies a manual order for the flat view: every
	// listed session takes flat_sort 0..n-1 in list order (workspace
	// membership is untouched).
	ReorderSessionsFlat(ctx context.Context, sessionIDs []string) error
	// SearchSessions finds sessions whose event bodies contain q (case- and
	// width-insensitive substring), returning one hit per session with the
	// first matching snippet, most recently updated first.
	SearchSessions(ctx context.Context, q string) ([]SearchHit, error)
	// DeleteSession removes a session and all of its events. ErrNotFound when
	// the id has no row.
	DeleteSession(ctx context.Context, sessionID string) error

	// CreateWorkspace materializes a workspace row (idempotent). Sort is
	// appended at the end of the current order.
	CreateWorkspace(ctx context.Context, id, title string) error
	// ListWorkspaces returns every workspace's metadata, ordered by Sort then
	// creation.
	ListWorkspaces(ctx context.Context) ([]WorkspaceMeta, error)
	// SetWorkspaceTitle stores a workspace's title. ErrNotFound when the id
	// has no row.
	SetWorkspaceTitle(ctx context.Context, id, title string) error
	// ReorderWorkspaces applies a manual order: sort is rewritten 0..n-1 in
	// list order.
	ReorderWorkspaces(ctx context.Context, ids []string) error
	// DeleteWorkspace removes a workspace; its sessions return to the
	// ungrouped bucket. ErrNotFound when the id has no row.
	DeleteWorkspace(ctx context.Context, id string) error

	// GetSettings returns every persisted runtime setting (key → value). These
	// back the General-settings rows (Agent preset / permission preset /
	// default terminal) and are applied at startup by the composition root.
	GetSettings(ctx context.Context) (map[string]string, error)
	// SetSetting stores one runtime setting, replacing any previous value.
	SetSetting(ctx context.Context, key, value string) error
	// DeleteSetting removes one runtime setting row (no-op when absent).
	DeleteSetting(ctx context.Context, key string) error

	// Close releases the backend's resources.
	Close() error
}
