// fssearch_test.go — the D-GAP-1 wiring tests (docs/dispatch-gap-1.md §5):
// registerFsSearch's D10 gate (disabled registers nothing), the enabled path
// (grep registered + a real search over a temp directory returns the matching
// line, glob lists matching paths), and the config-layer whitelist rule
// (fs_search.enabled: true ⇒ tools.enabled carries grep and glob). The
// makeFsSearchApp / fsSearchPolicy pattern mirrors the fs_test / eval_test
// harnesses.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/fssearch"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeFsSearchApp builds a minimal app for registerFsSearch tests: only the
// fields registerFsSearch touches (cfg.FsSearch, reg) are set.
func makeFsSearchApp(enabled bool) *app {
	return &app{
		cfg: config.Config{
			FsSearch: config.FsSearchConfig{Enabled: config.Bool(enabled)},
		},
		reg: tools.New(),
	}
}

// fsSearchPolicy whitelists grep and glob so the registry Execute gate can run
// them (in production config.applyDefaults + PolicyFromConfig do this).
func fsSearchPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{fssearch.GrepToolName, fssearch.GlobToolName},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

// TestRegisterFsSearchDisabledRegistersNothing verifies the D10 gate: with
// fs_search.enabled=false the composition root registers no grep/glob tool
// (dispatch-gap-1 §5).
func TestRegisterFsSearchDisabledRegistersNothing(t *testing.T) {
	a := makeFsSearchApp(false)
	if err := a.registerFsSearch(); err != nil {
		t.Fatalf("registerFsSearch: %v", err)
	}
	for _, spec := range a.reg.Specs() {
		if spec.Name == fssearch.GrepToolName || spec.Name == fssearch.GlobToolName {
			t.Fatalf("%s registered while fs_search disabled", spec.Name)
		}
	}
}

// TestRegisterFsSearchEnabledRegistersAndSearches verifies the enabled path:
// grep is registered and a real search over a temp directory returns the
// matching file:line; glob lists matching paths (dispatch-gap-1 §5 E2E).
func TestRegisterFsSearchEnabledRegistersAndSearches(t *testing.T) {
	a := makeFsSearchApp(true)
	a.reg.SetPolicy(fsSearchPolicy())
	if err := a.registerFsSearch(); err != nil {
		t.Fatalf("registerFsSearch: %v", err)
	}
	found := map[string]bool{}
	for _, s := range a.reg.Specs() {
		found[s.Name] = true
	}
	if !found[fssearch.GrepToolName] || !found[fssearch.GlobToolName] {
		t.Fatalf("grep/glob not registered when fs_search.enabled=true (got %v)", found)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("alpha\nneedle here\nomega\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	args, err := json.Marshal(map[string]any{"path": root, "pattern": "needle"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := a.reg.Execute(context.Background(), fssearch.GrepToolName, args)
	if err != nil {
		t.Fatalf("grep via registry: %v", err)
	}
	if !strings.Contains(res.Output, ":2: needle here") {
		t.Fatalf("grep output = %q, want the :2: needle here hit", res.Output)
	}
	if !strings.Contains(res.Output, "1 matches") {
		t.Fatalf("grep output = %q, want the match count", res.Output)
	}

	gargs, err := json.Marshal(map[string]any{"path": root, "pattern": "*.txt"})
	if err != nil {
		t.Fatalf("marshal glob args: %v", err)
	}
	gres, err := a.reg.Execute(context.Background(), fssearch.GlobToolName, gargs)
	if err != nil {
		t.Fatalf("glob via registry: %v", err)
	}
	if !strings.Contains(gres.Output, "seed.txt") {
		t.Fatalf("glob output = %q, want seed.txt", gres.Output)
	}
}

// TestFsSearchWhitelist verifies the config-layer whitelist rule: fs_search.
// enabled: true makes config.applyDefaults append grep and glob to
// tools.enabled (dispatch-gap-1 §4).
func TestFsSearchWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("fs_search:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !config.Enabled(cfg.FsSearch.Enabled) {
		t.Error("fs_search.enabled should be true")
	}
	for _, name := range []string{fssearch.GrepToolName, fssearch.GlobToolName} {
		if !containsStr(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after fs_search.enabled", cfg.Tools.Enabled, name)
		}
	}
}
