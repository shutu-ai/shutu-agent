// SQLite is the durable Store backend built on modernc.org/sqlite (pure Go,
// CGO-free; design.md §9). All sessions live in a single database file
// (dispatch-m2 §1: data/pa.db): a sessions table holding one row per session
// and an events table holding one row per appended event. Event rows store the
// type as TEXT, the payload as a JSON TEXT blob, and a version integer, so a
// new event type or payload version never requires migrating old rows (D8).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jabing/shutu-agent/internal/session"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    NOT NULL PRIMARY KEY,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    title      TEXT,
    cwd        TEXT
);
CREATE TABLE IF NOT EXISTS events (
    session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    type       TEXT    NOT NULL,
    version    INTEGER NOT NULL,
    at         INTEGER NOT NULL,
    data       TEXT    NOT NULL,
    PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_events_session ON events (session_id, seq);
CREATE TABLE IF NOT EXISTS workspaces (
    id    TEXT    NOT NULL PRIMARY KEY,
    title TEXT    NOT NULL,
    sort  INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS message_feedback (
    session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    rating     TEXT    NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_message_feedback_session ON message_feedback (session_id, seq);
`

// migrateSchema brings older databases forward. CREATE TABLE IF NOT EXISTS
// never alters an existing table, so columns are added here; a "duplicate
// column" error simply means the database is already current and is ignored.
func migrateSchema(db *sql.DB) error {
	steps := []struct{ table, col, ddl string }{
		{"sessions", "title", `ALTER TABLE sessions ADD COLUMN title TEXT`},
		{"sessions", "title_source", `ALTER TABLE sessions ADD COLUMN title_source TEXT`},
		{"sessions", "workspace_id", `ALTER TABLE sessions ADD COLUMN workspace_id TEXT`},
		{"sessions", "cwd", `ALTER TABLE sessions ADD COLUMN cwd TEXT`},
		{"sessions", "archived_at", `ALTER TABLE sessions ADD COLUMN archived_at INTEGER`},
		{"sessions", "sort", `ALTER TABLE sessions ADD COLUMN sort INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "flat_sort", `ALTER TABLE sessions ADD COLUMN flat_sort INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "last_viewed_at", `ALTER TABLE sessions ADD COLUMN last_viewed_at INTEGER`},
		{"sessions", "agent_preset", `ALTER TABLE sessions ADD COLUMN agent_preset TEXT`},
		{"sessions", "model", `ALTER TABLE sessions ADD COLUMN model TEXT`},
		{"sessions", "permission", `ALTER TABLE sessions ADD COLUMN permission TEXT`},
		{"sessions", "provider", `ALTER TABLE sessions ADD COLUMN provider TEXT`},
		{"sessions", "reasoning_effort", `ALTER TABLE sessions ADD COLUMN reasoning_effort TEXT`},
		{"workspaces", "created_at", `ALTER TABLE workspaces ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`},
	}
	for _, st := range steps {
		if _, err := db.Exec(st.ddl); err != nil {
			// modernc reports duplicate column as an error; any failure other
			// than "column already exists" is fatal.
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") &&
				!strings.Contains(strings.ToLower(err.Error()), "already exists") {
				return fmt.Errorf("store: migrate %s.%s: %w", st.table, st.col, err)
			}
		}
	}
	// Pre-title-source rows: before the session-title alignment the `title`
	// column was written only by an explicit rename (the sidebar's PATCH), so a
	// non-empty legacy title is a user pin. Mark it so automatic revisions never
	// overwrite a pre-existing rename.
	if _, err := db.Exec(`UPDATE sessions SET title_source = 'user'
		WHERE title IS NOT NULL AND title <> ''
		AND (title_source IS NULL OR title_source = '')`); err != nil {
		return fmt.Errorf("store: migrate legacy title pins: %w", err)
	}
	return nil
}

// SQLiteStore implements Store on one SQLite database file.
type SQLiteStore struct {
	db *sql.DB
}

func workingDirectory() any {
	cwd, err := os.Getwd()
	if err != nil || strings.TrimSpace(cwd) == "" {
		return nil
	}
	return cwd
}

// OpenSQLite opens (creating if needed) the SQLite database at path, applies
// the schema, and returns a ready store. The parent directory is created when
// missing. Time values are stored as Unix nanoseconds (UTC) in INTEGER columns.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection keeps SQLite serialized and makes the schema pragmas
	// deterministic; the agent loop is strictly serial anyway (D5).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pragma foreign_keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// CreateSession inserts the session row, keeping any existing row untouched.
func (s *SQLiteStore) CreateSession(ctx context.Context, id string, createdAt time.Time) error {
	now := unixNano(createdAt)
	cwd := workingDirectory()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
		id, now, now, cwd); err != nil {
		return fmt.Errorf("store: create session %q: %w", id, err)
	}
	return nil
}

// GetSessionConfig reads a session's per-session overrides (empty values mean
// "fall back to the global config"). A row with no columns set returns zeros.
func (s *SQLiteStore) GetSessionConfig(ctx context.Context, sessionID string) (SessionConfig, error) {
	var cfg SessionConfig
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(agent_preset,''), COALESCE(provider,''), COALESCE(model,''), COALESCE(reasoning_effort,''), COALESCE(permission,'') FROM sessions WHERE id = ?`,
		sessionID).Scan(&cfg.AgentPreset, &cfg.Provider, &cfg.Model, &cfg.ReasoningEffort, &cfg.Permission)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionConfig{}, fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return SessionConfig{}, fmt.Errorf("store: get session config %q: %w", sessionID, err)
	}
	return cfg, nil
}

// SetSessionConfig writes the full override set (used at session creation and
// to lock the mode). An empty field clears the override back to global.
func (s *SQLiteStore) SetSessionConfig(ctx context.Context, sessionID string, cfg SessionConfig) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET agent_preset = ?, provider = ?, model = ?, reasoning_effort = ?, permission = ? WHERE id = ?`,
		cfg.AgentPreset, cfg.Provider, cfg.Model, cfg.ReasoningEffort, cfg.Permission, sessionID)
	if err != nil {
		return fmt.Errorf("store: set session config %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// UpdateSessionConfig rewrites provider, model, reasoning effort and permission
// (mode stays locked).
func (s *SQLiteStore) UpdateSessionConfig(ctx context.Context, sessionID, provider, model, reasoningEffort, permission string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET provider = ?, model = ?, reasoning_effort = ?, permission = ? WHERE id = ?`,
		provider, model, reasoningEffort, permission, sessionID)
	if err != nil {
		return fmt.Errorf("store: update session config %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// ListMessageFeedback returns all ratings for one session in assistant-event
// sequence order. A missing session is reported as ErrNotFound.
func (s *SQLiteStore) ListMessageFeedback(ctx context.Context, sessionID string) ([]MessageFeedback, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, seq, rating, note, created_at, updated_at
		FROM message_feedback WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list message feedback %q: %w", sessionID, err)
	}
	defer rows.Close()
	feedback := make([]MessageFeedback, 0)
	for rows.Next() {
		var item MessageFeedback
		var seq, createdAt, updatedAt int64
		if err := rows.Scan(&item.SessionID, &seq, &item.Rating, &item.Note, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan message feedback: %w", err)
		}
		item.Seq = uint64(seq)
		item.CreatedAt = time.Unix(0, createdAt).UTC()
		item.UpdatedAt = time.Unix(0, updatedAt).UTC()
		feedback = append(feedback, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read message feedback: %w", err)
	}
	return feedback, nil
}

// GetMessageFeedback reads one rating. The bool is false when the session
// exists but this assistant event has not been rated yet.
func (s *SQLiteStore) GetMessageFeedback(ctx context.Context, sessionID string, seq uint64) (MessageFeedback, bool, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return MessageFeedback{}, false, err
	}
	var item MessageFeedback
	var storedSeq, createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT session_id, seq, rating, note, created_at, updated_at
		FROM message_feedback WHERE session_id = ? AND seq = ?`, sessionID, seq).
		Scan(&item.SessionID, &storedSeq, &item.Rating, &item.Note, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageFeedback{}, false, nil
	}
	if err != nil {
		return MessageFeedback{}, false, fmt.Errorf("store: get message feedback %q/%d: %w", sessionID, seq, err)
	}
	item.Seq = uint64(storedSeq)
	item.CreatedAt = time.Unix(0, createdAt).UTC()
	item.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return item, true, nil
}

// PutMessageFeedback creates or replaces one rating for an assistant event.
func (s *SQLiteStore) PutMessageFeedback(ctx context.Context, sessionID string, seq uint64, rating, note string) (MessageFeedback, error) {
	if rating != "positive" && rating != "negative" {
		return MessageFeedback{}, fmt.Errorf("store: invalid message feedback rating %q", rating)
	}
	if len([]byte(note)) > MaxMessageFeedbackNoteBytes {
		return MessageFeedback{}, fmt.Errorf("store: message feedback note exceeds %d bytes", MaxMessageFeedbackNoteBytes)
	}
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return MessageFeedback{}, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO message_feedback (session_id, seq, rating, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, seq) DO UPDATE SET
			rating = excluded.rating, note = excluded.note, updated_at = excluded.updated_at`,
		sessionID, seq, rating, note, unixNano(now), unixNano(now)); err != nil {
		return MessageFeedback{}, fmt.Errorf("store: put message feedback %q/%d: %w", sessionID, seq, err)
	}
	item, ok, err := s.GetMessageFeedback(ctx, sessionID, seq)
	if err != nil {
		return MessageFeedback{}, err
	}
	if !ok {
		return MessageFeedback{}, fmt.Errorf("store: message feedback %q/%d disappeared after write", sessionID, seq)
	}
	return item, nil
}

// DeleteMessageFeedback removes one rating. It is idempotent for an existing
// session, matching the toggle-off behavior of the Web button.
func (s *SQLiteStore) DeleteMessageFeedback(ctx context.Context, sessionID string, seq uint64) error {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM message_feedback WHERE session_id = ? AND seq = ?`, sessionID, seq); err != nil {
		return fmt.Errorf("store: delete message feedback %q/%d: %w", sessionID, seq, err)
	}
	return nil
}

func (s *SQLiteStore) ensureSession(ctx context.Context, sessionID string) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	} else if err != nil {
		return fmt.Errorf("store: check session %q: %w", sessionID, err)
	}
	return nil
}

// AppendEvents durably appends events in one transaction: it materializes the
// session row when missing, inserts every event, and touches updated_at.
func (s *SQLiteStore) AppendEvents(ctx context.Context, sessionID string, events []session.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin append: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
		sessionID, unixNano(now), unixNano(now), workingDirectory()); err != nil {
		return fmt.Errorf("store: ensure session %q: %w", sessionID, err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare append: %w", err)
	}
	defer stmt.Close()
	for _, ev := range events {
		if _, err := stmt.ExecContext(ctx, sessionID, ev.Seq, ev.Type, ev.Version, unixNano(ev.At), string(ev.Data)); err != nil {
			return fmt.Errorf("store: append %s seq %d: %w", ev.Type, ev.Seq, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, unixNano(now), sessionID); err != nil {
		return fmt.Errorf("store: touch session %q: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit append: %w", err)
	}
	return nil
}

// LoadSession replays all of a session's events in Seq order. An unknown
// session id yields ErrNotFound.
func (s *SQLiteStore) LoadSession(ctx context.Context, sessionID string) ([]session.Event, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("store: check session %q: %w", sessionID, err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, version, at, data FROM events WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: load session %q: %w", sessionID, err)
	}
	defer rows.Close()
	var events []session.Event
	for rows.Next() {
		var ev session.Event
		var seq, version int64
		var at int64
		var data string
		if err := rows.Scan(&seq, &ev.Type, &version, &at, &data); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev.Seq = uint64(seq)
		ev.Version = int(version)
		ev.At = time.Unix(0, at).UTC()
		ev.Data = []byte(data)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	return events, nil
}

// ListSessions returns every session's metadata, most recently updated first.
// Archived sessions are included (the webserver filters them out of the active
// list); Sort is the manual drag order within the group and FlatSort the
// manual drag order of the flat view.
func (s *SQLiteStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.created_at, s.updated_at, s.title, s.title_source, s.workspace_id, s.cwd, s.archived_at, s.sort, s.flat_sort, s.last_viewed_at, COUNT(e.seq)
		FROM sessions s LEFT JOIN events e ON e.session_id = s.id
		GROUP BY s.id
		ORDER BY s.updated_at DESC, s.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var metas []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var created, updated int64
		var title, titleSource, workspaceID, cwd sql.NullString
		var archived, lastViewed sql.NullInt64
		var count int
		if err := rows.Scan(&m.ID, &created, &updated, &title, &titleSource, &workspaceID, &cwd, &archived, &m.Sort, &m.FlatSort, &lastViewed, &count); err != nil {
			return nil, fmt.Errorf("store: scan session meta: %w", err)
		}
		m.CreatedAt = time.Unix(0, created).UTC()
		m.UpdatedAt = time.Unix(0, updated).UTC()
		m.Title = title.String
		m.TitleSource = titleSource.String
		m.WorkspaceID = workspaceID.String
		m.CWD = cwd.String
		if archived.Valid {
			m.ArchivedAt = time.Unix(0, archived.Int64).UTC()
		}
		if lastViewed.Valid {
			m.LastViewedAt = time.Unix(0, lastViewed.Int64).UTC()
		}
		m.EventCount = count
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read session metas: %w", err)
	}
	return metas, nil
}

// ArchiveSession toggles the archived mark on a session (archived_at NULL
// clears it).
func (s *SQLiteStore) ArchiveSession(ctx context.Context, sessionID string, archived bool) error {
	var stmt string
	var args []any
	if archived {
		stmt = `UPDATE sessions SET archived_at = ? WHERE id = ?`
		args = []any{time.Now().UTC().UnixNano(), sessionID}
	} else {
		stmt = `UPDATE sessions SET archived_at = NULL WHERE id = ?`
		args = []any{sessionID}
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("store: archive session %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// ReorderSessions applies a manual drag order: each listed session moves into
// workspaceID (empty = ungrouped) and takes sort = its index, all in one
// transaction. Sessions of the same group not in the list keep their sort.
func (s *SQLiteStore) ReorderSessions(ctx context.Context, workspaceID string, sessionIDs []string) error {
	var wid any
	if workspaceID != "" {
		wid = workspaceID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reorder sessions: %w", err)
	}
	defer tx.Rollback()
	for i, id := range sessionIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET workspace_id = ?, sort = ? WHERE id = ?`, wid, i, id); err != nil {
			return fmt.Errorf("store: reorder session %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reorder sessions: %w", err)
	}
	return nil
}

// ReorderSessionsFlat applies the flat-view manual order: flat_sort is
// rewritten 0..n-1 in list order, leaving workspace membership untouched.
func (s *SQLiteStore) ReorderSessionsFlat(ctx context.Context, sessionIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reorder flat: %w", err)
	}
	defer tx.Rollback()
	for i, id := range sessionIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET flat_sort = ? WHERE id = ?`, i, id); err != nil {
			return fmt.Errorf("store: reorder flat %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reorder flat: %w", err)
	}
	return nil
}

// SearchSessions finds sessions whose event bodies contain q, returning the
// first matching event's body as a snippet per session, most recently updated
// first. The LIKE is case-insensitive for ASCII (SQLite default) and the query
// is escaped so user %/_ don't act as wildcards.
func (s *SQLiteStore) SearchSessions(ctx context.Context, q string) ([]SearchHit, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.session_id, e.data, s.updated_at, s.title
		FROM events e
		JOIN sessions s ON s.id = e.session_id
		JOIN (
			SELECT session_id, MIN(seq) AS m FROM events
			WHERE data LIKE ? ESCAPE '\'
			GROUP BY session_id
		) m ON m.session_id = e.session_id AND e.seq = m.m
		ORDER BY s.updated_at DESC, e.session_id`,
		"%"+escaped+"%")
	if err != nil {
		return nil, fmt.Errorf("store: search sessions: %w", err)
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var data []byte
		var title sql.NullString
		var updated int64
		if err := rows.Scan(&h.SessionID, &data, &updated, &title); err != nil {
			return nil, fmt.Errorf("store: scan search hit: %w", err)
		}
		h.UpdatedAt = time.Unix(0, updated).UTC()
		h.Title = title.String
		h.Snippet = snippetFromEventData(data)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read search hits: %w", err)
	}
	return hits, nil
}

// SearchSessionsPage returns one bounded page and whether more matching
// sessions remain. Pages keep large histories out of one unbounded result
// allocation; callers decide how many pages to collect.
func (s *SQLiteStore) SearchSessionsPage(ctx context.Context, q string, offset, limit int) ([]SearchHit, bool, error) {
	if offset < 0 || limit < 1 {
		return nil, false, fmt.Errorf("store: invalid search page offset=%d limit=%d", offset, limit)
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.session_id, e.data, s.updated_at, s.title
		FROM events e
		JOIN sessions s ON s.id = e.session_id
		JOIN (
			SELECT session_id, MIN(seq) AS m FROM events
			WHERE data LIKE ? ESCAPE '\'
			GROUP BY session_id
		) m ON m.session_id = e.session_id AND e.seq = m.m
		ORDER BY s.updated_at DESC, e.session_id
		LIMIT ? OFFSET ?`, "%"+escaped+"%", limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("store: search session page: %w", err)
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var data []byte
		var title sql.NullString
		var updated int64
		if err := rows.Scan(&h.SessionID, &data, &updated, &title); err != nil {
			return nil, false, fmt.Errorf("store: scan search page hit: %w", err)
		}
		h.UpdatedAt = time.Unix(0, updated).UTC()
		h.Title = title.String
		h.Snippet = snippetFromEventData(data)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: read search page: %w", err)
	}
	more := len(hits) > limit
	if more {
		hits = hits[:limit]
	}
	return hits, more, nil
}

// snippetFromEventData extracts a readable text line from an event's JSON body
// for search previews (best effort, never returns an error).
func snippetFromEventData(data []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"text", "content", "summary"} {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// SetSessionWorkspace moves a session into a workspace; an empty workspaceID
// returns it to the ungrouped bucket.
func (s *SQLiteStore) SetSessionWorkspace(ctx context.Context, sessionID, workspaceID string) error {
	var wid any
	if workspaceID != "" {
		wid = workspaceID
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET workspace_id = ? WHERE id = ?`, wid, sessionID)
	if err != nil {
		return fmt.Errorf("store: set workspace %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// SetSessionTitle stores (or clears) the accepted title and its producer. An
// empty title clears both columns and returns the session to inference.
func (s *SQLiteStore) SetSessionTitle(ctx context.Context, sessionID, title, source string) error {
	var tv, sv any
	if title != "" {
		tv = title
		sv = source
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, tv, sv, sessionID)
	if err != nil {
		return fmt.Errorf("store: set title %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// GetSessionMeta returns one session's durable metadata. ErrNotFound when the
// id has no row.
func (s *SQLiteStore) GetSessionMeta(ctx context.Context, sessionID string) (SessionMeta, error) {
	var m SessionMeta
	var created, updated int64
	var title, titleSource, workspaceID, cwd sql.NullString
	var archived, lastViewed sql.NullInt64
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.created_at, s.updated_at, s.title, s.title_source, s.workspace_id, s.cwd, s.archived_at, s.sort, s.flat_sort, s.last_viewed_at, COUNT(e.seq)
		FROM sessions s LEFT JOIN events e ON e.session_id = s.id
		WHERE s.id = ?`, sessionID).Scan(
		&m.ID, &created, &updated, &title, &titleSource, &workspaceID, &cwd, &archived, &m.Sort, &m.FlatSort, &lastViewed, &count,
	); err != nil {
		if err == sql.ErrNoRows {
			return SessionMeta{}, fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return SessionMeta{}, fmt.Errorf("store: get session meta %q: %w", sessionID, err)
	}
	m.CreatedAt = time.Unix(0, created).UTC()
	m.UpdatedAt = time.Unix(0, updated).UTC()
	m.Title = title.String
	m.TitleSource = titleSource.String
	m.WorkspaceID = workspaceID.String
	m.CWD = cwd.String
	if archived.Valid {
		m.ArchivedAt = time.Unix(0, archived.Int64).UTC()
	}
	if lastViewed.Valid {
		m.LastViewedAt = time.Unix(0, lastViewed.Int64).UTC()
	}
	m.EventCount = count
	return m, nil
}

// SetSessionCWD records the immutable session-header working directory. The
// setter is mainly useful for importers; normal creation captures it in the
// INSERT path.
func (s *SQLiteStore) SetSessionCWD(ctx context.Context, sessionID, cwd string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET cwd = ? WHERE id = ?`, cwd, sessionID)
	if err != nil {
		return fmt.Errorf("store: set session cwd %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// MarkSessionViewed records that a session was opened or messaged, clearing the
// finished-but-unviewed reminder (dsh status.completed). ErrNotFound when the
// id has no row.
func (s *SQLiteStore) MarkSessionViewed(ctx context.Context, sessionID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_viewed_at = ? WHERE id = ?`, unixNano(at), sessionID)
	if err != nil {
		return fmt.Errorf("store: mark session %q viewed: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// DeleteSession removes the session row; events cascade (ON DELETE CASCADE,
// PRAGMA foreign_keys is ON at open).
func (s *SQLiteStore) DeleteSession(ctx context.Context, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("store: delete session %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	return nil
}

// CreateWorkspace inserts a workspace row (idempotent) at the end of the
// current sort order.
func (s *SQLiteStore) CreateWorkspace(ctx context.Context, id, title string) error {
	var next int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort), -1) + 1 FROM workspaces`).Scan(&next); err != nil {
		return fmt.Errorf("store: next workspace sort: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, title, sort, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`, id, title, next, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("store: create workspace %q: %w", id, err)
	}
	return nil
}

// ListWorkspaces returns every workspace, ordered by Sort then id.
func (s *SQLiteStore) ListWorkspaces(ctx context.Context) ([]WorkspaceMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, sort, created_at FROM workspaces ORDER BY sort, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceMeta
	for rows.Next() {
		var m WorkspaceMeta
		var created int64
		if err := rows.Scan(&m.ID, &m.Title, &m.Sort, &created); err != nil {
			return nil, fmt.Errorf("store: scan workspace: %w", err)
		}
		if created > 0 {
			m.CreatedAt = time.UnixMilli(created).UTC()
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read workspaces: %w", err)
	}
	return out, nil
}

// SetWorkspaceTitle renames a workspace.
func (s *SQLiteStore) SetWorkspaceTitle(ctx context.Context, id, title string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return fmt.Errorf("store: rename workspace %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}

// ReorderWorkspaces applies a manual drag order: sort is rewritten 0..n-1.
func (s *SQLiteStore) ReorderWorkspaces(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reorder workspaces: %w", err)
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET sort = ? WHERE id = ?`, i, id); err != nil {
			return fmt.Errorf("store: reorder workspace %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reorder workspaces: %w", err)
	}
	return nil
}

// DeleteWorkspace removes a workspace; its sessions return to the ungrouped
// bucket (workspace_id cleared) in the same transaction.
func (s *SQLiteStore) DeleteWorkspace(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete workspace: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete workspace %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET workspace_id = NULL WHERE workspace_id = ?`, id); err != nil {
		return fmt.Errorf("store: ungroup workspace %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete workspace: %w", err)
	}
	return nil
}

// GetSettings returns every persisted runtime setting as a key→value map.
func (s *SQLiteStore) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan settings: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting stores one runtime setting, replacing any previous value.
func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value); err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes one runtime setting row; a missing key is a no-op.
func (s *SQLiteStore) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func unixNano(t time.Time) int64 { return t.UnixNano() }
