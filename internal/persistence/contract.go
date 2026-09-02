package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

// SessionPersistence is the backend-independent transcript contract. Header
// metadata is storage-only; Events are the append-only source used to restore
// session.Log. Inspect must never repair or append data.
type SessionPersistence interface {
	Create(context.Context, Header, []session.Event) error
	Append(context.Context, string, []session.Event) error
	Flush(context.Context, string) error
	// Checkpoint is the explicit durability boundary used by write-behind
	// callers. The current adapters use synchronous append, so a successful
	// append already satisfies this barrier; keeping it in the shared contract
	// prevents an adapter from silently weakening that guarantee later.
	Checkpoint(context.Context, string) error
	Load(context.Context, string) (Loaded, error)
	Inspect(context.Context, string) (Loaded, error)
	ReadFrom(context.Context, string, uint64) (Loaded, error)
	ListSnapshots(context.Context) ([]Snapshot, error)
	Fork(context.Context, string, Header) error
	OpenLog(context.Context, string, Header) (*session.Log, Header, error)
}

var _ SessionPersistence = (*JSONL)(nil)

// SQLiteAdapter gives the production SQLite Store the same transcript
// contract as JSONL without making store depend on this package. The adapter
// deliberately uses Store's public append/load seam, so SQLite transaction and
// idempotency rules remain authoritative.
type SQLiteAdapter struct{ Store store.Store }

// Snapshot is the lightweight reconnect/listing view of one persisted
// session. Revision is retained as a numeric cursor for compatibility;
// RevisionToken is the source-qualified opaque comparison value.
type Snapshot struct {
	Header        Header
	Revision      uint64
	RevisionToken string
}

func (a SQLiteAdapter) Create(ctx context.Context, header Header, seed []session.Event) error {
	if a.Store == nil {
		return store.ErrNotFound
	}
	var err error
	header, err = normalizeHeader(header)
	if err != nil {
		return err
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
	if _, err := a.Load(ctx, header.ID); err == nil {
		return fmt.Errorf("persistence: session %q already exists", header.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = time.Now().UTC()
	}
	if atomic, ok := a.Store.(store.SessionCreateEventStore); ok {
		return atomic.CreateSessionWithEvents(ctx, header.ID, header.CreatedAt, toStoreHeader(header), seed)
	}
	if err := a.Store.CreateSession(ctx, header.ID, header.CreatedAt); err != nil {
		return err
	}
	if lineage, ok := a.Store.(store.SessionLineageStore); ok {
		if err := lineage.SetSessionHeader(ctx, header.ID, toStoreHeader(header)); err != nil {
			return err
		}
	}
	if len(seed) > 0 {
		return a.Store.AppendEvents(ctx, header.ID, seed)
	}
	return nil
}

func (a SQLiteAdapter) Append(ctx context.Context, id string, events []session.Event) error {
	if a.Store == nil {
		return store.ErrNotFound
	}
	existing := make([]session.Event, 0)
	if raw, ok := a.Store.(store.SessionRawStore); ok {
		loaded, err := raw.LoadSessionRaw(ctx, id)
		if err == nil {
			existing = loaded
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	} else {
		loaded, err := a.Store.LoadSession(ctx, id)
		if err == nil {
			existing = loaded
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	combined := append(append([]session.Event(nil), existing...), events...)
	if err := session.ValidateEventProvenance(combined); err != nil {
		return fmt.Errorf("persistence: append provenance: %w", err)
	}
	return a.Store.AppendEvents(ctx, id, events)
}

func (a SQLiteAdapter) Flush(ctx context.Context, id string) error {
	if a.Store == nil {
		return store.ErrNotFound
	}
	if flusher, ok := a.Store.(store.SessionFlushStore); ok {
		return flusher.Flush(ctx, id)
	}
	return a.Checkpoint(ctx, id)
}

func (a SQLiteAdapter) Checkpoint(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if raw, ok := a.Store.(store.SessionRawStore); ok {
		_, err := raw.LoadSessionRaw(ctx, id)
		return err
	}
	if _, err := a.Inspect(ctx, id); err != nil {
		return err
	}
	return nil
}

func (a SQLiteAdapter) Load(ctx context.Context, id string) (Loaded, error) {
	return a.load(ctx, id)
}

func (a SQLiteAdapter) Inspect(ctx context.Context, id string) (Loaded, error) {
	// A cold inspection returns a balanced logical view but must not append its
	// synthetic closers. Prefer the raw seam because the strict store inspect
	// path intentionally rejects an open live tail.
	if raw, ok := a.Store.(store.SessionRawStore); ok {
		return a.loadEventsWithRecovery(ctx, id, raw.LoadSessionRaw)
	}
	if inspected, ok := a.Store.(store.SessionInspectStore); ok {
		return a.loadEvents(ctx, id, inspected.InspectSession)
	}
	return a.load(ctx, id)
}

func (a SQLiteAdapter) ReadFrom(ctx context.Context, id string, fromSeq uint64) (Loaded, error) {
	if fromSeq == 0 {
		if raw, ok := a.Store.(store.SessionRawStore); ok {
			// readFrom is a physical cursor operation, not logical recovery: an
			// open tail and its exact durable revision must remain observable.
			return a.loadEventsRaw(ctx, id, raw.LoadSessionRaw)
		}
	}
	if fromSeq > 0 {
		if suffix, ok := a.Store.(store.SessionSuffixStore); ok {
			header, err := a.loadHeader(ctx, id)
			if err != nil {
				return Loaded{}, err
			}
			events := make([]session.Event, 0)
			cursor := fromSeq
			for {
				page, more, err := suffix.LoadSessionFrom(ctx, id, cursor, 512)
				if err != nil {
					return Loaded{}, err
				}
				for _, event := range page {
					if event.Seq != cursor {
						return Loaded{}, fmt.Errorf("%w: sequence %d after %d", ErrCorruptLog, event.Seq, cursor-1)
					}
					events = append(events, event)
					cursor = event.Seq + 1
				}
				if !more {
					break
				}
				if len(page) == 0 {
					return Loaded{}, fmt.Errorf("%w: empty paged suffix for %q", ErrCorruptLog, id)
				}
			}
			revision := uint64(0)
			if revisions, ok := a.Store.(store.SessionRevisionStore); ok {
				revision, err = revisions.SessionRevision(ctx, id)
				if err != nil {
					return Loaded{}, err
				}
			} else if len(events) > 0 {
				revision = events[len(events)-1].Seq
			}
			return a.withRevisionToken(ctx, id, Loaded{Header: header, Events: events, Revision: revision})
		}
	}
	loaded, err := a.Inspect(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	loaded.Events = eventsFrom(loaded.Events, fromSeq)
	return loaded, nil
}

func (a SQLiteAdapter) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	if a.Store == nil {
		return nil, store.ErrNotFound
	}
	metas, err := a.Store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(metas))
	for _, meta := range metas {
		header := Header{ID: meta.ID, CreatedAt: meta.CreatedAt, CWD: meta.CWD, Version: FormatVersion}
		if lineage, ok := a.Store.(store.SessionLineageStore); ok {
			if saved, headerErr := lineage.GetSessionHeader(ctx, meta.ID); headerErr == nil {
				header = fromStoreHeader(saved)
			} else if !errors.Is(headerErr, store.ErrNotFound) {
				return nil, headerErr
			}
		}
		revision := uint64(meta.EventCount)
		if revisions, ok := a.Store.(store.SessionRevisionStore); ok {
			if saved, revisionErr := revisions.SessionRevision(ctx, meta.ID); revisionErr == nil {
				revision = saved
			} else {
				return nil, revisionErr
			}
		}
		snapshot := Snapshot{Header: header, Revision: revision}
		if tokens, ok := a.Store.(store.SessionRevisionTokenStore); ok {
			var tokenErr error
			snapshot.RevisionToken, tokenErr = tokens.SessionRevisionToken(ctx, meta.ID)
			if tokenErr != nil {
				return nil, tokenErr
			}
		}
		out = append(out, snapshot)
	}
	return out, nil
}

func (a SQLiteAdapter) load(ctx context.Context, id string) (Loaded, error) {
	return a.loadEvents(ctx, id, a.Store.LoadSession)
}

func (a SQLiteAdapter) loadEvents(ctx context.Context, id string, read func(context.Context, string) ([]session.Event, error)) (Loaded, error) {
	if a.Store == nil {
		return Loaded{}, store.ErrNotFound
	}
	header, err := a.loadHeader(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	/*
		The event read below is intentionally full: Load validates the complete
		lifecycle and is the recovery path. ReadFrom above is the non-mutating
		seek path and therefore validates only its contiguous returned suffix.
	*/
	events, err := read(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	if err := validateEvents(events, 0); err != nil {
		return Loaded{}, err
	}
	if err := session.ValidateLifecycle(events); err != nil {
		return Loaded{}, fmt.Errorf("%w: lifecycle: %v", ErrCorruptLog, err)
	}
	var revision uint64
	if len(events) > 0 {
		revision = events[len(events)-1].Seq
	}
	return a.withRevisionToken(ctx, id, Loaded{Header: header, Events: events, Revision: revision})
}

func (a SQLiteAdapter) loadEventsWithRecovery(ctx context.Context, id string, read func(context.Context, string) ([]session.Event, error)) (Loaded, error) {
	loaded, err := a.loadEventsRaw(ctx, id, read)
	if err != nil {
		return Loaded{}, err
	}
	closers, err := session.InterruptedTurnClosers(loaded.Events)
	if err != nil {
		return Loaded{}, fmt.Errorf("%w: lifecycle: %v", ErrCorruptLog, err)
	}
	loaded.Events = append(append([]session.Event(nil), loaded.Events...), closers...)
	return loaded, nil
}

func (a SQLiteAdapter) loadEventsRaw(ctx context.Context, id string, read func(context.Context, string) ([]session.Event, error)) (Loaded, error) {
	if a.Store == nil {
		return Loaded{}, store.ErrNotFound
	}
	header, err := a.loadHeader(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	events, err := read(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	if err := validateEvents(events, 0); err != nil {
		return Loaded{}, err
	}
	var revision uint64
	if len(events) > 0 {
		revision = events[len(events)-1].Seq
	}
	return a.withRevisionToken(ctx, id, Loaded{Header: header, Events: events, Revision: revision})
}

func (a SQLiteAdapter) withRevisionToken(ctx context.Context, id string, loaded Loaded) (Loaded, error) {
	if tokens, ok := a.Store.(store.SessionRevisionTokenStore); ok {
		token, err := tokens.SessionRevisionToken(ctx, id)
		if err != nil {
			return Loaded{}, err
		}
		loaded.RevisionToken = token
	}
	return loaded, nil
}

func (a SQLiteAdapter) loadHeader(ctx context.Context, id string) (Header, error) {
	if a.Store == nil {
		return Header{}, store.ErrNotFound
	}
	header := Header{ID: id, Version: FormatVersion}
	if lineage, ok := a.Store.(store.SessionLineageStore); ok {
		h, err := lineage.GetSessionHeader(ctx, id)
		if err != nil {
			return Header{}, err
		}
		header = fromStoreHeader(h)
	} else {
		meta, err := a.Store.GetSessionMeta(ctx, id)
		if err != nil {
			return Header{}, err
		}
		header.CreatedAt, header.CWD = meta.CreatedAt, meta.CWD
	}
	header.Version = FormatVersion
	if header.ID != id {
		return Header{}, fmt.Errorf("%w: header id %q does not match %q", ErrCorruptLog, header.ID, id)
	}
	return header, nil
}

func eventsFrom(events []session.Event, fromSeq uint64) []session.Event {
	if fromSeq == 0 {
		return append([]session.Event(nil), events...)
	}
	index := 0
	for index < len(events) && events[index].Seq < fromSeq {
		index++
	}
	return append([]session.Event(nil), events[index:]...)
}

func (a SQLiteAdapter) Fork(ctx context.Context, parentID string, child Header) error {
	// Forking is a lineage operation, not crash recovery. A recovered
	// `interrupted` tail is a projection of an abandoned parent turn and must
	// never become a child's constructor seed. Read the physical transcript and
	// require it to be a closed lifecycle instead.
	parent, err := a.ReadFrom(ctx, parentID, 0)
	if err != nil {
		return err
	}
	if err := session.ValidateLifecycle(parent.Events); err != nil {
		return err
	}
	child.Parent = parentID
	child.SeedLength = len(parent.Events)
	if child.Origin == "" {
		child.Origin = "fork"
	}
	if child.DelegationDepth <= parent.Header.DelegationDepth {
		child.DelegationDepth = parent.Header.DelegationDepth + 1
	}
	return a.Create(ctx, child, parent.Events)
}

func normalizeHeader(header Header) (Header, error) {
	if err := validateID(header.ID); err != nil {
		return Header{}, err
	}
	if header.Version == 0 {
		header.Version = FormatVersion
	}
	if header.Version != FormatVersion {
		return Header{}, fmt.Errorf("%w: %d", ErrFormatUnsupported, header.Version)
	}
	if header.SeedLength < 0 || header.DelegationDepth < 0 {
		return Header{}, fmt.Errorf("%w: negative lineage metadata", ErrCorruptLog)
	}
	return header, nil
}

func (a SQLiteAdapter) OpenLog(ctx context.Context, id string, header Header) (*session.Log, Header, error) {
	loaded, err := a.Load(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, Header{}, err
		}
		header.ID = id
		if err := a.Create(ctx, header, nil); err != nil {
			return nil, Header{}, err
		}
		loaded, err = a.Load(ctx, id)
		if err != nil {
			return nil, Header{}, err
		}
	}
	log := session.New()
	if err := log.Restore(loaded.Events); err != nil {
		return nil, Header{}, err
	}
	log.SetSink(func(event session.Event) error { return a.Append(ctx, id, []session.Event{event}) })
	return log, loaded.Header, nil
}

func toStoreHeader(h Header) store.SessionHeader {
	return store.SessionHeader{ID: h.ID, CreatedAt: h.CreatedAt, CWD: h.CWD, Parent: h.Parent, SeedLength: h.SeedLength, Origin: h.Origin, DelegationDepth: h.DelegationDepth, AgentPreset: h.AgentPreset}
}

func fromStoreHeader(h store.SessionHeader) Header {
	return Header{Version: FormatVersion, ID: h.ID, CreatedAt: h.CreatedAt, CWD: h.CWD, Parent: h.Parent, SeedLength: h.SeedLength, Origin: h.Origin, DelegationDepth: h.DelegationDepth, AgentPreset: h.AgentPreset}
}
