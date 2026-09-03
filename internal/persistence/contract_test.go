package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

type persistenceBackend struct {
	persistence SessionPersistence
	close       func()
}

func openContractBackends(t *testing.T) []struct {
	name string
	open func(*testing.T) persistenceBackend
} {
	return []struct {
		name string
		open func(*testing.T) persistenceBackend
	}{
		{
			name: "jsonl",
			open: func(t *testing.T) persistenceBackend {
				p, err := OpenJSONL(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				return persistenceBackend{persistence: p}
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) persistenceBackend {
				st, err := store.OpenSQLite(t.TempDir() + "/sessions.db")
				if err != nil {
					t.Fatal(err)
				}
				return persistenceBackend{persistence: SQLiteAdapter{Store: st}, close: func() { _ = st.Close() }}
			},
		},
	}
}

func TestSessionPersistenceBackendsShareLifecycleContract(t *testing.T) {
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			created := time.Date(2026, 8, 28, 12, 0, 0, 123, time.UTC)
			header := Header{ID: "contract-parent", CreatedAt: created, CWD: "/workspace", AgentPreset: "code"}
			seed := contractEvents(t)
			if err := b.persistence.Create(ctx, Header{ID: "invalid-seed", SeedLength: len(seed) - 1}, seed); err == nil {
				t.Fatal("seed/header boundary mismatch was accepted")
			}
			if err := b.persistence.Create(ctx, header, nil); err != nil {
				t.Fatal(err)
			}
			events := contractEvents(t)
			if err := b.persistence.Append(ctx, header.ID, events); err != nil {
				t.Fatal(err)
			}
			if err := b.persistence.Checkpoint(ctx, header.ID); err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			if err := b.persistence.Flush(ctx, header.ID); err != nil {
				t.Fatalf("flush: %v", err)
			}
			loaded, err := b.persistence.Load(ctx, header.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Header.Version != FormatVersion || loaded.Header.ID != header.ID || loaded.Header.CWD != header.CWD || loaded.Revision != uint64(len(events)) {
				t.Fatalf("loaded header = %+v", loaded.Header)
			}
			if loaded.RevisionToken == "" {
				t.Fatalf("%s returned an empty opaque revision token", backend.name)
			}
			if !reflect.DeepEqual(loaded.Events, events) {
				t.Fatalf("loaded events differ:\nwant=%+v\n got=%+v", events, loaded.Events)
			}
			inspected, err := b.persistence.Inspect(ctx, header.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inspected, loaded) {
				t.Fatalf("inspect differs from load: %+v vs %+v", inspected, loaded)
			}
			from, err := b.persistence.ReadFrom(ctx, header.ID, events[2].Seq)
			if err != nil {
				t.Fatal(err)
			}
			if len(from.Events) != len(events)-2 || from.Events[0].Seq != events[2].Seq {
				t.Fatalf("readFrom = %+v, want suffix from seq %d", from.Events, events[2].Seq)
			}
			snapshots, err := b.persistence.ListSnapshots(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var found *Snapshot
			for index := range snapshots {
				if snapshots[index].Header.ID == header.ID {
					found = &snapshots[index]
					break
				}
			}
			if found == nil || found.Revision != uint64(len(events)) {
				t.Fatalf("snapshots = %+v, want %s at revision %d", snapshots, header.ID, len(events))
			}
			if found.RevisionToken != loaded.RevisionToken {
				t.Fatalf("snapshot token %q differs from loaded token %q", found.RevisionToken, loaded.RevisionToken)
			}

			tokenID := "revision-token"
			if err := b.persistence.Create(ctx, Header{ID: tokenID}, nil); err != nil {
				t.Fatal(err)
			}
			if err := b.persistence.Append(ctx, tokenID, []session.Event{{
				Seq: 1, Type: session.EventFeedbackRecord, Version: session.EventVersion,
				At: created, Data: []byte(`{"text":"before"}`),
			}}); err != nil {
				t.Fatal(err)
			}
			beforeToken, err := b.persistence.Load(ctx, tokenID)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.persistence.Append(ctx, tokenID, []session.Event{{
				Seq: 2, Type: session.EventFeedbackRecord, Version: session.EventVersion,
				At: created, Data: []byte(`{"text":"after"}`),
			}}); err != nil {
				t.Fatal(err)
			}
			afterToken, err := b.persistence.Load(ctx, tokenID)
			if err != nil {
				t.Fatal(err)
			}
			if beforeToken.RevisionToken == afterToken.RevisionToken {
				t.Fatalf("%s opaque revision did not change after append: %q", backend.name, beforeToken.RevisionToken)
			}

			if err := b.persistence.Append(ctx, header.ID, events); err != nil {
				t.Fatalf("idempotent replay: %v", err)
			}
			conflict := events[2]
			conflict.Data = []byte(`{"text":"conflict"}`)
			if err := b.persistence.Append(ctx, header.ID, []session.Event{conflict}); err == nil {
				t.Fatal("conflicting replay was accepted")
			}

			childID := "contract-child"
			if err := b.persistence.Fork(ctx, header.ID, Header{ID: childID}); err != nil {
				t.Fatal(err)
			}
			child, err := b.persistence.Load(ctx, childID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Header.Parent != header.ID || child.Header.SeedLength != len(events) || child.Header.Origin != "fork" || child.Header.DelegationDepth != 1 {
				t.Fatalf("child lineage = %+v", child.Header)
			}
			if !reflect.DeepEqual(child.Events, events) {
				t.Fatalf("child seed differs")
			}

			log, opened, err := b.persistence.OpenLog(ctx, childID, Header{ID: childID})
			if err != nil {
				t.Fatal(err)
			}
			if opened.ID != childID || log.NextSeq() != uint64(len(events)+1) {
				t.Fatalf("opened header/log = %+v / next=%d", opened, log.NextSeq())
			}
			if _, err := log.Append(session.EventTurnStart, session.NewTurnStart()); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(session.EventStepStart, session.NewStepStart(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(session.EventStepEnd, session.NewStepEnd(1, "completed", "")); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(session.EventTurnEnd, session.NewTurnEnd("completed", "")); err != nil {
				t.Fatal(err)
			}
			if got, err := b.persistence.Load(ctx, childID); err != nil || len(got.Events) != len(events)+4 {
				t.Fatalf("sink append count = %d, err=%v", len(got.Events), err)
			}

			if err := b.persistence.Create(ctx, Header{ID: header.ID}, nil); err == nil {
				t.Fatal("duplicate create was accepted")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := b.persistence.Flush(cancelled, header.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("flush cancellation = %v", err)
			}
		})
	}
}

func TestSessionPersistenceBackendsRejectUnknownDurableEvents(t *testing.T) {
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			id := "unknown-durable-event"
			if err := b.persistence.Create(ctx, Header{ID: id}, nil); err != nil {
				t.Fatal(err)
			}
			err := b.persistence.Append(ctx, id, []session.Event{{
				Seq: 1, Type: "future/required-event", Version: session.EventVersion,
				At: time.Now().UTC(), Data: []byte(`{"value":true}`),
			}})
			if !errors.Is(err, session.ErrUnknownRequiredEvent) {
				t.Fatalf("unknown durable append = %v, want ErrUnknownRequiredEvent", err)
			}
			loaded, loadErr := b.persistence.Load(ctx, id)
			if loadErr != nil {
				t.Fatalf("load after rejected append: %v", loadErr)
			}
			if len(loaded.Events) != 0 {
				t.Fatalf("rejected append changed durable events: %+v", loaded.Events)
			}
		})
	}
}

func TestPersistenceBackendsReplaySharedCoreFixture(t *testing.T) {
	records, err := contractfixture.CoreTurnEvents()
	if err != nil {
		t.Fatalf("load shared fixture: %v", err)
	}
	events := make([]session.Event, 0, len(records))
	for _, event := range records {
		events = append(events, session.Event{
			Seq: event.Seq, Type: event.Type, Version: session.EventVersion,
			At: time.UnixMilli(event.Time).UTC(), Data: append(json.RawMessage(nil), event.Data...),
		})
	}
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			id := "shared-core-fixture"
			if err := b.persistence.Create(ctx, Header{ID: id}, nil); err != nil {
				t.Fatal(err)
			}
			if err := b.persistence.Append(ctx, id, events); err != nil {
				t.Fatalf("append fixture: %v", err)
			}
			loaded, err := b.persistence.Load(ctx, id)
			if err != nil {
				t.Fatalf("load fixture: %v", err)
			}
			if !reflect.DeepEqual(loaded.Events, events) {
				t.Fatalf("loaded fixture differs:\nwant=%+v\n got=%+v", events, loaded.Events)
			}
			log := session.New()
			if err := log.Restore(loaded.Events); err != nil {
				t.Fatalf("restore loaded fixture: %v", err)
			}
			history := log.DeriveHistory()
			if len(history) != 3 || history[0].Role != llm.RoleUser || history[1].Role != llm.RoleAssistant || history[2].Role != llm.RoleTool {
				t.Fatalf("derived fixture history = %+v", history)
			}
		})
	}
}

// TestPersistenceBackendsReplaySurfaceReplacementAndRejectBadColdSeeds owns
// the A1.4 cold-seed boundary. A provenance-complete compaction replacement
// must produce the same model history live, after cold replay, and after fork;
// an under-cited replacement must fail before it can become durable state.
func TestPersistenceBackendsReplaySurfaceReplacementAndRejectBadColdSeeds(t *testing.T) {
	newSurfaceLog := func(t *testing.T) *session.Log {
		t.Helper()
		log := session.New()
		append := func(typ string, data any) {
			t.Helper()
			if _, err := log.Append(typ, data); err != nil {
				t.Fatalf("append %s: %v", typ, err)
			}
		}
		append(session.EventTurnStart, session.NewTurnStartAt(1))
		append(session.EventStepStart, session.NewStepStartAt(1, 1))
		append(session.EventUserMessage, session.NewUserMessage("source"))
		append(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(
			1, 1, "answer", nil, "stop", "", llm.TokenUsage{},
		))
		append(session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", ""))
		append(session.EventTurnEnd, session.NewTurnEndAt(1, "completed", ""))
		append(session.EventCompactionStart, session.NewCompactionStart("pressure", "pre-step"))
		append(session.EventCompactionSummary, session.NewCompactionSummaryWithStats(
			"compaction-1", "summary", []int64{3, 4}, 19, "pre-step",
		))
		append(session.EventCompactionEnd, session.NewCompactionEnd("compaction-1", [2]int64{3, 4}, 19))
		append(session.EventUserMessage, session.NewUserMessageReplaceWithSources(
			"compacted source and answer", 3, 4, []uint64{3, 4},
		))
		append(session.EventTurnStart, session.NewTurnStartAt(2))
		append(session.EventStepStart, session.NewStepStartAt(2, 1))
		append(session.EventUserMessage, session.NewUserMessage("tail"))
		append(session.EventStepEnd, session.NewStepEndAt(2, 1, "completed", ""))
		append(session.EventTurnEnd, session.NewTurnEndAt(2, "completed", ""))
		return log
	}
	historyText := func(t *testing.T, log *session.Log) []string {
		t.Helper()
		texts := make([]string, 0)
		for _, message := range log.DeriveHistory() {
			texts = append(texts, message.Text())
		}
		return texts
	}

	valid := newSurfaceLog(t).Events()
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			header := Header{ID: "surface-parent", SeedLength: 0}
			if err := b.persistence.Create(ctx, header, nil); err != nil {
				t.Fatal(err)
			}
			if err := b.persistence.Append(ctx, header.ID, valid); err != nil {
				t.Fatal(err)
			}
			loaded, err := b.persistence.Load(ctx, header.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Events, valid) {
				t.Fatalf("cold events differ: got=%+v", loaded.Events)
			}
			cold := session.New()
			if err := cold.Restore(loaded.Events); err != nil {
				t.Fatalf("cold replay: %v", err)
			}
			if got, want := historyText(t, cold), historyText(t, newSurfaceLog(t)); !reflect.DeepEqual(got, want) {
				t.Fatalf("cold history = %#v, want %#v", got, want)
			}
			if got := historyText(t, cold); len(got) != 2 || got[0] != "compacted source and answer" || got[1] != "tail" {
				t.Fatalf("replacement history = %#v, want summary plus tail", got)
			}

			childID := "surface-child"
			if err := b.persistence.Fork(ctx, header.ID, Header{ID: childID}); err != nil {
				t.Fatal(err)
			}
			child, err := b.persistence.Load(ctx, childID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Header.SeedLength != len(valid) || child.Header.Parent != header.ID {
				t.Fatalf("fork seed header = %+v", child.Header)
			}
			childLog := session.New()
			if err := childLog.Restore(child.Events); err != nil {
				t.Fatalf("fork replay: %v", err)
			}
			if got, want := historyText(t, childLog), historyText(t, newSurfaceLog(t)); !reflect.DeepEqual(got, want) {
				t.Fatalf("fork history = %#v, want %#v", got, want)
			}

			raw, err := json.Marshal(session.NewUserMessageReplaceWithSources(
				"under-cited", 3, 4, []uint64{},
			))
			if err != nil {
				t.Fatal(err)
			}
			invalid := session.Event{
				Seq: uint64(len(valid) + 1), Type: session.EventUserMessage,
				Version: session.EventVersion, At: time.Now().UTC(), Data: raw,
			}
			if err := b.persistence.Append(ctx, header.ID, []session.Event{invalid}); err == nil {
				t.Fatal("append accepted an under-cited surface replacement")
			}
			after, err := b.persistence.Load(ctx, header.ID)
			if err != nil {
				t.Fatalf("reload after rejected append: %v", err)
			}
			if !reflect.DeepEqual(after.Events, valid) {
				t.Fatalf("rejected append mutated durable state: got=%+v", after.Events)
			}

			badSeed := append(append([]session.Event(nil), valid...), invalid)
			if err := b.persistence.Create(ctx, Header{ID: "bad-cold-seed", SeedLength: len(badSeed)}, badSeed); err == nil {
				t.Fatal("create accepted an under-cited cold seed")
			}
			if _, err := b.persistence.Load(ctx, "bad-cold-seed"); err == nil {
				t.Fatal("rejected cold seed remained loadable")
			}
		})
	}
}

func TestSessionPersistenceColdInspectAndReadFromPreserveOpenTailContract(t *testing.T) {
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			id := "open-tail"
			if err := b.persistence.Create(ctx, Header{ID: id}, nil); err != nil {
				t.Fatal(err)
			}
			log := session.New()
			for _, item := range []struct {
				typ  string
				data any
			}{
				{session.EventTurnStart, session.NewTurnStartAt(1)},
				{session.EventStepStart, session.NewStepStartAt(1, 1)},
				{session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "", []llm.ToolCall{{ID: "call-x", Name: "bash", Arguments: "{}"}}, "tool-calls", "", llm.TokenUsage{})},
			} {
				if _, err := log.Append(item.typ, item.data); err != nil {
					t.Fatal(err)
				}
			}
			physical := log.Events()
			if err := b.persistence.Append(ctx, id, physical); err != nil {
				t.Fatal(err)
			}
			before, err := b.persistence.ListSnapshots(ctx)
			if err != nil {
				t.Fatal(err)
			}
			beforeRevision := snapshotRevision(t, before, id)

			inspected, err := b.persistence.Inspect(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(inspected.Events) != len(physical)+3 {
				t.Fatalf("inspect events = %d, want %d", len(inspected.Events), len(physical)+3)
			}
			if inspected.Events[len(physical)].Type != session.EventToolResult || inspected.Events[len(physical)+1].Type != session.EventStepEnd || inspected.Events[len(physical)+2].Type != session.EventTurnEnd {
				t.Fatalf("inspect closers = %+v", inspected.Events[len(physical):])
			}
			var result struct {
				Error struct {
					Name string `json:"name"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(inspected.Events[len(physical)].Data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Name != "ToolNotStartedError" || result.Error.Code != "TOOL_NOT_STARTED" {
				t.Fatalf("inspect synthetic result = %+v", result.Error)
			}
			afterInspect, err := b.persistence.ListSnapshots(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := snapshotRevision(t, afterInspect, id); got != beforeRevision {
				t.Fatalf("inspect changed durable revision from %d to %d", beforeRevision, got)
			}

			raw, err := b.persistence.ReadFrom(ctx, id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw.Events, physical) {
				t.Fatalf("readFrom returned logical recovery instead of raw tail:\nwant=%+v\n got=%+v", physical, raw.Events)
			}
			loaded, err := b.persistence.Load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Events) != len(physical)+3 || loaded.Revision != inspected.Events[len(inspected.Events)-1].Seq {
				t.Fatalf("load recovery = revision %d events %d", loaded.Revision, len(loaded.Events))
			}
		})
	}
}

func TestSessionPersistenceForkDoesNotMaterializeRecoveryClosers(t *testing.T) {
	for _, backend := range openContractBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			b := backend.open(t)
			if b.close != nil {
				defer b.close()
			}
			ctx := context.Background()
			parentID := "fork-open-parent"
			if err := b.persistence.Create(ctx, Header{ID: parentID}, nil); err != nil {
				t.Fatal(err)
			}
			log := session.New()
			for _, item := range []struct {
				typ  string
				data any
			}{
				{session.EventTurnStart, session.NewTurnStart()},
				{session.EventStepStart, session.NewStepStart(1)},
				{session.EventStepEnd, session.NewStepEnd(1, "completed", "")},
				{session.EventTurnEnd, session.NewTurnEnd("completed", "")},
				{session.EventTurnStart, session.NewTurnStart()},
				{session.EventStepStart, session.NewStepStart(1)},
			} {
				if _, err := log.Append(item.typ, item.data); err != nil {
					t.Fatal(err)
				}
			}
			physical := log.Events()
			if err := b.persistence.Append(ctx, parentID, physical); err != nil {
				t.Fatal(err)
			}

			if err := b.persistence.Fork(ctx, parentID, Header{ID: "fork-open-child"}); err == nil {
				t.Fatal("fork unexpectedly accepted an open durable parent")
			}
			if _, err := b.persistence.Load(ctx, "fork-open-child"); err == nil {
				t.Fatal("failed fork published a child")
			}
			inspected, err := b.persistence.ReadFrom(ctx, parentID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inspected.Events, physical) {
				t.Fatalf("fork mutated/repaired parent: got %d events, want %d", len(inspected.Events), len(physical))
			}
		})
	}
}

func snapshotRevision(t *testing.T, snapshots []Snapshot, id string) uint64 {
	t.Helper()
	for _, snapshot := range snapshots {
		if snapshot.Header.ID == id {
			return snapshot.Revision
		}
	}
	t.Fatalf("session %q missing from snapshots", id)
	return 0
}

func contractEvents(t *testing.T) []session.Event {
	t.Helper()
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventStepStart, session.NewStepStart(1)},
		{session.EventUserMessage, session.NewUserMessage("hello")},
		{session.EventStepEnd, session.NewStepEnd(1, "completed", "")},
		{session.EventTurnEnd, session.NewTurnEnd("completed", "")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	return log.Events()
}
