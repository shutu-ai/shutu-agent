// codes.go — the M6e-2 composition-root orchestration (dispatch-m6e-2 §4).
// This is where the code-sandbox capability seam is wired into the REPL:
// registerCode creates the TypeScript Code Mode runtime and registers the
// run_code tool when code.enabled (D10), and wires the D3 event sink so
// code/run is appended to the active session log. The wiring sits entirely in
// the tool registration layer — the loop's turn/step structure is untouched
// (D4) — and run_code execution is foreground and serial on the tool path (D5,
// no background goroutine). It must run before registerInteracts so the
// sensitive-tool gate can wrap run_code too.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/config"
)

// registerCode creates the TypeScript runtime, registers the run_code tool and
// wires the D3 event sink when code.enabled. When code is
// disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerPlans/registerSpills/registerInteracts).
func (a *app) registerCode() error {
	if !config.Enabled(a.cfg.Code.Enabled) {
		return nil
	}
	runtime := code.NewTypeScriptRuntime()
	a.code = runtime
	// D3 event sink: code/run is appended to the active session log. The
	// callback only ever runs inside a run_code tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the other register* wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	ct := code.NewCodeToolsWithRuntime(runtime, onEvent)
	ct.SetBinding(a.executeCodeBinding)
	// Config-derived sandbox policy knobs (code.timeout / code.max_output /
	// code.sandbox_dir). The tool stays decoupled from config (D2): the wiring
	// supplies the values after the seam constructor.
	ct.DefaultTimeout = a.cfg.Code.Timeout.Duration
	ct.DefaultMaxOutput = a.cfg.Code.MaxOutput
	ct.DefaultCwd = a.cfg.Code.SandboxDir
	// DSH starts tools in the workspace attached to the current session. Keep
	// an explicit model-provided cwd as an override, but resolve the omitted cwd
	// against the active session at execution time rather than process cwd or a
	// single startup sandbox.
	ct.DefaultCwdFunc = func() string { return a.sessionCWD() }
	if err := a.reg.Register(ct.Run()); err != nil {
		return fmt.Errorf("pa: register run_code: %w", err)
	}
	return nil
}

// executeCodeBinding is the host side of PTC's tools.<name>() bridge. The
// direct loop policy exposes only run_code, so nested calls use a cloned
// registry with the session's underlying capability policy. This preserves
// schema validation, approvals, timeouts, output bounds and tool-specific
// hooks without allowing a program to recursively invoke run_code.
func (a *app) executeCodeBinding(ctx context.Context, req code.ProgramBindingRequest) (any, error) {
	if req.Name == code.ToolRunName {
		return nil, fmt.Errorf("run_code cannot be called from inside another run_code program")
	}
	if a.reg == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	scoped := a.reg.Clone()
	policy := a.codeBindingPolicy
	if len(policy.Enabled) == 0 {
		policy = a.basePolicy
		policy.Enabled = modeToolWhitelist(config.ModeStandard, policy.Enabled)
	}
	scoped.SetPolicy(policy)
	result, err := scoped.Execute(ctx, req.Name, req.Args)
	if err != nil {
		return nil, err
	}
	if result.IsError {
		message := result.Output
		if message == "" && result.Error != nil {
			message = result.Error.Name + ": " + result.Error.Code
		}
		if message == "" {
			message = "tool execution failed"
		}
		return nil, fmt.Errorf("%s", message)
	}
	return result.Value, nil
}
