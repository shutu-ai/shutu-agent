package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
)

// writePromptFile writes a "NNN-name.md" prompt section file into dir and
// returns its path.
func writePromptFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestBuildPromptStandard verifies the default mode loads prompts_dir sections
// and injects no code-mode section (默认 standard ⇒ 现状零变化).
func TestBuildPromptStandard(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, dir, "10-persona.md", "You are the standard agent persona.")
	writePromptFile(t, dir, "20-skills.md", "Skills section.")

	b, err := buildPrompt(config.ModeStandard, dir)
	if err != nil {
		t.Fatalf("buildPrompt(standard): %v", err)
	}
	got := b.Build()
	if !strings.Contains(got, "You are the standard agent persona.") {
		t.Fatalf("standard prompt missing prompts_dir persona: %q", got)
	}
	if !strings.Contains(got, "Skills section.") {
		t.Fatalf("standard prompt missing the skills section: %q", got)
	}
	if strings.Contains(got, "程序化操作") || strings.Contains(got, "Code Mode") {
		t.Fatalf("standard prompt must not contain the code-mode section: %q", got)
	}
}

// TestBuildPromptMinimal verifies the minimal preset uses the fixed persona and
// never touches prompts_dir — even a nonexistent dir is not an error — and
// injects no code-mode section.
func TestBuildPromptMinimal(t *testing.T) {
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	b, err := buildPrompt(config.ModeMinimal, nonexistent)
	if err != nil {
		t.Fatalf("buildPrompt(minimal) with a nonexistent prompts_dir: %v", err)
	}
	if got := b.Build(); got != minimalPersona {
		t.Fatalf("minimal prompt = %q, want minimalPersona", got)
	}
	if strings.Contains(b.Build(), "程序化操作") || strings.Contains(b.Build(), "Code Mode") {
		t.Fatal("minimal prompt must not contain the code-mode section")
	}
}

// TestBuildPromptCode verifies the code preset loads prompts_dir and appends
// the programmatic-operation section after the persona (Order 1000 > persona).
func TestBuildPromptCode(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, dir, "10-persona.md", "You are the code-mode agent persona.")

	b, err := buildPrompt(config.ModeCode, dir)
	if err != nil {
		t.Fatalf("buildPrompt(code): %v", err)
	}
	got := b.Build()
	pi := strings.Index(got, "You are the code-mode agent persona.")
	ci := strings.Index(got, "`run_code` is the only tool you can call directly")
	if pi < 0 {
		t.Fatalf("code prompt missing prompts_dir persona: %q", got)
	}
	if ci < 0 {
		t.Fatalf("code prompt missing the code-mode section: %q", got)
	}
	if ci < pi {
		t.Fatalf("code-mode section (at %d) must follow the persona (at %d): %q", ci, pi, got)
	}
}

// TestBuildPromptCodeLoadDirError verifies a LoadDir failure is surfaced, not
// swallowed. The error mechanism is a NUL byte in the path: os.ReadDir rejects
// it with EINVAL ("invalid argument") on every platform, which is not
// os.IsNotExist, so LoadDir returns a real error. (Pointing at a file instead
// would not work here: on Windows os.ReadDir on a file yields ERROR_PATH_NOT_FOUND,
// which os.IsNotExist maps to true, so LoadDir's "missing dir" branch returns an
// empty builder — the failure the contract wants covered would be masked.)
func TestBuildPromptCodeLoadDirError(t *testing.T) {
	b, err := buildPrompt(config.ModeCode, "bad\x00name")
	if err == nil {
		t.Fatalf("buildPrompt(code) on an unreadable prompts_dir must return an error, got builder=%v", b)
	}
}
