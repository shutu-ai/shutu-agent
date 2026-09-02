package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/store"
)

func TestWebQueueOwnerAndShutdownAdmissionMatrix(t *testing.T) {
	st, err := store.OpenSQLite(strings.Join([]string{t.TempDir(), "web-owner.db"}, string([]rune{0x2f})))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"owner-a", "owner-b"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	a := &app{store: st, webQueue: map[string][]webQueueMessage{}}
	// Keep the queue idle so this test observes ownership state without
	// starting model turns.
	a.webQueueRunning = map[string]bool{"owner-a": true, "owner-b": true}

	if _, err := a.webQueueEnqueue(ctx, "missing", "must not queue"); err == nil {
		t.Fatal("enqueue for a nonexistent addressed session was admitted")
	}
	first, err := a.webQueueEnqueue(ctx, "owner-a", "owned message")
	if err != nil || first.ID == "" {
		t.Fatalf("owned enqueue = %+v, err=%v", first, err)
	}
	if err := a.webQueueUpdate(ctx, "owner-b", first.ID, "delete"); err == nil {
		t.Fatal("cross-session queue update was admitted")
	}
	if err := a.webQueueEdit(ctx, "owner-b", first.ID, "must not edit"); err == nil {
		t.Fatal("cross-session queue edit was admitted")
	}
	if err := a.webQueueUpdate(ctx, "owner-a", "not-a-real-item", "delete"); err == nil {
		t.Fatal("unknown queue item update was admitted")
	}
	items, err := a.webQueueList(ctx, "owner-a")
	if err != nil || len(items) != 1 || items[0].ID != first.ID || items[0].Text != "owned message" {
		t.Fatalf("owner-a queue after foreign mutations = %+v, err=%v", items, err)
	}
	if items, err := a.webQueueList(ctx, "owner-b"); err != nil || len(items) != 0 {
		t.Fatalf("owner-b queue after foreign mutations = %+v, err=%v", items, err)
	}

	a.beginShutdown()
	if err := a.requireRunning(); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("shutdown admission = %v, want errAppShuttingDown", err)
	}
	if _, err := a.webQueueEnqueue(ctx, "owner-a", "late work"); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("post-shutdown enqueue = %v, want shutdown rejection", err)
	}
	if err := a.webQueueUpdate(ctx, "owner-a", first.ID, "delete"); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("post-shutdown queue update = %v, want shutdown rejection", err)
	}
	if err := a.webQueueEdit(ctx, "owner-a", first.ID, "late edit"); !errors.Is(err, errAppShuttingDown) {
		t.Fatalf("post-shutdown queue edit = %v, want shutdown rejection", err)
	}
	items, err = a.webQueueList(ctx, "owner-a")
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("post-shutdown queue changed state: items=%+v err=%v", items, err)
	}
}
