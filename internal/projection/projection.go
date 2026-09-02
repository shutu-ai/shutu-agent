// Package projection contains the shared, durable-event projection used by
// every user-facing surface.  Runtime services may keep live indexes for
// latency, but a cold rebuild must always be possible through Build.
package projection

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/team"
)

// Entity is the lossless, provider-neutral state of one durable record. Data
// is a detached JSON object so callers cannot mutate the projection through a
// shared event payload.
type Entity struct {
	ID     string         `json:"id"`
	Scope  string         `json:"scope,omitempty"`
	Name   string         `json:"name,omitempty"`
	Status string         `json:"status,omitempty"`
	Seq    uint64         `json:"seq"`
	Data   map[string]any `json:"data"`
	// These timestamps are adapter metadata, not an additional serialized
	// authority. They let DSH-shaped wires retain millisecond creation/update
	// times while the durable event data remains the portable state.
	CreatedAt int64 `json:"-"`
	UpdatedAt int64 `json:"-"`
}

// Approval is the current durable approval state for one request. Settled
// approvals remain in the projection for audit/replay; Pending distinguishes
// cards that still need a user decision.
type Approval struct {
	ID       string         `json:"id"`
	CallID   string         `json:"callId,omitempty"`
	ToolName string         `json:"toolName,omitempty"`
	Outcome  string         `json:"outcome"`
	Pending  bool           `json:"pending"`
	Seq      uint64         `json:"seq"`
	Data     map[string]any `json:"data"`
}

// SurfaceEntry is one model-visible message together with the durable event
// sequence that currently owns it.  It lets consumers such as token metering
// retain positional diagnostics without reimplementing replacement folding.
type SurfaceEntry struct {
	Seq     uint64      `json:"seq"`
	Message llm.Message `json:"message"`
}

// PlanMode is the durable plan-mode switch, independent of the in-memory
// plan engine.
type PlanMode struct {
	Active  bool `json:"active"`
	Pending bool `json:"pending"`
}

// Snapshot is the cross-entry projection rebuilt from one ordered durable
// event stream. History remains model-visible data; the other fields are
// UI/control projections and never become model history implicitly.
type Snapshot struct {
	AsOfSeq       uint64                      `json:"asOfSeq"`
	History       []llm.Message               `json:"history"`
	Surface       []SurfaceEntry              `json:"surface"`
	SessionList   session.SessionListMetadata `json:"sessionList"`
	Title         string                      `json:"title"`
	TitleSource   string                      `json:"titleSource"`
	FirstUserText string                      `json:"firstUserText"`
	Goals         []Entity                    `json:"goals"`
	Plans         []Entity                    `json:"plans"`
	PlanTodos     []Entity                    `json:"planTodos"`
	Todos         map[string]any              `json:"todos,omitempty"`
	PlanMode      PlanMode                    `json:"planMode"`
	Permission    string                      `json:"permission,omitempty"`
	SandboxMode   string                      `json:"sandboxMode,omitempty"`
	Approvals     []Approval                  `json:"approvals"`
	Feedback      []Entity                    `json:"feedback"`
	Jobs          []Entity                    `json:"jobs"`
	MCPActivity   []Entity                    `json:"mcpActivity"`
	Team          *team.Snapshot              `json:"team,omitempty"`
}

// Cursor is the live counterpart of Build. It accepts only already committed
// events, validates them through the same session log boundary, and folds each
// event once. Snapshot returns a detached value, so a transport cannot turn a
// UI response into a second mutable authority.
type Cursor struct {
	mu        sync.Mutex
	log       *session.Log
	events    []session.Event
	snapshot  Snapshot
	goals     map[string]Entity
	plans     map[string]Entity
	planTodos map[string]Entity
	approvals map[string]Approval
	jobs      map[string]Entity
	team      *team.Board
}

// NewCursor creates a live projection from a durable prefix. Open turn tails
// are accepted because they are normal live state; malformed vocabulary,
// provenance, sequence, command, and workflow records remain rejected.
func NewCursor(events []session.Event) (*Cursor, error) {
	if err := validateProjectionEvents(events); err != nil {
		return nil, err
	}
	log, err := replayLog(events)
	if err != nil {
		return nil, err
	}
	cursor := &Cursor{
		log:       log,
		events:    cloneEvents(events),
		snapshot:  Snapshot{Todos: nil, SessionList: session.NewSessionListMetadata()},
		goals:     make(map[string]Entity),
		plans:     make(map[string]Entity),
		planTodos: make(map[string]Entity),
		approvals: make(map[string]Approval),
		jobs:      make(map[string]Entity),
	}
	for _, event := range events {
		if err := cursor.apply(event); err != nil {
			return nil, err
		}
	}
	cursor.refresh()
	return cursor, nil
}

// Append incorporates one event after the durable transaction has committed.
// A failed append leaves both the cursor and its event prefix unchanged.
func (c *Cursor) Append(event session.Event) error {
	if c == nil {
		return fmt.Errorf("projection: nil cursor")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log == nil {
		return fmt.Errorf("projection: uninitialized cursor")
	}
	if err := validateProjectionEvent(event); err != nil {
		return err
	}
	detached := cloneEvents([]session.Event{event})[0]
	var rollback *team.Snapshot
	if c.team != nil {
		snapshot := c.team.Snapshot()
		rollback = &snapshot
	}
	if err := c.apply(detached); err != nil {
		if rollback == nil {
			c.team = nil
		} else if board, boardErr := team.New(rollback.TeamID, nil); boardErr == nil {
			if restoreErr := board.Restore(*rollback); restoreErr == nil {
				c.team = board
			}
		}
		return err
	}
	if err := c.log.AppendPersisted(detached); err != nil {
		if rollback == nil {
			c.team = nil
		} else if board, boardErr := team.New(rollback.TeamID, nil); boardErr == nil {
			if restoreErr := board.Restore(*rollback); restoreErr == nil {
				c.team = board
			}
		}
		return err
	}
	c.events = append(c.events, detached)
	c.refresh()
	return nil
}

// validateProjectionEvent protects control-state projections from silently
// coercing malformed durable values. The session log vocabulary check admits
// internal event payload variants, but plan/mode is a typed control fact whose
// invalid value must never become the safe-looking default (inactive).
func validateProjectionEvent(event session.Event) error {
	switch event.Type {
	case session.EventPlanMode:
		data := object(event.Data)
		active, present := data["active"]
		if !present {
			return fmt.Errorf("projection: %s at seq %d is missing boolean active", event.Type, event.Seq)
		}
		if _, ok := active.(bool); !ok {
			return fmt.Errorf("projection: %s at seq %d active must be boolean", event.Type, event.Seq)
		}
		if pending, present := data["pending"]; present {
			if _, ok := pending.(bool); !ok {
				return fmt.Errorf("projection: %s at seq %d pending must be boolean", event.Type, event.Seq)
			}
		}
	case session.EventPermissionPreset:
		var data struct {
			Preset string `json:"preset"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil || data.Preset == "" {
			return fmt.Errorf("projection: %s at seq %d is missing preset", event.Type, event.Seq)
		}
	case session.EventSandboxMode:
		var data struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil || data.Mode == "" {
			return fmt.Errorf("projection: %s at seq %d is missing mode", event.Type, event.Seq)
		}
	}
	return nil
}

func validateProjectionEvents(events []session.Event) error {
	var teamBoard *team.Board
	for _, event := range events {
		if err := validateProjectionEvent(event); err != nil {
			return err
		}
		if err := validateTeamProjectionEvent(&teamBoard, event); err != nil {
			return err
		}
	}
	return nil
}

func validateTeamProjectionEvent(board **team.Board, event session.Event) error {
	switch event.Type {
	case session.EventTeamSnapshot:
		var snapshot team.Snapshot
		if err := json.Unmarshal(event.Data, &snapshot); err != nil {
			return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
		}
		if *board == nil || (*board).TeamID() != snapshot.TeamID {
			next, err := team.New(snapshot.TeamID, nil)
			if err != nil {
				return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
			}
			*board = next
		}
		if err := (*board).Restore(snapshot); err != nil {
			return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
		}
		return nil
	case session.EventTeamMember, session.EventTeamTask, session.EventTeamMessageQueued,
		session.EventTeamMessageDelivered:
		var selector struct {
			TeamID string `json:"teamId"`
		}
		if err := json.Unmarshal(event.Data, &selector); err != nil {
			return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
		}
		if *board == nil {
			if selector.TeamID == "" {
				return fmt.Errorf("projection: %s at seq %d is missing teamId", event.Type, event.Seq)
			}
			next, err := team.New(selector.TeamID, nil)
			if err != nil {
				return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
			}
			*board = next
		}
		if err := (*board).ApplyEvent(event.Type, event.Data); err != nil {
			return fmt.Errorf("projection: %s at seq %d: %w", event.Type, event.Seq, err)
		}
		return nil
	default:
		return nil
	}
}

// Snapshot returns a detached live projection at the latest committed event.
func (c *Cursor) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneSnapshot(c.snapshot)
}

// Events returns the durable prefix currently incorporated by the cursor.
func (c *Cursor) Events() []session.Event {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneEvents(c.events)
}

// Build validates and folds events exactly as a cold restart does. It is
// intentionally the only constructor for a complete Snapshot, which makes a
// caller choose an explicit error path instead of silently inventing state
// when an event cannot be replayed.
func Build(events []session.Event) (Snapshot, error) {
	cursor, err := NewCursor(events)
	if err != nil {
		return Snapshot{}, err
	}
	return cursor.Snapshot(), nil
}

// BuildWithImageResolver is the runtime-aware model projection. The durable
// snapshot remains path-free and reproducible; this explicit boundary only
// resolves attachment references for the provider request that is about to
// be sent. Callers without a resolver should use Build.
func BuildWithImageResolver(events []session.Event, resolve func(llm.ImageRef) llm.ImageRef) (Snapshot, error) {
	snapshot, err := Build(events)
	if err != nil || resolve == nil {
		return snapshot, err
	}
	resolveMessages(snapshot.History, resolve)
	for i := range snapshot.Surface {
		message := []llm.Message{snapshot.Surface[i].Message}
		resolveMessages(message, resolve)
		snapshot.Surface[i].Message = message[0]
	}
	return snapshot, nil
}

func resolveMessages(messages []llm.Message, resolve func(llm.ImageRef) llm.ImageRef) {
	if resolve == nil {
		return
	}
	var visit func([]llm.ContentBlock)
	visit = func(blocks []llm.ContentBlock) {
		for i := range blocks {
			if blocks[i].Kind == llm.BlockImage {
				blocks[i].Image = resolve(blocks[i].Image)
			}
			visit(blocks[i].Blocks)
		}
	}
	for i := range messages {
		visit(messages[i].Content)
	}
}

func replayLog(events []session.Event) (*session.Log, error) {
	log := session.New()
	if err := log.Restore(cloneEvents(events)); err == nil {
		return log, nil
	}
	// A live session can legitimately end at an open turn/step. Restore is
	// deliberately stricter because it is the completed-session recovery
	// boundary; replay through the persisted append path for this projection
	// so the event vocabulary, sequence, provenance, command and workflow
	// checks remain active without inventing a terminal lifecycle fact.
	log = session.New()
	for _, event := range cloneEvents(events) {
		if err := log.AppendPersisted(event); err != nil {
			return nil, fmt.Errorf("projection: restore events: %w", err)
		}
	}
	return log, nil
}

func (c *Cursor) apply(event session.Event) error {
	if err := validateTeamProjectionEvent(&c.team, event); err != nil {
		return err
	}
	data := object(event.Data)
	switch event.Type {
	case session.EventSessionTitle:
		if title := stringValue(data, "title", "text"); title != "" {
			c.snapshot.Title = session.NormalizeTitle(title, session.TitleMaxBytes)
			c.snapshot.TitleSource = objectValue(object(data["source"]), "kind")
		}
	case session.EventUserMessage:
		if c.snapshot.FirstUserText == "" {
			c.snapshot.FirstUserText = session.FirstEligibleUserText([]session.Event{event})
		}
	case session.EventGoalRoundStart:
		c.applyGoalRoundStart(event, data)
	case session.EventPlanMode:
		c.snapshot.PlanMode = PlanMode{Active: boolValue(data, "active"), Pending: boolValue(data, "pending")}
	case session.EventPermissionPreset:
		c.snapshot.Permission = normalizeProjectionPermission(stringValue(data, "preset", "permission", "current"))
	case session.EventSandboxMode:
		mode := stringValue(data, "mode")
		c.snapshot.SandboxMode = mode
		c.snapshot.Permission = normalizeProjectionPermission(mode)
	case session.EventPlanCreate, session.EventPlanUpdate, session.EventPlanStatus, session.EventPlanDelete:
		c.applyPlanEvent(event, data)
	case session.EventTodoWrite:
		if todos, ok := data["todos"]; ok {
			c.snapshot.Todos = detachedObject(map[string]any{"todos": todos})
		}
	case session.EventInteractRequest, session.EventInteractResolve, session.EventInteractCancel,
		session.EventApprovalAsked, session.EventApprovalDecided:
		foldApproval(c.approvals, event, data)
	case session.EventFeedbackRecord:
		c.snapshot.Feedback = append(c.snapshot.Feedback, entityFrom(event, data, "feedback"))
	case session.EventJobStart, session.EventJobStatus, session.EventJobDone:
		id := stringValue(data, "jobId", "id")
		if id != "" {
			job := entityFrom(event, data, id)
			if previous, ok := c.jobs[id]; ok {
				// job/status and job/done are intentionally lean event facts. Keep
				// immutable start metadata and merge the lifecycle delta so a cold
				// projection remains equivalent to the live job registry view.
				job.CreatedAt = previous.CreatedAt
				job.Data = mergeObject(previous.Data, job.Data)
				if job.Name == "" {
					job.Name = previous.Name
				}
				if job.Status == "" {
					job.Status = previous.Status
				}
			}
			c.jobs[id] = job
		}
	case session.EventMcpList, session.EventMcpCall:
		c.snapshot.MCPActivity = append(c.snapshot.MCPActivity, entityFrom(event, data, stringValue(data, "name", "tool")))
	case session.EventTeamSnapshot, session.EventTeamMember, session.EventTeamTask,
		session.EventTeamMessageQueued, session.EventTeamMessageDelivered:
		// Team state folded through validateTeamProjectionEvent.
	}
	c.snapshot.SessionList.Apply(event)
	return nil
}

func (c *Cursor) refresh() {
	// History is folded from the cursor's committed event prefix through the
	// session package's canonical event fold. The validation Log remains only
	// the append admission boundary; it is not a second history authority.
	c.snapshot.History = session.DeriveHistoryEvents(c.events, nil)
	c.snapshot.Surface = foldSurface(c.events)
	if len(c.events) > 0 {
		c.snapshot.AsOfSeq = c.events[len(c.events)-1].Seq
	}
	if c.snapshot.Title == "" && c.snapshot.FirstUserText != "" {
		c.snapshot.Title = session.FallbackTitle(c.snapshot.FirstUserText, session.TitleFallbackMaxWords, session.TitleFallbackMaxBytes)
		if c.snapshot.Title != "" {
			c.snapshot.TitleSource = session.TitleSourceFallback
		}
	}
	c.snapshot.Goals = sortedEntities(c.goals)
	c.snapshot.Plans = sortedEntities(c.plans)
	c.snapshot.PlanTodos = sortedEntities(c.planTodos)
	c.snapshot.Approvals = sortedApprovals(c.approvals)
	c.snapshot.Jobs = sortedEntities(c.jobs)
	if c.team != nil {
		teamSnapshot := c.team.Snapshot()
		c.snapshot.Team = cloneTeamSnapshot(&teamSnapshot)
	} else {
		c.snapshot.Team = nil
	}
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.History = cloneMessages(in.History)
	if in.Surface != nil {
		out.Surface = make([]SurfaceEntry, len(in.Surface))
		for i, entry := range in.Surface {
			out.Surface[i] = SurfaceEntry{Seq: entry.Seq, Message: cloneMessages([]llm.Message{entry.Message})[0]}
		}
	}
	if in.SessionList.LastPromptAt != nil {
		last := *in.SessionList.LastPromptAt
		out.SessionList.LastPromptAt = &last
	}
	out.Todos = detachedObjectIfPresent(in.Todos)
	out.Goals = cloneEntities(in.Goals)
	out.Plans = cloneEntities(in.Plans)
	out.PlanTodos = cloneEntities(in.PlanTodos)
	out.Approvals = cloneApprovals(in.Approvals)
	out.Feedback = cloneEntities(in.Feedback)
	out.Jobs = cloneEntities(in.Jobs)
	out.MCPActivity = cloneEntities(in.MCPActivity)
	out.Team = cloneTeamSnapshot(in.Team)
	return out
}

func cloneTeamSnapshot(in *team.Snapshot) *team.Snapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Tasks = append([]team.Task(nil), in.Tasks...)
	out.Messages = append([]team.Message(nil), in.Messages...)
	out.Members = append([]team.MemberSnapshot(nil), in.Members...)
	return &out
}

// foldSurface is the positional companion to session.Log.DeriveHistory. It
// intentionally delegates message decoding to the session package and owns
// only the durable replacement operation, so callers cannot grow another
// interpretation of the model-visible surface.
func foldSurface(events []session.Event) []SurfaceEntry {
	entries := make([]SurfaceEntry, 0)
	for _, event := range events {
		message, ok := session.DeriveEventMessage(event)
		if !ok {
			continue
		}
		entry := SurfaceEntry{Seq: event.Seq, Message: message}
		if replacement, replaces := session.SurfaceReplacement(event); replaces {
			start, end := -1, -1
			for i, existing := range entries {
				if int64(existing.Seq) >= replacement.Start && int64(existing.Seq) <= replacement.End {
					if start < 0 {
						start = i
					}
					end = i
				}
			}
			if start >= 0 && end >= start {
				next := make([]SurfaceEntry, 0, len(entries)-end+start+1)
				next = append(next, entries[:start]...)
				next = append(next, entry)
				next = append(next, entries[end+1:]...)
				entries = next
				continue
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func detachedObjectIfPresent(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return detachedObject(value)
}

func cloneEntities(values []Entity) []Entity {
	if values == nil {
		return nil
	}
	out := make([]Entity, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Data = detachedObject(value.Data)
	}
	return out
}

func cloneApprovals(values []Approval) []Approval {
	if values == nil {
		return nil
	}
	out := make([]Approval, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Data = detachedObject(value.Data)
	}
	return out
}

func cloneMessages(values []llm.Message) []llm.Message {
	if values == nil {
		return nil
	}
	out := make([]llm.Message, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Content = cloneContentBlocks(value.Content)
		out[i].ToolCalls = append([]llm.ToolCall(nil), value.ToolCalls...)
	}
	return out
}

func cloneContentBlocks(values []llm.ContentBlock) []llm.ContentBlock {
	if values == nil {
		return nil
	}
	out := make([]llm.ContentBlock, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Blocks = cloneContentBlocks(value.Blocks)
		out[i].Raw = append(json.RawMessage(nil), value.Raw...)
	}
	return out
}

func cloneEvents(events []session.Event) []session.Event {
	out := make([]session.Event, len(events))
	for i, event := range events {
		out[i] = event
		out[i].Data = append(json.RawMessage(nil), event.Data...)
	}
	return out
}

func (c *Cursor) applyPlanEvent(event session.Event, data map[string]any) {
	scope := stringValue(data, "scope")
	id := stringValue(data, "id")
	if scope == "" || id == "" {
		return
	}
	switch event.Type {
	case session.EventPlanCreate:
		entity := planEntityFrom(event, data, scope, id)
		switch scope {
		case "goal":
			c.goals[id] = entity
		case "plan":
			c.plans[id] = entity
			c.linkProjectedPlan(entity)
		case "todo":
			c.planTodos[id] = entity
			c.addProjectedTodo(entity)
		}
	case session.EventPlanUpdate, session.EventPlanStatus:
		var target map[string]Entity
		switch scope {
		case "goal":
			target = c.goals
		case "plan":
			target = c.plans
		case "todo":
			c.updateProjectedTodo(event, id, data)
			return
		default:
			return
		}
		previous, ok := target[id]
		if !ok {
			return
		}
		update := detachedObject(data)
		if event.Type == session.EventPlanUpdate {
			if detail, ok := data["detail"].(map[string]any); ok {
				update = mergeObject(update, detail)
			}
		}
		previous.Data = mergeObject(previous.Data, update)
		previous.Seq = event.Seq
		previous.UpdatedAt = event.At.UnixMilli()
		if status := stringValue(previous.Data, "status"); status != "" {
			previous.Status = status
		}
		target[id] = previous
	case session.EventPlanDelete:
		switch scope {
		case "goal":
			delete(c.goals, id)
			for planID, entity := range c.plans {
				if stringValue(entity.Data, "goalId", "goal_id") == id {
					c.removeProjectedPlan(planID, false)
				}
			}
		case "plan":
			c.removeProjectedPlan(id, true)
		case "todo":
			delete(c.planTodos, id)
			c.deleteProjectedTodo(id)
		}
	}
}

func (c *Cursor) applyGoalRoundStart(event session.Event, data map[string]any) {
	id := stringValue(data, "goalId", "goal_id")
	round := intValue(data, "round")
	if id == "" || round <= 0 {
		return
	}
	goal, ok := c.goals[id]
	if !ok {
		return
	}
	if current := intValue(goal.Data, "roundsStarted"); round > current {
		goal.Data["roundsStarted"] = round
		goal.Data = detachedObject(goal.Data)
		goal.Seq = event.Seq
		goal.UpdatedAt = event.At.UnixMilli()
		c.goals[id] = goal
	}
}

// planEntityFrom flattens the optional full-record detail into the detached
// entity data. This keeps the shared projection useful to Web/SDK consumers
// without making them instantiate the disposable plan engine just to recover
// goals and plan steps after a cold restart.
func planEntityFrom(event session.Event, data map[string]any, scope, id string) Entity {
	record := detachedObject(data)
	if detail, ok := data["detail"].(map[string]any); ok {
		record = mergeObject(record, detail)
	}
	entity := entityFrom(event, record, id)
	entity.ID = id
	entity.Scope = scope
	entity.Data = record
	if status := stringValue(record, "status"); status != "" {
		entity.Status = status
	}
	return entity
}

func (c *Cursor) linkProjectedPlan(plan Entity) {
	goalID := stringValue(plan.Data, "goalId", "goal_id")
	if goalID == "" {
		return
	}
	goal, ok := c.goals[goalID]
	if !ok {
		return
	}
	goal.Data["plans"] = appendStringValue(goal.Data["plans"], plan.ID)
	goal.Data = detachedObject(goal.Data)
	c.goals[goalID] = goal
}

func (c *Cursor) unlinkProjectedPlan(plan Entity) {
	goalID := stringValue(plan.Data, "goalId", "goal_id")
	goal, ok := c.goals[goalID]
	if !ok {
		return
	}
	goal.Data["plans"] = removeStringValue(goal.Data["plans"], plan.ID)
	goal.Data = detachedObject(goal.Data)
	c.goals[goalID] = goal
}

func (c *Cursor) addProjectedTodo(todo Entity) {
	planID := stringValue(todo.Data, "planId", "plan_id")
	if planID == "" {
		return
	}
	plan, ok := c.plans[planID]
	if !ok {
		return
	}
	steps, _ := plan.Data["steps"].([]any)
	for _, raw := range steps {
		if step, ok := raw.(map[string]any); ok && stringValue(step, "id") == todo.ID {
			return
		}
	}
	steps = append(steps, detachedObject(todo.Data))
	plan.Data["steps"] = steps
	plan.Data = detachedObject(plan.Data)
	plan.Seq = todo.Seq
	c.plans[planID] = plan
}

// removeProjectedPlan mirrors the plan engine's cascade contract in the
// event-only projection. A plan delete (and the provider's goal-delete
// cascade) must remove the detached todo index as well as the nested steps;
// otherwise Native/Web todo reads can resurrect records that no longer belong
// to any live plan after a cold rebuild.
func (c *Cursor) removeProjectedPlan(id string, unlink bool) {
	entity, ok := c.plans[id]
	if ok && unlink {
		c.unlinkProjectedPlan(entity)
	}
	delete(c.plans, id)
	for todoID, todo := range c.planTodos {
		if stringValue(todo.Data, "planId", "plan_id") == id {
			delete(c.planTodos, todoID)
		}
	}
}

func (c *Cursor) updateProjectedTodo(event session.Event, id string, data map[string]any) {
	if todo, ok := c.planTodos[id]; ok {
		todo.Data = mergeObject(todo.Data, data)
		todo.Seq = event.Seq
		if status := stringValue(todo.Data, "status"); status != "" {
			todo.Status = status
		}
		c.planTodos[id] = todo
	}
	for planID, plan := range c.plans {
		steps, ok := plan.Data["steps"].([]any)
		if !ok {
			continue
		}
		changed := false
		for index, raw := range steps {
			step, ok := raw.(map[string]any)
			if !ok || stringValue(step, "id") != id {
				continue
			}
			step = mergeObject(step, data)
			steps[index] = step
			changed = true
		}
		if changed {
			plan.Data["steps"] = steps
			plan.Data = detachedObject(plan.Data)
			plan.Seq = event.Seq
			c.plans[planID] = plan
		}
	}
}

func (c *Cursor) deleteProjectedTodo(id string) {
	for planID, plan := range c.plans {
		steps, ok := plan.Data["steps"].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(steps))
		for _, raw := range steps {
			step, ok := raw.(map[string]any)
			if ok && stringValue(step, "id") == id {
				continue
			}
			filtered = append(filtered, raw)
		}
		if len(filtered) != len(steps) {
			plan.Data["steps"] = filtered
			plan.Data = detachedObject(plan.Data)
			c.plans[planID] = plan
		}
	}
}

func appendStringValue(value any, id string) []string {
	values := stringValues(value)
	for _, candidate := range values {
		if candidate == id {
			return values
		}
	}
	return append(values, id)
}

func removeStringValue(value any, id string) []string {
	values := stringValues(value)
	out := values[:0]
	for _, candidate := range values {
		if candidate != id {
			out = append(out, candidate)
		}
	}
	return out
}

func stringValues(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func foldApproval(approvals map[string]Approval, event session.Event, data map[string]any) {
	canonicalType, canonical, ok := session.CanonicalApprovalEvent(event.Type, data)
	if ok {
		data = object(mustJSON(canonical))
		event.Type = canonicalType
	}
	id := stringValue(data, "id")
	if id == "" {
		return
	}
	current := approvals[id]
	current.ID = id
	current.CallID = stringValue(data, "callId", "call_id")
	current.ToolName = stringValue(data, "toolName", "tool_name")
	current.Seq = event.Seq
	current.Data = detachedObject(data)
	switch event.Type {
	case session.EventApprovalAsked:
		current.Outcome = "pending"
		current.Pending = true
	case session.EventApprovalDecided:
		current.Outcome = stringValue(data, "outcome")
		if current.Outcome == "" {
			current.Outcome = "rejected"
		}
		current.Pending = false
	}
	approvals[id] = current
}

func entityFrom(event session.Event, data map[string]any, fallbackID string) Entity {
	id := stringValue(data, "id", "jobId", "job_id", "name")
	if id == "" {
		id = fallbackID
	}
	at := event.At.UnixMilli()
	return Entity{
		ID:        id,
		Name:      stringValue(data, "name", "tool"),
		Status:    stringValue(data, "status", "state"),
		Seq:       event.Seq,
		Data:      detachedObject(data),
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func sortedEntities(values map[string]Entity) []Entity {
	out := make([]Entity, 0, len(values))
	for _, entity := range values {
		out = append(out, entity)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].ID < out[j].ID
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

func sortedApprovals(values map[string]Approval) []Approval {
	out := make([]Approval, 0, len(values))
	for _, approval := range values {
		out = append(out, approval)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].ID < out[j].ID
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

func object(raw any) map[string]any {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(encoded, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func detachedObject(value map[string]any) map[string]any {
	return object(value)
}

func mergeObject(base, update map[string]any) map[string]any {
	out := detachedObject(base)
	for key, value := range update {
		out[key] = value
	}
	return detachedObject(out)
}

func objectValue(value map[string]any, key string) string {
	return stringValue(value, key)
}

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok {
			return text
		}
	}
	return ""
}

func intValue(value map[string]any, key string) int {
	switch number := value[key].(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		if number >= 0 && number <= float64(^uint(0)>>1) && number == float64(int(number)) {
			return int(number)
		}
	}
	return 0
}

func boolValue(value map[string]any, key string) bool {
	result, _ := value[key].(bool)
	return result
}

// normalizeProjectionPermission projects DSH permission/sandbox vocabulary
// onto the three durable user-facing tiers. Unknown values retain the safe
// standard tier rather than inventing a fourth runtime policy.
func normalizeProjectionPermission(value string) string {
	switch value {
	case "read-only", "readonly":
		return "readonly"
	case "danger-full-access", "full":
		return "full"
	default:
		return "standard"
	}
}
