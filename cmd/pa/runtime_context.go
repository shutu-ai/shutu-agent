package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

func (a *app) clearSessionApprovalPolicy(sessionID string) {
	if a == nil || a.interacts == nil || sessionID == "" {
		return
	}
	if canceler, ok := a.interacts.(interact.SessionCanceler); ok {
		cancelled, err := canceler.CancelForSession(context.Background(), sessionID)
		if err != nil && !errors.Is(err, interact.ErrEngineClosed) {
			fmt.Fprintf(os.Stderr, "pa: cancel approvals for session %q: %v\n", sessionID, err)
		}
		if len(cancelled) > 0 {
			log := a.log
			// An Agent runtime remains authoritative even when its id happens
			// to equal the legacy selection (tests, migration, or a resumed
			// native session can create that overlap). Never project its
			// disposal decision into the process-global log.
			if a.agentRegistry != nil || sessionID != a.currentID {
				if resolved, logErr := a.sessionLogForAgent(context.Background(), sessionID); logErr == nil {
					log = resolved
				} else {
					fmt.Fprintf(os.Stderr, "pa: approval cancellation log %q: %v\n", sessionID, logErr)
					log = nil
				}
			}
			if log != nil {
				for _, request := range cancelled {
					payload := map[string]any{"id": request.ID, "outcome": string(interact.StatusCanceled)}
					if request.CallID != "" {
						payload["callId"] = request.CallID
					}
					if _, appendErr := log.Append(session.EventApprovalDecided, payload); appendErr != nil {
						fmt.Fprintf(os.Stderr, "pa: approval cancellation event %q: %v\n", sessionID, appendErr)
					}
				}
			}
		}
	}
	if controller, ok := a.interacts.(interact.PolicyController); ok {
		controller.ClearSessionPolicy(sessionID)
	}
}

// runtimeSessionID is the composition-root bridge for model tool calls. The
// context value wins for Agent-owned runs. Once the Agent registry is mounted,
// an absent runtime identity is an authorization error represented as an empty
// owner; falling back to currentID here would silently cross session scope.
// The legacy REPL keeps the fallback only while no Agent registry exists.
func (a *app) runtimeSessionID(ctx context.Context) string {
	if id := runtimectx.SessionID(ctx); id != "" {
		return id
	}
	if a.agentRegistry != nil {
		return ""
	}
	return a.currentID
}

func (a *app) runtimeLog(ctx context.Context) *session.Log {
	id := runtimectx.SessionID(ctx)
	if id == "" {
		if a.agentRegistry != nil {
			return nil
		}
		return a.log
	}
	a.runtimeMu.Lock()
	if a.runtimeLogs != nil {
		if log := a.runtimeLogs[id]; log != nil {
			a.runtimeMu.Unlock()
			return log
		}
	}
	a.runtimeMu.Unlock()
	// A runtime context is authoritative; never let an Agent-owned session
	// silently borrow a different current log during bootstrap. The current
	// log is a valid fallback only when it names the same session.
	if a.agentRegistry == nil && id == a.currentID {
		return a.log
	}
	return nil
}
