// fssearch.go — the D-GAP-1 composition-root orchestration
// (docs/dispatch-gap-1.md §5). This is where the search capability seam is
// wired into the REPL: registerFsSearch registers the grep and glob tools
// into the registry when fs_search.enabled (默认关 D10). config.applyDefaults
// already whitelisted the names when fs_search.enabled was true. The tools
// are read-only and hold no resources, so there is no deferred Close; they
// execute on the serial tool path (D5) and the loop's turn/step structure is
// untouched (D4).
package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/fssearch"
)

// registerFsSearch wires the search seam (D-GAP-1) when fs_search.enabled
// (默认关 D10): it registers grep and glob into the registry.
// config.applyDefaults already whitelisted the names when enabled. The default
// search root is the agent working directory — resolved with os.Getwd, the
// same "agent cwd" default internal/code and internal/skill use (run_command's
// empty workdir inherits it too). Read-only, no resources → no deferred Close.
func (a *app) registerFsSearch() error {
	if !config.Enabled(a.cfg.FsSearch.Enabled) {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("pa: search cwd: %w", err)
	}
	if err := a.reg.Register(fssearch.NewGrepTool(cwd)); err != nil {
		return fmt.Errorf("pa: register %s: %w", fssearch.GrepToolName, err)
	}
	if err := a.reg.Register(fssearch.NewGlobTool(cwd)); err != nil {
		return fmt.Errorf("pa: register %s: %w", fssearch.GlobToolName, err)
	}
	return nil
}
