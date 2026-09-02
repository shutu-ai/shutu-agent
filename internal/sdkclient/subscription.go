package sdkclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// NotificationFilter selects notifications delivered to one subscription.
type NotificationFilter func(Notification) bool

type subscriptionWait struct {
	notification Notification
	err          error
}

type subscription struct {
	mu       sync.Mutex
	queue    []Notification
	waiters  []chan subscriptionWait
	failure  error
	filter   NotificationFilter
	detach   func()
	detached bool
}

// SubscriptionHandle is the consumer side of one notification stream.
type SubscriptionHandle struct{ state *subscription }

// Next waits for the next matching notification.
func (h *SubscriptionHandle) Next(ctx context.Context) (Notification, error) {
	return h.state.next(ctx)
}

// TryNext drains an already delivered notification without waiting.
func (h *SubscriptionHandle) TryNext() (Notification, bool) { return h.state.tryNext() }

// Close detaches the subscription, drops its queue, and fails pending waits.
func (h *SubscriptionHandle) Close() {
	if h.state.detach != nil {
		h.state.detach()
	}
	h.state.close()
}

func (s *subscription) next(ctx context.Context) (Notification, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.mu.Lock()
		if len(s.queue) != 0 {
			value := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return value, nil
		}
		if s.failure != nil {
			err := s.failure
			s.mu.Unlock()
			return Notification{}, err
		}
		ch := make(chan subscriptionWait, 1)
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()

		select {
		case value := <-ch:
			if value.err != nil {
				return Notification{}, value.err
			}
			return value.notification, nil
		case <-ctx.Done():
			s.removeWaiter(ch)
			return Notification{}, ctx.Err()
		}
	}
}

func (s *subscription) removeWaiter(ch chan subscriptionWait) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, candidate := range s.waiters {
		if candidate == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

func (s *subscription) tryNext() (Notification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Notification{}, false
	}
	value := s.queue[0]
	s.queue = s.queue[1:]
	return value, true
}

func (s *subscription) close() {
	s.mu.Lock()
	s.detached = true
	s.queue = nil
	if s.failure == nil {
		s.failure = &ClosedError{Reason: "notification subscription closed"}
	}
	waiters := s.waiters
	s.waiters = nil
	failure := s.failure
	s.mu.Unlock()
	for _, ch := range waiters {
		ch <- subscriptionWait{err: failure}
	}
}

func (s *subscription) fail(err error) {
	if err == nil {
		err = fmt.Errorf("notification subscription failed")
	}
	s.mu.Lock()
	if s.failure != nil {
		s.mu.Unlock()
		return
	}
	s.failure = err
	waiters := s.waiters
	s.waiters = nil
	s.mu.Unlock()
	for _, ch := range waiters {
		ch <- subscriptionWait{err: err}
	}
}

func (s *subscription) push(notification Notification) {
	var matches bool
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if s.detach != nil {
					s.detach()
				}
				s.fail(fmt.Errorf("notification filter failed: %v", recovered))
				matches = false
			}
		}()
		s.mu.Lock()
		if s.detached || s.failure != nil {
			s.mu.Unlock()
			return
		}
		filter := s.filter
		s.mu.Unlock()
		matches = filter == nil || filter(notification)
	}()
	s.mu.Lock()
	if s.detached || s.failure != nil {
		s.mu.Unlock()
		return
	}
	if !matches {
		s.mu.Unlock()
		return
	}
	if len(s.waiters) != 0 {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.queue = append(s.queue, notification) // keep the delivered item drainable on close
		value := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		ch <- subscriptionWait{notification: value}
		return
	}
	s.queue = append(s.queue, notification)
	s.mu.Unlock()
}

func notificationString(notification Notification, keys ...string) map[string]string {
	var raw map[string]any
	if json.Unmarshal(notification.Params, &raw) != nil {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := raw[key].(string); ok {
			out[key] = value
		}
	}
	return out
}
