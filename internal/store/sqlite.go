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
	"sort"
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
    cwd        TEXT,
    parent_session TEXT,
    seed_length INTEGER NOT NULL DEFAULT 0,
    origin     TEXT,
    delegation_depth INTEGER NOT NULL DEFAULT 0,
    event_count INTEGER NOT NULL DEFAULT 0
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
    path  TEXT    NOT NULL DEFAULT '',
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
CREATE TABLE IF NOT EXISTS schema_meta (
    name    TEXT NOT NULL PRIMARY KEY,
    version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS approval_requests (
    id           TEXT NOT NULL PRIMARY KEY,
    session_id   TEXT NOT NULL DEFAULT '',
    call_id      TEXT NOT NULL DEFAULT '',
    prompt       TEXT NOT NULL,
    tool_name    TEXT NOT NULL DEFAULT '',
    args         TEXT NOT NULL DEFAULT '',
    questions    TEXT NOT NULL DEFAULT '[]',
    answer       TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    resolved_at  INTEGER,
    expires_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_approval_session ON approval_requests (session_id, id);
CREATE TABLE IF NOT EXISTS id_reservations (
    namespace   TEXT NOT NULL,
    id          TEXT NOT NULL,
    reserved_at INTEGER NOT NULL,
    PRIMARY KEY (namespace, id)
);
CREATE TABLE IF NOT EXISTS credentials (
    reference   TEXT NOT NULL PRIMARY KEY,
    secret      BLOB NOT NULL,
    generation  INTEGER NOT NULL DEFAULT 1,
    revoked     INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS session_projection_cache (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    revision   INTEGER NOT NULL,
    payload    BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id)
);
`

const currentSchemaVersion = 4

// migrateSchema brings older databases forward. CREATE TABLE IF NOT EXISTS
// never alters an existing table, so columns are added here; a "duplicate
// column" error simply means the database is already current and is ignored.
func migrateSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin schema migration: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()
	exec := func(query string, args ...any) error {
		_, err := tx.Exec(query, args...)
		return err
	}
	// Never run an older binary against a database written by a newer one.
	// Updating the marker unconditionally would make that downgrade appear
	// successful while silently ignoring columns or invariants it cannot
	// understand. The schema transaction is also the serialization boundary
	// for two processes opening the same database during deployment.
	var storedVersion int
	err = tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_meta WHERE name = 'store'`).Scan(&storedVersion)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if storedVersion > currentSchemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than supported version %d", storedVersion, currentSchemaVersion)
	}
	steps := []struct{ table, col, ddl string }{
		{"sessions", "title", `ALTER TABLE sessions ADD COLUMN title TEXT`},
		{"sessions", "title_source", `ALTER TABLE sessions ADD COLUMN title_source TEXT`},
		{"sessions", "workspace_id", `ALTER TABLE sessions ADD COLUMN workspace_id TEXT`},
		{"sessions", "cwd", `ALTER TABLE sessions ADD COLUMN cwd TEXT`},
		{"sessions", "parent_session", `ALTER TABLE sessions ADD COLUMN parent_session TEXT`},
		{"sessions", "seed_length", `ALTER TABLE sessions ADD COLUMN seed_length INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "origin", `ALTER TABLE sessions ADD COLUMN origin TEXT`},
		{"sessions", "delegation_depth", `ALTER TABLE sessions ADD COLUMN delegation_depth INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "archived_at", `ALTER TABLE sessions ADD COLUMN archived_at INTEGER`},
		{"sessions", "sort", `ALTER TABLE sessions ADD COLUMN sort INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "flat_sort", `ALTER TABLE sessions ADD COLUMN flat_sort INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "last_viewed_at", `ALTER TABLE sessions ADD COLUMN last_viewed_at INTEGER`},
		{"sessions", "event_count", `ALTER TABLE sessions ADD COLUMN event_count INTEGER NOT NULL DEFAULT 0`},
		{"sessions", "agent_preset", `ALTER TABLE sessions ADD COLUMN agent_preset TEXT`},
		{"sessions", "model", `ALTER TABLE sessions ADD COLUMN model TEXT`},
		{"sessions", "permission", `ALTER TABLE sessions ADD COLUMN permission TEXT`},
		{"sessions", "provider", `ALTER TABLE sessions ADD COLUMN provider TEXT`},
		{"sessions", "reasoning_effort", `ALTER TABLE sessions ADD COLUMN reasoning_effort TEXT`},
		{"workspaces", "created_at", `ALTER TABLE workspaces ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`},
		{"workspaces", "path", `ALTER TABLE workspaces ADD COLUMN path TEXT NOT NULL DEFAULT ''`},
	}
	// The reservation table was introduced after schema version 1. Keep this
	// explicit for databases opened by an older binary; the top-level schema
	// string only creates it for new databases.
	if err := exec(`CREATE TABLE IF NOT EXISTS id_reservations (
		namespace TEXT NOT NULL,
		id TEXT NOT NULL,
		reserved_at INTEGER NOT NULL,
		PRIMARY KEY (namespace, id)
	)`); err != nil {
		return fmt.Errorf("store: migrate id reservations: %w", err)
	}
	// Credential values are intentionally not part of the generic settings
	// table. Keep this table creation explicit for databases opened by an older
	// binary and preserve the same migration/locking boundary as other durable
	// control-plane state.
	if err := exec(`CREATE TABLE IF NOT EXISTS credentials (
		reference TEXT NOT NULL PRIMARY KEY,
		secret BLOB NOT NULL,
		generation INTEGER NOT NULL DEFAULT 1,
		revoked INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: migrate credentials: %w", err)
	}
	if err := exec(`CREATE TABLE IF NOT EXISTS session_projection_cache (
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		version INTEGER NOT NULL,
		revision INTEGER NOT NULL,
		payload BLOB NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (session_id)
	)`); err != nil {
		return fmt.Errorf("store: migrate projection cache: %w", err)
	}
	for _, st := range steps {
		if err := exec(st.ddl); err != nil {
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
	if err := exec(`UPDATE sessions SET title_source = 'user'
		WHERE title IS NOT NULL AND title <> ''
		AND (title_source IS NULL OR title_source = '')`); err != nil {
		return fmt.Errorf("store: migrate legacy title pins: %w", err)
	}
	if err := exec(`UPDATE sessions SET event_count = (
		SELECT COUNT(*) FROM events WHERE events.session_id = sessions.id
	) WHERE event_count = 0 AND EXISTS (
		SELECT 1 FROM events WHERE events.session_id = sessions.id
	)`); err != nil {
		return fmt.Errorf("store: migrate event counts: %w", err)
	}
	if err := exec(`INSERT INTO schema_meta(name, version) VALUES ('store', ?)
		ON CONFLICT(name) DO UPDATE SET version = excluded.version`, currentSchemaVersion); err != nil {
		return fmt.Errorf("store: record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit schema migration: %w", err)
	}
	commit = true
	return nil
}

// GetSessionHeader returns the immutable creation/lineage metadata plus the
// session's mode preset. CWD is captured at session creation and is not
// inferred from the process that happens to inspect it later.
func (s *SQLiteStore) GetSessionHeader(ctx context.Context, sessionID string) (SessionHeader, error) {
	var h SessionHeader
	var created, seed, depth int64
	err := s.db.QueryRowContext(ctx, `SELECT id, created_at, COALESCE(cwd,''), COALESCE(parent_session,''), COALESCE(seed_length,0), COALESCE(origin,''), COALESCE(delegation_depth,0), COALESCE(agent_preset,'') FROM sessions WHERE id = ?`, sessionID).
		Scan(&h.ID, &created, &h.CWD, &h.Parent, &seed, &h.Origin, &depth, &h.AgentPreset)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionHeader{}, fmt.Errorf("%w: %q", ErrNotFound, sessionID)
	}
	if err != nil {
		return SessionHeader{}, fmt.Errorf("store: get session header %q: %w", sessionID, err)
	}
	h.CreatedAt = time.Unix(0, created).UTC()
	h.SeedLength, h.DelegationDepth = int(seed), int(depth)
	return h, nil
}

// SetSessionHeader updates lineage fields atomically. ID and CreatedAt are
// identity fields and remain owned by CreateSession.
func (s *SQLiteStore) SetSessionHeader(ctx context.Context, sessionID string, header SessionHeader) error {
	return s.withProcessWriteLock(ctx, "session header", func() error {
		result, err := s.db.ExecContext(ctx, `UPDATE sessions SET cwd = ?, parent_session = ?, seed_length = ?, origin = ?, delegation_depth = ?, agent_preset = ? WHERE id = ?`,
			header.CWD, header.Parent, header.SeedLength, header.Origin, header.DelegationDepth, header.AgentPreset, sessionID)
		if err != nil {
			return fmt.Errorf("store: set session header %q: %w", sessionID, err)
		}
		if n, err := result.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return nil
	})
}

// SessionRevision returns the greatest durable event sequence for one session.
// It is intentionally separate from EventCount because forked logs preserve
// their seed sequence and a reconnect cursor must follow sequence identity.
func (s *SQLiteStore) SessionRevision(ctx context.Context, sessionID string) (uint64, error) {
	var revision sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session_id = ?`, sessionID).Scan(&revision); err != nil {
		return 0, fmt.Errorf("store: get session revision %q: %w", sessionID, err)
	}
	if !revision.Valid {
		if err := s.ensureSession(ctx, sessionID); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return uint64(revision.Int64), nil
}

// SessionRevisionToken returns a stable, source-qualified token for the
// current committed session row. It changes on append, repair, or recreation
// without loading the transcript.
func (s *SQLiteStore) SessionRevisionToken(ctx context.Context, sessionID string) (string, error) {
	var created, updated, count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT created_at, updated_at, event_count FROM sessions WHERE id = ?`, sessionID).
		Scan(&created, &updated, &count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return "", fmt.Errorf("store: get session revision token %q: %w", sessionID, err)
	}
	path, err := filepath.Abs(s.path)
	if err != nil {
		path = filepath.Clean(s.path)
	}
	return fmt.Sprintf("sqlite:%s:%s:%d:%d:%d", path, sessionID, created, updated, count), nil
}

// GetProjectionCache returns the last durable checkpoint for one session.
// Missing cache is not an error: callers must fall back to event replay.
func (s *SQLiteStore) GetProjectionCache(ctx context.Context, sessionID string) (ProjectionCacheRow, error) {
	var row ProjectionCacheRow
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT session_id, version, revision, payload, updated_at FROM session_projection_cache WHERE session_id = ?`, sessionID).
		Scan(&row.SessionID, &row.Version, &row.Revision, &row.Payload, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionCacheRow{}, nil
	}
	if err != nil {
		return ProjectionCacheRow{}, fmt.Errorf("store: get projection cache %q: %w", sessionID, err)
	}
	row.UpdatedAt = time.Unix(0, updated).UTC()
	row.Payload = append([]byte(nil), row.Payload...)
	return row, nil
}

// PutProjectionCache replaces a checkpoint only after verifying that its
// revision is already committed. The process lock is shared with event
// appends, so a cache can never claim a future event and can safely lag after
// a crash.
func (s *SQLiteStore) PutProjectionCache(ctx context.Context, row ProjectionCacheRow) error {
	if strings.TrimSpace(row.SessionID) == "" {
		return errors.New("store: projection cache session id is required")
	}
	if row.Version <= 0 {
		return errors.New("store: projection cache version must be positive")
	}
	if len(row.Payload) == 0 {
		return errors.New("store: projection cache payload is required")
	}
	return s.withProcessWriteLock(ctx, "projection cache", func() error {
		if err := s.ensureSession(ctx, row.SessionID); err != nil {
			return err
		}
		var current sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session_id = ?`, row.SessionID).Scan(&current); err != nil {
			return fmt.Errorf("store: read projection cache revision %q: %w", row.SessionID, err)
		}
		currentRevision := uint64(0)
		if current.Valid && current.Int64 > 0 {
			currentRevision = uint64(current.Int64)
		}
		if row.Revision > currentRevision {
			return fmt.Errorf("store: projection cache revision %d is ahead of committed revision %d", row.Revision, currentRevision)
		}
		updated := row.UpdatedAt
		if updated.IsZero() {
			updated = time.Now().UTC()
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO session_projection_cache(session_id, version, revision, payload, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET version=excluded.version, revision=excluded.revision,
			payload=excluded.payload, updated_at=excluded.updated_at
			WHERE excluded.revision >= session_projection_cache.revision
			   OR excluded.version > session_projection_cache.version`,
			row.SessionID, row.Version, row.Revision, row.Payload, updated.UnixNano())
		if err != nil {
			return fmt.Errorf("store: put projection cache %q: %w", row.SessionID, err)
		}
		return nil
	})
}

// DeleteProjectionCache invalidates a checkpoint without touching the event
// log. It is used when a projection schema version is no longer readable.
func (s *SQLiteStore) DeleteProjectionCache(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("store: projection cache session id is required")
	}
	return s.withProcessWriteLock(ctx, "projection cache delete", func() error {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM session_projection_cache WHERE session_id = ?`, sessionID); err != nil {
			return fmt.Errorf("store: delete projection cache %q: %w", sessionID, err)
		}
		return nil
	})
}

// ForkSession copies a complete, closed prefix into a new sequence namespace.
// The copied event sequence is intentionally preserved: the child can report
// the exact seed boundary while its next append continues after that boundary.
func (s *SQLiteStore) ForkSession(ctx context.Context, parentID, childID string, boundary uint64) error {
	return s.ForkSessionWithOptions(ctx, parentID, childID, boundary, SessionForkOptions{
		InheritParentMetadata: true,
	})
}

// SQLiteStore implements Store on one SQLite database file.
type SQLiteStore struct {
	db   *sql.DB
	path string
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// The web UI reads history while the agent is appending live events. Keep a
	// small pool so a long-running read cursor cannot starve control-plane RPCs;
	// WAL lets those readers continue while the append writer commits. The
	// pragmas in the DSN apply to every pooled connection, not only the first one
	// opened by database/sql.
	db.Close()
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	// Schema creation and migration must share the same cross-process lock as
	// event writes. SQLite serializes individual DDL statements, but without a
	// single deployment lock two first-open processes could observe different
	// migration generations or race the schema marker update.
	processLock, err := acquireSQLiteProcessLock(path + ".lock")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: acquire schema lock: %w", err)
	}
	defer processLock.Close()
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
	if err := secureSQLiteFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db, path: path}, nil
}

// secureSQLiteFiles applies the private-file policy to the database and any
// WAL sidecars already materialized during opening/migration. SQLite creates
// sidecars lazily, so this is repeated by the write paths that can create them.
// On Windows chmod is a best-effort ACL-compatible mode hint; the call remains
// useful for Unix deployments where the database otherwise inherits umask.
func secureSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("store: secure database file %s: %w", candidate, err)
		}
	}
	return nil
}

// withProcessWriteLock is the common cross-process serialization boundary for
// SQLite control-plane mutations. Event and approval transactions already
// acquire this lock around their complete transaction; all other durable
// projections use this helper so independent processes share one ordering
// boundary for control-plane updates.
func (s *SQLiteStore) withProcessWriteLock(ctx context.Context, operation string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire %s lock: %w", operation, err)
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return secureSQLiteFiles(s.path)
}

// CheckIntegrity runs SQLite's read-only integrity check. It is deliberately
// separate from OpenSQLite so operators can use it as a health gate without
// confusing a healthy empty database with a loaded session.
func (s *SQLiteStore) CheckIntegrity(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("store: integrity check: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(result), "ok") {
		return nil
	}
	return fmt.Errorf("store: integrity check failed: %s", result)
}

// Flush establishes an explicit SQLite durability barrier. Event writes are
// committed synchronously, while WAL pages may remain in the sidecar; a FULL
// checkpoint makes the committed state visible in the main database and
// verifies that the addressed session still exists.
func (s *SQLiteStore) Flush(ctx context.Context, sessionID string) error {
	return s.withProcessWriteLock(ctx, "flush", func() error {
		if err := s.ensureSession(ctx, sessionID); err != nil {
			return err
		}
		var busy, logPages, checkpointed int
		if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logPages, &checkpointed); err != nil {
			return fmt.Errorf("store: wal checkpoint: %w", err)
		}
		if busy != 0 {
			return fmt.Errorf("store: wal checkpoint remains busy (pages=%d checkpointed=%d)", logPages, checkpointed)
		}
		return nil
	})
}

// Backup creates a consistent SQLite snapshot using VACUUM INTO. The target
// must not already exist and may not be the live database; refusing overwrite
// makes an operator error recoverable and avoids silently destroying a prior
// backup. WAL contents are included by SQLite's snapshot semantics.
func (s *SQLiteStore) Backup(ctx context.Context, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	processLock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire backup lock: %w", err)
	}
	defer processLock.Close()
	destination, err = filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("store: resolve backup path: %w", err)
	}
	source, err := filepath.Abs(s.path)
	if err != nil {
		return fmt.Errorf("store: resolve database path: %w", err)
	}
	if filepath.Clean(destination) == filepath.Clean(source) {
		return errors.New("store: backup destination is the live database")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("store: backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("store: create backup directory: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	if err := secureSQLiteFiles(destination); err != nil {
		return err
	}
	return nil
}

// RepairSession explicitly applies the same interrupted-tail recovery used by
// LoadSession, while keeping the operation visible to maintenance tooling.
func (s *SQLiteStore) RepairSession(ctx context.Context, sessionID string) error {
	if _, err := s.LoadSession(ctx, sessionID); err != nil {
		return fmt.Errorf("store: repair session %q: %w", sessionID, err)
	}
	return nil
}

// CreateSession inserts the session row, keeping any existing row untouched.
func (s *SQLiteStore) CreateSession(ctx context.Context, id string, createdAt time.Time) error {
	return s.withProcessWriteLock(ctx, "session create", func() error {
		now := unixNano(createdAt)
		cwd := workingDirectory()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`,
			id, now, now, cwd); err != nil {
			return fmt.Errorf("store: create session %q: %w", id, err)
		}
		return nil
	})
}

// ReserveID atomically claims a generated identifier in one durable namespace.
// The claim survives process restart and is intentionally never deleted: a
// failed publication can abandon a token without making it reusable in a
// later process, which keeps cross-process generation monotonic in the
// uniqueness sense even when publication is retried.
func (s *SQLiteStore) ReserveID(ctx context.Context, namespace, id string) (bool, error) {
	namespace, id = strings.TrimSpace(namespace), strings.TrimSpace(id)
	if namespace == "" || id == "" {
		return false, errors.New("store: reservation namespace and id are required")
	}
	var reserved bool
	err := s.withProcessWriteLock(ctx, "id reservation", func() error {
		result, err := s.db.ExecContext(ctx, `INSERT INTO id_reservations (namespace, id, reserved_at)
			VALUES (?, ?, ?) ON CONFLICT(namespace, id) DO NOTHING`, namespace, id, time.Now().UnixNano())
		if err != nil {
			return fmt.Errorf("store: reserve %s/%s: %w", namespace, id, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: inspect reservation %s/%s: %w", namespace, id, err)
		}
		reserved = rows == 1
		return nil
	})
	return reserved, err
}

// CreateSessionWithEvents atomically inserts a session's durable header and
// optional seed transcript. It is used by persistence/fork paths where a
// separate CreateSession → SetHeader → Append sequence would expose a
// partially published child after a crash.
func (s *SQLiteStore) CreateSessionWithEvents(ctx context.Context, id string, createdAt time.Time, header SessionHeader, events []session.Event) error {
	return s.CreateSessionWithOptions(ctx, id, createdAt, SessionCreateOptions{Header: header}, events)
}

// CreateSessionWithOptions is the full atomic session-publication primitive.
// It is intentionally optional at the Store interface because lightweight
// adapters may only need CreateSession, but production SQLite callers use it
// whenever a transport needs a session immediately runnable after creation.
func (s *SQLiteStore) CreateSessionWithOptions(ctx context.Context, id string, createdAt time.Time, options SessionCreateOptions, events []session.Event) error {
	return s.withProcessWriteLock(ctx, "session create", func() error {
		header := options.Header
		if id == "" || (header.ID != "" && header.ID != id) {
			return errors.New("store: session id is invalid")
		}
		if header.ID == "" {
			header.ID = id
		}
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		if header.CreatedAt.IsZero() {
			header.CreatedAt = createdAt
		} else {
			createdAt = header.CreatedAt
		}
		if err := validateEventSequence(events); err != nil {
			return fmt.Errorf("store: seed events: %w", err)
		}
		if err := validateSQLiteEventPayloads(events); err != nil {
			return fmt.Errorf("store: seed events: %w", err)
		}
		if header.SeedLength < 0 || header.SeedLength > len(events) {
			return fmt.Errorf("store: seed length %d is outside initial event count %d", header.SeedLength, len(events))
		}
		cwd := any(header.CWD)
		if header.CWD == "" {
			cwd = workingDirectory()
		}
		cfg := SessionConfig{}
		if options.Config != nil {
			cfg = *options.Config
		}
		cfg.AgentPreset = header.AgentPreset
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin session create: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (
			id, created_at, updated_at, title, title_source, workspace_id, cwd,
			parent_session, seed_length, origin, delegation_depth, agent_preset,
			model, permission, provider, reasoning_effort, event_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, unixNano(createdAt), unixNano(createdAt), nullableString(options.Title),
			nullableString(options.TitleSource), nullableString(options.WorkspaceID), cwd,
			header.Parent, header.SeedLength, header.Origin, header.DelegationDepth,
			header.AgentPreset, nullableString(cfg.Model), nullableString(cfg.Permission),
			nullableString(cfg.Provider), nullableString(cfg.ReasoningEffort), len(events)); err != nil {
			return fmt.Errorf("store: create session %q: %w", id, err)
		}
		for _, event := range events {
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
				id, event.Seq, event.Type, event.Version, unixNano(event.At), string(event.Data)); err != nil {
				return fmt.Errorf("store: seed event %s seq %d: %w", event.Type, event.Seq, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit session create: %w", err)
		}
		return nil
	})
}

// ForkSessionWithOptions atomically copies a closed durable prefix and all
// metadata needed to make the child immediately runnable. The parent is read
// through the transaction rather than through the public read methods so a
// concurrent writer cannot produce a child whose title/config belongs to a
// different parent revision than its seed.
func (s *SQLiteStore) ForkSessionWithOptions(ctx context.Context, parentID, childID string, boundary uint64, options SessionForkOptions) error {
	parentID = strings.TrimSpace(parentID)
	childID = strings.TrimSpace(childID)
	if parentID == "" || childID == "" || parentID == childID {
		return fmt.Errorf("store: invalid fork ids")
	}
	return s.withProcessWriteLock(ctx, "session fork", func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin session fork: %w", err)
		}
		defer tx.Rollback()

		var parent SessionHeader
		var createdAt, seedLength, delegationDepth int64
		var parentTitle, parentTitleSource, parentWorkspace, parentCWD sql.NullString
		var parentProvider, parentModel, parentReasoning, parentPermission sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT id, created_at, COALESCE(cwd,''), COALESCE(parent_session,''),
				COALESCE(seed_length,0), COALESCE(origin,''), COALESCE(delegation_depth,0),
				COALESCE(agent_preset,''), COALESCE(title,''), COALESCE(title_source,''),
				COALESCE(workspace_id,''), COALESCE(provider,''), COALESCE(model,''),
				COALESCE(reasoning_effort,''), COALESCE(permission,'')
			FROM sessions WHERE id = ?`, parentID).Scan(
			&parent.ID, &createdAt, &parentCWD, &parent.Parent, &seedLength,
			&parent.Origin, &delegationDepth, &parent.AgentPreset, &parentTitle,
			&parentTitleSource, &parentWorkspace, &parentProvider, &parentModel,
			&parentReasoning, &parentPermission,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrNotFound, parentID)
		}
		if err != nil {
			return fmt.Errorf("store: read fork parent %q: %w", parentID, err)
		}
		parent.CreatedAt = time.Unix(0, createdAt).UTC()
		parent.CWD = parentCWD.String
		parent.SeedLength = int(seedLength)
		parent.DelegationDepth = int(delegationDepth)

		rows, err := tx.QueryContext(ctx, `
			SELECT seq, type, version, at, data FROM events
			WHERE session_id = ? AND seq <= ? ORDER BY seq`, parentID, boundary)
		if err != nil {
			return fmt.Errorf("store: read fork seed %q: %w", parentID, err)
		}
		seed := make([]session.Event, 0)
		for rows.Next() {
			var event session.Event
			var seq, version, at int64
			var data string
			if err := rows.Scan(&seq, &event.Type, &version, &at, &data); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan fork seed %q: %w", parentID, err)
			}
			event.Seq = uint64(seq)
			event.Version = int(version)
			event.At = time.Unix(0, at).UTC()
			event.Data = []byte(data)
			seed = append(seed, event)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("store: read fork seed %q: %w", parentID, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: close fork seed %q: %w", parentID, err)
		}
		if len(seed) != 0 && seed[len(seed)-1].Seq != boundary {
			return fmt.Errorf("store: fork boundary %d does not exist", boundary)
		}
		if err := validateEventSequence(seed); err != nil {
			return fmt.Errorf("store: fork boundary %d has invalid sequence: %w", boundary, err)
		}
		if err := validateSQLiteEventPayloads(seed); err != nil {
			return fmt.Errorf("store: fork seed: %w", err)
		}
		if err := session.ValidateLifecycle(seed); err != nil {
			return fmt.Errorf("store: fork boundary %d is not a closed lifecycle: %w", boundary, err)
		}

		childTitle := ""
		childTitleSource := ""
		childWorkspace := ""
		childCWD := ""
		cfg := SessionConfig{}
		if options.InheritParentMetadata {
			childTitle = parentTitle.String
			childTitleSource = parentTitleSource.String
			childWorkspace = parentWorkspace.String
			childCWD = parentCWD.String
			cfg = SessionConfig{
				AgentPreset:     parent.AgentPreset,
				Provider:        parentProvider.String,
				Model:           parentModel.String,
				ReasoningEffort: parentReasoning.String,
				Permission:      parentPermission.String,
			}
		}
		if options.Title != "" {
			childTitle, childTitleSource = options.Title, options.TitleSource
		}
		if options.WorkspaceID != "" {
			childWorkspace = options.WorkspaceID
		}
		if options.CWD != "" {
			childCWD = options.CWD
		}
		if options.Config != nil {
			cfg = *options.Config
		}

		created := time.Now().UTC()
		childHeader := SessionHeader{
			ID: childID, CreatedAt: created, CWD: childCWD, Parent: parentID,
			SeedLength: len(seed), Origin: "fork", DelegationDepth: parent.DelegationDepth + 1,
			AgentPreset: parent.AgentPreset,
		}
		if childHeader.CWD == "" {
			childHeader.CWD = parent.CWD
		}
		// AgentPreset is immutable lineage state. A caller may override the
		// other runtime settings, but cannot fork a child under a different mode
		// merely by passing a config pointer.
		cfg.AgentPreset = childHeader.AgentPreset
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, created_at, updated_at, title, title_source, workspace_id, cwd,
				parent_session, seed_length, origin, delegation_depth, agent_preset,
				model, permission, provider, reasoning_effort, event_count
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			childID, unixNano(created), unixNano(created), nullableString(childTitle),
			nullableString(childTitleSource), nullableString(childWorkspace), nullableString(childHeader.CWD),
			parentID, len(seed), childHeader.Origin, childHeader.DelegationDepth,
			childHeader.AgentPreset, nullableString(cfg.Model), nullableString(cfg.Permission),
			nullableString(cfg.Provider), nullableString(cfg.ReasoningEffort), len(seed)); err != nil {
			return fmt.Errorf("store: create fork session %q: %w", childID, err)
		}
		for _, event := range seed {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
				childID, event.Seq, event.Type, event.Version, unixNano(event.At), string(event.Data)); err != nil {
				return fmt.Errorf("store: seed fork event %s seq %d: %w", event.Type, event.Seq, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit session fork: %w", err)
		}
		return nil
	})
}

// CreateTeamMemberSession atomically publishes the child session and the
// lead-side provisioning fact. A crash cannot leave a durable child without
// its immutable roster edge, or a roster edge pointing at a child that was
// never created. The caller supplies the root event sequence; a conflicting
// concurrent writer is rejected rather than silently creating a split log.
func (s *SQLiteStore) CreateTeamMemberSession(ctx context.Context, childID string, createdAt time.Time, header SessionHeader, seed []session.Event, rootID string, memberEvent session.Event) error {
	return s.createTeamMemberSession(ctx, childID, createdAt, header, seed, rootID, memberEvent, false)
}

// CreateTeamMemberSessionWithReservation atomically claims the Team member
// identity and publishes the child session plus Lead provisioning receipt.
// The reservation is rolled back with the domain transaction on any failure.
func (s *SQLiteStore) CreateTeamMemberSessionWithReservation(ctx context.Context, childID string, createdAt time.Time, header SessionHeader, seed []session.Event, rootID string, memberEvent session.Event) error {
	return s.createTeamMemberSession(ctx, childID, createdAt, header, seed, rootID, memberEvent, true)
}

func (s *SQLiteStore) createTeamMemberSession(ctx context.Context, childID string, createdAt time.Time, header SessionHeader, seed []session.Event, rootID string, memberEvent session.Event, reserve bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(childID) == "" || strings.TrimSpace(rootID) == "" || childID == rootID {
		return errors.New("store: invalid Team member session ids")
	}
	if header.ID != "" && header.ID != childID {
		return errors.New("store: Team member session header id mismatch")
	}
	if header.ID == "" {
		header.ID = childID
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = createdAt
	} else {
		createdAt = header.CreatedAt
	}
	if header.SeedLength != len(seed) {
		return fmt.Errorf("store: Team seed length %d does not match %d events", header.SeedLength, len(seed))
	}
	if err := validateEventSequence(seed); err != nil {
		return fmt.Errorf("store: Team seed: %w", err)
	}
	if err := validateSQLiteEventPayloads(seed); err != nil {
		return fmt.Errorf("store: Team seed: %w", err)
	}
	if memberEvent.Seq == 0 || memberEvent.Type == "" || memberEvent.Version <= 0 {
		return errors.New("store: invalid Team member event")
	}
	if err := session.ValidateDurableEvent(memberEvent.Type, memberEvent.Data); err != nil {
		return fmt.Errorf("store: invalid Team member event: %w", err)
	}
	var provisioningPayload struct {
		TeamID string `json:"teamId"`
		Member struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		} `json:"member"`
	}
	if err := json.Unmarshal(memberEvent.Data, &provisioningPayload); err != nil ||
		strings.TrimSpace(provisioningPayload.TeamID) == "" || provisioningPayload.Member.ID != childID || provisioningPayload.Member.Phase != "provisioning" {
		return errors.New("store: Team member session requires a provisioning event")
	}
	return s.withProcessWriteLock(ctx, "Team member session", func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin Team member session: %w", err)
		}
		defer tx.Rollback()
		if reserve {
			result, err := tx.ExecContext(ctx, `INSERT INTO id_reservations (namespace, id, reserved_at)
				VALUES (?, ?, ?) ON CONFLICT(namespace, id) DO NOTHING`,
				"team-member:"+provisioningPayload.TeamID, childID, time.Now().UnixNano())
			if err != nil {
				return fmt.Errorf("store: reserve Team member %q: %w", childID, err)
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: inspect Team member reservation %q: %w", childID, err)
			}
			if rows != 1 {
				return fmt.Errorf("store: Team member identity %q is already reserved", childID)
			}
		}
		cwd := any(header.CWD)
		if header.CWD == "" {
			cwd = workingDirectory()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, created_at, updated_at, cwd, parent_session, seed_length, origin, delegation_depth, agent_preset, event_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			childID, unixNano(createdAt), unixNano(createdAt), cwd, header.Parent, header.SeedLength, header.Origin, header.DelegationDepth, header.AgentPreset, len(seed)); err != nil {
			return fmt.Errorf("store: create Team child session %q: %w", childID, err)
		}
		for _, event := range seed {
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
				childID, event.Seq, event.Type, event.Version, unixNano(event.At), string(event.Data)); err != nil {
				return fmt.Errorf("store: seed Team child event %s seq %d: %w", event.Type, event.Seq, err)
			}
		}
		var rootExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, rootID).Scan(&rootExists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: Team root session %q is missing", rootID)
			}
			return fmt.Errorf("store: inspect Team root session %q: %w", rootID, err)
		}
		var rootRevision sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session_id = ?`, rootID).Scan(&rootRevision); err != nil {
			return fmt.Errorf("store: inspect Team root revision %q: %w", rootID, err)
		}
		want := int64(memberEvent.Seq - 1)
		if !rootRevision.Valid {
			if want != 0 {
				return fmt.Errorf("store: Team root revision conflict for %q: want event %d after empty log", rootID, memberEvent.Seq)
			}
		} else if rootRevision.Int64 != want {
			return fmt.Errorf("store: Team root revision conflict for %q: have %d, member event %d", rootID, rootRevision.Int64, memberEvent.Seq)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
			rootID, memberEvent.Seq, memberEvent.Type, memberEvent.Version, unixNano(memberEvent.At), string(memberEvent.Data)); err != nil {
			return fmt.Errorf("store: append Team member event %s seq %d: %w", memberEvent.Type, memberEvent.Seq, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, event_count = event_count + 1 WHERE id = ?`, unixNano(time.Now().UTC()), rootID); err != nil {
			return fmt.Errorf("store: touch Team root session %q: %w", rootID, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit Team member session: %w", err)
		}
		return nil
	})
}

// TransitionTeamMember appends one lifecycle edge for an already provisioned
// Team child and validates the child/root relationship in the same SQLite
// transaction. Runtime Agent publication is necessarily outside storage, but
// the durable active/failed transition cannot be detached from its lineage or
// race a second process's transition.
func (s *SQLiteStore) TransitionTeamMember(ctx context.Context, rootID, childID string, memberEvent session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(rootID) == "" || strings.TrimSpace(childID) == "" || rootID == childID {
		return errors.New("store: invalid Team transition ids")
	}
	if memberEvent.Seq == 0 || memberEvent.Type != session.EventTeamMember || memberEvent.Version <= 0 {
		return errors.New("store: invalid Team transition event")
	}
	if err := session.ValidateDurableEvent(memberEvent.Type, memberEvent.Data); err != nil {
		return fmt.Errorf("store: invalid Team transition event: %w", err)
	}
	var payload struct {
		TeamID string `json:"teamId"`
		Member struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		} `json:"member"`
	}
	if err := json.Unmarshal(memberEvent.Data, &payload); err != nil || strings.TrimSpace(payload.TeamID) == "" || payload.Member.ID != childID {
		return errors.New("store: Team transition member identity mismatch")
	}
	if payload.Member.Phase != "active" && payload.Member.Phase != "failed" {
		return fmt.Errorf("store: invalid Team transition phase %q", payload.Member.Phase)
	}
	return s.withProcessWriteLock(ctx, "Team member transition", func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin Team member transition: %w", err)
		}
		defer tx.Rollback()

		var parent string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_session,'') FROM sessions WHERE id = ?`, childID).Scan(&parent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: Team child session %q is missing", childID)
			}
			return fmt.Errorf("store: inspect Team child session %q: %w", childID, err)
		}
		if parent != rootID {
			return fmt.Errorf("store: Team child %q has parent %q, want %q", childID, parent, rootID)
		}
		var rootExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, rootID).Scan(&rootExists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: Team root session %q is missing", rootID)
			}
			return fmt.Errorf("store: inspect Team root session %q: %w", rootID, err)
		}
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session_id = ?`, rootID).Scan(&maxSeq); err != nil {
			return fmt.Errorf("store: inspect Team root revision %q: %w", rootID, err)
		}
		last := int64(0)
		if maxSeq.Valid {
			last = maxSeq.Int64
		}
		want := int64(memberEvent.Seq)
		if last == want {
			var typ, data string
			var version, at int64
			if err := tx.QueryRowContext(ctx, `SELECT type, version, at, data FROM events WHERE session_id = ? AND seq = ?`, rootID, memberEvent.Seq).Scan(&typ, &version, &at, &data); err != nil {
				return fmt.Errorf("store: inspect existing Team transition: %w", err)
			}
			if typ == memberEvent.Type && int(version) == memberEvent.Version && at == unixNano(memberEvent.At) && data == string(memberEvent.Data) {
				return nil
			}
			return fmt.Errorf("store: conflicting Team transition at sequence %d", memberEvent.Seq)
		}
		if last != want-1 {
			return fmt.Errorf("store: Team root revision conflict for %q: have %d, transition %d", rootID, last, memberEvent.Seq)
		}

		rows, err := tx.QueryContext(ctx, `SELECT data FROM events WHERE session_id = ? AND type = ? ORDER BY seq`, rootID, session.EventTeamMember)
		if err != nil {
			return fmt.Errorf("store: inspect Team member history: %w", err)
		}
		priorPhase := ""
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scan Team member history: %w", err)
			}
			var prior struct {
				Member struct {
					ID    string `json:"id"`
					Phase string `json:"phase"`
				} `json:"member"`
			}
			if json.Unmarshal([]byte(raw), &prior) == nil && prior.Member.ID == childID {
				priorPhase = prior.Member.Phase
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: close Team member history: %w", err)
		}
		if priorPhase == "" || (priorPhase != "provisioning" && priorPhase != "active") {
			return fmt.Errorf("store: Team member %q has no transitionable provisioning state", childID)
		}
		if payload.Member.Phase == "active" && priorPhase != "provisioning" {
			return fmt.Errorf("store: Team member %q is already %s", childID, priorPhase)
		}
		if payload.Member.Phase == "failed" && priorPhase != "provisioning" && priorPhase != "active" {
			return fmt.Errorf("store: Team member %q is already %s", childID, priorPhase)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`, rootID, memberEvent.Seq, memberEvent.Type, memberEvent.Version, unixNano(memberEvent.At), string(memberEvent.Data)); err != nil {
			return fmt.Errorf("store: append Team transition: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, event_count = event_count + 1 WHERE id = ?`, unixNano(time.Now().UTC()), rootID); err != nil {
			return fmt.Errorf("store: touch Team root transition: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit Team member transition: %w", err)
		}
		return nil
	})
}

// ListApprovalRows returns the durable approval projection in stable ID order.
func (s *SQLiteStore) ListApprovalRows(ctx context.Context) ([]ApprovalRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return nil, fmt.Errorf("store: acquire approval read lock: %w", err)
	}
	defer lock.Close()
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at FROM approval_requests ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	defer rows.Close()
	return scanApprovalRows(rows)
}

func scanApprovalRows(rows *sql.Rows) ([]ApprovalRow, error) {
	var result []ApprovalRow
	for rows.Next() {
		var row ApprovalRow
		var created, resolved, expires sql.NullInt64
		var questions string
		if err := rows.Scan(&row.ID, &row.SessionID, &row.CallID, &row.Prompt, &row.ToolName, &row.Args, &questions, &row.Answer, &row.Status, &created, &resolved, &expires); err != nil {
			return nil, fmt.Errorf("store: scan approval: %w", err)
		}
		row.Questions = []byte(questions)
		if created.Valid {
			row.CreatedAt = time.Unix(0, created.Int64).UTC()
		}
		if resolved.Valid {
			value := time.Unix(0, resolved.Int64).UTC()
			row.ResolvedAt = &value
		}
		if expires.Valid {
			value := time.Unix(0, expires.Int64).UTC()
			row.ExpiresAt = &value
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	return result, nil
}

// ListApprovalRowsForSession performs the ownership predicate in SQLite so a
// scoped approval reader does not fetch prompts/args belonging to other
// sessions. The result uses the same stable ID ordering as the unscoped read.
func (s *SQLiteStore) ListApprovalRowsForSession(ctx context.Context, sessionID string) ([]ApprovalRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("store: approval session id is required")
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return nil, fmt.Errorf("store: acquire scoped approval read lock: %w", err)
	}
	defer lock.Close()
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at FROM approval_requests WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list scoped approvals: %w", err)
	}
	defer rows.Close()
	return scanApprovalRows(rows)
}

// CreateApprovalRow durably inserts one pending approval. IDs are allocated by
// the service layer so recovery preserves the exact request identity.
func (s *SQLiteStore) CreateApprovalRow(ctx context.Context, row ApprovalRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval create lock: %w", err)
	}
	defer lock.Close()
	questions := string(row.Questions)
	if questions == "" {
		questions = "[]"
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO approval_requests (id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.SessionID, row.CallID, row.Prompt, row.ToolName, row.Args, questions, row.Answer, row.Status, unixNano(row.CreatedAt), nullableUnixNano(row.ResolvedAt), nullableUnixNano(row.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: create approval %q: %w", row.ID, err)
	}
	return nil
}

// CreateApprovalAndAppendEvent atomically commits the pending approval row and
// its canonical asked event. The event sequence is supplied by the caller so
// the in-memory session can adopt the exact committed row without a second
// append window.
func (s *SQLiteStore) CreateApprovalAndAppendEvent(ctx context.Context, row ApprovalRow, sessionID string, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(sessionID) == "" || event.Seq == 0 || event.Type == "" || event.Version <= 0 {
		return errors.New("store: invalid atomic approval request")
	}
	if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
		return fmt.Errorf("store: approval request event: %w", err)
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval request lock: %w", err)
	}
	defer lock.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin approval request: %w", err)
	}
	defer tx.Rollback()
	questions := string(row.Questions)
	if questions == "" {
		questions = "[]"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approval_requests (id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.SessionID, row.CallID, row.Prompt, row.ToolName, row.Args, questions, row.Answer, row.Status, unixNano(row.CreatedAt), nullableUnixNano(row.ResolvedAt), nullableUnixNano(row.ExpiresAt)); err != nil {
		return fmt.Errorf("store: create approval %q: %w", row.ID, err)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		sessionID, unixNano(now), unixNano(now), workingDirectory()); err != nil {
		return fmt.Errorf("store: ensure approval request session %q: %w", sessionID, err)
	}
	if err := validateAppendSequence(ctx, tx, sessionID, []session.Event{event}); err != nil {
		return fmt.Errorf("store: approval request event sequence: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, event.Seq, event.Type, event.Version, unixNano(event.At), string(event.Data))
	if err != nil {
		return fmt.Errorf("store: append approval request event %s seq %d: %w", event.Type, event.Seq, err)
	}
	appended, err := result.RowsAffected()
	if err != nil || appended != 1 {
		return fmt.Errorf("store: approval request event %s seq %d was not inserted", event.Type, event.Seq)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, event_count = event_count + 1 WHERE id = ?`, unixNano(now), sessionID); err != nil {
		return fmt.Errorf("store: touch approval request session %q: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit approval request: %w", err)
	}
	return secureSQLiteFiles(s.path)
}

// RestoreApprovalRows upserts the durable projection during cold recovery.
// The operation is deliberately a transaction so a partially restored batch
// cannot expose a mixed approval table to another process.
func (s *SQLiteStore) RestoreApprovalRows(ctx context.Context, rows []ApprovalRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval restore lock: %w", err)
	}
	defer lock.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin approval restore: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	for _, row := range rows {
		questions := string(row.Questions)
		if questions == "" {
			questions = "[]"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_requests (id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET session_id=excluded.session_id, call_id=excluded.call_id, prompt=excluded.prompt, tool_name=excluded.tool_name, args=excluded.args, questions=excluded.questions, answer=excluded.answer, status=excluded.status, created_at=excluded.created_at, resolved_at=excluded.resolved_at, expires_at=excluded.expires_at`,
			row.ID, row.SessionID, row.CallID, row.Prompt, row.ToolName, row.Args, questions, row.Answer, row.Status, unixNano(row.CreatedAt), nullableUnixNano(row.ResolvedAt), nullableUnixNano(row.ExpiresAt)); err != nil {
			return fmt.Errorf("store: restore approval %q: %w", row.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit approval restore: %w", err)
	}
	rollback = false
	return nil
}

// ReplaceApprovalRows atomically reconciles the approval projection with the
// session event log. This is stronger than RestoreApprovalRows: stale/orphaned
// rows are removed, which closes the crash window where a provider mutation
// committed but its audit event did not.
func (s *SQLiteStore) ReplaceApprovalRows(ctx context.Context, rows []ApprovalRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval replace lock: %w", err)
	}
	defer lock.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin approval replace: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM approval_requests`); err != nil {
		return fmt.Errorf("store: clear approvals: %w", err)
	}
	for _, row := range rows {
		questions := string(row.Questions)
		if questions == "" {
			questions = "[]"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_requests (id, session_id, call_id, prompt, tool_name, args, questions, answer, status, created_at, resolved_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.SessionID, row.CallID, row.Prompt, row.ToolName, row.Args, questions, row.Answer, row.Status, unixNano(row.CreatedAt), nullableUnixNano(row.ResolvedAt), nullableUnixNano(row.ExpiresAt)); err != nil {
			return fmt.Errorf("store: replace approval %q: %w", row.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit approval replace: %w", err)
	}
	rollback = false
	return nil
}

// ResolveApprovalRow atomically changes one pending approval to a terminal
// status. A compare-and-set update makes duplicate answerers deterministic.
func (s *SQLiteStore) ResolveApprovalRow(ctx context.Context, id, status, answer string, resolvedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval resolve lock: %w", err)
	}
	defer lock.Close()
	result, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status = ?, answer = ?, resolved_at = ? WHERE id = ? AND status = 'pending'`, status, answer, unixNano(resolvedAt), id)
	if err != nil {
		return fmt.Errorf("store: resolve approval %q: %w", id, err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 1 {
		return nil
	}
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM approval_requests WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrApprovalNotFound, id)
		}
		return fmt.Errorf("store: inspect approval %q: %w", id, err)
	}
	return fmt.Errorf("%w: %s", ErrApprovalResolved, id)
}

// ResolveApprovalAndAppendEvent is the crash-safe approval decision path.
// Unlike ResolveApprovalRow followed by AppendEvents, both the compare-and-set
// and the canonical audit fact are committed or rolled back together. This is
// important because replay treats the event stream as authoritative.
func (s *SQLiteStore) ResolveApprovalAndAppendEvent(ctx context.Context, approvalID, status, answer string, resolvedAt time.Time, sessionID string, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(approvalID) == "" || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("store: approval id and session id are required")
	}
	if event.Seq == 0 || event.Type == "" || event.Version <= 0 {
		return fmt.Errorf("store: invalid approval audit event")
	}
	if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
		return fmt.Errorf("store: approval audit event: %w", err)
	}
	processLock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire approval event lock: %w", err)
	}
	defer processLock.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin approval event: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status = ?, answer = ?, resolved_at = ? WHERE id = ? AND status = 'pending'`, status, answer, unixNano(resolvedAt), approvalID)
	if err != nil {
		return fmt.Errorf("store: resolve approval %q: %w", approvalID, err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
		return fmt.Errorf("store: inspect approval resolve %q: %w", approvalID, affectedErr)
	} else if affected != 1 {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM approval_requests WHERE id = ?`, approvalID).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrApprovalNotFound, approvalID)
			}
			return fmt.Errorf("store: inspect approval %q: %w", approvalID, err)
		}
		return fmt.Errorf("%w: %s", ErrApprovalResolved, approvalID)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
		sessionID, unixNano(now), unixNano(now), workingDirectory()); err != nil {
		return fmt.Errorf("store: ensure approval session %q: %w", sessionID, err)
	}
	if err := validateAppendSequence(ctx, tx, sessionID, []session.Event{event}); err != nil {
		return fmt.Errorf("store: approval event sequence: %w", err)
	}
	result, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, event.Seq, event.Type, event.Version, unixNano(event.At), string(event.Data))
	if err != nil {
		return fmt.Errorf("store: append approval event %s seq %d: %w", event.Type, event.Seq, err)
	}
	appended, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect approval event %s seq %d: %w", event.Type, event.Seq, err)
	}
	if appended == 0 {
		var typ, data string
		var version, at int64
		if err := tx.QueryRowContext(ctx, `SELECT type, version, at, data FROM events WHERE session_id = ? AND seq = ?`, sessionID, event.Seq).Scan(&typ, &version, &at, &data); err != nil {
			return fmt.Errorf("store: inspect replay approval event %s seq %d: %w", event.Type, event.Seq, err)
		}
		if typ != event.Type || int(version) != event.Version || at != unixNano(event.At) || data != string(event.Data) {
			return fmt.Errorf("store: conflicting approval replay at sequence %d", event.Seq)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ?, event_count = event_count + 1 WHERE id = ?`, unixNano(now), sessionID); err != nil {
		return fmt.Errorf("store: touch approval session %q: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit approval event: %w", err)
	}
	return secureSQLiteFiles(s.path)
}

func nullableUnixNano(value *time.Time) any {
	if value == nil {
		return nil
	}
	return unixNano(*value)
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
	return s.withProcessWriteLock(ctx, "session config", func() error {
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
	})
}

// UpdateSessionConfig rewrites provider, model, reasoning effort and permission
// (mode stays locked).
func (s *SQLiteStore) UpdateSessionConfig(ctx context.Context, sessionID, provider, model, reasoningEffort, permission string) error {
	return s.withProcessWriteLock(ctx, "session config", func() error {
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
	})
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
	var item MessageFeedback
	err := s.withProcessWriteLock(ctx, "message feedback", func() error {
		if err := s.ensureSession(ctx, sessionID); err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO message_feedback (session_id, seq, rating, note, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id, seq) DO UPDATE SET
				rating = excluded.rating, note = excluded.note, updated_at = excluded.updated_at`,
			sessionID, seq, rating, note, unixNano(now), unixNano(now)); err != nil {
			return fmt.Errorf("store: put message feedback %q/%d: %w", sessionID, seq, err)
		}
		var ok bool
		var err error
		item, ok, err = s.GetMessageFeedback(ctx, sessionID, seq)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("store: message feedback %q/%d disappeared after write", sessionID, seq)
		}
		return nil
	})
	return item, err
}

// DeleteMessageFeedback removes one rating. It is idempotent for an existing
// session, matching the toggle-off behavior of the Web button.
func (s *SQLiteStore) DeleteMessageFeedback(ctx context.Context, sessionID string, seq uint64) error {
	return s.withProcessWriteLock(ctx, "message feedback", func() error {
		if err := s.ensureSession(ctx, sessionID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM message_feedback WHERE session_id = ? AND seq = ?`, sessionID, seq); err != nil {
			return fmt.Errorf("store: delete message feedback %q/%d: %w", sessionID, seq, err)
		}
		return nil
	})
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
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("store: session id is required")
	}
	if err := validateSQLiteEventPayloads(events); err != nil {
		return fmt.Errorf("store: append events: %w", err)
	}
	// Hold a process-level lock for the complete transaction. The database
	// remains the source of truth, while this coordinator prevents two Agent
	// processes from interleaving event writes and makes torn-tail repair and
	// backup boundaries deterministic.
	processLock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire append lock: %w", err)
	}
	defer processLock.Close()
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
		`INSERT OR IGNORE INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare append: %w", err)
	}
	defer stmt.Close()
	if err := validateAppendSequence(ctx, tx, sessionID, events); err != nil {
		return fmt.Errorf("store: append sequence: %w", err)
	}
	appended := 0
	for _, ev := range events {
		result, err := stmt.ExecContext(ctx, sessionID, ev.Seq, ev.Type, ev.Version, unixNano(ev.At), string(ev.Data))
		if err != nil {
			return fmt.Errorf("store: append %s seq %d: %w", ev.Type, ev.Seq, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: inspect append %s seq %d: %w", ev.Type, ev.Seq, err)
		}
		if count == 0 {
			var typ, data string
			var version, at int64
			if err := tx.QueryRowContext(ctx, `SELECT type, version, at, data FROM events WHERE session_id = ? AND seq = ?`, sessionID, ev.Seq).Scan(&typ, &version, &at, &data); err != nil {
				return fmt.Errorf("store: inspect replay %s seq %d: %w", ev.Type, ev.Seq, err)
			}
			if typ != ev.Type || int(version) != ev.Version || at != unixNano(ev.At) || data != string(ev.Data) {
				return fmt.Errorf("%w: store: conflicting replay at sequence %d", ErrConflictingReplay, ev.Seq)
			}
		} else {
			appended++
		}
		if err := applySessionEventProjection(ctx, tx, sessionID, ev); err != nil {
			return err
		}
	}
	if appended > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, event_count = event_count + ? WHERE id = ?`, unixNano(now), appended, sessionID); err != nil {
			return fmt.Errorf("store: touch session %q: %w", sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit append: %w", err)
	}
	if err := secureSQLiteFiles(s.path); err != nil {
		return err
	}
	return nil
}

// applySessionEventProjection keeps the query-facing session metadata as an
// atomic projection of the append-only log. Title is the first native
// projection that must be visible to both event consumers and ListSessions;
// updating it in this transaction prevents a crash between event commit and a
// separate metadata write from creating two different truths.
func applySessionEventProjection(ctx context.Context, tx *sql.Tx, sessionID string, ev session.Event) error {
	if ev.Type != session.EventSessionTitle {
		return nil
	}
	var data struct {
		Title  string `json:"title"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return fmt.Errorf("store: decode session title at sequence %d: %w", ev.Seq, err)
	}
	source := session.TitleSourceFallback
	switch data.Source.Kind {
	case "fallback":
		source = session.TitleSourceFallback
	case "provider":
		source = session.TitleSourceLLM
	case "user":
		source = session.TitleSourceUser
	default:
		return fmt.Errorf("store: invalid session title source %q at sequence %d", data.Source.Kind, ev.Seq)
	}
	var title, titleSource any
	if data.Title != "" {
		title, titleSource = data.Title, source
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, title, titleSource, sessionID); err != nil {
		return fmt.Errorf("store: project session title %q: %w", sessionID, err)
	}
	return nil
}

// AppendEventsAtomic commits event batches for several sessions in one SQLite
// transaction. Event bytes and sequence numbers are caller-owned so live
// session logs can incorporate the same rows with session.AppendAtomic.
func (s *SQLiteStore) AppendEventsAtomic(ctx context.Context, appends map[string][]session.Event) error {
	if len(appends) == 0 {
		return nil
	}
	for id, events := range appends {
		if strings.TrimSpace(id) == "" {
			return errors.New("store: atomic append session id is required")
		}
		if err := validateSQLiteEventPayloads(events); err != nil {
			return fmt.Errorf("store: atomic append %q: %w", id, err)
		}
	}
	lock, err := acquireSQLiteProcessLockContext(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("store: acquire atomic append lock: %w", err)
	}
	defer lock.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin atomic append: %w", err)
	}
	defer tx.Rollback()
	ids := make([]string, 0, len(appends))
	for id := range appends {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO events (session_id, seq, type, version, at, data) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare atomic append: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return errors.New("store: atomic append session id is required")
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`, id, unixNano(now), unixNano(now), workingDirectory()); err != nil {
			return fmt.Errorf("store: ensure atomic session %q: %w", id, err)
		}
		if err := validateAppendSequence(ctx, tx, id, appends[id]); err != nil {
			return fmt.Errorf("store: atomic append %q sequence: %w", id, err)
		}
		appended := 0
		for _, ev := range appends[id] {
			result, err := stmt.ExecContext(ctx, id, ev.Seq, ev.Type, ev.Version, unixNano(ev.At), string(ev.Data))
			if err != nil {
				return fmt.Errorf("store: atomic append %s seq %d: %w", ev.Type, ev.Seq, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: inspect atomic append %s seq %d: %w", ev.Type, ev.Seq, err)
			}
			if count == 0 {
				var typ, data string
				var version, at int64
				if err := tx.QueryRowContext(ctx, `SELECT type, version, at, data FROM events WHERE session_id = ? AND seq = ?`, id, ev.Seq).Scan(&typ, &version, &at, &data); err != nil {
					return fmt.Errorf("store: inspect atomic replay %s seq %d: %w", ev.Type, ev.Seq, err)
				}
				if typ != ev.Type || int(version) != ev.Version || at != unixNano(ev.At) || data != string(ev.Data) {
					return fmt.Errorf("store: conflicting atomic replay at %s sequence %d", id, ev.Seq)
				}
			} else {
				appended++
			}
			if err := applySessionEventProjection(ctx, tx, id, ev); err != nil {
				return err
			}
		}
		if appended > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, event_count = event_count + ? WHERE id = ?`, unixNano(time.Now().UTC()), appended, id); err != nil {
				return fmt.Errorf("store: touch atomic session %q: %w", id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit atomic append: %w", err)
	}
	return secureSQLiteFiles(s.path)
}

// LoadSession replays all of a session's events in Seq order. An unknown
// session id yields ErrNotFound.
func (s *SQLiteStore) LoadSession(ctx context.Context, sessionID string) ([]session.Event, error) {
	var lastConflict error
	for attempt := 0; attempt < 5; attempt++ {
		events, err := s.loadSessionEvents(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if err := validateEventSequence(events); err != nil {
			return nil, fmt.Errorf("store: invalid session sequence %q: %w", sessionID, err)
		}
		repaired, ok := interruptedRecovery(events)
		if !ok {
			if err := session.ValidateLifecycle(events); err != nil {
				return nil, fmt.Errorf("store: invalid session lifecycle %q: %w", sessionID, err)
			}
			return events, nil
		}
		if err := s.AppendEvents(ctx, sessionID, repaired); err != nil {
			if !errors.Is(err, ErrConflictingReplay) {
				return nil, fmt.Errorf("store: repair interrupted session %q: %w", sessionID, err)
			}
			lastConflict = err
			continue
		}
		return s.loadSessionEvents(ctx, sessionID)
	}
	if lastConflict != nil {
		return nil, fmt.Errorf("store: repair interrupted session %q: %w", sessionID, lastConflict)
	}
	return nil, errors.New("store: recovery retry loop exited unexpectedly")
}

// InspectSession reads the committed transcript without applying the
// interrupted-tail recovery performed by LoadSession. It is used by cold
// inspection, cursor repair and audit paths that must not mutate storage.
func (s *SQLiteStore) InspectSession(ctx context.Context, sessionID string) ([]session.Event, error) {
	events, err := s.loadSessionEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if err := validateEventSequence(events); err != nil {
		return nil, fmt.Errorf("store: invalid session sequence %q: %w", sessionID, err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		return nil, fmt.Errorf("store: invalid session lifecycle %q: %w", sessionID, err)
	}
	return events, nil
}

// LoadSessionRaw returns committed rows without the recovery and lifecycle
// validation applied by LoadSession. It is intentionally a narrow live-tail
// seam for UI history and fork inspection.
func (s *SQLiteStore) LoadSessionRaw(ctx context.Context, sessionID string) ([]session.Event, error) {
	return s.loadSessionEvents(ctx, sessionID)
}

func validateEventSequence(events []session.Event) error {
	if len(events) == 0 {
		return nil
	}
	// SQLite has historically accepted both the current one-based sequence
	// namespace and zero-based native test/legacy rows. Preserve the latter on
	// read while still requiring a contiguous namespace from its first row.
	expected := events[0].Seq
	for _, event := range events {
		if event.Seq != expected {
			return fmt.Errorf("sequence %d after %d", event.Seq, expected-1)
		}
		expected++
	}
	return nil
}

// validateSQLiteEventPayloads mirrors the session log's admission boundary.
// SQLite's raw reader intentionally remains capable of inspecting damaged
// rows, but normal create/append paths must never make an unsupported event
// durable and defer the failure until restart.
// validateAppendSequence enforces the durable append cursor while preserving
// exact-batch idempotency. Existing rows may be replayed byte-for-byte, but a
// newly inserted row must extend the current contiguous tail by exactly one.
// Without this check INSERT OR IGNORE would silently accept gaps, duplicate
// positions inside one batch, or a writer racing from a stale in-memory log.
func validateAppendSequence(ctx context.Context, tx *sql.Tx, sessionID string, events []session.Event) error {
	if len(events) == 0 {
		return nil
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			return fmt.Errorf("batch sequence %d is not greater than %d", events[i].Seq, events[i-1].Seq)
		}
	}
	var count, minSeq, maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(seq), MAX(seq) FROM events WHERE session_id = ?`, sessionID).
		Scan(&count, &minSeq, &maxSeq); err != nil {
		return fmt.Errorf("inspect sequence for %q: %w", sessionID, err)
	}
	hasEvents := count.Valid && count.Int64 > 0
	var last uint64
	if hasEvents {
		if !minSeq.Valid || !maxSeq.Valid || maxSeq.Int64 < minSeq.Int64 ||
			maxSeq.Int64-minSeq.Int64+1 != count.Int64 || (minSeq.Int64 != 0 && minSeq.Int64 != 1) {
			return fmt.Errorf("existing sequence for %q is not contiguous", sessionID)
		}
		last = uint64(maxSeq.Int64)
	}
	for _, event := range events {
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM events WHERE session_id = ? AND seq = ?`, sessionID, event.Seq).
			Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("inspect sequence %d for %q: %w", event.Seq, sessionID, err)
		}
		if !hasEvents {
			if event.Seq != 0 && event.Seq != 1 {
				return fmt.Errorf("first new sequence %d, want 0 or 1", event.Seq)
			}
			hasEvents = true
			last = event.Seq
			continue
		}
		if last == ^uint64(0) || event.Seq != last+1 {
			return fmt.Errorf("new sequence %d after %d is not contiguous", event.Seq, last)
		}
		last = event.Seq
	}
	return nil
}

func validateSQLiteEventPayloads(events []session.Event) error {
	for _, event := range events {
		if event.Type == "" || event.Version <= 0 {
			return fmt.Errorf("invalid event %q at seq %d", event.Type, event.Seq)
		}
		if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
			return err
		}
	}
	return nil
}

// validateOrderedEventRows checks the bounded-reader contract without
// materializing the session prefix. Raw inspection intentionally skips this
// helper so operators can still retrieve damaged rows for repair diagnostics.
func validateOrderedEventRows(events []session.Event) error {
	for i, event := range events {
		if event.Type == "" || event.Version <= 0 {
			return fmt.Errorf("invalid event %q at seq %d", event.Type, event.Seq)
		}
		if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
			return err
		}
		if i > 0 && event.Seq != events[i-1].Seq+1 {
			return fmt.Errorf("non-contiguous seq %d after %d", event.Seq, events[i-1].Seq)
		}
	}
	return nil
}

func (s *SQLiteStore) loadSessionEvents(ctx context.Context, sessionID string) ([]session.Event, error) {
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

// interruptedRecovery returns the minimal terminal events needed to close a
// tail that was interrupted after turn/start or step/start. It only repairs a
// structurally valid prefix; malformed nesting remains a hard corruption
// error instead of being hidden by recovery.
func interruptedRecovery(events []session.Event) ([]session.Event, bool) {
	out, err := session.InterruptedTurnClosers(events)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// LoadSessionFrom is the seek-capable suffix primitive used by persistence
// reconnects. Unlike LoadSession it never materializes the prefix before
// fromSeq, and reads at most limit+1 rows to determine hasMore.
func (s *SQLiteStore) LoadSessionFrom(ctx context.Context, sessionID string, fromSeq uint64, limit int) ([]session.Event, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: invalid event suffix limit %d", limit)
	}
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, type, version, at, data FROM events
		WHERE session_id = ? AND seq >= ? ORDER BY seq LIMIT ?`, sessionID, fromSeq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: load event suffix %q: %w", sessionID, err)
	}
	defer rows.Close()
	events := make([]session.Event, 0, limit+1)
	for rows.Next() {
		var ev session.Event
		var seq, version, at int64
		var data string
		if err := rows.Scan(&seq, &ev.Type, &version, &at, &data); err != nil {
			return nil, false, fmt.Errorf("store: scan event suffix: %w", err)
		}
		ev.Seq = uint64(seq)
		ev.Version = int(version)
		ev.At = time.Unix(0, at).UTC()
		ev.Data = []byte(data)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: read event suffix: %w", err)
	}
	if err := validateOrderedEventRows(events); err != nil {
		return nil, false, fmt.Errorf("store: invalid event suffix %q: %w", sessionID, err)
	}
	if len(events) > 0 && fromSeq > 0 && events[0].Seq != fromSeq {
		return nil, false, fmt.Errorf("store: event suffix %q starts at %d, want %d", sessionID, events[0].Seq, fromSeq)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	return events, hasMore, nil
}

// LoadSessionPage returns one bounded, contiguous event window. The newest
// page is returned when both cursors are zero; before walks toward history and
// after walks toward the live tail. The SQL query reads at most limit+1 rows,
// which is important for large trajectory sessions.
func (s *SQLiteStore) LoadSessionPage(ctx context.Context, sessionID string, before, after uint64, limit int) ([]session.Event, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: invalid event page limit %d", limit)
	}
	if before != 0 && after != 0 {
		return nil, false, fmt.Errorf("store: before and after cursors are mutually exclusive")
	}
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return nil, false, err
	}

	query := `SELECT seq, type, version, at, data FROM events WHERE session_id = ? ORDER BY seq DESC LIMIT ?`
	args := []any{sessionID, limit + 1}
	if before != 0 {
		query = `SELECT seq, type, version, at, data FROM events WHERE session_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`
		args = []any{sessionID, before, limit + 1}
	} else if after != 0 {
		query = `SELECT seq, type, version, at, data FROM events WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`
		args = []any{sessionID, after, limit + 1}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: load event page %q: %w", sessionID, err)
	}
	defer rows.Close()
	events := make([]session.Event, 0, limit+1)
	for rows.Next() {
		var ev session.Event
		var seq, version, at int64
		var data string
		if err := rows.Scan(&seq, &ev.Type, &version, &at, &data); err != nil {
			return nil, false, fmt.Errorf("store: scan event page: %w", err)
		}
		ev.Seq = uint64(seq)
		ev.Version = int(version)
		ev.At = time.Unix(0, at).UTC()
		ev.Data = []byte(data)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: read event page: %w", err)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	if before == 0 && after == 0 || before != 0 {
		// The newest/history query is DESC for efficient tail reads; normalize
		// the wire contract to ascending sequence order for the client merger.
		for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
			events[left], events[right] = events[right], events[left]
		}
	}
	if err := validateOrderedEventRows(events); err != nil {
		return nil, false, fmt.Errorf("store: invalid event page %q: %w", sessionID, err)
	}
	if len(events) > 0 && after > 0 && events[0].Seq != after+1 {
		return nil, false, fmt.Errorf("store: event page %q starts at %d after cursor %d", sessionID, events[0].Seq, after)
	}
	return events, hasMore, nil
}

// ListSessions returns every session's metadata, most recently updated first.
// Archived sessions are included (the webserver filters them out of the active
// list); Sort is the manual drag order within the group and FlatSort the
// manual drag order of the flat view.
func (s *SQLiteStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.created_at, s.updated_at, s.title, s.title_source, s.workspace_id, s.cwd, s.archived_at, s.sort, s.flat_sort, s.last_viewed_at, s.event_count
		FROM sessions s
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
	return s.withProcessWriteLock(ctx, "session archive", func() error {
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
	})
}

// ReorderSessions applies a manual drag order: each listed session moves into
// workspaceID (empty = ungrouped) and takes sort = its index, all in one
// transaction. Sessions of the same group not in the list keep their sort.
func (s *SQLiteStore) ReorderSessions(ctx context.Context, workspaceID string, sessionIDs []string) error {
	return s.withProcessWriteLock(ctx, "session reorder", func() error {
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
	})
}

// ReorderSessionsFlat applies the flat-view manual order: flat_sort is
// rewritten 0..n-1 in list order, leaving workspace membership untouched.
func (s *SQLiteStore) ReorderSessionsFlat(ctx context.Context, sessionIDs []string) error {
	return s.withProcessWriteLock(ctx, "flat session reorder", func() error {
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
	})
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
	return s.withProcessWriteLock(ctx, "session workspace", func() error {
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
	})
}

// SetSessionTitle stores (or clears) the accepted title and its producer. An
// empty title clears both columns and returns the session to inference.
func (s *SQLiteStore) SetSessionTitle(ctx context.Context, sessionID, title, source string) error {
	return s.withProcessWriteLock(ctx, "session title", func() error {
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
	})
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
		SELECT id, created_at, updated_at, title, title_source, workspace_id, cwd, archived_at, sort, flat_sort, last_viewed_at, event_count
		FROM sessions
		WHERE id = ?`, sessionID).Scan(
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
	return s.withProcessWriteLock(ctx, "session cwd", func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE sessions SET cwd = ? WHERE id = ?`, cwd, sessionID)
		if err != nil {
			return fmt.Errorf("store: set session cwd %q: %w", sessionID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return nil
	})
}

// MarkSessionViewed records that a session was opened or messaged, clearing the
// finished-but-unviewed reminder (dsh status.completed). ErrNotFound when the
// id has no row.
func (s *SQLiteStore) MarkSessionViewed(ctx context.Context, sessionID string, at time.Time) error {
	return s.withProcessWriteLock(ctx, "session viewed", func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_viewed_at = ? WHERE id = ?`, unixNano(at), sessionID)
		if err != nil {
			return fmt.Errorf("store: mark session %q viewed: %w", sessionID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return nil
	})
}

// DeleteSession removes the session row; events cascade (ON DELETE CASCADE,
// PRAGMA foreign_keys is ON at open).
func (s *SQLiteStore) DeleteSession(ctx context.Context, sessionID string) error {
	return s.withProcessWriteLock(ctx, "session delete", func() error {
		res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
		if err != nil {
			return fmt.Errorf("store: delete session %q: %w", sessionID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, sessionID)
		}
		return nil
	})
}

// CreateWorkspace inserts a workspace row (idempotent) at the end of the
// current sort order.
func (s *SQLiteStore) CreateWorkspace(ctx context.Context, id, title string) error {
	return s.CreateWorkspaceWithPath(ctx, id, title, "")
}

// CreateWorkspaceWithPath inserts a workspace and persists its canonical
// directory. The empty path is retained for legacy callers; the Web layer
// supplies its configured default before calling this method.
func (s *SQLiteStore) CreateWorkspaceWithPath(ctx context.Context, id, title, path string) error {
	return s.withProcessWriteLock(ctx, "workspace create", func() error {
		var next int
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort), -1) + 1 FROM workspaces`).Scan(&next); err != nil {
			return fmt.Errorf("store: next workspace sort: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO workspaces (id, title, path, sort, created_at) VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`, id, title, path, next, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("store: create workspace %q: %w", id, err)
		}
		return nil
	})
}

// ListWorkspaces returns every workspace, ordered by Sort then id.
func (s *SQLiteStore) ListWorkspaces(ctx context.Context) ([]WorkspaceMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, path, sort, created_at FROM workspaces ORDER BY sort, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceMeta
	for rows.Next() {
		var m WorkspaceMeta
		var created int64
		if err := rows.Scan(&m.ID, &m.Title, &m.Path, &m.Sort, &created); err != nil {
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
	return s.withProcessWriteLock(ctx, "workspace title", func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET title = ? WHERE id = ?`, title, id)
		if err != nil {
			return fmt.Errorf("store: rename workspace %q: %w", id, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return nil
	})
}

// ReorderWorkspaces applies a manual drag order: sort is rewritten 0..n-1.
func (s *SQLiteStore) ReorderWorkspaces(ctx context.Context, ids []string) error {
	return s.withProcessWriteLock(ctx, "workspace reorder", func() error {
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
	})
}

// DeleteWorkspace removes a workspace; its sessions return to the ungrouped
// bucket (workspace_id cleared) in the same transaction.
func (s *SQLiteStore) DeleteWorkspace(ctx context.Context, id string) error {
	return s.withProcessWriteLock(ctx, "workspace delete", func() error {
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
	})
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
	return s.withProcessWriteLock(ctx, "setting", func() error {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value); err != nil {
			return fmt.Errorf("store: set setting %q: %w", key, err)
		}
		return nil
	})
}

// DeleteSetting removes one runtime setting row; a missing key is a no-op.
func (s *SQLiteStore) DeleteSetting(ctx context.Context, key string) error {
	return s.withProcessWriteLock(ctx, "setting", func() error {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return fmt.Errorf("store: delete setting %q: %w", key, err)
		}
		return nil
	})
}

// ListCredentialRecords returns the durable credential records without
// exposing them through GetSettings. Callers own the returned byte slices and
// must wipe them when no longer needed.
func (s *SQLiteStore) ListCredentialRecords(ctx context.Context) ([]CredentialRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT reference, secret, generation, revoked, updated_at FROM credentials ORDER BY reference`)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()
	var out []CredentialRecord
	for rows.Next() {
		var record CredentialRecord
		var generation, revoked, updated int64
		if err := rows.Scan(&record.Reference, &record.Value, &generation, &revoked, &updated); err != nil {
			return nil, fmt.Errorf("store: scan credential: %w", err)
		}
		record.Value = append([]byte(nil), record.Value...)
		record.Generation = uint64(generation)
		record.Revoked = revoked != 0
		record.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	return out, nil
}

// GetCredentialRecord reads one record from the dedicated credential table.
func (s *SQLiteStore) GetCredentialRecord(ctx context.Context, reference string) (CredentialRecord, error) {
	var record CredentialRecord
	var generation, revoked, updated int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT reference, secret, generation, revoked, updated_at FROM credentials WHERE reference = ?`, reference).
		Scan(&record.Reference, &record.Value, &generation, &revoked, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CredentialRecord{}, fmt.Errorf("%w: %q", ErrCredentialNotFound, reference)
		}
		return CredentialRecord{}, fmt.Errorf("store: get credential %q: %w", reference, err)
	}
	record.Value = append([]byte(nil), record.Value...)
	record.Generation = uint64(generation)
	record.Revoked = revoked != 0
	record.UpdatedAt = time.Unix(0, updated).UTC()
	return record, nil
}

// PutCredentialRecord atomically replaces one credential record. The value is
// persisted outside the generic settings table and the caller's byte slice is
// never retained by the backend.
func (s *SQLiteStore) PutCredentialRecord(ctx context.Context, record CredentialRecord) error {
	record.Reference = strings.TrimSpace(record.Reference)
	if record.Reference == "" {
		return errors.New("store: credential reference is empty")
	}
	if len(record.Value) == 0 {
		return errors.New("store: credential value is empty")
	}
	if record.Generation == 0 {
		record.Generation = 1
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	secret := append([]byte(nil), record.Value...)
	defer wipeBytes(secret)
	return s.withProcessWriteLock(ctx, "credential", func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO credentials
			(reference, secret, generation, revoked, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(reference) DO UPDATE SET secret = excluded.secret,
			generation = excluded.generation, revoked = excluded.revoked,
			updated_at = excluded.updated_at`,
			record.Reference, secret, record.Generation, boolInt(record.Revoked), record.UpdatedAt.UnixNano())
		if err != nil {
			return fmt.Errorf("store: put credential %q: %w", record.Reference, err)
		}
		return nil
	})
}

// DeleteCredentialRecord logically revokes and removes a credential. The
// overwrite before DELETE reduces residual exposure in the active SQLite page;
// callers that require cryptographic erasure must provide an OS keyring/KMS
// backend through CredentialRecordStore instead of relying on SQLite.
func (s *SQLiteStore) DeleteCredentialRecord(ctx context.Context, reference string) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("store: credential reference is empty")
	}
	return s.withProcessWriteLock(ctx, "credential", func() error {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE credentials SET secret = zeroblob(length(secret)), revoked = 1, updated_at = ? WHERE reference = ?`,
			time.Now().UTC().UnixNano(), reference); err != nil {
			return fmt.Errorf("store: revoke credential %q: %w", reference, err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE reference = ?`, reference); err != nil {
			return fmt.Errorf("store: delete credential %q: %w", reference, err)
		}
		return nil
	})
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func unixNano(t time.Time) int64 { return t.UnixNano() }

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func wipeBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
