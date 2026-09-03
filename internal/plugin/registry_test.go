package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestRegistryDynamicRuntimeReturnsGenerationAndClassifiesFailures(t *testing.T) {
	registry := NewRegistry(nil)
	defer registry.Close()
	if err := registry.Mount(Spec{
		ID:    "dynamic",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(ctx context.Context, name string, args any) (any, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return map[string]any{"name": name, "args": args}, nil
			}), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Call(context.Background(), "dynamic", "hello", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.PluginID != "dynamic" || first.Revision != 1 {
		t.Fatalf("invocation = %+v", first)
	}
	if got := first.Value.(map[string]any)["name"]; got != "hello" {
		t.Fatalf("runtime value = %#v", first.Value)
	}
	if _, err := registry.Call(context.Background(), "missing", "x", nil); !errors.Is(err, ErrNotMounted) {
		t.Fatalf("missing runtime = %v", err)
	}
	if err := registry.Reload(Spec{
		ID:    "dynamic",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(context.Context, string, any) (any, error) { return "new", nil }), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := registry.Call(context.Background(), "dynamic", "hello", nil)
	if err != nil || second.Revision != 2 || second.Value != "new" {
		t.Fatalf("reloaded invocation = %+v, err=%v", second, err)
	}
}

func TestRegistryDynamicRuntimeCancellationPanicAndFactoryRollback(t *testing.T) {
	registry := NewRegistry(nil)
	defer registry.Close()
	if err := registry.Mount(Spec{
		ID:    "dynamic",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(ctx context.Context, name string, args any) (any, error) {
				if name == "panic" {
					panic("boom")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Call(context.Background(), "dynamic", "panic", nil); !errors.Is(err, ErrRuntimePanic) {
		t.Fatalf("panic error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := registry.Call(ctx, "dynamic", "wait", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error = %v", err)
	}
	if err := registry.Reload(Spec{
		ID: "dynamic",
		Mount: func(scope *agent.Scope) (func() error, error) {
			if err := scope.Provide("partial", true); err != nil {
				return nil, err
			}
			return func() error { return nil }, nil
		},
		Runtime: func(*agent.Scope) (Runtime, error) { return nil, errors.New("factory failed") },
	}); err == nil {
		t.Fatal("runtime factory failure was accepted")
	}
	if got := registry.Snapshot()["dynamic"]; got != 1 {
		t.Fatalf("old generation after failed reload = %d", got)
	}
}

func TestRegistryCallObserverEmitsAtMostOnceGenerationBoundReceipts(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()

	receipts := make(chan CallReceipt, 16)
	registry.SetCallObserver(func(receipt CallReceipt) { receipts <- receipt })
	if err := registry.Mount(Spec{
		ID:    "audited",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(ctx context.Context, name string, _ any) (any, error) {
				switch name {
				case "panic":
					panic("boom")
				case "cancel":
					<-ctx.Done()
					return nil, ctx.Err()
				default:
					return "ok", nil
				}
			}), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Call(context.Background(), "audited", "ok", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Call(context.Background(), "audited", "panic", nil); !errors.Is(err, ErrRuntimePanic) {
		t.Fatalf("panic call = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := registry.Call(ctx, "audited", "cancel", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel call = %v", err)
	}

	got := make([]CallReceipt, 0, 6)
	for len(got) < 6 {
		select {
		case receipt := <-receipts:
			got = append(got, receipt)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for receipts: %+v", got)
		}
	}
	want := []CallOutcome{CallStarted, CallCompleted, CallStarted, CallPanicked, CallStarted, CallCancelled}
	for i, outcome := range want {
		if got[i].CallID != uint64(i/2+1) || got[i].Outcome != outcome || got[i].PluginID != "audited" || got[i].Revision != 1 {
			t.Fatalf("receipt[%d] = %+v, want call=%d outcome=%q revision=1", i, got[i], i/2+1, outcome)
		}
	}

	// A reload advances the fence. A call after reload receives a new
	// generation receipt; the observer cannot make the old generation replay.
	if err := registry.Reload(Spec{
		ID:    "audited",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(context.Context, string, any) (any, error) { return "new", nil }), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	invocation, err := registry.Call(context.Background(), "audited", "ok", nil)
	if err != nil || invocation.Value != "new" || invocation.Revision != 2 {
		t.Fatalf("reloaded invocation = %+v, %v", invocation, err)
	}
	for len(got) < 8 {
		select {
		case receipt := <-receipts:
			got = append(got, receipt)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for reload receipts: %+v", got)
		}
	}
	if got[6].CallID != 4 || got[6].Outcome != CallStarted || got[6].Revision != 2 || got[7].CallID != 4 || got[7].Outcome != CallCompleted || got[7].Revision != 2 {
		t.Fatalf("reload receipts = %+v", got[6:])
	}
}

func TestRegistryCallObserverPanicCannotChangeCallOutcome(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()
	registry.SetCallObserver(func(CallReceipt) { panic("observer failure") })
	if err := registry.Mount(Spec{
		ID:    "observer-panic",
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(context.Context, string, any) (any, error) { return "ok", nil }), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	invocation, err := registry.Call(context.Background(), "observer-panic", "run", nil)
	if err != nil || invocation.Value != "ok" {
		t.Fatalf("observer panic changed call = %+v, %v", invocation, err)
	}
}

func TestRegistryMountReloadFailureAndOwnership(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()
	var disposed []string
	mount := func(value string) Spec {
		return Spec{ID: "demo", Mount: func(scope *agent.Scope) (func() error, error) {
			if err := scope.Provide("value", value); err != nil {
				return nil, err
			}
			return func() error {
				disposed = append(disposed, value)
				return nil
			}, nil
		}}
	}
	if err := registry.Mount(mount("one")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Mount(mount("duplicate")); !errors.Is(err, ErrAlreadyMount) {
		t.Fatalf("duplicate mount error = %v", err)
	}
	failing := Spec{ID: "demo", Mount: func(*agent.Scope) (func() error, error) {
		return nil, errors.New("bad plugin")
	}}
	if err := registry.Reload(failing); err == nil {
		t.Fatal("failed reload unexpectedly succeeded")
	}
	if got := registry.Snapshot(); len(got) != 1 {
		t.Fatalf("failed reload removed old plugin: %v", got)
	}
	if err := registry.Reload(mount("two")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disposed, []string{"one"}) {
		t.Fatalf("disposed after reload = %v", disposed)
	}
	if err := registry.Unmount("demo"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disposed, []string{"one", "two"}) {
		t.Fatalf("disposed after unmount = %v", disposed)
	}
}

func TestRegistryScopeIsChildAndClosesAfterUnmount(t *testing.T) {
	root := agent.NewScope(nil)
	if err := root.Provide("root", "ok"); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(root)
	if err := registry.Mount(Spec{ID: "scoped", Mount: func(scope *agent.Scope) (func() error, error) {
		value, err := scope.Resolve("root")
		if err != nil || value != "ok" {
			t.Fatalf("inherited root = %#v, %v", value, err)
		}
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryManifestDependenciesAndInventory(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()

	dependent := Spec{
		ID: "feature",
		Manifest: Manifest{
			ID:           "feature",
			Version:      "1.2.3",
			Dependencies: []string{"base"},
			Profile:      "default",
			Bundles:      []string{"tools", "prompts"},
			Patch:        "feature.patch",
		},
		Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
	}
	if err := registry.Mount(dependent); !errors.Is(err, ErrDependencyMissing) {
		t.Fatalf("missing dependency error = %v", err)
	}
	if err := registry.Mount(Spec{
		ID:       "base",
		Manifest: Manifest{ID: "base", Version: "1.0.0"},
		Mount:    func(*agent.Scope) (func() error, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Mount(dependent); err != nil {
		t.Fatal(err)
	}

	inventory := registry.Inventory()
	if len(inventory) != 2 || inventory[0].ID != "base" || inventory[1].ID != "feature" {
		t.Fatalf("inventory ordering = %+v", inventory)
	}
	if inventory[1].Revision == 0 || inventory[1].Version != "1.2.3" || inventory[1].Profile != "default" {
		t.Fatalf("inventory metadata = %+v", inventory[1])
	}
	inventory[1].Dependencies[0] = "mutated"
	if got := registry.Inventory()[1].Dependencies[0]; got != "base" {
		t.Fatalf("inventory leaked mutable dependencies: %v", got)
	}
}

func TestRegistryRejectsInvalidManifest(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()
	base := func(manifest Manifest) error {
		return registry.Mount(Spec{ID: "plugin", Manifest: manifest, Mount: func(*agent.Scope) (func() error, error) {
			return nil, nil
		}})
	}
	for name, manifest := range map[string]Manifest{
		"mismatched id":        {ID: "other"},
		"blank version":        {Version: "   "},
		"empty dependency":     {Dependencies: []string{"  "}},
		"self dependency":      {Dependencies: []string{"plugin"}},
		"duplicate dependency": {Dependencies: []string{"base", "base"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := base(manifest); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type pluginTool struct {
	name        string
	description string
	value       string
}

func (t pluginTool) Name() string                                 { return t.name }
func (t pluginTool) Description() string                          { return t.description }
func (t pluginTool) Schema() map[string]any                       { return map[string]any{"type": "object"} }
func (t pluginTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (t pluginTool) Execute(context.Context, any) (string, error) { return t.value, nil }

func TestRegistryPublishesAndReplacesPluginOwnedTool(t *testing.T) {
	toolRegistry := tools.New()
	registry := NewRegistryWithTools(nil, toolRegistry)
	defer func() { _ = registry.Close(); _ = toolRegistry.Unregister("plugin_echo") }()
	spec := func(value string) Spec {
		return Spec{
			ID: "tool-plugin", Manifest: Manifest{Version: "1.0.0"},
			Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
			Tools: func(_ *agent.Scope, publisher ToolPublisher) error {
				return publisher.Publish(pluginTool{name: "plugin_echo", description: "generation " + value, value: value})
			},
		}
	}
	if err := registry.Mount(spec("one")); err != nil {
		t.Fatal(err)
	}
	info, ok := toolRegistry.Registration("plugin_echo")
	if !ok || info.Owner != "tool-plugin" || info.Plugin != "tool-plugin" || info.Provenance != "plugin" || info.Generation != 1 {
		t.Fatalf("plugin registration = %+v, ok=%v", info, ok)
	}
	toolRegistry.SetPolicy(tools.Policy{Profile: "standard", Enabled: []string{"plugin_echo"}})
	prepared, err := toolRegistry.Prepare(context.Background(), "old-call", "plugin_echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Reload(spec("two")); err != nil {
		t.Fatal(err)
	}
	info, _ = toolRegistry.Registration("plugin_echo")
	if info.Generation != 2 {
		t.Fatalf("reloaded registration generation = %d, want 2", info.Generation)
	}
	if _, err := toolRegistry.ExecutePrepared(context.Background(), prepared); tools.ErrorInfoOf(err).Code != tools.CodeStaleToolGeneration {
		t.Fatalf("old prepared execution error = %v, want stale generation", err)
	}
	result, err := toolRegistry.Execute(context.Background(), "plugin_echo", json.RawMessage(`{}`))
	if err != nil || result.Output != "two" {
		t.Fatalf("reloaded execution = %+v, %v, want two", result, err)
	}
}

func TestRegistryReloadQuiescesInFlightGeneration(t *testing.T) {
	registry := NewRegistry(nil)
	defer func() { _ = registry.Close() }()

	started := make(chan struct{})
	release := make(chan struct{})
	var disposed = make(chan struct{}, 1)
	if err := registry.Mount(Spec{
		ID: "quiesced",
		Mount: func(*agent.Scope) (func() error, error) {
			return func() error { disposed <- struct{}{}; return nil }, nil
		},
		Runtime: func(*agent.Scope) (Runtime, error) {
			return RuntimeFunc(func(context.Context, string, any) (any, error) {
				close(started)
				<-release
				return "old", nil
			}), nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	callDone := make(chan Invocation, 1)
	callErr := make(chan error, 1)
	go func() {
		value, err := registry.Call(context.Background(), "quiesced", "run", nil)
		callDone <- value
		callErr <- err
	}()
	<-started

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- registry.Reload(Spec{
			ID:    "quiesced",
			Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
			Runtime: func(*agent.Scope) (Runtime, error) {
				return RuntimeFunc(func(context.Context, string, any) (any, error) { return "new", nil }), nil
			},
		})
	}()

	select {
	case err := <-reloadDone:
		t.Fatalf("reload published before in-flight call completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-callErr; err != nil {
		t.Fatal(err)
	}
	if got := (<-callDone).Value; got != "old" {
		t.Fatalf("in-flight result = %#v", got)
	}
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-disposed:
	case <-time.After(time.Second):
		t.Fatal("old generation was not disposed after quiescing")
	}
	value, err := registry.Call(context.Background(), "quiesced", "run", nil)
	if err != nil || value.Value != "new" || value.Revision != 2 {
		t.Fatalf("new generation = %+v, err=%v", value, err)
	}
}
