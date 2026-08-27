package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

func TestBuildSingleSection(t *testing.T) {
	b := New("You are helpful.")
	got := b.Build()
	if got != "You are helpful." {
		t.Fatalf("Build() = %q", got)
	}
}

func writePrompt(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestLoadDirOrdersSections verifies sections load in numeric-prefix order,
// independent of file creation order (dispatch-m2: 提示词分节).
func TestLoadDirOrdersSections(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "10-persona.md", "persona body")
	writePrompt(t, dir, "20-skills.md", "skills body")

	b, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	got := b.Build()
	want := "persona body\n\nskills body"
	if got != want {
		t.Fatalf("Build() = %q, want %q", got, want)
	}
}

// TestLoadDirSkipsEmptySections verifies an empty section file contributes
// nothing.
func TestLoadDirSkipsEmptySections(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "10-persona.md", "persona body")
	writePrompt(t, dir, "20-skills.md", "") // placeholder, no content yet

	b, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got := b.Build(); got != "persona body" {
		t.Fatalf("Build() = %q, want %q", got, "persona body")
	}
}

// TestLoadDirIgnoresNonSectionFiles verifies files without the NNN- prefix
// (like README.md) never become prompt sections.
func TestLoadDirIgnoresNonSectionFiles(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "10-persona.md", "persona body")
	writePrompt(t, dir, "README.md", "documentation must not leak into the prompt")

	b, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got := b.Build(); got != "persona body" {
		t.Fatalf("Build() = %q, want %q", got, "persona body")
	}
}

// TestLoadDirMissingDirEmpty verifies a missing prompts directory yields an
// empty builder, not an error.
func TestLoadDirMissingDirEmpty(t *testing.T) {
	b, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got := b.Build(); got != "" {
		t.Fatalf("Build() = %q, want empty", got)
	}
}

// TestAddRemoveSections verifies sections can be added and removed
// programmatically without touching the loop (design.md §7).
func TestAddRemoveSections(t *testing.T) {
	b := NewBuilder()
	b.Add(Section{Name: "persona", Order: 10, Text: "persona body"})
	b.Add(Section{Name: "skills", Order: 20, Text: "skills body"})
	if got := b.Build(); got != "persona body\n\nskills body" {
		t.Fatalf("Build() = %q", got)
	}
	// Replacing by name keeps a single section.
	b.Add(Section{Name: "persona", Order: 10, Text: "new persona"})
	if got := b.Build(); got != "new persona\n\nskills body" {
		t.Fatalf("Build() after replace = %q", got)
	}
	b.Remove("skills")
	if got := b.Build(); got != "new persona" {
		t.Fatalf("Build() after remove = %q", got)
	}
	b.Remove("does-not-exist") // no-op
	if got := b.Build(); got != "new persona" {
		t.Fatalf("Build() after no-op remove = %q", got)
	}
}

// TestBuildAppendsToolCatalog verifies the automatic tool catalog renders last
// when a tools provider is installed (design.md §7 "工具 schema 自动").
func TestBuildAppendsToolCatalog(t *testing.T) {
	b := NewBuilder()
	b.Add(Section{Name: "persona", Order: 10, Text: "persona body"})
	b.SetTools(func() []llm.ToolSchema {
		return []llm.ToolSchema{
			{Name: "get_time", Description: "current time"},
			{Name: "read", Description: "read a file"},
		}
	})
	got := b.Build()
	want := "persona body\n\nAvailable tools:\n- get_time: current time\n- read: read a file"
	if got != want {
		t.Fatalf("Build() = %q, want %q", got, want)
	}
}

func TestBuildRendersDSHVariablesWithoutMutatingSections(t *testing.T) {
	b := New("You are powered by {{model}} in {{cwd}}.")
	b.SetVariables(map[string]string{"model": "deepseek-v4-flash", "cwd": `C:\work`})

	if got := b.Build(); got != `You are powered by deepseek-v4-flash in C:\work.` {
		t.Fatalf("rendered prompt = %q", got)
	}

	clone := b.Clone().SetVariables(map[string]string{"model": "other", "cwd": "/tmp"})
	if got := clone.Build(); got != "You are powered by other in /tmp." {
		t.Fatalf("clone rendered prompt = %q", got)
	}
	if got := b.Build(); got != `You are powered by deepseek-v4-flash in C:\work.` {
		t.Fatalf("original builder mutated by clone = %q", got)
	}
}

// TestBuildToolCatalogEmptyProvider verifies no catalog when the provider
// returns no tools.
func TestBuildToolCatalogEmptyProvider(t *testing.T) {
	b := NewBuilder()
	b.Add(Section{Name: "persona", Order: 10, Text: "persona body"})
	b.SetTools(func() []llm.ToolSchema { return nil })
	if got := b.Build(); got != "persona body" {
		t.Fatalf("Build() = %q, want %q", got, "persona body")
	}
}
