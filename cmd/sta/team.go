package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/team"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// registerTeam wires the session-scoped task/mailbox surface alongside the
// subagent runtime. The board resolver is deliberately lazy: sessions that
// never use Teams do not allocate a board. A future durable adapter can replace
// teamBoard without changing any model-facing tool contract.
func (a *app) registerTeam() error {
	if !config.Enabled(a.cfg.Subagent.Enabled) {
		return nil
	}
	adapter := team.NewToolsWithContext(func(ctx context.Context, sessionID string) (*team.Board, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		board, err := a.teamBoard(sessionID)
		if err != nil {
			return nil, err
		}
		board.SetMessageDispatcher(func(dispatchCtx context.Context, message team.Message) (bool, error) {
			return a.dispatchTeamMessage(dispatchCtx, sessionID, message)
		})
		if err := a.redeliverPendingTeamMessages(ctx, sessionID, board); err != nil {
			return nil, err
		}
		return board, nil
	}, func(ctx context.Context) (string, string) {
		id := a.runtimeSessionID(ctx)
		id = strings.TrimSpace(id)
		return a.teamRootSessionID(id), id
	})
	adapter.SetSnapshotSink(func(ctx context.Context, sessionID string, snapshot team.Snapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		log, err := a.sessionLogForAgent(ctx, a.teamRootSessionID(sessionID))
		if err != nil {
			return fmt.Errorf("team: session %q runtime log is unavailable: %w", sessionID, err)
		}
		_, err = log.Append(session.EventTeamSnapshot, snapshot)
		return err
	})
	adapter.SetEventSink(func(ctx context.Context, sessionID, typ string, value any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		log, err := a.sessionLogForAgent(ctx, a.teamRootSessionID(sessionID))
		if err != nil {
			return fmt.Errorf("team: session %q runtime log is unavailable: %w", sessionID, err)
		}
		if typ == session.EventTeamMessageDelivered {
			var delivered team.MessageDeliveredEvent
			if jsonBytes, marshalErr := json.Marshal(value); marshalErr == nil && json.Unmarshal(jsonBytes, &delivered) == nil && teamDeliveryRecorded(log, delivered.TeamID, delivered.MessageID, delivered.TargetID) {
				// A live SQLite dispatcher may have committed this edge together
				// with the target receipt before the Team tool reaches its
				// compatibility saveEvent callback. Treat that replay as idempotent.
				return nil
			}
		}
		_, err = log.Append(typ, value)
		return err
	})
	registrationSession := ""
	if a.agentRegistry == nil {
		registrationSession = a.currentID
	}
	for _, item := range adapter.Tools() {
		tool, ok := item.(tools.Tool)
		if !ok {
			return fmt.Errorf("sta: team tool %T does not implement tools.Tool", item)
		}
		if err := a.reg.RegisterWithInfo(tool, tools.RegistrationInfo{Owner: "agent-teams", Plugin: "builtin-team", SessionID: registrationSession}); err != nil {
			return fmt.Errorf("sta: register %s: %w", tool.Name(), err)
		}
	}
	if a.subagentTools != nil {
		a.subagentTools.SetTeammateProvisioner(a.provisionTeammate)
		a.subagentTools.SetTeammateDirectory(appTeammateDirectory{app: a})
	}
	return nil
}

// appTeammateDirectory adapts the durable Team roster to the subagent control
// plane. It only exposes active roster members and obtains all live callbacks
// from the rebound Agent handle, so a restored row without a handle cannot be
// accidentally reported as controllable.
type appTeammateDirectory struct{ app *app }

func (d appTeammateDirectory) List(ctx context.Context, parent string) ([]subagent.Teammate, error) {
	board, err := d.app.teamBoard(parent)
	if err != nil {
		return nil, err
	}
	roster := board.Roster()
	if roster == nil {
		return nil, nil
	}
	result := make([]subagent.Teammate, 0)
	for _, member := range roster.List() {
		if member.ID == string(roster.LeadID()) || member.Phase != team.MemberActive {
			continue
		}
		teammate, ok := d.teammate(ctx, roster, member)
		if ok {
			result = append(result, teammate)
		}
	}
	return result, nil
}

func (d appTeammateDirectory) Direct(ctx context.Context, parent, target string) (subagent.Teammate, error) {
	if strings.TrimSpace(parent) != "" {
		board, err := d.app.teamBoard(parent)
		if err != nil {
			return subagent.Teammate{}, err
		}
		return d.directFromRoster(ctx, board.Roster(), parent, target)
	}

	// External control calls may have only a durable member id. Include boards
	// reconstructed from the store, not only boards touched by this process;
	// otherwise a reconnect after restart cannot address a valid teammate.
	ids := d.app.durableTeamRootIDs(ctx)
	for _, id := range ids {
		board, err := d.app.teamBoard(id)
		if err != nil {
			continue
		}
		if member, memberErr := d.directFromRoster(ctx, board.Roster(), id, target); memberErr == nil {
			return member, nil
		}
	}
	return subagent.Teammate{}, fmt.Errorf("team: teammate %q not found", target)
}

func (d appTeammateDirectory) directFromRoster(ctx context.Context, roster *team.Roster, parent, target string) (subagent.Teammate, error) {
	if roster == nil {
		return subagent.Teammate{}, fmt.Errorf("team: roster unavailable")
	}
	target = strings.TrimSpace(target)
	for _, member := range roster.List() {
		if member.ID == string(roster.LeadID()) || member.Phase != team.MemberActive {
			continue
		}
		if target != member.ID && target != member.Name && !strings.HasPrefix(member.Name, target+": ") {
			continue
		}
		teammate, ok := d.teammate(ctx, roster, member)
		if !ok {
			return subagent.Teammate{}, fmt.Errorf("team: teammate %q is not rebound", target)
		}
		teammate.Parent = parent
		return teammate, nil
	}
	return subagent.Teammate{}, fmt.Errorf("team: teammate %q not found", target)
}

func (d appTeammateDirectory) teammate(ctx context.Context, roster *team.Roster, member team.MemberView) (subagent.Teammate, bool) {
	handle, err := roster.Handle(agent.ID(member.ID))
	if err != nil || handle == nil {
		return subagent.Teammate{}, false
	}
	if err := ctx.Err(); err != nil {
		return subagent.Teammate{}, false
	}
	return subagent.Teammate{
		ID: member.ID, Label: member.Name, Running: handle.Status() == agent.StatusRunning, Continuable: true,
		Send: func(sendCtx context.Context, message string, metadata map[string]string) error {
			if err := sendCtx.Err(); err != nil {
				return err
			}
			return handle.Send(message, metadata)
		},
		SendContent: func(sendCtx context.Context, content []llm.ContentBlock, metadata map[string]string) error {
			if err := sendCtx.Err(); err != nil {
				return err
			}
			return handle.FollowupContent(content, metadata)
		},
		SendQuiet: func(sendCtx context.Context, message string) error {
			if err := sendCtx.Err(); err != nil {
				return err
			}
			return handle.Inject(message, nil)
		},
		Followup: func(sendCtx context.Context, message string) error {
			if err := sendCtx.Err(); err != nil {
				return err
			}
			return handle.Followup(message, nil)
		},
		Cancel: func(_ string) error { return handle.Cancel() },
	}, true
}

func (d appTeammateDirectory) Parent(ctx context.Context, id string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	ids := d.app.durableTeamRootIDs(ctx)
	for _, teamID := range ids {
		board, err := d.app.teamBoard(teamID)
		if err != nil {
			continue
		}
		roster := board.Roster()
		if roster == nil {
			continue
		}
		if id == string(roster.LeadID()) {
			continue
		}
		if _, err := roster.Member(agent.ID(id)); err == nil {
			return string(roster.LeadID()), true, nil
		}
	}
	return "", false, nil
}

// durableTeamRootIDs returns the union of materialized boards and session
// roots whose durable transcript contains Team facts. Team boards are lazy, so
// a process restarted before any Web/native request has not populated
// teamBoards yet; scanning the metadata/event index makes the subagent control
// plane cold-restart addressable without manufacturing boards for ordinary
// sessions.
func (a *app) durableTeamRootIDs(ctx context.Context) []string {
	seen := make(map[string]struct{})
	a.teamMu.Lock()
	for id := range a.teamBoards {
		seen[id] = struct{}{}
	}
	a.teamMu.Unlock()
	if a.store == nil {
		return sortedTeamRootIDs(seen)
	}
	metas, err := a.store.ListSessions(ctx)
	if err != nil {
		return sortedTeamRootIDs(seen)
	}
	for _, meta := range metas {
		root := a.teamRootSessionID(meta.ID)
		if root != meta.ID {
			seen[root] = struct{}{}
			continue
		}
		events, loadErr := a.store.LoadSession(ctx, meta.ID)
		if loadErr != nil {
			continue
		}
		for _, event := range events {
			switch event.Type {
			case session.EventTeamSnapshot, session.EventTeamMember, session.EventTeamTask,
				session.EventTeamMessageQueued, session.EventTeamMessageDelivered:
				seen[root] = struct{}{}
				break
			}
			if _, ok := seen[root]; ok {
				break
			}
		}
	}
	return sortedTeamRootIDs(seen)
}

func sortedTeamRootIDs(ids map[string]struct{}) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// provisionTeammate is the composition-root bridge from the model-facing
// spawn_teammate tool to the durable Team roster and Agent Registry. The
// initial turn is submitted asynchronously so provisioning remains a fast
// tool call while the child owns its own session log and lifecycle.
func (a *app) provisionTeammate(ctx context.Context, parent, name, description, prompt, contextKind string) (subagent.TeammateProvision, error) {
	board, err := a.teamBoard(parent)
	if err != nil {
		return subagent.TeammateProvision{}, err
	}
	roster := board.Roster()
	if roster == nil || a.agentRegistry == nil {
		return subagent.TeammateProvision{}, fmt.Errorf("team: Agent Registry roster is unavailable")
	}
	if a.store == nil {
		return subagent.TeammateProvision{}, fmt.Errorf("team: session store is unavailable")
	}
	if contextKind == "" {
		contextKind = "fresh"
	}
	before := board.Snapshot()
	_, atomicReservation := a.store.(store.TeamMemberSessionReservationStore)
	var childID string
	if atomicReservation {
		childID, err = board.MemberID(name)
	} else {
		childID, err = board.ReserveMemberID(name)
	}
	if err != nil {
		return subagent.TeammateProvision{}, fmt.Errorf("team: reserve member identity: %w", err)
	}
	rootLog, err := a.sessionLogForAgent(ctx, parent)
	if err != nil {
		return subagent.TeammateProvision{}, fmt.Errorf("team: load root journal: %w", err)
	}
	provisioning := team.MemberEvent{Version: 1, TeamID: board.TeamID(), Member: team.MemberSnapshot{
		ID: childID, Name: name, Description: description, Provider: "agent-registry", Context: contextKind, Phase: team.MemberProvisioning,
	}}
	if err := a.createTeammateSessionAndMember(ctx, parent, childID, contextKind, rootLog, provisioning, atomicReservation); err != nil {
		return subagent.TeammateProvision{}, fmt.Errorf("team: persist member provisioning: %w", err)
	}
	if atomicReservation {
		if err := roster.AdoptReservedMemberID(agent.ID(childID)); err != nil {
			return subagent.TeammateProvision{}, fmt.Errorf("team: adopt committed member identity: %w", err)
		}
	}
	runContext := a.baseCtx
	if runContext == nil {
		runContext = context.Background()
	}
	view, err := roster.Spawn(runContext, agent.ID(parent), name, description, "agent-registry", contextKind, func(runCtx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
		return a.runAgentTurn(runCtx, childID, input, runtimeAgent)
	})
	if err != nil {
		failed := provisioning
		failed.Member.Phase = team.MemberFailed
		failed.Member.Error = err.Error()
		if appendErr := a.appendTeamMemberState(ctx, parent, childID, rootLog, failed); appendErr != nil {
			err = errors.Join(err, fmt.Errorf("persist failed member state: %w", appendErr))
		}
		_ = a.store.DeleteSession(context.Background(), childID)
		return subagent.TeammateProvision{}, err
	}
	handle, err := roster.Handle(agent.ID(view.ID))
	if err != nil {
		failed := provisioning
		failed.Member.Phase = team.MemberFailed
		failed.Member.Error = err.Error()
		if appendErr := a.appendTeamMemberState(ctx, parent, childID, rootLog, failed); appendErr != nil {
			err = errors.Join(err, fmt.Errorf("persist failed member state: %w", appendErr))
		}
		_ = a.store.DeleteSession(context.Background(), childID)
		return subagent.TeammateProvision{}, err
	}
	if a.jobs != nil {
		if cleanupErr := handle.Scope().AddCleanup(func() error {
			return a.jobs.CloseOwner(childID)
		}); cleanupErr != nil {
			_ = a.agentRegistry.Close(agent.ID(view.ID))
			_ = a.store.DeleteSession(context.Background(), childID)
			return subagent.TeammateProvision{}, fmt.Errorf("team: register job owner cleanup: %w", cleanupErr)
		}
	}
	if cleanupErr := handle.Scope().AddCleanup(func() error {
		return a.closeModelTerminalOwner(childID)
	}); cleanupErr != nil {
		_ = a.agentRegistry.Close(agent.ID(view.ID))
		_ = a.store.DeleteSession(context.Background(), childID)
		return subagent.TeammateProvision{}, fmt.Errorf("team: register terminal owner cleanup: %w", cleanupErr)
	}
	if cleanupErr := handle.Scope().AddCleanup(func() error {
		a.clearSessionApprovalPolicy(childID)
		return nil
	}); cleanupErr != nil {
		_ = a.agentRegistry.Close(agent.ID(view.ID))
		_ = a.store.DeleteSession(context.Background(), childID)
		return subagent.TeammateProvision{}, fmt.Errorf("team: register approval policy cleanup: %w", cleanupErr)
	}
	active := provisioning
	active.Member.Phase = team.MemberActive
	if err := a.appendTeamMemberState(ctx, parent, childID, rootLog, active); err != nil {
		_ = board.Restore(before)
		_ = a.agentRegistry.Close(agent.ID(view.ID))
		_ = a.store.DeleteSession(context.Background(), childID)
		return subagent.TeammateProvision{}, fmt.Errorf("team: persist active member state: %w", err)
	}
	done := make(chan subagent.Result, 1)
	go func() {
		runErr := handle.Run(runContext, prompt, map[string]string{"team": "teammate", "context": contextKind})
		result := subagent.Result{StopReason: subagent.StopCompleted}
		if runErr != nil {
			result.StopReason = subagent.StopError
			if errors.Is(runErr, context.Canceled) {
				result.StopReason = subagent.StopAborted
			}
		}
		if log, logErr := a.sessionLogForAgent(context.Background(), childID); logErr == nil {
			history := log.DeriveHistory()
			for index := len(history) - 1; index >= 0; index-- {
				if history[index].Role == llm.RoleAssistant && strings.TrimSpace(history[index].Text()) != "" {
					result.Output = history[index].Text()
					break
				}
			}
		}
		done <- result
	}()
	run := &subagent.Run{
		ID: childID,
		Result: func(waitCtx context.Context) (subagent.Result, error) {
			select {
			case result := <-done:
				return result, nil
			case <-waitCtx.Done():
				return subagent.Result{}, waitCtx.Err()
			}
		},
		Send: func(sendCtx context.Context, message string) error {
			return handle.Followup(message, nil)
		},
		SendContentWithMetadata: func(sendCtx context.Context, content []llm.ContentBlock, metadata map[string]string) error {
			return handle.FollowupContent(content, metadata)
		},
		SendQuiet: func(sendCtx context.Context, message string) error {
			return handle.Inject(message, nil)
		},
		Cancel: func(string) error { return handle.Cancel() },
	}
	return subagent.TeammateProvision{ID: childID, Name: name, Description: description, Provider: "agent-registry", Context: contextKind, Status: string(agent.StatusRunning), Run: run}, nil
}

// createTeammateSessionAndMember closes the durable provisioning window when
// the store supports the Team transaction: the child session (and fork seed)
// plus the lead's provisioning edge become visible together. Older stores use
// the compatibility sequence and retain its compensating cleanup behavior.
func (a *app) createTeammateSessionAndMember(ctx context.Context, parent, childID, contextKind string, rootLog *session.Log, member team.MemberEvent, atomicReservation bool) error {
	if atomicReservation {
		atomicStore, ok := a.store.(store.TeamMemberSessionReservationStore)
		if !ok {
			return errors.New("team: atomic reservation store disappeared")
		}
		created, header, seed, err := a.teammateSessionPlan(ctx, parent, childID, contextKind)
		if err != nil {
			return err
		}
		data, err := json.Marshal(member)
		if err != nil {
			return fmt.Errorf("encode member provisioning: %w", err)
		}
		event := session.Event{Seq: rootLog.NextSeq(), Type: session.EventTeamMember, At: time.Now().UTC(), Version: session.EventVersion, Data: data}
		if err := atomicStore.CreateTeamMemberSessionWithReservation(ctx, childID, created, header, seed, parent, event); err != nil {
			return err
		}
		if err := rootLog.AppendPersisted(event); err != nil {
			return fmt.Errorf("adopt committed member provisioning: %w", err)
		}
		return nil
	}
	if atomicStore, ok := a.store.(store.TeamMemberSessionStore); ok {
		created, header, seed, err := a.teammateSessionPlan(ctx, parent, childID, contextKind)
		if err != nil {
			return err
		}
		data, err := json.Marshal(member)
		if err != nil {
			return fmt.Errorf("encode member provisioning: %w", err)
		}
		event := session.Event{Seq: rootLog.NextSeq(), Type: session.EventTeamMember, At: time.Now().UTC(), Version: session.EventVersion, Data: data}
		if err := atomicStore.CreateTeamMemberSession(ctx, childID, created, header, seed, parent, event); err != nil {
			return err
		}
		if err := rootLog.AppendPersisted(event); err != nil {
			return fmt.Errorf("adopt committed member provisioning: %w", err)
		}
		return nil
	}
	if err := a.createTeammateSession(ctx, parent, childID, contextKind); err != nil {
		return err
	}
	if hs, ok := a.store.(interface {
		SetSessionCWD(context.Context, string, string) error
	}); ok {
		if err := hs.SetSessionCWD(ctx, childID, a.sessionCWDFor(parent)); err != nil {
			_ = a.store.DeleteSession(context.Background(), childID)
			return err
		}
	}
	if _, err := rootLog.Append(session.EventTeamMember, member); err != nil {
		_ = a.store.DeleteSession(context.Background(), childID)
		return err
	}
	return nil
}

// teammateSessionPlan computes the exact child header and closed parent seed
// without publishing either. Keeping this pure until the SQLite transaction
// starts is what makes the provisioning edge and child session atomic.
func (a *app) teammateSessionPlan(ctx context.Context, parent, childID, contextKind string) (time.Time, store.SessionHeader, []session.Event, error) {
	created := time.Now().UTC()
	parentHeader := store.SessionHeader{}
	if headers, ok := a.store.(store.SessionLineageStore); ok {
		header, err := headers.GetSessionHeader(ctx, parent)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return time.Time{}, store.SessionHeader{}, nil, err
		}
		parentHeader = header
	}
	header := store.SessionHeader{ID: childID, CreatedAt: created, CWD: a.sessionCWDFor(parent), Parent: parent,
		Origin: "team", DelegationDepth: parentHeader.DelegationDepth + 1, AgentPreset: parentHeader.AgentPreset}
	if contextKind != "fork" {
		return created, header, nil, nil
	}
	var events []session.Event
	var err error
	if raw, ok := a.store.(store.SessionRawStore); ok {
		events, err = raw.LoadSessionRaw(ctx, parent)
	} else {
		events, err = a.store.LoadSession(ctx, parent)
	}
	if err != nil {
		return time.Time{}, store.SessionHeader{}, nil, fmt.Errorf("team: load fork parent %q: %w", parent, err)
	}
	seed := closedSessionPrefix(events)
	header.Origin = "fork"
	header.SeedLength = len(seed)
	return created, header, seed, nil
}

// createTeammateSession gives a Team fork the same durable seed semantics as
// the normal session fork path. A live parent can have an open turn when the
// spawn_teammate tool executes; only the last fully closed turn is copied, so
// the child never replays half of a request or a tool barrier. Fresh members
// remain empty sessions. The fallback works with older Store test doubles;
// SQLiteStore additionally exposes the atomic fork primitive.
func (a *app) createTeammateSession(ctx context.Context, parent, childID, contextKind string) error {
	if contextKind != "fork" {
		created := time.Now().UTC()
		parentHeader := store.SessionHeader{}
		if headers, ok := a.store.(store.SessionLineageStore); ok {
			var err error
			parentHeader, err = headers.GetSessionHeader(ctx, parent)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		childHeader := store.SessionHeader{
			ID: childID, CreatedAt: created, CWD: parentHeader.CWD,
			Parent: parent, Origin: "team", DelegationDepth: parentHeader.DelegationDepth + 1,
			AgentPreset: parentHeader.AgentPreset,
		}
		if atomic, ok := a.store.(store.SessionCreateStore); ok {
			if err := atomic.CreateSessionWithOptions(ctx, childID, created, store.SessionCreateOptions{Header: childHeader}, nil); err != nil {
				return err
			}
		} else {
			if err := a.store.CreateSession(ctx, childID, created); err != nil {
				return err
			}
			if headers, ok := a.store.(store.SessionLineageStore); ok {
				if err := headers.SetSessionHeader(ctx, childID, childHeader); err != nil {
					_ = a.store.DeleteSession(context.Background(), childID)
					return err
				}
			}
		}
		return nil
	}
	// Team fork selection must inspect the physical durable prefix. LoadSession
	// may append synthetic interrupted closers, which would turn an unfinished
	// parent turn into false child history before closedSessionPrefix runs.
	var events []session.Event
	var err error
	if raw, ok := a.store.(store.SessionRawStore); ok {
		events, err = raw.LoadSessionRaw(ctx, parent)
	} else {
		events, err = a.store.LoadSession(ctx, parent)
	}
	if err != nil {
		return fmt.Errorf("team: load fork parent %q: %w", parent, err)
	}
	seed := closedSessionPrefix(events)
	if forker, ok := a.store.(store.SessionForkStore); ok && len(seed) > 0 {
		return forker.ForkSessionWithOptions(ctx, parent, childID, seed[len(seed)-1].Seq, store.SessionForkOptions{
			InheritParentMetadata: true,
		})
	}
	if forker, ok := a.store.(interface {
		ForkSession(context.Context, string, string, uint64) error
	}); ok && len(seed) == len(events) && len(seed) > 0 {
		return forker.ForkSession(ctx, parent, childID, seed[len(seed)-1].Seq)
	}

	created := time.Now().UTC()
	if err := a.store.CreateSession(ctx, childID, created); err != nil {
		return err
	}
	cleanup := func(cause error) error {
		_ = a.store.DeleteSession(context.Background(), childID)
		return cause
	}
	if headers, ok := a.store.(store.SessionLineageStore); ok {
		parentHeader, headerErr := headers.GetSessionHeader(ctx, parent)
		if headerErr != nil && !errors.Is(headerErr, store.ErrNotFound) {
			return cleanup(headerErr)
		}
		childHeader := store.SessionHeader{
			ID: childID, CreatedAt: created, CWD: parentHeader.CWD,
			Parent: parent, SeedLength: len(seed), Origin: "fork",
			DelegationDepth: parentHeader.DelegationDepth + 1,
			AgentPreset:     parentHeader.AgentPreset,
		}
		if err := headers.SetSessionHeader(ctx, childID, childHeader); err != nil {
			return cleanup(err)
		}
	}
	if len(seed) > 0 {
		if err := a.store.AppendEvents(ctx, childID, seed); err != nil {
			return cleanup(err)
		}
	}
	return nil
}

// closedSessionPrefix returns the maximal prefix ending at a complete turn.
// Legacy logs without lifecycle anchors are already replayable as a whole.
func closedSessionPrefix(events []session.Event) []session.Event {
	turn, step := 0, 0
	lastClosed := -1
	hasLifecycle := false
	for index, event := range events {
		switch event.Type {
		case session.EventTurnStart:
			hasLifecycle = true
			if turn == 0 {
				turn = 1
			}
		case session.EventStepStart:
			hasLifecycle = true
			if turn == 1 && step == 0 {
				step = 1
			}
		case session.EventStepEnd:
			hasLifecycle = true
			if turn == 1 && step == 1 {
				step = 0
			}
		case session.EventTurnEnd:
			hasLifecycle = true
			if turn == 1 && step == 0 {
				turn = 0
				lastClosed = index
			}
		}
	}
	if !hasLifecycle {
		return append([]session.Event(nil), events...)
	}
	if lastClosed < 0 {
		return nil
	}
	return append([]session.Event(nil), events[:lastClosed+1]...)
}

func (a *app) teamBoard(sessionID string) (*team.Board, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("team: session id is required")
	}
	sessionID = a.teamRootSessionID(sessionID)
	a.teamMu.Lock()
	defer a.teamMu.Unlock()
	if a.teamBoards == nil {
		a.teamBoards = make(map[string]*team.Board)
	}
	if board := a.teamBoards[sessionID]; board != nil {
		return board, nil
	}
	board, err := team.New("team:"+sessionID, nil)
	if err != nil {
		return nil, err
	}
	if reservations, ok := a.store.(store.IDReservationStore); ok {
		board.SetIDReservation(func(namespace, id string) (bool, error) {
			return reservations.ReserveID(context.Background(), namespace, id)
		})
	}
	if a.agentRegistry != nil {
		if _, agentErr := a.sessionAgent(sessionID); agentErr != nil {
			return nil, agentErr
		}
		roster, rosterErr := team.NewRoster("team:"+sessionID, agent.ID(sessionID), a.agentRegistry)
		if rosterErr != nil {
			return nil, rosterErr
		}
		board.AttachRoster(roster)
	}
	if a.store != nil {
		if events, loadErr := a.store.LoadSession(context.Background(), sessionID); loadErr == nil {
			snapshotIndex := -1
			for index := len(events) - 1; index >= 0; index-- {
				if events[index].Type != session.EventTeamSnapshot {
					continue
				}
				var snapshot team.Snapshot
				if decodeErr := json.Unmarshal(events[index].Data, &snapshot); decodeErr != nil {
					// The newest Team snapshot is a required reconstruction record.
					// Silently falling through would expose an empty board and make a
					// corrupt durable log look like valid fresh state.
					return nil, fmt.Errorf("team: decode durable board snapshot for %q: %w", sessionID, decodeErr)
				}
				if restoreErr := board.Restore(snapshot); restoreErr != nil {
					return nil, restoreErr
				}
				snapshotIndex = index
				// A snapshot carries durable identities, not process-local
				// handles. Rebind every active teammate to this process's
				// Agent Registry before exposing the board. Failed rebinds are
				// retained by the roster as failed identities.
				if roster := board.Roster(); roster != nil {
					rebindFailed := false
					runContext := a.baseCtx
					if runContext == nil {
						runContext = context.Background()
					}
					for _, member := range roster.Snapshot() {
						if member.ID == string(roster.LeadID()) || member.Phase != team.MemberActive {
							continue
						}
						memberID := member.ID
						if childErr := a.validateTeamChildSession(context.Background(), sessionID, memberID); childErr != nil {
							_ = roster.MarkFailed(agent.ID(memberID), childErr)
							rebindFailed = true
							continue
						}
						if _, rebindErr := roster.Rebind(runContext, agent.ID(memberID), func(runCtx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
							return a.runAgentTurn(runCtx, memberID, input, runtimeAgent)
						}); rebindErr != nil {
							rebindFailed = true
						}
					}
					if rebindFailed {
						// Rebind transitions failed identities in memory. Persist that
						// transition before exposing the board; otherwise a restart would
						// repeatedly claim the same teammate is active.
						log, logErr := a.sessionLogForAgent(context.Background(), sessionID)
						if logErr != nil {
							return nil, fmt.Errorf("team: persist failed rebind state: %w", logErr)
						}
						if _, appendErr := log.Append(session.EventTeamSnapshot, board.Snapshot()); appendErr != nil {
							return nil, fmt.Errorf("team: persist failed rebind state: %w", appendErr)
						}
					}
				}
				break
			}
			for _, event := range events[snapshotIndex+1:] {
				switch event.Type {
				case session.EventTeamMember, session.EventTeamTask, session.EventTeamMessageQueued, session.EventTeamMessageDelivered:
					if err := board.ApplyEvent(event.Type, event.Data); err != nil {
						return nil, fmt.Errorf("team: replay %s for %q: %w", event.Type, sessionID, err)
					}
				}
			}
			if err := a.rebindTeamMembers(context.Background(), sessionID, board); err != nil {
				return nil, err
			}
		} else if loadErr != nil && !errors.Is(loadErr, store.ErrNotFound) {
			return nil, fmt.Errorf("team: load durable board snapshot for %q: %w", sessionID, loadErr)
		}
	}
	a.teamBoards[sessionID] = board
	return board, nil
}

// rebindTeamMembers attaches process-local Agent handles to active durable
// member identities that were reconstructed from append-only events. It is
// also safe after the legacy snapshot rebind path: already-bound handles are
// skipped, while failed rows remain terminal.
func (a *app) rebindTeamMembers(ctx context.Context, rootID string, board *team.Board) error {
	if a == nil || a.agentRegistry == nil || board == nil {
		return nil
	}
	roster := board.Roster()
	if roster == nil {
		return nil
	}
	runContext := a.baseCtx
	if runContext == nil {
		runContext = context.Background()
	}
	for _, member := range roster.Snapshot() {
		if member.ID == string(roster.LeadID()) {
			continue
		}
		if member.Phase == team.MemberProvisioning {
			if err := a.validateTeamChildSession(ctx, rootID, member.ID); err != nil {
				_ = roster.MarkFailed(agent.ID(member.ID), fmt.Errorf("child Session recovery failed: %w", err))
				if persistErr := a.persistTeamMemberState(ctx, rootID, board, member.ID); persistErr != nil {
					return persistErr
				}
				continue
			}
			memberID := member.ID
			if _, err := roster.RebindProvisioning(runContext, agent.ID(memberID), func(runCtx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
				return a.runAgentTurn(runCtx, memberID, input, runtimeAgent)
			}); err != nil {
				// RebindProvisioning records a terminal failed row on start/create
				// failure; persist that edge before exposing the board.
				if persistErr := a.persistTeamMemberState(ctx, rootID, board, memberID); persistErr != nil {
					return persistErr
				}
				continue
			}
			if err := a.persistTeamMemberState(ctx, rootID, board, memberID); err != nil {
				_ = roster.MarkFailed(agent.ID(memberID), err)
				return err
			}
			continue
		}
		if member.Phase != team.MemberActive {
			continue
		}
		if _, err := roster.Handle(agent.ID(member.ID)); err == nil {
			continue
		}
		if err := a.validateTeamChildSession(ctx, rootID, member.ID); err != nil {
			_ = roster.MarkFailed(agent.ID(member.ID), err)
			if persistErr := a.persistTeamMemberState(ctx, rootID, board, member.ID); persistErr != nil {
				return persistErr
			}
			continue
		}
		memberID := member.ID
		if _, err := roster.Rebind(runContext, agent.ID(memberID), func(runCtx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
			return a.runAgentTurn(runCtx, memberID, input, runtimeAgent)
		}); err != nil {
			_ = roster.MarkFailed(agent.ID(memberID), err)
			if persistErr := a.persistTeamMemberState(ctx, rootID, board, memberID); persistErr != nil {
				return persistErr
			}
		}
	}
	return nil
}

func (a *app) persistTeamMemberState(ctx context.Context, rootID string, board *team.Board, memberID string) error {
	member, err := board.Roster().Member(agent.ID(memberID))
	if err != nil {
		return err
	}
	log, err := a.sessionLogForAgent(ctx, rootID)
	if err != nil {
		return fmt.Errorf("team: load journal for failed member %q: %w", memberID, err)
	}
	if err := a.appendTeamMemberState(ctx, rootID, memberID, log, team.MemberEvent{Version: 1, TeamID: board.TeamID(), Member: member.MemberSnapshot}); err != nil {
		return fmt.Errorf("team: persist failed member %q: %w", memberID, err)
	}
	return nil
}

// appendTeamMemberState uses the storage CAS seam when available. The live
// roster has already moved through its Agent publication barrier; adopting the
// exact committed event with AppendPersisted prevents a second sink append and
// keeps the root Log's cursor aligned with the cross-process transaction.
func (a *app) appendTeamMemberState(ctx context.Context, rootID, childID string, log *session.Log, member team.MemberEvent) error {
	if log == nil {
		return errors.New("team: root log is required")
	}
	if transition, ok := a.store.(store.TeamMemberTransitionStore); ok {
		data, err := json.Marshal(member)
		if err != nil {
			return err
		}
		event := session.Event{Seq: log.NextSeq(), Type: session.EventTeamMember, At: time.Now().UTC(), Version: session.EventVersion, Data: data}
		if err := transition.TransitionTeamMember(ctx, rootID, childID, event); err != nil {
			return err
		}
		return log.AppendPersisted(event)
	}
	_, err := log.Append(session.EventTeamMember, member)
	return err
}

type teamDispatchResult struct {
	done      chan struct{}
	delivered bool
	err       error
	waiters   int
}

// dispatchTeamMessage admits one delivery through a target-local FIFO. The
// queue event is appended before this seam is called, so the target id's tail
// is only a process-local ordering fence; restart recovery reconstructs the
// same order from the durable Lead journal. In-flight identity coalescing is
// required because a recovery pass and a live send can observe one pending
// message at the same time.
func (a *app) dispatchTeamMessage(ctx context.Context, rootID string, message team.Message) (bool, error) {
	if a == nil {
		return false, errors.New("team: nil app")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	targetID := strings.TrimSpace(message.TargetID)
	if targetID == "" || strings.TrimSpace(message.ID) == "" {
		return false, errors.New("team: message target and id are required")
	}
	// Board-local ids (msg-1, msg-2, ...) are not process-global identities.
	// Include the durable Team root so two Teams can dispatch equal local ids
	// without coalescing one another.
	dispatchID := strings.TrimSpace(rootID) + "\x00" + message.ID
	return a.runOrderedTeamDispatch(ctx, targetID, dispatchID, func() (bool, error) {
		return a.dispatchTeamMessageNow(ctx, rootID, message)
	})
}

func (a *app) runOrderedTeamDispatch(ctx context.Context, targetID, messageID string, dispatch func() (bool, error)) (bool, error) {
	if dispatch == nil {
		return false, errors.New("team: dispatch function is required")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	a.teamDispatchMu.Lock()
	if a.teamDispatchInFlight == nil {
		a.teamDispatchInFlight = make(map[string]*teamDispatchResult)
	}
	if existing := a.teamDispatchInFlight[messageID]; existing != nil {
		existing.waiters++
		a.teamDispatchMu.Unlock()
		select {
		case <-existing.done:
			return existing.delivered, existing.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if a.teamDispatchTails == nil {
		a.teamDispatchTails = make(map[string]chan struct{})
	}
	prior := a.teamDispatchTails[targetID]
	tail := make(chan struct{})
	a.teamDispatchTails[targetID] = tail
	result := &teamDispatchResult{done: make(chan struct{})}
	a.teamDispatchInFlight[messageID] = result
	a.teamDispatchMu.Unlock()

	if prior != nil {
		select {
		case <-prior:
		case <-ctx.Done():
			a.finishTeamDispatch(targetID, messageID, tail, result, false, ctx.Err())
			return false, ctx.Err()
		}
	}
	delivered, err := dispatch()
	a.finishTeamDispatch(targetID, messageID, tail, result, delivered, err)
	return delivered, err
}

func (a *app) finishTeamDispatch(targetID, messageID string, tail chan struct{}, result *teamDispatchResult, delivered bool, err error) {
	a.teamDispatchMu.Lock()
	result.delivered = delivered
	result.err = err
	if a.teamDispatchInFlight[messageID] == result {
		delete(a.teamDispatchInFlight, messageID)
	}
	if a.teamDispatchTails[targetID] == tail {
		delete(a.teamDispatchTails, targetID)
	}
	close(result.done)
	close(tail)
	a.teamDispatchMu.Unlock()
}

// dispatchTeamMessageNow attempts one runtime delivery after the queue
// snapshot has committed. Offline members remain queued and are eligible for a
// later cold-recovery/rebind pass.
func (a *app) dispatchTeamMessageNow(ctx context.Context, rootID string, message team.Message) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var handle *agent.Handle
	if message.TargetID == rootID {
		var err error
		handle, err = a.sessionAgent(rootID)
		if err != nil {
			return false, err
		}
	} else {
		board, err := a.teamBoard(rootID)
		if err != nil {
			return false, err
		}
		roster := board.Roster()
		if roster == nil {
			return false, nil
		}
		handle, err = roster.Handle(agent.ID(message.TargetID))
		if err != nil {
			return false, nil
		}
	}
	header := fmt.Sprintf("Team message %s from %s:", message.ID, message.SenderName)
	blocks := cloneTeamContentBlocks(message.ContentBlocks)
	if len(blocks) == 0 {
		blocks = []llm.ContentBlock{llm.Text(message.Content)}
	}
	deliveryBlocks := make([]llm.ContentBlock, 0, len(blocks)+1)
	deliveryBlocks = append(deliveryBlocks, llm.Text(header))
	deliveryBlocks = append(deliveryBlocks, blocks...)
	// The target inbox is the durable delivery receipt. It must be committed
	// before the Lead delivery fact: if the process stops after this call but
	// before the Lead event, recovery sees the still-pending mailbox item and
	// either reuses the inbox entry or completes the missing acknowledgement.
	// Committing a target user/message and Lead delivered event first is unsafe:
	// a crash before the live inbox enqueue would make the message look delivered
	// while leaving no work to recover.
	targetLog, err := a.sessionLogForAgent(ctx, message.TargetID)
	if err != nil {
		return false, fmt.Errorf("team: load target receipt log: %w", err)
	}
	teamID := "team:" + rootID
	if !teamMessageReceiptRecorded(targetLog, teamID, message.ID) {
		metadata := map[string]string{
			"team_id":          teamID,
			"team_message_id":  message.ID,
			"team_sender_id":   message.SenderID,
			"team_sender_name": message.SenderName,
		}
		var deliveryErr error
		if message.Delivery == "quiet" {
			deliveryErr = handle.InjectContent(deliveryBlocks, metadata)
		} else {
			deliveryErr = handle.FollowupContent(deliveryBlocks, metadata)
		}
		if deliveryErr != nil {
			return false, deliveryErr
		}
	}
	if err := a.recordTeamDelivery(ctx, rootID, message); err != nil {
		// Keep the Board item pending. The target inbox is already durable, so a
		// retry can deduplicate the enqueue and finish only the missing edge.
		return false, err
	}
	return true, nil
}

// recordTeamDelivery commits only the Lead-owned acknowledgement edge. The
// target inbox (or an already persisted target user/message receipt) must have
// landed before this method is called. Keeping this as a separate seam makes
// the crash boundary explicit and lets recovery finish a half-completed send.
func (a *app) recordTeamDelivery(ctx context.Context, rootID string, message team.Message) error {
	log, err := a.sessionLogForAgent(ctx, rootID)
	if err != nil {
		return fmt.Errorf("team: load root delivery log: %w", err)
	}
	teamID := "team:" + rootID
	if teamDeliveryRecorded(log, teamID, message.ID, message.TargetID) {
		return nil
	}
	_, err = log.Append(session.EventTeamMessageDelivered, team.MessageDeliveredEvent{
		Version: 1, TeamID: teamID, MessageID: message.ID, TargetID: message.TargetID,
	})
	return err
}

// recordTeamMessage commits the target-side receipt before mailbox delivery
// is acknowledged. Inbox de-duplication makes the following enqueue retryable
// if a process stops between the receipt and the in-memory enqueue.
func (a *app) recordTeamMessage(ctx context.Context, rootID string, message team.Message, framed string) error {
	return a.recordTeamMessageWithBlocks(ctx, rootID, message, framed, []llm.ContentBlock{llm.Text(framed)})
}

func (a *app) recordTeamMessageWithBlocks(ctx context.Context, rootID string, message team.Message, framed string, blocks []llm.ContentBlock) error {
	log, err := a.sessionLogForAgent(ctx, message.TargetID)
	if err != nil {
		return fmt.Errorf("team: load target receipt log: %w", err)
	}
	for _, event := range log.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Source *struct {
				Kind      string `json:"kind"`
				TeamID    string `json:"teamId"`
				MessageID string `json:"messageId"`
			} `json:"source"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Source != nil &&
			data.Source.Kind == "team-message" && data.Source.TeamID == "team:"+rootID && data.Source.MessageID == message.ID {
			return nil
		}
	}
	_, err = log.Append(session.EventUserMessage, session.NewTeamMessage(framed,
		blocks, "team:"+rootID, message.ID, message.SenderID, message.SenderName))
	return err
}

// recordTeamReceiptAndDelivery uses the strongest available persistence seam:
// the target receipt and Lead mailbox delivery fact receive their sequence
// numbers under the two live log locks and commit in one SQLite transaction.
// Callers intentionally commit this before the live inbox enqueue. If the
// process dies between the commit and enqueue, the Board message remains
// pending and recovery can reconstruct the live delivery from the durable
// receipt instead of losing it.
func (a *app) recordTeamReceiptAndDelivery(ctx context.Context, rootID string, message team.Message, framed string, blocks []llm.ContentBlock) error {
	multi, ok := a.store.(store.MultiSessionEventStore)
	if !ok {
		return errors.New("team: multi-session atomic persistence is unavailable")
	}
	targetLog, err := a.sessionLogForAgent(ctx, message.TargetID)
	if err != nil {
		return fmt.Errorf("team: load target receipt log: %w", err)
	}
	rootLog, err := a.sessionLogForAgent(ctx, rootID)
	if err != nil {
		return fmt.Errorf("team: load root delivery log: %w", err)
	}
	teamID := "team:" + rootID
	receiptNeeded := !teamMessageReceiptRecorded(targetLog, teamID, message.ID)
	deliveryNeeded := !teamDeliveryRecorded(rootLog, teamID, message.ID, message.TargetID)
	if !receiptNeeded && !deliveryNeeded {
		return nil
	}
	appends := make([]session.AtomicAppend, 0, 2)
	if receiptNeeded {
		appends = append(appends, session.AtomicAppend{
			Log: targetLog, Type: session.EventUserMessage,
			Data: session.NewTeamMessage(framed, blocks, teamID, message.ID, message.SenderID, message.SenderName),
		})
	}
	if deliveryNeeded {
		appends = append(appends, session.AtomicAppend{
			Log: rootLog, Type: session.EventTeamMessageDelivered,
			Data: team.MessageDeliveredEvent{Version: 1, TeamID: teamID, MessageID: message.ID, TargetID: message.TargetID},
		})
	}
	return session.AppendAtomic(appends, func(events []session.Event) error {
		batches := make(map[string][]session.Event, 2)
		for index, event := range events {
			if appends[index].Log == targetLog {
				batches[message.TargetID] = append(batches[message.TargetID], event)
			} else {
				batches[rootID] = append(batches[rootID], event)
			}
		}
		return multi.AppendEventsAtomic(ctx, batches)
	})
}

func teamMessageReceiptRecorded(log *session.Log, teamID, messageID string) bool {
	if log == nil {
		return false
	}
	// The Agent runtime path records the target receipt as an inbox splice,
	// not as a model-visible user/message event. Inspect every durable inserted
	// message, including messages that have already been claimed from the live
	// queue, so a retry after a missing root acknowledgement cannot enqueue a
	// second Team message.
	if inboxEvents, err := replaySessionInbox(log.Events()); err == nil {
		for _, inboxEvent := range inboxEvents {
			for _, message := range inboxEvent.Inserted {
				if message.Metadata["team_id"] == teamID && message.Metadata["team_message_id"] == messageID {
					return true
				}
			}
		}
	}
	for _, event := range log.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Source *struct {
				Kind      string `json:"kind"`
				TeamID    string `json:"teamId"`
				MessageID string `json:"messageId"`
			} `json:"source"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Source != nil && data.Source.Kind == "team-message" && data.Source.TeamID == teamID && data.Source.MessageID == messageID {
			return true
		}
	}
	return false
}

func teamDeliveryRecorded(log *session.Log, teamID, messageID, targetID string) bool {
	if log == nil {
		return false
	}
	for _, event := range log.Events() {
		if event.Type != session.EventTeamMessageDelivered {
			continue
		}
		var delivered team.MessageDeliveredEvent
		if json.Unmarshal(event.Data, &delivered) == nil && delivered.TeamID == teamID && delivered.MessageID == messageID && delivered.TargetID == targetID {
			return true
		}
	}
	return false
}

func cloneTeamContentBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]llm.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		out[i].Blocks = cloneTeamContentBlocks(out[i].Blocks)
	}
	return out
}

func (a *app) redeliverPendingTeamMessages(ctx context.Context, rootID string, board *team.Board) error {
	if roster := board.Roster(); roster != nil {
		for _, member := range roster.List() {
			for _, message := range board.PendingMessages(member.ID) {
				delivered, _ := a.dispatchTeamMessage(ctx, rootID, message)
				if !delivered {
					continue
				}
				log, err := a.sessionLogForAgent(ctx, rootID)
				if err != nil {
					return err
				}
				if !teamDeliveryRecorded(log, board.TeamID(), message.ID, message.TargetID) {
					if _, err := log.Append(session.EventTeamMessageDelivered, team.MessageDeliveredEvent{
						Version: 1, TeamID: board.TeamID(), MessageID: message.ID, TargetID: message.TargetID,
					}); err != nil {
						return err
					}
				}
				// The durable delivered fact is the commit point. Ack only after
				// it lands; a crash or sink failure before this line leaves the
				// message queued and therefore safely retryable.
				if err := board.AckMessage(message.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateTeamChildSession prevents cold restore from manufacturing an active
// teammate Agent when its durable child transcript or parent lineage vanished.
// The reference roster treats that case as failed provisioning, not as a new
// live runtime with an unverifiable identity.
func (a *app) validateTeamChildSession(ctx context.Context, parent, child string) error {
	if a == nil || a.store == nil {
		return nil
	}
	if _, err := a.store.LoadSession(ctx, child); err != nil {
		return fmt.Errorf("team: child session %q is unavailable: %w", child, err)
	}
	if lineage, ok := a.store.(store.SessionLineageStore); ok {
		header, err := lineage.GetSessionHeader(ctx, child)
		if err == nil {
			if strings.TrimSpace(header.Parent) != parent {
				return fmt.Errorf("team: child session %q has parent %q, want %q", child, header.Parent, parent)
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("team: child session %q lineage is unavailable: %w", child, err)
		}
		if !strings.HasPrefix(child, "team:"+parent+":") {
			return fmt.Errorf("team: child session %q has no durable parent lineage", child)
		}
	}
	return nil
}

// teamRootSessionID maps a teammate's durable child session back to the lead
// session that owns the shared Team board. The member remains the original
// runtime session for authorization, but all task/mailbox state and snapshots
// live on one root board. Following durable lineage also makes this survive a
// process restart; the bounded walk rejects malformed cyclic metadata by
// stopping at the first repeated id.
func (a *app) teamRootSessionID(sessionID string) string {
	current := strings.TrimSpace(sessionID)
	if current == "" || a.store == nil {
		return current
	}
	lineage, ok := a.store.(store.SessionLineageStore)
	if !ok {
		if parent, inferred := legacyTeamParent(current); inferred {
			return parent
		}
		return current
	}
	seen := map[string]struct{}{}
	for len(seen) < 64 {
		if _, exists := seen[current]; exists {
			return current
		}
		seen[current] = struct{}{}
		header, err := lineage.GetSessionHeader(context.Background(), current)
		if err != nil {
			if parent, inferred := legacyTeamParent(current); inferred {
				current = parent
				continue
			}
			return current
		}
		if strings.TrimSpace(header.Parent) == "" {
			return current
		}
		current = strings.TrimSpace(header.Parent)
	}
	return current
}

// legacyTeamParent keeps Teams created before durable lineage was added
// addressable after restart. Current sessions always write the header; this
// naming fallback is restricted to the generated team:<parent>:<member> form.
func legacyTeamParent(sessionID string) (string, bool) {
	if !strings.HasPrefix(sessionID, "team:") {
		return "", false
	}
	rest := strings.TrimPrefix(sessionID, "team:")
	cut := strings.IndexByte(rest, ':')
	if cut <= 0 {
		return "", false
	}
	return rest[:cut], true
}
