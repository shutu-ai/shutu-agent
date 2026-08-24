// subagent.go — the M5b-2 composition-root orchestration (dispatch-m5b-2 §4).
// This is where the subagent capability seam is wired into the REPL:
// registerSubagent creates the spawn provider + Runtime and registers the four
// subagent_* tools when subagent.enabled (D10), and wires the D3 event sink so
// subagent/start, subagent/end and subagent/report are appended to the active
// session log. The loop's turn/step structure is untouched (D4): a spawned
// child is driven by its own independent loop instance in a background
// goroutine and never enters the parent's turn/step (D5) — the tools observe
// children through the serial tool path, and the deferred Close cancels and
// awaits every live child at shutdown so no goroutine leaks (lifecycle
// reversible, ADR 决策 ②).
package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
)

// registerSubagent creates the SpawnProvider + Runtime and registers the four
// subagent_* tools when subagent.enabled, and wires the D3 event sink. When
// subagent is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerKB).
func (a *app) registerSubagent() error {
	if !config.Enabled(a.cfg.Subagent.Enabled) {
		return nil
	}
	prov := subagent.NewSpawnProvider(subagent.Deps{
		// Log is the parent/host log the provider is bound to; it is never
		// appended to by the provider (each child owns an independent log) —
		// subagent/* events reach the parent log through the onEvent sink
		// below (D3, serial tool path).
		Log:    a.log,
		LLM:    a.currentLLM(),
		Tools:  a.reg,
		Prompt: a.prompt,
		Model:  a.cfg.Model,
		Store:  a.store,
	})
	rt := subagent.NewRuntime()
	if err := rt.RegisterProvider(prov); err != nil {
		return fmt.Errorf("pa: register subagent provider: %w", err)
	}
	// D-GAP-4: optional external subagent backends (codex / claude-code).
	// Register one provider per enabled config entry; a failed registration
	// (e.g. a duplicate name) fails closed — no silent fallback. The config
	// key "claude_code" registers under the tool-facing provider name
	// "claude-code" (the subagent_spawn provider enum), which also selects the
	// `claude -p` headless args preset in NewExternalProvider.
	for name, ep := range a.cfg.Subagent.ExternalProviders {
		if !ep.Enabled {
			continue // D10: an unenabled provider is never registered
		}
		providerName := name
		if name == "claude_code" {
			providerName = "claude-code"
		}
		if err := rt.RegisterProvider(subagent.NewExternalProvider(providerName, ep.Command)); err != nil {
			return fmt.Errorf("pa: register external subagent provider %q: %w", name, err)
		}
	}
	a.subagents = rt
	// D3 event sink: subagent/* events are appended to the active session log.
	// The callback only ever runs inside a subagent_* tool Execute — the
	// serial main-loop path — so the session log is never touched from a
	// background child goroutine (D5; the dispatch-m5b-2 §2 tool-layer
	// decision). a.log is read at call time, so a session switch (/new,
	// /resume) is honored the same way as the kb/jobs wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	st := subagent.NewSubagentTools(rt, a.cfg.Subagent.MaxDepth, func() string { return a.currentID }, onEvent)
	for _, t := range []tools.Tool{
		st.Spawn(),
		st.Status(),
		st.Cancel(),
		st.List(),
		st.Send(),
		st.Interrupt(),
		st.Report(),
		st.Resume(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	return nil
}
