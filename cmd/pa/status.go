// status.go — dsh ui-workspace session-status alignment: the composition root
// computes each session row's live status (pending interaction > own/descendant
// activity > finished-but-unviewed reminder > idle) and the webserver forwards
// it so the sidebar renders the status dot and the hover card without the
// webserver knowing any runtime state. The runtime signals are read here, not
// in the generic webserver.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/webserver"
)

// sessionStatus returns the live sidebar status for one session (dsh
// sessionStatuses precedence): a pending user interaction outranks running
// activity, which outranks the finished-but-unviewed reminder, then idle.
func (a *app) sessionStatus(ctx context.Context, m store.SessionMeta) webserver.SessionStatus {
	running := a.isSessionRunning(m.ID)
	subagents := a.subagentRunningCount(ctx, m.ID)
	pending := a.pendingInteraction(ctx, m.ID)

	switch {
	case pending != "":
		st := webserver.SessionStatus{
			State:    "warning",
			Statuses: []webserver.StatusEntry{{State: "warning", Label: pendingLabel(pending)}},
		}
		if subagents > 0 {
			st.Statuses = append(st.Statuses, subagentStatus(subagents))
		}
		return st
	case running:
		st := webserver.SessionStatus{
			State:    "ongoing",
			Statuses: []webserver.StatusEntry{{State: "ongoing", Label: "运行中"}},
		}
		if subagents > 0 {
			st.Statuses = append(st.Statuses, subagentStatus(subagents))
		}
		return st
	case subagents > 0:
		return webserver.SessionStatus{State: "ongoing", Statuses: []webserver.StatusEntry{subagentStatus(subagents)}}
	// Finished-but-unviewed (dsh status.completed): content exists and the last
	// activity is newer than the last time the session was opened/messaged
	// (never viewed ≡ viewed at epoch, so any content is unviewed).
	case m.EventCount > 0 && (m.LastViewedAt.IsZero() || m.UpdatedAt.After(m.LastViewedAt)):
		return webserver.SessionStatus{State: "done", Statuses: []webserver.StatusEntry{{State: "done", Label: "已完成"}}}
	default:
		return webserver.SessionStatus{State: "idle", Statuses: []webserver.StatusEntry{{State: "idle", Label: "空闲"}}}
	}
}

// isSessionRunning reports whether a turn is currently in flight for sessionID.
// The running session id is published by runTurn under the serial turn lock and
// read here atomically, so the status provider never touches a.currentID
// (which other handlers mutate) and stays race-free.
func (a *app) isSessionRunning(id string) bool {
	a.runningMu.Lock()
	if a.runningSessions != nil && a.runningSessions[id] > 0 {
		a.runningMu.Unlock()
		return true
	}
	a.runningMu.Unlock()
	v := a.runningSession.Load()
	s, _ := v.(string)
	return s != "" && s == id
}

// beginSessionRun/endSessionRun maintain the full running-session set. A
// single atomic string is retained only as a source-compatible legacy signal;
// it cannot represent concurrent Agent turns.
func (a *app) beginSessionRun(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	a.runningMu.Lock()
	if a.runningSessions == nil {
		a.runningSessions = make(map[string]int)
	}
	a.runningSessions[id]++
	a.runningMu.Unlock()
	a.runningSession.Store(id)
}

func (a *app) endSessionRun(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	a.runningMu.Lock()
	if a.runningSessions != nil {
		if count := a.runningSessions[id]; count <= 1 {
			delete(a.runningSessions, id)
		} else {
			a.runningSessions[id] = count - 1
		}
	}
	remaining := len(a.runningSessions) > 0
	a.runningMu.Unlock()
	if !remaining {
		a.runningSession.Store("")
	}
}

// subagentRunningCount returns how many of the session's spawned children are
// still running; a disabled subagent capability answers 0 (no children).
func (a *app) subagentRunningCount(ctx context.Context, sessionID string) int {
	if a.subagents == nil {
		return 0
	}
	children, err := a.subagents.ListChildren(ctx, sessionID)
	if err != nil {
		return 0 // fail-open: an unreadable count never disturbs the status
	}
	n := 0
	for _, c := range children {
		if c.Running {
			n++
		}
	}
	return n
}

// pendingInteraction returns a dsh pending-status key ("approval" today — the
// interact sensitive-tool gate) when the running session is blocked on the
// user. interact requests carry no session owner, so a pending request is
// attributed to the session whose turn created it (the running session).
func (a *app) pendingInteraction(ctx context.Context, sessionID string) string {
	if a.interacts == nil || !a.isSessionRunning(sessionID) {
		return ""
	}
	reqs, err := a.interacts.List(ctx)
	if err != nil || len(reqs) == 0 {
		return ""
	}
	for _, req := range reqs {
		if req.Status != interact.StatusPending {
			continue
		}
		owned := req.SessionID == sessionID || a.interactionBelongsTo(req.ID, sessionID)
		if req.SessionID == "" && !a.interactionHasOwner(req.ID) {
			// Direct/legacy engines predate SessionID. Preserve their single
			// active-session behavior until the app has indexed an owner.
			owned = true
		}
		if owned {
			return "approval"
		}
	}
	return ""
}

func (a *app) interactionHasOwner(id string) bool {
	a.interactionMu.RLock()
	_, ok := a.interactionSessions[id]
	a.interactionMu.RUnlock()
	return ok
}

// pendingLabel maps a dsh pending-status key to the localized status label.
func pendingLabel(p string) string {
	switch p {
	case "plan-review":
		return "计划待审"
	case "question":
		return "等待回答"
	default:
		return "等待审批"
	}
}

// subagentStatus is the secondary running-subagent status entry (dsh
// status.subagentsRunning).
func subagentStatus(n int) webserver.StatusEntry {
	return webserver.StatusEntry{State: "ongoing", Label: fmt.Sprintf("%d 个子代理运行中", n)}
}

// markSessionViewed records an open/message at the current time, clearing a
// session's finished-but-unviewed reminder. Fail-open: a store error never
// disturbs the flow.
func (a *app) markSessionViewed(ctx context.Context, id string) {
	if a.store == nil || id == "" {
		return
	}
	_ = a.store.MarkSessionViewed(ctx, id, time.Now().UTC())
}
