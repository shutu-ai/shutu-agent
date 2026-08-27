package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// defaultOnCaps returns the consumer-tool names appended to the whitelist when
// every capability is at its (now enabled-by-default) state. Order matches
// applyDefaults. Compaction is intentionally absent: it has no tools.
func defaultOnCaps() []string {
	var names []string
	for _, group := range [][]string{jobsToolNames, subagentToolNames,
		skillToolNames, scheduleToolNames, goalToolNames, todoToolNames,
		interactToolNames, codeToolNames, mcpToolNames, fsToolNames,
		[]string{"read_image"},
		webToolNames, terminalToolNames, fsSearchToolNames,
		ralphToolNames, workflowToolNames} {
		names = append(names, group...)
	}
	return names
}

// defaultOnWhitelist is the full execution whitelist when every capability is
// at its (now enabled) default: the read-only base plus all consumer tools.
func defaultOnWhitelist() []string {
	w := append([]string(nil), defaultEnabledTools...)
	return append(w, defaultOnCaps()...)
}

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != DefaultModel {
		t.Errorf("model = %q, want %q", cfg.Model, DefaultModel)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Errorf("base_url = %q, want %q", cfg.BaseURL, DefaultBaseURL)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("data_dir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.PromptsDir != DefaultPromptsDir {
		t.Errorf("prompts_dir = %q, want %q", cfg.PromptsDir, DefaultPromptsDir)
	}
}

func TestLoadParsesFileAndFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "model: deepseek-reasoner\nbase_url: https://example.com\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "deepseek-reasoner" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("base_url = %q", cfg.BaseURL)
	}
	// Omitted fields fall back to defaults.
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("data_dir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.PromptsDir != DefaultPromptsDir {
		t.Errorf("prompts_dir = %q, want %q", cfg.PromptsDir, DefaultPromptsDir)
	}
}

func TestLoadRejectsInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("model: [unclosed"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid yaml")
	}
}

func TestLoadEmptyBaseURLMeansProviderDefault(t *testing.T) {
	// An explicitly-empty base_url must stay empty (=> provider default),
	// not be silently replaced.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("base_url: \"\""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "" {
		t.Errorf("base_url = %q, want empty", cfg.BaseURL)
	}
}

// M3 (dsh 对齐): a config without a tools section must carry the defaults — the
// read-only whitelist base, a 30s deadline, a 64KB output limit, and every
// capability's consumer tools (all capabilities now default-on).
func TestLoadToolsDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := defaultOnWhitelist()
	if !reflect.DeepEqual(cfg.Tools.Enabled, want) {
		t.Errorf("enabled = %v, want %v", cfg.Tools.Enabled, want)
	}
	if cfg.Tools.Timeout.Duration != DefaultToolTimeout {
		t.Errorf("timeout = %v, want %v", cfg.Tools.Timeout, DefaultToolTimeout)
	}
	if cfg.Tools.OutputLimit != DefaultOutputLimit {
		t.Errorf("output_limit = %d, want %d", cfg.Tools.OutputLimit, DefaultOutputLimit)
	}
	if cfg.Tools.RunCommand.Enabled {
		t.Error("run_command must be disabled by default")
	}
}

func TestLoadParsesToolsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `
tools:
  enabled: [read]
  timeout: 5s
  output_limit: 4096
  run_command:
    enabled: true
    timeout: 1m
    workdir: C:\work
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := append([]string{"read", "bash"}, defaultOnCaps()...)
	if !reflect.DeepEqual(cfg.Tools.Enabled, want) {
		t.Errorf("enabled = %v", cfg.Tools.Enabled)
	}
	if cfg.Tools.Timeout.Duration != 5*time.Second {
		t.Errorf("timeout = %v", cfg.Tools.Timeout)
	}
	if cfg.Tools.OutputLimit != 4096 {
		t.Errorf("output_limit = %d", cfg.Tools.OutputLimit)
	}
	if !cfg.Tools.RunCommand.Enabled {
		t.Error("run_command.enabled should be true")
	}
	if cfg.Tools.RunCommand.Timeout.Duration != time.Minute {
		t.Errorf("run_command.timeout = %v", cfg.Tools.RunCommand.Timeout)
	}
	if cfg.Tools.RunCommand.Workdir != `C:\work` {
		t.Errorf("run_command.workdir = %q", cfg.Tools.RunCommand.Workdir)
	}
}

func TestLoadRunCommandEnabledAppendsToWhitelist(t *testing.T) {
	// tools.run_command.enabled: true is the single switch that turns the
	// execution tool on: it must also become whitelisted (design.md §5).
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("tools:\n  run_command:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"read", "bash"} {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q", cfg.Tools.Enabled, name)
		}
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("tools:\n  timeout: not-a-duration\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadDefaultsEmptyRunCommandTimeoutToDshBash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("tools:\n  run_command:\n    enabled: true\n    timeout: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.RunCommand.Timeout.Duration != DefaultRunCommandTimeout {
		t.Errorf("run_command.timeout = %v, want %v", cfg.Tools.RunCommand.Timeout, DefaultRunCommandTimeout)
	}
	if cfg.Tools.Timeout.Duration != DefaultToolTimeout {
		t.Errorf("global timeout = %v, want %v", cfg.Tools.Timeout, DefaultToolTimeout)
	}
	if !strings.Contains(strings.Join(cfg.Tools.Enabled, ","), "bash") {
		t.Errorf("whitelist = %v, want run_command present", cfg.Tools.Enabled)
	}
}

// M5a: jobs is off by default (D10), and the per-owner active-job cap defaults
// to 10 (dispatch-m5a-2 §3).
func TestLoadJobsDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Jobs.Enabled) {
		t.Error("Jobs must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Jobs.MaxConcurrentJobsPerOwner != DefaultMaxConcurrentJobsPerOwner {
		t.Errorf("jobs.max_concurrent_jobs_per_owner = %d, want default %d",
			cfg.Jobs.MaxConcurrentJobsPerOwner, DefaultMaxConcurrentJobsPerOwner)
	}
	// With jobs disabled no job tool may be whitelisted.
	for _, name := range jobsToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when jobs ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M5a: jobs.enabled: true is the single switch that turns the whole capability
// on — the five job_* tools must also become whitelisted (dispatch-m5a-2 §3,
func TestLoadJobsEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("jobs:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range jobsToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after jobs.enabled", cfg.Tools.Enabled, name)
		}
	}
	if cfg.Jobs.MaxConcurrentJobsPerOwner != DefaultMaxConcurrentJobsPerOwner {
		t.Errorf("absent jobs.max_concurrent_jobs_per_owner = %d, want default %d",
			cfg.Jobs.MaxConcurrentJobsPerOwner, DefaultMaxConcurrentJobsPerOwner)
	}
}

// M5a: an explicit max_concurrent_jobs_per_owner is honored, and a
// non-positive value falls back to the default 10 (dispatch-m5a-2 §3).
func TestLoadJobsMaxConcurrentExplicitAndDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("jobs:\n  enabled: true\n  max_concurrent_jobs_per_owner: 3\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jobs.MaxConcurrentJobsPerOwner != 3 {
		t.Errorf("jobs.max_concurrent_jobs_per_owner = %d, want 3", cfg.Jobs.MaxConcurrentJobsPerOwner)
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("jobs:\n  enabled: true\n  max_concurrent_jobs_per_owner: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Jobs.MaxConcurrentJobsPerOwner != DefaultMaxConcurrentJobsPerOwner {
		t.Errorf("jobs.max_concurrent_jobs_per_owner 0 = %d, want default %d",
			cfg2.Jobs.MaxConcurrentJobsPerOwner, DefaultMaxConcurrentJobsPerOwner)
	}
}

// M5b: subagent is off by default (D10), the delegation depth cap defaults to
// 8, the default provider to "spawn", and no subagent tool is whitelisted
// while disabled (dispatch-m5b-2 §3).
func TestLoadSubagentDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Subagent.Enabled) {
		t.Error("Subagent must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Subagent.MaxDepth != DefaultSubagentMaxDepth {
		t.Errorf("subagent.max_depth = %d, want default %d", cfg.Subagent.MaxDepth, DefaultSubagentMaxDepth)
	}
	if cfg.Subagent.DefaultProvider != DefaultSubagentProvider {
		t.Errorf("subagent.default_provider = %q, want default %q", cfg.Subagent.DefaultProvider, DefaultSubagentProvider)
	}
	for _, name := range subagentToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when subagent ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M5b: an explicit subagent section is honored (enabled, max_depth,
// default_provider), while a non-positive max_depth and an empty
// default_provider fall back to their defaults (dispatch-m5b-2 §3).
func TestLoadSubagentParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("subagent:\n  enabled: true\n  max_depth: 4\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Subagent.Enabled) {
		t.Error("subagent.enabled should be true")
	}
	if cfg.Subagent.MaxDepth != 4 {
		t.Errorf("subagent.max_depth = %d, want 4", cfg.Subagent.MaxDepth)
	}
	if cfg.Subagent.DefaultProvider != DefaultSubagentProvider {
		t.Errorf("absent subagent.default_provider = %q, want default %q", cfg.Subagent.DefaultProvider, DefaultSubagentProvider)
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("subagent:\n  enabled: true\n  max_depth: 0\n  default_provider: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Subagent.MaxDepth != DefaultSubagentMaxDepth {
		t.Errorf("subagent.max_depth 0 = %d, want default %d", cfg2.Subagent.MaxDepth, DefaultSubagentMaxDepth)
	}
	if cfg2.Subagent.DefaultProvider != DefaultSubagentProvider {
		t.Errorf("subagent.default_provider empty = %q, want default %q", cfg2.Subagent.DefaultProvider, DefaultSubagentProvider)
	}
}

// M5b: subagent.enabled: true is the single switch that turns the whole
// capability on — the four subagent_* tools must also become whitelisted
func TestLoadSubagentEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("subagent:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range subagentToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after subagent.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// D-GAP-4: external subagent providers are off by default (D10) and an empty
// command falls back to the per-name default (codex→"codex",
// claude_code→"claude") in applyDefaults.
func TestExternalProviderDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(
		"subagent:\n  external_providers:\n    codex:\n      enabled: true\n    claude_code: {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	codex, ok := cfg.Subagent.ExternalProviders["codex"]
	if !ok {
		t.Fatal("external_providers.codex missing after Load")
	}
	if !codex.Enabled {
		t.Error("codex.enabled should be true")
	}
	if codex.Command != "codex" {
		t.Errorf("codex command = %q, want the default \"codex\"", codex.Command)
	}
	cc, ok := cfg.Subagent.ExternalProviders["claude_code"]
	if !ok {
		t.Fatal("external_providers.claude_code missing after Load")
	}
	if cc.Enabled {
		t.Error("claude_code must be disabled by default (D10)")
	}
	if cc.Command != "claude" {
		t.Errorf("claude_code command = %q, want the default \"claude\"", cc.Command)
	}
}

// M5c: compaction is off by default (D10), the token-pressure threshold
// defaults to 32000, the retained tail to 8 turns, and max_chars to 0 (the
// nothing may be whitelisted for it even when enabled (dispatch-m5c-2a §2).
func TestLoadCompactionDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Compaction.Enabled) {
		t.Error("Compaction must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Compaction.TokenThreshold != DefaultCompactionTokenThreshold {
		t.Errorf("compaction.token_threshold = %d, want default %d",
			cfg.Compaction.TokenThreshold, DefaultCompactionTokenThreshold)
	}
	if cfg.Compaction.RetainTurns != DefaultCompactionRetainTurns {
		t.Errorf("compaction.retain_turns = %d, want default %d",
			cfg.Compaction.RetainTurns, DefaultCompactionRetainTurns)
	}
	if cfg.Compaction.MaxChars != 0 {
		t.Errorf("compaction.max_chars = %d, want 0 (engine default)", cfg.Compaction.MaxChars)
	}
	if cfg.Compaction.SummaryInputTokens != DefaultCompactionSummaryInputTokens {
		t.Errorf("compaction.summary_input_tokens = %d, want default %d",
			cfg.Compaction.SummaryInputTokens, DefaultCompactionSummaryInputTokens)
	}
	// The whitelist is the full default-on set (all capability tools):
	// compaction adds no tools of its own.
	want := defaultOnWhitelist()
	if !reflect.DeepEqual(cfg.Tools.Enabled, want) {
		t.Errorf("whitelist = %v, want %v (compaction adds nothing)", cfg.Tools.Enabled, want)
	}
}

// M5c: an explicit compaction section is honored (enabled, token_threshold,
// retain_turns, max_chars), while a non-positive token_threshold/retain_turns
// fall back to their defaults (校验非负: a negative value never survives) and
// max_chars stays 0 (engine default) (dispatch-m5c-2a §2).
func TestLoadCompactionParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("compaction:\n  enabled: true\n  token_threshold: 50000\n  retain_turns: 12\n  summary_input_tokens: 9000\n  max_chars: 2000\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Compaction.Enabled) {
		t.Error("compaction.enabled should be true")
	}
	if cfg.Compaction.TokenThreshold != 50000 {
		t.Errorf("compaction.token_threshold = %d, want 50000", cfg.Compaction.TokenThreshold)
	}
	if cfg.Compaction.RetainTurns != 12 {
		t.Errorf("compaction.retain_turns = %d, want 12", cfg.Compaction.RetainTurns)
	}
	if cfg.Compaction.MaxChars != 2000 {
		t.Errorf("compaction.max_chars = %d, want 2000", cfg.Compaction.MaxChars)
	}
	if cfg.Compaction.SummaryInputTokens != 9000 {
		t.Errorf("compaction.summary_input_tokens = %d, want 9000", cfg.Compaction.SummaryInputTokens)
	}

	// Non-positive (including negative) thresholds fall back to defaults;
	// max_chars stays 0.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("compaction:\n  enabled: true\n  token_threshold: 0\n  retain_turns: -3\n  summary_input_tokens: -1\n  max_chars: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Compaction.TokenThreshold != DefaultCompactionTokenThreshold {
		t.Errorf("compaction.token_threshold 0 = %d, want default %d",
			cfg2.Compaction.TokenThreshold, DefaultCompactionTokenThreshold)
	}
	if cfg2.Compaction.RetainTurns != DefaultCompactionRetainTurns {
		t.Errorf("compaction.retain_turns -3 = %d, want default %d",
			cfg2.Compaction.RetainTurns, DefaultCompactionRetainTurns)
	}
	if cfg2.Compaction.MaxChars != 0 {
		t.Errorf("compaction.max_chars = %d, want 0 (engine default)", cfg2.Compaction.MaxChars)
	}
	if cfg2.Compaction.SummaryInputTokens != DefaultCompactionSummaryInputTokens {
		t.Errorf("compaction.summary_input_tokens -1 = %d, want default %d",
			cfg2.Compaction.SummaryInputTokens, DefaultCompactionSummaryInputTokens)
	}
}

// M5c: compaction.enabled: true must NOT append any tool to the whitelist —
// compaction has no consumer tools (automatic triggering runs through the loop
// pre-step injector, manual through the /compact command, dispatch-m5c-2a §2).
func TestLoadCompactionEnabledDoesNotAppendToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("compaction:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Enabling compaction leaves the whitelist exactly at the default-on set:
	// no compaction tool exists to add, and none is invented.
	want := defaultOnWhitelist()
	if !reflect.DeepEqual(cfg.Tools.Enabled, want) {
		t.Errorf("whitelist = %v, want %v (compaction.enabled adds nothing)", cfg.Tools.Enabled, want)
	}
}

// M5d: skill is off by default (D10), dirs are empty, the catalog is bounded
// to 500 chars and the returned skill body to 8000 chars, and skill_load is
// not whitelisted while disabled (dispatch-m5d-2 §2).
func TestLoadSkillDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Skill.Enabled) {
		t.Error("Skill must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if len(cfg.Skill.Dirs) != 0 {
		t.Errorf("skill.dirs = %v, want empty", cfg.Skill.Dirs)
	}
	if cfg.Skill.CatalogMaxChars != DefaultSkillCatalogMaxChars {
		t.Errorf("skill.catalog_max_chars = %d, want default %d",
			cfg.Skill.CatalogMaxChars, DefaultSkillCatalogMaxChars)
	}
	if cfg.Skill.BodyMaxChars != DefaultSkillBodyMaxChars {
		t.Errorf("skill.body_max_chars = %d, want default %d",
			cfg.Skill.BodyMaxChars, DefaultSkillBodyMaxChars)
	}
	for _, name := range skillToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when skill ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M5d: an explicit skill section is honored (enabled, dirs, catalog_max_chars,
// body_max_chars), while non-positive bounds fall back to their defaults
// (校验非负: a negative value never survives) (dispatch-m5d-2 §2).
func TestLoadSkillParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("skill:\n  enabled: true\n  dirs: [C:\\skills, D:\\more]\n  catalog_max_chars: 300\n  body_max_chars: 4096\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Skill.Enabled) {
		t.Error("skill.enabled should be true")
	}
	if len(cfg.Skill.Dirs) != 2 || cfg.Skill.Dirs[0] != `C:\skills` || cfg.Skill.Dirs[1] != `D:\more` {
		t.Errorf("skill.dirs = %v, want [C:\\skills D:\\more]", cfg.Skill.Dirs)
	}
	if cfg.Skill.CatalogMaxChars != 300 {
		t.Errorf("skill.catalog_max_chars = %d, want 300", cfg.Skill.CatalogMaxChars)
	}
	if cfg.Skill.BodyMaxChars != 4096 {
		t.Errorf("skill.body_max_chars = %d, want 4096", cfg.Skill.BodyMaxChars)
	}

	// Non-positive (including negative) bounds fall back to the defaults.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("skill:\n  enabled: true\n  catalog_max_chars: 0\n  body_max_chars: -1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Skill.CatalogMaxChars != DefaultSkillCatalogMaxChars {
		t.Errorf("skill.catalog_max_chars 0 = %d, want default %d",
			cfg2.Skill.CatalogMaxChars, DefaultSkillCatalogMaxChars)
	}
	if cfg2.Skill.BodyMaxChars != DefaultSkillBodyMaxChars {
		t.Errorf("skill.body_max_chars -1 = %d, want default %d",
			cfg2.Skill.BodyMaxChars, DefaultSkillBodyMaxChars)
	}
}

// M5d: skill.enabled: true is the single switch that turns the whole capability
// on — the skill_load tool must also become whitelisted (dispatch-m5d-2 §2,
func TestLoadSkillEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("skill:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range skillToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after skill.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M6a-2: schedule is off by default (D10) and the serial clock cadence
// defaults to 1m; the schedule_* tools must not be whitelisted while disabled
// (dispatch-m6a-2 §2).
func TestLoadScheduleDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Schedule.Enabled) {
		t.Error("Schedule must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Schedule.TickInterval.Duration != DefaultScheduleTickInterval {
		t.Errorf("schedule.tick_interval = %v, want default %v",
			cfg.Schedule.TickInterval.Duration, DefaultScheduleTickInterval)
	}
	for _, name := range scheduleToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when schedule ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6a-2: an explicit schedule section is honored (enabled, tick_interval as a
// Go duration string), while a non-positive cadence falls back to the default
// (校验非负: a negative value never survives) (dispatch-m6a-2 §2).
func TestLoadScheduleParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("schedule:\n  enabled: true\n  tick_interval: 30s\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Schedule.Enabled) {
		t.Error("schedule.enabled should be true")
	}
	if cfg.Schedule.TickInterval.Duration != 30*time.Second {
		t.Errorf("schedule.tick_interval = %v, want 30s", cfg.Schedule.TickInterval.Duration)
	}

	// A non-positive (including negative) cadence falls back to the default.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("schedule:\n  enabled: true\n  tick_interval: -1m\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Schedule.TickInterval.Duration != DefaultScheduleTickInterval {
		t.Errorf("schedule.tick_interval -1m = %v, want default %v",
			cfg2.Schedule.TickInterval.Duration, DefaultScheduleTickInterval)
	}
}

// M6a-2: schedule.enabled: true is the single switch that turns the whole
// capability on — the schedule_* tools must also become whitelisted
func TestLoadScheduleEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("schedule:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range scheduleToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after schedule.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M6b-2: plan is off by default (D10) and the plan_* tools must not be
// whitelisted while disabled (dispatch-m6b-2 §2).
func TestLoadPlanDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Plan.Enabled) {
		t.Error("Plan must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	for _, name := range append(append([]string{}, goalToolNames...), todoToolNames...) {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when plan ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6b-2: an explicit plan section is honored, and an explicit plan.enabled:
// false leaves the default whitelist untouched (D10).
func TestLoadPlanParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("plan:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Plan.Enabled) {
		t.Error("plan.enabled should be true")
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("plan:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Plan.Enabled) {
		t.Error("plan.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range append(append([]string{}, goalToolNames...), todoToolNames...) {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when plan explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M6b-2: plan.enabled: true is the single switch that turns the whole
// capability on — the six plan_* tools must also become whitelisted
func TestLoadPlanEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("plan:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range append(append([]string{}, goalToolNames...), todoToolNames...) {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after plan.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M6c-2: an absent spill section means the capability is off by default (D10)
// with auto_spill defaulting on within an enabled spill (AutoSpillValue), and
// no spill_* tool is whitelisted.
func TestLoadSpillDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Spill.Enabled) {
		t.Error("Spill must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if !cfg.Spill.AutoSpillValue() {
		t.Error("auto_spill must default to true (absent ⇒ true)")
	}
}

// M6c-2: an explicit spill section is honored; an explicit enabled:false
// leaves the default whitelist untouched, and auto_spill:false disables the
// auto-sedimentation while an explicit true/absent keeps it on.
func TestLoadSpillParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spill:\n  enabled: false\n  auto_spill: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg.Spill.Enabled) {
		t.Error("spill.enabled = true, want false (explicitly disabled)")
	}
	if cfg.Spill.AutoSpillValue() {
		t.Error("auto_spill must be false when explicitly disabled")
	}

	// auto_spill absent within an enabled spill defaults to true.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("spill:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg2.Spill.Enabled) {
		t.Error("spill.enabled should be true")
	}
	if !cfg2.Spill.AutoSpillValue() {
		t.Error("auto_spill must default to true when absent within an enabled spill")
	}
}

// M6c-2: spill.enabled: true is the single switch that turns the whole
// capability on — the four spill_* tools must also become whitelisted
func TestLoadSpillEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spill:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// M6d-2: interact is off by default (D10), sensitive_tools is empty (no
// gating), and the interact_* tools must not be whitelisted while disabled
// (dispatch-m6d-2 §2).
func TestLoadInteractDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Interact.Enabled) {
		t.Error("Interact must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if len(cfg.Interact.SensitiveTools) != 0 {
		t.Errorf("interact.sensitive_tools = %v, want empty (no gating by default)", cfg.Interact.SensitiveTools)
	}
	for _, name := range interactToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when interact ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6d-2: an explicit interact section is honored (enabled, sensitive_tools),
// while an explicit enabled:false leaves the default whitelist untouched and an
// empty sensitive_tools stays empty (D10, dispatch-m6d-2 §2).
func TestLoadInteractParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("interact:\n  enabled: true\n  sensitive_tools: [bash, job_start]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Interact.Enabled) {
		t.Error("interact.enabled should be true")
	}
	if len(cfg.Interact.SensitiveTools) != 2 || cfg.Interact.SensitiveTools[0] != "bash" || cfg.Interact.SensitiveTools[1] != "job_start" {
		t.Errorf("interact.sensitive_tools = %v, want [bash job_start]", cfg.Interact.SensitiveTools)
	}

	// Explicit enabled:false and an empty sensitive_tools stay verbatim.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("interact:\n  enabled: false\n  sensitive_tools: []\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Interact.Enabled) {
		t.Error("interact.enabled = true, want false (explicitly disabled)")
	}
	if len(cfg2.Interact.SensitiveTools) != 0 {
		t.Errorf("interact.sensitive_tools = %v, want empty", cfg2.Interact.SensitiveTools)
	}
	for _, name := range interactToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when interact explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M6d-2: interact.enabled: true is the single switch that turns the whole
// capability on — the two interact_* tools must also become whitelisted
func TestLoadInteractEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("interact:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range interactToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after interact.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M6e-2: an absent code section means the capability is off by default (D10),
// the sandbox timeout defaults to 30s, the per-stream output cap to 65536,
// sandbox_dir stays empty (provider default <project>/.sandbox), allow_network
// stays false (declarative no-network boundary), and run_code is not
// whitelisted while disabled (dispatch-m6e-2 §2).
func TestLoadCodeDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Code.Enabled) {
		t.Error("Code must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Code.Timeout.Duration != DefaultCodeTimeout {
		t.Errorf("code.timeout = %v, want default %v", cfg.Code.Timeout.Duration, DefaultCodeTimeout)
	}
	if cfg.Code.MaxOutput != DefaultCodeMaxOutput {
		t.Errorf("code.max_output = %d, want default %d", cfg.Code.MaxOutput, DefaultCodeMaxOutput)
	}
	if cfg.Code.SandboxDir != "" {
		t.Errorf("code.sandbox_dir = %q, want empty (provider default <project>/.sandbox)", cfg.Code.SandboxDir)
	}
	if cfg.Code.AllowNetwork {
		t.Error("code.allow_network must default to false (declarative no-network boundary)")
	}
	for _, name := range codeToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when code ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6e-2: an explicit code section is honored (enabled, timeout, max_output,
// sandbox_dir, allow_network), while a non-positive timeout/max_output fall
// back to their defaults (校验非负: a negative configured value never survives).
func TestLoadCodeParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("code:\n  enabled: true\n  timeout: 5s\n  max_output: 4096\n  sandbox_dir: C:\\sandbox\n  allow_network: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Code.Enabled) {
		t.Error("code.enabled should be true")
	}
	if cfg.Code.Timeout.Duration != 5*time.Second {
		t.Errorf("code.timeout = %v, want 5s", cfg.Code.Timeout.Duration)
	}
	if cfg.Code.MaxOutput != 4096 {
		t.Errorf("code.max_output = %d, want 4096", cfg.Code.MaxOutput)
	}
	if cfg.Code.SandboxDir != `C:\sandbox` {
		t.Errorf("code.sandbox_dir = %q, want C:\\sandbox", cfg.Code.SandboxDir)
	}
	if !cfg.Code.AllowNetwork {
		t.Error("code.allow_network = false, want true (explicitly enabled)")
	}

	// Non-positive (including negative) bounds fall back to the defaults.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("code:\n  enabled: true\n  timeout: -1s\n  max_output: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Code.Timeout.Duration != DefaultCodeTimeout {
		t.Errorf("code.timeout -1s = %v, want default %v", cfg2.Code.Timeout.Duration, DefaultCodeTimeout)
	}
	if cfg2.Code.MaxOutput != DefaultCodeMaxOutput {
		t.Errorf("code.max_output 0 = %d, want default %d", cfg2.Code.MaxOutput, DefaultCodeMaxOutput)
	}
}

// M6e-2: code.enabled: true is the single switch that turns the whole
// capability on — the run_code tool must also become whitelisted
// interact).
func TestLoadCodeEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("code:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range codeToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after code.enabled", cfg.Tools.Enabled, name)
		}
	}

	// Explicit enabled:false leaves the default whitelist untouched.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("code:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Code.Enabled) {
		t.Error("code.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range codeToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when code explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M6f-2: an absent mcp section means the capability is off by default (D10),
// with no servers, and the mcp_* tools are not whitelisted while disabled
// (dispatch-m6f-2 §2).
func TestLoadMcpDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Mcp.Enabled) {
		t.Error("Mcp must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if len(cfg.Mcp.Servers) != 0 {
		t.Errorf("mcp.servers = %v, want empty", cfg.Mcp.Servers)
	}
	for _, name := range mcpToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when mcp ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6f-2: an explicit mcp section is honored (enabled, servers with
// name/cmd/args), while an explicit enabled:false leaves the default whitelist
// untouched (D10, dispatch-m6f-2 §2).
func TestLoadMcpParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "mcp:\n  enabled: true\n  servers:\n    - name: filesystem\n      cmd: npx\n      args: [\"-y\", \"@modelcontextprotocol/server-filesystem\", \".\"]\n    - name: echo\n      cmd: echo-server\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Mcp.Enabled) {
		t.Error("mcp.enabled should be true")
	}
	if len(cfg.Mcp.Servers) != 2 {
		t.Fatalf("mcp.servers = %v, want 2 entries", cfg.Mcp.Servers)
	}
	fs := cfg.Mcp.Servers[0]
	if fs.Name != "filesystem" || fs.Cmd != "npx" {
		t.Errorf("mcp.servers[0] = %+v, want name filesystem / cmd npx", fs)
	}
	if len(fs.Args) != 3 || fs.Args[0] != "-y" || fs.Args[1] != "@modelcontextprotocol/server-filesystem" || fs.Args[2] != "." {
		t.Errorf("mcp.servers[0].args = %v, want the npx args", fs.Args)
	}
	ec := cfg.Mcp.Servers[1]
	if ec.Name != "echo" || ec.Cmd != "echo-server" || len(ec.Args) != 0 {
		t.Errorf("mcp.servers[1] = %+v, want name echo / cmd echo-server / no args", ec)
	}

	// Explicit enabled:false leaves the default whitelist untouched.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("mcp:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Mcp.Enabled) {
		t.Error("mcp.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range mcpToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when mcp explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M6f-2: mcp.enabled: true is the single switch that turns the whole
// capability on — the mcp_list and mcp_call tools must also become whitelisted
// interact/code).
func TestLoadMcpEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mcp:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range mcpToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after mcp.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M6f-3: an absent fs section means the capability is off by default (D10)
// with an empty root (the FileService constructor resolves the default
// <project>), and no fs_* tool is whitelisted while disabled
// (dispatch-m6f-3 §3).
func TestLoadFsDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Fs.Enabled) {
		t.Error("Fs must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Fs.Root != "" {
		t.Errorf("fs.root = %q, want empty (default <project>)", cfg.Fs.Root)
	}
	for _, name := range fsToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when fs ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M6f-3: an explicit fs section is honored (enabled, root), while an explicit
// enabled:false leaves the default whitelist untouched (D10, dispatch-m6f-3
// §3).
func TestLoadFsParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("fs:\n  enabled: true\n  root: C:\\workspace\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Fs.Enabled) {
		t.Error("fs.enabled should be true")
	}
	if cfg.Fs.Root != `C:\workspace` {
		t.Errorf("fs.root = %q, want C:\\workspace", cfg.Fs.Root)
	}

	// Explicit enabled:false leaves the default whitelist untouched.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("fs:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Fs.Enabled) {
		t.Error("fs.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range fsToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when fs explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M6f-3: fs.enabled: true is the single switch that turns the whole
// capability on — the three fs_* tools must also become whitelisted
// interact/code/mcp).
func TestLoadFsEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("fs:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range fsToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after fs.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// D-GAP-1: an absent fs_search section means the capability is off by default
// (D10) and the fs_search tool is not whitelisted.
func TestLoadFsSearchDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.FsSearch.Enabled) {
		t.Error("FsSearch must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	for _, name := range fsSearchToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when fs_search ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// D-GAP-1: an explicit fs_search.enabled: true is honored, while an explicit
// enabled:false leaves the default whitelist untouched (D10).
func TestLoadFsSearchParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("fs_search:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.FsSearch.Enabled) {
		t.Error("fs_search.enabled should be true")
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("fs_search:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.FsSearch.Enabled) {
		t.Error("fs_search.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range fsSearchToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when fs_search explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// D-GAP-1: fs_search.enabled: true is the single switch that turns the whole
// capability on — the fs_search tool must also become whitelisted.
func TestLoadFsSearchEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("fs_search:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range fsSearchToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after fs_search.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// GAP-2: an absent ralph section means the capability is off by default (D10)
// and the ralph tool is not whitelisted.
func TestLoadRalphDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Ralph.Enabled) {
		t.Error("Ralph must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	for _, name := range ralphToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when ralph ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// GAP-2: an explicit ralph.enabled: true is honored, while an explicit
// enabled:false leaves the default whitelist untouched (D10).
func TestLoadRalphParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ralph:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Ralph.Enabled) {
		t.Error("ralph.enabled should be true")
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("ralph:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Ralph.Enabled) {
		t.Error("ralph.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range ralphToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when ralph explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// GAP-2: ralph.enabled: true is the single switch that turns the whole
// capability on — the ralph tool must also become whitelisted.
func TestLoadRalphEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ralph:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range ralphToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after ralph.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// GAP-3: an absent workflow section means the capability is off by default
// (D10) with max_concurrent at its default, and workflow_run is not
// whitelisted.
func TestLoadWorkflowDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Workflow.Enabled) {
		t.Error("Workflow must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Workflow.MaxConcurrent != DefaultWorkflowMaxConcurrent {
		t.Errorf("workflow.max_concurrent = %d, want %d", cfg.Workflow.MaxConcurrent, DefaultWorkflowMaxConcurrent)
	}
	for _, name := range workflowToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when workflow ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// GAP-3: an explicit workflow.enabled: true is honored (and workflow_run is
// whitelisted), while an explicit enabled:false leaves the default whitelist
// untouched (D10); a non-positive max_concurrent is clamped to the default.
func TestLoadWorkflowParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("workflow:\n  enabled: true\n  max_concurrent: 2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Workflow.Enabled) {
		t.Error("workflow.enabled should be true")
	}
	if cfg.Workflow.MaxConcurrent != 2 {
		t.Errorf("workflow.max_concurrent = %d, want 2", cfg.Workflow.MaxConcurrent)
	}
	for _, name := range workflowToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after workflow.enabled", cfg.Tools.Enabled, name)
		}
	}

	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("workflow:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Workflow.Enabled) {
		t.Error("workflow.enabled = true, want false (explicitly disabled)")
	}
	if cfg2.Workflow.MaxConcurrent != DefaultWorkflowMaxConcurrent {
		t.Errorf("workflow.max_concurrent = %d, want %d when absent", cfg2.Workflow.MaxConcurrent, DefaultWorkflowMaxConcurrent)
	}
	for _, name := range workflowToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when workflow explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}

	// A non-positive max_concurrent is clamped to the default (校验非负).
	path3 := filepath.Join(t.TempDir(), "config3.yaml")
	if err := os.WriteFile(path3, []byte("workflow:\n  enabled: true\n  max_concurrent: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg3, err := Load(path3)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg3.Workflow.MaxConcurrent != DefaultWorkflowMaxConcurrent {
		t.Errorf("workflow.max_concurrent = %d, want %d (clamped)", cfg3.Workflow.MaxConcurrent, DefaultWorkflowMaxConcurrent)
	}
}

// GAP-3: workflow.enabled: true is the single switch that turns the whole
// capability on — workflow_run must also become whitelisted.
func TestLoadWorkflowEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("workflow:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range workflowToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after workflow.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M7-2: an absent web section means the capability is off by default (D10)
// with every field at its default, and no web_* tool is whitelisted while
// disabled (dispatch-m7-2 §5).
func TestLoadWebDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Web.Enabled) {
		t.Error("Web must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Web.SearchMaxResults != DefaultWebSearchMaxResults {
		t.Errorf("web.search_max_results = %d, want %d", cfg.Web.SearchMaxResults, DefaultWebSearchMaxResults)
	}
	if cfg.Web.SearchMaxQueries != DefaultWebSearchMaxQueries {
		t.Errorf("web.search_max_queries = %d, want %d", cfg.Web.SearchMaxQueries, DefaultWebSearchMaxQueries)
	}
	if cfg.Web.SearchTimeoutMs != DefaultWebSearchTimeoutMs {
		t.Errorf("web.search_timeout_ms = %d, want %d", cfg.Web.SearchTimeoutMs, DefaultWebSearchTimeoutMs)
	}
	if cfg.Web.FetchTimeoutMs != DefaultWebFetchTimeoutMs {
		t.Errorf("web.fetch_timeout_ms = %d, want %d", cfg.Web.FetchTimeoutMs, DefaultWebFetchTimeoutMs)
	}
	if cfg.Web.FetchMaxOutputChars != DefaultWebFetchMaxOutputChars {
		t.Errorf("web.fetch_max_output_chars = %d, want %d", cfg.Web.FetchMaxOutputChars, DefaultWebFetchMaxOutputChars)
	}
	if cfg.Web.FetchMaxResponseBytes != DefaultWebFetchMaxResponseBytes {
		t.Errorf("web.fetch_max_response_bytes = %d, want %d", cfg.Web.FetchMaxResponseBytes, DefaultWebFetchMaxResponseBytes)
	}
	if cfg.Web.FetchMaxURLBytes != DefaultWebFetchMaxURLBytes {
		t.Errorf("web.fetch_max_url_bytes = %d, want %d", cfg.Web.FetchMaxURLBytes, DefaultWebFetchMaxURLBytes)
	}
	if cfg.Web.FetchMaxRedirects != DefaultWebFetchMaxRedirects {
		t.Errorf("web.fetch_max_redirects = %d, want %d", cfg.Web.FetchMaxRedirects, DefaultWebFetchMaxRedirects)
	}
	if cfg.Web.FetchUserAgent != DefaultWebFetchUserAgent {
		t.Errorf("web.fetch_user_agent = %q, want %q", cfg.Web.FetchUserAgent, DefaultWebFetchUserAgent)
	}
	if cfg.Web.DeepSeek.BaseURL != DefaultWebDeepSeekBaseURL ||
		cfg.Web.DeepSeek.Model != DefaultWebDeepSeekModel ||
		cfg.Web.DeepSeek.APIVersion != DefaultWebDeepSeekAPIVersion ||
		cfg.Web.DeepSeek.MaxTokens != DefaultWebDeepSeekMaxTokens ||
		cfg.Web.DeepSeek.MaxUses != DefaultWebDeepSeekMaxUses {
		t.Errorf("web.deepseek defaults not applied: %+v", cfg.Web.DeepSeek)
	}
	for _, name := range webToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when web ENABLED by default", cfg.Tools.Enabled, name)
		}
	}
}

// M7-2: an explicit web section is honored (enabled and every field), while an
// explicit enabled:false leaves the default whitelist untouched (D10,
// dispatch-m7-2 §5).
func TestLoadWebParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `web:
  enabled: true
  search_max_results: 12
  search_max_queries: 6
  search_timeout_ms: 60000
  fetch_timeout_ms: 15000
  fetch_max_output_chars: 50000
  fetch_max_response_bytes: 1048576
  fetch_max_url_bytes: 1024
  fetch_max_redirects: 3
  fetch_user_agent: custom-agent/2.0
  deepseek:
    base_url: https://custom.example/anthropic
    model: custom-model
    api_version: 2024-01-01
    max_tokens: 2048
    max_uses: 10
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Web.Enabled) {
		t.Error("web.enabled should be true")
	}
	if cfg.Web.SearchMaxResults != 12 || cfg.Web.SearchMaxQueries != 6 || cfg.Web.SearchTimeoutMs != 60000 {
		t.Errorf("search fields = %d/%d/%d", cfg.Web.SearchMaxResults, cfg.Web.SearchMaxQueries, cfg.Web.SearchTimeoutMs)
	}
	if cfg.Web.FetchTimeoutMs != 15000 || cfg.Web.FetchMaxOutputChars != 50000 ||
		cfg.Web.FetchMaxResponseBytes != 1048576 || cfg.Web.FetchMaxURLBytes != 1024 ||
		cfg.Web.FetchMaxRedirects != 3 || cfg.Web.FetchUserAgent != "custom-agent/2.0" {
		t.Errorf("fetch fields = %+v", cfg.Web)
	}
	if cfg.Web.DeepSeek.BaseURL != "https://custom.example/anthropic" ||
		cfg.Web.DeepSeek.Model != "custom-model" ||
		cfg.Web.DeepSeek.APIVersion != "2024-01-01" ||
		cfg.Web.DeepSeek.MaxTokens != 2048 || cfg.Web.DeepSeek.MaxUses != 10 {
		t.Errorf("deepseek fields = %+v", cfg.Web.DeepSeek)
	}

	// Explicit enabled:false leaves the default whitelist untouched.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("web:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg2.Web.Enabled) {
		t.Error("web.enabled = true, want false (explicitly disabled)")
	}
	for _, name := range webToolNames {
		if contains(cfg2.Tools.Enabled, name) {
			t.Errorf("whitelist %v must not contain %q when web explicitly disabled", cfg2.Tools.Enabled, name)
		}
	}
}

// M7-2: web.enabled: true is the single switch that turns the whole capability
// on — the two web_* tools must also become whitelisted (dispatch-m7-2 §5,
func TestLoadWebEnabledAppendsToolsToWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range webToolNames {
		if !contains(cfg.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after web.enabled", cfg.Tools.Enabled, name)
		}
	}
}

// M7-2: a non-positive search/query cap falls back to the default (校验非负:
// a negative configured value can never survive to the wiring).
func TestLoadWebNonPositiveCapsFallBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  enabled: true\n  search_max_results: 0\n  search_max_queries: -1\n  fetch_max_redirects: -2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.SearchMaxResults != DefaultWebSearchMaxResults {
		t.Errorf("search_max_results 0 = %d, want default %d", cfg.Web.SearchMaxResults, DefaultWebSearchMaxResults)
	}
	if cfg.Web.SearchMaxQueries != DefaultWebSearchMaxQueries {
		t.Errorf("search_max_queries -1 = %d, want default %d", cfg.Web.SearchMaxQueries, DefaultWebSearchMaxQueries)
	}
	if cfg.Web.FetchMaxRedirects != DefaultWebFetchMaxRedirects {
		t.Errorf("fetch_max_redirects -2 = %d, want default %d", cfg.Web.FetchMaxRedirects, DefaultWebFetchMaxRedirects)
	}
}

// M8-2: an absent llm section means the default provider is deepseek
// (regression: behavior identical to before M8-2), the openai base_url/model
// fall back to https://api.openai.com/v1 / gpt-4o-mini, the anthropic
// placeholders fall back to their defaults, and the top-level model/base_url
// stay as the deepseek default configuration (dispatch-m8-2 §5/§7).
func TestLoadLLMDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != DefaultLLMProvider {
		t.Errorf("llm.provider = %q, want default %q", cfg.LLM.Provider, DefaultLLMProvider)
	}
	if cfg.LLM.OpenAI.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("llm.openai.base_url = %q, want %q", cfg.LLM.OpenAI.BaseURL, DefaultOpenAIBaseURL)
	}
	if cfg.LLM.OpenAI.Model != DefaultOpenAIModel {
		t.Errorf("llm.openai.model = %q, want %q", cfg.LLM.OpenAI.Model, DefaultOpenAIModel)
	}
	if cfg.LLM.Anthropic.BaseURL != DefaultAnthropicBaseURL {
		t.Errorf("llm.anthropic.base_url = %q, want %q", cfg.LLM.Anthropic.BaseURL, DefaultAnthropicBaseURL)
	}
	if cfg.LLM.Anthropic.Model != DefaultAnthropicModel {
		t.Errorf("llm.anthropic.model = %q, want %q", cfg.LLM.Anthropic.Model, DefaultAnthropicModel)
	}
	// Top-level deepseek default configuration stays put (not migrated).
	if cfg.Model != DefaultModel {
		t.Errorf("top-level model = %q, want %q", cfg.Model, DefaultModel)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Errorf("top-level base_url = %q, want %q", cfg.BaseURL, DefaultBaseURL)
	}
}

// M8-2: an explicit llm section is honored — provider selection, openai
// base_url/model, anthropic base_url/model — while fields left absent still
// fall back to their defaults (dispatch-m8-2 §5).
func TestLoadLLMParsesSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `llm:
  provider: openai
  openai:
    base_url: https://custom.example/v1
    model: custom-model
  anthropic:
    base_url: https://custom-anthropic.example/v1
    model: custom-claude
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != "openai" {
		t.Errorf("llm.provider = %q, want openai", cfg.LLM.Provider)
	}
	if cfg.LLM.OpenAI.BaseURL != "https://custom.example/v1" || cfg.LLM.OpenAI.Model != "custom-model" {
		t.Errorf("llm.openai = %+v, want the explicit values", cfg.LLM.OpenAI)
	}
	if cfg.LLM.Anthropic.BaseURL != "https://custom-anthropic.example/v1" || cfg.LLM.Anthropic.Model != "custom-claude" {
		t.Errorf("llm.anthropic = %+v, want the explicit values", cfg.LLM.Anthropic)
	}
	// An empty provider field still defaults to deepseek.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	if err := os.WriteFile(path2, []byte("llm:\n  openai:\n    model: gpt-5\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.LLM.Provider != DefaultLLMProvider {
		t.Errorf("absent llm.provider = %q, want default %q", cfg2.LLM.Provider, DefaultLLMProvider)
	}
	if cfg2.LLM.OpenAI.Model != "gpt-5" {
		t.Errorf("llm.openai.model = %q, want gpt-5", cfg2.LLM.OpenAI.Model)
	}
	if cfg2.LLM.OpenAI.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("absent llm.openai.base_url = %q, want default %q", cfg2.LLM.OpenAI.BaseURL, DefaultOpenAIBaseURL)
	}
}

// M8-3 + 用户拍板「图片附件默认打开」(2026-08-20): an absent multimodal section
// means the capability is ON by default (覆盖原 D10 默认关), model_input_modalities
// defaults to "text", and max_image_bytes defaults to 10MiB (dispatch-m8-3 §3).
func TestLoadMultimodalDefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !*cfg.LLM.Multimodal.Enabled {
		t.Error("llm.multimodal must be enabled by default (图片附件默认打开, 用户 2026-08-20 拍板)")
	}
	if cfg.LLM.ModelInputModalities != DefaultModelInputModalities {
		t.Errorf("llm.model_input_modalities = %q, want default %q",
			cfg.LLM.ModelInputModalities, DefaultModelInputModalities)
	}
	if cfg.LLM.Multimodal.MaxImageBytes != DefaultMultimodalMaxImageBytes {
		t.Errorf("llm.multimodal.max_image_bytes = %d, want default %d",
			cfg.LLM.Multimodal.MaxImageBytes, DefaultMultimodalMaxImageBytes)
	}
	if cfg.LLM.Multimodal.MaxRequestImageBytes != DefaultMultimodalMaxRequestImageBytes {
		t.Errorf("llm.multimodal.max_request_image_bytes = %d, want default %d",
			cfg.LLM.Multimodal.MaxRequestImageBytes, DefaultMultimodalMaxRequestImageBytes)
	}
}

// M8-3: an explicit multimodal section is honored (enabled, model_input_
// modalities, max_image_bytes), while a non-positive max_image_bytes and an
// empty model_input_modalities fall back to their defaults (校验非负, dispatch
// -m8-3 §3).
func TestLoadMultimodalParsesSectionAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `llm:
  model_input_modalities: text,image
  multimodal:
    enabled: true
    max_image_bytes: 2097152
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !*cfg.LLM.Multimodal.Enabled {
		t.Error("llm.multimodal.enabled should be true")
	}
	if cfg.LLM.ModelInputModalities != "text,image" {
		t.Errorf("llm.model_input_modalities = %q, want text,image", cfg.LLM.ModelInputModalities)
	}
	if cfg.LLM.Multimodal.MaxImageBytes != 2097152 {
		t.Errorf("llm.multimodal.max_image_bytes = %d, want 2097152", cfg.LLM.Multimodal.MaxImageBytes)
	}

	// Non-positive max_image_bytes / empty modalities fall back to defaults.
	path2 := filepath.Join(t.TempDir(), "config2.yaml")
	content2 := `llm:
  multimodal:
    enabled: true
    max_image_bytes: 0
`
	if err := os.WriteFile(path2, []byte(content2), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !*cfg2.LLM.Multimodal.Enabled {
		t.Error("llm.multimodal.enabled should stay true")
	}
	if cfg2.LLM.ModelInputModalities != DefaultModelInputModalities {
		t.Errorf("absent llm.model_input_modalities = %q, want default %q",
			cfg2.LLM.ModelInputModalities, DefaultModelInputModalities)
	}
	if cfg2.LLM.Multimodal.MaxImageBytes != DefaultMultimodalMaxImageBytes {
		t.Errorf("llm.multimodal.max_image_bytes 0 = %d, want default %d",
			cfg2.LLM.Multimodal.MaxImageBytes, DefaultMultimodalMaxImageBytes)
	}
}

// M8-3: an explicit enabled:false keeps multimodal off even when other fields
// are set (D10 gate stays closed).
func TestLoadMultimodalExplicitDisabledStaysOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `llm:
  multimodal:
    enabled: false
    max_image_bytes: 512
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *cfg.LLM.Multimodal.Enabled {
		t.Error("llm.multimodal.enabled = true, want false (explicitly disabled)")
	}
	if cfg.LLM.Multimodal.MaxImageBytes != 512 {
		t.Errorf("llm.multimodal.max_image_bytes = %d, want 512 (explicit value kept)", cfg.LLM.Multimodal.MaxImageBytes)
	}
}

// M8-3b: llm.multimodal.max_request_image_bytes defaults to 20MiB and parses
// from YAML; a non-positive value falls back to the default (dispatch-m8-3b
// §6/§7).
func TestLoadMultimodalMaxRequestImageBytes(t *testing.T) {
	// Default when absent.
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Multimodal.MaxRequestImageBytes != DefaultMultimodalMaxRequestImageBytes {
		t.Errorf("llm.multimodal.max_request_image_bytes default = %d, want %d",
			cfg.LLM.Multimodal.MaxRequestImageBytes, DefaultMultimodalMaxRequestImageBytes)
	}

	// Explicit value honored.
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `llm:
  multimodal:
    max_request_image_bytes: 1048576
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.LLM.Multimodal.MaxRequestImageBytes != 1048576 {
		t.Errorf("llm.multimodal.max_request_image_bytes = %d, want 1048576",
			cfg2.LLM.Multimodal.MaxRequestImageBytes)
	}

	// Non-positive → default (校验非负).
	path3 := filepath.Join(t.TempDir(), "config3.yaml")
	if err := os.WriteFile(path3, []byte("llm:\n  multimodal:\n    max_request_image_bytes: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg3, err := Load(path3)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg3.LLM.Multimodal.MaxRequestImageBytes != DefaultMultimodalMaxRequestImageBytes {
		t.Errorf("non-positive max_request_image_bytes = %d, want default %d",
			cfg3.LLM.Multimodal.MaxRequestImageBytes, DefaultMultimodalMaxRequestImageBytes)
	}
}

// M9: a zero-value TerminalConfig falls back to the design defaults
// (scrollback caps, read pacing, single-active-owner concurrency), and
// terminal stays off by default (D10, bool 零值即关).
func TestTerminalDefaults(t *testing.T) {
	var cfg Config
	applyDefaults(&cfg)
	if !Enabled(cfg.Terminal.Enabled) {
		t.Error("Terminal must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Terminal.ScrollbackMaxBytes != DefaultTerminalScrollbackMaxBytes {
		t.Errorf("terminal.scrollback_max_bytes = %d, want default %d",
			cfg.Terminal.ScrollbackMaxBytes, DefaultTerminalScrollbackMaxBytes)
	}
	if cfg.Terminal.ScrollbackLines != DefaultTerminalScrollbackLines {
		t.Errorf("terminal.scrollback_lines = %d, want default %d",
			cfg.Terminal.ScrollbackLines, DefaultTerminalScrollbackLines)
	}
	if cfg.Terminal.ReadIdleMS != DefaultTerminalReadIdleMS {
		t.Errorf("terminal.read_idle_ms = %d, want default %d",
			cfg.Terminal.ReadIdleMS, DefaultTerminalReadIdleMS)
	}
	if cfg.Terminal.ReadTimeoutMS != DefaultTerminalReadTimeoutMS {
		t.Errorf("terminal.read_timeout_ms = %d, want default %d",
			cfg.Terminal.ReadTimeoutMS, DefaultTerminalReadTimeoutMS)
	}
	if cfg.Terminal.MaxConcurrentSessions != DefaultTerminalMaxConcurrent {
		t.Errorf("terminal.max_concurrent_sessions = %d, want default %d",
			cfg.Terminal.MaxConcurrentSessions, DefaultTerminalMaxConcurrent)
	}
}

// M9: terminal.enabled is the single switch that whitelists the five
// terminal_* consumer tools; with terminal disabled none of them is
// whitelisted (D10, dispatch-m9-2 §2).
func TestTerminalEnabledWhitelists(t *testing.T) {
	// Default (terminal disabled): no terminal tool in the whitelist.
	var disabled Config
	applyDefaults(&disabled)
	for _, name := range terminalToolNames {
		if !contains(disabled.Tools.Enabled, name) {
			t.Errorf("whitelist %v must contain %q when terminal ENABLED by default", disabled.Tools.Enabled, name)
		}
	}

	// Enabled: all five terminal_* tools enter the whitelist.
	var enabled Config
	enabled.Terminal.Enabled = Bool(true)
	applyDefaults(&enabled)
	for _, name := range terminalToolNames {
		if !contains(enabled.Tools.Enabled, name) {
			t.Errorf("whitelist %v lacks %q after terminal.enabled", enabled.Tools.Enabled, name)
		}
	}
}

// M-Eval: eval is off by default (D10), manual_fallback defaults to true
// (absent ⇒ true via the pointer default — applyDefaults guarantees a non-nil
// pointer), and the history cap defaults to 100 (dispatch-eval-3a §1).
func TestEvalDefaults(t *testing.T) {
	var cfg Config
	applyDefaults(&cfg)
	if !Enabled(cfg.Eval.Enabled) {
		t.Error("Eval must be ENABLED by default (dsh 对齐; D10 default-off is now opt-out)")
	}
	if cfg.Eval.ManualFallback == nil {
		t.Fatal("eval.manual_fallback must be non-nil after applyDefaults")
	}
	if !*cfg.Eval.ManualFallback {
		t.Error("eval.manual_fallback must default to true (absent ⇒ true)")
	}
	if cfg.Eval.MaxRecords != DefaultEvalMaxRecords {
		t.Errorf("eval.max_records = %d, want default %d", cfg.Eval.MaxRecords, DefaultEvalMaxRecords)
	}
}

// M-Eval: eval.enabled is the single switch that whitelists the three eval_*
// consumer tools; with eval disabled none of them is whitelisted (D10,
// dispatch-eval-3a §1).
func TestEvalEnabledWhitelists(t *testing.T) {
	// Default (eval disabled): no eval tool in the whitelist.
	var disabled Config
	applyDefaults(&disabled)

	// Enabled: all three eval_* tools enter the whitelist.
	var enabled Config
	enabled.Eval.Enabled = Bool(true)
	applyDefaults(&enabled)
}

// M-Eval: an explicit manual_fallback: false survives applyDefaults — the
// pointer default only fills absent fields, so an explicit false means the
// LLM-undecided verdict fails closed instead of routing to a human
// (dispatch-eval-3a §1).
func TestEvalManualFallbackExplicitFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("eval:\n  enabled: true\n  manual_fallback: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Eval.ManualFallback == nil {
		t.Fatal("eval.manual_fallback must be non-nil after Load")
	}
	if *cfg.Eval.ManualFallback {
		t.Error("eval.manual_fallback = true, want false (explicitly disabled)")
	}
}

// D-MODE-1: a zero-value Config (or a Load of a missing file) defaults the
// mode to "standard" — the existing default behavior is unchanged (D10).
func TestModeDefault(t *testing.T) {
	var cfg Config
	applyDefaults(&cfg)
	if cfg.Mode != DefaultMode {
		t.Errorf("mode = %q, want default %q", cfg.Mode, DefaultMode)
	}

	cfg2, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Mode != DefaultMode {
		t.Errorf("mode (missing file) = %q, want default %q", cfg2.Mode, DefaultMode)
	}
}

// D-MODE-2: mode: minimal is preset-first — only the persistent shell and
// file editing survive (Terminal.Enabled / Fs.Enabled true), every other
// capability cap is forced false, and the execution whitelist is reset to
// exactly the minimal set (M1 只读 + terminal_* + fs_*).
func TestModeMinimalWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: minimal\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Terminal.Enabled) {
		t.Error("minimal: terminal must be enabled")
	}
	if !Enabled(cfg.Fs.Enabled) {
		t.Error("minimal: fs must be enabled")
	}
	for name, enabled := range modeCapStates(cfg) {
		if name == "terminal" || name == "fs" {
			continue // these two are the minimal preset's survivors
		}
		if enabled {
			t.Errorf("minimal: %s must be disabled (preset-first, D-MODE-2)", name)
		}
	}
	if cfg.Tools.RunCommand.Enabled {
		t.Error("minimal: tools.run_command must be disabled")
	}
	if !reflect.DeepEqual(cfg.Tools.Enabled, minimalEnabledTools) {
		t.Errorf("minimal whitelist = %v, want exactly %v", cfg.Tools.Enabled, minimalEnabledTools)
	}
}

// D-MODE-6: minimal is preset-first — user-explicitly-enabled capabilities
// minimal set.
func TestModeMinimalPresetFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "mode: minimal\nweb:\n  enabled: true\nfs_search:\n  enabled: true\nralph:\n  enabled: true\nworkflow:\n  enabled: true\ntools:\n  enabled: [web_search]\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if Enabled(cfg.Web.Enabled) {
		t.Error("minimal must override an explicit web.enabled: true (preset-first, D-MODE-6)")
	}
	if Enabled(cfg.FsSearch.Enabled) {
		t.Error("minimal must override an explicit fs_search.enabled: true (D-MODE-2 不含搜索)")
	}
	if Enabled(cfg.Ralph.Enabled) {
		t.Error("minimal must override an explicit ralph.enabled: true (D-MODE-2 不含 fresh-agent 循环)")
	}
	if Enabled(cfg.Workflow.Enabled) {
		t.Error("minimal must override an explicit workflow.enabled: true (D-MODE-2 不含 workflow 编排)")
	}
	if !Enabled(cfg.Terminal.Enabled) || !Enabled(cfg.Fs.Enabled) {
		t.Error("minimal must keep terminal and fs enabled despite the explicit overrides")
	}
	if !reflect.DeepEqual(cfg.Tools.Enabled, minimalEnabledTools) {
		t.Errorf("minimal whitelist = %v, want exactly %v (explicit tools.enabled overridden)",
			cfg.Tools.Enabled, minimalEnabledTools)
	}
}

// survives and every remaining cap equals a config with no mode set (default
// standard, D10).
func TestModeStandardUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: standard\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfgDefault, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if !reflect.DeepEqual(modeCapStates(cfg), modeCapStates(cfgDefault)) {
		t.Errorf("standard changed cap states vs no-mode: got %v, want %v",
			modeCapStates(cfg), modeCapStates(cfgDefault))
	}
}

// terminal.enabled survive exactly as in standard (code only adds the Code
// Mode prompt segment in the composition root).
func TestModeCodeKeepsCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: code\nterminal:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !Enabled(cfg.Terminal.Enabled) {
		t.Error("code must keep terminal.enabled: true (like standard)")
	}
	pathStd := filepath.Join(t.TempDir(), "config-std.yaml")
	if err := os.WriteFile(pathStd, []byte("terminal:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfgStd, err := Load(pathStd)
	if err != nil {
		t.Fatalf("Load standard: %v", err)
	}
	if !reflect.DeepEqual(modeCapStates(cfg), modeCapStates(cfgStd)) {
		t.Errorf("code changed cap states vs standard: got %v, want %v",
			modeCapStates(cfg), modeCapStates(cfgStd))
	}
}

// D-MODE-1: an unknown mode value fails closed at Load — never silently fall
// back (like the LLM provider route).
func TestModeInvalidFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: turbo\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for an unknown mode")
	} else if !strings.Contains(err.Error(), "invalid mode") {
		t.Errorf("error = %v, want it to contain \"invalid mode\"", err)
	}
}

// D-MODE-2: minimal exposes only the platform shell and DSH's editor seam.
func TestModeMinimalTerminalToolsRegistered(t *testing.T) {
	var cfg Config
	cfg.Mode = ModeMinimal
	applyDefaults(&cfg)
	want := []string{platformShellToolName(), "str_replace_editor"}
	if !reflect.DeepEqual(cfg.Tools.Enabled, want) {
		t.Fatalf("minimal whitelist = %v, want exactly %v", cfg.Tools.Enabled, want)
	}
}

// TestWebServerDefaults verifies the M10a defaults (ADR
// 2026-08-20-m10-web-portal.md D-WEB-7): addr defaults to the local-only
// personal portal and minimal mode disables the portal.
func TestWebServerDefaults(t *testing.T) {
	var cfg Config
	cfg.Mode = ModeStandard
	applyDefaults(&cfg)
	if cfg.WebServer.Addr != "127.0.0.1:8080" {
		t.Errorf("web_server.addr = %q, want default 127.0.0.1:8080", cfg.WebServer.Addr)
	}
	var minimal Config
	minimal.Mode = ModeMinimal
	applyDefaults(&minimal)
	if minimal.WebServer.Enabled {
		t.Error("minimal must disable web_server (D-MODE-2)")
	}
}

// modeCapStates returns every capability master switch, used to compare cap
// states across modes.
func modeCapStates(cfg Config) map[string]bool {
	return map[string]bool{
		"terminal":       Enabled(cfg.Terminal.Enabled),
		"fs":             Enabled(cfg.Fs.Enabled),
		"fs_search":      Enabled(cfg.FsSearch.Enabled),
		"ralph":          Enabled(cfg.Ralph.Enabled),
		"workflow":       Enabled(cfg.Workflow.Enabled),
		"web_server":     cfg.WebServer.Enabled,
		"jobs":           Enabled(cfg.Jobs.Enabled),
		"subagent":       Enabled(cfg.Subagent.Enabled),
		"web":            Enabled(cfg.Web.Enabled),
		"eval":           Enabled(cfg.Eval.Enabled),
		"code":           Enabled(cfg.Code.Enabled),
		"interact":       Enabled(cfg.Interact.Enabled),
		"compaction":     Enabled(cfg.Compaction.Enabled),
		"skill":          Enabled(cfg.Skill.Enabled),
		"schedule":       Enabled(cfg.Schedule.Enabled),
		"plan":           Enabled(cfg.Plan.Enabled),
		"spill":          Enabled(cfg.Spill.Enabled),
		"mcp":            Enabled(cfg.Mcp.Enabled),
		"llm.multimodal": *cfg.LLM.Multimodal.Enabled,
	}
}
