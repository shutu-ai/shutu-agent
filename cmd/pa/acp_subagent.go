package main

import (
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
)

// newACPSubagent creates the subagent runtime and tools owned by one ACP
// session. It never reuses app.subagents: that runtime is bound to the REPL's
// mutable registry, prompt, log and current session.
func newACPSubagent(a *app, id string, log *session.Log, registry *tools.Registry, pb *prompt.Builder) (subagent.Runtime, *subagent.SubagentTools, error) {
	rt := subagent.NewRuntime()
	prov := subagent.NewSpawnProvider(subagent.Deps{
		Log:    log,
		LLM:    a.currentLLM(),
		Tools:  registry,
		Prompt: pb,
		Model:  a.cfg.Model,
		Store:  a.store,
	})
	if err := rt.RegisterProvider(prov); err != nil {
		_ = rt.Close()
		return nil, nil, fmt.Errorf("ACP subagent spawn provider: %w", err)
	}
	for name, ep := range a.cfg.Subagent.ExternalProviders {
		if !ep.Enabled {
			continue
		}
		providerName := name
		if name == "claude_code" {
			providerName = "claude-code"
		}
		if err := rt.RegisterProvider(subagent.NewExternalProvider(providerName, ep.Command)); err != nil {
			_ = rt.Close()
			return nil, nil, fmt.Errorf("ACP external subagent provider %q: %w", name, err)
		}
	}
	onEvent := func(typ string, data any) {
		if log == nil {
			return
		}
		if _, err := log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	st := subagent.NewSubagentTools(rt, a.cfg.Subagent.MaxDepth, func() string { return id }, onEvent)
	return rt, st, nil
}

func registerACPSubagentTools(registry *tools.Registry, st *subagent.SubagentTools) error {
	for _, tool := range []tools.Tool{
		st.Spawn(),
		st.Status(),
		st.Cancel(),
		st.List(),
		st.Send(),
		st.Interrupt(),
		st.Report(),
		st.Resume(),
	} {
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("register ACP %s: %w", tool.Name(), err)
		}
		registry.Allow(tool.Name())
	}
	return nil
}
