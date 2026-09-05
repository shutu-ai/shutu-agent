package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/agentinstructions"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

func (a *app) agentInstructionsConfig() agentinstructions.Config {
	cfg := a.cfg.AgentInstructions
	return agentinstructions.Config{
		Home:           cfg.Home,
		MaxBytes:       cfg.MaxBytes,
		MaxSourceBytes: cfg.MaxSourceBytes,
	}
}

// agentInstructionsInjectorFor publishes the bounded AGENTS.md/CLAUDE.md
// baseline for one addressed session. The producer owns DSH's durable source,
// and the loop's visible-message check prevents repeating an unchanged row on
// every turn.
func (a *app) agentInstructionsInjectorFor(sessionID string, log *session.Log) loop.PreStepInjector {
	state := visibleAgentInstructionsState(log)
	var initialEvents []session.Event
	if log != nil {
		initialEvents = log.Events()
	}
	cursor := len(initialEvents)
	toolCalls := make(map[string]toolTouchCall)
	return loop.PreStepInjector{
		Name: "agent-instructions",
		InjectWithError: func(context.Context, string) ([]llm.Message, error) {
			var events []session.Event
			if log != nil {
				events = log.Events()
			}
			touched := make([]string, 0)
			for _, event := range events[min(cursor, len(events)):] {
				switch event.Type {
				case session.EventToolCall:
					var call struct {
						CallID    string `json:"callId"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}
					if json.Unmarshal(event.Data, &call) == nil && call.CallID != "" {
						toolCalls[call.CallID] = toolTouchCall{Name: call.Name, Arguments: call.Arguments}
					}
				case session.EventToolResult:
					var result struct {
						CallID string `json:"callId"`
						Name   string `json:"name"`
						Error  *struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					if json.Unmarshal(event.Data, &result) != nil || result.Error != nil {
						continue
					}
					switch result.Name {
					case "read", "write", "edit":
					default:
						continue
					}
					call := toolCalls[result.CallID]
					if call.Name != result.Name {
						continue
					}
					var args struct {
						FilePath string `json:"file_path"`
						Path     string `json:"path"`
					}
					if json.Unmarshal([]byte(call.Arguments), &args) == nil {
						if path := strings.TrimSpace(args.FilePath); path != "" {
							touched = append(touched, path)
						} else if path := strings.TrimSpace(args.Path); path != "" {
							touched = append(touched, path)
						}
					}
				}
			}
			cursor = len(events)
			message, next, err := agentinstructions.ReconcileTouch(
				a.sessionCWDFor(sessionID), a.agentInstructionsConfig(), state, touched,
			)
			if err != nil {
				return nil, err
			}
			if next != nil {
				state = next
			}
			if message == nil {
				return nil, nil
			}
			return []llm.Message{*message}, nil
		},
		OncePerTurn: false,
		Deduplicate: true,
		Unbounded:   true,
	}
}

type toolTouchCall struct {
	Name      string
	Arguments string
}

// visibleAgentInstructionsState rebuilds producer state from durable rows.
// Prose is discarded; only the baseline identity and per-candidate changes are
// retained. Compaction-shadowed rows no longer contribute active state.
func visibleAgentInstructionsState(log *session.Log) *agentinstructions.State {
	if log == nil {
		return &agentinstructions.State{Changes: map[string]agentinstructions.Change{}}
	}
	events := log.Events()
	shadowed := make(map[uint64]bool)
	for _, event := range events {
		if sourceSeqs, ok := session.EventSourceEventSeqs(event); ok {
			for _, seq := range sourceSeqs {
				shadowed[seq] = true
			}
			continue
		}
		if replacement, ok := session.SurfaceReplacement(event); ok {
			for seq := uint64(replacement.Start); seq <= uint64(replacement.End); seq++ {
				shadowed[seq] = true
			}
		}
	}
	state := &agentinstructions.State{Changes: map[string]agentinstructions.Change{}}
	foundBaseline := false
	for _, event := range events {
		if event.Type != session.EventUserMessage || shadowed[event.Seq] {
			continue
		}
		var source struct {
			Source *struct {
				Kind             string                     `json:"kind"`
				Baseline         bool                       `json:"baseline"`
				BaselineIdentity string                     `json:"baselineIdentity"`
				Changes          []agentinstructions.Change `json:"changes"`
			} `json:"source"`
		}
		if json.Unmarshal(event.Data, &source) != nil || source.Source == nil ||
			source.Source.Kind != "agent-instructions" {
			continue
		}
		changes := source.Source.Changes
		if source.Source.Baseline {
			foundBaseline = true
			state.Identity = source.Source.BaselineIdentity
			state.Changes = make(map[string]agentinstructions.Change, len(changes))
		}
		for _, change := range changes {
			if change.Action == "remove" {
				delete(state.Changes, change.Scope)
				continue
			}
			state.Changes[change.Scope] = change
		}
	}
	if !foundBaseline {
		return nil
	}
	return state
}
