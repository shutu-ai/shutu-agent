// Package agent contains the process-local Agent runtime.  It deliberately
// owns lifecycle and scope primitives without importing the product loop or
// any concrete provider; those are installed through the Runner seam.
package agent

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrScopeClosed = errors.New("agent scope is closed")
	ErrValueAbsent = errors.New("agent scope value is absent")
)

// Scope is an owned, hierarchical capability scope.  A child can resolve
// values from its parent but owns its own registrations and cleanups.
type Scope struct {
	mu       sync.RWMutex
	parent   *Scope
	values   map[string]any
	cleanup  []func() error
	children []*Scope
	closed   bool
}

// NewScope creates a child scope.  A nil parent creates a root scope.
func NewScope(parent *Scope) *Scope {
	scope := &Scope{parent: parent, values: make(map[string]any)}
	if parent != nil {
		parent.mu.Lock()
		if parent.closed {
			scope.closed = true
		} else {
			parent.children = append(parent.children, scope)
		}
		parent.mu.Unlock()
	}
	return scope
}

// Provide registers one capability in this scope.  Duplicate local keys are
// rejected so an accidental second provider cannot silently replace a live
// capability.
func (s *Scope) Provide(key string, value any) error {
	if s == nil {
		return ErrScopeClosed
	}
	if key == "" {
		return errors.New("agent scope key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrScopeClosed
	}
	if _, exists := s.values[key]; exists {
		return fmt.Errorf("agent scope capability %q already registered", key)
	}
	s.values[key] = value
	return nil
}

// Resolve finds a capability in this scope or one of its ancestors.
func (s *Scope) Resolve(key string) (any, error) {
	if s == nil {
		return nil, ErrScopeClosed
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrScopeClosed
	}
	value, ok := s.values[key]
	parent := s.parent
	s.mu.RUnlock()
	if ok {
		return value, nil
	}
	if parent != nil {
		return parent.Resolve(key)
	}
	return nil, fmt.Errorf("%w: %q", ErrValueAbsent, key)
}

// AddCleanup adds an owned disposer.  Disposers run in reverse registration
// order when the scope closes.
func (s *Scope) AddCleanup(cleanup func() error) error {
	if s == nil {
		return ErrScopeClosed
	}
	if cleanup == nil {
		return errors.New("agent scope cleanup is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrScopeClosed
	}
	s.cleanup = append(s.cleanup, cleanup)
	return nil
}

// Close disposes this scope exactly once.  The first disposer error is
// returned after all disposers have had a chance to run.
func (s *Scope) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cleanups := append([]func() error(nil), s.cleanup...)
	children := append([]*Scope(nil), s.children...)
	s.cleanup = nil
	s.children = nil
	s.values = nil
	s.mu.Unlock()

	var first error
	for _, child := range children {
		if err := child.Close(); err != nil && first == nil {
			first = err
		}
	}
	for i := len(cleanups) - 1; i >= 0; i-- {
		if err := cleanups[i](); err != nil && first == nil {
			first = err
		}
	}
	return first
}
