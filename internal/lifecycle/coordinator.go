// Package lifecycle provides the process-owned shutdown coordinator.
//
// Registration order is dependency order: entries registered later are closed
// first. Close is a one-shot, concurrent-safe barrier; a caller that races the
// first Close waits for the same completion and receives the same error.
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
)

var ErrClosed = errors.New("lifecycle: coordinator is closed")

type entry struct {
	name  string
	close func() error
}

// Coordinator owns the process-level resource teardown order.
type Coordinator struct {
	mu        sync.Mutex
	entries   []entry
	closing   bool
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

func New() *Coordinator {
	return &Coordinator{closeDone: make(chan struct{})}
}

// Register adds one closer. Later registrations are closed before earlier
// ones, which makes dependency order explicit at the composition root.
func (c *Coordinator) Register(name string, closeFn func() error) error {
	if c == nil {
		return ErrClosed
	}
	if closeFn == nil {
		return fmt.Errorf("lifecycle: %s closer is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.closed {
		return ErrClosed
	}
	c.entries = append(c.entries, entry{name: name, close: closeFn})
	return nil
}

// Close begins teardown once and waits for all registered resources. It is
// safe for concurrent callers and returns joined errors with resource names.
func (c *Coordinator) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closing || c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	c.closing = true
	entries := append([]entry(nil), c.entries...)
	c.mu.Unlock()

	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		if err := closeEntry(entries[i]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entries[i].name, err))
		}
	}
	err := errors.Join(errs...)
	c.mu.Lock()
	c.closeErr = err
	c.closed = true
	close(c.closeDone)
	c.mu.Unlock()
	return err
}

func closeEntry(e entry) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			switch value := recovered.(type) {
			case error:
				err = fmt.Errorf("closer panic: %w", value)
			default:
				err = fmt.Errorf("closer panic: %v", value)
			}
		}
	}()
	return e.close()
}

// Closed reports whether teardown has completed.
func (c *Coordinator) Closed() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
