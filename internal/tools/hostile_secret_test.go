package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type hostilePanicTool struct{}

func (hostilePanicTool) Name() string        { return "hostile_panic" }
func (hostilePanicTool) Description() string { return "hostile panic redaction tool" }
func (hostilePanicTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (hostilePanicTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (hostilePanicTool) Execute(context.Context, any) (string, error) {
	panic("backend rejected Authorization: Bearer hostile-panic-bearer")
}

// TestHostileToolPanicDiagnosticIsRedacted proves a panic value cannot become
// a durable tool-result dump by escaping the registry's panic classifier.
func TestHostileToolPanicDiagnosticIsRedacted(t *testing.T) {
	registry := New()
	if err := registry.Register(hostilePanicTool{}); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(Policy{Enabled: []string{"hostile_panic"}})
	result, err := registry.Execute(context.Background(), "hostile_panic", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("hostile panic escaped registry: %v", err)
	}
	if !result.IsError || result.Error == nil || result.Error.Code != CodeToolPanic {
		t.Fatalf("hostile panic result = %+v, want TOOL_PANIC", result)
	}
	if strings.Contains(result.Output, "hostile-panic-bearer") {
		t.Fatalf("panic result leaked credential: %q", result.Output)
	}
	if !strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("panic result lost useful redacted context: %q", result.Output)
	}
}
