package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

var ErrInboxClosed = errors.New("agent inbox is closed")

// InboxEvent is the durable state transition for one inbox splice. A journal
// must commit it before the live queue is changed. Replaying the same events
// reconstructs pending work after a process restart without re-running the
// Agent.
type InboxEvent struct {
	Target       string
	Start        int
	RemovedCount int
	Inserted     []Message
	Outcome      string
	Turn         int
}

// InboxJournal is deliberately smaller than session.Log: the Agent package
// stays independent of persistence while the composition root can project the
// event into its canonical session event stream.
type InboxJournal interface {
	AppendInboxEvent(InboxEvent) error
}

// MessageKind identifies how a message enters the next turn.
type MessageKind string

const (
	MessageNextStep  MessageKind = "next-step"
	MessageNextTurn  MessageKind = "next-turn"
	MessageSteering  MessageKind = "steering"
	MessageInjection MessageKind = "quiet-injection"
)

// Message is the model-visible input claimed by a turn.  IDs are assigned by
// the inbox and remain stable for auditing and deduplication.
type Message struct {
	ID       string             `json:"id"`
	Text     string             `json:"text,omitempty"`
	Content  []llm.ContentBlock `json:"content,omitempty"`
	Kind     MessageKind        `json:"kind"`
	Metadata map[string]string  `json:"metadata,omitempty"`
}

// TurnInput is one claimed turn boundary.  All next-step messages are claimed
// first, followed by one waking next-turn message and any quiet injections.
type TurnInput struct {
	Messages []Message
}

// Inbox owns the two DSH-style queues.  Quiet injections never wake an idle
// Agent; they are claimed when a later waking message arrives.
type Inbox struct {
	mu       sync.Mutex
	nextStep []Message
	nextTurn []Message
	quiet    []Message
	nextID   uint64
	closed   bool
	wake     chan struct{}
	journal  InboxJournal
}

// NewInbox creates an empty inbox.
func NewInbox() *Inbox { return &Inbox{wake: make(chan struct{}, 1)} }

// NewDurableInbox creates an inbox whose mutations are committed through the
// supplied journal before becoming visible to the live driver.
func NewDurableInbox(journal InboxJournal, events []InboxEvent) (*Inbox, error) {
	i := &Inbox{wake: make(chan struct{}, 1), journal: journal}
	for _, event := range events {
		if err := i.apply(event); err != nil {
			return nil, err
		}
	}
	return i, nil
}

func (i *Inbox) put(kind MessageKind, text string, metadata map[string]string, wakes bool) (Message, error) {
	return i.putContent(kind, text, nil, metadata, wakes)
}

func (i *Inbox) putContent(kind MessageKind, text string, content []llm.ContentBlock, metadata map[string]string, wakes bool) (Message, error) {
	if i == nil {
		return Message{}, ErrInboxClosed
	}
	if text == "" && len(content) == 0 {
		return Message{}, errors.New("agent inbox message is required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return Message{}, ErrInboxClosed
	}
	if dedupeKey := messageDedupeKey(metadata); dedupeKey != "" {
		for _, queue := range [][]Message{i.nextStep, i.nextTurn, i.quiet} {
			for _, existing := range queue {
				if messageDedupeKey(existing.Metadata) == dedupeKey {
					return existing, nil
				}
			}
		}
	}
	i.nextID++
	m := Message{ID: formatMessageID(i.nextID), Text: text, Content: cloneContent(content), Kind: kind, Metadata: cloneMetadata(metadata)}
	target := inboxTarget(kind)
	queue := i.queue(target)
	if err := i.commit(InboxEvent{Target: target, Start: len(queue), Inserted: []Message{m}}); err != nil {
		return Message{}, err
	}
	switch kind {
	case MessageNextStep:
		i.nextStep = append(i.nextStep, m)
	case MessageSteering:
		i.nextStep = append(i.nextStep, m)
	case MessageNextTurn:
		i.nextTurn = append(i.nextTurn, m)
	case MessageInjection:
		i.quiet = append(i.quiet, m)
	default:
		return Message{}, errors.New("agent inbox message kind is invalid")
	}
	if wakes {
		select {
		case i.wake <- struct{}{}:
		default:
		}
	}
	return m, nil
}

func teamMessageKey(metadata map[string]string) string {
	messageID := metadata["team_message_id"]
	if messageID == "" {
		return ""
	}
	return metadata["team_id"] + "\x00" + messageID
}

func messageDedupeKey(metadata map[string]string) string {
	if key := teamMessageKey(metadata); key != "" {
		return "team:" + key
	}
	if key := strings.TrimSpace(metadata["dedupe_key"]); key != "" {
		return key
	}
	return ""
}

func cloneContent(in []llm.ContentBlock) []llm.ContentBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]llm.ContentBlock, len(in))
	copy(out, in)
	for n := range out {
		out[n].Blocks = cloneContent(out[n].Blocks)
	}
	return out
}

func formatMessageID(n uint64) string {
	// Avoid fmt in the hot path and keep the ID opaque to consumers.
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	return "inbox-" + string(buf[pos:])
}

// Send queues a message for the current/next step and wakes the Agent.
func (i *Inbox) Send(text string, metadata map[string]string) (Message, error) {
	return i.put(MessageNextStep, text, metadata, true)
}

// SendContent queues rich model-visible next-step input and wakes the Agent.
func (i *Inbox) SendContent(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	return i.putContent(MessageNextStep, contentText(content), content, metadata, true)
}

// Steer queues an explicit interruption for the nearest step. Keeping this
// kind distinct makes the durable/event bridge able to preserve user intent
// instead of collapsing steering into an ordinary prompt.
func (i *Inbox) Steer(text string, metadata map[string]string) (Message, error) {
	return i.put(MessageSteering, text, metadata, true)
}

// SteerContent queues an explicit rich-content interruption for the nearest
// step. It preserves image/resource blocks instead of reducing a steering
// message to its text projection.
func (i *Inbox) SteerContent(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	return i.putContent(MessageSteering, contentText(content), content, metadata, true)
}

// Followup queues one next-turn message and wakes the Agent if idle.
func (i *Inbox) Followup(text string, metadata map[string]string) (Message, error) {
	return i.put(MessageNextTurn, text, metadata, true)
}

// FollowupContent queues one rich next-turn message and wakes the Agent.
func (i *Inbox) FollowupContent(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	return i.putContent(MessageNextTurn, contentText(content), content, metadata, true)
}

// Inject queues context without waking an idle Agent.
func (i *Inbox) Inject(text string, metadata map[string]string) (Message, error) {
	return i.put(MessageInjection, text, metadata, false)
}

// InjectContent queues rich quiet context without waking an idle Agent.
func (i *Inbox) InjectContent(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	return i.putContent(MessageInjection, contentText(content), content, metadata, false)
}

// Claim claims one turn.  A turn may contain all messages queued for the
// current step, one waking next-turn message, and all quiet context released by
// that wake.  It returns false when no waking work exists.
func (i *Inbox) Claim() (TurnInput, bool) {
	input, ok, _ := i.ClaimWithError(0)
	return input, ok
}

// ClaimWithError is Claim with the durable commit error exposed to the
// driver. The live queue is untouched when claiming cannot be persisted.
func (i *Inbox) ClaimWithError(turn int) (TurnInput, bool, error) {
	if i == nil {
		return TurnInput{}, false, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.nextStep) == 0 && len(i.nextTurn) == 0 {
		return TurnInput{}, false, nil
	}
	messages := make([]Message, 0, len(i.nextStep)+len(i.quiet)+1)
	nextStepCount := len(i.nextStep) + len(i.quiet)
	messages = append(messages, i.nextStep...)
	if len(i.nextTurn) > 0 {
		messages = append(messages, i.nextTurn[0])
	}
	messages = append(messages, i.quiet...)
	if nextStepCount > 0 {
		if err := i.commit(InboxEvent{Target: "next-step", Start: 0, RemovedCount: nextStepCount, Turn: turn}); err != nil {
			return TurnInput{}, false, err
		}
	}
	if len(i.nextTurn) > 0 {
		if err := i.commit(InboxEvent{Target: "next-turn", Start: 0, RemovedCount: 1, Turn: turn}); err != nil {
			return TurnInput{}, false, err
		}
	}
	i.nextStep = nil
	if len(i.nextTurn) > 0 {
		i.nextTurn = i.nextTurn[1:]
	}
	i.quiet = nil
	return TurnInput{Messages: messages}, true, nil
}

// ClaimStep claims steering and next-step work without consuming an ordinary
// next-turn prompt. This is the boundary an active Agent runner uses after a
// model/tool step has settled.
func (i *Inbox) ClaimStep() (TurnInput, bool) {
	input, ok, _ := i.ClaimStepWithError(0)
	return input, ok
}

// ClaimStepWithError claims only next-step/quiet work and persists the
// deletion before mutating the live projection.
func (i *Inbox) ClaimStepWithError(turn int) (TurnInput, bool, error) {
	if i == nil {
		return TurnInput{}, false, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.nextStep) == 0 && len(i.quiet) == 0 {
		return TurnInput{}, false, nil
	}
	messages := make([]Message, 0, len(i.nextStep)+len(i.quiet))
	messages = append(messages, i.nextStep...)
	if len(i.nextStep)+len(i.quiet) > 0 {
		if err := i.commit(InboxEvent{Target: "next-step", Start: 0, RemovedCount: len(i.nextStep) + len(i.quiet), Turn: turn}); err != nil {
			return TurnInput{}, false, err
		}
	}
	i.nextStep = nil
	messages = append(messages, i.quiet...)
	i.quiet = nil
	return TurnInput{Messages: messages}, true, nil
}

// HasWork reports whether a waking message is waiting. Quiet injections alone
// deliberately do not count as work.
func (i *Inbox) HasWork() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.nextStep) > 0 || len(i.nextTurn) > 0
}

// HasStepWork reports work that an active turn may consume without starting a
// second ordinary turn.
func (i *Inbox) HasStepWork() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.nextStep) > 0 || len(i.quiet) > 0
}

// PendingMessages returns a stable snapshot of work that has been durably
// enqueued but not yet claimed. It is intentionally an observation API: the
// caller cannot mutate the live queues through the returned messages. Hosts
// use it during cold-recovery to distinguish a completion wake that was never
// enqueued from one that is already pending.
func (i *Inbox) PendingMessages() []Message {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Message, 0, len(i.nextStep)+len(i.nextTurn)+len(i.quiet))
	out = append(out, i.nextStep...)
	out = append(out, i.nextTurn...)
	out = append(out, i.quiet...)
	return cloneMessages(out)
}

// Remove removes a queued message by id. It is used to abandon a synchronous
// Run submission whose caller context was canceled before claim.
func (i *Inbox) Remove(id string) bool {
	ok, _ := i.RemoveWithError(id)
	return ok
}

// RemoveWithError durably removes one queued message before applying the
// deletion. It is used when a caller abandons a synchronous Run submission.
func (i *Inbox) RemoveWithError(id string) (bool, error) {
	if i == nil || id == "" {
		return false, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, entry := range []struct {
		target string
		queue  *[]Message
	}{{"next-step", &i.nextStep}, {"next-turn", &i.nextTurn}} {
		target, queue := entry.target, entry.queue
		for n, message := range *queue {
			if message.ID != id {
				continue
			}
			if err := i.commit(InboxEvent{Target: target, Start: n, RemovedCount: 1, Outcome: "canceled"}); err != nil {
				return false, err
			}
			*queue = append((*queue)[:n], (*queue)[n+1:]...)
			return true, nil
		}
	}
	for n, message := range i.quiet {
		if message.ID != id {
			continue
		}
		if err := i.commit(InboxEvent{Target: "next-step", Start: len(i.nextStep) + n, RemovedCount: 1, Outcome: "canceled"}); err != nil {
			return false, err
		}
		i.quiet = append(i.quiet[:n], i.quiet[n+1:]...)
		return true, nil
	}
	return false, nil
}

// Clear discards all queued work without closing the inbox.
func (i *Inbox) Clear() {
	_ = i.ClearWithError()
}

// ClearWithError durably discards all queued work.
func (i *Inbox) ClearWithError() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.nextStep)+len(i.quiet) > 0 {
		if err := i.commit(InboxEvent{Target: "next-step", Start: 0, RemovedCount: len(i.nextStep) + len(i.quiet), Outcome: "canceled"}); err != nil {
			return err
		}
	}
	if len(i.nextTurn) > 0 {
		if err := i.commit(InboxEvent{Target: "next-turn", Start: 0, RemovedCount: len(i.nextTurn), Outcome: "canceled"}); err != nil {
			return err
		}
	}
	i.nextStep = nil
	i.nextTurn = nil
	i.quiet = nil
	return nil
}

func inboxTarget(kind MessageKind) string {
	if kind == MessageNextTurn {
		return "next-turn"
	}
	return "next-step"
}

func (i *Inbox) queue(target string) []Message {
	if target == "next-turn" {
		return i.nextTurn
	}
	return append(append([]Message(nil), i.nextStep...), i.quiet...)
}

func (i *Inbox) commit(event InboxEvent) error {
	if i.journal == nil {
		return nil
	}
	return i.journal.AppendInboxEvent(event)
}

func (i *Inbox) apply(event InboxEvent) error {
	if event.Target != "next-step" && event.Target != "next-turn" {
		return fmt.Errorf("agent inbox: invalid durable target %q", event.Target)
	}
	if event.Start < 0 || event.RemovedCount < 0 {
		return errors.New("agent inbox: invalid durable splice")
	}
	for _, message := range event.Inserted {
		if message.ID == "" || (message.Text == "" && len(message.Content) == 0) {
			return errors.New("agent inbox: invalid durable message")
		}
	}
	if event.Target == "next-turn" {
		if event.Start > len(i.nextTurn) || event.Start+event.RemovedCount > len(i.nextTurn) {
			return errors.New("agent inbox: durable next-turn splice is out of bounds")
		}
		i.nextTurn = append(i.nextTurn[:event.Start], append(cloneMessages(event.Inserted), i.nextTurn[event.Start+event.RemovedCount:]...)...)
	} else {
		// next-step and quiet are one durable target. Replayed inserted work is
		// ordinary next-step; quiet-vs-waking is retained by Message.Kind.
		all := append(append([]Message(nil), i.nextStep...), i.quiet...)
		if event.Start > len(all) || event.Start+event.RemovedCount > len(all) {
			return errors.New("agent inbox: durable next-step splice is out of bounds")
		}
		all = append(all[:event.Start], append(cloneMessages(event.Inserted), all[event.Start+event.RemovedCount:]...)...)
		i.nextStep, i.quiet = nil, nil
		for _, message := range all {
			if message.Kind == MessageInjection {
				i.quiet = append(i.quiet, message)
			} else {
				i.nextStep = append(i.nextStep, message)
			}
		}
	}
	seen := make(map[string]struct{})
	for _, message := range append(append([]Message(nil), i.nextStep...), append(append([]Message(nil), i.quiet...), i.nextTurn...)...) {
		if _, exists := seen[message.ID]; exists {
			return fmt.Errorf("agent inbox: duplicate durable message %q", message.ID)
		}
		seen[message.ID] = struct{}{}
		if strings.HasPrefix(message.ID, "inbox-") {
			if number, err := strconv.ParseUint(strings.TrimPrefix(message.ID, "inbox-"), 10, 64); err == nil && number > i.nextID {
				i.nextID = number
			}
		}
	}
	return nil
}

func cloneMessages(in []Message) []Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]Message, len(in))
	for n, message := range in {
		out[n] = Message{ID: message.ID, Text: message.Text, Content: cloneContent(message.Content), Kind: message.Kind, Metadata: cloneMetadata(message.Metadata)}
	}
	return out
}

// Wait blocks until a waking message is available or the context is canceled.
func (i *Inbox) Wait(ctx context.Context) error {
	if i == nil {
		return ErrInboxClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-i.wake:
		return nil
	}
}

// Close prevents new messages and wakes a blocked waiter.
func (i *Inbox) Close() {
	if i == nil {
		return
	}
	i.mu.Lock()
	if !i.closed {
		i.closed = true
		select {
		case i.wake <- struct{}{}:
		default:
		}
	}
	i.mu.Unlock()
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func contentText(content []llm.ContentBlock) string {
	var text string
	for _, block := range content {
		if block.Kind == llm.BlockText {
			text += block.Text
		}
	}
	return text
}
