package extensionhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

type queuedEvent struct {
	event extension.Event
	done  chan struct{}
}

func (h *Host) startEventDelivery(item *managedExtension) {
	if h == nil || item == nil || len(item.subscriptions) == 0 || item.eventQueue != nil {
		return
	}
	item.eventQueue = make(chan queuedEvent, h.config.EventQueueSize)
	item.eventDone = make(chan struct{})
	go h.dispatchEvents(item)
}

func (h *Host) stopEventDelivery(item *managedExtension) {
	if item == nil || item.eventDone == nil {
		return
	}
	item.eventStop.Do(func() { close(item.eventDone) })
}

func (h *Host) dispatchEvents(item *managedExtension) {
	for {
		select {
		case <-item.eventDone:
			return
		case delivery, ok := <-item.eventQueue:
			if !ok {
				return
			}
			h.deliverEvent(item, delivery.event)
			if delivery.done != nil {
				close(delivery.done)
			}
		}
	}
}

func (h *Host) deliverEvent(item *managedExtension, event extension.Event) {
	started := time.Now()
	callCtx, cancel := context.WithTimeout(context.Background(), h.config.EventTimeout)
	defer cancel()
	var result struct{}
	err := item.connection.Call(callCtx, extension.MethodEvent, event, &result)
	observed := Event{
		ExtensionID: item.manifest.ID, Capability: "events", Method: extension.MethodEvent,
		DurationMS: time.Since(started).Milliseconds(), Success: err == nil, EventType: event.Type,
		Delivered: err == nil, QueueDepth: len(item.eventQueue), At: started.UTC(),
	}
	if err != nil {
		observed.Error = err.Error()
		observed.Timeout = err == context.DeadlineExceeded
	}
	h.observe(observed)
}

func subscribed(item *managedExtension, eventType string) bool {
	_, ok := item.subscriptions[eventType]
	return ok
}

// PublishEvent fans one stable observational event to allowed subscribers.
// The call never blocks the publisher: a full per-extension queue is counted
// and dropped, preserving both agent liveness and at-most-once semantics.
func (h *Host) PublishEvent(event extension.Event) {
	if h == nil || event.Type == "" {
		return
	}
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	items := append([]*managedExtension(nil), h.items...)
	h.mu.RUnlock()
	if event.Version == 0 {
		event.Version = extension.EventVersion
	}
	if event.OccurredAt == "" {
		event.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	for _, item := range items {
		if !subscribed(item, event.Type) || item.eventQueue == nil || item.eventDone == nil {
			continue
		}
		eventCopy := event
		eventCopy.EventID = newEventID()
		delivered := h.enqueueEvent(item, queuedEvent{event: eventCopy})
		h.observe(Event{
			ExtensionID: item.manifest.ID, Capability: "events", Method: extension.MethodEvent,
			Success: delivered, Delivered: delivered, Queued: delivered, Dropped: !delivered, EventType: event.Type,
			QueueDepth: len(item.eventQueue), At: time.Now().UTC(),
		})
	}
}

// PublishSessionStarted is the composition-root seam for durable session
// creation. It uses the same stable, payload-free envelope as projected turns.
func (h *Host) PublishSessionStarted(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.PublishEvent(newExtensionEvent(extension.EventSessionStarted, sessionID, "", "", 0, nil))
}

var nextEventID atomic.Uint64

func newEventID() string {
	return fmt.Sprintf("evt-%016x", nextEventID.Add(1))
}

func (h *Host) publishLifecycle(item *managedExtension, eventType string, payload map[string]any) {
	if !subscribed(item, eventType) {
		return
	}
	// Terminal events can be published while Host.Close has already marked the
	// host closed. Queue directly and wait briefly so shutdown observations are
	// not swallowed; ordinary PublishEvent remains closed to new traffic.
	delivery := queuedEvent{event: newExtensionEvent(eventType, item.manifest.ID, "", "", 0, payload), done: make(chan struct{})}
	if item.eventQueue == nil || item.eventDone == nil {
		return
	}
	if !h.enqueueEvent(item, delivery) {
		h.observe(Event{
			ExtensionID: item.manifest.ID, Capability: "events", Method: extension.MethodEvent,
			Success: false, Delivered: false, Dropped: true, EventType: delivery.event.Type,
			QueueDepth: len(item.eventQueue), At: time.Now().UTC(),
		})
		return
	}
	timeout := timer(h.config.EventTimeout + 100*time.Millisecond)
	defer timeout.Stop()
	select {
	case <-delivery.done:
	case <-timeout.C:
	}
}

func (h *Host) enqueueEvent(item *managedExtension, delivery queuedEvent) bool {
	select {
	case item.eventQueue <- delivery:
		return true
	default:
		return false
	}
}

func timer(timeout time.Duration) *time.Timer {
	return time.NewTimer(timeout)
}

func newExtensionEvent(eventType, sessionID, turnID, stepID string, step int, payload map[string]any) extension.Event {
	return extension.Event{
		Type: eventType, Version: extension.EventVersion, EventID: newEventID(),
		SessionID: sessionID, TurnID: turnID, StepID: stepID, Step: step,
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: payload,
	}
}

// PublishSessionEvent projects the durable vocabulary into the smaller,
// stable extension vocabulary. It is observational and intentionally excludes
// user text, tool arguments, tool output, model text and provider metadata.
func (h *Host) PublishSessionEvent(sessionID string, event session.Event) {
	if h == nil {
		return
	}
	var data map[string]any
	_ = json.Unmarshal(event.Data, &data)
	turn, step := eventNumbers(data)
	turnID, stepID := eventIDs(turn, step)
	switch event.Type {
	case session.EventTurnStart:
		h.PublishEvent(newExtensionEvent(extension.EventTurnStarted, sessionID, turnID, "", 0, nil))
	case session.EventTurnEnd:
		h.PublishEvent(newExtensionEvent(extension.EventTurnCompleted, sessionID, turnID, "", 0, map[string]any{
			"status": stringValue(data, "status"),
		}))
	case session.EventStepStart:
		h.PublishEvent(newExtensionEvent(extension.EventStepStarted, sessionID, turnID, stepID, step, nil))
	case session.EventStepEnd:
		h.PublishEvent(newExtensionEvent(extension.EventStepCompleted, sessionID, turnID, stepID, step, map[string]any{
			"status": stringValue(data, "status"),
		}))
	case session.EventToolCall:
		h.PublishEvent(newExtensionEvent(extension.EventToolStarted, sessionID, turnID, stepID, step, map[string]any{
			"tool": stringValue(data, "name"), "call_id": stringValue(data, "callId"),
		}))
	case session.EventToolResult:
		failed := data["error"] != nil || boolValue(data, "isError") || boolValue(data, "is_error")
		eventType := extension.EventToolCompleted
		if failed {
			eventType = extension.EventToolFailed
		}
		h.PublishEvent(newExtensionEvent(eventType, sessionID, turnID, stepID, step, map[string]any{
			"tool": stringValue(data, "name"), "call_id": stringValue(data, "callId"), "failed": failed,
		}))
	default:
		return
	}
}

func (h *Host) publishContextRequested(ctx context.Context, userText string) {
	correlation, _ := runtimectx.CorrelationOf(ctx)
	payload := map[string]any{"mode": "automatic"}
	if userText != "" {
		payload["input_hash"] = inputHash(userText)
	}
	h.PublishEvent(newExtensionEvent(extension.EventContextRequested, correlation.SessionID, correlation.TurnID, correlation.StepID, stepNumber(correlation.StepID), payload))
}

func (h *Host) publishContextInjected(ctx context.Context, contributions []extension.ContextContribution) {
	if len(contributions) == 0 {
		return
	}
	correlation, _ := runtimectx.CorrelationOf(ctx)
	sources := make([]string, 0, len(contributions))
	chars, tokens := 0, 0
	for _, contribution := range contributions {
		sources = append(sources, contribution.Source)
		chars += len(contribution.Content)
		tokens += estimateTokens(contribution)
	}
	h.PublishEvent(newExtensionEvent(extension.EventContextInjected, correlation.SessionID, correlation.TurnID, correlation.StepID, stepNumber(correlation.StepID), map[string]any{
		"sources": sources, "contributions": len(contributions), "chars": chars, "tokens": tokens,
	}))
}

func inputHash(value string) string {
	normalized := strings.Join(strings.Fields(value), " ")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

func normalizedInput(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func eventNumbers(data map[string]any) (turn, step int) {
	return intValue(data, "turn"), intValue(data, "step")
}

func eventIDs(turn, step int) (turnID, stepID string) {
	if turn > 0 {
		turnID = fmt.Sprintf("turn:%d", turn)
	}
	if turn > 0 && step > 0 {
		stepID = fmt.Sprintf("step:%d", step)
	}
	return turnID, stepID
}

func stepNumber(stepID string) int {
	var step int
	_, _ = fmt.Sscanf(stepID, "step:%d", &step)
	return step
}

func stringValue(data map[string]any, key string) string {
	if value, ok := data[key].(string); ok {
		return value
	}
	return ""
}

func intValue(data map[string]any, key string) int {
	if value, ok := data[key].(float64); ok {
		return int(value)
	}
	return 0
}

func boolValue(data map[string]any, key string) bool {
	value, ok := data[key].(bool)
	return ok && value
}
