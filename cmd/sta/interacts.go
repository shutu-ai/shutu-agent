// interacts.go — the M6d-2 composition-root orchestration (dispatch-m6d-2
// §4/§5). This is where the human-approval capability seam is wired into the
// REPL: registerInteracts creates the in-memory Provider + Engine, registers
// the two interact_* tools and installs the sensitive-tool gate on the tool
// registry when interact.enabled (D10), and wires the D3 event sink so
// interact/* events are appended to the active session log.
//
// The sensitive-tool gate is the ADR 决策 M6d 落地 (design.md §10 D5): when
// interact.sensitive_tools is non-empty and the model requests a gated tool,
// the gate first Engine.Requests a human approval, then blocks on the CLI
// serial path reading the user's y/n answer from the terminal, then records the
// decision (Engine.Resolve) and re-reads it through Engine.Await (the
// caller-driven poll — the resolution made here is visible on the next probe).
// Approved lets the tool run; rejected returns a denial the model sees as a
// tool/error. The loop's turn/step structure is untouched (D4): the gate hangs
// off the tools registry's pre-execution hook (tools.AddPreExecuteHook), not the loop.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

type webApprovalContextKey struct{}

func withWebApprovalContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, webApprovalContextKey{}, true)
}

func isWebApprovalContext(ctx context.Context) bool {
	value, _ := ctx.Value(webApprovalContextKey{}).(bool)
	return value
}

// approveArgsBound mirrors the approval engine's stored-args bound (interact
// maxArgsLen, 200 runes): the gate trims an over-long tool args payload to it
// so the approval Request can never fail on the gate path.
const approveArgsBound = 200

// registerInteracts creates the in-memory Provider + Engine, registers the two
// interact_* tools and installs the sensitive-tool gate when interact.enabled,
// and wires the D3 event sink. When interact is disabled it creates nothing and
// registers nothing (D10, mirrors registerJobs/registerSkills). It must run
// after every other register* so the sensitive-tool gate can see the full
// registered tool set (it is called last in main.go).
func (a *app) registerInteracts() error {
	if !config.Enabled(a.cfg.Interact.Enabled) {
		return nil
	}
	var prov interact.Provider = interact.NewMemProvider()
	if backend, ok := a.store.(store.ApprovalStore); ok {
		var err error
		prov, err = interact.NewSQLiteProvider(backend)
		if err != nil {
			return fmt.Errorf("sta: approval provider: %w", err)
		}
	}
	eng := interact.NewEngine(prov)
	if controller, ok := interface{}(eng).(interact.PolicyController); ok && a.cfg.Interact.Policy != "" {
		if err := controller.SetDefaultPolicy(interact.ApprovalPolicy(a.cfg.Interact.Policy)); err != nil {
			return fmt.Errorf("sta: approval policy: %w", err)
		}
	}
	if auditor, ok := interface{}(eng).(interact.ExpiryAuditor); ok {
		auditor.SetExpiryAuditor(a.auditExpiredInteraction)
	}
	a.interacts = eng
	if a.store != nil {
		// Rebuild from the event log on every startup. The SQLite projection is a
		// live CAS/index, not a second authority: this also removes orphan rows
		// from a crash between provider mutation and audit-event append.
		if err := a.restoreInteractions(context.Background(), eng); err != nil {
			return fmt.Errorf("sta: restore interactions: %w", err)
		}
		if err := a.indexLiveInteractions(context.Background(), eng); err != nil {
			return fmt.Errorf("sta: index interactions: %w", err)
		}
	}
	// D3 event sink: interact/* events are appended to the active session log.
	// The callback only ever runs inside an interact_* tool Execute or the
	// sensitive-tool gate — the serial main-loop path (D5). a.log is read at
	// call time, so a session switch (/new, /resume) is honored the same way
	// as the other session-bound event wiring.
	onEventErr := func(typ string, data any) error {
		if typ == session.EventInteractRequest {
			var request struct {
				ID     string `json:"id"`
				CallID string `json:"callId"`
			}
			if raw, err := json.Marshal(data); err == nil {
				_ = json.Unmarshal(raw, &request)
			}
			a.rememberInteraction(request.ID, a.currentID, request.CallID)
		}
		_, err := a.log.Append(typ, data)
		return err
	}
	onEvent := func(typ string, data any) {
		if err := onEventErr(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", err)
		}
	}
	st := interact.NewInteractToolsWithSessionAndErrorSink(eng, onEvent, onEventErr, func() string { return a.currentID })
	// Give both model-facing question tools the same durable creation seam as
	// the sensitive-tool and ACP answerers. The resolver is evaluated per call
	// so child/ACP sessions audit into their own transcript.
	st.SetSessionLogResolver(a.interactionLogFor)
	for _, t := range []tools.Tool{st.AskUserQuestion()} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("sta: register %s: %w", t.Name(), err)
		}
	}
	// Sensitive-tool gate: install the registry pre-execution gate when
	// sensitive_tools is non-empty (an enabled interact with an empty list
	// registers only the interact_* tools — no gating).
	if len(a.cfg.Interact.SensitiveTools) > 0 {
		gate := a.sensitiveGate(eng, onEvent, onEventErr)
		a.reg.AddPreExecuteHook(func(ctx context.Context, exec tools.Execution) (tools.PreToolDecision, error) {
			if err := gate(ctx, exec.Name, exec.Arguments, exec.CallID); err != nil {
				return tools.PreToolDecision{Kind: "deny", Reason: err.Error()}, nil
			}
			return tools.PreToolDecision{Kind: "allow"}, nil
		})
	}
	return nil
}

// auditExpiredInteraction projects the provider's expiry transition into the
// owning session. Expiry is evaluated lazily by reads, so this callback is
// deliberately idempotent: a failed live read may retry the audit without
// creating duplicate approval/decided facts.
func (a *app) auditExpiredInteraction(ctx context.Context, request interact.Request) error {
	if a == nil || request.SessionID == "" || request.ID == "" {
		return nil
	}
	var log *session.Log
	var err error
	if a.agentRegistry == nil && request.SessionID == a.currentID {
		log = a.log
	} else {
		log, err = a.sessionLogForAgent(ctx, request.SessionID)
		if err != nil {
			return err
		}
	}
	if log == nil {
		return errors.New("approval session log is unavailable")
	}
	for _, event := range log.Events() {
		if event.Type != session.EventApprovalDecided {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.ID == request.ID {
			return nil
		}
	}
	payload := map[string]any{"id": request.ID, "outcome": string(interact.StatusExpired)}
	if request.CallID != "" {
		payload["callId"] = request.CallID
	}
	_, err = log.Append(session.EventApprovalDecided, payload)
	return err
}

// indexLiveInteractions rebuilds the transport ownership/correlation index
// from the provider projection. SQLite is the source of truth when present;
// the same helper also keeps the legacy memory-backed test path consistent.
func (a *app) indexLiveInteractions(ctx context.Context, eng interact.Engine) error {
	items, err := eng.List(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != "" && item.SessionID != "" {
			a.rememberInteraction(item.ID, item.SessionID, item.CallID)
		}
	}
	return nil
}

// restoreInteractions rebuilds the process-local approval table and its
// session ownership index from durable interact/request + interact/resolve
// facts. Old request events without detail remain visible with a safe fallback
// prompt, while newer events restore the full approval card.
func (a *app) restoreInteractions(ctx context.Context, restorer interface {
	Restore(context.Context, []interact.Request) error
}) error {
	metas, err := a.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	type requestFact struct {
		ID        string              `json:"id"`
		CallID    string              `json:"callId"`
		ToolName  string              `json:"toolName"`
		Reason    string              `json:"reason"`
		Prompt    string              `json:"prompt"`
		Args      string              `json:"args"`
		Questions []interact.Question `json:"questions"`
	}
	type resolveFact struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
		Answer   string `json:"answer"`
	}
	requests := make(map[string]interact.Request)
	owners := make(map[string]string)
	callIDs := make(map[string]string)
	ambiguous := make(map[string]struct{})
	// Approval restoration is a control-plane projection, not session replay.
	// Read the committed rows exactly when SQLite exposes the raw seam: an old
	// or interrupted transcript must not prevent the process from starting just
	// because it contains unrelated lifecycle damage. Session opening still uses
	// the strict recovery-aware LoadSession path and will report that damage at
	// the point where the transcript is actually consumed.
	readSession := a.store.LoadSession
	if raw, ok := a.store.(store.SessionRawStore); ok {
		readSession = raw.LoadSessionRaw
	}
	for _, meta := range metas {
		events, err := readSession(ctx, meta.ID)
		if err != nil {
			return err
		}
		for _, ev := range events {
			switch ev.Type {
			case session.EventInteractRequest, session.EventApprovalAsked:
				var fact requestFact
				if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				prompt := fact.Prompt
				if prompt == "" {
					prompt = fact.Reason
				}
				if prompt == "" {
					prompt = fmt.Sprintf("Approval required for %s", fact.ToolName)
				}
				if _, conflicted := ambiguous[fact.ID]; conflicted {
					continue
				}
				if previous, exists := owners[fact.ID]; exists && previous != meta.ID {
					// Legacy req-N IDs were not session-scoped. Do not let one
					// restored session silently take ownership of another session's
					// approval; an ambiguous request is safer to hide than to resolve
					// under the wrong principal.
					delete(requests, fact.ID)
					delete(owners, fact.ID)
					ambiguous[fact.ID] = struct{}{}
					continue
				}
				requests[fact.ID] = interact.Request{
					ID: fact.ID, SessionID: meta.ID, CallID: fact.CallID, Prompt: prompt, ToolName: fact.ToolName, Args: fact.Args,
					Questions: fact.Questions, Status: interact.StatusPending, CreatedAt: ev.At,
				}
				owners[fact.ID] = meta.ID
				if fact.CallID != "" {
					callIDs[fact.ID] = fact.CallID
				}
			case session.EventInteractResolve, session.EventApprovalDecided:
				var fact resolveFact
				var canonical struct {
					ID      string `json:"id"`
					Outcome string `json:"outcome"`
				}
				resolvedStatus := interact.StatusRejected
				if ev.Type == session.EventApprovalDecided {
					if err := json.Unmarshal(ev.Data, &canonical); err != nil || canonical.ID == "" {
						continue
					}
					var detail struct {
						ID      string `json:"id"`
						Outcome string `json:"outcome"`
						Answer  string `json:"answer"`
					}
					_ = json.Unmarshal(ev.Data, &detail)
					fact.ID = canonical.ID
					fact.Answer = detail.Answer
					fact.Approved = canonical.Outcome == string(interact.StatusApproved) || canonical.Outcome == string(interact.StatusAllowedOnce)
					switch canonical.Outcome {
					case string(interact.StatusAllowedOnce):
						resolvedStatus = interact.StatusAllowedOnce
					case string(interact.StatusCanceled):
						resolvedStatus = interact.StatusCanceled
					case string(interact.StatusUnavailable):
						resolvedStatus = interact.StatusUnavailable
					case string(interact.StatusExpired):
						resolvedStatus = interact.StatusExpired
					case string(interact.StatusApproved):
						resolvedStatus = interact.StatusApproved
					}
				} else if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				if fact.ID == "" {
					continue
				}
				if _, conflicted := ambiguous[fact.ID]; conflicted {
					continue
				}
				r, ok := requests[fact.ID]
				if !ok {
					continue
				}
				if ev.Type == session.EventInteractResolve {
					if fact.Approved {
						resolvedStatus = interact.StatusApproved
					}
				}
				r.Status = resolvedStatus
				if fact.Answer != "" {
					r.Answer = fact.Answer
				}
				resolvedAt := ev.At
				r.ResolvedAt = &resolvedAt
				requests[fact.ID] = r
			case session.EventInteractCancel:
				var fact struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				if _, conflicted := ambiguous[fact.ID]; conflicted {
					continue
				}
				r, ok := requests[fact.ID]
				if !ok {
					continue
				}
				r.Status = interact.StatusCanceled
				resolvedAt := ev.At
				r.ResolvedAt = &resolvedAt
				requests[fact.ID] = r
			}
		}
	}
	restored := make([]interact.Request, 0, len(requests))
	for _, request := range requests {
		restored = append(restored, request)
	}
	if replacer, ok := restorer.(interact.RequestReplacer); ok {
		if err := replacer.Replace(ctx, restored); err != nil {
			return err
		}
	} else if err := restorer.Restore(ctx, restored); err != nil {
		return err
	}
	a.interactionMu.Lock()
	if a.interactionSessions == nil {
		a.interactionSessions = make(map[string]string)
	}
	if a.interactionCallIDs == nil {
		a.interactionCallIDs = make(map[string]string)
	}
	for id, owner := range owners {
		a.interactionSessions[id] = owner
	}
	for id, callID := range callIDs {
		a.interactionCallIDs[id] = callID
	}
	a.interactionMu.Unlock()
	return nil
}

func (a *app) interactionBelongsTo(id, sessionID string) bool {
	if sessionID == "" {
		return true
	}
	owner, known := a.interactionOwner(id)
	return known && owner == sessionID
}

// interactionOwner distinguishes an unknown process-local correlation from a
// known correlation owned by another session. Durable providers can receive a
// request written by a different process after startup; rejecting an unknown
// local id before consulting the provider would make that valid answer
// permanently unanswerable. The provider's scoped listing remains the
// authority in that case, while a known mismatched owner still fails closed.
func (a *app) interactionOwner(id string) (string, bool) {
	a.interactionMu.RLock()
	owner, known := a.interactionSessions[id]
	a.interactionMu.RUnlock()
	return owner, known
}

// interactionSession repairs the process-local correlation after a Web/native
// reconnect. A request may have been created by another process after this
// app indexed its approvals; the durable provider is authoritative for the
// owner in that case. Only the session id is returned, never approval content.
func (a *app) interactionSession(ctx context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if owner, known := a.interactionOwner(id); known && owner != "" {
		return owner, nil
	}
	if a == nil || a.interacts == nil || id == "" {
		return "", interact.ErrUnknownRequest
	}
	items, err := a.interacts.List(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.ID == id && strings.TrimSpace(item.SessionID) != "" {
			a.rememberInteraction(item.ID, item.SessionID, item.CallID)
			return item.SessionID, nil
		}
	}
	return "", interact.ErrUnknownRequest
}

func (a *app) interactionCallID(id string) string {
	a.interactionMu.RLock()
	callID := a.interactionCallIDs[id]
	a.interactionMu.RUnlock()
	return callID
}

func (a *app) rememberInteraction(id, sessionID, callID string) {
	if id == "" {
		return
	}
	a.interactionMu.Lock()
	if a.interactionSessions == nil {
		a.interactionSessions = make(map[string]string)
	}
	a.interactionSessions[id] = sessionID
	if callID != "" {
		if a.interactionCallIDs == nil {
			a.interactionCallIDs = make(map[string]string)
		}
		a.interactionCallIDs[id] = callID
	}
	a.interactionMu.Unlock()
}

func (a *app) forgetInteraction(id string) {
	if id == "" {
		return
	}
	a.interactionMu.Lock()
	delete(a.interactionSessions, id)
	delete(a.interactionCallIDs, id)
	a.interactionMu.Unlock()
}

// resolveInteractionDurably is the common Web/CLI answer transition. The
// approval engine remains the live state machine, while the session log is
// the recovery source of truth. The request is serialized so at most one
// answerer can win. If the durable append fails, restore the exact pending
// request; this is important because Engine.Resolve intentionally has no
// public "unresolve" operation.
func (a *app) resolveInteractionDurably(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error {
	return a.resolveInteractionDurablyAs(ctx, sessionID, id, status, answer, false)
}

func (a *app) resolveInteractionDurablyAs(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string, compatibilityEvent bool) error {
	a.interactionResolveMu.Lock()
	defer a.interactionResolveMu.Unlock()
	if a.interacts == nil {
		return interact.ErrUnknownRequest
	}
	if sessionID != "" {
		if owner, known := a.interactionOwner(id); known && owner != sessionID {
			return interact.ErrUnknownRequest
		}
	}
	var (
		items []interact.Request
		err   error
	)
	if sessionID != "" {
		if lister, ok := a.interacts.(interact.SessionLister); ok {
			items, err = lister.ListForSession(ctx, sessionID)
		} else {
			items, err = a.interacts.List(ctx)
		}
	} else {
		items, err = a.interacts.List(ctx)
	}
	if err != nil {
		return err
	}
	var before interact.Request
	found := false
	for _, item := range items {
		if item.ID == id {
			before, found = item, true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s", interact.ErrUnknownRequest, id)
	}
	owner := strings.TrimSpace(sessionID)
	if owner == "" {
		owner = before.SessionID
	}
	if owner == "" {
		owner = a.currentID
	}
	if before.SessionID != "" && owner != before.SessionID {
		return fmt.Errorf("%w: %s", interact.ErrWrongSession, id)
	}
	rollback := func(cause error) error {
		restorer, ok := a.interacts.(interact.RequestRestorer)
		if !ok {
			return fmt.Errorf("%w (approval state cannot be rolled back)", cause)
		}
		if restoreErr := restorer.Restore(context.Background(), []interact.Request{before}); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback approval state: %w", restoreErr))
		}
		a.forgetInteraction(id)
		if before.SessionID != "" {
			a.rememberInteraction(id, before.SessionID, before.CallID)
		}
		return cause
	}
	callID := before.CallID
	if callID == "" {
		callID = a.interactionCallID(id)
	}
	target := a.log
	canonical := owner != "" && !compatibilityEvent
	if owner != "" && (a.agentRegistry != nil || owner != a.currentID) {
		target, err = a.sessionLogForAgent(ctx, owner)
		if err != nil {
			return rollback(fmt.Errorf("approval owner session %q is unavailable: %w", owner, err))
		}
	}
	if target == nil {
		return rollback(errors.New("approval durable log is unavailable"))
	}
	var eventType string
	var eventData any
	if status == interact.StatusCanceled {
		if canonical {
			eventType, eventData = session.EventApprovalDecided, map[string]any{"id": id, "outcome": string(interact.StatusCanceled)}
			if callID != "" {
				eventData.(map[string]any)["callId"] = callID
			}
		} else {
			eventType, eventData = session.EventInteractCancel, session.NewInteractCancel(id)
		}
	} else if canonical {
		outcome := string(interact.StatusRejected)
		if status == interact.StatusApproved || status == interact.StatusAllowedOnce {
			outcome = string(interact.StatusAllowedOnce)
		} else if status == interact.StatusExpired {
			outcome = string(interact.StatusExpired)
		}
		payload := map[string]any{"id": id, "outcome": outcome}
		if callID != "" {
			payload["callId"] = callID
		}
		if answer != "" {
			payload["answer"] = answer
		}
		eventType, eventData = session.EventApprovalDecided, payload
	} else {
		eventType, eventData = session.EventInteractResolve, session.NewInteractResolveWithCallID(id, callID, status == interact.StatusApproved || status == interact.StatusAllowedOnce)
	}
	raw, marshalErr := json.Marshal(eventData)
	if marshalErr != nil {
		return fmt.Errorf("encode approval decision: %w", marshalErr)
	}
	event := session.Event{Seq: target.NextSeq(), Type: eventType, At: time.Now().UTC(), Version: session.EventVersion, Data: raw}
	var resolved interact.Request
	atomic := false
	if resolver, ok := a.interacts.(interact.AtomicEventResolver); ok && owner != "" {
		resolved, atomic, err = resolver.ResolveForSessionWithAnswerAndEvent(ctx, owner, id, status, answer, event)
	} else if resolver, ok := a.interacts.(interact.SessionResolver); ok && owner != "" {
		resolved, err = resolver.ResolveForSessionWithAnswer(ctx, owner, id, status, answer)
	} else if resolver, ok := a.interacts.(interact.AnswerResolver); ok {
		resolved, err = resolver.ResolveWithAnswer(ctx, id, status, answer)
	} else {
		resolved, err = a.interacts.Resolve(ctx, id, status)
	}
	if err != nil {
		return err
	}
	_ = resolved
	if atomic {
		if err := target.AppendPersisted(event); err != nil {
			// The database transaction has already committed both facts. A
			// projection mismatch is recoverable by the next session reload and
			// must not undo the durable decision without its audit event.
			return fmt.Errorf("project committed approval decision: %w", err)
		}
	} else if _, err := target.Append(eventType, eventData); err != nil {
		return rollback(fmt.Errorf("persist approval decision: %w", err))
	}
	return nil
}

func (a *app) emitInteractionEvent(ctx context.Context, fallback func(string, any), typ string, data any) error {
	return a.emitInteractionEventWithErrorSink(ctx, fallback, nil, typ, data)
}

func (a *app) emitInteractionEventWithErrorSink(ctx context.Context, fallback func(string, any), fallbackErr func(string, any) error, typ string, data any) error {
	if log := a.runtimeLog(ctx); log != nil {
		if typ == session.EventInteractRequest {
			var request struct {
				ID     string `json:"id"`
				CallID string `json:"callId"`
			}
			if raw, err := json.Marshal(data); err == nil {
				_ = json.Unmarshal(raw, &request)
			}
			a.rememberInteraction(request.ID, a.runtimeSessionID(ctx), request.CallID)
		}
		if runtimectx.SessionID(ctx) != "" {
			if canonical, value, projected := session.CanonicalApprovalEvent(typ, data); projected {
				if _, err := log.Append(canonical, value); err != nil {
					return fmt.Errorf("sta: persist %s event: %w", canonical, err)
				}
			} else if _, err := log.Append(typ, data); err != nil {
				return fmt.Errorf("sta: persist %s event: %w", typ, err)
			}
		} else if _, err := log.Append(typ, data); err != nil {
			return fmt.Errorf("sta: persist %s event: %w", typ, err)
		}
		return nil
	}
	if fallback != nil {
		if fallbackErr != nil {
			return fallbackErr(typ, data)
		}
		fallback(typ, data)
	}
	return nil
}

// sensitiveGate returns the registry pre-execution gate for the configured
// sensitive tools (ADR 决策 M6d / dispatch-m6d-2 §4). The registry calls it for
// every whitelisted execution; tools outside sensitive_tools pass through
// untouched. A gated tool first creates a pending approval request, then blocks
// on the CLI serial path reading the user's y/n answer from the terminal (D5),
// then records the decision (Engine.Resolve) and re-reads it through
// Engine.Await (the caller-driven poll — the resolution made here becomes
// visible on the next probe). Approved returns nil and the tool runs; rejected
// appends the interact/deny fact and returns a denial the model sees as a
// tool/error.
func (a *app) sensitiveGate(eng interact.Engine, onEvent func(string, any), fallbackErrs ...func(string, any) error) func(context.Context, string, any, ...string) error {
	configSnapshot := a.providerConfigSnapshot()
	sensitive := append([]string(nil), configSnapshot.Interact.SensitiveTools...)
	return func(ctx context.Context, name string, args any, callIDs ...string) error {
		var fallbackErr func(string, any) error
		if len(fallbackErrs) > 0 {
			fallbackErr = fallbackErrs[0]
		}
		callID := ""
		if len(callIDs) > 0 {
			callID = callIDs[0]
		}
		if !containsSensitive(sensitive, name) {
			return nil // not a sensitive tool; no approval needed
		}
		if correlation, ok := runtimectx.CorrelationOf(ctx); ok && correlation.TurnID != "" {
			approvalLog, logErr := a.interactionLogFor(ctx)
			if logErr != nil {
				return fmt.Errorf("interact: %s approval log unavailable: %w", name, logErr)
			}
			if approvalLog == nil || !session.HasOpenTurn(approvalLog.Events()) {
				return fmt.Errorf("interact: %s approval request requires an open turn", name)
			}
		}
		rawArgs, _ := json.Marshal(args)
		argsText := boundRunes(string(rawArgs))
		prompt := fmt.Sprintf("Allow the sensitive tool %s to run? args: %s", name, argsText)
		var req interact.Request
		var err error
		atomicAsked := false
		var askedEvent session.Event
		if requester, ok := interface{}(eng).(interact.AtomicSessionCallRequester); ok {
			if approvalLog, logErr := a.interactionLogFor(ctx); logErr == nil && approvalLog != nil {
				req, atomicAsked, err = requester.RequestForSessionWithCallIDAndEvent(ctx, a.runtimeSessionID(ctx), callID, prompt, name, argsText, func(created interact.Request) session.Event {
					payload := session.NewInteractRequestDetailWithCallID(created.ID, callID, name, created.Prompt, created.Args, created.Questions)
					askedEvent = session.Event{Seq: approvalLog.NextSeq(), Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: marshalInteractionData(payload)}
					return askedEvent
				})
			} else {
				// A runtime log is required to supply the exact next sequence. The
				// compatibility request path below remains available for bare tests.
				err = logErr
			}
		} else if requester, ok := interface{}(eng).(interact.SessionCallRequester); ok {
			req, err = requester.RequestForSessionWithCallID(ctx, a.runtimeSessionID(ctx), callID, prompt, name, argsText)
		} else if requester, ok := interface{}(eng).(interact.SessionRequester); ok {
			req, err = requester.RequestForSession(ctx, a.runtimeSessionID(ctx), prompt, name, argsText)
		} else {
			req, err = eng.Request(ctx, prompt, name, argsText)
		}
		if err != nil {
			return fmt.Errorf("interact: %s approval request failed: %w", name, err)
		}
		if req.Status == interact.StatusUnavailable {
			return fmt.Errorf("interact: %s approval unavailable by policy", name)
		}
		if req.Status == interact.StatusRejected && req.ID == "" {
			return fmt.Errorf("interact: %s denied by approval policy", name)
		}
		ownerSession := a.runtimeSessionID(ctx)
		if ownerSession == "" {
			ownerSession = a.currentID
		}
		// The atomic provider path bypasses the legacy interact/request event
		// callback, so install the transport ownership index explicitly. This is
		// also what lets a Web/CLI answerer find the request after a cold restore.
		a.rememberInteraction(req.ID, ownerSession, callID)
		if atomicAsked {
			if approvalLog, logErr := a.interactionLogFor(ctx); logErr != nil {
				a.forgetInteraction(req.ID)
				return fmt.Errorf("interact: %s approval log unavailable: %w", name, logErr)
			} else if err := approvalLog.AppendPersisted(askedEvent); err != nil {
				a.forgetInteraction(req.ID)
				return fmt.Errorf("interact: %s approval event adoption failed: %w", name, err)
			}
		} else if err := a.emitInteractionEventWithErrorSink(ctx, onEvent, fallbackErr, session.EventInteractRequest, session.NewInteractRequestDetailWithCallID(req.ID, callID, name, req.Prompt, req.Args, req.Questions)); err != nil {
			// The request is not visible until its durable asked fact commits.
			// Roll back the in-memory pending item on append failure; otherwise a
			// retry would leave an orphan approval that can never be answered.
			if canceler, ok := eng.(interact.Canceler); ok && req.ID != "" {
				_, _ = canceler.Cancel(context.Background(), req.ID)
			}
			a.forgetInteraction(req.ID)
			return fmt.Errorf("interact: %s approval request persistence failed: %w", name, err)
		}
		approved := false
		if isWebApprovalContext(ctx) {
			// The browser resolves the same engine through /api/interactions;
			// Await keeps the serial tool path blocked until that decision arrives.
			var resolved interact.Request
			var awaitErr error
			if runtimeSession := runtimectx.SessionID(ctx); runtimeSession != "" {
				awaiter, ok := eng.(interact.SessionAwaiter)
				if !ok {
					return fmt.Errorf("interact: %s web approval engine lacks session-scoped waiting", name)
				}
				resolved, awaitErr = awaiter.AwaitForSession(ctx, runtimeSession, req.ID)
			} else {
				resolved, awaitErr = eng.Await(ctx, req.ID)
			}
			if awaitErr != nil {
				return fmt.Errorf("interact: %s web approval wait failed: %w", name, awaitErr)
			}
			approved = resolved.Status == interact.StatusApproved || resolved.Status == interact.StatusAllowedOnce
		} else {
			var err error
			approved, err = a.approvePrompt(req.ID, prompt)
			if err != nil {
				return fmt.Errorf("interact: %s approval read failed: %w", name, err)
			}
			status := interact.StatusRejected
			if approved {
				// A CLI grant is the same one-shot outcome as Web and ACP.
				status = interact.StatusAllowedOnce
			}
			// Agent-owned calls use the same canonical approval transition as Web
			// and ACP. Only direct legacy callers without a runtime identity keep
			// the old interact/resolve event shape for compatibility readers.
			compatibilityEvent := runtimectx.SessionID(ctx) == ""
			owner := a.runtimeSessionID(ctx)
			if owner == "" {
				owner = a.currentID
			}
			if err := a.approvalAnswerer().Resolve(ctx, owner, req.ID, status, "", compatibilityEvent); err != nil {
				return fmt.Errorf("interact: %s approval resolve failed: %w", name, err)
			}
		}
		if !approved {
			if err := a.emitInteractionEventWithErrorSink(ctx, onEvent, fallbackErr, session.EventInteractDeny, session.NewInteractDeny(req.ID)); err != nil {
				return fmt.Errorf("interact: %s denial persistence failed: %w", name, err)
			}
			return fmt.Errorf("interact: %s denied by user (request %s)", name, req.ID)
		}
		return nil
	}
}

func marshalInteractionData(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

// interactionLogFor resolves the same session-owned log used by the current
// runtime turn. It prevents an approval requested by a child/ACP Agent from
// being audited into the mutable REPL session.
func (a *app) interactionLogFor(ctx context.Context) (*session.Log, error) {
	sessionID := a.runtimeSessionID(ctx)
	if sessionID == "" || (a.agentRegistry == nil && sessionID == a.currentID) {
		if a.log == nil {
			return nil, errors.New("session log is unavailable")
		}
		return a.log, nil
	}
	a.runtimeMu.Lock()
	log := a.runtimeLogs[sessionID]
	a.runtimeMu.Unlock()
	if log != nil {
		return log, nil
	}
	return a.sessionLogForAgent(ctx, sessionID)
}

// approvePrompt prints the approval request to the terminal and blocks reading
// the user's y/n answer on the serial path (D5). It reads from a.approveInput
// (os.Stdin by default) so the wiring tests can inject canned answers. Bare
// Enter and EOF count as no (fail-closed for a security gate); y/yes allow,
// n/no deny, anything else re-prompts.
func (a *app) approvePrompt(id, prompt string) (bool, error) {
	fmt.Printf("\n⚠ approval request %s\n%s\n", id, prompt)
	in := a.approveInput
	if in == nil {
		in = os.Stdin
	}
	r := bufio.NewReader(in)
	for {
		fmt.Print("  allow execution? [y/N] > ")
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
	}
}

// containsSensitive reports whether name is listed in the configured sensitive
// tools (exact match, mirroring the whitelist's own exact-name semantics in
// tools.Policy.Allows).
func containsSensitive(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// boundRunes trims s to approveArgsBound runes so an over-long tool args
// payload can never make the approval Request fail on the gate path.
func boundRunes(s string) string {
	runes := []rune(s)
	if len(runes) > approveArgsBound {
		return string(runes[:approveArgsBound])
	}
	return s
}
