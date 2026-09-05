package main

import (
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

func TestCodeModeSDKSectionDocumentsNestedTools(t *testing.T) {
	got := codeModeSDKSection([]llm.ToolSchema{
		{Name: "read", Description: "read a text file", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []string{"file_path"},
		}},
		{Name: "mcp.demo.search", Description: "search the demo server", Parameters: map[string]any{"type": "object"}},
		{Name: "run_code", Description: "outer transport", Parameters: map[string]any{"type": "object"}},
	})
	for _, want := range []string{
		"## TypeScript Code Mode SDK",
		"await tools.name(args)",
		"read: {",
		"file_path",
		"tools[\"my-tool\"]",
		"mcp.demo.search",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("SDK section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- `run_code`:") {
		t.Fatalf("SDK section must not expose recursive run_code binding:\n%s", got)
	}
}
