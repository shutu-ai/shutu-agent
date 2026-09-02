package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jabing/shutu-agent/internal/tools"
)

func TestToolsAreSessionScopedAndUseCAS(t *testing.T) {
	boards := map[string]*Board{}
	session, member := "s1", "agent-1"
	adapter := NewTools(func(id string) (*Board, error) {
		if boards[id] == nil {
			var err error
			boards[id], err = New("team-"+id, nil)
			if err != nil {
				return nil, err
			}
		}
		return boards[id], nil
	}, func() (string, string) { return session, member })
	reg := tools.New()
	for _, item := range adapter.Tools() {
		if err := reg.Register(item.(tools.Tool)); err != nil {
			t.Fatal(err)
		}
	}
	reg.SetPolicy(tools.Policy{Enabled: []string{ToolTaskCreate, ToolTaskUpdate, ToolTaskList, ToolTaskGet, ToolMessage}})
	created, err := reg.Execute(context.Background(), ToolTaskCreate, json.RawMessage(`{"subject":"compile","write_scopes":["internal/team"]}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	value, ok := created.Value.(map[string]any)
	if !ok || value["ownerId"] != nil {
		t.Fatalf("created result = %+v value=%#v", created, created.Value)
	}

	claimed, err := reg.Execute(context.Background(), ToolTaskUpdate, json.RawMessage(`{"id":"task-1","revision":1,"action":"claim"}`))
	if err != nil || claimed.IsError {
		t.Fatalf("claiming own task = %+v err=%v", claimed, err)
	}
	member = "agent-2"
	denied, err := reg.Execute(context.Background(), ToolTaskUpdate, json.RawMessage(`{"id":"task-1","revision":2,"action":"complete"}`))
	if err != nil || !denied.IsError || !strings.Contains(denied.Output, ErrUnauthorized.Error()) {
		t.Fatalf("claiming another member's task = %+v err=%v, want ErrUnauthorized", denied, err)
	}
	member = "agent-1"
	completed, err := reg.Execute(context.Background(), ToolTaskUpdate, json.RawMessage(`{"id":"task-1","revision":2,"action":"complete"}`))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.IsError {
		t.Fatalf("complete result = %+v", completed)
	}
	stale, err := reg.Execute(context.Background(), ToolTaskUpdate, json.RawMessage(`{"id":"task-1","revision":1,"action":"delete"}`))
	if err != nil || !stale.IsError || !strings.Contains(stale.Output, ErrRevision.Error()) {
		t.Fatalf("stale revision result = %+v err=%v, want ErrRevision", stale, err)
	}

	session = "s2"
	missing, err := reg.Execute(context.Background(), ToolTaskGet, json.RawMessage(`{"id":"task-1"}`))
	if err != nil || !missing.IsError || !strings.Contains(missing.Output, ErrTaskNotFound.Error()) {
		t.Fatalf("cross-session lookup = %+v err=%v, want ErrTaskNotFound", missing, err)
	}
}

func TestToolsRollbackMutationWhenSnapshotPersistenceFails(t *testing.T) {
	board, err := New("team-s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "s1", "agent-1"
	})
	adapter.SetSnapshotSink(func(context.Context, string, Snapshot) error {
		return errors.New("disk full")
	})
	created, err := adapter.TaskCreate().ExecuteResult(context.Background(), map[string]any{"subject": "must rollback"})
	if err == nil {
		t.Fatalf("create with failed snapshot = %+v err=%v, want failure", created, err)
	}
	if tasks := board.ListTasks(); len(tasks) != 0 {
		t.Fatalf("board after failed create = %#v, want empty", tasks)
	}

	// Seed a task outside the adapter, then prove update and message mutations
	// use the same rollback boundary.
	seed, err := board.CreateTask("seed", "", "agent-1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := adapter.TaskUpdate().ExecuteResult(context.Background(), map[string]any{
		"id": seed.ID, "revision": seed.Revision, "action": "complete",
	})
	if err == nil {
		t.Fatalf("update with failed snapshot = %+v err=%v, want failure", updated, err)
	}
	current, err := board.GetTask(seed.ID)
	if err != nil || current.Status != TaskPending || current.Revision != seed.Revision {
		t.Fatalf("task after failed update = %+v err=%v, want original pending revision", current, err)
	}
	message, err := adapter.Message().ExecuteResult(context.Background(), map[string]any{
		"target": "agent-2", "content": "must rollback",
	})
	if err == nil {
		t.Fatalf("message with failed snapshot = %+v err=%v, want failure", message, err)
	}
	if messages := board.PendingMessages("agent-2"); len(messages) != 0 {
		t.Fatalf("messages after failed send = %#v, want empty", messages)
	}
}

func TestToolsSerializeMutationAndDurableRollback(t *testing.T) {
	board, err := New("team-serial", nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "serial", "agent-1"
	})
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var sinkMu sync.Mutex
	sinkCalls := 0
	adapter.SetEventSink(func(context.Context, string, string, any) error {
		sinkMu.Lock()
		sinkCalls++
		call := sinkCalls
		sinkMu.Unlock()
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
			return errors.New("event append failed")
		}
		return nil
	})
	var wg sync.WaitGroup
	var firstErr, secondErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, firstErr = adapter.TaskCreate().ExecuteResult(context.Background(), map[string]any{"subject": "first"})
	}()
	<-firstEntered
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, secondErr = adapter.TaskCreate().ExecuteResult(context.Background(), map[string]any{"subject": "second"})
	}()
	close(releaseFirst)
	wg.Wait()
	if firstErr == nil || secondErr != nil {
		t.Fatalf("firstErr=%v secondErr=%v, want only first mutation to fail", firstErr, secondErr)
	}
	tasks := board.ListTasks()
	if len(tasks) != 1 || tasks[0].Subject != "second" {
		t.Fatalf("board after serialized rollback = %#v, want only second task", tasks)
	}
}

func TestMessageToolDispatchesAndAcknowledgesLiveDelivery(t *testing.T) {
	board, err := New("team-delivery", nil)
	if err != nil {
		t.Fatal(err)
	}
	var delivered Message
	board.SetMessageDispatcher(func(_ context.Context, message Message) (bool, error) {
		delivered = message
		return true, nil
	})
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "team-delivery", "lead"
	})
	result, err := adapter.Message().ExecuteResult(context.Background(), map[string]any{
		"target": "child", "content": "wake", "delivery": "wakeup",
	})
	if err != nil || result.IsError || delivered.ID == "" {
		t.Fatalf("message result=%+v err=%v delivered=%+v", result, err, delivered)
	}
	if pending := board.PendingMessages("child"); len(pending) != 0 {
		t.Fatalf("delivered message remained pending: %+v", pending)
	}
}

func TestMessageToolKeepsQueuedStateWhenDeliveryCommitFails(t *testing.T) {
	board, err := New("team-delivery-commit", nil)
	if err != nil {
		t.Fatal(err)
	}
	board.SetMessageDispatcher(func(_ context.Context, _ Message) (bool, error) {
		return true, nil
	})
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "team-delivery-commit", "lead"
	})
	var persisted []string
	adapter.SetEventSink(func(_ context.Context, _, typ string, _ any) error {
		persisted = append(persisted, typ)
		if typ == "team/message/delivered" {
			return errors.New("delivery journal unavailable")
		}
		return nil
	})
	if _, err := adapter.Message().ExecuteResult(context.Background(), map[string]any{
		"target": "child", "content": "retry me", "delivery": "quiet",
	}); err == nil {
		t.Fatal("delivery commit unexpectedly succeeded")
	}
	if len(persisted) != 2 || persisted[0] != "team/message/queued" || persisted[1] != "team/message/delivered" {
		t.Fatalf("persisted events = %#v, want queued then failed delivered attempt", persisted)
	}
	if pending := board.PendingMessages("child"); len(pending) != 1 {
		t.Fatalf("pending messages = %#v, want queued message retained", pending)
	}
}

func TestMessageToolRejectsSelfMessage(t *testing.T) {
	board, err := New("team-self-message", nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "team-self-message", "lead"
	})
	if _, err := adapter.Message().ExecuteResult(context.Background(), map[string]any{
		"target": "lead", "content": "loopback",
	}); !errors.Is(err, ErrSelfMessage) {
		t.Fatalf("self message error = %v, want %v", err, ErrSelfMessage)
	}
}

func TestMessageToolAcceptsRichContentBlocks(t *testing.T) {
	board, err := New("team-rich-tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewTools(func(string) (*Board, error) { return board, nil }, func() (string, string) {
		return "team-rich-tool", "lead"
	})
	if _, err := adapter.Message().ExecuteResult(context.Background(), map[string]any{
		"target": "child",
		"content": []any{
			map[string]any{"type": "text", "text": "inspect"},
			map[string]any{"type": "tool-call", "id": "call-1", "name": "read", "arguments": `{"path":"x"}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	pending := board.PendingMessages("child")
	if len(pending) != 1 || len(pending[0].ContentBlocks) != 2 || pending[0].ContentBlocks[1].CallID != "call-1" {
		t.Fatalf("queued rich message = %+v", pending)
	}
}
