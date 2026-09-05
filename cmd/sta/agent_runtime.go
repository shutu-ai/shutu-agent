package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// sessionInboxJournal projects Agent queue mutations into the canonical
// session event stream. The Agent inbox commits the event before changing its
// live projection, so a crash between those operations is replay-safe.
type sessionInboxJournal struct {
	app       *app
	sessionID string
	log       *session.Log
}

func (j sessionInboxJournal) AppendInboxEvent(event agent.InboxEvent) error {
	log := j.log
	if j.app != nil {
		resolved, err := j.app.sessionLogForAgent(context.Background(), j.sessionID)
		if err != nil {
			return err
		}
		log = resolved
	}
	if log == nil {
		return errors.New("agent inbox journal has no session log")
	}
	// Removals have no inserted work. Persist the DSH wire contract's array
	// form rather than Go's nil-slice JSON encoding, which would emit null and
	// break browser history replay.
	inserted := event.Inserted
	if inserted == nil {
		inserted = []agent.Message{}
	}
	payload := map[string]any{
		"target":   event.Target,
		"start":    event.Start,
		"inserted": inserted,
	}
	if event.RemovedCount > 0 {
		payload["removedCount"] = event.RemovedCount
	}
	if event.Outcome != "" {
		payload["outcome"] = event.Outcome
	}
	if event.Turn > 0 {
		payload["turn"] = event.Turn
	}
	_, err := log.Append(session.EventAgentInboxSpliced, payload)
	return err
}

func replaySessionInbox(events []session.Event) ([]agent.InboxEvent, error) {
	out := make([]agent.InboxEvent, 0)
	for _, event := range events {
		if event.Type != session.EventAgentInboxSpliced {
			continue
		}
		var payload struct {
			Target       string          `json:"target"`
			Start        int             `json:"start"`
			RemovedCount int             `json:"removedCount"`
			Inserted     []agent.Message `json:"inserted"`
			Outcome      string          `json:"outcome"`
			Turn         int             `json:"turn"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("decode agent inbox event %d: %w", event.Seq, err)
		}
		out = append(out, agent.InboxEvent{
			Target: payload.Target, Start: payload.Start, RemovedCount: payload.RemovedCount,
			Inserted: payload.Inserted, Outcome: payload.Outcome, Turn: payload.Turn,
		})
	}
	return out, nil
}

// recoverSubagentCompletionWakes closes the durable boundary between a
// parent's subagent/end event and its Agent inbox. The child terminal event is
// authoritative; if the process died before the corresponding inbox splice
// was committed, cold recovery must recreate the wake. We treat the existence
// of any durable insertion with the same dedupe key as the receipt, even when
// that message has since been claimed: this gives at-least-once recovery
// without replaying a wake on every process restart.
func (a *app) recoverSubagentCompletionWakes(log *session.Log, handle *agent.Handle) error {
	if log == nil || handle == nil {
		return nil
	}
	events := log.Events()
	inboxEvents, err := replaySessionInbox(events)
	if err != nil {
		return err
	}
	recorded := make(map[string]bool)
	for _, event := range inboxEvents {
		for _, message := range event.Inserted {
			if key := strings.TrimSpace(message.Metadata["dedupe_key"]); key != "" {
				recorded[key] = true
			}
		}
	}
	var firstErr error
	for _, event := range events {
		if event.Type != session.EventSubagentEnd {
			continue
		}
		var payload struct {
			ID            string `json:"id"`
			Provider      string `json:"provider"`
			StopReason    string `json:"stopReason"`
			OutputSummary string `json:"outputSummary"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode subagent completion %d: %w", event.Seq, err)
			}
			continue
		}
		if strings.TrimSpace(payload.ID) == "" {
			continue
		}
		key := "subagent:end:" + payload.ID
		if recorded[key] {
			continue
		}
		prompt := subagentCompletionPrompt(payload.ID, payload.StopReason, payload.OutputSummary)
		if err := handle.Followup(prompt, subagentCompletionMetadata(payload.ID, payload.StopReason)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recorded[key] = true
	}
	return firstErr
}

// recoverSubagentReportRelays reconstructs explicit child reports whose durable
// fact committed before the parent inbox receipt. A report-specific identity
// permits multiple reports from one child without replaying acknowledged rows.
// Legacy report events without messageId retain their historical log-only
// semantics rather than risking duplicate parent input.
func (a *app) recoverSubagentReportRelays(log *session.Log, handle *agent.Handle) error {
	if log == nil || handle == nil {
		return nil
	}
	events := log.Events()
	inboxEvents, err := replaySessionInbox(events)
	if err != nil {
		return err
	}
	recorded := make(map[string]bool)
	for _, event := range inboxEvents {
		for _, message := range event.Inserted {
			if key := strings.TrimSpace(message.Metadata["dedupe_key"]); key != "" {
				recorded[key] = true
			}
		}
	}
	var firstErr error
	for _, event := range events {
		if event.Type != session.EventSubagentReport {
			continue
		}
		var payload struct {
			ID            string `json:"id"`
			ParentSession string `json:"parentSession"`
			Content       string `json:"content"`
			MessageID     string `json:"messageId"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode subagent report %d: %w", event.Seq, err)
			}
			continue
		}
		messageID := strings.TrimSpace(payload.MessageID)
		childID := strings.TrimSpace(payload.ID)
		parentID := strings.TrimSpace(payload.ParentSession)
		if messageID == "" || childID == "" || (parentID != "" && parentID != string(handle.ID())) {
			continue
		}
		key := "subagent:report:" + messageID
		if recorded[key] {
			continue
		}
		if err := handle.Followup(subagentReportPrompt(childID, payload.Content), subagentReportMetadata(childID, messageID)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recorded[key] = true
	}
	return firstErr
}

// recoverJobCompletionWakes applies the same durable receipt rule to
// background jobs. job/done is committed by the settlement observer before the
// owner inbox is touched; a restart therefore has enough information to
// recreate a lost owner notification without asking the job producer to run
// again.
func (a *app) recoverJobCompletionWakes(log *session.Log, handle *agent.Handle) error {
	if log == nil || handle == nil {
		return nil
	}
	events := log.Events()
	inboxEvents, err := replaySessionInbox(events)
	if err != nil {
		return err
	}
	recorded := make(map[string]bool)
	for _, event := range inboxEvents {
		for _, message := range event.Inserted {
			if key := strings.TrimSpace(message.Metadata["dedupe_key"]); key != "" {
				recorded[key] = true
			}
		}
	}
	var firstErr error
	owner := strings.TrimSpace(string(handle.ID()))
	jobLabels := make(map[string]struct{ kind, label string })
	for _, event := range events {
		switch event.Type {
		case session.EventJobStart:
			var start struct {
				ID    string `json:"id"`
				Kind  string `json:"kind"`
				Label string `json:"label"`
			}
			if err := json.Unmarshal(event.Data, &start); err == nil && start.ID != "" {
				jobLabels[start.ID] = struct{ kind, label string }{kind: start.Kind, label: start.Label}
			}
			continue
		case session.EventJobDone:
		default:
			continue
		}
		var payload struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			Detail        string `json:"detail"`
			OutputSummary string `json:"outputSummary"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode job completion %d: %w", event.Seq, err)
			}
			continue
		}
		if strings.TrimSpace(payload.ID) == "" {
			continue
		}
		key := "job:" + payload.ID
		if recorded[key] {
			continue
		}
		prompt := fmt.Sprintf("[JOB COMPLETION]\njob_id: %s\nstatus: %s\ndetail: %s\noutput:\n%s", payload.ID, payload.Status, payload.Detail, payload.OutputSummary)
		identity := jobLabels[payload.ID]
		metadata := jobCompletionMetadata(payload.ID, identity.kind, identity.label, payload.Status)
		deliverErr := a.deliverJobCompletionWake(handle, owner, prompt, metadata)
		if deliverErr != nil {
			if firstErr == nil {
				firstErr = deliverErr
			}
			continue
		}
		recorded[key] = true
	}
	return firstErr
}

func (a *app) recoverSessionCompletionWakes(log *session.Log, handle *agent.Handle) error {
	return errors.Join(
		a.recoverSubagentCompletionWakes(log, handle),
		a.recoverSubagentReportRelays(log, handle),
		a.recoverJobCompletionWakes(log, handle),
	)
}

// sessionAgent returns the long-lived Agent handle for one session. The
// adapter deliberately keeps the current loop bridge in one place; all
// production turn entry points reach this Agent-owned adapter.
func (a *app) sessionAgent(sessionID string) (*agent.Handle, error) {
	if err := a.requireRunning(); err != nil {
		return nil, err
	}
	if a.agentRegistry == nil {
		return nil, errors.New("agent runtime is unavailable")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	if a.sessionAgents == nil {
		a.sessionAgents = make(map[string]*agent.Handle)
	}
	if handle := a.sessionAgents[sessionID]; handle != nil {
		// The registry normally removes this memo through the scope cleanup,
		// but a stale memo can still exist after a legacy close path or while
		// disposal races with a new request. Never hand a closed Agent back to
		// a surface; discard the stale entry and materialize a fresh owner from
		// the durable session below.
		if handle.Status() == agent.StatusClosed {
			delete(a.sessionAgents, sessionID)
		} else {
			if log, err := a.sessionLogForAgent(context.Background(), sessionID); err == nil {
				if err := a.recoverSessionCompletionWakes(log, handle); err != nil {
					fmt.Fprintln(os.Stderr, "sta: subagent completion recovery:", err)
				}
			}
			return handle, nil
		}
	}
	log, err := a.sessionLogForAgent(context.Background(), sessionID)
	if err != nil {
		return nil, err
	}
	if err := a.recoverTerminalClaims(log, sessionID); err != nil {
		return nil, err
	}
	inboxEvents, err := replaySessionInbox(log.Events())
	if err != nil {
		return nil, err
	}
	handle, err := a.agentRegistry.Create(agent.Options{
		ID: agent.ID(sessionID),
		// Resolve the journal through the current runtime log on every write.
		// A memoized Agent can outlive an in-memory log reload and observe
		// receipts appended by another process; writing to the stale captured
		// log would allocate a conflicting durable sequence.
		InboxJournal: sessionInboxJournal{app: a, sessionID: sessionID},
		InitialInbox: inboxEvents,
		InitialTurn:  log.NextTurn(),
		Runner: func(ctx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
			return a.runAgentTurn(ctx, sessionID, input, runtimeAgent)
		},
	})
	if err != nil {
		return nil, err
	}
	if err := handle.Scope().AddCleanup(func() error {
		// Registry.Close removes the Agent before running its cleanup. Drop the
		// app-side memo as part of that same disposal boundary; otherwise a later
		// Web/Native request can retrieve a closed handle and mistake it for a
		// live session runtime.
		return a.removeSessionAgentMemo(sessionID, handle)
	}); err != nil {
		_ = a.agentRegistry.Close(handle.ID())
		return nil, fmt.Errorf("register Agent memo cleanup: %w", err)
	}
	// jobs-local binds records to the exact Agent scope.  Without this cleanup
	// a closed web/ACP Agent would leave its background jobs addressable in the
	// process-wide registry, unlike the Harness owner-disposal contract.
	if a.jobs != nil {
		if err := handle.Scope().AddCleanup(func() error {
			return a.jobs.CloseOwner(sessionID)
		}); err != nil {
			_ = a.agentRegistry.Close(handle.ID())
			return nil, fmt.Errorf("register job owner cleanup: %w", err)
		}
	}
	if err := handle.Scope().AddCleanup(func() error {
		return a.closeModelTerminalOwner(sessionID)
	}); err != nil {
		_ = a.agentRegistry.Close(handle.ID())
		return nil, fmt.Errorf("register terminal owner cleanup: %w", err)
	}
	if err := handle.Scope().AddCleanup(func() error {
		a.clearSessionApprovalPolicy(sessionID)
		return nil
	}); err != nil {
		_ = a.agentRegistry.Close(handle.ID())
		return nil, fmt.Errorf("register approval policy cleanup: %w", err)
	}
	runContext := a.baseCtx
	if runContext == nil {
		runContext = context.Background()
	}
	if err := handle.Start(runContext); err != nil {
		_ = a.agentRegistry.Close(handle.ID())
		return nil, err
	}
	a.sessionAgents[sessionID] = handle
	if err := a.recoverSessionCompletionWakes(log, handle); err != nil {
		// Recovery is best effort at this boundary. The terminal event remains
		// durable and the next materialization/retry will attempt the wake
		// again; do not discard an otherwise valid Agent publication.
		fmt.Fprintln(os.Stderr, "sta: subagent completion recovery:", err)
	}
	return handle, nil
}

// removeSessionAgentMemo is the scope-owned rollback for the session Agent
// memo. During rollback from inside sessionAgent, that publisher still owns
// agentMu and the memo was never published; TryLock avoids a rollback deadlock.
// A published handle's normal external Close can always take the lock.
func (a *app) removeSessionAgentMemo(sessionID string, handle *agent.Handle) error {
	if !a.agentMu.TryLock() {
		return nil
	}
	defer a.agentMu.Unlock()
	if a.sessionAgents[sessionID] == handle {
		delete(a.sessionAgents, sessionID)
	}
	return nil
}

func (a *app) sessionLogForAgent(ctx context.Context, sessionID string) (*session.Log, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	a.runtimeMu.Lock()
	if a.runtimeLogs == nil {
		a.runtimeLogs = make(map[string]*session.Log)
	}
	if log := a.runtimeLogs[sessionID]; log != nil {
		a.runtimeMu.Unlock()
		return log, nil
	}
	a.runtimeMu.Unlock()
	if a.store == nil {
		return nil, errors.New("session store is unavailable")
	}
	events, err := a.store.LoadSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", sessionID, err)
	}
	log := session.New()
	a.configureImageResolver(log)
	if err := log.Restore(events); err != nil {
		return nil, err
	}
	a.attachSinkFor(ctx, sessionID, log)
	a.runtimeMu.Lock()
	if existing := a.runtimeLogs[sessionID]; existing != nil {
		a.runtimeMu.Unlock()
		return existing, nil
	}
	a.runtimeLogs[sessionID] = log
	a.runtimeMu.Unlock()
	return log, nil
}

// newAgentSession creates a web/native session without changing the REPL's
// compatibility selection. The browser can open several sessions at once;
// their Agent handles and logs must not be swapped through currentID/log.
func (a *app) newAgentSession(ctx context.Context) (string, error) {
	if err := a.requireRunning(); err != nil {
		return "", err
	}
	if a.store == nil {
		return "", errors.New("session store is unavailable")
	}
	id, err := store.GenerateReservedID(ctx, a.store, "session", newSessionID)
	if err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	created := time.Now().UTC()
	cwd := a.defaultWorkdir()
	if atomic, ok := a.store.(store.SessionCreateStore); ok {
		cfg := a.providerConfigSnapshot()
		if err := atomic.CreateSessionWithOptions(ctx, id, created, store.SessionCreateOptions{
			Header: store.SessionHeader{ID: id, CreatedAt: created, CWD: cwd, AgentPreset: cfg.Mode},
			Config: &store.SessionConfig{AgentPreset: cfg.Mode, Provider: cfg.LLM.Provider, Model: llmProviderModel(cfg, cfg.LLM.Provider), ReasoningEffort: cfg.ReasoningEffort},
		}, nil); err != nil {
			return "", err
		}
	} else {
		if err := a.store.CreateSession(ctx, id, created); err != nil {
			return "", err
		}
		if err := a.setSessionCWD(ctx, id, cwd); err != nil {
			_ = a.store.DeleteSession(context.Background(), id)
			return "", err
		}
	}
	if _, err := a.sessionLogForAgent(ctx, id); err != nil {
		_ = a.store.DeleteSession(context.Background(), id)
		return "", err
	}
	a.markSessionViewed(ctx, id)
	a.extensions.PublishSessionStarted(id)
	return id, nil
}

// resumeAgentSession validates and materializes a web/native session while
// leaving the process-global REPL selection untouched.
func (a *app) resumeAgentSession(ctx context.Context, id string) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("session id is required")
	}
	if _, err := a.sessionLogForAgent(ctx, id); err != nil {
		return err
	}
	a.markSessionViewed(ctx, id)
	return nil
}

func (a *app) runAgentTurn(ctx context.Context, sessionID string, input agent.TurnInput, runtimeAgent *agent.Agent) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	a.beginSessionRun(sessionID)
	defer a.endSessionRun(sessionID)
	log, err := a.sessionLogForAgent(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := a.applyPendingPlanMode(sessionID, log); err != nil {
		return err
	}
	if a.reg == nil {
		return errors.New("tool registry is unavailable")
	}
	registry := a.reg.Clone()
	ownerLog := log
	registry.SetOwner(tools.Owner{SessionID: sessionID, NextSeq: ownerLog.NextSeq})
	// Human/transport-authored input replenishes the bounded completion-wake
	// budget. A pure job notice does not, otherwise a self-triggering chain
	// could reset its own guard on every wake.
	for _, message := range input.Messages {
		if message.Metadata["source"] != "job" {
			a.resetJobWakeBudget(sessionID)
			break
		}
	}
	rt, restore, err := a.applySessionRuntimeOnStrict(sessionID, log, registry)
	if err != nil {
		return err
	}
	defer restore()
	interactive := false
	messages := make([]llm.Message, 0, len(input.Messages))
	var steering []agent.Message
	for _, message := range input.Messages {
		if message.Metadata["interactive"] == "true" {
			interactive = true
		}
		if message.Kind == agent.MessageSteering {
			steering = append(steering, message)
		}
		content := message.Content
		if len(content) == 0 && strings.TrimSpace(message.Text) != "" {
			content = []llm.ContentBlock{llm.Text(message.Text)}
		}
		if len(content) > 0 {
			modelMessage := llm.Message{Role: llm.RoleUser, Content: content}
			applyInboxMessageSource(&modelMessage, message.Metadata)
			if message.Kind == agent.MessageSteering {
				// The canonical steering row is already durable; mark the loop
				// input as projected so step persistence does not duplicate it.
				modelMessage.Persisted = true
			}
			if teamMessageID := message.Metadata["team_message_id"]; teamMessageID != "" {
				modelMessage.SourceKind = "team-message"
				modelMessage.SourceTeamID = message.Metadata["team_id"]
				modelMessage.SourceMessageID = teamMessageID
				modelMessage.SourceSenderID = message.Metadata["team_sender_id"]
				modelMessage.SourceSenderName = message.Metadata["team_sender_name"]
				modelMessage.Persisted = message.Metadata["team_message_recorded"] == "true"
			}
			messages = append(messages, modelMessage)
		}
	}
	for _, message := range steering {
		if _, err := log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); err != nil {
			return err
		}
	}
	if len(messages) == 0 {
		return errors.New("agent input is empty")
	}
	var driver *loop.Loop
	if interactive {
		driver = a.buildLoopBoundWithProvider(func(delta string) { fmt.Print(delta) }, func(err error) { fmt.Fprintln(os.Stderr, "\n[stream error]", err) }, sessionID, rt.provider, rt.model, rt.effort, rt.mode, rt.prompt, log, registry, rt.selected)
	} else {
		driver = a.buildLoopBoundWithProvider(func(string) {}, func(error) {}, sessionID, rt.provider, rt.model, rt.effort, rt.mode, rt.prompt, log, registry, rt.selected)
	}
	// The reference Agent claims the next-step inbox at every proposed step,
	// including the implicit continuation after a tool batch. The legacy bridge
	// only claimed work from turn-stopping, which meant a message queued during
	// a tool call could be invisible until a later turn. Claim here when the
	// loop has no already-supplied continuation; turn-stopping remains the
	// decision point that can supply an explicit next-step batch.
	driver.PrependPreStepHook(func(hctx context.Context, payload loop.PreStepPayload, next loop.PreStepNext) (loop.PreStepDecision, error) {
		if runtimeAgent == nil || len(payload.Messages) > 0 {
			return next(hctx, payload)
		}
		stepInput, ok, claimErr := runtimeAgent.ClaimStepWithError()
		if claimErr != nil {
			return loop.PreStepDecision{}, claimErr
		}
		if !ok {
			return next(hctx, payload)
		}
		claimed := make([]llm.Message, 0, len(stepInput.Messages))
		for _, message := range stepInput.Messages {
			content := message.Content
			if len(content) == 0 && strings.TrimSpace(message.Text) != "" {
				content = []llm.ContentBlock{llm.Text(message.Text)}
			}
			if len(content) == 0 {
				continue
			}
			modelMessage := llm.Message{Role: llm.RoleUser, Content: content}
			applyInboxMessageSource(&modelMessage, message.Metadata)
			if message.Kind == agent.MessageSteering {
				if _, appendErr := log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); appendErr != nil {
					return loop.PreStepDecision{}, appendErr
				}
				modelMessage.Persisted = true
			}
			if teamMessageID := message.Metadata["team_message_id"]; teamMessageID != "" {
				modelMessage.SourceKind = "team-message"
				modelMessage.SourceTeamID = message.Metadata["team_id"]
				modelMessage.SourceMessageID = teamMessageID
				modelMessage.SourceSenderID = message.Metadata["team_sender_id"]
				modelMessage.SourceSenderName = message.Metadata["team_sender_name"]
				modelMessage.Persisted = message.Metadata["team_message_recorded"] == "true"
			}
			claimed = append(claimed, modelMessage)
		}
		proposal := payload
		proposal.Messages = append(cloneRuntimeMessages(payload.Messages), claimed...)
		return next(hctx, proposal)
	})
	driver.AddTurnStoppingHook(func(hctx context.Context, payload loop.TurnStoppingPayload, next loop.TurnStoppingNext) (loop.TurnStoppingDecision, error) {
		stepInput, ok, claimErr := runtimeAgent.ClaimStepWithError()
		if claimErr != nil {
			return loop.TurnStoppingDecision{}, claimErr
		}
		if !ok {
			return next(hctx, payload)
		}
		nextMessages := make([]llm.Message, 0, len(stepInput.Messages))
		for _, message := range stepInput.Messages {
			content := message.Content
			if len(content) == 0 {
				content = []llm.ContentBlock{llm.Text(message.Text)}
			}
			nextMessages = append(nextMessages, llm.Message{Role: llm.RoleUser, Content: content})
		}
		return loop.TurnStoppingDecision{Stop: false, Messages: nextMessages}, nil
	})
	driver.SetContinueOnCancel(func(hctx context.Context) ([]llm.Message, bool, error) {
		stepInput, ok, claimErr := runtimeAgent.ClaimSteerStepWithError()
		if claimErr != nil || !ok {
			return nil, false, claimErr
		}
		nextMessages := make([]llm.Message, 0, len(stepInput.Messages))
		for _, message := range stepInput.Messages {
			content := message.Content
			if len(content) == 0 {
				content = []llm.ContentBlock{llm.Text(message.Text)}
			}
			persisted := false
			if message.Kind == agent.MessageSteering {
				if _, err := log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); err != nil {
					return nil, false, err
				}
				persisted = true
			}
			nextMessages = append(nextMessages, llm.Message{Role: llm.RoleUser, Content: content, Persisted: persisted})
		}
		if err := hctx.Err(); err != nil && len(nextMessages) == 0 {
			return nil, false, err
		}
		return nextMessages, true, nil
	})
	return driver.RunMessages(ctx, messages)
}

func cloneRuntimeMessages(messages []llm.Message) []llm.Message {
	cloned := make([]llm.Message, len(messages))
	copy(cloned, messages)
	for index := range cloned {
		cloned[index].Content = append([]llm.ContentBlock(nil), cloned[index].Content...)
		cloned[index].ToolCalls = append([]llm.ToolCall(nil), cloned[index].ToolCalls...)
	}
	return cloned
}

// applyInboxMessageSource projects producer-owned inbox provenance onto the
// model message. The durable source is presentation and provenance only; it
// never becomes part of provider wire content.
func applyInboxMessageSource(message *llm.Message, metadata map[string]string) {
	message.SourceRPCID = strings.TrimSpace(metadata["rpc_id"])
	message.SourceClientTimeZone = strings.TrimSpace(metadata["client_time_zone"])
	kind := strings.TrimSpace(metadata["source_kind"])
	if kind == "" {
		return
	}
	message.SourceKind = kind
	message.SourcePlugin = strings.TrimSpace(metadata["source_plugin"])
	message.SourceForm = strings.TrimSpace(metadata["source_form"])
	message.SourceName = strings.TrimSpace(metadata["source_name"])
	message.SourceSummary = strings.TrimSpace(metadata["source_summary"])
	message.SourceSenderSessionID = strings.TrimSpace(metadata["source_sender_session_id"])
}
