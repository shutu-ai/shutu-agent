package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
)

// newACPSubagent creates the subagent runtime and tools owned by one ACP
// session. It never reuses app.subagents: that runtime is bound to the REPL's
// mutable registry, prompt, log and current session.
func newACPSubagent(a *app, id string, log *session.Log, registry *tools.Registry, pb *prompt.Builder) (subagent.Runtime, *subagent.SubagentTools, error) {
	cfg := a.providerConfigSnapshot()
	provider, model := a.sessionProviderModel(id)
	return newACPSubagentWithConfig(a, id, log, registry, pb, cfg, provider, model)
}

func newACPSubagentWithConfig(a *app, id string, log *session.Log, registry *tools.Registry, pb *prompt.Builder, cfg config.Config, provider, model string) (subagent.Runtime, *subagent.SubagentTools, error) {
	rt := subagent.NewRuntime()
	var reportTools *subagent.SubagentTools
	childLogsMu := &sync.RWMutex{}
	childLogs := map[string]*session.Log{id: log}
	deps := subagent.Deps{
		Log: log,
		ParentLogFor: func(_ context.Context, parent string) *session.Log {
			childLogsMu.RLock()
			defer childLogsMu.RUnlock()
			return childLogs[parent]
		},
		BindSessionLog: func(childID string, childLog *session.Log) {
			childLogsMu.Lock()
			defer childLogsMu.Unlock()
			childLogs[childID] = childLog
		},
		LLM:       a.llmFor(provider),
		Tools:     registry,
		Prompt:    pb,
		Model:     model,
		MaxTokens: cfg.MaxTokens,
		Store:     a.store,
		Report: func(childID, parentID, output string) (string, error) {
			if reportTools == nil {
				return "", fmt.Errorf("report: subagent tools are not ready")
			}
			return reportTools.ReportFromChild(childID, output)
		},
	}
	prov := subagent.NewSpawnProvider(deps)
	if err := rt.RegisterProvider(prov); err != nil {
		_ = rt.Close()
		return nil, nil, fmt.Errorf("ACP subagent spawn provider: %w", err)
	}
	if err := rt.RegisterProvider(subagent.NewForkProvider(deps)); err != nil {
		_ = rt.Close()
		return nil, nil, fmt.Errorf("ACP subagent fork provider: %w", err)
	}
	for name, ep := range cfg.Subagent.ExternalProviders {
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
	st := subagent.NewSubagentToolsWithContinuable(rt, cfg.Subagent.MaxDepth, func() string { return id }, onEvent, true)
	// Child settlement runs outside the ACP prompt goroutine. Bind its
	// parent-addressed durable event path explicitly so a completion cannot be
	// redirected by another session and append failures are observable by the
	// settlement seam.
	st.SetSessionEventSink(func(sessionID, typ string, data any) error {
		if strings.TrimSpace(sessionID) == "" || sessionID != id {
			return fmt.Errorf("ACP subagent: event owner %q is not session %q", sessionID, id)
		}
		if log == nil {
			return fmt.Errorf("ACP subagent: session log is unavailable")
		}
		_, err := log.Append(typ, data)
		return err
	})
	st.SetJobs(a.jobs)
	reportTools = st
	return rt, st, nil
}

func registerACPSubagentTools(registry *tools.Registry, st *subagent.SubagentTools) error {
	for _, tool := range []tools.Tool{
		st.Spawn(),
		st.Fork(),
		st.SpawnTeammate(),
		st.DshSend(),
		st.FollowupTask(),
		st.WaitAgent(),
		st.Interrupt(),
		st.ListAgents(),
	} {
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("register ACP %s: %w", tool.Name(), err)
		}
		registry.Allow(tool.Name())
	}
	return nil
}
