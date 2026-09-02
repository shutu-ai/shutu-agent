// Package plugin provides the small ownership and hot-reload seam used by
// Agent-scoped extensions. Product plugins can still expose their own typed
// services; this package owns publication, replacement and disposal ordering.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/tools"
)

var (
	ErrClosed            = errors.New("plugin registry is closed")
	ErrInvalidID         = errors.New("plugin id is required")
	ErrNotMounted        = errors.New("plugin is not mounted")
	ErrAlreadyMount      = errors.New("plugin is already mounted")
	ErrNoRuntime         = errors.New("plugin has no dynamic runtime")
	ErrRuntimePanic      = errors.New("plugin runtime panicked")
	ErrInvalidManifest   = errors.New("plugin manifest is invalid")
	ErrDependencyMissing = errors.New("plugin dependency is not mounted")
)

// CallOutcome is the lifecycle state reported to the optional host audit
// sink. A plugin call is deliberately at-most-once: a terminal failure or a
// process death never authorizes the registry to invoke it again implicitly.
type CallOutcome string

const (
	CallStarted   CallOutcome = "started"
	CallCompleted CallOutcome = "completed"
	CallCancelled CallOutcome = "cancelled"
	CallFailed    CallOutcome = "failed"
	CallPanicked  CallOutcome = "panicked"
)

// CallReceipt is the host-facing audit record for one dynamic plugin call.
// The registry does not pretend this in-memory record is durable; a
// composition root that needs crash evidence must persist it from
// CallObserver before/after the external operation.
type CallReceipt struct {
	CallID   uint64      `json:"callId"`
	PluginID string      `json:"pluginId"`
	Method   string      `json:"method"`
	Revision uint64      `json:"revision"`
	Outcome  CallOutcome `json:"outcome"`
}

// CallObserver receives a start receipt before the plugin body runs and one
// terminal receipt after it settles. Observers must be non-blocking with
// respect to the registry and must not call back into Registry; panics are
// isolated so audit code cannot change plugin behavior.
type CallObserver func(CallReceipt)

// Runtime is the executable half of a dynamic plugin. Implementations must
// honor ctx cancellation and return the actual value produced by the plugin;
// a nil value is still a valid result when the plugin deliberately returns it.
type Runtime interface {
	Run(context.Context, string, any) (any, error)
}

// RuntimeFunc adapts a function to Runtime.
type RuntimeFunc func(context.Context, string, any) (any, error)

func (f RuntimeFunc) Run(ctx context.Context, name string, args any) (any, error) {
	return f(ctx, name, args)
}

// RuntimeFactory constructs the executable half inside the plugin's owned
// scope. A failed factory never becomes visible.
type RuntimeFactory func(*agent.Scope) (Runtime, error)

// ToolPublisher is the only production seam for binding a plugin-owned tool to
// the host registry. Every publication is stamped with the plugin identity and
// mount revision; cleanups are generation-guarded so an old scope cannot delete
// a newer successful reload.
type ToolPublisher interface {
	Publish(tools.Tool) error
}

// Manifest describes the deployable identity of a plugin. Profile, bundles
// and patch are metadata consumed by host inventory/deployment tooling; the
// registry still owns the lifecycle and dependency admission boundary.
type Manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Dependencies []string `json:"dependencies,omitempty"`
	Profile      string   `json:"profile,omitempty"`
	Bundles      []string `json:"bundles,omitempty"`
	Patch        string   `json:"patch,omitempty"`
}

// Invocation is a dynamic call result tagged with the exact published
// generation that executed it.
type Invocation struct {
	PluginID string
	Revision uint64
	Value    any
}

// Spec is one plugin composition unit. Mount receives an owned scope and must
// return a disposer for resources not registered through Scope.AddCleanup.
type Spec struct {
	ID       string
	Manifest Manifest
	Mount    func(*agent.Scope) (func() error, error)
	Runtime  RuntimeFactory
	// Tools is optional. It receives the mount-owned publisher after Mount has
	// validated plugin identity and dependencies.
	Tools func(*agent.Scope, ToolPublisher) error
}

// Registration is the detached host/client inventory projection of one
// mounted plugin generation.
type Registration struct {
	Manifest
	Revision uint64 `json:"revision"`
}

type mounted struct {
	spec     Spec
	scope    *agent.Scope
	close    func() error
	runtime  Runtime
	revision uint64
}

// Registry owns plugin instances and guarantees that replacement disposes the
// previous instance before its replacement becomes visible.
type Registry struct {
	mu        sync.RWMutex
	root      *agent.Scope
	tools     *tools.Registry
	mounted   map[string]mounted
	pending   map[string]struct{}
	next      uint64
	nextCall  atomic.Uint64
	observer  CallObserver
	closed    bool
	closeDone chan struct{}
}

func NewRegistry(root *agent.Scope) *Registry {
	if root == nil {
		root = agent.NewScope(nil)
	}
	return &Registry{root: root, mounted: make(map[string]mounted), pending: make(map[string]struct{}), closeDone: make(chan struct{})}
}

// NewRegistryWithTools composes the plugin lifecycle registry with the host's
// canonical tool registry. This is the production tool-plugin composition root.
func NewRegistryWithTools(root *agent.Scope, registry *tools.Registry) *Registry {
	pluginRegistry := NewRegistry(root)
	pluginRegistry.tools = registry
	return pluginRegistry
}

func (r *Registry) RootScope() *agent.Scope { return r.root }

// SetCallObserver installs the optional host audit sink for dynamic calls.
// Passing nil disables auditing. The observer is configuration state and is
// not copied into a replacement plugin generation.
func (r *Registry) SetCallObserver(observer CallObserver) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.observer = observer
	r.mu.Unlock()
}

// Mount publishes a plugin after its setup succeeds. A failed setup leaves
// the previous plugin untouched and disposes the partially-created scope.
func (r *Registry) Mount(spec Spec) error {
	if err := validate(spec); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if _, ok := r.mounted[spec.ID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAlreadyMount, spec.ID)
	}
	if _, ok := r.pending[spec.ID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAlreadyMount, spec.ID)
	}
	if err := r.checkDependenciesLocked(spec); err != nil {
		r.mu.Unlock()
		return err
	}
	r.pending[spec.ID] = struct{}{}
	r.next++
	revision := r.next
	scope := agent.NewScope(r.root)
	r.mu.Unlock()
	publisher := newToolPublisher(r.toolRegistry(), scope, spec.ID, revision)
	closeFn, runtime, err := mountSpec(spec, scope, publisher)
	if err != nil {
		r.mu.Lock()
		delete(r.pending, spec.ID)
		r.mu.Unlock()
		_ = closePlugin(closeFn, scope)
		return err
	}
	r.mu.Lock()
	if r.closed {
		delete(r.pending, spec.ID)
		r.mu.Unlock()
		_ = closePlugin(closeFn, scope)
		return ErrClosed
	}
	delete(r.pending, spec.ID)
	r.mounted[spec.ID] = mounted{spec: spec, scope: scope, close: closeFn, runtime: runtime, revision: revision}
	r.mu.Unlock()
	return nil
}

// Reload performs a replacement with a transaction-like visibility boundary:
// the old instance remains published until the new mount succeeds.
func (r *Registry) Reload(spec Spec) error {
	if err := validate(spec); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if _, ok := r.pending[spec.ID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAlreadyMount, spec.ID)
	}
	if err := r.checkDependenciesLocked(spec); err != nil {
		r.mu.Unlock()
		return err
	}
	r.pending[spec.ID] = struct{}{}
	old, exists := r.mounted[spec.ID]
	r.next++
	revision := r.next
	scope := agent.NewScope(r.root)
	r.mu.Unlock()
	publisher := newToolPublisher(r.toolRegistry(), scope, spec.ID, revision)
	closeFn, runtime, err := mountSpec(spec, scope, publisher)
	if err != nil {
		r.mu.Lock()
		delete(r.pending, spec.ID)
		r.mu.Unlock()
		_ = closePlugin(closeFn, scope)
		return err
	}
	r.mu.Lock()
	if r.closed {
		delete(r.pending, spec.ID)
		r.mu.Unlock()
		return closePlugin(closeFn, scope)
	}
	// A concurrent reload that won the publication race must not be disposed
	// by this older operation; its revision is the ownership fence.
	current := r.mounted[spec.ID]
	if exists && current.revision != old.revision {
		delete(r.pending, spec.ID)
		r.mu.Unlock()
		return closePlugin(closeFn, scope)
	}
	delete(r.pending, spec.ID)
	r.mounted[spec.ID] = mounted{spec: spec, scope: scope, close: closeFn, runtime: runtime, revision: revision}
	r.mu.Unlock()
	return closePlugin(old.close, old.scope)
}

func (r *Registry) toolRegistry() *tools.Registry {
	if r == nil {
		return nil
	}
	return r.tools
}

func (r *Registry) Unmount(id string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	item, ok := r.mounted[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotMounted, id)
	}
	delete(r.mounted, id)
	r.mu.Unlock()
	return closePlugin(item.close, item.scope)
}

// Call executes one method on the currently published generation. The read
// lock is held for the complete invocation, so reload/unmount cannot dispose
// the generation while a call is using it. A runtime panic is converted into a
// classified error and cannot tear down the registry.
func (r *Registry) Call(ctx context.Context, id, name string, args any) (Invocation, error) {
	if err := ctx.Err(); err != nil {
		return Invocation{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return Invocation{}, ErrClosed
	}
	item, ok := r.mounted[id]
	if !ok {
		return Invocation{}, fmt.Errorf("%w: %s", ErrNotMounted, id)
	}
	if item.runtime == nil {
		return Invocation{}, fmt.Errorf("%w: %s", ErrNoRuntime, id)
	}
	callID := r.nextCall.Add(1)
	receipt := CallReceipt{CallID: callID, PluginID: id, Method: name, Revision: item.revision, Outcome: CallStarted}
	r.observeCallLocked(receipt)
	value, err := runRuntime(item.runtime, ctx, name, args)
	if err != nil {
		outcome := CallFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = CallCancelled
		} else if errors.Is(err, ErrRuntimePanic) {
			outcome = CallPanicked
		}
		r.observeCallLocked(CallReceipt{CallID: callID, PluginID: id, Method: name, Revision: item.revision, Outcome: outcome})
		return Invocation{}, err
	}
	r.observeCallLocked(CallReceipt{CallID: callID, PluginID: id, Method: name, Revision: item.revision, Outcome: CallCompleted})
	return Invocation{PluginID: id, Revision: item.revision, Value: value}, nil
}

func (r *Registry) observeCallLocked(receipt CallReceipt) {
	if r == nil || r.observer == nil {
		return
	}
	observer := r.observer
	defer func() { _ = recover() }()
	observer(receipt)
}

// Snapshot returns currently published plugin ids and revisions.
func (r *Registry) Snapshot() map[string]uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]uint64, len(r.mounted))
	for id, item := range r.mounted {
		out[id] = item.revision
	}
	return out
}

// Inventory returns deterministic manifest metadata for the currently
// published generations. It never exposes scopes, runtimes or disposer
// functions to host/client consumers.
func (r *Registry) Inventory() []Registration {
	r.mu.RLock()
	out := make([]Registration, 0, len(r.mounted))
	for _, item := range r.mounted {
		manifest := item.spec.Manifest
		if manifest.ID == "" {
			manifest.ID = item.spec.ID
		}
		manifest.Dependencies = append([]string(nil), manifest.Dependencies...)
		manifest.Bundles = append([]string(nil), manifest.Bundles...)
		out = append(out, Registration{Manifest: manifest, Revision: item.revision})
	}
	r.mu.RUnlock()
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (r *Registry) Close() error {
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
	items := make([]mounted, 0, len(r.mounted))
	for id, item := range r.mounted {
		delete(r.mounted, id)
		items = append(items, item)
	}
	r.mu.Unlock()
	var first error
	for _, item := range items {
		if err := closePlugin(item.close, item.scope); err != nil && first == nil {
			first = err
		}
	}
	if err := r.root.Close(); err != nil && first == nil {
		first = err
	}
	close(r.closeDone)
	return first
}

func validate(spec Spec) error {
	if spec.ID == "" {
		return ErrInvalidID
	}
	if spec.Manifest.ID != "" && spec.Manifest.ID != spec.ID {
		return fmt.Errorf("%w: manifest id %q does not match %q", ErrInvalidManifest, spec.Manifest.ID, spec.ID)
	}
	if spec.Manifest.Version != "" && strings.TrimSpace(spec.Manifest.Version) == "" {
		return fmt.Errorf("%w: version must not be blank", ErrInvalidManifest)
	}
	seen := map[string]struct{}{}
	for _, dependency := range spec.Manifest.Dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" || dependency == spec.ID {
			return fmt.Errorf("%w: invalid dependency %q", ErrInvalidManifest, dependency)
		}
		if _, ok := seen[dependency]; ok {
			return fmt.Errorf("%w: duplicate dependency %q", ErrInvalidManifest, dependency)
		}
		seen[dependency] = struct{}{}
	}
	if spec.Mount == nil {
		return fmt.Errorf("plugin %q: mount function is required", spec.ID)
	}
	return nil
}

func (r *Registry) checkDependenciesLocked(spec Spec) error {
	for _, dependency := range spec.Manifest.Dependencies {
		dependency = strings.TrimSpace(dependency)
		if _, ok := r.mounted[dependency]; !ok {
			return fmt.Errorf("%w: %s requires %s", ErrDependencyMissing, spec.ID, dependency)
		}
	}
	return nil
}

func mountSpec(spec Spec, scope *agent.Scope, publisher ToolPublisher) (func() error, Runtime, error) {
	closeFn, err := spec.Mount(scope)
	if err != nil {
		return nil, nil, err
	}
	if spec.Tools != nil {
		if err := spec.Tools(scope, publisher); err != nil {
			err = joinRollback(err, rollbackPublisher(publisher))
			return closeFn, nil, err
		}
	}
	if spec.Runtime == nil {
		return closeFn, nil, nil
	}
	runtime, err := spec.Runtime(scope)
	if err != nil {
		err = joinRollback(err, rollbackPublisher(publisher))
		return closeFn, nil, err
	}
	if runtime == nil {
		err := errors.New("plugin runtime factory returned nil")
		err = joinRollback(err, rollbackPublisher(publisher))
		return closeFn, nil, err
	}
	return closeFn, runtime, nil
}

func rollbackPublisher(publisher ToolPublisher) error {
	concrete, ok := publisher.(*toolPublisher)
	if !ok {
		return nil
	}
	return concrete.rollback()
}

func joinRollback(err, rollbackErr error) error {
	if rollbackErr == nil {
		return err
	}
	return errors.Join(err, rollbackErr)
}

type toolPublisher struct {
	registry   *tools.Registry
	scope      *agent.Scope
	pluginID   string
	revision   uint64
	published  map[string]bool
	previousOf map[string]previousTool
}

type previousTool struct {
	tool    tools.Tool
	info    tools.RegistrationInfo
	existed bool
}

func newToolPublisher(registry *tools.Registry, scope *agent.Scope, pluginID string, revision uint64) *toolPublisher {
	return &toolPublisher{
		registry: registry, pluginID: pluginID, revision: revision,
		scope:      scope,
		published:  make(map[string]bool),
		previousOf: make(map[string]previousTool),
	}
}

func (p *toolPublisher) Publish(tool tools.Tool) error {
	if p == nil || tool == nil || tool.Name() == "" {
		return errors.New("plugin tool and name are required")
	}
	if p.registry == nil {
		return errors.New("plugin tool registry is unavailable")
	}
	if p.published[tool.Name()] {
		return fmt.Errorf("plugin %q published tool %q more than once", p.pluginID, tool.Name())
	}
	if _, captured := p.previousOf[tool.Name()]; !captured {
		previous, info, existed := p.registry.RegistrationTool(tool.Name())
		p.previousOf[tool.Name()] = previousTool{tool: previous, info: info, existed: existed}
	}
	info := tools.RegistrationInfo{
		Owner: p.pluginID, Plugin: p.pluginID, Generation: p.revision,
		Provenance: "plugin",
	}
	if _, exists := p.registry.Registration(tool.Name()); exists {
		if err := p.registry.ReplaceWithInfo(tool, info); err != nil {
			return err
		}
	} else if err := p.registry.RegisterWithInfo(tool, info); err != nil {
		return err
	}
	// A brand-new attempted generation is removed if its mount transaction
	// fails. A replacement is restored explicitly by rollback; this cleanup is
	// a no-op so closing the attempted scope cannot erase the live capability
	// before that rollback runs.
	cleanup := func() error {
		current, ok := p.registry.Registration(tool.Name())
		if !ok || current.Generation != p.revision || p.previousOf[tool.Name()].existed {
			return nil
		}
		return p.registry.Unregister(tool.Name())
	}
	if err := p.scope.AddCleanup(cleanup); err != nil {
		_ = p.registry.Unregister(tool.Name())
		return err
	}
	p.published[tool.Name()] = true
	return nil
}

// rollback returns every tool touched by this attempted generation to its
// previous definition/schema/registration. It is deliberately reverse-ordered
// and idempotent.
func (p *toolPublisher) rollback() error {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.published))
	for name := range p.published {
		names = append(names, name)
	}
	sort.Strings(names)
	var first error
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		previous := p.previousOf[name]
		var err error
		if previous.existed {
			err = p.registry.RestoreWithInfo(previous.tool, previous.info)
		} else {
			err = p.registry.Unregister(name)
		}
		if err != nil && first == nil {
			first = err
		}
		delete(p.published, name)
	}
	return first
}

func runRuntime(runtime Runtime, ctx context.Context, name string, args any) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrRuntimePanic, recovered)
			value = nil
		}
	}()
	return runtime.Run(ctx, name, args)
}

func closePlugin(closeFn func() error, scope *agent.Scope) error {
	var first error
	if closeFn != nil {
		first = closeFn()
	}
	if scope != nil {
		if err := scope.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
