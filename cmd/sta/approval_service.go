package main

import (
	"context"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/interact"
)

// approvalAnswerer is the application-owned answerer contract shared by the
// CLI, Web and ACP surfaces. The interact.Engine remains the Provider/Service
// state machine; this seam adds the application concerns that all answerers
// must observe: session scoping, durable audit projection and rollback on a
// failed non-atomic commit.
//
// compatibilityEvent is intentionally explicit. It is true only for the
// legacy REPL path, whose old interact/* event names are still consumed by
// compatibility readers. Agent/Web/ACP paths always use canonical approval/*
// events through the same resolver.
type approvalAnswerer interface {
	List(context.Context, string) ([]interact.Request, error)
	Resolve(context.Context, string, string, interact.ApprovalStatus, string, bool) error
}

type appApprovalAnswerer struct{ app *app }

func (a *app) approvalAnswerer() approvalAnswerer {
	return appApprovalAnswerer{app: a}
}

func (s appApprovalAnswerer) List(ctx context.Context, sessionID string) ([]interact.Request, error) {
	if s.app == nil || s.app.interacts == nil || strings.TrimSpace(sessionID) == "" {
		return []interact.Request{}, nil
	}
	if lister, ok := s.app.interacts.(interact.SessionLister); ok {
		return lister.ListForSession(ctx, sessionID)
	}
	items, err := s.app.interacts.List(ctx)
	if err != nil {
		return nil, err
	}
	// Compatibility providers may not have a scoped read operation. Keep the
	// fallback fail-closed with respect to unowned rows: the process-wide
	// approval queue must never become a Web/ACP disclosure surface.
	filtered := make([]interact.Request, 0, len(items))
	for _, item := range items {
		if item.SessionID == sessionID || s.app.interactionBelongsTo(item.ID, sessionID) {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func (s appApprovalAnswerer) Resolve(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string, compatibilityEvent bool) error {
	if s.app == nil || s.app.interacts == nil || strings.TrimSpace(sessionID) == "" {
		return interact.ErrUnknownRequest
	}
	return s.app.resolveInteractionDurablyAs(ctx, sessionID, id, status, answer, compatibilityEvent)
}
