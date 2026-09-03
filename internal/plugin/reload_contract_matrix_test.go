package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

type matrixPluginTool struct {
	name        string
	description string
	value       string
	schema      map[string]any
}

func (t matrixPluginTool) Name() string                 { return t.name }
func (t matrixPluginTool) Description() string          { return t.description }
func (t matrixPluginTool) Schema() map[string]any       { return t.schema }
func (t matrixPluginTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t matrixPluginTool) Execute(context.Context, any) (string, error) {
	return t.value, nil
}

// TestPluginReloadFaultAndGenerationMatrix consolidates the externally visible
// reload contract: malformed manifests/dependencies fail before mount; any
// mount/tool/runtime failure preserves the old generation; and a successful
// reload atomically replaces the schema, generation and inventory.
func TestPluginReloadFaultAndGenerationMatrix(t *testing.T) {
	toolRegistry := tools.New()
	registry := NewRegistryWithTools(nil, toolRegistry)
	defer func() {
		_ = registry.Close()
		_ = toolRegistry.Unregister("external_tool")
	}()

	spec := func(version string, schema map[string]any, mutate func(spec *Spec)) Spec {
		value := version
		result := Spec{
			ID: "external", Manifest: Manifest{ID: "external", Version: version},
			Mount: func(*agent.Scope) (func() error, error) { return nil, nil },
			Tools: func(_ *agent.Scope, publisher ToolPublisher) error {
				return publisher.Publish(matrixPluginTool{
					name: "external_tool", description: "generation " + value, value: value,
					schema: schema,
				})
			},
			Runtime: func(*agent.Scope) (Runtime, error) {
				return RuntimeFunc(func(context.Context, string, any) (any, error) { return value, nil }), nil
			},
		}
		if mutate != nil {
			mutate(&result)
		}
		return result
	}
	firstSchema := map[string]any{"type": "object", "additionalProperties": false}
	if err := registry.Mount(spec("one", firstSchema, nil)); err != nil {
		t.Fatal(err)
	}
	firstInfo, ok := toolRegistry.Registration("external_tool")
	if !ok || firstInfo.Provenance != "plugin" || firstInfo.Generation != 1 {
		t.Fatalf("initial registration = %+v, ok=%v", firstInfo, ok)
	}
	toolRegistry.SetPolicy(tools.Policy{Enabled: []string{"external_tool"}})
	prepared, err := toolRegistry.Prepare(context.Background(), "old-call", "external_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	faults := []struct {
		name    string
		spec    Spec
		wantErr error
	}{
		{
			name:    "manifest mismatch",
			spec:    spec("bad", firstSchema, func(spec *Spec) { spec.Manifest.ID = "other" }),
			wantErr: ErrInvalidManifest,
		},
		{
			name:    "missing dependency",
			spec:    spec("bad", firstSchema, func(spec *Spec) { spec.Manifest.Dependencies = []string{"missing"} }),
			wantErr: ErrDependencyMissing,
		},
		{
			name: "mount failure",
			spec: spec("bad", firstSchema, func(spec *Spec) {
				spec.Mount = func(*agent.Scope) (func() error, error) { return nil, errors.New("mount failed") }
			}),
		},
		{
			name: "tool publication failure",
			spec: spec("bad", firstSchema, func(spec *Spec) {
				oldTools := spec.Tools
				spec.Tools = func(scope *agent.Scope, publisher ToolPublisher) error {
					if err := oldTools(scope, publisher); err != nil {
						return err
					}
					return errors.New("schema publication rejected")
				}
			}),
		},
		{
			name: "runtime factory failure",
			spec: spec("bad", firstSchema, func(spec *Spec) {
				spec.Runtime = func(*agent.Scope) (Runtime, error) { return nil, errors.New("runtime failed") }
			}),
		},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			err := registry.Reload(tc.spec)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("reload error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && err == nil {
				t.Fatal("reload fault was accepted")
			}
			info, ok := toolRegistry.Registration("external_tool")
			if !ok || info.Generation != 1 {
				t.Fatalf("failed reload changed registration = %+v, ok=%v", info, ok)
			}
			result, err := toolRegistry.Execute(context.Background(), "external_tool", json.RawMessage(`{}`))
			if err != nil || result.Output != "one" {
				t.Fatalf("old generation after fault = %+v, %v", result, err)
			}
		})
	}

	secondSchema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}
	if err := registry.Reload(spec("two", secondSchema, nil)); err != nil {
		t.Fatal(err)
	}
	generation := registry.Snapshot()["external"]
	if generation <= 1 {
		t.Fatalf("reloaded generation = %d, want an advanced published revision", generation)
	}
	if _, err := toolRegistry.ExecutePrepared(context.Background(), prepared); tools.ErrorInfoOf(err).Code != tools.CodeStaleToolGeneration {
		t.Fatalf("stale prepared execution = %v, want stale generation", err)
	}
	if _, err := toolRegistry.Execute(context.Background(), "external_tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("new required-path schema accepted an empty object")
	}
	result, err := toolRegistry.Execute(context.Background(), "external_tool", json.RawMessage(`{"path":"ok"}`))
	if err != nil || result.Output != "two" {
		t.Fatalf("reloaded execution = %+v, %v", result, err)
	}
	info, _ := toolRegistry.Registration("external_tool")
	if info.Owner != "external" || info.Plugin != "external" || info.Provenance != "plugin" || info.Generation != generation {
		t.Fatalf("reloaded registration = %+v", info)
	}
	invocation, err := registry.Call(context.Background(), "external", "run", nil)
	if err != nil || invocation.Value != "two" || invocation.Revision != generation {
		t.Fatalf("reloaded invocation = %+v, %v", invocation, err)
	}
	inventory := registry.Inventory()
	if len(inventory) != 1 || inventory[0].Version != "two" || inventory[0].Revision != generation {
		t.Fatalf("reloaded inventory = %+v", inventory)
	}
}
