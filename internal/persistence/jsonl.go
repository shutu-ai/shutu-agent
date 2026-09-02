// Package persistence contains durable session-log providers. JSONL is kept
// independent from the product Store so deployments can choose a file-backed
// transcript without changing session derivation or the Agent loop.
package persistence

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

const FormatVersion = 1

var (
	ErrInvalidSessionID  = errors.New("persistence: invalid session id")
	ErrFormatUnsupported = errors.New("persistence: unsupported format version")
	ErrCorruptLog        = errors.New("persistence: corrupt session log")
)

// jsonlWriteFault is a process-local fault-injection seam used by the
// release-gate crash matrix. Normal deployments leave it nil; appendJSONLEvents
// still owns the same rollback/restart semantics when a real ENOSPC or process
// death interrupts a physical batch.
type jsonlWriteFault func(writeIndex int) error

var jsonlWriteFaultHook atomic.Pointer[jsonlWriteFault]

// installJSONLWriteFaultForTest installs the bounded process-local fault hook.
// It is intentionally unexported and limited to write ordering; callers cannot
// alter bytes or bypass validation.
func installJSONLWriteFaultForTest(fault jsonlWriteFault) {
	if fault == nil {
		jsonlWriteFaultHook.Store(nil)
		return
	}
	jsonlWriteFaultHook.Store(&fault)
}

// Header is storage metadata and never enters session.Log.DeriveHistory.
type Header struct {
	Version         int       `json:"version"`
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"createdAt"`
	CWD             string    `json:"cwd,omitempty"`
	Parent          string    `json:"parentSession,omitempty"`
	SeedLength      int       `json:"seedLength,omitempty"`
	Origin          string    `json:"origin,omitempty"`
	DelegationDepth int       `json:"delegationDepth,omitempty"`
	AgentPreset     string    `json:"agentPreset,omitempty"`
}

type headerRecord struct {
	Kind string `json:"kind"`
	Header
}

type eventRecord struct {
	Kind    string          `json:"kind"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// JSONL is a synchronous append-only JSONL backend. Each Append fsyncs the
// batch before returning, which is a stronger durability default than DSH's
// optional write-behind policy and is safe for the existing session.Log sink.
type JSONL struct {
	root string
	mu   sync.Mutex
}

func OpenJSONL(root string) (*JSONL, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("persistence: root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &JSONL{root: filepath.Clean(root)}, nil
}

func (j *JSONL) Locate(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(j.root, id+".jsonl"), nil
}

func validateID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return nil
}

func (j *JSONL) Create(ctx context.Context, header Header, seed []session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	header, err = normalizeHeader(header)
	if err != nil {
		return err
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = time.Now().UTC()
	}
	if err := validateEvents(seed, 0); err != nil {
		return err
	}
	if header.SeedLength != len(seed) {
		return fmt.Errorf("%w: seed length %d does not match %d events", ErrCorruptLog, header.SeedLength, len(seed))
	}
	if err := session.ValidateLifecycle(seed); err != nil {
		return fmt.Errorf("%w: seed lifecycle: %v", ErrCorruptLog, err)
	}
	if err := session.ValidateEventProvenance(seed); err != nil {
		return fmt.Errorf("%w: seed provenance: %v", ErrCorruptLog, err)
	}
	path, _ := j.Locate(header.ID)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("persistence: lock session %q: %w", header.ID, err)
	}
	defer lock.Close()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("persistence: session %q already exists", header.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func(cause error) error {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return errors.Join(cause, closeErr, removeErr)
	}
	if err := writeRecord(file, headerRecord{Kind: "header", Header: header}); err != nil {
		return cleanup(err)
	}
	for _, event := range seed {
		if err := writeRecord(file, eventRecordFrom(event)); err != nil {
			return cleanup(err)
		}
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	return file.Close()
}

func (j *JSONL) Append(ctx context.Context, id string, events []session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	path, _ := j.Locate(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("persistence: lock session %q: %w", id, err)
	}
	defer lock.Close()
	loaded, err := j.loadLocked(path, true)
	if err != nil {
		return err
	}
	last := uint64(0)
	if len(loaded.Events) > 0 {
		last = loaded.Events[len(loaded.Events)-1].Seq
	}
	remaining := make([]session.Event, 0, len(events))
	for _, event := range events {
		if event.Seq <= last {
			found := false
			for _, existing := range loaded.Events {
				if existing.Seq == event.Seq && existing.Type == event.Type && existing.Version == event.Version && existing.At.Equal(event.At) && string(existing.Data) == string(event.Data) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: conflicting replay at sequence %d", ErrCorruptLog, event.Seq)
			}
			continue
		}
		remaining = append(remaining, event)
	}
	if len(remaining) == 0 {
		return nil
	}
	if err := validateEvents(remaining, last); err != nil {
		return err
	}
	combined := append(append([]session.Event(nil), loaded.Events...), remaining...)
	if err := session.ValidateEventProvenance(combined); err != nil {
		return fmt.Errorf("%w: append provenance: %v", ErrCorruptLog, err)
	}
	return appendJSONLEvents(path, remaining)
}

// appendJSONLEvents commits a batch as one physical append transaction. The
// durable prefix is captured before opening the append handle; every failure
// after that point closes the writer and restores the exact prefix. Keeping
// this boundary shared by normal append and interrupted-tail repair is
// important: recovery must never turn a second crash into a new corrupt tail.
func appendJSONLEvents(path string, events []session.Event) error {
	if len(events) == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	before := info.Size()
	writeIndex := 0
	rollback := func(cause error) error {
		closeErr := file.Close()
		rollbackErr := rollbackJSONLAppend(path, before)
		return errors.Join(cause, closeErr, rollbackErr)
	}
	for _, event := range events {
		if err := writeRecordWithFault(file, eventRecordFrom(event), writeIndex); err != nil {
			return rollback(err)
		}
		writeIndex++
	}
	if err := file.Sync(); err != nil {
		return rollback(err)
	}
	if err := file.Close(); err != nil {
		// A close failure leaves the commit ambiguous. Prefer a recoverable
		// prefix over reporting a successful append with unknown durability.
		return errors.Join(err, rollbackJSONLAppend(path, before))
	}
	return nil
}

// rollbackJSONLAppend restores the exact committed prefix after a failed
// append batch. It is deliberately a separate open handle: Windows cannot
// truncate a file while the append handle is still open, and a partial write
// must not be left for a later reader to interpret as committed events.
func rollbackJSONLAppend(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persistence: reopen append for rollback: %w", err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return fmt.Errorf("persistence: truncate failed append: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("persistence: sync rolled-back append: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("persistence: close rolled-back append: %w", err)
	}
	return nil
}

// Flush is an explicit durability barrier. Append already syncs each batch,
// but Flush still opens and syncs the addressed artifact so callers get a real
// barrier after a sequence of adapter operations and a missing session cannot
// be mistaken for a successful flush.
func (j *JSONL) Flush(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	path, _ := j.Locate(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("persistence: lock flush %q: %w", id, err)
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("persistence: open flush target %q: %w", id, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("persistence: sync session %q: %w", id, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("persistence: close flush target %q: %w", id, err)
	}
	return nil
}

// Checkpoint is a named durability barrier for the shared persistence
// contract. JSONL appends fsync before returning, therefore no deferred batch
// exists to drain; checking the artifact also makes a missing/corrupt target
// observable to callers requesting an explicit checkpoint.
func (j *JSONL) Checkpoint(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := j.Inspect(ctx, id)
	return err
}

type Loaded struct {
	Header Header
	Events []session.Event
	// Revision is the last durable sequence, including zero for a new log.
	// It is opaque to history derivation but lets reconnecting consumers resume
	// from an exact append position without re-reading the entire transcript.
	Revision uint64
	// RevisionToken is a source-qualified physical change token. Revision is
	// retained for cursor compatibility; independent stores must compare this
	// opaque token instead of the numeric sequence.
	RevisionToken string
}

func jsonlRevisionToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		root = filepath.Clean(filepath.Dir(path))
	}
	return fmt.Sprintf("jsonl:%s:%s:%d:%d", root, filepath.Base(path), info.Size(), info.ModTime().UTC().UnixNano()), nil
}

func (j *JSONL) Load(ctx context.Context, id string) (Loaded, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	if err := validateID(id); err != nil {
		return Loaded{}, err
	}
	path, _ := j.Locate(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return Loaded{}, fmt.Errorf("persistence: lock session %q: %w", id, err)
	}
	defer lock.Close()
	loaded, err := j.loadLocked(path, true)
	if err != nil {
		return Loaded{}, err
	}
	if err := repairInterruptedLocked(path, &loaded); err != nil {
		return Loaded{}, err
	}
	if len(loaded.Events) > 0 {
		loaded.Revision = loaded.Events[len(loaded.Events)-1].Seq
	}
	loaded.RevisionToken, err = jsonlRevisionToken(path)
	if err != nil {
		return Loaded{}, err
	}
	if err := session.ValidateLifecycle(loaded.Events); err != nil {
		return Loaded{}, fmt.Errorf("%w: lifecycle: %v", ErrCorruptLog, err)
	}
	if loaded.Header.ID != id {
		return Loaded{}, fmt.Errorf("%w: header id %q does not match %q", ErrCorruptLog, loaded.Header.ID, id)
	}
	return loaded, nil
}

// Inspect reads a cold transcript without appending crash-repair boundaries.
// It is useful for history/search views that must not mutate storage.
func (j *JSONL) Inspect(ctx context.Context, id string) (Loaded, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	if err := validateID(id); err != nil {
		return Loaded{}, err
	}
	path, _ := j.Locate(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return Loaded{}, fmt.Errorf("persistence: lock session %q: %w", id, err)
	}
	defer lock.Close()
	loaded, err := j.loadLocked(path, false)
	if err != nil {
		return Loaded{}, err
	}
	if loaded.Header.ID != id {
		return Loaded{}, fmt.Errorf("%w: header id %q does not match %q", ErrCorruptLog, loaded.Header.ID, id)
	}
	closers, err := session.InterruptedTurnClosers(loaded.Events)
	if err != nil {
		return Loaded{}, fmt.Errorf("%w: lifecycle: %v", ErrCorruptLog, err)
	}
	loaded.Events = append(loaded.Events, closers...)
	loaded.RevisionToken, err = jsonlRevisionToken(path)
	if err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

// ReadFrom returns a non-mutating committed suffix beginning at fromSeq. A
// zero cursor requests the complete inspected transcript.
func (j *JSONL) ReadFrom(ctx context.Context, id string, fromSeq uint64) (Loaded, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	if err := validateID(id); err != nil {
		return Loaded{}, err
	}
	path, _ := j.Locate(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := acquireProcessLockContext(ctx, path+".lock")
	if err != nil {
		return Loaded{}, fmt.Errorf("persistence: lock session %q: %w", id, err)
	}
	defer lock.Close()
	return j.readSuffixLocked(ctx, path, id, fromSeq)
}

// ListSnapshots reads each artifact's validated header and durable revision.
// JSONL has no separate session index, so the revision scan is deliberately
// read-only and shares the same parser/validation path as Inspect.
func (j *JSONL) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(j.root)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		loaded, err := j.Inspect(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Snapshot{Header: loaded.Header, Revision: loaded.Revision, RevisionToken: loaded.RevisionToken})
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Header.CreatedAt.Equal(out[k].Header.CreatedAt) {
			return out[i].Header.ID < out[k].Header.ID
		}
		return out[i].Header.CreatedAt.Before(out[k].Header.CreatedAt)
	})
	return out, nil
}

// Backup copies the complete JSONL store to a new directory. Each session is
// copied while holding its cross-process lock, so a concurrent append cannot
// produce a mixed file. The destination must be new and cannot be nested under
// the source, preventing recursive self-backups.
func (j *JSONL) Backup(ctx context.Context, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := filepath.Abs(j.root)
	if err != nil {
		return fmt.Errorf("persistence: resolve source backup root: %w", err)
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("persistence: resolve backup root: %w", err)
	}
	rel, err := filepath.Rel(source, destination)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("persistence: backup destination must be outside the source root")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("persistence: backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("persistence: create backup root: %w", err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionPath := filepath.Join(source, entry.Name())
		lock, err := acquireProcessLockContext(ctx, sessionPath+".lock")
		if err != nil {
			return fmt.Errorf("persistence: lock backup source %q: %w", entry.Name(), err)
		}
		data, readErr := os.ReadFile(sessionPath)
		closeErr := lock.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		tmp, err := os.CreateTemp(destination, ".backup-*.tmp")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		ok := false
		defer func() {
			if !ok {
				_ = os.Remove(tmpName)
			}
		}()
		if err := tmp.Chmod(0o600); err != nil {
			_ = tmp.Close()
			return err
		}
		_, err = tmp.Write(data)
		if err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := os.Rename(tmpName, filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
		ok = true
	}
	return nil
}

// CheckIntegrity parses every session without performing crash repair. A
// caller that wants repair must use RepairSession explicitly.
func (j *JSONL) CheckIntegrity(ctx context.Context) error {
	_, err := j.ListSnapshots(ctx)
	return err
}

// RepairSession applies torn-tail and interrupted-turn recovery to one JSONL
// artifact through the normal recovery-aware Load path.
func (j *JSONL) RepairSession(ctx context.Context, sessionID string) error {
	if _, err := j.Load(ctx, sessionID); err != nil {
		return fmt.Errorf("persistence: repair session %q: %w", sessionID, err)
	}
	return nil
}

// Fork creates an independent child transcript seeded with the parent's
// logical events. The child gets a new sequence namespace while preserving
// lineage metadata and the exact seed boundary.
func (j *JSONL) Fork(ctx context.Context, parentID string, child Header) error {
	// A fork must copy a durable closed prefix, never the synthetic closers
	// produced by crash recovery. ReadFrom is the non-mutating physical read;
	// an open parent is rejected below instead of being silently rewritten as
	// an interrupted child seed.
	parent, err := j.ReadFrom(ctx, parentID, 0)
	if err != nil {
		return err
	}
	if err := session.ValidateLifecycle(parent.Events); err != nil {
		return fmt.Errorf("%w: cannot fork open parent %q: %v", ErrCorruptLog, parentID, err)
	}
	child.Parent = parentID
	child.SeedLength = len(parent.Events)
	if child.Origin == "" {
		child.Origin = "fork"
	}
	if child.DelegationDepth <= parent.Header.DelegationDepth {
		child.DelegationDepth = parent.Header.DelegationDepth + 1
	}
	return j.Create(ctx, child, parent.Events)
}

// OpenLog restores an existing transcript or creates a new one, then binds
// the Log sink so every future append uses this backend. The returned log is
// ready for the Agent loop and the returned header remains storage metadata.
func (j *JSONL) OpenLog(ctx context.Context, id string, header Header) (*session.Log, Header, error) {
	path, err := j.Locate(id)
	if err != nil {
		return nil, Header{}, err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		header.ID = id
		if err := j.Create(ctx, header, nil); err != nil {
			return nil, Header{}, err
		}
	} else if statErr != nil {
		return nil, Header{}, statErr
	}
	loaded, err := j.Load(ctx, id)
	if err != nil {
		return nil, Header{}, err
	}
	log := session.New()
	if err := log.Restore(loaded.Events); err != nil {
		return nil, Header{}, err
	}
	log.SetSink(func(event session.Event) error {
		return j.Append(context.Background(), id, []session.Event{event})
	})
	return log, loaded.Header, nil
}

func (j *JSONL) loadLocked(path string, tolerateTornTail bool) (Loaded, error) {
	file, err := os.Open(path)
	if err != nil {
		return Loaded{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	raw, err := io.ReadAll(reader)
	if err != nil {
		return Loaded{}, err
	}
	lines := strings.Split(string(raw), "\n")
	// A crash can leave a torn final JSONL record without its newline. Preserve
	// every complete preceding record and let Load repair lifecycle balance.
	tornTail := len(raw) > 0 && raw[len(raw)-1] != '\n'
	if tornTail && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	if tornTail && tolerateTornTail {
		lastNewline := len(raw) - 1
		for lastNewline >= 0 && raw[lastNewline] != '\n' {
			lastNewline--
		}
		if err := file.Close(); err != nil {
			return Loaded{}, err
		}
		if err := os.Truncate(path, int64(lastNewline+1)); err != nil {
			return Loaded{}, err
		}
	}
	var loaded Loaded
	var sawHeader bool
	for _, lineText := range lines {
		line := []byte(lineText)
		if len(bytesTrimSpace(line)) == 0 {
			continue
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			return Loaded{}, fmt.Errorf("%w: %v", ErrCorruptLog, err)
		}
		switch kind.Kind {
		case "header":
			if sawHeader {
				return Loaded{}, fmt.Errorf("%w: duplicate header", ErrCorruptLog)
			}
			var record headerRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return Loaded{}, fmt.Errorf("%w: header: %v", ErrCorruptLog, err)
			}
			loaded.Header = record.Header
			sawHeader = true
		case "event":
			var record eventRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return Loaded{}, fmt.Errorf("%w: event: %v", ErrCorruptLog, err)
			}
			loaded.Events = append(loaded.Events, session.Event{Seq: record.Seq, Type: record.Type, At: record.At, Version: record.Version, Data: append(json.RawMessage(nil), record.Data...)})
		default:
			return Loaded{}, fmt.Errorf("%w: unknown record %q", ErrCorruptLog, kind.Kind)
		}
	}
	if !sawHeader {
		return Loaded{}, fmt.Errorf("%w: missing header", ErrCorruptLog)
	}
	if loaded.Header.Version != FormatVersion {
		return Loaded{}, fmt.Errorf("%w: %d", ErrFormatUnsupported, loaded.Header.Version)
	}
	if loaded.Header.ID == "" {
		return Loaded{}, fmt.Errorf("%w: empty header id", ErrCorruptLog)
	}
	if err := validateEvents(loaded.Events, 0); err != nil {
		return Loaded{}, err
	}
	if len(loaded.Events) > 0 {
		loaded.Revision = loaded.Events[len(loaded.Events)-1].Seq
	}
	return loaded, nil
}

// readSuffixLocked validates a JSONL transcript while retaining only events
// at or after fromSeq. It deliberately ignores an incomplete final line just
// like Inspect: reconnect/cursor reads are read-only and must never repair a
// cold transcript. Memory is therefore O(one record + returned suffix), not
// O(entire history).
func (j *JSONL) readSuffixLocked(ctx context.Context, path, id string, fromSeq uint64) (Loaded, error) {
	file, err := os.Open(path)
	if err != nil {
		return Loaded{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	var loaded Loaded
	var sawHeader bool
	var lastSeq uint64
	turn, step := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return Loaded{}, err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && readErr == nil {
			line = bytesTrimSpace(line)
			if len(line) > 0 {
				var kind struct {
					Kind string `json:"kind"`
				}
				if err := json.Unmarshal(line, &kind); err != nil {
					return Loaded{}, fmt.Errorf("%w: %v", ErrCorruptLog, err)
				}
				switch kind.Kind {
				case "header":
					if sawHeader {
						return Loaded{}, fmt.Errorf("%w: duplicate header", ErrCorruptLog)
					}
					var record headerRecord
					if err := json.Unmarshal(line, &record); err != nil {
						return Loaded{}, fmt.Errorf("%w: header: %v", ErrCorruptLog, err)
					}
					loaded.Header = record.Header
					sawHeader = true
				case "event":
					if !sawHeader {
						return Loaded{}, fmt.Errorf("%w: event before header", ErrCorruptLog)
					}
					var record eventRecord
					if err := json.Unmarshal(line, &record); err != nil {
						return Loaded{}, fmt.Errorf("%w: event: %v", ErrCorruptLog, err)
					}
					if err := session.ValidateDurableEvent(record.Type, record.Data); err != nil {
						return Loaded{}, fmt.Errorf("%w: event %d: %w", ErrCorruptLog, record.Seq, err)
					}
					if record.Seq != lastSeq+1 {
						return Loaded{}, fmt.Errorf("%w: sequence %d after %d", ErrCorruptLog, record.Seq, lastSeq)
					}
					lastSeq = record.Seq
					switch record.Type {
					case session.EventTurnStart:
						if turn != 0 {
							return Loaded{}, fmt.Errorf("%w: lifecycle: turn/start while turn %d is open", ErrCorruptLog, turn)
						}
						turn++
					case session.EventTurnEnd:
						if turn == 0 || step != 0 {
							return Loaded{}, fmt.Errorf("%w: lifecycle: turn/end without a closed step", ErrCorruptLog)
						}
						turn = 0
					case session.EventStepStart:
						if turn == 0 || step != 0 {
							return Loaded{}, fmt.Errorf("%w: lifecycle: step/start outside a turn or with step open", ErrCorruptLog)
						}
						step++
					case session.EventStepEnd:
						if turn == 0 || step == 0 {
							return Loaded{}, fmt.Errorf("%w: lifecycle: step/end without step/start", ErrCorruptLog)
						}
						step = 0
					}
					if record.Seq >= fromSeq {
						loaded.Events = append(loaded.Events, session.Event{
							Seq: record.Seq, Type: record.Type, At: record.At, Version: record.Version,
							Data: append(json.RawMessage(nil), record.Data...),
						})
					}
				default:
					return Loaded{}, fmt.Errorf("%w: unknown record %q", ErrCorruptLog, kind.Kind)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Loaded{}, readErr
		}
	}
	if !sawHeader {
		return Loaded{}, fmt.Errorf("%w: missing header", ErrCorruptLog)
	}
	if loaded.Header.Version != FormatVersion {
		return Loaded{}, fmt.Errorf("%w: %d", ErrFormatUnsupported, loaded.Header.Version)
	}
	if loaded.Header.ID != id {
		return Loaded{}, fmt.Errorf("%w: header id %q does not match %q", ErrCorruptLog, loaded.Header.ID, id)
	}
	loaded.Revision = lastSeq
	loaded.RevisionToken, err = jsonlRevisionToken(path)
	if err != nil {
		return Loaded{}, err
	}
	return loaded, nil
}

func repairInterruptedLocked(path string, loaded *Loaded) error {
	closers, err := session.InterruptedTurnClosers(loaded.Events)
	if err != nil || len(closers) == 0 {
		return nil
	}
	next := uint64(1)
	if len(loaded.Events) > 0 {
		next = loaded.Events[len(loaded.Events)-1].Seq + 1
	}
	recovery := make([]session.Event, 0, len(closers))
	for _, event := range closers {
		if event.Seq != next {
			return fmt.Errorf("%w: recovery sequence %d after %d", ErrCorruptLog, event.Seq, next-1)
		}
		recovery = append(recovery, event)
		next++
	}
	if err := appendJSONLEvents(path, recovery); err != nil {
		return err
	}
	loaded.Events = append(loaded.Events, recovery...)
	return nil
}

func validateEvents(events []session.Event, previous uint64) error {
	for _, event := range events {
		if event.Seq != previous+1 {
			return fmt.Errorf("%w: sequence %d after %d", ErrCorruptLog, event.Seq, previous)
		}
		if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
			return fmt.Errorf("%w: event %d: %w", ErrCorruptLog, event.Seq, err)
		}
		previous = event.Seq
	}
	return nil
}

func eventRecordFrom(event session.Event) eventRecord {
	return eventRecord{Kind: "event", Seq: event.Seq, Type: event.Type, At: event.At, Version: event.Version, Data: event.Data}
}

func writeRecord(writer io.Writer, value any) error {
	return writeRecordWithFault(writer, value, 0)
}

func writeRecordWithFault(writer io.Writer, value any, writeIndex int) error {
	if hook := jsonlWriteFaultHook.Load(); hook != nil {
		if err := (*hook)(writeIndex); err != nil {
			return err
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func bytesTrimSpace(in []byte) []byte { return []byte(strings.TrimSpace(string(in))) }
