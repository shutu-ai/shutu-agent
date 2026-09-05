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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// registerSubagent creates the SpawnProvider + Runtime and registers the
// model-facing subagent tools when subagent.enabled, and wires the D3 event sink. When
// subagent is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs).
func (a *app) registerSubagent() error {
	if !config.Enabled(a.cfg.Subagent.Enabled) {
		return nil
	}
	var reportTools *subagent.SubagentTools
	deps := subagent.Deps{
		// Log is the parent/host log the provider is bound to; it is never
		// appended to by the provider (each child owns an independent log) —
		// subagent/* events reach the parent log through the onEvent sink
		// below (D3, serial tool path).
		Log: a.log,
		ParentLogFor: func(ctx context.Context, parentSession string) *session.Log {
			return a.subagentParentLog(parentSession)
		},
		BindSessionLog: func(id string, log *session.Log) {
			a.runtimeMu.Lock()
			if a.runtimeLogs == nil {
				a.runtimeLogs = make(map[string]*session.Log)
			}
			a.runtimeLogs[id] = log
			a.runtimeMu.Unlock()
		},
		LLM: a.currentLLM(),
		LLMFor: func(ctx context.Context, parentSession string) llm.LLM {
			provider, _, err := a.sessionProviderModelStrict(parentSession)
			if err != nil {
				return unavailableLLM{err: err}
			}
			return a.llmFor(provider)
		},
		Tools: a.reg,
		ToolsFor: func(_ context.Context, parentSession string) *tools.Registry {
			registry := a.reg.Clone()
			log := a.subagentParentLog(parentSession)
			if _, _, err := a.applySessionRuntimeOnStrict(parentSession, log, registry); err != nil {
				// A child must not inherit the process-global tool surface when its
				// parent's durable runtime cannot be reconstructed. Return a real
				// reject-all registry so the provider fails closed at execution.
				rejected := tools.New()
				policy := rejected.Policy()
				policy.Enabled = nil
				rejected.SetPolicy(policy)
				return rejected
			}
			return registry
		},
		Prompt: a.prompt,
		PromptFor: func(_ context.Context, parentSession string) *prompt.Builder {
			registry := a.reg.Clone()
			log := a.subagentParentLog(parentSession)
			runtime, _, err := a.applySessionRuntimeOnStrict(parentSession, log, registry)
			if err != nil {
				return prompt.New("Session runtime configuration is unavailable; do not execute tools.")
			}
			return runtime.prompt
		},
		Model:     a.cfg.Model,
		MaxTokens: a.cfg.MaxTokens,
		MaxTokensFor: func(_ context.Context, parentSession string) int {
			a.runtimeMaxTokensMu.RLock()
			deferred := a.runtimeMaxTokens[parentSession]
			a.runtimeMaxTokensMu.RUnlock()
			return deferred
		},
		ModelFor: func(_ context.Context, parentSession string) string {
			_, model, err := a.sessionProviderModelStrict(parentSession)
			if err != nil {
				return "__session_config_unavailable__"
			}
			return model
		},
		Store: a.store,
		Report: func(childID, parentID, output string) (string, error) {
			if reportTools == nil {
				return "", fmt.Errorf("report: subagent tools are not ready")
			}
			return reportTools.ReportFromChild(childID, output)
		},
		ReportContext: func(ctx context.Context, childID, parentID, output string) (string, error) {
			if reportTools == nil {
				return "", fmt.Errorf("report: subagent tools are not ready")
			}
			return reportTools.ReportFromChildContext(ctx, childID, output)
		},
	}
	prov := subagent.NewSpawnProvider(deps)
	rt := subagent.NewRuntime()
	if err := rt.RegisterProvider(prov); err != nil {
		return fmt.Errorf("sta: register subagent provider: %w", err)
	}
	if err := rt.RegisterProvider(subagent.NewForkProvider(deps)); err != nil {
		return fmt.Errorf("sta: register fork provider: %w", err)
	}
	// D-GAP-4: optional external subagent backends (codex / claude-code).
	// Register one provider per enabled config entry; a failed registration
	// (e.g. a duplicate name) fails closed — no silent fallback. The config
	// key "claude_code" registers under the tool-facing provider name
	// "claude-code" (the subagent provider enum), which also selects the
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
			return fmt.Errorf("sta: register external subagent provider %q: %w", name, err)
		}
	}
	a.subagents = rt
	// D3 event sink: subagent/* events are appended to the active session log.
	// The callback only ever runs inside a subagent_* tool Execute — the
	// serial main-loop path — so the session log is never touched from a
	// background child goroutine (D5; the dispatch-m5b-2 §2 tool-layer
	// decision). a.log is read at call time, so a session switch (/new,
	// /resume) is honored the same way as the other session-bound event wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", err)
		}
	}
	st := subagent.NewSubagentToolsWithContinuableContext(rt, a.cfg.Subagent.MaxDepth, func(ctx context.Context) string {
		return a.runtimeSessionID(ctx)
	}, onEvent, true)
	st.SetSessionEventSink(func(sessionID, typ string, data any) error {
		log, err := a.sessionLogForAgent(context.Background(), sessionID)
		if err != nil {
			return err
		}
		_, err = log.Append(typ, data)
		return err
	})
	st.SetCompletionWake(func(ctx context.Context, sessionID, childID string, result subagent.Result) error {
		if a.agentRegistry == nil || sessionID == "" {
			return nil
		}
		parent, err := a.agentRegistry.Lookup(agent.ID(sessionID))
		if err != nil {
			// A cold parent is still recoverable from its durable
			// subagent/end event; only a live Agent receives an inbox wake.
			return nil
		}
		prompt := subagentCompletionPrompt(childID, result.StopReason, result.Output)
		err = parent.Followup(prompt, subagentCompletionMetadata(childID, result.StopReason))
		if err != nil {
			// subagent/end has already been committed by the settlement path.
			// Retry the missing inbox receipt from that durable fact so a
			// transient journal failure is recoverable without a restart.
			if log, logErr := a.sessionLogForAgent(ctx, sessionID); logErr == nil {
				if recoveryErr := a.recoverSubagentCompletionWakes(log, parent); recoveryErr != nil {
					return errors.Join(err, recoveryErr)
				}
				return nil
			}
		}
		return err
	})
	st.SetReportDelivery(func(ctx context.Context, sessionID, childID, messageID, output string) error {
		if a.agentRegistry == nil || sessionID == "" {
			return nil
		}
		parent, err := a.agentRegistry.Lookup(agent.ID(sessionID))
		if err != nil {
			// The report fact is already durable; cold parent materialization
			// uses it to reconstruct the DSH relay receipt.
			return nil
		}
		if err := parent.Followup(subagentReportPrompt(childID, output), subagentReportMetadata(childID, messageID)); err != nil {
			if log, logErr := a.sessionLogForAgent(ctx, sessionID); logErr == nil {
				if recoveryErr := a.recoverSubagentReportRelays(log, parent); recoveryErr != nil {
					return errors.Join(err, recoveryErr)
				}
				return nil
			}
		}
		return err
	})
	st.SetJobs(a.jobs)
	reportTools = st
	a.subagentTools = st
	for _, t := range []tools.Tool{
		st.Spawn(),
		st.Fork(),
		st.SpawnTeammate(),
		st.DshSend(),
		st.FollowupTask(),
		st.WaitAgent(),
		st.Interrupt(),
		st.ListAgents(),
	} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("sta: register %s: %w", t.Name(), err)
		}
	}
	return nil
}

// subagentCompletionPrompt keeps the live completion notice on the same
// DSH-compatible model contract as cold recovery: one settlement account,
// then the child's chosen closing output as clearly attributed content.
func subagentCompletionPrompt(childID, stopReason, output string) string {
	var sb strings.Builder
	sb.WriteString(subagentSettlementSummary(childID, stopReason))
	if strings.TrimSpace(output) == "" {
		sb.WriteString("\nIt left no closing message.")
	} else {
		sb.WriteString("\nIts closing message:\n")
		sb.WriteString(runeHead(output, 200))
	}
	return sb.String()
}

// subagentCompletionMetadata carries the runtime fields used by the inbox
// receipt plus the durable provenance projected by the Web UI. Keeping this in
// one helper ensures live delivery and crash recovery produce identical rows.
func subagentCompletionMetadata(childID, stopReason string) map[string]string {
	return map[string]string{
		"source":                   "subagent",
		"child_id":                 childID,
		"dedupe_key":               "subagent:end:" + childID,
		"source_kind":              "subagent-settled",
		"source_form":              "notice",
		"source_summary":           boundSubagentSummary(subagentSettlementSummary(childID, stopReason)),
		"source_sender_session_id": childID,
	}
}

// subagentSettlementSummary mirrors DSH's one-line account of a child's
// terminal state. Unknown merge-extensible stop reasons remain explicitly
// abnormal instead of being misreported as success.
func subagentSettlementSummary(childID, stopReason string) string {
	subject := "Background subagent " + childID
	switch stopReason {
	case subagent.StopCompleted:
		return subject + " finished and will do no further work unless you send it more."
	case subagent.StopAborted:
		return subject + " was stopped before it finished."
	case subagent.StopMaxTokens:
		return subject + " ran out of room before it finished."
	case subagent.StopRefusal:
		return subject + " declined the task."
	case subagent.StopError:
		return subject + " failed before it finished."
	default:
		return subject + " ended abnormally (" + stopReason + ") before it finished."
	}
}

// DSH bounds notice summaries because they are durable collapsed-row labels.
const subagentSummaryMaxRunes = 120

func boundSubagentSummary(summary string) string {
	runes := []rune(summary)
	if len(runes) <= subagentSummaryMaxRunes {
		return summary
	}
	return string(runes[:subagentSummaryMaxRunes-1]) + "…"
}

func subagentReportPrompt(childID, output string) string {
	return "Background subagent " + childID + " reported:\n" + output
}

func subagentReportMetadata(childID, messageID string) map[string]string {
	return map[string]string{
		"source":                   "subagent",
		"child_id":                 childID,
		"report_id":                messageID,
		"dedupe_key":               "subagent:report:" + messageID,
		"source_kind":              "subagent-report",
		"source_form":              "relay",
		"source_sender_session_id": childID,
	}
}

func (a *app) subagentParentLog(sessionID string) *session.Log {
	if sessionID == "" || (a.agentRegistry == nil && sessionID == a.currentID) {
		return a.log
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if a.runtimeLogs == nil {
		return nil
	}
	return a.runtimeLogs[sessionID]
}
