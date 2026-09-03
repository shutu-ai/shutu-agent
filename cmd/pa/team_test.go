package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/team"
)

func TestTeamChildSessionSharesDurableRootBoard(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	a := &app{store: st}
	if err := a.createTeammateSession(ctx, "lead", "team:lead:worker", "fresh"); err != nil {
		t.Fatalf("create teammate session: %v", err)
	}
	if got := a.teamRootSessionID("team:lead:worker"); got != "lead" {
		t.Fatalf("team root = %q, want lead", got)
	}
	root, err := a.teamBoard("lead")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.teamBoard("team:lead:worker")
	if err != nil {
		t.Fatal(err)
	}
	if root != child {
		t.Fatal("lead and teammate resolved different Team boards")
	}
	if _, err := root.CreateTask("shared", "visible to the teammate", "lead", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(child.ListTasks()); got != 1 {
		t.Fatalf("child board task count = %d, want 1", got)
	}
}

func TestTeamBoardFailsClosedOnCorruptLatestSnapshot(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "lead", []session.Event{{
		Seq: 1, Type: session.EventTeamSnapshot, At: time.Unix(1, 0).UTC(), Version: session.EventVersion,
		Data: json.RawMessage(`{"teamId":`),
	}}); err != nil {
		t.Fatal(err)
	}

	_, err = (&app{store: st}).teamBoard("lead")
	if err == nil || !strings.Contains(err.Error(), "decode durable board snapshot") {
		t.Fatalf("corrupt Team snapshot error = %v, want fail-closed decode error", err)
	}
}

func TestTeamBoardReplaysAppendOnlyTaskEventsAfterSnapshot(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(team.TaskEvent{Version: 1, TeamID: "team:lead", Task: team.Task{ID: "task-1", Revision: 1, Subject: "replayed", Status: team.TaskPending}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "lead", []session.Event{{
		Seq: 1, Type: session.EventTeamTask, At: time.Unix(1, 0).UTC(), Version: session.EventVersion, Data: data,
	}}); err != nil {
		t.Fatal(err)
	}
	board, err := (&app{store: st}).teamBoard("lead")
	if err != nil {
		t.Fatal(err)
	}
	if tasks := board.ListTasks(); len(tasks) != 1 || tasks[0].ID != "task-1" {
		t.Fatalf("replayed Team tasks = %+v", tasks)
	}
}

func TestDurableTeamRootIDsFindsColdBoardAfterRestart(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-cold-directory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	board, err := team.New("team:lead", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(board.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents(ctx, "lead", []session.Event{{
		Seq: 1, Type: session.EventTeamSnapshot, At: time.Now().UTC(), Version: session.EventVersion,
		Data: snapshot,
	}}); err != nil {
		t.Fatal(err)
	}
	a := &app{store: st, teamBoards: map[string]*team.Board{}}
	ids := a.durableTeamRootIDs(ctx)
	if len(ids) != 1 || ids[0] != "lead" {
		t.Fatalf("cold Team roots = %v, want [lead]", ids)
	}
}

func TestTeamMessageReceiptIsDurableAndIdempotent(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "team:lead:worker", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	a := &app{store: st}
	message := team.Message{ID: "msg-1", SenderID: "lead", SenderName: "lead", TargetID: "team:lead:worker", Content: "hello", Delivery: "quiet"}
	if err := a.recordTeamMessage(ctx, "lead", message, "Team message msg-1 from lead:\nhello"); err != nil {
		t.Fatal(err)
	}
	if err := a.recordTeamMessage(ctx, "lead", message, "Team message msg-1 from lead:\nhello"); err != nil {
		t.Fatal(err)
	}
	events, err := st.LoadSession(ctx, message.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != session.EventUserMessage {
		t.Fatalf("receipt events = %+v", events)
	}
	var data struct {
		Source struct {
			Kind      string `json:"kind"`
			TeamID    string `json:"teamId"`
			MessageID string `json:"messageId"`
		} `json:"source"`
	}
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source.Kind != "team-message" || data.Source.TeamID != "team:lead" || data.Source.MessageID != message.ID {
		t.Fatalf("receipt source = %+v", data.Source)
	}
}

func TestTeamReceiptAndDeliveryUseOneSQLiteCommit(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-atomic-receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"lead", "team:lead:worker"} {
		if err := st.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{store: st}
	message := team.Message{ID: "msg-1", SenderID: "lead", SenderName: "lead", TargetID: "team:lead:worker", Content: "hello", Delivery: "quiet"}
	blocks := []llm.ContentBlock{llm.Text("Team message msg-1 from lead:"), llm.Text("hello"), {Kind: llm.BlockReasoning, Text: "internal"}}
	if err := a.recordTeamReceiptAndDelivery(ctx, "lead", message, "Team message msg-1 from lead:\nhello", blocks); err != nil {
		t.Fatal(err)
	}
	childEvents, err := st.LoadSession(ctx, message.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	rootEvents, err := st.LoadSession(ctx, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if len(childEvents) != 1 || childEvents[0].Type != session.EventUserMessage || len(rootEvents) != 1 || rootEvents[0].Type != session.EventTeamMessageDelivered {
		t.Fatalf("atomic receipt events child=%+v root=%+v", childEvents, rootEvents)
	}
	if err := a.recordTeamReceiptAndDelivery(ctx, "lead", message, "Team message msg-1 from lead:\nhello", blocks); err != nil {
		t.Fatal(err)
	}
	childEvents, _ = st.LoadSession(ctx, message.TargetID)
	rootEvents, _ = st.LoadSession(ctx, "lead")
	if len(childEvents) != 1 || len(rootEvents) != 1 {
		t.Fatalf("atomic receipt replay duplicated rows child=%d root=%d", len(childEvents), len(rootEvents))
	}
}

func TestTeamDispatchPersistsInboxBeforeDeliveryAcknowledgement(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-dispatch-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	a := &app{store: st, agentRegistry: agent.NewRegistry(), baseCtx: ctx}
	message := team.Message{
		ID: "msg-1", SenderID: "sender", SenderName: "sender", TargetID: "lead",
		Content: "recoverable", Delivery: "quiet",
	}
	if delivered, err := a.dispatchTeamMessageNow(ctx, "lead", message); err != nil || !delivered {
		t.Fatalf("dispatch delivered=%v err=%v", delivered, err)
	}
	defer func() { _ = a.agentRegistry.Close(agent.ID("lead")) }()

	events, err := st.LoadSession(ctx, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != session.EventAgentInboxSpliced || events[1].Type != session.EventTeamMessageDelivered {
		t.Fatalf("dispatch event order = %+v, want inbox splice before delivered", events)
	}
	for _, event := range events {
		if event.Type == session.EventUserMessage {
			t.Fatal("dispatch must not mark a target user message before the inbox is durably committed")
		}
	}
}

func TestTeamDispatchRetryRecognizesClaimedInboxReceipt(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "team-claimed-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "lead", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	a := &app{store: st, agentRegistry: agent.NewRegistry(), baseCtx: ctx}
	defer func() { _ = a.agentRegistry.Close(agent.ID("lead")) }()
	handle, err := a.sessionAgent("lead")
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.InjectContent([]llm.ContentBlock{llm.Text("already received")}, map[string]string{
		"team_id": "team:lead", "team_message_id": "msg-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := handle.ClaimStep(); !ok {
		t.Fatal("failed to claim the durable inbox receipt")
	}

	message := team.Message{ID: "msg-1", SenderID: "sender", SenderName: "sender", TargetID: "lead", Content: "retry", Delivery: "quiet"}
	delivered, err := a.dispatchTeamMessageNow(ctx, "lead", message)
	if err != nil || !delivered {
		t.Fatalf("retry delivered=%v err=%v", delivered, err)
	}
	events, err := st.LoadSession(ctx, "lead")
	if err != nil {
		t.Fatal(err)
	}
	inserted := 0
	for _, event := range events {
		if event.Type != session.EventAgentInboxSpliced {
			continue
		}
		var payload struct {
			Inserted []agent.Message `json:"inserted"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		for _, item := range payload.Inserted {
			if item.Metadata["team_message_id"] == "msg-1" {
				inserted++
			}
		}
	}
	if inserted != 1 {
		t.Fatalf("claimed Team receipt was inserted %d times, want once", inserted)
	}
	rootLog := session.New()
	if err := rootLog.Restore(events); err != nil {
		t.Fatal(err)
	}
	if !teamDeliveryRecorded(rootLog, "team:lead", "msg-1", "lead") {
		t.Fatal("retry did not commit the root delivery edge")
	}
}

func TestTeamDispatchIsTargetFIFOAndInFlightIdempotent(t *testing.T) {
	a := &app{}
	entered := make(chan string, 2)
	release := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	call := func(id string) func() (bool, error) {
		return func() (bool, error) {
			orderMu.Lock()
			order = append(order, id)
			orderMu.Unlock()
			entered <- id
			if id == "message-1" {
				<-release
			}
			return true, nil
		}
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := a.runOrderedTeamDispatch(context.Background(), "worker", "message-1", call("message-1"))
		firstDone <- err
	}()
	if got := <-entered; got != "message-1" {
		t.Fatalf("first dispatch entered as %q", got)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := a.runOrderedTeamDispatch(context.Background(), "worker", "message-2", call("message-2"))
		secondDone <- err
	}()
	select {
	case got := <-entered:
		t.Fatalf("second dispatch entered before first completed: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if strings.Join(gotOrder, ",") != "message-1,message-2" {
		t.Fatalf("dispatch order = %v, want message-1,message-2", gotOrder)
	}

	duplicateEntered := make(chan struct{})
	duplicateRelease := make(chan struct{})
	var duplicateCalls int
	duplicate := func() (bool, error) {
		duplicateCalls++
		close(duplicateEntered)
		<-duplicateRelease
		return true, nil
	}
	firstDuplicateDone := make(chan error, 1)
	go func() {
		_, err := a.runOrderedTeamDispatch(context.Background(), "worker", "message-3", duplicate)
		firstDuplicateDone <- err
	}()
	<-duplicateEntered
	secondDuplicateDone := make(chan error, 1)
	go func() {
		_, err := a.runOrderedTeamDispatch(context.Background(), "worker", "message-3", func() (bool, error) {
			return false, errors.New("duplicate dispatch ran")
		})
		secondDuplicateDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		a.teamDispatchMu.Lock()
		inFlight := a.teamDispatchInFlight["message-3"]
		registered := inFlight != nil && inFlight.waiters > 0
		a.teamDispatchMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("duplicate dispatch did not register in-flight")
		}
		time.Sleep(time.Millisecond)
	}
	close(duplicateRelease)
	if err := <-firstDuplicateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDuplicateDone; err != nil {
		t.Fatal(err)
	}
	if duplicateCalls != 1 {
		t.Fatalf("duplicate dispatch calls = %d, want 1", duplicateCalls)
	}
}
