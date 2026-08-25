package tools

import (
	"path/filepath"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
)

// M3 defaults (design.md §5 / dispatch-m3). The shipped configuration always
// installs a Policy with these, so every Execute is wrapped in a deadline and
// oversized output is truncated and spilled.
const (
	// DefaultTimeout is the per-tool execute deadline applied by the Execute
	// pipeline (context.WithTimeout) unless Policy.Timeout says otherwise.
	DefaultTimeout = 30 * time.Second
	// DefaultOutputLimit is the max model-facing tool-result size in bytes.
	DefaultOutputLimit = 64 * 1024
	// DefaultRunCommandTimeout is dsh bash's fresh-process default. It is
	// separate from the 30s deadline used by ordinary tools.
	DefaultRunCommandTimeout = 120 * time.Second
	// DefaultSpillDir is where spilled output lands when Policy.SpillDir is
	// empty; the REPL overrides it to <data_dir>/spill.
	DefaultSpillDir = "data/spill"
)

// Policy is the Execute pipeline's safety policy (M3): a name whitelist, a
// per-tool deadline, an output-size cap with spill-to-disk, and the
// run_command policy. It lives entirely in the tools package — the loop never
// sees it (D4).
type Policy struct {
	// Enabled is the whitelist: only these tool names may execute. A name not
	// listed is rejected at the Execute gate (未启用 ⇒ 拒绝执行).
	Enabled []string
	// Timeout is the per-tool execute deadline. Zero means no deadline (used
	// by tests and by an explicit configuration choice); the default policy
	// uses DefaultTimeout.
	Timeout time.Duration
	// OutputLimit caps the model-facing tool result in bytes. A result larger
	// than this is truncated and the full text is spilled to SpillDir. Zero
	// means no cap (used by tests).
	OutputLimit int
	// SpillDir is where full oversized outputs are written. Empty uses
	// DefaultSpillDir.
	SpillDir string
	// RunCommand is the policy for the sole execution-class tool.
	RunCommand RunCommandPolicy
	// CodeRun is the run_code sandbox tool policy (M6e-2, ADR 决策 M6e).
	CodeRun CodeRunPolicy
}

// RunCommandPolicy is the run_command tool policy (design.md §5 / D10 落地).
type RunCommandPolicy struct {
	// Enabled mirrors tools.run_command.enabled; registration is gated on it
	// by the caller (main.go), so the tool is not even advertised when off.
	Enabled bool
	// Timeout overrides Policy.Timeout for run_command when positive.
	Timeout time.Duration
	// Workdir is the fixed working directory of every command. Empty means
	// the agent process's own working directory.
	Workdir string
}

// CodeRunPolicy is the run_code sandbox tool policy (ADR 决策 M6e /
// dispatch-m6e-2 §3). Unlike RunCommandPolicy there is no Enabled flag here —
// code.enabled gates registration at the composition root and run_code is
// whitelisted by config.applyDefaults; this carries only the outer per-tool
// deadline bound (code.timeout). The sandbox applies its own execution timeout
// internally (RunRequest.Timeout); this is the outer bound the Execute
// pipeline enforces, mirroring RunCommand.Timeout.
type CodeRunPolicy struct {
	// Timeout overrides Policy.Timeout for run_code when positive (the config
	// code.timeout value, supplied by the composition root).
	Timeout time.Duration
}

// DefaultPolicy returns the safe-by-default policy: only the read-only tools
// whitelisted, a 30s deadline, a 64KB output cap, and spill to DefaultSpillDir.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:     []string{"get_time", "read"},
		Timeout:     DefaultTimeout,
		OutputLimit: DefaultOutputLimit,
		SpillDir:    DefaultSpillDir,
		RunCommand:  RunCommandPolicy{Timeout: DefaultRunCommandTimeout},
	}
}

// PolicyFromConfig maps the normalized tools config onto a Policy. The caller
// (cmd/pa) passes the config's data_dir so spill files land under
// <data_dir>/spill (dispatch-m3).
func PolicyFromConfig(cfg config.ToolsConfig, dataDir string) Policy {
	p := DefaultPolicy()
	p.Enabled = append([]string(nil), cfg.Enabled...)
	if cfg.Timeout.Duration > 0 {
		p.Timeout = cfg.Timeout.Duration
	}
	if cfg.OutputLimit > 0 {
		p.OutputLimit = cfg.OutputLimit
	}
	p.RunCommand = RunCommandPolicy{
		Enabled: cfg.RunCommand.Enabled,
		Timeout: cfg.RunCommand.Timeout.Duration,
		Workdir: cfg.RunCommand.Workdir,
	}
	if p.RunCommand.Timeout <= 0 {
		p.RunCommand.Timeout = DefaultRunCommandTimeout
	}
	if dataDir != "" {
		p.SpillDir = filepath.Join(dataDir, "spill")
	}
	return p
}

// Allows reports whether name may execute under this policy's whitelist. An
// empty whitelist rejects everything (whitelist semantics, not allow-all).
func (p Policy) Allows(name string) bool {
	for _, n := range p.Enabled {
		if n == name {
			return true
		}
	}
	return false
}

// spillDir resolves the effective spill directory for a Policy.
func (p Policy) spillDir() string {
	if p.SpillDir != "" {
		return p.SpillDir
	}
	return DefaultSpillDir
}
