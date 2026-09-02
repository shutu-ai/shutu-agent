package code

import (
	"context"
	"fmt"
	"sync"
)

// engine is the default code-sandbox Service (ADR 决策 M6e): it owns the
// closed state and delegates execution to a Provider. The unexported concrete
// type (mirroring the schedule seam's engine) keeps the Engine interface the
// only public shape; NewEngine returns it as a concrete *engine that satisfies
// Engine.
type engine struct {
	prov Provider

	mu        sync.Mutex
	closed    bool
	closeDone chan struct{}
}

// NewEngine returns an engine backed by prov; a nil prov selects the default
// local subprocess sandbox (NewLocalProvider). Each engine should own its
// provider: Close releases it.
func NewEngine(prov Provider) *engine {
	if prov == nil {
		prov = newLocalProvider()
	}
	return &engine{prov: prov, closeDone: make(chan struct{})}
}

// Run executes req through the Provider. A non-zero exit and a timeout are
// normal outcomes reported in Result; the error return signals the run did not
// happen (closed engine, cancelled context, or a provider failure).
func (e *engine) Run(ctx context.Context, req RunRequest) (Result, error) {
	if err := e.checkOpen(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if reporter, ok := e.prov.(capabilityReporter); ok {
		cap := reporter.Capabilities()
		if !cap.Available {
			return Result{}, fmt.Errorf("%w: provider %q is unavailable", ErrSandboxUnavailable, e.prov.Name())
		}
		if req.RequireStrongIsolation && !cap.StrongIsolation {
			return Result{}, fmt.Errorf("%w: provider %q lacks strong isolation", ErrSandboxUnavailable, e.prov.Name())
		}
		if req.RequireNetworkIsolation && !cap.NetworkIsolation {
			return Result{}, fmt.Errorf("%w: provider %q cannot enforce network policy", ErrSandboxUnavailable, e.prov.Name())
		}
		mode := normalizeSandboxMode(req.Mode)
		if mode == "" {
			mode = SandboxWorkspaceWrite
		}
		if req.AllowNetwork && mode != SandboxFullAccess && !cap.NetworkIsolation {
			return Result{}, fmt.Errorf("%w: provider %q cannot enforce requested network access policy", ErrSandboxUnavailable, e.prov.Name())
		}
		if len(cap.SupportedModes) > 0 && !supportsMode(cap.SupportedModes, mode) {
			return Result{}, fmt.Errorf("%w: provider %q cannot enforce sandbox mode %q", ErrSandboxUnavailable, e.prov.Name(), mode)
		}
	} else if req.RequireStrongIsolation || req.RequireNetworkIsolation {
		return Result{}, fmt.Errorf("%w: provider %q has no capability report", ErrSandboxUnavailable, e.prov.Name())
	}
	req.Mode = normalizeSandboxMode(req.Mode)
	return e.prov.Run(ctx, req)
}

func supportsMode(modes []SandboxMode, requested SandboxMode) bool {
	for _, mode := range modes {
		if mode == requested {
			return true
		}
	}
	return false
}

// checkOpen rejects operations on a closed engine.
func (e *engine) checkOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEngineClosed
	}
	return nil
}

// Close releases the Provider (if it implements closer) and marks the engine
// closed so further Run calls are rejected with ErrEngineClosed. It is
// idempotent.
func (e *engine) Close() error {
	e.mu.Lock()
	if e.closed {
		closeDone := e.closeDone
		e.mu.Unlock()
		<-closeDone
		return nil
	}
	e.closed = true
	prov := e.prov
	e.mu.Unlock()
	if c, ok := prov.(closer); ok {
		err := c.Close()
		close(e.closeDone)
		return err
	}
	close(e.closeDone)
	return nil
}
