// Package team implements the durable-state core of the experimental Agent
// Teams domain. Transport and UI adapters consume detached snapshots from this
// board; authorization remains the caller's Agent membership responsibility.
package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/llm"
)

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskCompleted  TaskStatus = "completed"
	TaskDeleted    TaskStatus = "deleted"
)

var (
	ErrTaskNotFound  = errors.New("team: task not found")
	ErrRevision      = errors.New("team: task revision conflict")
	ErrInvalidTask   = errors.New("team: invalid task")
	ErrTaskBlocked   = errors.New("team: task is blocked")
	ErrCycle         = errors.New("team: task dependency cycle")
	ErrMessageAbsent = errors.New("team: message not found")
	ErrTeamLimit     = errors.New("team: deployment limit exceeded")
)

type Task struct {
	ID          string     `json:"id"`
	Revision    int        `json:"revision"`
	Subject     string     `json:"subject"`
	Description string     `json:"description,omitempty"`
	Status      TaskStatus `json:"status"`
	OwnerID     string     `json:"ownerId,omitempty"`
	BlockedBy   []string   `json:"blockedBy,omitempty"`
	WriteScopes []string   `json:"writeScopes,omitempty"`
	DeletedAt   time.Time  `json:"deletedAt,omitempty"`
}

type TaskView struct {
	Task
	Ready             bool
	OwnerName         string
	WriteScopeWarning bool
}

type Message struct {
	ID         string `json:"id"`
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName,omitempty"`
	TargetID   string `json:"targetId"`
	Delivery   string `json:"delivery"` // quiet | wakeup
	Content    string `json:"content"`
	// ContentBlocks is the canonical rich payload. Content remains the legacy
	// text projection used by older callers and snapshots.
	ContentBlocks []llm.ContentBlock `json:"contentBlocks,omitempty"`
	CreatedAt     time.Time          `json:"createdAt"`
	Delivered     bool               `json:"delivered"`
}

// Snapshot is the durable, detached representation of one team board. A
// persistence adapter can store it as a session event or a database row; the
// board never exposes its live maps.
type Snapshot struct {
	TeamID   string           `json:"teamId"`
	Next     int              `json:"next"`
	MsgNext  uint64           `json:"messageNext"`
	Tasks    []Task           `json:"tasks"`
	Messages []Message        `json:"messages"`
	Members  []MemberSnapshot `json:"members,omitempty"`
}

// TaskEvent and mailbox event payloads mirror the reference Team journal's
// selector/version envelope. A task record is a complete revision, so replay
// can enforce contiguous CAS revisions without depending on a snapshot.
type TaskEvent struct {
	Version int    `json:"version"`
	TeamID  string `json:"teamId"`
	Task    Task   `json:"task"`
}

// taskEventWire intentionally excludes board-only fields. The reference Team
// journal stores the complete task revision, not the local deletion timestamp.
type taskEventWire struct {
	ID          string     `json:"id"`
	Revision    int        `json:"revision"`
	Subject     string     `json:"subject"`
	Description string     `json:"description"`
	Status      TaskStatus `json:"status"`
	OwnerID     string     `json:"ownerId,omitempty"`
	BlockedBy   []string   `json:"blockedBy"`
	WriteScopes []string   `json:"writeScopes"`
}

func (e TaskEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Version int           `json:"version"`
		TeamID  string        `json:"teamId"`
		Task    taskEventWire `json:"task"`
	}{
		Version: e.Version,
		TeamID:  e.TeamID,
		Task: taskEventWire{
			ID: e.Task.ID, Revision: e.Task.Revision, Subject: e.Task.Subject,
			Description: e.Task.Description, Status: e.Task.Status, OwnerID: e.Task.OwnerID,
			BlockedBy:   append([]string{}, e.Task.BlockedBy...),
			WriteScopes: append([]string{}, e.Task.WriteScopes...),
		},
	})
}

type MessageQueuedEvent struct {
	Version int     `json:"version"`
	TeamID  string  `json:"teamId"`
	Message Message `json:"message"`
}

type messageContentWire struct {
	Type       string               `json:"type"`
	Text       string               `json:"text,omitempty"`
	ID         string               `json:"id,omitempty"`
	ToolCallID string               `json:"toolCallId,omitempty"`
	Name       string               `json:"name,omitempty"`
	Arguments  string               `json:"arguments,omitempty"`
	IsError    bool                 `json:"isError,omitempty"`
	Attachment any                  `json:"attachment,omitempty"`
	Content    []messageContentWire `json:"content,omitempty"`
}

type messageEventWire struct {
	ID         string               `json:"id"`
	SenderID   string               `json:"senderId"`
	SenderName string               `json:"senderName"`
	TargetID   string               `json:"targetId"`
	Delivery   string               `json:"delivery"`
	Content    []messageContentWire `json:"content"`
}

func messageEventWireFrom(message Message) messageEventWire {
	blocks := message.ContentBlocks
	if len(blocks) == 0 && message.Content != "" {
		blocks = []llm.ContentBlock{llm.Text(message.Content)}
	}
	content := make([]messageContentWire, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, messageContentWireFrom(block))
	}
	return messageEventWire{ID: message.ID, SenderID: message.SenderID, SenderName: message.SenderName, TargetID: message.TargetID, Delivery: message.Delivery, Content: content}
}

func messageContentWireFrom(block llm.ContentBlock) messageContentWire {
	wire := messageContentWire{
		Type: string(block.Kind), Text: block.Text, Name: block.Name, Arguments: block.Arguments,
		IsError: block.IsError,
	}
	if wire.Type == "" {
		wire.Type = string(llm.BlockText)
	}
	if block.Kind == llm.BlockToolResult {
		wire.ToolCallID = block.CallID
	} else {
		wire.ID = block.CallID
	}
	if block.Kind == llm.BlockImage {
		wire.Attachment = map[string]any{
			"attachmentId": block.Image.ID, "mediaType": block.Image.MediaType,
			"bytes": block.Image.Bytes, "width": block.Image.Width, "height": block.Image.Height,
		}
	}
	if len(block.Blocks) > 0 {
		wire.Content = make([]messageContentWire, 0, len(block.Blocks))
		for _, child := range block.Blocks {
			wire.Content = append(wire.Content, messageContentWireFrom(child))
		}
	}
	return wire
}

func messageContentBlockFrom(wire messageContentWire) (llm.ContentBlock, error) {
	kind := llm.ContentBlockKind(wire.Type)
	switch kind {
	case llm.BlockText, llm.BlockReasoning, llm.BlockImage, llm.BlockToolCall, llm.BlockToolResult:
	default:
		return llm.ContentBlock{}, fmt.Errorf("team: unsupported queued content block %q", wire.Type)
	}
	block := llm.ContentBlock{Kind: kind, Text: wire.Text, CallID: wire.ID, Name: wire.Name, Arguments: wire.Arguments, IsError: wire.IsError}
	if block.CallID == "" {
		block.CallID = wire.ToolCallID
	}
	if kind == llm.BlockImage {
		if attachment, ok := wire.Attachment.(map[string]any); ok {
			block.Image.ID, _ = attachment["attachmentId"].(string)
			block.Image.MediaType, _ = attachment["mediaType"].(string)
			if n, ok := attachment["bytes"].(float64); ok {
				block.Image.Bytes = int64(n)
			}
			if n, ok := attachment["width"].(float64); ok {
				block.Image.Width = int(n)
			}
			if n, ok := attachment["height"].(float64); ok {
				block.Image.Height = int(n)
			}
		}
	}
	if len(wire.Content) > 0 {
		block.Blocks = make([]llm.ContentBlock, 0, len(wire.Content))
		for _, child := range wire.Content {
			converted, err := messageContentBlockFrom(child)
			if err != nil {
				return llm.ContentBlock{}, err
			}
			block.Blocks = append(block.Blocks, converted)
		}
	}
	return block, nil
}

func messageBlocksText(blocks []llm.ContentBlock) string {
	var content strings.Builder
	for _, block := range blocks {
		if block.Kind == llm.BlockText || block.Kind == llm.BlockReasoning {
			content.WriteString(block.Text)
		}
	}
	return content.String()
}

func cloneMessage(message Message) Message {
	message.ContentBlocks = cloneContentBlocks(message.ContentBlocks)
	return message
}

func cloneContentBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]llm.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		out[i].Blocks = cloneContentBlocks(out[i].Blocks)
	}
	return out
}

func messageEventBytes(message Message) []byte {
	encoded, err := json.Marshal(messageEventWireFrom(message))
	if err != nil {
		return nil
	}
	return encoded
}

func (e MessageQueuedEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Version int              `json:"version"`
		TeamID  string           `json:"teamId"`
		Message messageEventWire `json:"message"`
	}{Version: e.Version, TeamID: e.TeamID, Message: messageEventWireFrom(e.Message)})
}

func (e *MessageQueuedEvent) UnmarshalJSON(data []byte) error {
	var wire struct {
		Version int              `json:"version"`
		TeamID  string           `json:"teamId"`
		Message messageEventWire `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Message.Content) == 0 {
		return errors.New("team: queued message content is empty")
	}
	blocks := make([]llm.ContentBlock, 0, len(wire.Message.Content))
	for _, block := range wire.Message.Content {
		converted, err := messageContentBlockFrom(block)
		if err != nil {
			return err
		}
		blocks = append(blocks, converted)
	}
	if len(blocks) == 0 || (messageBlocksText(blocks) == "" && !messageBlocksHaveNonText(blocks)) {
		return errors.New("team: queued message content is empty")
	}
	e.Version, e.TeamID = wire.Version, wire.TeamID
	e.Message = Message{ID: wire.Message.ID, SenderID: wire.Message.SenderID, SenderName: wire.Message.SenderName, TargetID: wire.Message.TargetID, Delivery: wire.Message.Delivery, Content: messageBlocksText(blocks), ContentBlocks: blocks}
	return nil
}

// DecodeMessageContent accepts the legacy string form or the canonical rich
// content array used by team_message. It returns both the legacy text
// projection and a detached block slice for callers that preserve content
// kinds across the Agent boundary.
func DecodeMessageContent(raw json.RawMessage) (string, []llm.ContentBlock, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return "", nil, errors.New("team: message content is required")
		}
		return text, []llm.ContentBlock{llm.Text(text)}, nil
	}
	var wire []messageContentWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return "", nil, errors.New("team: content must be a string or content block array")
	}
	if len(wire) == 0 {
		return "", nil, errors.New("team: message content is required")
	}
	blocks := make([]llm.ContentBlock, 0, len(wire))
	for _, item := range wire {
		block, err := messageContentBlockFrom(item)
		if err != nil {
			return "", nil, err
		}
		blocks = append(blocks, block)
	}
	if messageBlocksText(blocks) == "" && !messageBlocksHaveNonText(blocks) {
		return "", nil, errors.New("team: message content is required")
	}
	return messageBlocksText(blocks), cloneContentBlocks(blocks), nil
}

func messageBlocksHaveNonText(blocks []llm.ContentBlock) bool {
	for _, block := range blocks {
		if block.Kind != llm.BlockText && block.Kind != llm.BlockReasoning {
			return true
		}
	}
	return false
}

type MessageDeliveredEvent struct {
	Version   int    `json:"version"`
	TeamID    string `json:"teamId"`
	MessageID string `json:"messageId"`
	TargetID  string `json:"targetId"`
}

type MemberEvent struct {
	Version int            `json:"version"`
	TeamID  string         `json:"teamId"`
	Member  MemberSnapshot `json:"member"`
}

func (e MemberEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Version int    `json:"version"`
		TeamID  string `json:"teamId"`
		Member  struct {
			ID          string      `json:"id"`
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Provider    string      `json:"provider"`
			Context     string      `json:"context"`
			Phase       MemberPhase `json:"phase"`
			Error       string      `json:"error,omitempty"`
		} `json:"member"`
	}{
		Version: e.Version,
		TeamID:  e.TeamID,
		Member: struct {
			ID          string      `json:"id"`
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Provider    string      `json:"provider"`
			Context     string      `json:"context"`
			Phase       MemberPhase `json:"phase"`
			Error       string      `json:"error,omitempty"`
		}{ID: e.Member.ID, Name: e.Member.Name, Description: e.Member.Description, Provider: e.Member.Provider, Context: e.Member.Context, Phase: e.Member.Phase, Error: e.Member.Error},
	})
}

type Board struct {
	teamID             string
	mu                 sync.Mutex
	next               int
	msgNext            uint64
	tasks              map[string]Task
	messages           map[string]Message
	memberRows         map[string]MemberSnapshot
	notify             chan struct{}
	emit               func(kind string, value any)
	roster             *Roster
	maxTasks           int
	maxPendingMessages int
	maxMessageBytes    int
	dispatch           func(context.Context, Message) (bool, error)
	reserveID          func(namespace, id string) (bool, error)
}

// SetIDReservation installs the durable identity allocator used by this
// board. It is optional for in-memory library callers; the application wires
// it to SQLite so task/message counters and member identities remain unique
// across processes.
func (b *Board) SetIDReservation(reserve func(namespace, id string) (bool, error)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.reserveID = reserve
	b.mu.Unlock()
}

// ReserveMemberID claims a Team member identity before its session, durable
// member event, and live Agent are published. A registry-backed roster owns
// the in-memory member/name state; the Board supplies its durable allocator.
func (b *Board) ReserveMemberID(name string) (string, error) {
	if b == nil {
		return "", errors.New("team: nil board")
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) || len(name) > 64 || name == "lead" {
		return "", errors.New("team: member name must be lower-kebab-case, at most 64 characters, and not lead")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roster != nil {
		id, err := b.roster.reserveMemberIDForBoard(name, b.reserveID)
		if err != nil {
			return "", err
		}
		return string(id), nil
	}
	id := "team:" + strings.TrimPrefix(b.teamID, "team:") + ":" + name
	if b.reserveID != nil {
		claimed, err := b.reserveID("team-member:"+b.teamID, id)
		if err != nil {
			return "", fmt.Errorf("team: reserve member id: %w", err)
		}
		if !claimed {
			return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
		}
	}
	return id, nil
}

// MemberID returns a validated member identity without durable side effects.
// It is used only when the storage backend can atomically reserve and publish
// the member receipt; otherwise callers must use ReserveMemberID.
func (b *Board) MemberID(name string) (string, error) {
	if b == nil {
		return "", errors.New("team: nil board")
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) || len(name) > 64 || name == "lead" {
		return "", errors.New("team: member name must be lower-kebab-case, at most 64 characters, and not lead")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roster != nil {
		id, err := b.roster.MemberID(name)
		if err != nil {
			return "", err
		}
		return string(id), nil
	}
	return "team:" + strings.TrimPrefix(b.teamID, "team:") + ":" + name, nil
}

func New(teamID string, emit func(string, any)) (*Board, error) {
	return NewWithLimits(teamID, emit, 256, 64, 65536)
}

// NewWithLimits creates a board with the reference deployment bounds. Zero
// values select the corresponding defaults for source-compatible callers.
func NewWithLimits(teamID string, emit func(string, any), maxTasks, maxPendingMessages, maxMessageBytes int) (*Board, error) {
	if strings.TrimSpace(teamID) == "" {
		return nil, errors.New("team: team id is required")
	}
	if maxTasks <= 0 {
		maxTasks = 256
	}
	if maxPendingMessages <= 0 {
		maxPendingMessages = 64
	}
	if maxMessageBytes <= 0 {
		maxMessageBytes = 65536
	}
	return &Board{teamID: teamID, tasks: make(map[string]Task), messages: make(map[string]Message), memberRows: make(map[string]MemberSnapshot), notify: make(chan struct{}, 1), emit: emit, maxTasks: maxTasks, maxPendingMessages: maxPendingMessages, maxMessageBytes: maxMessageBytes}, nil
}

// Snapshot returns a deterministic deep copy suitable for persistence.
func (b *Board) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := Snapshot{TeamID: b.teamID, Next: b.next, MsgNext: b.msgNext}
	for _, task := range b.tasks {
		out.Tasks = append(out.Tasks, cloneTask(task))
	}
	for _, message := range b.messages {
		out.Messages = append(out.Messages, cloneMessage(message))
	}
	if b.roster != nil {
		out.Members = b.roster.Snapshot()
	} else {
		for _, member := range b.memberRows {
			out.Members = append(out.Members, member)
		}
		sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].ID < out.Members[j].ID })
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].ID < out.Tasks[j].ID })
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].ID < out.Messages[j].ID })
	return out
}

// TeamID returns the durable root selector for this board.
func (b *Board) TeamID() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.teamID
}

// ApplyEvent folds one append-only Team event after a snapshot checkpoint.
// It is intentionally side-effect free with respect to emit/dispatch: replay
// must rebuild state without publishing duplicate runtime work.
func (b *Board) ApplyEvent(typ string, data []byte) error {
	if b == nil {
		return errors.New("team: nil board")
	}
	switch typ {
	case "team/member":
		var event MemberEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("team: decode member event: %w", err)
		}
		if event.Version != 1 || event.TeamID != b.teamID {
			return errors.New("team: invalid member event selector")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		prior, existed := b.memberRows[event.Member.ID]
		roster := b.roster
		if roster != nil {
			roster.mu.Lock()
			defer roster.mu.Unlock()
			if err := roster.validateMemberEventLocked(event.Member); err != nil {
				return err
			}
		}
		if err := b.validateMemberRowLocked(event.Member); err != nil {
			return err
		}
		b.memberRows[event.Member.ID] = event.Member
		if roster != nil {
			if err := roster.applyMemberEventLocked(event.Member); err != nil {
				// The same locked state was prevalidated above; this is a
				// defensive guard for future changes to the fold rules.
				if existed {
					b.memberRows[event.Member.ID] = prior
				} else {
					delete(b.memberRows, event.Member.ID)
				}
				return err
			}
		}
		return nil
	case "team/task":
		var event TaskEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("team: decode task event: %w", err)
		}
		if event.Version != 1 || event.TeamID != b.teamID || event.Task.ID == "" {
			return fmt.Errorf("team: invalid task event selector")
		}
		if event.Task.Revision <= 0 || event.Task.Subject == "" {
			return fmt.Errorf("team: invalid task event task")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		prior, exists := b.tasks[event.Task.ID]
		if (!exists && event.Task.Revision != 1) || (exists && event.Task.Revision != prior.Revision+1) {
			return fmt.Errorf("team: non-contiguous task revision for %s", event.Task.ID)
		}
		if event.Task.Status != TaskPending && event.Task.Status != TaskInProgress && event.Task.Status != TaskCompleted && event.Task.Status != TaskDeleted {
			return fmt.Errorf("team: invalid task status %q", event.Task.Status)
		}
		if err := b.validateEdgesLocked(event.Task.ID, event.Task.BlockedBy); err != nil {
			return err
		}
		b.tasks[event.Task.ID] = cloneTask(event.Task)
		b.next = maxInt(b.next, taskNumber(event.Task.ID))
		return nil
	case "team/message/queued":
		var event MessageQueuedEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("team: decode queued message event: %w", err)
		}
		if event.Version != 1 || event.TeamID != b.teamID || event.Message.ID == "" || event.Message.TargetID == "" || event.Message.SenderID == "" {
			return errors.New("team: invalid queued message event")
		}
		if event.Message.Delivery != "quiet" && event.Message.Delivery != "wakeup" {
			return errors.New("team: invalid queued message delivery")
		}
		if len(messageEventBytes(event.Message)) > b.maxMessageBytes {
			return fmt.Errorf("%w: queued message exceeds %d bytes", ErrTeamLimit, b.maxMessageBytes)
		}
		if len(event.Message.ContentBlocks) == 0 && event.Message.Content != "" {
			event.Message.ContentBlocks = []llm.ContentBlock{llm.Text(event.Message.Content)}
		}
		if len(event.Message.ContentBlocks) == 0 || (messageBlocksText(event.Message.ContentBlocks) == "" && !messageBlocksHaveNonText(event.Message.ContentBlocks)) {
			return errors.New("team: invalid queued message content")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, exists := b.messages[event.Message.ID]; exists {
			return fmt.Errorf("team: message %s queued twice", event.Message.ID)
		}
		pending := 0
		for _, message := range b.messages {
			if message.TargetID == event.Message.TargetID && !message.Delivered {
				pending++
			}
		}
		if pending >= b.maxPendingMessages {
			return fmt.Errorf("%w: pending messages for %s exceed %d", ErrTeamLimit, event.Message.TargetID, b.maxPendingMessages)
		}
		b.messages[event.Message.ID] = event.Message
		b.msgNext = maxUint(b.msgNext, messageNumber(event.Message.ID))
		return nil
	case "team/message/delivered":
		var event MessageDeliveredEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("team: decode delivered message event: %w", err)
		}
		if event.Version != 1 || event.TeamID != b.teamID || event.MessageID == "" || event.TargetID == "" {
			return errors.New("team: invalid delivered message event")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		message, exists := b.messages[event.MessageID]
		if !exists || message.TargetID != event.TargetID || message.Delivered {
			return fmt.Errorf("team: invalid delivery transition for %s", event.MessageID)
		}
		message.Delivered = true
		b.messages[event.MessageID] = message
		return nil
	default:
		return fmt.Errorf("team: unsupported replay event %q", typ)
	}
}

// AttachRoster binds Agent Registry membership to this Team board. Once
// attached, the roster is included in snapshots and can be used by adapters
// to authorize task/mailbox operations.
func (b *Board) AttachRoster(roster *Roster) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.roster = roster
	if roster != nil && len(b.memberRows) > 0 {
		// A registry-backed roster is authoritative for live handles, while the
		// detached rows remain the cold-inspection projection.
		for id, member := range b.memberRows {
			if _, err := roster.Member(agent.ID(id)); err != nil {
				_ = roster.ApplyMemberEvent(member)
			}
		}
	}
	b.mu.Unlock()
}

// SetMessageDispatcher installs the runtime delivery seam. The board remains
// the durable queue; a dispatcher may report false when the target is offline,
// leaving the message pending for a later recovery pass.
func (b *Board) SetMessageDispatcher(dispatch func(context.Context, Message) (bool, error)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.dispatch = dispatch
	b.mu.Unlock()
}

func (b *Board) messageDispatcher() func(context.Context, Message) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dispatch
}

// Roster returns the optional Agent Registry-backed membership boundary.
func (b *Board) Roster() *Roster {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roster
}

// Restore replaces the board state from a detached snapshot. It is intended
// for startup recovery, so malformed or cross-team state is rejected before
// any live map is changed.
func (b *Board) Restore(snapshot Snapshot) error {
	if snapshot.TeamID != b.teamID || strings.TrimSpace(snapshot.TeamID) == "" {
		return fmt.Errorf("team: snapshot team mismatch")
	}
	tasks := make(map[string]Task, len(snapshot.Tasks))
	maxTask := 0
	for _, task := range snapshot.Tasks {
		if taskNumber(task.ID) <= 0 || task.Revision <= 0 || task.Subject == "" || tasks[task.ID].ID != "" {
			return fmt.Errorf("%w: invalid task snapshot", ErrInvalidTask)
		}
		if task.Status != TaskPending && task.Status != TaskInProgress && task.Status != TaskCompleted && task.Status != TaskDeleted {
			return fmt.Errorf("%w: invalid task status %q", ErrInvalidTask, task.Status)
		}
		if task.Status != TaskDeleted {
			if task.DeletedAt.IsZero() == false {
				return fmt.Errorf("%w: live task has deletedAt", ErrInvalidTask)
			}
		} else if task.DeletedAt.IsZero() {
			return fmt.Errorf("%w: deleted task lacks deletedAt", ErrInvalidTask)
		}
		copy := cloneTask(task)
		tasks[task.ID] = copy
		if n := taskNumber(task.ID); n > maxTask {
			maxTask = n
		}
	}
	for _, task := range tasks {
		for _, blocker := range task.BlockedBy {
			other, ok := tasks[blocker]
			if !ok || other.Status == TaskDeleted {
				return fmt.Errorf("%w: blocker %s", ErrInvalidTask, blocker)
			}
		}
	}
	check := &Board{teamID: b.teamID, tasks: tasks, maxTasks: b.maxTasks, maxPendingMessages: b.maxPendingMessages, maxMessageBytes: b.maxMessageBytes}
	for _, task := range tasks {
		if err := check.validateEdgesLocked(task.ID, task.BlockedBy); err != nil {
			return err
		}
	}
	activeTasks := 0
	for _, task := range tasks {
		if task.Status != TaskDeleted {
			activeTasks++
		}
	}
	if activeTasks > b.maxTasks {
		return fmt.Errorf("%w: snapshot has %d tasks, limit is %d", ErrTeamLimit, activeTasks, b.maxTasks)
	}
	messages := make(map[string]Message, len(snapshot.Messages))
	var maxMessage uint64
	pendingByTarget := make(map[string]int)
	for _, message := range snapshot.Messages {
		if messageNumber(message.ID) <= 0 || message.TargetID == "" || message.SenderID == "" || messages[message.ID].ID != "" {
			return errors.New("team: invalid message snapshot")
		}
		if message.Delivery != "quiet" && message.Delivery != "wakeup" {
			return errors.New("team: invalid message delivery")
		}
		if len([]byte(message.Content)) > b.maxMessageBytes {
			return fmt.Errorf("%w: snapshot message exceeds %d bytes", ErrTeamLimit, b.maxMessageBytes)
		}
		if len(message.ContentBlocks) == 0 && message.Content != "" {
			message.ContentBlocks = []llm.ContentBlock{llm.Text(message.Content)}
		}
		if len(message.ContentBlocks) == 0 || (messageBlocksText(message.ContentBlocks) == "" && !messageBlocksHaveNonText(message.ContentBlocks)) {
			return errors.New("team: invalid message content")
		}
		messages[message.ID] = cloneMessage(message)
		if !message.Delivered {
			pendingByTarget[message.TargetID]++
			if pendingByTarget[message.TargetID] > b.maxPendingMessages {
				return fmt.Errorf("%w: snapshot pending messages for %s exceed %d", ErrTeamLimit, message.TargetID, b.maxPendingMessages)
			}
		}
		if n := messageNumber(message.ID); n > maxMessage {
			maxMessage = n
		}
	}
	if b.roster != nil && len(snapshot.Members) > 0 {
		if err := b.roster.Restore(snapshot.Members); err != nil {
			return err
		}
	}
	members := make(map[string]MemberSnapshot, len(snapshot.Members))
	for _, member := range snapshot.Members {
		if err := validateMemberSnapshot(member); err != nil {
			return err
		}
		if _, exists := members[member.ID]; exists {
			return errors.New("team: duplicate member snapshot")
		}
		members[member.ID] = member
	}
	b.mu.Lock()
	b.tasks = tasks
	b.messages = messages
	b.memberRows = members
	b.next = maxInt(snapshot.Next, maxTask)
	b.msgNext = maxUint(snapshot.MsgNext, maxMessage)
	b.mu.Unlock()
	return nil
}

func (b *Board) applyMemberRowLocked(member MemberSnapshot) error {
	if err := b.validateMemberRowLocked(member); err != nil {
		return err
	}
	if b.memberRows == nil {
		b.memberRows = make(map[string]MemberSnapshot)
	}
	b.memberRows[member.ID] = member
	return nil
}

func (b *Board) validateMemberRowLocked(member MemberSnapshot) error {
	if err := validateMemberSnapshot(member); err != nil {
		return err
	}
	if b.memberRows == nil {
		b.memberRows = make(map[string]MemberSnapshot)
	}
	prior, exists := b.memberRows[member.ID]
	if !exists {
		for _, row := range b.memberRows {
			if row.Name == member.Name && row.ID != member.ID {
				return ErrMemberExists
			}
		}
		if member.Phase != MemberProvisioning {
			return errors.New("team: a new member must begin provisioning")
		}
	} else {
		if prior.Name != member.Name || prior.Description != member.Description || prior.Provider != member.Provider || prior.Context != member.Context {
			return errors.New("team: member event changed immutable identity fields")
		}
		if prior.Phase != MemberProvisioning || member.Phase == MemberProvisioning {
			return fmt.Errorf("team: invalid member transition %s -> %s", prior.Phase, member.Phase)
		}
	}
	return nil
}

func validateMemberSnapshot(member MemberSnapshot) error {
	if strings.TrimSpace(member.ID) == "" || strings.TrimSpace(member.Name) == "" ||
		(member.Phase != MemberProvisioning && member.Phase != MemberActive && member.Phase != MemberFailed) {
		return errors.New("team: invalid member snapshot")
	}
	if member.Name != "lead" && (!memberNamePattern.MatchString(member.Name) || len(member.Name) > 64) {
		return errors.New("team: invalid roster member name")
	}
	return nil
}

func (b *Board) CreateTask(subject, description, ownerID string, blockedBy, writeScopes []string) (TaskView, error) {
	if strings.TrimSpace(subject) == "" {
		return TaskView{}, fmt.Errorf("%w: subject is required", ErrInvalidTask)
	}
	if err := validateWriteScopes(writeScopes); err != nil {
		return TaskView{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	active := 0
	for _, existing := range b.tasks {
		if existing.Status != TaskDeleted {
			active++
		}
	}
	if active >= b.maxTasks {
		return TaskView{}, fmt.Errorf("%w: task limit %d", ErrTeamLimit, b.maxTasks)
	}
	if err := b.validateEdgesLocked("", blockedBy); err != nil {
		return TaskView{}, err
	}
	var taskID string
	for attempt := 0; attempt < 32; attempt++ {
		b.next++
		candidate := fmt.Sprintf("task-%d", b.next)
		if b.reserveID == nil {
			taskID = candidate
			break
		}
		claimed, err := b.reserveID("team-task:"+b.teamID, candidate)
		if err != nil {
			return TaskView{}, fmt.Errorf("team: reserve task id: %w", err)
		}
		if claimed {
			taskID = candidate
			break
		}
	}
	if taskID == "" {
		return TaskView{}, errors.New("team: unable to reserve a unique task id")
	}
	task := Task{ID: taskID, Revision: 1, Subject: subject, Description: description, Status: TaskPending, OwnerID: ownerID, BlockedBy: cleanIDs(blockedBy), WriteScopes: normalizeScopes(writeScopes)}
	b.tasks[task.ID] = task
	b.emitLocked("task/create", task)
	b.signalLocked()
	return b.viewLocked(task), nil
}

type Update struct {
	ID          string
	Revision    int
	Action      string // claim | complete | release | delete
	OwnerID     string
	ActorID     string
	Subject     string
	Description string
	BlockedBy   []string
	WriteScopes []string
}

func (b *Board) UpdateTask(update Update) (TaskView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.tasks[update.ID]
	if !ok {
		return TaskView{}, fmt.Errorf("%w: %s", ErrTaskNotFound, update.ID)
	}
	if update.Revision != task.Revision {
		return TaskView{}, fmt.Errorf("%w: %s expected %d got %d", ErrRevision, update.ID, task.Revision, update.Revision)
	}
	if task.Status == TaskDeleted {
		return TaskView{}, fmt.Errorf("%w: deleted task", ErrInvalidTask)
	}
	switch update.Action {
	case "claim":
		if task.Status != TaskPending || task.OwnerID != "" {
			return TaskView{}, fmt.Errorf("%w: %s", ErrTaskBlocked, task.ID)
		}
		if !b.viewLocked(task).Ready {
			return TaskView{}, fmt.Errorf("%w: %s", ErrTaskBlocked, task.ID)
		}
		if strings.TrimSpace(update.OwnerID) == "" {
			return TaskView{}, fmt.Errorf("%w: claim owner is required", ErrInvalidTask)
		}
		task.Status, task.OwnerID = TaskInProgress, update.OwnerID
	case "complete":
		if task.Status != TaskInProgress {
			return TaskView{}, fmt.Errorf("%w: complete requires in-progress task", ErrInvalidTask)
		}
		task.Status = TaskCompleted
	case "release":
		if task.Status != TaskInProgress {
			return TaskView{}, fmt.Errorf("%w: release requires in-progress task", ErrInvalidTask)
		}
		task.Status, task.OwnerID = TaskPending, ""
	case "edit":
		if update.Subject == "" && update.Description == "" && update.WriteScopes == nil {
			return TaskView{}, fmt.Errorf("%w: edit requires subject, description, or write scopes", ErrInvalidTask)
		}
		if update.Subject != "" {
			task.Subject = strings.TrimSpace(update.Subject)
			if task.Subject == "" {
				return TaskView{}, fmt.Errorf("%w: subject is required", ErrInvalidTask)
			}
		}
		if update.Description != "" {
			task.Description = strings.TrimSpace(update.Description)
		}
		if update.WriteScopes != nil {
			if err := validateWriteScopes(update.WriteScopes); err != nil {
				return TaskView{}, err
			}
			task.WriteScopes = normalizeScopes(update.WriteScopes)
		}
	case "set_dependencies":
		if update.BlockedBy == nil {
			return TaskView{}, fmt.Errorf("%w: blocked_by is required", ErrInvalidTask)
		}
		if err := b.validateEdgesLocked(task.ID, update.BlockedBy); err != nil {
			return TaskView{}, err
		}
		task.BlockedBy = cleanIDs(update.BlockedBy)
	case "reopen":
		if task.Status != TaskCompleted {
			return TaskView{}, fmt.Errorf("%w: reopen requires completed task", ErrInvalidTask)
		}
		task.Status, task.OwnerID = TaskPending, ""
	case "reassign":
		if b.roster == nil || !b.roster.IsLead(agent.ID(update.ActorID)) {
			return TaskView{}, ErrUnauthorized
		}
		if task.Status != TaskPending && task.Status != TaskInProgress {
			return TaskView{}, fmt.Errorf("%w: reassign requires pending or in-progress task", ErrInvalidTask)
		}
		if strings.TrimSpace(update.OwnerID) == "" {
			task.Status, task.OwnerID = TaskPending, ""
			break
		}
		if !b.viewLocked(task).Ready {
			return TaskView{}, fmt.Errorf("%w: %s", ErrTaskBlocked, task.ID)
		}
		if err := b.roster.Authorize(agent.ID(update.OwnerID), ""); err != nil {
			return TaskView{}, err
		}
		task.Status, task.OwnerID = TaskInProgress, strings.TrimSpace(update.OwnerID)
	case "delete":
		for _, other := range b.tasks {
			if other.Status != TaskDeleted && other.ID != task.ID && containsID(other.BlockedBy, task.ID) {
				return TaskView{}, fmt.Errorf("%w: task %s still depends on %s", ErrInvalidTask, other.ID, task.ID)
			}
		}
		task.Status, task.DeletedAt = TaskDeleted, time.Now().UTC()
	default:
		return TaskView{}, fmt.Errorf("%w: action %q", ErrInvalidTask, update.Action)
	}
	if update.Subject != "" {
		task.Subject = update.Subject
	}
	if update.Description != "" {
		task.Description = update.Description
	}
	if update.BlockedBy != nil && update.Action != "set_dependencies" {
		if err := b.validateEdgesLocked(task.ID, update.BlockedBy); err != nil {
			return TaskView{}, err
		}
		task.BlockedBy = cleanIDs(update.BlockedBy)
	}
	if update.WriteScopes != nil && update.Action != "edit" {
		if err := validateWriteScopes(update.WriteScopes); err != nil {
			return TaskView{}, err
		}
		task.WriteScopes = normalizeScopes(update.WriteScopes)
	}
	task.Revision++
	b.tasks[task.ID] = task
	b.emitLocked("task/update", task)
	b.signalLocked()
	return b.viewLocked(task), nil
}

func (b *Board) GetTask(id string) (TaskView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.tasks[id]
	if !ok {
		return TaskView{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return b.viewLocked(task), nil
}

func (b *Board) ListTasks() []TaskView {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.tasks))
	for id, task := range b.tasks {
		if task.Status != TaskDeleted {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return taskNumber(ids[i]) < taskNumber(ids[j]) })
	out := make([]TaskView, 0, len(ids))
	for _, id := range ids {
		out = append(out, b.viewLocked(b.tasks[id]))
	}
	return out
}

func (b *Board) SendMessage(senderID, senderName, targetID, content, delivery string) (Message, error) {
	return b.SendMessageWithContent(senderID, senderName, targetID, []llm.ContentBlock{llm.Text(content)}, delivery)
}

// SendMessageWithContent queues a durable Team message without collapsing
// reasoning, images, tool calls, or nested tool results into plain text.
func (b *Board) SendMessageWithContent(senderID, senderName, targetID string, blocks []llm.ContentBlock, delivery string) (Message, error) {
	if strings.TrimSpace(senderID) == "" || strings.TrimSpace(targetID) == "" {
		return Message{}, errors.New("team: sender, target and content are required")
	}
	if delivery != "quiet" && delivery != "wakeup" {
		return Message{}, errors.New("team: delivery must be quiet or wakeup")
	}
	blocks = cloneContentBlocks(blocks)
	if len(blocks) == 0 || (messageBlocksText(blocks) == "" && !messageBlocksHaveNonText(blocks)) {
		return Message{}, errors.New("team: sender, target and content are required")
	}
	content := messageBlocksText(blocks)
	probe := Message{SenderID: senderID, SenderName: senderName, TargetID: targetID, Content: content, ContentBlocks: blocks, Delivery: delivery}
	if len(messageEventBytes(probe)) > b.maxMessageBytes {
		return Message{}, fmt.Errorf("%w: message exceeds %d bytes", ErrTeamLimit, b.maxMessageBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := 0
	for _, existing := range b.messages {
		if existing.TargetID == targetID && !existing.Delivered {
			pending++
		}
	}
	if pending >= b.maxPendingMessages {
		return Message{}, fmt.Errorf("%w: pending messages for %s exceed %d", ErrTeamLimit, targetID, b.maxPendingMessages)
	}
	var messageID string
	for attempt := 0; attempt < 32; attempt++ {
		b.msgNext++
		candidate := fmt.Sprintf("msg-%d", b.msgNext)
		if b.reserveID == nil {
			messageID = candidate
			break
		}
		claimed, err := b.reserveID("team-message:"+b.teamID, candidate)
		if err != nil {
			return Message{}, fmt.Errorf("team: reserve message id: %w", err)
		}
		if claimed {
			messageID = candidate
			break
		}
	}
	if messageID == "" {
		return Message{}, errors.New("team: unable to reserve a unique message id")
	}
	m := Message{ID: messageID, SenderID: senderID, SenderName: senderName, TargetID: targetID, Content: content, ContentBlocks: blocks, Delivery: delivery, CreatedAt: time.Now().UTC()}
	b.messages[m.ID] = m
	b.emitLocked("message/send", m)
	b.signalLocked()
	return m, nil
}

func (b *Board) PendingMessages(targetID string) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Message, 0)
	for _, message := range b.messages {
		if message.TargetID == targetID && !message.Delivered {
			out = append(out, cloneMessage(message))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (b *Board) AckMessage(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	message, ok := b.messages[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageAbsent, id)
	}
	if !message.Delivered {
		message.Delivered = true
		b.messages[id] = message
		b.emitLocked("message/deliver", message)
		b.signalLocked()
	}
	return nil
}

func (b *Board) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.notify:
		return nil
	}
}

func (b *Board) viewLocked(task Task) TaskView {
	ready := task.Status == TaskPending
	for _, blockerID := range task.BlockedBy {
		blocker, ok := b.tasks[blockerID]
		if !ok || blocker.Status != TaskCompleted {
			ready = false
			break
		}
	}
	warning := false
	for _, other := range b.tasks {
		if other.ID == task.ID || other.Status == TaskDeleted {
			continue
		}
		for _, a := range task.WriteScopes {
			for _, c := range other.WriteScopes {
				if scopesOverlap(a, c) {
					warning = true
				}
			}
		}
	}
	return TaskView{Task: cloneTask(task), Ready: ready, WriteScopeWarning: warning}
}

func (b *Board) validateEdgesLocked(id string, blockedBy []string) error {
	seen := map[string]bool{}
	for _, blockerID := range blockedBy {
		if blockerID == id || seen[blockerID] {
			return fmt.Errorf("%w: %s", ErrCycle, blockerID)
		}
		seen[blockerID] = true
		blocker, ok := b.tasks[blockerID]
		if !ok || blocker.Status == TaskDeleted {
			return fmt.Errorf("%w: blocker %s", ErrInvalidTask, blockerID)
		}
		if b.reachesLocked(blockerID, id, map[string]bool{}) {
			return fmt.Errorf("%w: %s -> %s", ErrCycle, id, blockerID)
		}
	}
	return nil
}

func (b *Board) reachesLocked(from, target string, seen map[string]bool) bool {
	if from == target {
		return true
	}
	if seen[from] {
		return false
	}
	seen[from] = true
	for _, dependency := range b.tasks[from].BlockedBy {
		if b.reachesLocked(dependency, target, seen) {
			return true
		}
	}
	return false
}

func (b *Board) emitLocked(kind string, value any) {
	if b.emit != nil {
		b.emit(kind, value)
	}
}

func (b *Board) signalLocked() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

func cleanIDs(ids []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func normalizeScopes(scopes []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = filepath.Clean(strings.TrimSpace(scope))
		if scope == "." || scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

func validateWriteScopes(scopes []string) error {
	for _, raw := range scopes {
		scope := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
		scope = strings.TrimSuffix(scope, "/")
		if scope == "" || strings.HasPrefix(scope, "/") || (len(scope) >= 2 && scope[1] == ':') {
			return fmt.Errorf("%w: invalid workspace-relative write scope %q", ErrInvalidTask, raw)
		}
		for _, segment := range strings.Split(scope, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return fmt.Errorf("%w: invalid workspace-relative write scope %q", ErrInvalidTask, raw)
			}
		}
	}
	return nil
}

func containsID(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

func scopesOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
func taskNumber(id string) int       { var n int; _, _ = fmt.Sscanf(id, "task-%d", &n); return n }
func messageNumber(id string) uint64 { var n uint64; _, _ = fmt.Sscanf(id, "msg-%d", &n); return n }
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func maxUint(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
func cloneTask(t Task) Task {
	t.BlockedBy = append([]string(nil), t.BlockedBy...)
	t.WriteScopes = append([]string(nil), t.WriteScopes...)
	return t
}
