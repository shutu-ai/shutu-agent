package main

import (
	"errors"
)

var errAppShuttingDown = errors.New("pa: application is shutting down")

// beginShutdown is idempotent and intentionally only closes admission. The
// caller's deferred cleanup then drains each owned service in dependency
// order. Keeping admission separate from cleanup also makes late requests
// deterministic instead of racing a half-closed registry or store.
func (a *app) beginShutdown() {
	if a == nil {
		return
	}
	a.lifecycleMu.Lock()
	a.lifecycleClosed = true
	a.lifecycleMu.Unlock()
}

func (a *app) shutdownStarted() bool {
	if a == nil {
		return true
	}
	a.lifecycleMu.RLock()
	closed := a.lifecycleClosed
	a.lifecycleMu.RUnlock()
	return closed
}

func (a *app) requireRunning() error {
	if a.shutdownStarted() {
		return errAppShuttingDown
	}
	return nil
}
