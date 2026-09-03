package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

const jsonlCrashHelperEnv = "SHUTU_JSONL_WRITE_FAULT_HELPER"

func TestJSONLWriteFaultSettlesAcrossProcessRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "disk full rolls back complete physical batch", mode: "disk-full"},
		{name: "process kill settles event-level prefix", mode: "kill-write"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			store, err := OpenJSONL(root)
			if err != nil {
				t.Fatal(err)
			}
			seed := []session.Event{
				{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1}`)},
				{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(2), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1}`)},
				{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(3), Version: session.EventVersion, Data: json.RawMessage(`{"text":"committed prompt"}`)},
				{Seq: 4, Type: session.EventStepEnd, At: time.UnixMilli(4), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"step":1,"status":"completed"}`)},
				{Seq: 5, Type: session.EventTurnEnd, At: time.UnixMilli(5), Version: session.EventVersion, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
			}
			if err := store.Create(ctx, Header{ID: "write-fault", CWD: "/workspace", SeedLength: len(seed)}, seed); err != nil {
				t.Fatal(err)
			}
			path, err := store.Locate("write-fault")
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLWriteFaultHelper$")
			cmd.Env = append(os.Environ(),
				jsonlCrashHelperEnv+"="+tc.mode,
				"SHUTU_JSONL_CRASH_ROOT="+root,
			)
			runErr := cmd.Run()
			switch tc.mode {
			case "disk-full":
				if runErr != nil {
					t.Fatalf("disk-full helper: %v", runErr)
				}
			case "kill-write":
				if runErr == nil {
					t.Fatal("kill helper unexpectedly exited successfully")
				}
			}

			// Read the physical file through a second independent handle. This
			// observes exactly what the failed process left behind; Inspect
			// returns interrupted-turn closers in its projected result.
			physical, err := physicalJSONLEvents(path)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.mode {
			case "disk-full":
				if len(physical) != 5 || physical[4].Seq != 5 || physical[4].Type != session.EventTurnEnd {
					t.Fatalf("disk-full external prefix = %+v, want exact committed prefix", physical)
				}
			case "kill-write":
				if len(physical) != 6 || physical[5].Seq != 6 || physical[5].Type != session.EventTurnStart {
					t.Fatalf("kill external prefix = %+v, want only the first complete batch event", physical)
				}
			}

			// A genuinely independent storage handle is the restart oracle.
			reopened, err := OpenJSONL(root)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := reopened.Load(ctx, "write-fault")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Header.ID != "write-fault" || len(loaded.Events) < 4 {
				t.Fatalf("restart load = header %+v events %+v", loaded.Header, loaded.Events)
			}
			wantEvents := 5
			if tc.mode == "kill-write" {
				wantEvents = 7
			}
			if len(loaded.Events) != wantEvents {
				t.Fatalf("restart events = %+v, want prefix plus deterministic lifecycle closers", loaded.Events)
			}
			wantTail := 0
			switch tc.mode {
			case "disk-full":
				wantTail = 0
			case "kill-write":
				wantTail = 2
			}
			if wantTail > 0 {
				tail := loaded.Events[len(loaded.Events)-wantTail:]
				if tail[0].Type != session.EventTurnStart || tail[1].Type != session.EventTurnEnd {
					t.Fatalf("kill recovered tail = %+v, want new turn and closer", tail)
				}
			}
			for _, event := range loaded.Events {
				if event.Seq == 6 && tc.mode == "disk-full" {
					t.Fatalf("rolled-back event survived restart: %+v", event)
				}
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(after, before) {
				t.Fatal("restart recovery rewrote the committed prefix")
			}
		})
	}
}

// TestJSONLWriteFaultHelper is launched only by
// TestJSONLWriteFaultSettlesAcrossProcessRestart. It exercises the public JSONL
// Append seam in a real second process; abrupt exit models process death before
// cleanup, while disk-full returns through the normal rollback path.
func TestJSONLWriteFaultHelper(t *testing.T) {
	mode := os.Getenv(jsonlCrashHelperEnv)
	if mode == "" {
		t.Skip("JSONL write-fault helper")
	}
	root := os.Getenv("SHUTU_JSONL_CRASH_ROOT")
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "disk-full":
		installJSONLWriteFaultForTest(func(writeIndex int) error {
			if writeIndex == 1 {
				return errors.New("injected ENOSPC")
			}
			return nil
		})
	case "kill-write":
		installJSONLWriteFaultForTest(func(writeIndex int) error {
			if writeIndex == 1 {
				// No rollback or close can run: model abrupt process death at
				// the second physical record boundary.
				os.Exit(9)
			}
			return nil
		})
	default:
		t.Fatalf("unknown write-fault mode %q", mode)
	}
	events := []session.Event{
		{Seq: 6, Type: session.EventTurnStart, At: time.UnixMilli(6), Version: session.EventVersion, Data: json.RawMessage(`{"turn":2}`)},
		{Seq: 7, Type: session.EventTurnEnd, At: time.UnixMilli(7), Version: session.EventVersion, Data: json.RawMessage(`{"turn":2,"reason":{"kind":"completed"}}`)},
	}
	if err := store.Append(context.Background(), "write-fault", events); err != nil {
		if mode == "disk-full" && strings.Contains(err.Error(), "injected ENOSPC") {
			return
		}
		t.Fatalf("helper append: %v", err)
	}
	if mode == "disk-full" {
		t.Fatal("injected disk-full did not fail Append")
	}
}

func physicalJSONLEvents(path string) ([]session.Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []session.Event
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record eventRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, err
		}
		if record.Kind != "event" {
			continue
		}
		events = append(events, session.Event{
			Seq: record.Seq, Type: record.Type, At: record.At,
			Version: record.Version, Data: append(json.RawMessage(nil), record.Data...),
		})
	}
	return events, nil
}

func TestJSONLCreateAppendLoadAndRepairInterruptedTurn(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), Header{ID: "s-1", CWD: "/workspace"}, nil); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, pair := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventStepStart, session.NewStepStart(1)},
		{session.EventUserMessage, session.NewUserMessage("hello")},
	} {
		if _, err := log.Append(pair.typ, pair.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append(context.Background(), "s-1", log.Events()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Header.CWD != "/workspace" || len(loaded.Events) != 5 || loaded.Events[3].Type != session.EventStepEnd || loaded.Events[4].Type != session.EventTurnEnd {
		t.Fatalf("loaded header/events = %+v / %+v", loaded.Header, loaded.Events)
	}
	if err := store.Append(context.Background(), "s-1", log.Events()); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	loaded, err = store.Load(context.Background(), "s-1")
	if err != nil || len(loaded.Events) != 5 {
		t.Fatalf("reloaded events = %d, err=%v", len(loaded.Events), err)
	}
}

func TestRollbackJSONLAppendRestoresCommittedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	prefix := []byte("committed-prefix\n")
	if err := os.WriteFile(path, append([]byte{}, prefix...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(prefix, []byte("partial-record")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackJSONLAppend(path, int64(len(prefix))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(prefix) {
		t.Fatalf("rolled-back bytes = %q, want %q", got, prefix)
	}
}

func TestJSONLTornTailIsIgnoredAndConflictsRejected(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), Header{ID: "tail"}, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tail.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("ok"))
	data := `{"kind":"event","seq":1,"type":"user/message","version":1,"data":{"text":"ok"}}` + "\n"
	_, _ = file.WriteString(data)
	_, _ = file.WriteString(`{"kind":"event","seq":2`)
	_ = file.Close()
	loaded, err := store.Load(context.Background(), "tail")
	if err != nil || len(loaded.Events) != 1 {
		t.Fatalf("torn load = %+v, err=%v", loaded.Events, err)
	}
	conflict := session.Event{Seq: 1, Type: session.EventUserMessage, Data: []byte(`{"text":"different"}`)}
	if err := store.Append(context.Background(), "tail", []session.Event{conflict}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting append error = %v", err)
	}
}

func TestJSONLReadFromIsBoundedAndDoesNotRepairTornTail(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), Header{ID: "cursor"}, nil); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, text := range []string{"one", "two", "three"} {
		if _, err := log.Append(session.EventUserMessage, session.NewUserMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append(context.Background(), "cursor", log.Events()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cursor.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"kind":"event","seq":4`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.ReadFrom(context.Background(), "cursor", 3)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 3 || len(loaded.Events) != 1 || string(loaded.Events[0].Data) == "" {
		t.Fatalf("cursor result = revision %d events %+v", loaded.Revision, loaded.Events)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("ReadFrom mutated the torn tail")
	}
}

func TestJSONLReplayConflictIncludesEventMetadata(t *testing.T) {
	store, err := OpenJSONL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), Header{ID: "metadata"}, nil); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	event, err := log.Append(session.EventUserMessage, session.NewUserMessage("same"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "metadata", []session.Event{event}); err != nil {
		t.Fatal(err)
	}
	conflict := event
	conflict.Version++
	if err := store.Append(context.Background(), "metadata", []session.Event{conflict}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("metadata conflict = %v", err)
	}
}

func TestJSONLRejectsPathTraversal(t *testing.T) {
	store, err := OpenJSONL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Locate("..\\escape"); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestJSONLProcessLockSerializesIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	first, err := acquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		second, err := acquireProcessLock(path)
		if err == nil {
			err = second.Close()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		_ = first.Close()
		t.Fatalf("second independent handle acquired lock early, err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second lock after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second lock did not acquire after release")
	}
}

func TestJSONLProcessLockHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.lock")
	first, err := acquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second, err := acquireProcessLockContext(ctx, path)
	if second != nil {
		_ = second.Close()
		t.Fatal("cancelled JSONL lock unexpectedly acquired")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled JSONL lock error = %v, want context.Canceled", err)
	}
}

func TestJSONLRecoveryAfterChildProcessExit(t *testing.T) {
	if os.Getenv("SHUTU_JSONL_CRASH_CHILD") == "1" {
		path := os.Getenv("SHUTU_JSONL_CRASH_PATH")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.WriteString(`{"kind":"event","seq":4`)
		_ = file.Sync()
		// Deliberately skip cleanup: the operating system closes the descriptor
		// as the process exits, just as it would after a worker crash.
		os.Exit(0)
	}

	root := t.TempDir()
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), Header{ID: "crash", SeedLength: 0}, nil); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	for _, pair := range []struct {
		typ  string
		data any
	}{
		{session.EventTurnStart, session.NewTurnStart()},
		{session.EventStepStart, session.NewStepStart(1)},
		{session.EventUserMessage, session.NewUserMessage("before crash")},
	} {
		if _, err := log.Append(pair.typ, pair.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append(context.Background(), "crash", log.Events()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLRecoveryAfterChildProcessExit$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SHUTU_JSONL_CRASH_CHILD=1",
		"SHUTU_JSONL_CRASH_PATH="+filepath.Join(root, "crash.jsonl"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash child: %v\n%s", err, output)
	}

	loaded, err := store.Load(context.Background(), "crash")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) != 5 || loaded.Revision != 5 || loaded.Events[3].Type != session.EventStepEnd || loaded.Events[4].Type != session.EventTurnEnd {
		t.Fatalf("post-crash recovery = revision %d events %+v", loaded.Revision, loaded.Events)
	}
}

func TestJSONLProcessLockIsHeldAcrossChildProcess(t *testing.T) {
	if os.Getenv("SHUTU_JSONL_LOCK_CHILD") == "1" {
		path := os.Getenv("SHUTU_JSONL_LOCK_PATH")
		ready := os.Getenv("SHUTU_JSONL_LOCK_READY")
		release := os.Getenv("SHUTU_JSONL_LOCK_RELEASE")
		lock, err := acquireProcessLock(path)
		if err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			_ = lock.Close()
			os.Exit(3)
		}
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = lock.Close()
		os.Exit(0)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJSONLProcessLockIsHeldAcrossChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SHUTU_JSONL_LOCK_CHILD=1",
		"SHUTU_JSONL_LOCK_PATH="+path,
		"SHUTU_JSONL_LOCK_READY="+ready,
		"SHUTU_JSONL_LOCK_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child did not acquire process lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second, err := acquireProcessLockContext(ctx, path)
	if second != nil {
		_ = second.Close()
		t.Fatal("parent acquired child-held process lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("child-held lock error = %v, want deadline exceeded", err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock child: %v", err)
	}
}

func TestJSONLMaintenanceBackupAndIntegrity(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONL(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Create(ctx, Header{ID: "maintenance", CWD: "/workspace"}, nil); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("durable")); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, "maintenance", log.Events()); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}

	backup := filepath.Join(t.TempDir(), "jsonl-backup")
	if err := store.Backup(ctx, backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backupStore, err := OpenJSONL(backup)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	loaded, err := backupStore.Load(ctx, "maintenance")
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	if loaded.Header.CWD != "/workspace" || len(loaded.Events) != 1 || string(loaded.Events[0].Data) == "" {
		t.Fatalf("backup contents = header=%+v events=%+v", loaded.Header, loaded.Events)
	}
	if err := store.Backup(ctx, backup); err == nil {
		t.Fatal("backup should refuse an existing destination")
	}

	// RepairSession is an explicit maintenance entry point, even though Load
	// also uses the recovery-aware path for normal reads.
	path := filepath.Join(root, "maintenance.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"event","seq":3`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RepairSession(ctx, "maintenance"); err != nil {
		t.Fatalf("RepairSession: %v", err)
	}
	if _, err := store.Inspect(ctx, "maintenance"); err != nil {
		t.Fatalf("inspect after repair: %v", err)
	}
}
