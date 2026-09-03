package team

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

func TestTeamEventsUseReferenceWireShapes(t *testing.T) {
	memberBytes, err := json.Marshal(MemberEvent{Version: 1, TeamID: "team-1", Member: MemberSnapshot{ID: "child", Name: "worker", Phase: MemberProvisioning}})
	if err != nil {
		t.Fatal(err)
	}
	var member map[string]any
	if err := json.Unmarshal(memberBytes, &member); err != nil {
		t.Fatal(err)
	}
	memberValue := member["member"].(map[string]any)
	for _, key := range []string{"description", "provider", "context", "phase"} {
		if _, ok := memberValue[key]; !ok {
			t.Fatalf("member event missing required %s: %s", key, memberBytes)
		}
	}

	taskBytes, err := json.Marshal(TaskEvent{Version: 1, TeamID: "team-1", Task: Task{ID: "task-1", Revision: 1, Subject: "inspect", Status: TaskPending}})
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]any
	if err := json.Unmarshal(taskBytes, &task); err != nil {
		t.Fatal(err)
	}
	taskValue := task["task"].(map[string]any)
	if _, ok := taskValue["deletedAt"]; ok {
		t.Fatalf("task event leaked board-only deletedAt: %s", taskBytes)
	}
	if _, ok := taskValue["blockedBy"].([]any); !ok {
		t.Fatalf("task blockedBy is not an array: %s", taskBytes)
	}
	if _, ok := taskValue["writeScopes"].([]any); !ok {
		t.Fatalf("task writeScopes is not an array: %s", taskBytes)
	}

	messageBytes, err := json.Marshal(MessageQueuedEvent{Version: 1, TeamID: "team-1", Message: Message{ID: "msg-1", SenderID: "lead", SenderName: "Lead", TargetID: "worker", Delivery: "quiet", Content: "hello", CreatedAt: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	var queued map[string]any
	if err := json.Unmarshal(messageBytes, &queued); err != nil {
		t.Fatal(err)
	}
	messageValue := queued["message"].(map[string]any)
	if _, ok := messageValue["createdAt"]; ok {
		t.Fatalf("queued event leaked board-only createdAt: %s", messageBytes)
	}
	if _, ok := messageValue["delivered"]; ok {
		t.Fatalf("queued event leaked board-only delivered: %s", messageBytes)
	}
	content, ok := messageValue["content"].([]any)
	if !ok || len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("queued content = %#v, want one text block", messageValue["content"])
	}
}

func TestBoardUsesDurableReservationForTaskAndMessageIDs(t *testing.T) {
	board, err := New("team-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(map[string]int)
	board.SetIDReservation(func(namespace, id string) (bool, error) {
		calls[namespace]++
		return calls[namespace] > 1, nil
	})
	task, err := board.CreateTask("inspect", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-2" {
		t.Fatalf("task id = %q, want task-2 after a rejected reservation", task.ID)
	}
	message, err := board.SendMessage("lead", "Lead", "worker", "hello", "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "msg-2" {
		t.Fatalf("message id = %q, want msg-2 after a rejected reservation", message.ID)
	}
}

// TestBoardRecoversAbandonedTaskAndMessageReservations models a crash after
// the durable reservation but before the domain receipt. A fresh process must
// never reuse either token, while the counters must recover by advancing to
// the next candidate rather than stranding Team task/mailbox creation forever.
func TestBoardRecoversAbandonedTaskAndMessageReservations(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-orphan-reservations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	teamID := "team-orphan"
	for _, item := range []struct{ namespace, id string }{
		{"team-task:" + teamID, "task-1"},
		{"team-message:" + teamID, "msg-1"},
	} {
		claimed, err := st.ReserveID(ctx, item.namespace, item.id)
		if err != nil || !claimed {
			t.Fatalf("orphan reservation %s/%s = %v, %v", item.namespace, item.id, claimed, err)
		}
	}

	board, err := New(teamID, nil)
	if err != nil {
		t.Fatal(err)
	}
	board.SetIDReservation(func(namespace, id string) (bool, error) {
		return st.ReserveID(context.Background(), namespace, id)
	})
	task, err := board.CreateTask("inspect", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-2" {
		t.Fatalf("task after abandoned reservation = %q, want task-2", task.ID)
	}
	message, err := board.SendMessage("lead", "Lead", "worker", "hello", "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "msg-2" {
		t.Fatalf("message after abandoned reservation = %q, want msg-2", message.ID)
	}
}

func TestBoardReservesMemberBeforeRosterSpawn(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-board-members", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	board, err := New("team-board-members", nil)
	if err != nil {
		t.Fatal(err)
	}
	board.AttachRoster(roster)
	var calls int
	board.SetIDReservation(func(namespace, id string) (bool, error) {
		calls++
		if namespace != "team-member:team-board-members" || id != "team-board-members:worker" {
			t.Fatalf("reservation = namespace %q id %q", namespace, id)
		}
		return true, nil
	})
	childID, err := board.ReserveMemberID("worker")
	if err != nil {
		t.Fatal(err)
	}
	if childID != "team-board-members:worker" || calls != 1 {
		t.Fatalf("childID=%q calls=%d", childID, calls)
	}
	member, err := roster.Spawn(context.Background(), "lead", "worker", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if member.ID != childID {
		t.Fatalf("member id = %q, want %q", member.ID, childID)
	}
}

func TestTeamRichMessageContentRoundTripsInReferenceWire(t *testing.T) {
	original := Message{
		ID: "msg-7", SenderID: "lead", SenderName: "Lead", TargetID: "worker", Delivery: "wakeup",
		Content: "visible reasoning result",
		ContentBlocks: []llm.ContentBlock{
			llm.Text("visible "),
			{Kind: llm.BlockReasoning, Text: "reasoning ", CallID: "ignored"},
			{Kind: llm.BlockImage, Image: llm.ImageRef{ID: "att-1", MediaType: "image/png", Bytes: 42, Width: 4, Height: 5}},
			{Kind: llm.BlockToolCall, CallID: "call-1", Name: "read", Arguments: `{"path":"x"}`},
			{Kind: llm.BlockToolResult, CallID: "call-1", IsError: true, Blocks: []llm.ContentBlock{llm.Text("failed")}},
		},
	}
	wantWire, err := json.Marshal(MessageQueuedEvent{Version: 1, TeamID: "team-rich", Message: original})
	if err != nil {
		t.Fatal(err)
	}
	var decoded MessageQueuedEvent
	if err := json.Unmarshal(wantWire, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := len(decoded.Message.ContentBlocks); got != len(original.ContentBlocks) {
		t.Fatalf("decoded block count = %d, want %d", got, len(original.ContentBlocks))
	}
	for i, want := range original.ContentBlocks {
		got := decoded.Message.ContentBlocks[i]
		if got.Kind != want.Kind || got.Text != want.Text || got.CallID != want.CallID || got.Name != want.Name || got.Arguments != want.Arguments || got.IsError != want.IsError || got.Image != want.Image {
			t.Fatalf("decoded block %d = %+v, want %+v", i, got, want)
		}
	}
	if decoded.Message.ContentBlocks[4].Blocks[0].Text != "failed" {
		t.Fatalf("nested tool result was not preserved: %+v", decoded.Message.ContentBlocks[4])
	}

	text, blocks, err := DecodeMessageContent(json.RawMessage(`[{"type":"text","text":"hi"},{"type":"image","attachment":{"attachmentId":"a","mediaType":"image/jpeg","bytes":3}}]`))
	if err != nil || text != "hi" || len(blocks) != 2 || blocks[1].Image.ID != "a" {
		t.Fatalf("DecodeMessageContent = text=%q blocks=%+v err=%v", text, blocks, err)
	}
}

func TestBoardAppendOnlyEventsFoldTaskAndMailbox(t *testing.T) {
	board, err := New("team-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := board.CreateTask("evented", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := board.UpdateTask(Update{ID: created.ID, Revision: created.Revision, Action: "claim", OwnerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := board.SendMessage("lead", "Lead", "worker", "hello", "quiet")
	if err != nil {
		t.Fatal(err)
	}

	replay, err := New("team-events", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		typ  string
		data any
	}{
		{"team/task", TaskEvent{Version: 1, TeamID: board.TeamID(), Task: created.Task}},
		{"team/task", TaskEvent{Version: 1, TeamID: board.TeamID(), Task: claimed.Task}},
		{"team/message/queued", MessageQueuedEvent{Version: 1, TeamID: board.TeamID(), Message: message}},
		{"team/message/delivered", MessageDeliveredEvent{Version: 1, TeamID: board.TeamID(), MessageID: message.ID, TargetID: message.TargetID}},
	} {
		data, err := json.Marshal(event.data)
		if err != nil {
			t.Fatal(err)
		}
		if event.typ == "team/message/delivered" {
			if err := replay.ApplyEvent(event.typ, data); err != nil {
				// Delivery is tested below after queueing; this branch is kept
				// unreachable only to make the sequence explicit in the table.
				t.Fatal(err)
			}
		} else if err := replay.ApplyEvent(event.typ, data); err != nil {
			t.Fatal(err)
		}
	}
	got, err := replay.GetTask(created.ID)
	if err != nil || got.Revision != claimed.Revision || got.OwnerID != "worker" {
		t.Fatalf("folded task = %+v err=%v", got, err)
	}
	if pending := replay.PendingMessages("worker"); len(pending) != 0 {
		t.Fatalf("folded delivered mailbox = %+v", pending)
	}
}

func TestBoardTaskCASBlockersAndTombstone(t *testing.T) {
	board, err := New("team-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := board.CreateTask("first", "", "", nil, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := board.CreateTask("second", "", "", []string{first.ID}, []string{"src/pkg"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Ready {
		t.Fatal("blocked task reported ready")
	}
	if _, err := board.UpdateTask(Update{ID: second.ID, Revision: second.Revision, Action: "claim", OwnerID: "agent-2"}); !errors.Is(err, ErrTaskBlocked) {
		t.Fatalf("claim blocked error = %v", err)
	}
	first, err = board.UpdateTask(Update{ID: first.ID, Revision: first.Revision, Action: "claim", OwnerID: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err = board.UpdateTask(Update{ID: first.ID, Revision: first.Revision, Action: "complete"})
	if err != nil || first.Revision != 3 {
		t.Fatalf("complete first = %+v, err=%v", first, err)
	}
	if _, err := board.UpdateTask(Update{ID: second.ID, Revision: 1, Action: "claim", OwnerID: "agent-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := board.UpdateTask(Update{ID: second.ID, Revision: 1, Action: "complete"}); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale CAS error = %v", err)
	}
	if _, err := board.UpdateTask(Update{ID: second.ID, Revision: 2, Action: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := board.CreateTask("cycle", "", "", []string{second.ID}, nil); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("deleted blocker error = %v", err)
	}
	if tasks := board.ListTasks(); len(tasks) != 1 || tasks[0].ID != first.ID || tasks[0].WriteScopeWarning {
		t.Fatalf("task views = %+v", tasks)
	}
}

func TestBoardCycleAndMailboxRecovery(t *testing.T) {
	board, err := New("team-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := board.CreateTask("a", "", "", nil, nil)
	b, _ := board.CreateTask("b", "", "", []string{a.ID}, nil)
	if _, err := board.UpdateTask(Update{ID: a.ID, Revision: a.Revision, Action: "claim", OwnerID: "x", BlockedBy: []string{b.ID}}); !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle error = %v", err)
	}
	m1, err := board.SendMessage("lead", "Lead", "child", "quiet", "quiet")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := board.SendMessage("lead", "Lead", "child", "wake", "wakeup")
	if err != nil {
		t.Fatal(err)
	}
	pending := board.PendingMessages("child")
	if len(pending) != 2 || !reflect.DeepEqual([]string{pending[0].ID, pending[1].ID}, []string{m1.ID, m2.ID}) {
		t.Fatalf("pending mailbox = %+v", pending)
	}
	if err := board.AckMessage(m1.ID); err != nil {
		t.Fatal(err)
	}
	if len(board.PendingMessages("child")) != 1 {
		t.Fatal("ack did not remove exactly one message")
	}
}

func TestBoardSnapshotRestorePreservesCASAndMailbox(t *testing.T) {
	board, err := New("team-snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := board.CreateTask("first", "one", "", nil, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.UpdateTask(Update{ID: first.ID, Revision: first.Revision, Action: "claim", OwnerID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := board.SendMessage("a", "A", "b", "hello", "wakeup"); err != nil {
		t.Fatal(err)
	}
	snapshot := board.Snapshot()
	restored, err := New("team-snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	task, err := restored.GetTask(first.ID)
	if err != nil || task.Revision != 2 || task.OwnerID != "a" {
		t.Fatalf("restored task = %+v, err=%v", task, err)
	}
	messages := restored.PendingMessages("b")
	if len(messages) != 1 || messages[0].Content != "hello" {
		t.Fatalf("restored mailbox = %+v", messages)
	}
	if _, err := restored.UpdateTask(Update{ID: first.ID, Revision: 1, Action: "complete"}); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale revision error = %v, want ErrRevision", err)
	}
}

func TestBoardColdInspectionReplaysMembersWithoutLiveRoster(t *testing.T) {
	board, err := New("team-cold-members", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []MemberSnapshot{
		{ID: "team-cold-members:worker", Name: "worker", Description: "inspectable", Provider: "spawn", Context: "fresh", Phase: MemberProvisioning},
		{ID: "team-cold-members:worker", Name: "worker", Description: "inspectable", Provider: "spawn", Context: "fresh", Phase: MemberFailed, Error: "child session missing"},
	} {
		data, marshalErr := json.Marshal(MemberEvent{Version: 1, TeamID: board.TeamID(), Member: member})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if applyErr := board.ApplyEvent("team/member", data); applyErr != nil {
			t.Fatalf("apply member %s: %v", member.Phase, applyErr)
		}
	}
	snapshot := board.Snapshot()
	if len(snapshot.Members) != 1 || snapshot.Members[0].Phase != MemberFailed {
		t.Fatalf("cold member snapshot = %+v", snapshot.Members)
	}
	restored, err := New("team-cold-members", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	if got := restored.Snapshot().Members; len(got) != 1 || got[0].Error != "child session missing" {
		t.Fatalf("restored cold members = %+v", got)
	}
}

func TestBoardMemberReplayDoesNotPartiallyMutateBoardOrRoster(t *testing.T) {
	board, err := New("team-atomic-members", nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	roster, err := NewRoster("team-atomic-members", agent.ID("lead"), registry)
	if err != nil {
		t.Fatal(err)
	}
	board.AttachRoster(roster)

	provisioning := MemberSnapshot{ID: "team-atomic-members:worker", Name: "worker", Context: "fresh", Phase: MemberProvisioning}
	data, err := json.Marshal(MemberEvent{Version: 1, TeamID: board.TeamID(), Member: provisioning})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.ApplyEvent("team/member", data); err != nil {
		t.Fatal(err)
	}

	// This event changes an immutable identity field. The replay must reject it
	// before either projection advances.
	invalid := provisioning
	invalid.Name = "renamed"
	invalid.Phase = MemberActive
	data, err = json.Marshal(MemberEvent{Version: 1, TeamID: board.TeamID(), Member: invalid})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.ApplyEvent("team/member", data); err == nil {
		t.Fatal("immutable member mutation must be rejected")
	}
	member, err := roster.Member(agent.ID(provisioning.ID))
	if err != nil || member.Name != "worker" || member.Phase != MemberProvisioning {
		t.Fatalf("roster after rejected event = %+v, err=%v", member, err)
	}
	got := board.Snapshot().Members
	if len(got) != 2 || got[1].Name != "worker" || got[1].Phase != MemberProvisioning {
		t.Fatalf("board after rejected event = %+v", got)
	}
}
