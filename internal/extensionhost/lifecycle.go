package extensionhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

func (h *Host) find(id string) (*managedExtension, int, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for index, item := range h.items {
		if strings.EqualFold(item.manifest.ID, id) {
			return item, index, true
		}
	}
	return nil, -1, false
}

func (h *Host) Health(ctx context.Context, id string) (extension.HealthResult, error) {
	h.mu.RLock()
	item, _, ok := h.find(id)
	h.mu.RUnlock()
	if !ok {
		return extension.HealthResult{}, fmt.Errorf("extension: %q is not mounted", id)
	}
	if !item.initialized.Capabilities.Health {
		ready := item.ready.Load()
		return extension.HealthResult{Ready: ready, Status: "capability-disabled"}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout(time.Duration(item.manifest.Health.TimeoutMS)*time.Millisecond, h.config.HealthTimeout))
	defer cancel()
	started := time.Now()
	var result extension.HealthResult
	err := item.connection.Call(callCtx, extension.MethodHealth, nil, &result)
	ready := err == nil && result.Ready
	item.ready.Store(ready)
	event := Event{ExtensionID: item.manifest.ID, Capability: "health", Method: extension.MethodHealth, DurationMS: time.Since(started).Milliseconds(), Success: err == nil, HealthReady: ready, At: started.UTC()}
	if err != nil {
		event.Error = err.Error()
	} else if !result.Ready {
		event.Error = result.Detail
	}
	h.observe(event)
	if err != nil {
		return extension.HealthResult{}, err
	}
	return result, nil
}

// Restart performs one explicit generation replacement. Calls that observed
// the previous generation are never replayed: a committed tool side effect or
// context retrieval is allowed to surface its error first.
func (h *Host) Restart(ctx context.Context, id string) error {
	h.mu.Lock()
	old, index, ok := h.find(id)
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("extension: %q is not mounted", id)
	}
	policy := old.manifest.Lifecycle.RestartPolicy
	if policy == "" {
		policy = extension.RestartNever
	}
	if policy == extension.RestartNever {
		h.mu.Unlock()
		return fmt.Errorf("extension: %s has restart policy %q", old.manifest.ID, policy)
	}
	maxRestarts := old.manifest.Lifecycle.MaxRestarts
	if maxRestarts <= 0 {
		maxRestarts = 3
	}
	if old.restarts >= maxRestarts {
		h.mu.Unlock()
		return fmt.Errorf("%w: %s restarted %d times", ErrRestartExhausted, old.manifest.ID, old.restarts)
	}
	source := old.source
	manifest := old.manifest
	grants := old.grants
	restarts := old.restarts + 1
	h.items = append(h.items[:index], h.items[index+1:]...)
	h.mu.Unlock()

	_ = h.removeTools(old)
	h.publishLifecycle(old, extension.EventExtensionStopped, map[string]any{"reason": "restart"})
	h.stopEventDelivery(old)
	shutdownCtx, cancel := context.WithTimeout(ctx, h.config.ShutdownTimeout)
	_ = old.connection.Call(shutdownCtx, extension.MethodShutdown, nil, nil)
	cancel()
	_ = old.connection.Close()
	old.ready.Store(false)

	conn, err := newConnection(manifest.Transport)
	if err == nil {
		var replacement *managedExtension
		replacement, err = h.initialize(ctx, manifest, source, conn, grants)
		if err == nil {
			replacement.restarts = restarts
			err = h.publishTools(replacement)
			if err == nil {
				h.mu.Lock()
				if h.closed {
					h.mu.Unlock()
					_ = h.removeTools(replacement)
					_ = replacement.connection.Close()
					return ErrConnectionClosed
				}
				h.items = append(h.items, replacement)
				h.mu.Unlock()
				h.notifyWebContributions()
				h.startEventDelivery(replacement)
				h.observe(Event{ExtensionID: manifest.ID, Capability: "lifecycle", Method: "restart", Success: true, Restarts: restarts, HealthReady: true, At: time.Now().UTC()})
				h.publishLifecycle(replacement, extension.EventExtensionRestarted, map[string]any{"restarts": restarts})
				return nil
			}
			_ = h.removeTools(replacement)
		}
		_ = conn.Close()
	}
	h.observe(Event{ExtensionID: manifest.ID, Capability: "lifecycle", Method: "restart", Success: false, Restarts: restarts, Error: err.Error(), At: time.Now().UTC()})
	return err
}

// recoverAfterCall applies the declared restart policy after a terminal call
// failure. Recovery is asynchronous so an already failed model/tool call is
// never prolonged by startup, and the failed operation is never replayed.
func (h *Host) recoverAfterCall(id string, err error) {
	h.mu.RLock()
	closed := h.closed
	h.mu.RUnlock()
	if err == nil || closed || (!errors.Is(err, ErrConnectionLost) && !errors.Is(err, ErrConnectionClosed)) {
		return
	}
	h.mu.RLock()
	item, _, ok := h.find(id)
	policy := extension.RestartNever
	if ok && item.manifest.Lifecycle.RestartPolicy != "" {
		policy = item.manifest.Lifecycle.RestartPolicy
	}
	h.mu.RUnlock()
	if policy == extension.RestartNever {
		return
	}
	go func() {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), h.config.StartupTimeout*2)
		defer cancel()
		if restartErr := h.Restart(recoveryCtx, id); restartErr != nil {
			h.observe(Event{ExtensionID: id, Capability: "lifecycle", Method: "automatic-restart", Success: false, Error: restartErr.Error(), At: time.Now().UTC()})
		}
	}()
}
