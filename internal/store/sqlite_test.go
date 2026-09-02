package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

func openSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSQLiteProjectionCacheIsDurableVersionedAndRevisionBound(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "projection-cache.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendEvents(ctx, "projection-session", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion,
		At: time.UnixMilli(1001), Data: []byte(`{"text":"one"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.GetProjectionCache(ctx, "projection-session"); err != nil {
		t.Fatal(err)
	}
	if err := first.PutProjectionCache(ctx, ProjectionCacheRow{SessionID: "projection-session", Version: 1, Revision: 1, Payload: []byte(`{"state":"one"}`)}); err != nil {
		t.Fatalf("put cache: %v", err)
	}
	if err := first.PutProjectionCache(ctx, ProjectionCacheRow{SessionID: "projection-session", Version: 1, Revision: 2, Payload: []byte(`{"state":"future"}`)}); err == nil {
		t.Fatal("future projection cache revision was accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	row, err := second.GetProjectionCache(ctx, "projection-session")
	if err != nil {
		t.Fatal(err)
	}
	if row.SessionID != "projection-session" || row.Version != 1 || row.Revision != 1 || string(row.Payload) != `{"state":"one"}` {
		t.Fatalf("durable projection cache = %+v", row)
	}
	if err := second.DeleteProjectionCache(ctx, "projection-session"); err != nil {
		t.Fatal(err)
	}
	row, err = second.GetProjectionCache(ctx, "projection-session")
	if err != nil {
		t.Fatal(err)
	}
	if row.SessionID != "" {
		t.Fatalf("deleted projection cache = %+v", row)
	}
}

func TestSQLiteProjectionCacheNeverRegressesAfterLateReconnectWrite(t *testing.T) {
	ctx := context.Background()
	st := openSQLite(t)
	if err := st.AppendEvents(ctx, "projection-monotonic", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, Data: []byte(`{"text":"one"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, Version: session.EventVersion, Data: []byte(`{"text":"two"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutProjectionCache(ctx, ProjectionCacheRow{
		SessionID: "projection-monotonic", Version: 1, Revision: 2, Payload: []byte(`{"state":"new"}`),
	}); err != nil {
		t.Fatalf("put newest cache: %v", err)
	}
	// A delayed reconnect/history reader may finish with an older durable
	// prefix after the newer checkpoint has already committed. It must not
	// roll the cache back; the next exact-revision read must still use the
	// newest checkpoint or rebuild from the event log.
	if err := st.PutProjectionCache(ctx, ProjectionCacheRow{
		SessionID: "projection-monotonic", Version: 1, Revision: 1, Payload: []byte(`{"state":"stale"}`),
	}); err != nil {
		t.Fatalf("put stale cache: %v", err)
	}
	row, err := st.GetProjectionCache(ctx, "projection-monotonic")
	if err != nil {
		t.Fatal(err)
	}
	if row.Revision != 2 || string(row.Payload) != `{"state":"new"}` {
		t.Fatalf("projection cache regressed after late write = %+v", row)
	}
}

func TestSQLiteProjectionCacheConcurrentWritersConvergeToHighestRevision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "projection-concurrent.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLite(path)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	if err := first.AppendEvents(ctx, "projection-concurrent", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, Data: []byte(`{"text":"one"}`)},
		{Seq: 2, Type: session.EventAssistantMessage, Version: session.EventVersion, Data: []byte(`{"text":"two"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- first.PutProjectionCache(ctx, ProjectionCacheRow{
			SessionID: "projection-concurrent", Version: 1, Revision: 1, Payload: []byte(`{"state":"old"}`),
		})
	}()
	go func() {
		<-start
		errs <- second.PutProjectionCache(ctx, ProjectionCacheRow{
			SessionID: "projection-concurrent", Version: 1, Revision: 2, Payload: []byte(`{"state":"new"}`),
		})
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent cache write: %v", err)
		}
	}
	row, err := first.GetProjectionCache(ctx, "projection-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if row.Revision != 2 || string(row.Payload) != `{"state":"new"}` {
		t.Fatalf("concurrent cache writers converged to %+v, want revision 2", row)
	}
}

func TestSQLiteIDReservationIsDurableAndNamespaceScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reservations.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := first.ReserveID(context.Background(), "session", "s-fixed")
	if err != nil || !ok {
		t.Fatalf("first reservation = %v, %v", ok, err)
	}
	ok, err = first.ReserveID(context.Background(), "session", "s-fixed")
	if err != nil || ok {
		t.Fatalf("duplicate reservation = %v, %v", ok, err)
	}
	ok, err = first.ReserveID(context.Background(), "job", "s-fixed")
	if err != nil || !ok {
		t.Fatalf("namespace-scoped reservation = %v, %v", ok, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ok, err = second.ReserveID(context.Background(), "session", "s-fixed")
	if err != nil || ok {
		t.Fatalf("restart duplicate reservation = %v, %v", ok, err)
	}
}

// TestSQLiteAbandonedJobReservationRecoversAcrossRestart models a process exit
// after the durable job identity claim but before its domain receipt. The
// orphan remains non-reusable, while GenerateReservedID skips it on the next
// process and keeps the legacy job namespace alive.
func TestSQLiteAbandonedJobReservationRecoversAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "orphan-job-reservations.db")

	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := first.ReserveID(ctx, "job", "bash-1")
	if err != nil || !claimed {
		t.Fatalf("orphan job reservation = %v, %v", claimed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	next := 0
	id, err := GenerateReservedID(ctx, second, "job", func() (string, error) {
		next++
		return fmt.Sprintf("bash-%d", next), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "bash-2" {
		t.Fatalf("job id after orphan = %q, want bash-2", id)
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	next = 0
	id, err = GenerateReservedID(ctx, third, "job", func() (string, error) {
		next++
		return fmt.Sprintf("bash-%d", next), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "bash-3" {
		t.Fatalf("job id after restart = %q, want bash-3", id)
	}
}

// TestSQLiteMigrationPreservesLegacyDataAndRejectsNewer runs a real v1-style
// database through OpenSQLite. It proves additive columns, reservation tables,
// legacy title classification and event counters migrate without losing rows;
// it also proves a newer database fails closed instead of being downgraded.
func TestSQLiteMigrationPreservesLegacyDataAndRejectsNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
		CREATE TABLE schema_meta (name TEXT PRIMARY KEY, version INTEGER NOT NULL);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			title TEXT
		);
		CREATE TABLE events (
			session_id TEXT NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL,
			version INTEGER NOT NULL, at INTEGER NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY(session_id, seq)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixNano()
	if _, err := raw.Exec(
		`INSERT INTO sessions(id, created_at, updated_at, title) VALUES ('legacy', ?, ?, 'Pinned Title')`,
		at, at,
	); err != nil {
		t.Fatal(err)
	}
	for seq, text := range []string{"one", "two"} {
		if _, err := raw.Exec(
			`INSERT INTO events(session_id, seq, type, version, at, data) VALUES ('legacy', ?, ?, ?, ?, ?)`,
			seq+1, session.EventUserMessage, session.EventVersion, at, fmt.Sprintf(`{"text":%q}`, text),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO schema_meta(name, version) VALUES ('store', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite legacy database: %v", err)
	}
	var version int
	if err := st.db.QueryRow(`SELECT version FROM schema_meta WHERE name='store'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("migrated schema version = %d, want %d", version, currentSchemaVersion)
	}
	var title, titleSource string
	var eventCount int
	if err := st.db.QueryRow(
		`SELECT title, COALESCE(title_source,''), event_count FROM sessions WHERE id='legacy'`,
	).Scan(&title, &titleSource, &eventCount); err != nil {
		t.Fatal(err)
	}
	if title != "Pinned Title" || titleSource != "user" || eventCount != 2 {
		t.Fatalf("migrated legacy row = %q/%q/%d", title, titleSource, eventCount)
	}
	claimed, err := st.ReserveID(context.Background(), "migration", "reserved-once")
	if err != nil || !claimed {
		t.Fatalf("migrated reservation table = %v, %v", claimed, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	newer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newer.Exec(`UPDATE schema_meta SET version=? WHERE name='store'`, currentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("newer schema open err = %v, want unsupported-version failure", err)
	}
}

// TestSQLiteIndependentHandlesAppendWithoutGapsOrDuplicates serializes two
// independently opened SQLite handles through the same process lock and proves
// concurrent writers leave a contiguous, conflict-free event namespace.
func TestSQLiteIndependentHandlesAppendWithoutGapsOrDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi-handle.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateSession(context.Background(), "shared", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Close()
		_ = second.Close()
	}()

	const writers, perWriter = 4, 25
	var next atomic.Int64
	var wg sync.WaitGroup
	// A deployment must allocate and append each sequence under one writer
	// transaction. This mutex models that caller-level reservation boundary so
	// the test exercises two SQLite handles against one shared durable namespace
	// without knowingly submitting out-of-order work.
	var writerMu sync.Mutex
	errs := make(chan error, writers)
	for writer := range writers {
		store := []*SQLiteStore{first, second}[writer%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				writerMu.Lock()
				seq := next.Add(1)
				err := store.AppendEvents(context.Background(), "shared", []session.Event{{
					Seq: uint64(seq), Type: session.EventUserMessage, Version: session.EventVersion,
					At: time.Now().UTC(), Data: []byte(fmt.Sprintf(`{"text":"writer-%d-seq-%d"}`, writer, seq)),
				}})
				writerMu.Unlock()
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	events, err := first.LoadSessionRaw(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != writers*perWriter {
		t.Fatalf("durable events = %d, want %d", len(events), writers*perWriter)
	}
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("durable sequence at %d = %d; want contiguous 1..%d", index, event.Seq, len(events))
		}
	}
}

func TestSQLiteCredentialRecordsAreSeparateFromSettingsAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("openai-secret")
	if err := first.PutCredentialRecord(context.Background(), CredentialRecord{
		Reference: "OPENAI_API_KEY", Value: secret, Generation: 3,
	}); err != nil {
		t.Fatal(err)
	}
	secret[0] = 'X'
	settings, err := first.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range settings {
		if strings.Contains(key, "llm.key") || strings.Contains(value, "openai-secret") {
			t.Fatalf("credential leaked into generic settings: %q=%q", key, value)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	record, err := second.GetCredentialRecord(context.Background(), "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if record.Generation != 3 || string(record.Value) != "openai-secret" || record.Revoked {
		t.Fatalf("credential record = %+v, want generation 3 and original secret", record)
	}
	if err := second.DeleteCredentialRecord(context.Background(), "OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.GetCredentialRecord(context.Background(), "OPENAI_API_KEY"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("credential after delete = %v, want ErrCredentialNotFound", err)
	}
}

func TestSQLiteDatabaseFileUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not authoritative on Windows")
	}
	path := filepath.Join(t.TempDir(), "private.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendEvents(context.Background(), "private", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion,
		At: time.Now().UTC(), Data: []byte(`{"text":"private"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions = %o, want 600", got)
	}
}

func TestSQLiteRejectsUnsupportedEventsBeforeCommit(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	bad := session.Event{
		Seq: 1, Type: "mode/set", Version: session.EventVersion,
		At: time.Now().UTC(), Data: []byte(`{"mode":"code"}`),
	}
	if err := st.AppendEvents(ctx, "reject-before-commit", []session.Event{bad}); err == nil {
		t.Fatal("unsupported event append unexpectedly succeeded")
	}
	if _, err := st.LoadSession(ctx, "reject-before-commit"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected append created a session: %v", err)
	}
	if err := st.CreateSessionWithEvents(ctx, "reject-seed", time.Now().UTC(), SessionHeader{ID: "reject-seed", SeedLength: 1}, []session.Event{bad}); err == nil {
		t.Fatal("unsupported seed unexpectedly succeeded")
	}
	if _, err := st.LoadSession(ctx, "reject-seed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected seed created a session: %v", err)
	}
	unknown := bad
	unknown.Type = "future/required-event"
	unknown.Data = []byte(`{"value":true}`)
	if err := st.AppendEvents(ctx, "reject-unknown", []session.Event{unknown}); !errors.Is(err, session.ErrUnknownRequiredEvent) {
		t.Fatalf("unknown event append = %v, want ErrUnknownRequiredEvent", err)
	}
	if _, err := st.LoadSession(ctx, "reject-unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected unknown append created a session: %v", err)
	}
	if err := st.CreateSessionWithEvents(ctx, "reject-unknown-seed", time.Now().UTC(), SessionHeader{ID: "reject-unknown-seed", SeedLength: 1}, []session.Event{unknown}); !errors.Is(err, session.ErrUnknownRequiredEvent) {
		t.Fatalf("unknown seed = %v, want ErrUnknownRequiredEvent", err)
	}
	if _, err := st.LoadSession(ctx, "reject-unknown-seed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected unknown seed created a session: %v", err)
	}
}

func TestSQLiteAppendRejectsGapsAndDuplicateBatchPositions(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	at := time.Now().UTC()
	first := session.Event{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"one"}`)}
	if err := st.AppendEvents(ctx, "append-cursor", []session.Event{first}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "append-cursor", []session.Event{{
		Seq: 3, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"gap"}`),
	}}); err == nil {
		t.Fatal("sequence gap append unexpectedly succeeded")
	}
	if err := st.AppendEvents(ctx, "append-cursor", []session.Event{
		{Seq: 2, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"two"}`)},
		{Seq: 2, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"two"}`)},
	}); err == nil {
		t.Fatal("duplicate sequence positions in one batch unexpectedly succeeded")
	}
	got, err := st.LoadSessionRaw(ctx, "append-cursor")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("failed appends changed durable cursor: %+v", got)
	}
	if err := st.AppendEvents(ctx, "append-cursor", []session.Event{{
		Seq: 2, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"two"}`),
	}}); err != nil {
		t.Fatalf("contiguous append: %v", err)
	}
}

func TestSQLiteBoundedReadersRejectSequenceGaps(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	log := session.New()
	for _, text := range []string{"one", "two", "three"} {
		if _, err := log.Append(session.EventUserMessage, session.NewUserMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvents(ctx, "bounded-gap", log.Events()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM events WHERE session_id = ? AND seq = ?`, "bounded-gap", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadSessionFrom(ctx, "bounded-gap", 1, 10); err == nil {
		t.Fatal("suffix reader accepted a sequence gap")
	}
	if _, _, err := st.LoadSessionPage(ctx, "bounded-gap", 0, 0, 10); err == nil {
		t.Fatal("page reader accepted a sequence gap")
	}
}

func TestSQLiteMultiSessionAppendIsAtomic(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	at := time.Now().UTC()
	appends := map[string][]session.Event{
		"atomic-left":  {{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"left"}`)}},
		"atomic-right": {{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"right"}`)}},
	}
	if err := st.AppendEventsAtomic(ctx, appends); err != nil {
		t.Fatal(err)
	}
	left, err := st.LoadSessionRaw(ctx, "atomic-left")
	if err != nil || len(left) != 1 {
		t.Fatalf("left atomic rows = %d err=%v", len(left), err)
	}
	right, err := st.LoadSessionRaw(ctx, "atomic-right")
	if err != nil || len(right) != 1 {
		t.Fatalf("right atomic rows = %d err=%v", len(right), err)
	}
	if err := st.AppendEventsAtomic(ctx, map[string][]session.Event{
		"atomic-left":  {{Seq: 2, Type: session.EventStepStart, Version: session.EventVersion, At: at, Data: []byte(`{"step":1}`)}},
		"atomic-right": {{Seq: 2, Type: session.EventStepStart, Version: session.EventVersion, At: at, Data: []byte(`{"step":1}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	// A conflicting row in one batch rolls back the other session's new row.
	if err := st.AppendEventsAtomic(ctx, map[string][]session.Event{
		"atomic-left":  {{Seq: 3, Type: session.EventStepEnd, Version: session.EventVersion, At: at, Data: []byte(`{"step":1}`)}},
		"atomic-right": {{Seq: 2, Type: session.EventStepEnd, Version: session.EventVersion, At: at, Data: []byte(`{"step":1}`)}},
	}); err == nil {
		t.Fatal("conflicting multi-session append unexpectedly succeeded")
	}
	left, _ = st.LoadSessionRaw(ctx, "atomic-left")
	if len(left) != 2 {
		t.Fatalf("left rows after rollback = %d, want 2", len(left))
	}
}

func TestSQLiteSessionCreateWithSeedIsAtomic(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	at := time.Now().UTC()
	err := st.CreateSessionWithEvents(ctx, "seed-atomic", at, SessionHeader{
		ID: "seed-atomic", CreatedAt: at, Origin: "fork", SeedLength: 2,
	}, []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"one"}`)},
		{Seq: 3, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"gap"}`)},
	})
	if err == nil {
		t.Fatal("invalid seed unexpectedly committed")
	}
	if _, err := st.GetSessionMeta(ctx, "seed-atomic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially published seed session err = %v, want ErrNotFound", err)
	}

	if err := st.CreateSessionWithEvents(ctx, "seed-atomic", at, SessionHeader{
		ID: "seed-atomic", CreatedAt: at, Origin: "fork", SeedLength: 1,
	}, []session.Event{{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: at, Data: []byte(`{"text":"one"}`)}}); err != nil {
		t.Fatal(err)
	}
	header, err := st.GetSessionHeader(ctx, "seed-atomic")
	if err != nil || header.Origin != "fork" || header.SeedLength != 1 {
		t.Fatalf("seed header = %+v, err=%v", header, err)
	}
}

func TestSQLiteForkWithOptionsPublishesSeedAndMetadataAtomically(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Nanosecond)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := st.CreateWorkspaceWithPath(ctx, "fork-options-workspace", "Fork options", workspace); err != nil {
		t.Fatal(err)
	}
	seed := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, Version: session.EventVersion, At: at, Data: []byte(`{}`)},
		{Seq: 2, Type: session.EventUserMessage, Version: session.EventVersion, At: at.Add(time.Millisecond), Data: []byte(`{"text":"fork me"}`)},
		{Seq: 3, Type: session.EventTurnEnd, Version: session.EventVersion, At: at.Add(2 * time.Millisecond), Data: []byte(`{}`)},
	}
	if err := st.CreateSessionWithEvents(ctx, "fork-options-parent", at, SessionHeader{
		ID: "fork-options-parent", CreatedAt: at, CWD: workspace, AgentPreset: "code", SeedLength: len(seed),
	}, seed); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionTitle(ctx, "fork-options-parent", "Parent title", session.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionWorkspace(ctx, "fork-options-parent", "fork-options-workspace"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionConfig(ctx, "fork-options-parent", SessionConfig{
		AgentPreset: "code", Provider: "openai", Model: "gpt-4o", ReasoningEffort: "high", Permission: "full",
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.ForkSessionWithOptions(ctx, "fork-options-parent", "fork-options-missing-boundary", 2, SessionForkOptions{InheritParentMetadata: true}); err == nil {
		t.Fatal("fork at a non-event boundary unexpectedly succeeded")
	}
	if _, err := st.GetSessionMeta(ctx, "fork-options-missing-boundary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed fork left a child row: %v", err)
	}

	if err := st.ForkSessionWithOptions(ctx, "fork-options-parent", "fork-options-child", 3, SessionForkOptions{InheritParentMetadata: true}); err != nil {
		t.Fatal(err)
	}
	cloned, err := st.LoadSession(ctx, "fork-options-child")
	if err != nil || len(cloned) != len(seed) {
		t.Fatalf("forked seed = %+v, err=%v", cloned, err)
	}
	meta, err := st.GetSessionMeta(ctx, "fork-options-child")
	if err != nil || meta.Title != "Parent title" || meta.TitleSource != session.TitleSourceUser || meta.WorkspaceID != "fork-options-workspace" || meta.CWD != workspace {
		t.Fatalf("forked metadata = %+v, err=%v", meta, err)
	}
	cfg, err := st.GetSessionConfig(ctx, "fork-options-child")
	if err != nil || cfg.Provider != "openai" || cfg.Model != "gpt-4o" || cfg.ReasoningEffort != "high" || cfg.Permission != "full" || cfg.AgentPreset != "code" {
		t.Fatalf("forked config = %+v, err=%v", cfg, err)
	}
}

func TestSQLiteApprovalDecisionAndAuditAreAtomic(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Nanosecond)
	if err := st.CreateApprovalRow(ctx, ApprovalRow{
		ID: "approval-atomic", SessionID: "approval-session", Prompt: "run", ToolName: "exec",
		Status: "pending", CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	event := session.Event{Seq: 1, Type: session.EventApprovalDecided, Version: session.EventVersion, At: created.Add(time.Second), Data: []byte(`{"id":"approval-atomic","outcome":"allowed-once"}`)}
	if err := st.ResolveApprovalAndAppendEvent(ctx, "approval-atomic", "allowed-once", "", created.Add(2*time.Second), "approval-session", event); err != nil {
		t.Fatalf("atomic resolve: %v", err)
	}
	rows, err := st.ListApprovalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "allowed-once" {
		t.Fatalf("approval rows after commit = %+v", rows)
	}
	events, err := st.LoadSession(ctx, "approval-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != session.EventApprovalDecided {
		t.Fatalf("audit events after commit = %+v", events)
	}

	if err := st.CreateApprovalRow(ctx, ApprovalRow{ID: "approval-conflict", SessionID: "approval-session", Prompt: "conflict", Status: "pending", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	conflicting := event
	conflicting.Data = []byte(`{"id":"different"}`)
	if err := st.ResolveApprovalAndAppendEvent(ctx, "approval-conflict", "rejected", "", created.Add(3*time.Second), "approval-session", conflicting); err == nil {
		t.Fatal("conflicting audit event unexpectedly committed")
	}
	rows, err = st.ListApprovalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "approval-conflict" && row.Status != "pending" {
			t.Fatalf("conflicting approval was not rolled back: %+v", row)
		}
	}
}

func TestSQLiteTeamMemberSessionAndProvisioningEventAreAtomic(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Nanosecond)
	if err := st.CreateSession(ctx, "team-root", created); err != nil {
		t.Fatal(err)
	}
	event := session.Event{Seq: 1, Type: "team/member", Version: session.EventVersion, At: created, Data: []byte(`{"version":1,"teamId":"team-root","member":{"id":"team-root:worker","name":"worker","context":"fresh","phase":"provisioning"}}`)}
	err := st.CreateTeamMemberSession(ctx, "team-root:worker", created, SessionHeader{
		ID: "team-root:worker", CreatedAt: created, Parent: "team-root", Origin: "team", SeedLength: 0,
	}, nil, "team-root", event)
	if err != nil {
		t.Fatal(err)
	}
	header, err := st.GetSessionHeader(ctx, "team-root:worker")
	if err != nil || header.Parent != "team-root" || header.Origin != "team" {
		t.Fatalf("child header = %+v, err=%v", header, err)
	}
	root, err := st.InspectSession(ctx, "team-root")
	if err != nil || len(root) != 1 || root[0].Type != "team/member" {
		t.Fatalf("root member event = %+v, err=%v", root, err)
	}

	failedEvent := event
	failedEvent.Seq = 1
	failedEvent.Data = []byte(`{"version":1,"teamId":"team-root","member":{"id":"team-root:second","name":"second","context":"fresh","phase":"provisioning"}}`)
	if err := st.CreateTeamMemberSession(ctx, "team-root:second", created, SessionHeader{
		ID: "team-root:second", CreatedAt: created, Parent: "team-root", Origin: "team", SeedLength: 1,
	}, []session.Event{{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, At: created, Data: []byte(`{"text":"seed"}`)}}, "team-root", failedEvent); err == nil {
		t.Fatal("invalid Team seed/event transaction unexpectedly committed")
	}
	if _, err := st.GetSessionHeader(ctx, "team-root:second"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially created Team child err=%v, want ErrNotFound", err)
	}
	root, err = st.InspectSession(ctx, "team-root")
	if err != nil || len(root) != 1 {
		t.Fatalf("root after rolled-back Team transaction = %+v, err=%v", root, err)
	}
}

func TestSQLiteTeamMemberReservationAndReceiptShareOneTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-reservation.db")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })

	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Nanosecond)
	if err := first.CreateSession(ctx, "atomic-team-root", created); err != nil {
		t.Fatal(err)
	}
	memberID := "atomic-team-root:worker"
	provisioning := session.Event{
		Seq: 1, Type: session.EventTeamMember, Version: session.EventVersion, At: created,
		Data: []byte(`{"version":1,"teamId":"atomic-team-root","member":{"id":"atomic-team-root:worker","name":"worker","context":"fresh","phase":"provisioning"}}`),
	}
	if err := first.CreateTeamMemberSessionWithReservation(ctx, memberID, created, SessionHeader{
		ID: memberID, CreatedAt: created, Parent: "atomic-team-root", Origin: "team", SeedLength: 0,
	}, nil, "atomic-team-root", provisioning); err != nil {
		t.Fatal(err)
	}
	claimed, err := second.ReserveID(ctx, "team-member:atomic-team-root", memberID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("the domain receipt did not retain its member identity reservation")
	}

	receipt, err := first.InspectSession(ctx, "atomic-team-root")
	if err != nil || len(receipt) != 1 || string(receipt[0].Data) != string(provisioning.Data) {
		t.Fatalf("receipt = %+v, err=%v", receipt, err)
	}

	duplicate := provisioning
	duplicate.Seq = 2
	if err := second.CreateTeamMemberSessionWithReservation(ctx, memberID, created, SessionHeader{
		ID: memberID, CreatedAt: created, Parent: "atomic-team-root", Origin: "team", SeedLength: 0,
	}, nil, "atomic-team-root", duplicate); err == nil {
		t.Fatal("duplicate Team member reservation unexpectedly committed")
	}
	receipt, err = first.InspectSession(ctx, "atomic-team-root")
	if err != nil || len(receipt) != 1 {
		t.Fatalf("receipt after duplicate reservation = %+v, err=%v", receipt, err)
	}
}

func TestSQLiteTeamMemberReservationRollsBackWhenReceiptFails(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Nanosecond)
	rootID, childID := "rollback-team-root", "rollback-team-root:worker"
	if err := st.CreateSession(ctx, rootID, created); err != nil {
		t.Fatal(err)
	}
	// The reservation is inserted before the child session. A pre-existing
	// child therefore forces failure after the reservation claim and proves
	// that the transaction cannot strand the identity.
	if err := st.CreateSession(ctx, childID, created); err != nil {
		t.Fatal(err)
	}
	provisioning := session.Event{
		Seq: 1, Type: session.EventTeamMember, Version: session.EventVersion, At: created,
		Data: []byte(`{"version":1,"teamId":"rollback-team-root","member":{"id":"rollback-team-root:worker","name":"worker","context":"fresh","phase":"provisioning"}}`),
	}
	if err := st.CreateTeamMemberSessionWithReservation(ctx, childID, created, SessionHeader{
		ID: childID, CreatedAt: created, Parent: rootID, Origin: "team", SeedLength: 0,
	}, nil, rootID, provisioning); err == nil {
		t.Fatal("Team reservation transaction with a conflicting receipt unexpectedly succeeded")
	}
	claimed, err := st.ReserveID(ctx, "team-member:rollback-team-root", childID)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("failed Team publication leaked its member identity reservation")
	}
	receipt, err := st.InspectSession(ctx, rootID)
	if err != nil || len(receipt) != 0 {
		t.Fatalf("receipt after rollback = %+v, err=%v", receipt, err)
	}
}

func TestSQLiteTeamMemberTransitionUsesLineageAndIsIdempotent(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Nanosecond)
	if err := st.CreateSession(ctx, "transition-root", created); err != nil {
		t.Fatal(err)
	}
	provisioning := session.Event{Seq: 1, Type: session.EventTeamMember, Version: session.EventVersion, At: created, Data: []byte(`{"version":1,"teamId":"team:transition-root","member":{"id":"team:transition-root:worker","name":"worker","description":"d","provider":"agent-registry","context":"fresh","phase":"provisioning"}}`)}
	if err := st.CreateTeamMemberSession(ctx, "team:transition-root:worker", created, SessionHeader{
		ID: "team:transition-root:worker", CreatedAt: created, Parent: "transition-root", Origin: "team", SeedLength: 0,
	}, nil, "transition-root", provisioning); err != nil {
		t.Fatal(err)
	}
	active := provisioning
	active.Seq = 2
	active.At = created.Add(time.Second)
	active.Data = []byte(`{"version":1,"teamId":"team:transition-root","member":{"id":"team:transition-root:worker","name":"worker","description":"d","provider":"agent-registry","context":"fresh","phase":"active"}}`)
	if err := st.TransitionTeamMember(ctx, "transition-root", "team:transition-root:worker", active); err != nil {
		t.Fatalf("active transition: %v", err)
	}
	if err := st.TransitionTeamMember(ctx, "transition-root", "team:transition-root:worker", active); err != nil {
		t.Fatalf("idempotent active transition: %v", err)
	}
	failed := active
	failed.Seq = 3
	failed.At = created.Add(2 * time.Second)
	failed.Data = []byte(`{"version":1,"teamId":"team:transition-root","member":{"id":"team:transition-root:worker","name":"worker","description":"d","provider":"agent-registry","context":"fresh","phase":"failed","error":"runtime stopped"}}`)
	if err := st.TransitionTeamMember(ctx, "transition-root", "team:transition-root:worker", failed); err != nil {
		t.Fatalf("failed transition: %v", err)
	}
	if _, err := st.LoadSession(ctx, "team:transition-root:worker"); err != nil {
		t.Fatalf("child after transition: %v", err)
	}
	root, err := st.InspectSession(ctx, "transition-root")
	if err != nil || len(root) != 3 || string(root[2].Data) != string(failed.Data) {
		t.Fatalf("transition root events = %+v err=%v", root, err)
	}
	wrong := failed
	wrong.Seq = 4
	if err := st.TransitionTeamMember(ctx, "other-root", "team:transition-root:worker", wrong); err == nil {
		t.Fatal("transition with wrong root unexpectedly succeeded")
	}
}

func TestSQLiteRejectsNewerSchemaOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE schema_meta SET version = 99 WHERE name = 'store'`); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("opening newer schema error = %v", err)
	}
}

func TestSQLiteLoadRepairsInterruptedTailButInspectDoesNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repair.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "repair", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventStepStart, session.NewStepStart(3)},
		{session.EventUserMessage, session.NewUserMessage("interrupted")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvents(ctx, "repair", log.Events()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InspectSession(ctx, "repair"); err == nil {
		t.Fatal("inspect accepted an open lifecycle")
	}
	before, err := st.LoadSession(ctx, "repair")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 || before[3].Type != session.EventStepEnd || before[4].Type != session.EventTurnEnd {
		t.Fatalf("repaired events = %+v", before)
	}
	if _, err := st.InspectSession(ctx, "repair"); err != nil {
		t.Fatalf("inspect after repair: %v", err)
	}
}

func TestSQLiteMaintenanceBackupIntegrityAndRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "maintenance", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "maintenance", []session.Event{{
		Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion,
		At: time.Now().UTC(), Data: []byte(`{"text":"durable"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "maintenance-backup.db")
	if err := st.Backup(ctx, backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backupStore, err := OpenSQLite(backup)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backupStore.Close()
	events, err := backupStore.LoadSession(ctx, "maintenance")
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	if len(events) != 1 || events[0].Type != session.EventUserMessage {
		t.Fatalf("backup events = %+v", events)
	}
	if err := st.Backup(ctx, backup); err == nil {
		t.Fatal("backup should refuse an existing destination")
	}

	// An open turn is repaired only through the explicit recovery-aware path.
	turn := session.New()
	if _, err := turn.Append(session.EventTurnStart, session.NewTurnStart()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "repair-maintenance", turn.Events()); err != nil {
		t.Fatal(err)
	}
	if err := st.RepairSession(ctx, "repair-maintenance"); err != nil {
		t.Fatalf("RepairSession: %v", err)
	}
	if _, err := st.InspectSession(ctx, "repair-maintenance"); err != nil {
		t.Fatalf("inspect after repair: %v", err)
	}
}

func TestSQLiteUncommittedTransactionDisappearsAfterChildProcessExit(t *testing.T) {
	if os.Getenv("SHUTU_SQLITE_CRASH_CHILD") == "1" {
		path := os.Getenv("SHUTU_SQLITE_CRASH_PATH")
		st, err := OpenSQLite(path)
		if err != nil {
			os.Exit(2)
		}
		tx, err := st.db.BeginTx(context.Background(), nil)
		if err != nil {
			os.Exit(3)
		}
		if _, err := tx.Exec(`INSERT INTO sessions (id, created_at, updated_at, cwd) VALUES ('crashed-session', 1, 1, '/crashed')`); err != nil {
			os.Exit(4)
		}
		if _, err := tx.Exec(`INSERT INTO events (session_id, seq, type, version, at, data) VALUES ('crashed-session', 1, 'user/message', 1, 1, '{"text":"uncommitted"}')`); err != nil {
			os.Exit(5)
		}
		// Do not commit or close: process termination must leave the SQLite
		// transaction uncommitted and recoverable by the next opener.
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "crash.db")
	initial, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.CreateSession(context.Background(), "committed-session", time.Now().UTC()); err != nil {
		_ = initial.Close()
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteUncommittedTransactionDisappearsAfterChildProcessExit$", "-test.v")
	cmd.Env = append(os.Environ(), "SHUTU_SQLITE_CRASH_CHILD=1", "SHUTU_SQLITE_CRASH_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite crash child: %v\n%s", err, output)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.CheckIntegrity(context.Background()); err != nil {
		t.Fatalf("integrity after child exit: %v", err)
	}
	if _, err := reopened.LoadSession(context.Background(), "crashed-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted session after child exit: %v", err)
	}
	if _, err := reopened.GetSessionHeader(context.Background(), "committed-session"); err != nil {
		t.Fatalf("committed session after child exit: %v", err)
	}
}

func TestListSessionsIsNotStarvedByHistoryReadCursor(t *testing.T) {
	st := openSQLite(t)
	if err := st.CreateSession(context.Background(), "concurrent", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	tx, err := st.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(context.Background(), `SELECT id FROM sessions`)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	defer rows.Close()
	defer tx.Rollback()

	done := make(chan error, 1)
	go func() {
		_, err := st.ListSessions(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListSessions while history cursor is open: %v", err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("ListSessions was starved by an open history read cursor")
	}

	metaDone := make(chan error, 1)
	go func() {
		_, err := st.GetSessionMeta(context.Background(), "concurrent")
		metaDone <- err
	}()
	select {
	case err := <-metaDone:
		if err != nil {
			t.Fatalf("GetSessionMeta while history cursor is open: %v", err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("GetSessionMeta was starved by an open history read cursor")
	}
}

// buildLog appends a representative mini-conversation through a session.Log
// whose sink forwards to the store, returning the log for later comparison.
func buildLog(t *testing.T, st Store, id string, wantDerived int) *session.Log {
	t.Helper()
	log := session.New()
	log.SetSink(func(ev session.Event) error {
		return st.AppendEvents(context.Background(), id, []session.Event{ev})
	})
	must := func(typ string, data any) {
		if _, err := log.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	must(session.EventUserMessage, session.NewUserMessage("what time is it"))
	must(session.EventAssistantChunk, session.NewAssistantChunk("Let "))
	must(session.EventAssistantChunk, session.NewAssistantChunk("me check"))
	must(session.EventAssistantMessage, session.NewAssistantMessage("Let me check", nil, "stop"))
	if wantDerived > 0 && len(log.DeriveHistory()) != wantDerived {
		t.Fatalf("derived %d messages, want %d", len(log.DeriveHistory()), wantDerived)
	}
	return log
}

func assertEventsEqual(t *testing.T, want, got []session.Event) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Seq != g.Seq {
			t.Errorf("event %d: seq = %d, want %d", i, g.Seq, w.Seq)
		}
		if w.Type != g.Type {
			t.Errorf("event %d: type = %q, want %q", i, g.Type, w.Type)
		}
		if w.Version != g.Version {
			t.Errorf("event %d: version = %d, want %d", i, g.Version, w.Version)
		}
		if w.At.UnixNano() != g.At.UnixNano() {
			t.Errorf("event %d: at = %v, want %v", i, g.At, w.At)
		}
		if !bytes.Equal(w.Data, g.Data) {
			t.Errorf("event %d: data = %s, want %s", i, g.Data, w.Data)
		}
	}
}

// TestReplayEventsConsistent persists events, closes the store, reopens it,
// and verifies the reloaded events match one-by-one and derive the same
// history (dispatch-m2: "事件逐条一致、派生历史一致").
func TestReplayEventsConsistent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pa.db")
	ctx := context.Background()

	st1, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const id = "s-replay"
	if err := st1.CreateSession(ctx, id, time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	want := buildLog(t, st1, id, 2) // user + assistant = 2 derived messages
	st1.Close()

	st2, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	events, err := st2.LoadSession(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assertEventsEqual(t, want.Events(), events)

	// Derived history must be identical after replay.
	replayed := session.New()
	if err := replayed.Restore(events); err != nil {
		t.Fatalf("restore: %v", err)
	}
	h1, h2 := want.DeriveHistory(), replayed.DeriveHistory()
	if len(h1) != len(h2) {
		t.Fatalf("derived history len = %d, want %d", len(h2), len(h1))
	}
	for i := range h1 {
		if h1[i].Role != h2[i].Role || h1[i].Text() != h2[i].Text() {
			t.Errorf("history %d: got %+v, want %+v", i, h2[i], h1[i])
		}
	}
}

// TestMultiSessionRestore verifies two sessions coexist: each loads only its
// own events and /list reports both (dispatch-m2: "多会话恢复").
func TestMultiSessionRestore(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()

	a := session.New()
	a.SetSink(func(ev session.Event) error { return st.AppendEvents(ctx, "s-a", []session.Event{ev}) })
	if _, err := a.Append(session.EventUserMessage, session.NewUserMessage("hello A")); err != nil {
		t.Fatalf("append A: %v", err)
	}

	b := session.New()
	b.SetSink(func(ev session.Event) error { return st.AppendEvents(ctx, "s-b", []session.Event{ev}) })
	if _, err := b.Append(session.EventUserMessage, session.NewUserMessage("hello B")); err != nil {
		t.Fatalf("append B: %v", err)
	}

	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("listed %d sessions, want 2: %+v", len(metas), metas)
	}

	evA, err := st.LoadSession(ctx, "s-a")
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	evB, err := st.LoadSession(ctx, "s-b")
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if len(evA) != 1 || len(evB) != 1 {
		t.Fatalf("event counts A=%d B=%d, want 1 each", len(evA), len(evB))
	}
	if !bytes.Contains(evA[0].Data, []byte("hello A")) {
		t.Errorf("session A data = %s", evA[0].Data)
	}
	if !bytes.Contains(evB[0].Data, []byte("hello B")) {
		t.Errorf("session B data = %s", evB[0].Data)
	}

	// Restoring each into a fresh log yields the correct history.
	la := session.New()
	if err := la.Restore(evA); err != nil {
		t.Fatalf("restore A: %v", err)
	}
	h := la.DeriveHistory()
	if len(h) != 1 || h[0].Role != llm.RoleUser || h[0].Text() != "hello A" {
		t.Errorf("session A history = %+v", h)
	}
}

// TestLoadNotFound verifies ErrNotFound for an unknown session id.
func TestLoadNotFound(t *testing.T) {
	st := openSQLite(t)
	if _, err := st.LoadSession(context.Background(), "s-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadSession = %v, want ErrNotFound", err)
	}
}

// TestAppendMaterializesSession verifies appending to a never-created session
// materializes its row (defensive) and it then appears in /list.
func TestAppendMaterializesSession(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	ev := session.Event{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"hi"}`)}
	if err := st.AppendEvents(ctx, "s-auto", []session.Event{ev}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := st.LoadSession(ctx, "s-auto")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
}

func TestSessionHeaderForkAndIdempotentAppend(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "parent", time.Unix(10, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionHeader(ctx, "parent", SessionHeader{
		ID: "parent", CWD: `C:\workspace`, Origin: "user", AgentPreset: "code",
		DelegationDepth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStartAt(1)},
		{session.EventStepStart, session.NewStepStartAt(1, 1)},
		{session.EventUserMessage, session.NewUserMessageAt(1, 1, 0, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hello")}})},
		{session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "done", nil, "stop", "", llm.TokenUsage{})},
		{session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", "")},
		{session.EventTurnEnd, session.NewTurnEndAt(1, "completed", "")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvents(ctx, "parent", log.Events()); err != nil {
		t.Fatal(err)
	}
	// Replaying the exact batch is idempotent rather than a unique-key failure.
	if err := st.AppendEvents(ctx, "parent", log.Events()); err != nil {
		t.Fatalf("idempotent append: %v", err)
	}
	if err := st.ForkSession(ctx, "parent", "child", 6); err != nil {
		t.Fatalf("fork: %v", err)
	}
	header, err := st.GetSessionHeader(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if header.Parent != "parent" || header.SeedLength != 6 || header.Origin != "fork" || header.DelegationDepth != 3 || header.AgentPreset != "code" {
		t.Fatalf("child header = %+v", header)
	}
	child, err := st.LoadSession(ctx, "child")
	if err != nil || len(child) != 6 || child[len(child)-1].Seq != 6 {
		t.Fatalf("child events = %d / %+v, err=%v", len(child), child, err)
	}
}

func TestSQLiteForkTreatsBoundaryZeroAsExplicit(t *testing.T) {
	st := openSQLite(t)
	defer st.Close()
	ctx := context.Background()
	created := time.Unix(20, 0).UTC()
	seed := []session.Event{{
		Seq: 0, Type: session.EventUserMessage, Version: session.EventVersion,
		At: created, Data: json.RawMessage(`{"text":"zero"}`),
	}}
	if err := st.CreateSessionWithEvents(ctx, "zero-parent", created, SessionHeader{
		ID: "zero-parent", SeedLength: 1,
	}, seed); err != nil {
		t.Fatal(err)
	}
	if err := st.ForkSession(ctx, "zero-parent", "zero-child", 0); err != nil {
		t.Fatalf("explicit zero boundary was rejected: %v", err)
	}
	child, err := st.LoadSessionRaw(ctx, "zero-child")
	if err != nil || len(child) != 1 || child[0].Seq != 0 {
		t.Fatalf("child = %+v, err=%v", child, err)
	}
}

func TestSQLiteForkDoesNotRepairOpenParent(t *testing.T) {
	st := openSQLite(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "open-fork-parent", time.Unix(30, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventStepStart, session.NewStepStart(1)},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvents(ctx, "open-fork-parent", log.Events()); err != nil {
		t.Fatal(err)
	}
	if err := st.ForkSession(ctx, "open-fork-parent", "open-fork-child", log.Events()[len(log.Events())-1].Seq); err == nil {
		t.Fatal("fork unexpectedly accepted an open parent")
	}
	if _, err := st.GetSessionMeta(ctx, "open-fork-child"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed fork published child: %v", err)
	}
	if got, err := st.LoadSessionRaw(ctx, "open-fork-parent"); err != nil || len(got) != 2 {
		t.Fatalf("parent was repaired: events=%d err=%v", len(got), err)
	}
}

// TestWorkspaceLifecycle covers the P6 workspace store: create (idempotent,
// appended sort), list order, rename, session membership + ungroup, and delete
// returning sessions to the ungrouped bucket.
func TestWorkspaceLifecycle(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()

	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil { // idempotent
		t.Fatalf("re-create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w2", "日常"); err != nil {
		t.Fatalf("create w2: %v", err)
	}
	ws, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ws) != 2 || ws[0].ID != "w1" || ws[1].ID != "w2" {
		t.Fatalf("workspaces = %+v, want w1,w2 in creation order", ws)
	}
	if ws[1].Sort <= ws[0].Sort {
		t.Fatalf("sorts not ascending: %d then %d", ws[0].Sort, ws[1].Sort)
	}

	if err := st.SetWorkspaceTitle(ctx, "w1", "研究·改"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := st.SetWorkspaceTitle(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename unknown: %v, want ErrNotFound", err)
	}

	// Sessions join a workspace and read back through ListSessions.
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := st.SetSessionWorkspace(ctx, "s1", "w1"); err != nil {
		t.Fatalf("set s1->w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s2", "w1"); err != nil {
		t.Fatalf("set s2->w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s2", ""); err != nil { // back to ungrouped
		t.Fatalf("ungroup s2: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "nope", "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set unknown session: %v, want ErrNotFound", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s1"].WorkspaceID != "w1" || byID["s2"].WorkspaceID != "" || byID["s3"].WorkspaceID != "" {
		t.Fatalf("workspace ids = %q/%q/%q, want w1/''/''", byID["s1"].WorkspaceID, byID["s2"].WorkspaceID, byID["s3"].WorkspaceID)
	}

	// Deleting a workspace returns its sessions to the ungrouped bucket.
	if err := st.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatalf("delete w1: %v", err)
	}
	if err := st.DeleteWorkspace(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v, want ErrNotFound", err)
	}
	metas, err = st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions after delete: %v", err)
	}
	for _, m := range metas {
		if m.WorkspaceID != "" {
			t.Fatalf("session %s still in workspace %q after delete", m.ID, m.WorkspaceID)
		}
	}
}

func TestSQLiteControlPlaneWritesShareCrossProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	left, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()

	ctx := context.Background()
	const total = 32
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := left
			if i%2 == 1 {
				st = right
			}
			if err := st.CreateWorkspace(ctx, fmt.Sprintf("shared-%02d", i), "shared"); err != nil {
				t.Errorf("create workspace %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	workspaces, err := left.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != total {
		t.Fatalf("workspace count = %d, want %d", len(workspaces), total)
	}
	seenSort := make(map[int]string, total)
	for _, workspace := range workspaces {
		if previous, exists := seenSort[workspace.Sort]; exists {
			t.Fatalf("duplicate workspace sort %d for %q and %q", workspace.Sort, previous, workspace.ID)
		}
		seenSort[workspace.Sort] = workspace.ID
	}
}

func TestSQLiteWriteLockHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel-lock.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	held, err := acquireSQLiteProcessLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.SetSetting(ctx, "cancelled", "value") }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled write error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SQLite write remained blocked after context cancellation")
	}
}

// TestArchiveAndOrder covers the P6.2 additions: archive/unarchive toggling,
// manual session order (with cross-workspace move) and manual workspace order.
func TestArchiveAndOrder(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.CreateWorkspace(ctx, "w2", "日常"); err != nil {
		t.Fatalf("create w2: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// Archive hides a session from the active set (ArchivedAt set, cleared on
	// unarchive).
	if err := st.ArchiveSession(ctx, "s2", true); err != nil {
		t.Fatalf("archive s2: %v", err)
	}
	if err := st.ArchiveSession(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive unknown: %v, want ErrNotFound", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range metas {
		if m.ID == "s2" && m.ArchivedAt.IsZero() {
			t.Fatal("s2 should be archived")
		}
		if m.ID != "s2" && !m.ArchivedAt.IsZero() {
			t.Fatalf("%s should not be archived", m.ID)
		}
	}
	if err := st.ArchiveSession(ctx, "s2", false); err != nil {
		t.Fatalf("unarchive s2: %v", err)
	}

	// Manual session order moves sessions across workspaces and assigns Sort.
	if err := st.ReorderSessions(ctx, "w1", []string{"s2", "s1", "s3"}); err != nil {
		t.Fatalf("reorder sessions: %v", err)
	}
	metas, _ = st.ListSessions(ctx)
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	for i, id := range []string{"s2", "s1", "s3"} {
		if byID[id].WorkspaceID != "w1" || byID[id].Sort != i {
			t.Fatalf("%s = ws %q sort %d, want w1 sort %d", id, byID[id].WorkspaceID, byID[id].Sort, i)
		}
	}
	// Moving back to ungrouped with a new order.
	if err := st.ReorderSessions(ctx, "", []string{"s3", "s2"}); err != nil {
		t.Fatalf("reorder to ungrouped: %v", err)
	}
	metas, _ = st.ListSessions(ctx)
	byID = map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s3"].WorkspaceID != "" || byID["s3"].Sort != 0 {
		t.Fatalf("s3 after move = ws %q sort %d, want ungrouped sort 0", byID["s3"].WorkspaceID, byID["s3"].Sort)
	}

	// Manual workspace order.
	if err := st.ReorderWorkspaces(ctx, []string{"w2", "w1"}); err != nil {
		t.Fatalf("reorder workspaces: %v", err)
	}
	ws, err := st.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("list ws: %v", err)
	}
	if ws[0].ID != "w2" || ws[1].ID != "w1" {
		t.Fatalf("workspace order = %s,%s, want w2,w1", ws[0].ID, ws[1].ID)
	}
}

// TestSearchAndFlatOrder covers P6.3: body-text search returns a snippet per
// matching session, and flat-order rewrites flat_sort without touching
// workspace membership.
func TestSearchAndFlatOrder(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	mk := func(id, text string) {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := st.AppendEvents(ctx, id, []session.Event{
			{Seq: 1, Type: session.EventUserMessage, Version: 1, At: time.Now().UTC(), Data: []byte(`{"text":"` + text + `"}`)},
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	mk("s1", "部署 dsh 网关的配置说明")
	mk("s2", "今天天气很好")
	mk("s3", "再次部署网关")
	// Set a title for one hit to confirm it is carried.
	if err := st.SetSessionTitle(ctx, "s1", "网关部署手册", session.TitleSourceUser); err != nil {
		t.Fatalf("set title: %v", err)
	}

	hits, err := st.SearchSessions(ctx, "部署")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("search hits = %d, want 2 (s1, s3)", len(hits))
	}
	got := map[string]SearchHit{}
	for _, h := range hits {
		got[h.SessionID] = h
	}
	if got["s1"].Snippet == "" || got["s1"].Title != "网关部署手册" {
		t.Fatalf("s1 hit = %+v, want snippet + title", got["s1"])
	}
	if _, ok := got["s2"]; ok {
		t.Fatal("s2 should not match 部署")
	}
	// A literal % must not act as a wildcard.
	if hits, _ := st.SearchSessions(ctx, "%"); len(hits) != 0 {
		t.Fatalf("literal %% matched %d sessions, want 0", len(hits))
	}

	// Flat order: s1/s3 keep membership, take flat_sort.
	if err := st.CreateWorkspace(ctx, "w1", "研究"); err != nil {
		t.Fatalf("create w1: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "s3", "w1"); err != nil {
		t.Fatalf("set s3->w1: %v", err)
	}
	if err := st.ReorderSessionsFlat(ctx, []string{"s3", "s1"}); err != nil {
		t.Fatalf("flat order: %v", err)
	}
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]SessionMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if byID["s3"].FlatSort != 0 || byID["s1"].FlatSort != 1 {
		t.Fatalf("flat sorts = %d/%d, want 0/1", byID["s3"].FlatSort, byID["s1"].FlatSort)
	}
	if byID["s3"].WorkspaceID != "w1" {
		t.Fatalf("flat reorder changed membership: s3 ws = %q", byID["s3"].WorkspaceID)
	}
}

// TestMigrateLegacyTitleBecomesUserPin verifies that a pre-title-source row
// carrying a non-empty title (written only by the old sidebar rename) is pinned
// as user-sourced after migrating, so automatic title revision never overwrites
// a pre-existing rename.
func TestMigrateLegacyTitleBecomesUserPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pa.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, created_at INTEGER, updated_at INTEGER, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, title) VALUES ('s-old', 1, 1, '老标题')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	meta, err := st.GetSessionMeta(context.Background(), "s-old")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "老标题" || meta.TitleSource != session.TitleSourceUser {
		t.Fatalf("legacy pin: title=%q source=%q, want user", meta.Title, meta.TitleSource)
	}
}

// TestMarkSessionViewedAndLastViewedAt verifies the finished-but-unviewed
// tracking: LastViewedAt surfaces through ListSessions/GetSessionMeta and
// MarkSessionViewed clears it.
func TestMarkSessionViewedAndLastViewedAt(t *testing.T) {
	st := openSQLite(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "s-v", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Hour)

	// Unset initially.
	meta, err := st.GetSessionMeta(ctx, "s-v")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.LastViewedAt.IsZero() {
		t.Fatalf("new session LastViewedAt = %v, want zero", meta.LastViewedAt)
	}

	if err := st.MarkSessionViewed(ctx, "s-v", before); err != nil {
		t.Fatal(err)
	}
	meta, err = st.GetSessionMeta(ctx, "s-v")
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastViewedAt.IsZero() || !meta.LastViewedAt.Equal(before) {
		t.Fatalf("LastViewedAt = %v, want %v", meta.LastViewedAt, before)
	}

	// ListSessions surfaces it too.
	metas, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range metas {
		if m.ID == "s-v" && !m.LastViewedAt.IsZero() {
			found = true
		}
	}
	if !found {
		t.Fatal("ListSessions did not surface LastViewedAt for s-v")
	}

	// Unknown id → ErrNotFound.
	if err := st.MarkSessionViewed(ctx, "s-missing", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkSessionViewed(missing) err = %v, want ErrNotFound", err)
	}
}

func TestGetSessionMetaNotFound(t *testing.T) {
	st := openSQLite(t)
	if _, err := st.GetSessionMeta(context.Background(), "missing-meta"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSessionMeta(missing) err = %v, want ErrNotFound", err)
	}
}

// TestSessionConfig verifies the per-session override read/write surface
// (Phase 2 按会话切换; dsh ModelSelection 对齐): default zeros,
// SetSessionConfig stores the full override set (mode/provider/model/effort/
// permission), UpdateSessionConfig rewrites provider/model/effort/permission
// (mode stays locked), and a missing id returns ErrNotFound.
func TestSessionConfig(t *testing.T) {
	ctx := context.Background()
	st := openSQLite(t)
	if err := st.CreateSession(ctx, "s-cfg", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Default: all empty (fall back to the globals).
	got, err := st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got != (SessionConfig{}) {
		t.Fatalf("default session config = %+v, want zero", got)
	}

	// Set the full override set (dsh selection: provider+model+effort).
	if err := st.SetSessionConfig(ctx, "s-cfg", SessionConfig{AgentPreset: "minimal", Provider: "openai", Model: "gpt-4o", ReasoningEffort: "high", Permission: "readonly"}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPreset != "minimal" || got.Provider != "openai" || got.Model != "gpt-4o" || got.ReasoningEffort != "high" || got.Permission != "readonly" {
		t.Fatalf("session config = %+v, want minimal/openai/gpt-4o/high/readonly", got)
	}

	// UpdateSessionConfig rewrites provider/model/effort/permission; the mode
	// is untouched.
	if err := st.UpdateSessionConfig(ctx, "s-cfg", "anthropic", "claude-3-5", "max", "full"); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPreset != "minimal" || got.Provider != "anthropic" || got.Model != "claude-3-5" || got.ReasoningEffort != "max" || got.Permission != "full" {
		t.Fatalf("session config after update = %+v, want minimal/anthropic/claude-3-5/max/full", got)
	}

	// Clearing provider/model/effort/permission returns to global fallback,
	// mode still locked.
	if err := st.UpdateSessionConfig(ctx, "s-cfg", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "" || got.Model != "" || got.ReasoningEffort != "" || got.Permission != "" || got.AgentPreset != "minimal" {
		t.Fatalf("session config after clear = %+v, want ''/''/''/''/minimal", got)
	}

	// Empty mode clears the lock too (SetSessionConfig with empty set).
	if err := st.SetSessionConfig(ctx, "s-cfg", SessionConfig{}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSessionConfig(ctx, "s-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if got != (SessionConfig{}) {
		t.Fatalf("session config after full clear = %+v, want zero", got)
	}

	// Unknown id → ErrNotFound.
	if _, err := st.GetSessionConfig(ctx, "s-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSessionConfig(missing) err = %v, want ErrNotFound", err)
	}
	if err := st.UpdateSessionConfig(ctx, "s-missing", "m", "", "m", "full"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSessionConfig(missing) err = %v, want ErrNotFound", err)
	}
}

func TestMessageFeedbackCRUD(t *testing.T) {
	ctx := context.Background()
	st := openSQLite(t)
	if err := st.CreateSession(ctx, "s-feedback", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := st.GetMessageFeedback(ctx, "s-feedback", 2); err != nil || ok || got != (MessageFeedback{}) {
		t.Fatalf("initial feedback = %+v, %v, %v; want zero,false,nil", got, ok, err)
	}
	created, err := st.PutMessageFeedback(ctx, "s-feedback", 2, "positive", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID != "s-feedback" || created.Seq != 2 || created.Rating != "positive" {
		t.Fatalf("created feedback = %+v", created)
	}
	items, err := st.ListMessageFeedback(ctx, "s-feedback")
	if err != nil || len(items) != 1 || items[0].Rating != "positive" {
		t.Fatalf("listed feedback = %+v, err=%v", items, err)
	}
	updated, err := st.PutMessageFeedback(ctx, "s-feedback", 2, "negative", "changed")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Rating != "negative" || updated.Note != "changed" || updated.UpdatedAt.Before(updated.CreatedAt) {
		t.Fatalf("updated feedback = %+v", updated)
	}
	if err := st.DeleteMessageFeedback(ctx, "s-feedback", 2); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetMessageFeedback(ctx, "s-feedback", 2); err != nil || ok {
		t.Fatalf("feedback after delete = ok=%v err=%v, want false,nil", ok, err)
	}
	if _, err := st.ListMessageFeedback(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListMessageFeedback(missing) err=%v, want ErrNotFound", err)
	}
}

func TestSQLiteSessionTitleEventProjectsMetadataAtomically(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "title.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "title-session", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	data := json.RawMessage(`{"title":"Canonical title","messageSeqs":[],"source":{"kind":"user"}}`)
	if err := st.AppendEvents(ctx, "title-session", []session.Event{{
		Seq: 1, Type: session.EventSessionTitle, Version: session.EventVersion, At: time.Now().UTC(), Data: data,
	}}); err != nil {
		t.Fatal(err)
	}
	meta, err := st.GetSessionMeta(ctx, "title-session")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Canonical title" || meta.TitleSource != session.TitleSourceUser {
		t.Fatalf("title metadata = %+v, want canonical user projection", meta)
	}
	events, err := st.LoadSession(ctx, "title-session")
	if err != nil || len(events) != 1 || events[0].Type != session.EventSessionTitle {
		t.Fatalf("title events = %+v, err=%v", events, err)
	}
}
