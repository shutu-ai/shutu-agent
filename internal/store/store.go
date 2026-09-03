// Package store defines the persistence abstraction for the session log
// (design.md D8) and its SQLite backend. The store appends events durably and
// replays them at startup; the in-memory log is always rebuilt from the store,
// never the other way around (D1: history is a derived value).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// ErrNotFound is returned when a session id has no row in the store.
var ErrNotFound = errors.New("store: session not found")

// ErrConflictingReplay reports that an append reached a sequence already
// occupied by different bytes. Repair callers may reload the durable tail and
// recompute a synthetic closure; ordinary appenders must fail closed.
var ErrConflictingReplay = errors.New("store: conflicting replay")

var ErrCredentialNotFound = errors.New("store: credential not found")

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

// SessionHeader is the durable lineage header carried by the reference
// session store. It is separate from the UI-oriented SessionMeta projection.
type SessionHeader struct {
	ID              string
	CreatedAt       time.Time
	CWD             string
	Parent          string
	SeedLength      int
	Origin          string
	DelegationDepth int
	AgentPreset     string
}

// ApprovalRow is the storage-neutral durable projection of one approval
// request. The interact package maps this row to its service-level Request so
// store remains independent from the approval service and import cycles.
type ApprovalRow struct {
	ID         string
	SessionID  string
	CallID     string
	Prompt     string
	ToolName   string
	Args       string
	Questions  []byte
	Answer     string
	Status     string
	CreatedAt  time.Time
	ResolvedAt *time.Time
	ExpiresAt  *time.Time
}

// ApprovalStore is the optional durable approval projection implemented by
// SQLiteStore. It is deliberately separate from Store so lightweight test
// backends remain source-compatible.
type ApprovalStore interface {
	ListApprovalRows(context.Context) ([]ApprovalRow, error)
	CreateApprovalRow(context.Context, ApprovalRow) error
	RestoreApprovalRows(context.Context, []ApprovalRow) error
	ResolveApprovalRow(context.Context, string, string, string, time.Time) error
}

// ApprovalSessionStore is the least-privilege read seam for answerers. A
// caller that is already scoped to one session should not have to materialize
// the process-wide approval projection and remember to filter it afterwards.
// It is optional so older ApprovalStore implementations remain source
// compatible; SQLiteStore implements it.
type ApprovalSessionStore interface {
	ListApprovalRowsForSession(context.Context, string) ([]ApprovalRow, error)
}

// ApprovalProjectionReplacer replaces the complete approval projection during
// cold recovery. The event log is authoritative; a replace operation removes
// rows left behind by a crash between provider mutation and event append.
// It is optional so lightweight ApprovalStore implementations remain valid.
type ApprovalProjectionReplacer interface {
	ReplaceApprovalRows(context.Context, []ApprovalRow) error
}

// ApprovalEventStore commits an approval terminal transition and its audit
// event in one backend transaction. The event is supplied by the caller so
// its sequence, timestamp and canonical payload are exactly the bytes that
// the live session log will project after the commit.
type ApprovalEventStore interface {
	ResolveApprovalAndAppendEvent(ctx context.Context, approvalID, status, answer string, resolvedAt time.Time, sessionID string, event session.Event) error
}

// ApprovalRequestEventStore commits creation of a pending approval and its
// canonical asked event together. Without this seam a crash between provider
// creation and event append leaves an answerable but unreplayable request.
type ApprovalRequestEventStore interface {
	CreateApprovalAndAppendEvent(ctx context.Context, row ApprovalRow, sessionID string, event session.Event) error
}

var ErrApprovalResolved = errors.New("store: approval already resolved")
var ErrApprovalNotFound = errors.New("store: approval not found")

// SessionHeaderStore exposes durable session lineage metadata. Optional
// methods keep older Store test doubles source-compatible.
type SessionLineageStore interface {
	GetSessionHeader(ctx context.Context, sessionID string) (SessionHeader, error)
	SetSessionHeader(ctx context.Context, sessionID string, header SessionHeader) error
}

// SessionCreateEventStore atomically publishes a session header and its seed
// events. It closes the crash window between creating a session row and
// appending a fork/restore seed, while remaining optional for lightweight
// Store test doubles.
type SessionCreateEventStore interface {
	CreateSessionWithEvents(ctx context.Context, id string, createdAt time.Time, header SessionHeader, events []session.Event) error
}

// SessionCreateOptions extends the atomic creation seam with the projections
// required by addressed transports. A nil Config means the backend stores no
// per-session override. The header remains the source of immutable lineage;
// Config.AgentPreset is normalized to that header by the SQLite backend.
type SessionCreateOptions struct {
	Header      SessionHeader
	Title       string
	TitleSource string
	WorkspaceID string
	Config      *SessionConfig
}

// SessionCreateStore atomically publishes a session row, its lineage/sidebar
// metadata, optional runtime config, and initial events. Header.SeedLength is
// the inherited-prefix boundary; initial events after that boundary may carry
// the first lifecycle marker that must be published with the new session.
type SessionCreateStore interface {
	CreateSessionWithOptions(ctx context.Context, id string, createdAt time.Time, options SessionCreateOptions, events []session.Event) error
}

// SessionForkOptions controls the metadata copied into an atomically forked
// session. When InheritParentMetadata is true, zero-valued metadata fields and
// a nil Config inherit the parent row inside the same transaction. Non-empty
// fields are explicit overrides. Keeping the read and publish in one backend
// operation prevents a child from becoming visible with only part of its
// transcript or runtime configuration.
type SessionForkOptions struct {
	InheritParentMetadata bool
	Title                 string
	TitleSource           string
	WorkspaceID           string
	CWD                   string
	Config                *SessionConfig
}

// SessionForkStore atomically publishes a forked session, its closed seed,
// lineage, sidebar metadata, and per-session runtime configuration.
type SessionForkStore interface {
	ForkSessionWithOptions(ctx context.Context, parentID, childID string, boundary uint64, options SessionForkOptions) error
}

// IDReservationStore provides a durable, cross-process uniqueness reservation
// for generated control-plane identifiers. Reservations are intentionally
// separate from the final projection row: a caller may reserve before a
// multi-step publication and safely abandon the token after a later failure.
// The namespace prevents unrelated id domains from colliding.
type IDReservationStore interface {
	ReserveID(ctx context.Context, namespace, id string) (bool, error)
}

// GenerateReservedID returns a candidate directly for compatibility stores,
// or retries candidates until the durable reservation succeeds. Callers use
// this at the identity boundary, before publishing a session or child row.
func GenerateReservedID(ctx context.Context, backend any, namespace string, generate func() (string, error)) (string, error) {
	if generate == nil {
		return "", errors.New("store: id generator is required")
	}
	reservations, ok := backend.(IDReservationStore)
	if !ok {
		return generate()
	}
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id, err := generate()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(id) == "" {
			return "", errors.New("store: id generator returned an empty id")
		}
		claimed, err := reservations.ReserveID(ctx, namespace, id)
		if err != nil {
			return "", err
		}
		if claimed {
			return id, nil
		}
	}
	return "", fmt.Errorf("store: unable to reserve a unique %s id after 32 attempts", namespace)
}

// TeamMemberSessionStore atomically publishes a Team child's durable session
// (including an optional fork seed) together with the lead's member event.
// The member event uses the caller's exact sequence so the live lead log can
// adopt the committed row without a second append window.
type TeamMemberSessionStore interface {
	CreateTeamMemberSession(ctx context.Context, childID string, createdAt time.Time, header SessionHeader, seed []session.Event, rootID string, memberEvent session.Event) error
}

// TeamMemberSessionReservationStore extends TeamMemberSessionStore with the
// stronger publication primitive used by production SQLite. The durable
// member identity reservation and the child/root provisioning receipt commit
// in the same backend transaction; a failed publication therefore does not
// leave an orphan reservation.
type TeamMemberSessionReservationStore interface {
	CreateTeamMemberSessionWithReservation(ctx context.Context, childID string, createdAt time.Time, header SessionHeader, seed []session.Event, rootID string, memberEvent session.Event) error
}

// TeamMemberTransitionStore atomically publishes a provisioning-to-active or
// provisioning/active-to-failed member transition after the child session has
// been created. The transaction verifies the immutable child lineage and the
// prior provisioning fact, so a stale process cannot append a transition for
// another Team or resurrect a terminal identity.
type TeamMemberTransitionStore interface {
	TransitionTeamMember(ctx context.Context, rootID, childID string, memberEvent session.Event) error
}

// SessionRevisionStore exposes a lightweight durable cursor for reconnect and
// snapshot listings without forcing callers to load the entire transcript.
type SessionRevisionStore interface {
	SessionRevision(ctx context.Context, sessionID string) (uint64, error)
}

// SessionRevisionTokenStore exposes a source-qualified opaque change token.
// Sequence numbers are only meaningful inside one session; this token is safe
// for reconnect and cache comparisons across independently backed stores.
type SessionRevisionTokenStore interface {
	SessionRevisionToken(ctx context.Context, sessionID string) (string, error)
}

// ProjectionCacheRow is an opaque, versioned checkpoint for a derived
// session projection. The event log remains authoritative: Revision is the
// highest event included in Payload, and a cache row is usable only when the
// consumer can decode its Version and reconcile the tail from that revision.
// Keeping the payload opaque prevents storage from becoming a second
// projection implementation.
type ProjectionCacheRow struct {
	SessionID string
	Version   int
	Revision  uint64
	Payload   []byte
	UpdatedAt time.Time
}

// SessionProjectionCacheStore is the durable checkpoint seam for projection
// consumers. Implementations reject checkpoints ahead of the committed event
// revision and replace rows atomically; cache failures are recoverable and
// must never change the event-log outcome.
type SessionProjectionCacheStore interface {
	GetProjectionCache(context.Context, string) (ProjectionCacheRow, error)
	PutProjectionCache(context.Context, ProjectionCacheRow) error
	DeleteProjectionCache(context.Context, string) error
}

// SessionFlushStore exposes an explicit backend durability barrier. Stores
// whose normal append path is already synchronous may implement this as a
// file/database sync, but it must still validate the addressed session and
// honor cancellation rather than silently succeeding.
type SessionFlushStore interface {
	Flush(context.Context, string) error
}

// SessionSuffixStore is the optional seek-capable event reader used by
// persistence adapters for reconnect/cursor reads. Implementations return
// seq >= fromSeq in ascending order and at most limit rows, plus whether more
// rows remain. Stores without this seam can still satisfy the public Store
// contract; adapters fall back to a validated full read for them.
type SessionSuffixStore interface {
	LoadSessionFrom(ctx context.Context, sessionID string, fromSeq uint64, limit int) (events []session.Event, hasMore bool, err error)
}

// SessionInspectStore is the optional non-mutating cold-read seam. Stores that
// repair an interrupted live tail from LoadSession implement this method so
// history/search inspection can validate the same bytes without triggering a
// repair write.
type SessionInspectStore interface {
	InspectSession(ctx context.Context, sessionID string) ([]session.Event, error)
}

// SessionRawStore is the live-history seam. It returns committed rows exactly
// as stored, including an open turn tail, without repair or lifecycle
// validation. UI history and fork-boundary code use it to observe in-flight
// work; recovery callers use Store.LoadSession instead.
type SessionRawStore interface {
	LoadSessionRaw(ctx context.Context, sessionID string) ([]session.Event, error)
}

// MultiSessionEventStore atomically appends exact event rows across multiple
// sessions. It is optional because JSONL files cannot provide a transaction
// spanning independent files; callers must retain a recoverable fallback.
type MultiSessionEventStore interface {
	AppendEventsAtomic(context.Context, map[string][]session.Event) error
}

// SessionMaintenanceStore contains administrative durability operations that
// are intentionally outside the hot Store interface. Backups are consistent
// snapshots, integrity checks never rewrite data, and repair is the explicit
// opt-in counterpart of recovery-aware LoadSession.
type SessionMaintenanceStore interface {
	Backup(ctx context.Context, destination string) error
	CheckIntegrity(ctx context.Context) error
	RepairSession(ctx context.Context, sessionID string) error
}

// SessionHeaderStore exposes the small durable header projection used by
// read-only session-query consumers. It is deliberately optional so existing
// Store implementations and test doubles remain source-compatible.
type SessionHeaderStore interface {
	SetSessionCWD(ctx context.Context, sessionID, cwd string) error
}

// CredentialRecord is the persistence-neutral record for a credential held by
// the dedicated secret-store seam. Reference is safe to put in configuration;
// Value must never be copied into the generic settings projection or emitted in
// diagnostics. Generation changes on every rotation/replacement.
type CredentialRecord struct {
	Reference  string
	Value      []byte
	Generation uint64
	Revoked    bool
	UpdatedAt  time.Time
}

// CredentialRecordStore is deliberately separate from Store and settings. A
// production secret backend may replace SQLite with an OS keyring/KMS without
// changing provider or transport code. SQLite implements this seam as the
// local durable fallback, while callers can still fail closed when no backend
// is available.
type CredentialRecordStore interface {
	ListCredentialRecords(context.Context) ([]CredentialRecord, error)
	GetCredentialRecord(context.Context, string) (CredentialRecord, error)
	PutCredentialRecord(context.Context, CredentialRecord) error
	DeleteCredentialRecord(context.Context, string) error
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
