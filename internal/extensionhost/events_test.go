package extensionhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

type fakeEventConnection struct {
	mu        sync.Mutex
	events    []extension.Event
	delay     time.Duration
	failures  int
	entered   chan struct{}
	enterOnce sync.Once
	closed    bool
}

func (c *fakeEventConnection) Call(ctx context.Context, method string, params, result any) error {
	if method != extension.MethodEvent {
		return nil
	}
	if c.entered != nil {
		c.enterOnce.Do(func() { close(c.entered) })
	}
	if c.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.delay):
		}
	}
	if c.failures > 0 {
		c.failures--
		if c.failures == 1 {
			return ErrConnectionLost
		}
		return errors.New("invalid event response")
	}
	event, ok := params.(extension.Event)
	if !ok {
		return errors.New("invalid event params")
	}
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	return nil
}

func (c *fakeEventConnection) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeEventConnection) received() []extension.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]extension.Event(nil), c.events...)
}

func eventSubscriber(t *testing.T, h *Host, id string, types []string, conn connection) *managedExtension {
	t.Helper()
	subscriptions := make(map[string]struct{}, len(types))
	for _, eventType := range types {
		subscriptions[eventType] = struct{}{}
	}
	item := &managedExtension{
		manifest:   extension.Manifest{ID: id, Capabilities: extension.Capabilities{Events: true}},
		connection: conn, subscriptions: subscriptions,
		initialized: extension.InitializeResult{Capabilities: extension.Capabilities{Events: true}},
	}
	h.mu.Lock()
	h.items = append(h.items, item)
	h.mu.Unlock()
	h.startEventDelivery(item)
	return item
}

func waitForEvents(t *testing.T, conn *fakeEventConnection, count int) []extension.Event {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if events := conn.received(); len(events) >= count {
			return events
		}
		time.Sleep(time.Millisecond)
	}
	return conn.received()
}

func TestEventSubscriptionFilteringAndOrder(t *testing.T) {
	host := New(Config{EventQueueSize: 16, EventTimeout: time.Second})
	subscribedConn := &fakeEventConnection{}
	unsubscribedConn := &fakeEventConnection{}
	subscriber := eventSubscriber(t, host, "subscribed", []string{
		extension.EventTurnStarted, extension.EventToolStarted, extension.EventToolFailed,
	}, subscribedConn)
	eventSubscriber(t, host, "unsubscribed", nil, unsubscribedConn)

	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	host.PublishEvent(extension.Event{Type: extension.EventTurnCompleted})
	host.PublishEvent(extension.Event{Type: extension.EventToolStarted})
	host.PublishEvent(extension.Event{Type: extension.EventToolFailed})

	events := waitForEvents(t, subscribedConn, 3)
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != extension.EventTurnStarted || events[1].Type != extension.EventToolStarted || events[2].Type != extension.EventToolFailed {
		t.Fatalf("event order = %#v", events)
	}
	if events[0].Version != extension.EventVersion || events[0].EventID == "" || events[0].OccurredAt == "" {
		t.Fatalf("event envelope = %#v", events[0])
	}
	if len(unsubscribedConn.received()) != 0 {
		t.Fatal("unsubscribed extension received an event")
	}
	if !subscribed(subscriber, extension.EventTurnStarted) || subscribed(subscriber, extension.EventTurnCompleted) {
		t.Fatal("subscription matcher is inconsistent")
	}
	host.Close()
}

func TestSlowEventSubscriberDoesNotBlockPublish(t *testing.T) {
	host := New(Config{EventQueueSize: 3, EventTimeout: time.Second})
	conn := &fakeEventConnection{delay: 30 * time.Millisecond}
	eventSubscriber(t, host, "slow", []string{extension.EventTurnStarted}, conn)
	started := time.Now()
	for i := 0; i < 20; i++ {
		host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	}
	elapsed := time.Since(started)
	if elapsed > 10*time.Millisecond {
		t.Fatalf("publish blocked for %s", elapsed)
	}
	host.Close()
}

func TestEventSubscribersReceiveIndependentOrderedCopies(t *testing.T) {
	host := New(Config{EventQueueSize: 8, EventTimeout: time.Second})
	first := &fakeEventConnection{}
	second := &fakeEventConnection{}
	eventSubscriber(t, host, "first", []string{extension.EventTurnStarted, extension.EventToolStarted}, first)
	eventSubscriber(t, host, "second", []string{extension.EventTurnStarted, extension.EventToolStarted}, second)

	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	host.PublishEvent(extension.Event{Type: extension.EventToolStarted})
	firstEvents := waitForEvents(t, first, 2)
	secondEvents := waitForEvents(t, second, 2)
	if len(firstEvents) != 2 || len(secondEvents) != 2 {
		t.Fatalf("subscriber events = %#v / %#v", firstEvents, secondEvents)
	}
	for _, events := range [][]extension.Event{firstEvents, secondEvents} {
		if events[0].Type != extension.EventTurnStarted || events[1].Type != extension.EventToolStarted {
			t.Fatalf("ordered events = %#v", events)
		}
	}
	if firstEvents[0].EventID == secondEvents[0].EventID {
		t.Fatal("each subscriber must receive a distinct event envelope copy")
	}
	host.Close()
}

func TestEventHandlerTimeoutIsObserved(t *testing.T) {
	observed := make(chan Event, 4)
	host := New(Config{EventQueueSize: 2, EventTimeout: 10 * time.Millisecond, Observer: func(event Event) { observed <- event }})
	conn := &fakeEventConnection{delay: time.Second}
	eventSubscriber(t, host, "timeout", []string{extension.EventTurnStarted}, conn)
	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-observed:
			if event.Timeout && !event.Delivered && event.Error != "" {
				host.Close()
				return
			}
		case <-deadline:
			host.Close()
			t.Fatal("event timeout was not observed")
		}
	}
}

func TestEventFailureIsObservedWithoutStoppingAgentPublishing(t *testing.T) {
	observed := make(chan Event, 8)
	host := New(Config{EventQueueSize: 8, EventTimeout: 100 * time.Millisecond, Observer: func(event Event) { observed <- event }})
	conn := &fakeEventConnection{failures: 2}
	eventSubscriber(t, host, "failing", []string{extension.EventToolStarted}, conn)
	for i := 0; i < 3; i++ {
		host.PublishEvent(extension.Event{Type: extension.EventToolStarted})
	}
	deadline := time.Now().Add(time.Second)
	failures := 0
	deliveries := 0
	for time.Now().Before(deadline) {
		select {
		case event := <-observed:
			if event.Dropped || event.Error != "" {
				failures++
			} else if event.Delivered {
				deliveries++
			}
		default:
			if failures >= 2 && deliveries >= 1 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatalf("failures=%d deliveries=%d", failures, deliveries)
}

func TestEventQueueOverflowIsObservedAndDropped(t *testing.T) {
	observed := make(chan Event, 8)
	host := New(Config{EventQueueSize: 1, EventTimeout: 50 * time.Millisecond, Observer: func(event Event) { observed <- event }})
	entered := make(chan struct{})
	conn := &fakeEventConnection{entered: entered, delay: time.Second}
	eventSubscriber(t, host, "overflow", []string{extension.EventTurnStarted}, conn)

	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start delivery")
	}
	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-observed:
			if event.Dropped {
				host.Close()
				return
			}
		case <-deadline:
			t.Fatal("queue overflow was not observed")
		}
	}
}

func TestSessionEventProjectionAndMinimalPayload(t *testing.T) {
	host := New(Config{EventQueueSize: 16, EventTimeout: time.Second})
	conn := &fakeEventConnection{}
	eventSubscriber(t, host, "observer", []string{
		extension.EventTurnStarted, extension.EventStepStarted,
		extension.EventToolStarted, extension.EventToolFailed,
	}, conn)
	sessionID := "session-events"
	host.PublishSessionEvent(sessionID, session.Event{Seq: 1, Type: session.EventTurnStart, Data: []byte(`{"turn":1}`)})
	host.PublishSessionEvent(sessionID, session.Event{Seq: 2, Type: session.EventStepStart, Data: []byte(`{"turn":1,"step":1}`)})
	host.PublishSessionEvent(sessionID, session.Event{Seq: 3, Type: session.EventUserMessage, Data: []byte(`{"text":"secret prompt"}`)})
	host.PublishSessionEvent(sessionID, session.Event{Seq: 4, Type: session.EventToolCall, Data: []byte(`{"turn":1,"step":1,"callId":"call-1","name":"shell","arguments":{"command":"secret"}}`)})
	host.PublishSessionEvent(sessionID, session.Event{Seq: 5, Type: session.EventToolResult, Data: []byte(`{"turn":1,"step":1,"callId":"call-1","name":"shell","error":{"code":"TOOL_RESULT_ERROR"}}`)})

	events := waitForEvents(t, conn, 4)
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	if events[3].Type != extension.EventToolFailed || events[3].Payload["failed"] != true {
		t.Fatalf("tool failure event = %#v", events[3])
	}
	if events[3].SessionID != sessionID || events[3].TurnID != "turn:1" || events[3].StepID != "step:1" || events[3].Step != 1 {
		t.Fatalf("event correlation = %#v", events[3])
	}
	for _, event := range events {
		for _, value := range event.Payload {
			if text, ok := value.(string); ok && (text == "secret prompt" || text == "secret") {
				t.Fatalf("sensitive payload leaked: %#v", event)
			}
		}
	}
	host.Close()
}

func TestPublishSessionStartedUsesStableEnvelope(t *testing.T) {
	host := New(Config{EventQueueSize: 8, EventTimeout: time.Second})
	conn := &fakeEventConnection{}
	eventSubscriber(t, host, "session-observer", []string{extension.EventSessionStarted}, conn)

	host.PublishSessionStarted("new-session")
	events := waitForEvents(t, conn, 1)
	if len(events) != 1 {
		t.Fatalf("session started events = %#v", events)
	}
	event := events[0]
	if event.Type != extension.EventSessionStarted || event.SessionID != "new-session" ||
		event.Version != extension.EventVersion || event.EventID == "" || event.Payload != nil {
		t.Fatalf("session started envelope = %#v", event)
	}
	host.Close()
}

func TestReplacementGenerationResubscribes(t *testing.T) {
	host := New(Config{EventQueueSize: 8, EventTimeout: time.Second})
	oldConn := &fakeEventConnection{}
	old := eventSubscriber(t, host, "restart", []string{extension.EventTurnStarted}, oldConn)
	host.stopEventDelivery(old)
	old.connection.Close()

	newConn := &fakeEventConnection{}
	replacement := &managedExtension{
		manifest: old.manifest, connection: newConn, subscriptions: old.subscriptions,
		initialized: old.initialized,
	}
	host.mu.Lock()
	host.items[0] = replacement
	host.mu.Unlock()
	host.startEventDelivery(replacement)
	host.PublishEvent(extension.Event{Type: extension.EventTurnStarted})
	if events := waitForEvents(t, newConn, 1); len(events) != 1 {
		t.Fatalf("replacement events = %#v", events)
	}
	host.Close()
}

func TestCloseDeliversTerminalLifecycleBeforeTransportClose(t *testing.T) {
	host := New(Config{EventQueueSize: 1, EventTimeout: time.Second})
	conn := &fakeEventConnection{}
	eventSubscriber(t, host, "shutdown", []string{extension.EventExtensionStopped}, conn)

	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	events := waitForEvents(t, conn, 1)
	if len(events) != 1 || events[0].Type != extension.EventExtensionStopped {
		t.Fatalf("shutdown events = %#v", events)
	}
	if events[0].Payload["reason"] != "shutdown" {
		t.Fatalf("shutdown payload = %#v", events[0].Payload)
	}
}
