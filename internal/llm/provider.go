// Provider interface + multi-provider Registry (M8-2, ADR
// 2026-08-20-m8-message-model.md D2 / dispatch-m8-2 §2). The LLM seam is the
// D2 三件套: an interface (Provider), a registry (Registry), and consumers
// (loop/compaction/subagent) that depend only on the resolved llm.LLM — the
// composition root picks one provider and injects it, so swapping providers
// never touches a consumer.
package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrProviderUnavailable is the stable route-admission class used when a
// registered provider fails its local availability check. Callers may add
// route details with %w, but must not require parsing the message text.
var ErrProviderUnavailable = errors.New("llm provider is unavailable")

// ErrCapabilityUnavailable is the stable negative class for an explicit model
// feature request that the selected model's catalog declaration does not own.
var ErrCapabilityUnavailable = errors.New("model capability is unavailable")

// CredentialProvider resolves a provider credential for one operation. The
// composition root may rotate credentials without rebuilding every consumer;
// adapters must snapshot the returned value before constructing the request
// and must never persist it in durable events or diagnostics.
type CredentialProvider func(context.Context) (string, error)

// CredentialLease keeps a resolved credential alive for the lifetime of one
// provider operation. Revocation blocks new leases, while an in-flight stream
// may finish before Release wipes the old value.
type CredentialLease interface {
	Value() string
	Release()
}

// CredentialLeaseProvider is the release-aware credential seam. The older
// CredentialProvider remains supported for standalone adapters and embedders.
type CredentialLeaseProvider func(context.Context) (CredentialLease, error)

// Provider is an LLM backend (D2). Consumers (loop/composition root) depend
// only on this interface, never on a concrete provider.
type Provider interface {
	// ID returns the stable provider id ("deepseek-official" / "openai" / "anthropic").
	ID() string
	// Available reports whether the provider can be used: a cheap local check
	// (key/endpoint resolvable) that never performs a network call
	// (dispatch-m8-2 §2).
	Available() bool
	// Stream starts a chat request and returns an incremental reader honoring
	// ctx cancellation (D6).
	Stream(ctx context.Context, req ChatRequest) (StreamReader, error)
}

// RetryPolicyProvider is an optional provider-owned recovery declaration.
// The loop/retry layer captures this at route selection time; transport
// adapters must not silently replace it with a different policy.
type RetryPolicyProvider interface {
	RetryPolicy() RetryPolicy
}

// RetryPolicy is the provider-neutral shape of the reference route policy.
// Durations are represented in milliseconds at the boundary so the value is
// stable in durable retry events and JSON configuration.
type RetryPolicy struct {
	Mode           string
	MaxRetries     int
	RetryableCodes []string
	InitialDelayMS int64
	MaxDelayMS     int64
	JitterRatio    float64
}

// ImageCapability is an optional provider capability. A transport must not
// advertise or admit image input merely because a global config flag is set;
// it must also verify the exact selected provider route exposes this marker.
type ImageCapability interface {
	SupportsImages() bool
}

// Closeable is an optional provider lifecycle seam. Providers that retain
// credential material implement it to wipe that material when their
// generation is retired or the application shuts down.
type Closeable interface {
	Close() error
}

// Registry is the multi-provider registry (D2). Providers are registered by
// stable id at wiring time and selected by the composition root; consumers
// never see the registry (they hold the resolved provider via the LLM
// interface). Registration happens during wiring and selection during startup,
// both on the serial path (D5); the RWMutex guards against any future
// concurrent reader, mirroring web.Engine.
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]Provider
	order []Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Provider)}
}

// Register adds p under its stable id; a duplicate or empty id is an error.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errors.New("llm: nil provider")
	}
	id := p.ID()
	if id == "" {
		return errors.New("llm: provider with empty id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("llm: duplicate provider id %q", id)
	}
	r.byID[id] = p
	r.order = append(r.order, p)
	return nil
}

// Get returns the provider registered under id, or an error when absent.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("llm: no such provider %q", id)
	}
	return p, nil
}

// List returns every registered provider in registration order (a copy, so
// callers never alias the registry's internal slice).
func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.order))
	copy(out, r.order)
	return out
}

// Close disposes every provider that exposes a lifecycle. Providers are
// closed after taking a stable snapshot so a provider's disposer never runs
// while the registry lock is held.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	providers := r.List()
	var first error
	for _, provider := range providers {
		if closeable, ok := provider.(Closeable); ok {
			if err := closeable.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
