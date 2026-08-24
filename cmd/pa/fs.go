// fs.go — the M6f-3 composition-root orchestration (dispatch-m6f-3 §5).
// This is where the safe-file-operation capability seam is wired into the
// REPL: registerFs creates the local FileService (constrained to the allowed
// root, fs.root defaulting to <project>) and registers the write / list /
// edit tools (dsh-aligned names; read is the base tool) when fs.enabled (D10),
// and wires the D3 event sink so fs/write and fs/list are appended to the
// active session log. The wiring sits entirely in the tool registration layer
// — the loop's turn/step structure is untouched (D4) — and every tool
// executes on the serial tool path (D5, no background goroutine). It must run
// before registerInteracts so the sensitive-tool gate can wrap these tools.
package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/fs"
	"github.com/jabing/shutu-agent/internal/tools"
)

// registerFs creates the local FileService (root = fs.root, defaulting to
// <project> in the constructor), registers the write/list/edit tools and
// wires the D3 event sink when fs.enabled. When fs is disabled it creates
// nothing and registers nothing (D10, mirrors registerCode/registerMcps/
// registerJobs).
func (a *app) registerFs() error {
	if !config.Enabled(a.cfg.Fs.Enabled) {
		return nil
	}
	var svc fs.FileService
	if a.cfg.Fs.Root != "" {
		svc = fs.NewLocalFS(a.cfg.Fs.Root)
	} else {
		svc = fs.NewLocalFSForRoot(a.sessionCWD)
	}
	a.fs = svc
	// D3 event sink: fs/* events are appended to the active session log. The
	// callback only ever runs inside a tool Execute — the serial main-loop
	// path (D5). a.log is read at call time, so a session switch (/new,
	// /resume) is honored the same way as the other register* wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	ft := fs.NewFsTools(svc, onEvent)
	fileTools := []tools.Tool{ft.Read(), ft.Write(), ft.List(), ft.Edit()}
	if a.multimodalEnabled() {
		fileTools = append(fileTools, ft.ReadImage())
	}
	for _, tl := range fileTools {
		if err := a.reg.Register(tl); err != nil {
			return fmt.Errorf("pa: register %s: %w", tl.Name(), err)
		}
	}
	return nil
}
