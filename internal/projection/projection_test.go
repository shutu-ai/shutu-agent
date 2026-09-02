package projection

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/team"
)

func TestBuildRejectsMalformedPlanMode(t *testing.T) {
	events := []session.Event{{
		Seq: 0, Type: session.EventPlanMode, Version: session.EventVersion,
		Data: json.RawMessage(`{"active":"yes"}`),
	}}
	if _, err := Build(events); err == nil {
		t.Fatal("malformed plan-mode event was accepted")
	}
}

func TestBuildFoldsPermissionPresetAndSandboxMode(t *testing.T) {
	log := session.New()
	appends := []struct {
		typ  string
		data any
	}{
		{session.EventPermissionPreset, session.NewPermissionPreset("workspace-write")},
		{session.EventSandboxMode, session.NewSandboxMode("danger-full-access")},
		{session.EventPermissionPreset, session.NewPermissionPreset("read-only")},
		{session.EventSandboxMode, session.NewSandboxMode("read-only")},
	}
	for _, item := range appends {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatalf("append %s: %v", item.typ, err)
		}
	}
	cold, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if cold.Permission != "readonly" || cold.SandboxMode != "read-only" {
		t.Fatalf("permission projection = %q/%q, want readonly/read-only", cold.Permission, cold.SandboxMode)
	}

	live, err := NewCursor(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range log.Events() {
		if err := live.Append(event); err != nil {
			t.Fatalf("live append %s: %v", event.Type, err)
		}
	}
	if got := live.Snapshot(); !reflect.DeepEqual(cold, got) {
		t.Fatalf("live/cold permission snapshots differ:\ncold=%#v\nlive=%#v", cold, got)
	}
}

func TestBuildRejectsMalformedPermissionControlFacts(t *testing.T) {
	for _, test := range []struct {
		event session.Event
		want  string
	}{
		{session.Event{Seq: 1, Type: session.EventPermissionPreset, Version: session.EventVersion, Data: json.RawMessage(`{}`)}, "missing preset"},
		{session.Event{Seq: 1, Type: session.EventSandboxMode, Version: session.EventVersion, Data: json.RawMessage(`{}`)}, "missing mode"},
	} {
		if _, err := Build([]session.Event{test.event}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("Build(%s) error = %v, want %s", test.event.Type, err, test.want)
		}
	}
}

func TestBuildReplaysSharedStateAcrossColdProjection(t *testing.T) {
	log := session.New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := log.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEvent(session.EventTurnStart, session.NewTurnStartAt(1))
	appendEvent(session.EventUserMessage, session.NewUserMessage("inspect the deployment logs"))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("plan", "p1", "Inspect", nil))
	appendEvent(session.EventPlanStatus, session.NewPlanStatus("plan", "p1", "in-progress"))
	appendEvent(session.EventTodoWrite, session.NewTodoWrite([]map[string]any{{"id": "todo-1", "status": "pending"}}))
	appendEvent(session.EventInteractRequest, session.NewInteractRequestDetailWithCallID("approval-1", "call-1", "bash", "run command", "{}", nil))
	appendEvent(session.EventInteractResolve, map[string]any{"id": "approval-1", "approved": true, "callId": "call-1"})
	appendEvent(session.EventFeedbackRecord, session.NewFeedbackRecord("useful"))
	appendEvent(session.EventJobStart, session.NewJobStart("job-1", "bash", "echo ok", "session-1"))
	appendEvent(session.EventJobDone, session.NewJobDone("job-1", "completed", "exit 0", "ok"))
	appendEvent(session.EventMcpList, session.NewMcpList(2))
	appendEvent(session.EventTurnEnd, session.NewTurnEndAt(1, "completed", ""))

	first, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "inspect the deployment logs" || first.TitleSource != session.TitleSourceFallback {
		t.Fatalf("title projection = %q/%q", first.Title, first.TitleSource)
	}
	if first.SessionList.Blank || len(first.History) != 1 {
		t.Fatalf("session projection = blank=%v history=%d", first.SessionList.Blank, len(first.History))
	}
	if len(first.Plans) != 1 || first.Plans[0].Status != "in-progress" {
		t.Fatalf("plan projection = %#v", first.Plans)
	}
	if first.Todos == nil || len(first.Approvals) != 1 || first.Approvals[0].Outcome != "allowed-once" || first.Approvals[0].Pending {
		t.Fatalf("control projection = todos=%#v approvals=%#v", first.Todos, first.Approvals)
	}
	if len(first.Jobs) != 1 || first.Jobs[0].Status != "completed" || first.Jobs[0].Data["kind"] != "bash" || first.Jobs[0].Data["label"] != "echo ok" || first.Jobs[0].CreatedAt == 0 || first.Jobs[0].UpdatedAt == 0 || first.Jobs[0].CreatedAt > first.Jobs[0].UpdatedAt {
		t.Fatalf("job lifecycle projection lost start metadata: %#v", first.Jobs)
	}
	if len(first.Feedback) != 1 || len(first.MCPActivity) != 1 {
		t.Fatalf("activity projection = jobs=%#v feedback=%#v mcp=%#v", first.Jobs, first.Feedback, first.MCPActivity)
	}

	restored := session.New()
	if err := restored.Restore(log.Events()); err != nil {
		t.Fatal(err)
	}
	second, err := Build(restored.Events())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cold rebuild differs:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestBuildJobProjectionMergesLifecycleMetadata(t *testing.T) {
	events := []session.Event{
		{Seq: 1, Type: session.EventJobStart, At: time.UnixMilli(100), Version: session.EventVersion, Data: mustJSON(map[string]any{
			"jobId": "job-1", "kind": "bash", "label": "compile", "ownerSession": "session-1",
		})},
		{Seq: 2, Type: session.EventJobStatus, At: time.UnixMilli(200), Version: session.EventVersion, Data: mustJSON(map[string]any{
			"jobId": "job-1", "status": "stopping", "detail": "cancel requested",
		})},
		{Seq: 3, Type: session.EventJobDone, At: time.UnixMilli(300), Version: session.EventVersion, Data: mustJSON(map[string]any{
			"jobId": "job-1", "status": "killed", "detail": "cancelled", "outputSummary": "partial output",
		})},
	}
	snapshot, err := Build(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("jobs = %#v", snapshot.Jobs)
	}
	job := snapshot.Jobs[0]
	if job.CreatedAt != 100 || job.UpdatedAt != 300 || job.Data["kind"] != "bash" || job.Data["label"] != "compile" || job.Data["status"] != "killed" {
		t.Fatalf("merged job = %#v", job)
	}
}

func TestBuildFoldsTeamSnapshotAndIncrementalEvents(t *testing.T) {
	boardSnapshot := team.Snapshot{
		TeamID: "team-alpha",
		Next:   1,
		Tasks: []team.Task{{
			ID: "task-1", Revision: 1, Subject: "first", Status: team.TaskPending,
		}},
	}
	log := session.New()
	if _, err := log.Append(session.EventTeamSnapshot, boardSnapshot); err != nil {
		t.Fatal(err)
	}
	update := team.TaskEvent{
		Version: 1, TeamID: "team-alpha",
		Task: team.Task{ID: "task-1", Revision: 2, Subject: "first", Status: team.TaskInProgress},
	}
	if _, err := log.Append(session.EventTeamTask, update); err != nil {
		t.Fatal(err)
	}

	cold, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if cold.Team == nil || cold.Team.TeamID != "team-alpha" || len(cold.Team.Tasks) != 1 ||
		cold.Team.Tasks[0].Revision != 2 || cold.Team.Tasks[0].Status != team.TaskInProgress {
		t.Fatalf("cold Team projection = %#v", cold.Team)
	}

	live, err := NewCursor(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range log.Events() {
		if err := live.Append(event); err != nil {
			t.Fatalf("live append %s: %v", event.Type, err)
		}
	}
	if !reflect.DeepEqual(cold, live.Snapshot()) {
		t.Fatalf("live/cold Team projections differ:\ncold=%#v\nlive=%#v", cold, live.Snapshot())
	}

	invalid := session.Event{
		Seq: uint64(len(log.Events()) + 1), Type: session.EventTeamTask,
		Version: session.EventVersion, Data: json.RawMessage(`{"teamId":"team-alpha","task":{"id":"task-1","revision":1}}`),
	}
	if _, err := Build(append(log.Events(), invalid)); err == nil {
		t.Fatal("non-contiguous Team task was accepted")
	}
}

func TestBuildProjectsGoalPlanAndTodoIntoSharedControlState(t *testing.T) {
	log := session.New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := log.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("goal", "goal-1", "Ship", nil, map[string]any{
		"objective": "Ship the agent", "status": "in-progress", "plans": []string{},
	}))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("plan", "plan-1", "Implement", nil, map[string]any{
		"goalId": "goal-1", "status": "pending", "steps": []any{},
	}))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("todo", "todo-1", "Add tests", nil, map[string]any{
		"planId": "plan-1", "status": "pending",
	}))
	appendEvent(session.EventPlanStatus, session.NewPlanStatus("plan", "plan-1", "in-progress"))

	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Goals) != 1 || len(snapshot.Plans) != 1 {
		t.Fatalf("shared plan projection = goals=%#v plans=%#v", snapshot.Goals, snapshot.Plans)
	}
	if snapshot.Goals[0].ID != "goal-1" || snapshot.Goals[0].Data["objective"] != "Ship the agent" {
		t.Fatalf("goal projection = %#v", snapshot.Goals[0])
	}
	if got := stringValues(snapshot.Goals[0].Data["plans"]); len(got) != 1 || got[0] != "plan-1" {
		t.Fatalf("goal plan links = %#v, want [plan-1]", snapshot.Goals[0].Data["plans"])
	}
	plan := snapshot.Plans[0]
	if plan.ID != "plan-1" || plan.Status != "in-progress" {
		t.Fatalf("plan projection = %#v", plan)
	}
	steps, ok := plan.Data["steps"].([]any)
	if !ok || len(steps) != 1 || stringValue(steps[0].(map[string]any), "id") != "todo-1" {
		t.Fatalf("plan steps = %#v, want todo-1", plan.Data["steps"])
	}
}

func TestBuildPlanDeleteCascadesDetachedTodoProjection(t *testing.T) {
	log := session.New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := log.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("goal", "goal-1", "Ship", nil))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("plan", "plan-1", "Implement", nil, map[string]any{
		"goalId": "goal-1", "steps": []any{},
	}))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("todo", "todo-1", "Test", nil, map[string]any{
		"planId": "plan-1",
	}))
	appendEvent(session.EventPlanDelete, session.NewPlanDelete("plan", "plan-1"))

	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Plans) != 0 || len(snapshot.PlanTodos) != 0 {
		t.Fatalf("plan deletion left projections: plans=%#v todos=%#v", snapshot.Plans, snapshot.PlanTodos)
	}
	if len(snapshot.Goals) != 1 || len(stringValues(snapshot.Goals[0].Data["plans"])) != 0 {
		t.Fatalf("plan deletion left goal link: %#v", snapshot.Goals)
	}

	appendEvent(session.EventPlanCreate, session.NewPlanCreate("plan", "plan-2", "Deduplicate", nil, map[string]any{
		"goalId": "goal-1", "steps": []any{map[string]any{"id": "todo-2", "title": "once"}},
	}))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("todo", "todo-2", "once", nil, map[string]any{
		"planId": "plan-2",
	}))
	snapshot, err = Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	steps, ok := snapshot.Plans[0].Data["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("plan snapshot duplicated todo: %#v", snapshot.Plans[0].Data["steps"])
	}
}

func TestBuildRejectsInvalidDurableStream(t *testing.T) {
	_, err := Build([]session.Event{{Seq: 2, Type: session.EventUserMessage, Data: []byte(`{"role":"user"}`)}})
	if err == nil {
		t.Fatal("invalid durable stream unexpectedly projected")
	}
}

func TestBuildAllowsAValidOpenTurnTail(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventTurnStart, session.NewTurnStartAt(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("still running")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatalf("open live tail should project: %v", err)
	}
	if snapshot.Title != "still running" || len(snapshot.History) != 1 {
		t.Fatalf("open-tail projection = title=%q history=%d", snapshot.Title, len(snapshot.History))
	}
}

func TestBuildAllowsHistoricalZeroBasedOpenTurnTail(t *testing.T) {
	events := []session.Event{
		{Seq: 0, Type: session.EventTurnStart, Version: session.EventVersion, Data: []byte(`{"turn":1}`)},
		{Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion, Data: []byte(`{"text":"legacy live tail"}`)},
	}
	snapshot, err := Build(events)
	if err != nil {
		t.Fatalf("zero-based open tail should project: %v", err)
	}
	if len(snapshot.History) != 1 || snapshot.History[0].Text() != "legacy live tail" {
		t.Fatalf("zero-based history = %#v", snapshot.History)
	}
	if len(snapshot.Surface) != 1 || snapshot.Surface[0].Seq != 1 {
		t.Fatalf("zero-based surface = %#v", snapshot.Surface)
	}
}

func TestBuildExposesReplacementAwareSurfaceWithDurableSequences(t *testing.T) {
	log := session.New()
	first, err := log.Append(session.EventUserMessage, session.NewUserMessage("old question"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage("old answer", nil, "stop"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageReplace("compressed summary", int64(first.Seq), int64(second.Seq))); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Surface) != 1 || snapshot.Surface[0].Seq != 3 || len(snapshot.History) != 1 {
		t.Fatalf("surface projection = %#v history=%#v", snapshot.Surface, snapshot.History)
	}
	if got := snapshot.Surface[0].Message.Text(); got != "compressed summary" {
		t.Fatalf("surface message = %q", got)
	}
	if got := snapshot.History[0].Text(); got != snapshot.Surface[0].Message.Text() {
		t.Fatalf("surface/history diverged: surface=%q history=%q", snapshot.Surface[0].Message.Text(), got)
	}
}

func TestBuildWithImageResolverKeepsDurableSnapshotPathFree(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageWithBlocks("see", []llm.ContentBlock{{
		Kind: llm.BlockImage, Image: llm.ImageRef{ID: "sha256:one", MediaType: "image/png"},
	}})); err != nil {
		t.Fatal(err)
	}
	events := log.Events()
	pure, err := Build(events)
	if err != nil {
		t.Fatal(err)
	}
	if pure.History[0].Content[0].Image.Path != "" {
		t.Fatalf("durable projection leaked image path: %#v", pure.History)
	}
	runtime, err := BuildWithImageResolver(events, func(ref llm.ImageRef) llm.ImageRef {
		ref.Path = "C:/attachments/one.png"
		return ref
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.History[0].Content[0].Image.Path != "C:/attachments/one.png" || runtime.Surface[0].Message.Content[0].Image.Path != "C:/attachments/one.png" {
		t.Fatalf("runtime projection did not resolve image path: history=%#v surface=%#v", runtime.History, runtime.Surface)
	}
	if pure.History[0].Content[0].Image.Path != "" {
		t.Fatal("runtime resolver mutated the pure projection")
	}
}

func TestBuildDetachesNestedState(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventTodoWrite, session.NewTodoWrite([]map[string]any{{"id": "todo-1", "meta": map[string]any{"owner": "user"}}})); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	todos := snapshot.Todos["todos"].([]any)
	todos[0].(map[string]any)["meta"].(map[string]any)["owner"] = "mutated"
	again, err := Build(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if again.Todos["todos"].([]any)[0].(map[string]any)["meta"].(map[string]any)["owner"] != "user" {
		t.Fatal("projection state aliases a previous nested value")
	}
}

func TestLiveCursorMatchesColdProjectionAfterEveryCommittedEvent(t *testing.T) {
	source := session.New()
	appendEvent := func(typ string, data any) {
		t.Helper()
		if _, err := source.Append(typ, data); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEvent(session.EventTurnStart, session.NewTurnStartAt(1))
	appendEvent(session.EventUserMessage, session.NewUserMessage("cursor equivalence"))
	appendEvent(session.EventPlanCreate, session.NewPlanCreate("plan", "p1", "Inspect", nil))
	appendEvent(session.EventTodoWrite, session.NewTodoWrite([]map[string]any{{"id": "todo-1", "status": "pending"}}))
	appendEvent(session.EventInteractRequest, session.NewInteractRequestDetailWithCallID("approval-1", "call-1", "bash", "run", "{}", nil))
	appendEvent(session.EventInteractResolve, map[string]any{"id": "approval-1", "approved": true, "callId": "call-1"})
	appendEvent(session.EventTurnEnd, session.NewTurnEndAt(1, "completed", ""))

	cursor, err := NewCursor(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range source.Events() {
		if err := cursor.Append(event); err != nil {
			t.Fatalf("cursor append seq %d: %v", event.Seq, err)
		}
		live := cursor.Snapshot()
		cold, err := Build(cursor.Events())
		if err != nil {
			t.Fatalf("cold rebuild at seq %d: %v", event.Seq, err)
		}
		if !reflect.DeepEqual(live, cold) {
			t.Fatalf("live/cold mismatch at seq %d:\nlive=%#v\ncold=%#v", event.Seq, live, cold)
		}
	}
}

func TestLiveCursorSnapshotIsDetached(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventTodoWrite, session.NewTodoWrite([]map[string]any{{"id": "todo-1", "meta": map[string]any{"owner": "user"}}})); err != nil {
		t.Fatal(err)
	}
	cursor, err := NewCursor(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cursor.Snapshot()
	snapshot.Todos["todos"].([]any)[0].(map[string]any)["meta"].(map[string]any)["owner"] = "mutated"
	if got := cursor.Snapshot().Todos["todos"].([]any)[0].(map[string]any)["meta"].(map[string]any)["owner"]; got != "user" {
		t.Fatalf("live snapshot aliases cursor state: %v", got)
	}
}
