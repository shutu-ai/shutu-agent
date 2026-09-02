package contractfixture_test

// A9.3 needs cross-process evidence, not only package-local fault callbacks.
// These cases model an owned worker dying while its storage handle is open and
// an attacker-controlled workspace link attempting an external write. They are
// intentionally a bounded matrix: the register still tracks the remaining
// disk, pipe, kill-at-every-write, provider-wipe, and platform-CI cases.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/fs"
	"github.com/jabing/shutu-agent/internal/persistence"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/web"
)

func TestFaultSecurityRestartMatrix(t *testing.T) {
	t.Run("SQLite process death preserves committed prefix and repairs only on load", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "fault.db")

		if err := createSQLiteCrashSession(t, root, dbPath); err != nil {
			t.Fatalf("seed crash database: %v", err)
		}
		ready := filepath.Join(root, "sqlite-worker.ready")
		cmd := faultHelperCommand(t, "sqlite-open-tail", root, dbPath, ready)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start SQLite crash worker: %v", err)
		}
		if err := waitForFaultHelperFile(ready); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("SQLite crash worker exited cleanly; expected modeled process death")
		}

		// The first post-crash handle is the recovery authority. Inspect would
		// still see the physical open tail; Load is the production restart path.
		firstStore, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatalf("open first post-crash SQLite handle: %v", err)
		}
		defer firstStore.Close()
		first := persistence.SQLiteAdapter{Store: firstStore}
		loaded, err := first.Load(context.Background(), "sqlite-crash")
		if err != nil {
			t.Fatalf("recover SQLite session: %v", err)
		}
		assertRecoveredTurn(t, loaded.Events, 5)

		// A second independent process/handle must observe the same durable
		// result, including the recovery rows, rather than replaying another
		// repair from process-local state.
		secondStore, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatalf("open second post-crash SQLite handle: %v", err)
		}
		defer secondStore.Close()
		second := persistence.SQLiteAdapter{Store: secondStore}
		reopened, err := second.Load(context.Background(), "sqlite-crash")
		if err != nil {
			t.Fatalf("load after recovery: %v", err)
		}
		assertRecoveredTurn(t, reopened.Events, 5)
		if reopened.Revision != 5 {
			t.Fatalf("post-crash revision = %d, want 5", reopened.Revision)
		}
	})

	t.Run("workspace symlink/reparse-point escape produces no outside side effect", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		outsideSentinel := filepath.Join(outside, "must-not-change")
		if err := os.WriteFile(outsideSentinel, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "escape")
		linkKind := "symlink"
		if err := os.Symlink(outside, link); err != nil {
			if runtime.GOOS == "windows" {
				// Managed hosts may deny symbolic-link creation. A junction is
				// still a Windows directory reparse point and exercises the
				// same path-resolver traversal boundary. The Linux leg proves
				// the literal symbolic-link case.
				command := fmt.Sprintf(
					"New-Item -ItemType Junction -Path %s -Target %s | Out-Null",
					strconv.Quote(link), strconv.Quote(outside),
				)
				if out, linkErr := exec.Command("powershell", "-NoProfile", "-Command", command).CombinedOutput(); linkErr != nil {
					t.Skipf("workspace symlink and junction unavailable: %v: %s", err, strings.TrimSpace(string(out)))
				}
				linkKind = "junction"
			} else {
				t.Fatal(err)
			}
		}

		fsys := fs.NewLocalFS(root)
		ctx := context.Background()
		writeErr := fsys.Write(ctx, filepath.Join("escape", "created.txt"), "escape")
		if writeErr == nil || (linkKind == "symlink" && !errors.Is(writeErr, fs.ErrPathOutsideRoot)) {
			t.Fatalf("%s write error = %v, want fail-closed traversal rejection", linkKind, writeErr)
		}
		_, readErr := fsys.Read(ctx, filepath.Join("escape", "must-not-change"), 0)
		if readErr == nil || (linkKind == "symlink" && !errors.Is(readErr, fs.ErrPathOutsideRoot)) {
			t.Fatalf("%s read error = %v, want fail-closed traversal rejection", linkKind, readErr)
		}
		got, err := os.ReadFile(outsideSentinel)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte("original")) {
			t.Fatalf("outside sentinel changed to %q; symlink boundary was not fail closed", got)
		}
		if _, err := os.Stat(filepath.Join(outside, "created.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("outside created.txt stat = %v, want not exist", err)
		}
	})

	t.Run("network cross-origin boundary preserves durable prefix", func(t *testing.T) {
		root := t.TempDir()
		jsonl, err := persistence.OpenJSONL(root)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := jsonl.Create(ctx, persistence.Header{ID: "network-boundary", CWD: root}, nil); err != nil {
			t.Fatal(err)
		}
		seed := []session.Event{
			{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1})},
			{Seq: 2, Type: session.EventUserMessage, At: time.UnixMilli(2).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"text": "before network boundary"})},
			{Seq: 3, Type: session.EventTurnEnd, At: time.UnixMilli(3).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})},
		}
		if err := jsonl.Append(ctx, "network-boundary", seed); err != nil {
			t.Fatal(err)
		}

		targetHit := filepath.Join(root, "cross-origin-target.hit")
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = os.WriteFile(targetHit, []byte("contacted"), 0o600)
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
		}))
		defer source.Close()

		status := filepath.Join(root, "network.status")
		cmd := faultHelperCommand(t, "network-fetch", root, "", status)
		cmd.Env = append(cmd.Env,
			"SHUTU_FAULT_URL="+source.URL,
			"SHUTU_FAULT_TARGET_HIT="+targetHit,
		)
		if err := cmd.Run(); err != nil {
			t.Fatalf("network helper: %v", err)
		}
		encoded, err := os.ReadFile(status)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != "redirect-blocked" {
			t.Fatalf("network helper status = %q, want redirect-blocked", encoded)
		}
		if _, err := os.Stat(targetHit); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cross-origin target stat = %v, want no external request", err)
		}

		// A second independent storage handle proves the network fault did not
		// leave a partial durable record or invent a tool result after failure.
		reopened, err := persistence.OpenJSONL(root)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := reopened.Load(ctx, "network-boundary")
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Events) != len(seed) {
			t.Fatalf("post-network events = %d, want committed prefix %d", len(loaded.Events), len(seed))
		}
		for i := range seed {
			if loaded.Events[i].Type != seed[i].Type || string(loaded.Events[i].Data) != string(seed[i].Data) {
				t.Fatalf("post-network event %d changed: %s/%s", loaded.Events[i].Seq, loaded.Events[i].Type, loaded.Events[i].Data)
			}
		}
		if !strings.Contains(loaded.RevisionToken, "jsonl:") {
			t.Fatalf("revision token = %q, want an independent JSONL boundary", loaded.RevisionToken)
		}
	})
}

func createSQLiteCrashSession(t *testing.T, root, dbPath string) error {
	t.Helper()
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	p := persistence.SQLiteAdapter{Store: st}
	ctx := context.Background()
	header := persistence.Header{ID: "sqlite-crash", CWD: root}
	return p.Create(ctx, header, nil)
}

func faultHelperCommand(t *testing.T, mode, root, dbPath, ready string) *exec.Cmd {
	t.Helper()
	testName := "^TestFaultSecurityHelper$"
	if mode == "network-fetch" {
		testName = "^TestNetworkFaultHelper$"
	}
	cmd := exec.Command(os.Args[0], "-test.run="+testName)
	cmd.Env = append(os.Environ(),
		"SHUTU_FAULT_HELPER="+mode,
		"SHUTU_FAULT_ROOT="+root,
		"SHUTU_FAULT_DB="+dbPath,
		"SHUTU_FAULT_READY="+ready,
		"SHUTU_FAULT_STATUS="+ready,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}

func waitForFaultHelperFile(path string) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("fault helper did not publish readiness before deadline")
}

func assertRecoveredTurn(t *testing.T, events []session.Event, wantCount int) {
	t.Helper()
	if len(events) != wantCount {
		t.Fatalf("recovered event count = %d, want %d: %#v", len(events), wantCount, events)
	}
	if events[0].Type != session.EventTurnStart || events[1].Type != session.EventStepStart ||
		events[2].Type != session.EventUserMessage || events[3].Type != session.EventStepEnd ||
		events[4].Type != session.EventTurnEnd {
		t.Fatalf("recovered event types = %s/%s/%s/%s/%s, want turn lifecycle",
			events[0].Type, events[1].Type, events[2].Type, events[3].Type, events[4].Type)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("recovered lifecycle invalid: %v", err)
	}
}

// TestFaultSecurityHelper is a compiled child worker used by
// TestFaultSecurityRestartMatrix. It writes through the production SQLite
// adapter, publishes readiness, then exits without cleanup to model process
// death before an open turn is settled.
func TestFaultSecurityHelper(t *testing.T) {
	mode := os.Getenv("SHUTU_FAULT_HELPER")
	if mode == "" {
		t.Skip("fault security helper")
	}
	switch mode {
	case "sqlite-open-tail":
	default:
		t.Fatalf("unknown fault helper mode %q", mode)
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_FAULT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	p := persistence.SQLiteAdapter{Store: st}
	events := []session.Event{
		{Seq: 1, Type: session.EventTurnStart, At: time.UnixMilli(1).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1})},
		{Seq: 2, Type: session.EventStepStart, At: time.UnixMilli(2).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"turn": 1, "step": 1})},
		{Seq: 3, Type: session.EventUserMessage, At: time.UnixMilli(3).UTC(), Version: session.EventVersion, Data: mustJSON(map[string]any{"text": "before crash"})},
	}
	if err := p.Append(context.Background(), "sqlite-crash", events); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SHUTU_FAULT_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately skip st.Close and process exit handlers: the oracle must be
	// valid after an abrupt process boundary, not after a graceful shutdown.
	os.Exit(23)
}

func TestNetworkFaultHelper(t *testing.T) {
	mode := os.Getenv("SHUTU_FAULT_HELPER")
	if mode == "" {
		t.Skip("fault security helper")
	}
	if mode != "network-fetch" {
		t.Skip("not network helper")
	}
	provider := web.NewHttpFetchProvider(web.FetchLimits{TimeoutMs: 2000})
	_, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: os.Getenv("SHUTU_FAULT_URL")})
	status := "unexpected-success"
	switch {
	case errors.Is(err, web.ErrRedirectBlocked):
		status = "redirect-blocked"
	case errors.Is(err, web.ErrProvider):
		status = "provider-error"
	case err == nil:
	default:
		status = "provider-error"
	}
	if writeErr := os.WriteFile(os.Getenv("SHUTU_FAULT_STATUS"), []byte(status), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}
