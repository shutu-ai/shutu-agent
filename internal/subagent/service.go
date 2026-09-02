// Package subagent defines the subagent capability seam (design.md §10 D2,
// ADR 2026-08-18-m5-agent-core.md 决策 ②): a multi-provider runtime where the
// main agent delegates a task to a child agent that owns an independent session
// and an independent loop instance. A provider declares the StartRequest
// features it supports through Capabilities; the Runtime validates a request
// against the named provider's capabilities before delegating, so consumers
// depend only on the seam's interfaces (D2).
//
// The default in-process provider is SpawnProvider (a brand-new child session,
// spawn.go). Fork/remote/outputSchema providers and the job-backed background
// continuation, event, tool and config wiring are M5b-2 and later (ADR 决策 ②
// 裁剪). This package never imports the jobs, config or session-event packages:
// it only reuses the core components (session, loop, llm, prompt, tools) as a
// library, instantiated once per spawned child.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Capabilities declares which StartRequest features a Provider supports (ADR
// 决策 ②). Each bool gates the matching StartRequest field in Runtime.Start.
type Capabilities struct {
	OutputSchema bool // structured (schema-constrained) results
	DepthLimit   bool // MaxDepth is enforced (depth tracking + ErrDepthExceeded)
	ToolFilter   bool // ToolFilter whitelist is applied to the child's visible tools
	Persona      bool // Persona is applied to the child's system prompt
	// ContextInheritance means the provider can seed a child from its parent's
	// completed session history (the fork provider capability).
	ContextInheritance bool
}

// StartRequest is one delegation request (ADR 决策 ②).
type StartRequest struct {
	Label string
	// Prompt is the child agent's first user message (its task).
	Prompt string
	// Model optionally overrides the provider's configured model. Providers
	// that do not expose model routing may ignore this field explicitly.
	Model string
	// MaxTokens optionally overrides the inherited/provider output-token cap.
	// Zero means inherit the parent cap (or use the provider default).
	MaxTokens int
	// OutputSchema requests a dsh-style structured result. Providers that
	// support it expose a scoped structured_output tool to the child.
	OutputSchema map[string]any
	// ParentSessionID identifies the delegating session ("" for an unowned
	// root spawn). Providers track the parent → child lineage for depth and
	// ListChildren.
	ParentSessionID string
	// MaxDepth bounds the delegation depth. 0 means no limit. Positive values
	// require the DepthLimit capability; the provider rejects an over-deep
	// spawn with ErrDepthExceeded.
	MaxDepth int
	// ToolFilter is the child-visible tool whitelist (optional). Requires the
	// ToolFilter capability.
	ToolFilter []string
	// Persona is an optional child agent persona (optional). Requires the
	// Persona capability.
	Persona string
	// AcceptanceCriteria is the optional eval acceptance criteria list (ADR
	// D-EVAL-4). The provider injects it into the child's prompt as a
	// "验收标准（交付自检）" section so the child self-checks its deliverable.
	AcceptanceCriteria []string
	// Continuable keeps the child alive after a completed turn so Send can
	// queue follow-up messages while the process remains alive. It is false
	// for one-shot children and is intentionally opt-in for compatibility.
	Continuable bool
	// InheritParentContext requests a fork-style child seeded from its parent's
	// completed session history.
	InheritParentContext bool
}

// Result is the terminal outcome of a subagent run. StopReason is one of
// completed | aborted | error | max-tokens | refusal (ADR 决策 ②).
type Result struct {
	Output     string // the child's last non-empty assistant/message text
	StopReason string
	// Structured is populated only when OutputSchema was requested and the
	// child successfully called the scoped structured_output tool.
	Structured any
}

// Stop reason vocabulary (ADR 决策 ②: completed | aborted | error | max-tokens
// | refusal).
const (
	StopCompleted = "completed"  // normal finish (stop)
	StopAborted   = "aborted"    // cancelled before finishing
	StopError     = "error"      // the child run failed
	StopMaxTokens = "max-tokens" // model hit its token budget (length)
	StopRefusal   = "refusal"    // model refused / content filter
)

// Run is one in-flight (or settled) subagent.
type Run struct {
	// ID is the subagent's session id — under the local providers this is the
	// child session id issued by the provider.
	ID string
	// Result blocks until the subagent settles (or ctx is cancelled) and
	// returns its terminal outcome. It is safe to call repeatedly.
	Result func(ctx context.Context) (Result, error)
	// Send queues one follow-up user message for a live continuable child.
	// Providers may reject it when the child is one-shot or already settled.
	Send func(ctx context.Context, message string) error
	// SendQuiet queues context for the next explicitly waking turn. It never
	// wakes an idle child; providers may leave it nil when unsupported.
	SendQuiet func(ctx context.Context, message string) error
	// Cancel requests cancellation of a live subagent with a reason; it is
	// idempotent and fails with an error once the subagent has settled.
	Cancel func(reason string) error
}

// Provider is one subagent backend (ADR 决策 ②, D2: Service / Provider /
// Consumer three-piece seam). Multiple providers coexist in the Runtime
// registry under distinct names.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Start(ctx context.Context, req StartRequest) (*Run, error)
}

// ChildSummary is a read-only projection of one spawned child for
// ListChildren.
type ChildSummary struct {
	ID          string
	Label       string
	Running     bool
	Continuable bool
}

// childrenLister is the optional extension a Provider implements to enumerate
// the children it spawned under a parent session. The Runtime aggregates the
// results of every registered provider that implements it.
type childrenLister interface {
	ListChildren(ctx context.Context, parentSessionID string) ([]ChildSummary, error)
}

// closer is the optional extension a Provider implements to release its
// resources (e.g. await live children) when the Runtime is closed.
type closer interface {
	Close() error
}

type resumer interface {
	Resume(ctx context.Context, sessionID, prompt string, continuable bool) (*Run, error)
}

// Runtime is the subagent Service (ADR 决策 ②): a multi-provider registry that
// validates each Start against the named provider's capabilities and delegates.
// Consumers (the M5b-2 subagent tools) depend only on this interface.
type Runtime interface {
	// RegisterProvider adds a provider under its Name; a duplicate name is
	// rejected. Registering after Close is rejected.
	RegisterProvider(p Provider) error
	// GetProvider returns the named provider and whether it exists.
	GetProvider(name string) (Provider, bool)
	// ListProviders returns the registered provider names, sorted.
	ListProviders() []string
	// Start validates the request against the named provider's capabilities
	// (MaxDepth>0 ⇒ DepthLimit; ToolFilter ⇒ ToolFilter; Persona ⇒ Persona),
	// then delegates to the provider. An unknown provider name is rejected.
	Start(ctx context.Context, name string, req StartRequest) (*Run, error)
	Resume(ctx context.Context, name, sessionID, prompt string, continuable bool) (*Run, error)
	// ListChildren aggregates the spawned children of every provider that
	// tracks them for one parent session, sorted by id.
	ListChildren(ctx context.Context, parentSessionID string) ([]ChildSummary, error)
	// Close releases every registered provider that supports it (e.g. awaits
	// live children so no goroutine leaks). After Close, Register/Start are
	// rejected.
	Close() error
}

// Sentinel errors returned by Runtime and provider implementations, so callers
// can distinguish failures without parsing message text.
var (
	ErrUnknownProvider        = errors.New("subagent: unknown provider")
	ErrDuplicateProvider      = errors.New("subagent: provider already registered")
	ErrCapabilityNotSupported = errors.New("subagent: capability not supported by provider")
	ErrDepthExceeded          = errors.New("subagent: delegation depth exceeded")
	ErrProviderClosed         = errors.New("subagent: provider closed")
	ErrInvalidRequest         = errors.New("subagent: invalid start request")
	ErrNotContinuable         = errors.New("subagent: run is not continuable")
)

// NewRuntime returns an empty Runtime registry.
func NewRuntime() Runtime {
	return &runtime{providers: map[string]Provider{}, closeDone: make(chan struct{})}
}

// runtime is the default Runtime implementation: an in-memory, name-keyed
// provider registry guarded by a mutex.
type runtime struct {
	mu        sync.Mutex
	providers map[string]Provider
	closed    bool
	closeDone chan struct{}
}

func (r *runtime) RegisterProvider(p Provider) error {
	if p == nil {
		return fmt.Errorf("%w: nil provider", ErrInvalidRequest)
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("%w: provider name must be non-empty", ErrInvalidRequest)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrProviderClosed
	}
	if _, ok := r.providers[name]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateProvider, name)
	}
	r.providers[name] = p
	return nil
}

func (r *runtime) GetProvider(name string) (Provider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[name]
	return p, ok
}

func (r *runtime) ListProviders() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r *runtime) Start(ctx context.Context, name string, req StartRequest) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrProviderClosed
	}
	p, ok := r.providers[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	caps := p.Capabilities()
	if req.MaxDepth > 0 && !caps.DepthLimit {
		return nil, fmt.Errorf("%w: max_depth=%d requires a depth-limit provider", ErrCapabilityNotSupported, req.MaxDepth)
	}
	if req.OutputSchema != nil && !caps.OutputSchema {
		return nil, fmt.Errorf("%w: structured output requires an output-schema provider", ErrCapabilityNotSupported)
	}
	if len(req.ToolFilter) > 0 && !caps.ToolFilter {
		return nil, fmt.Errorf("%w: tool filter requires a tool-filter provider", ErrCapabilityNotSupported)
	}
	if req.Persona != "" && !caps.Persona {
		return nil, fmt.Errorf("%w: persona requires a persona provider", ErrCapabilityNotSupported)
	}
	if req.InheritParentContext && !caps.ContextInheritance {
		return nil, fmt.Errorf("%w: parent context inheritance requires a fork-capable provider", ErrCapabilityNotSupported)
	}
	return p.Start(ctx, req)
}

func (r *runtime) Resume(ctx context.Context, name, sessionID, prompt string, continuable bool) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrProviderClosed
	}
	p, ok := r.providers[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	rp, ok := p.(resumer)
	if !ok {
		return nil, fmt.Errorf("%w: provider %s does not support resume", ErrCapabilityNotSupported, name)
	}
	return rp.Resume(ctx, sessionID, prompt, continuable)
}

func (r *runtime) ListChildren(ctx context.Context, parentSessionID string) ([]ChildSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	ps := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		ps = append(ps, p)
	}
	r.mu.Unlock()

	var out []ChildSummary
	for _, p := range ps {
		lc, ok := p.(childrenLister)
		if !ok {
			continue
		}
		children, err := lc.ListChildren(ctx, parentSessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	r.closed = true
	ps := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		ps = append(ps, p)
	}
	r.mu.Unlock()

	var first error
	for _, p := range ps {
		if c, ok := p.(closer); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	close(r.closeDone)
	return first
}
