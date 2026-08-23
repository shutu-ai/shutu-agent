// codes.go — the M6e-2 composition-root orchestration (dispatch-m6e-2 §4).
// This is where the code-sandbox capability seam is wired into the REPL:
// registerCode creates the local subprocess Provider + Engine and registers the
// run_code tool when code.enabled (D10), and wires the D3 event sink so
// code/run is appended to the active session log. The wiring sits entirely in
// the tool registration layer — the loop's turn/step structure is untouched
// (D4) — and run_code execution is foreground and serial on the tool path (D5,
// no background goroutine). It must run before registerInteracts so the
// sensitive-tool gate can wrap run_code too.
package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/config"
)

// registerCode creates the local subprocess Provider + Engine, registers the
// run_code tool and wires the D3 event sink when code.enabled. When code is
// disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerPlans/registerSpills/registerInteracts).
func (a *app) registerCode() error {
	if !config.Enabled(a.cfg.Code.Enabled) {
		return nil
	}
	prov := code.NewLocalProvider()
	eng := code.NewEngine(prov)
	a.code = eng
	// D3 event sink: code/run is appended to the active session log. The
	// callback only ever runs inside a run_code tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the other register* wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	ct := code.NewCodeTools(eng, onEvent)
	// Config-derived sandbox policy knobs (code.timeout / code.max_output /
	// code.sandbox_dir). The tool stays decoupled from config (D2): the wiring
	// supplies the values after the seam constructor.
	ct.DefaultTimeout = a.cfg.Code.Timeout.Duration
	ct.DefaultMaxOutput = a.cfg.Code.MaxOutput
	ct.DefaultCwd = a.cfg.Code.SandboxDir
	if err := a.reg.Register(ct.Run()); err != nil {
		return fmt.Errorf("pa: register run_code: %w", err)
	}
	return nil
}
