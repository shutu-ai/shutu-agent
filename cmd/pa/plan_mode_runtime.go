package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/projection"
	"github.com/jabing/shutu-agent/internal/session"
)

type planModeAction string

const (
	planModeCommitted planModeAction = "committed"
	planModeQueued    planModeAction = "queued"
	planModeCancelled planModeAction = "cancelled"
	planModeNoop      planModeAction = "noop"
)

func hasOpenSessionTurn(events []session.Event) bool {
	open := false
	for _, event := range events {
		switch event.Type {
		case session.EventTurnStart:
			open = true
		case session.EventTurnEnd:
			open = false
		}
	}
	return open
}

// currentPlanModeActive is the single composition-root read boundary for the
// durable plan-mode switch. Consumers must not fold EventPlanMode separately:
// projection.Build applies the same validation and replay rules used by native,
// Web, and other session-state projections.
func currentPlanModeActive(log *session.Log) (bool, error) {
	if log == nil {
		return false, errors.New("nil session log")
	}
	return currentPlanModeActiveFromEvents(log.Events())
}

func currentPlanModeActiveFromEvents(events []session.Event) (bool, error) {
	snapshot, err := projection.Build(events)
	if err != nil {
		return false, fmt.Errorf("plan mode projection: %w", err)
	}
	return snapshot.PlanMode.Active, nil
}

func (a *app) publishedSessionAgent(sessionID string) *agent.Handle {
	if a == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	return a.sessionAgents[sessionID]
}

// setPlanModeFor is the shared CLI/Web state transition. Idle sessions commit
// immediately. An open Agent turn records the desired state for the next
// accepted turn boundary; command/run + command/done makes that selection
// recoverable after a process restart.
func (a *app) setPlanModeFor(ctx context.Context, sessionID string, log *session.Log, active bool) (planModeAction, error) {
	if log == nil {
		return planModeNoop, errors.New("no active session")
	}
	if sessionID == "" {
		sessionID = a.runtimeSessionID(ctx)
	}
	current, err := currentPlanModeActive(log)
	if err != nil {
		return planModeNoop, err
	}
	turnOpen := hasOpenSessionTurn(log.Events())
	if handle := a.publishedSessionAgent(sessionID); handle != nil && handle.Status() == agent.StatusRunning {
		turnOpen = true
	}

	a.planMu.Lock()
	if a.planPending == nil {
		a.planPending = make(map[string]bool)
	}
	target := current
	if pending, ok := a.planPending[sessionID]; ok {
		target = pending
	}
	if active == target {
		if active == current {
			delete(a.planPending, sessionID)
			a.planMu.Unlock()
			return planModeNoop, nil
		}
		delete(a.planPending, sessionID)
		a.planMu.Unlock()
		return planModeCancelled, nil
	}
	if turnOpen {
		a.planPending[sessionID] = active
		a.planMu.Unlock()
		if current == active {
			return planModeCancelled, nil
		}
		return planModeQueued, nil
	}
	if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(active)); err != nil {
		a.planMu.Unlock()
		return planModeNoop, err
	}
	delete(a.planPending, sessionID)
	a.planMu.Unlock()
	return planModeCommitted, nil
}

func (a *app) steerPlanMessage(sessionID, text string) (bool, error) {
	return a.steerPlanContent(sessionID, []llm.ContentBlock{llm.Text(text)})
}

func (a *app) steerPlanContent(sessionID string, content []llm.ContentBlock) (bool, error) {
	handle := a.publishedSessionAgent(sessionID)
	if handle == nil || handle.Status() != agent.StatusRunning {
		return false, nil
	}
	if err := handle.SteerContent(content, map[string]string{"source": "user", "command": "plan"}); err != nil {
		return false, err
	}
	return true, nil
}

func isPlanCommandLine(text string) bool {
	return strings.HasPrefix(text, "/plan") && (len(text) == len("/plan") || text[len("/plan")] == ' ' || text[len("/plan")] == '\t')
}

func planContent(text string, images []llm.ImageRef) []llm.ContentBlock {
	content := make([]llm.ContentBlock, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		content = append(content, llm.Text(text))
	}
	for _, image := range images {
		content = append(content, llm.ContentBlock{Kind: llm.BlockImage, Image: image})
	}
	return content
}

// applyPendingPlanMode runs before request/runtime assembly. The live map is
// needed while the outer command has not yet appended command/done; the
// durable fold covers a crash after a successful command but before this
// boundary.
func (a *app) applyPendingPlanMode(sessionID string, log *session.Log) error {
	if log == nil {
		return errors.New("no active session")
	}
	current, err := currentPlanModeActive(log)
	if err != nil {
		return err
	}
	target, ok := a.pendingPlanMode(sessionID)
	if !ok {
		target, ok = foldPendingPlanMode(log.Events())
	}
	if !ok {
		return nil
	}
	if target == current {
		a.clearPendingPlanMode(sessionID)
		return nil
	}
	if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(target)); err != nil {
		return err
	}
	a.clearPendingPlanMode(sessionID)
	return nil
}

func (a *app) pendingPlanMode(sessionID string) (bool, bool) {
	a.planMu.Lock()
	defer a.planMu.Unlock()
	target, ok := a.planPending[sessionID]
	return target, ok
}

func (a *app) clearPendingPlanMode(sessionID string) {
	a.planMu.Lock()
	delete(a.planPending, sessionID)
	a.planMu.Unlock()
}

// foldPendingPlanMode reconstructs successful plan commands whose mode event
// has not yet been committed. An unfinished command is not a selection.
func foldPendingPlanMode(events []session.Event) (bool, bool) {
	active := false
	type commandState struct{ wanted bool }
	running := make(map[string]commandState)
	var pending *bool
	for _, event := range events {
		switch event.Type {
		case session.EventPlanMode:
			var mode struct {
				Active bool `json:"active"`
			}
			if json.Unmarshal(event.Data, &mode) == nil {
				active = mode.Active
				pending = nil
			}
		case session.EventCommandRun:
			var command struct {
				CommandID string `json:"commandId"`
				Name      string `json:"name"`
				Args      string `json:"args"`
			}
			if json.Unmarshal(event.Data, &command) == nil && command.Name == "plan" && command.CommandID != "" {
				running[command.CommandID] = commandState{wanted: strings.TrimSpace(command.Args) != "off"}
			}
		case session.EventCommandDone:
			var done struct {
				CommandID string `json:"commandId"`
				Kind      string `json:"kind"`
			}
			if json.Unmarshal(event.Data, &done) != nil {
				continue
			}
			command, ok := running[done.CommandID]
			if !ok {
				continue
			}
			delete(running, done.CommandID)
			if done.Kind == "success" && command.wanted != active {
				wanted := command.wanted
				pending = &wanted
			} else {
				// A successful selection that already matches the logged state
				// also clears an older opposite selection (the command is the
				// latest whole-value intent).
				pending = nil
			}
		}
	}
	if pending == nil {
		return false, false
	}
	return *pending, true
}
