package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/jabing/shutu-agent/internal/store"
)

// defaultWorkdir is the explicit fallback for ungrouped sessions. A bad or
// stale config path fails open to the process cwd, just like dsh's default.
func (a *app) defaultWorkdir() string {
	dir := strings.TrimSpace(a.cfg.Workspace.DefaultDir)
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir, _ = os.Getwd()
	}
	return filepath.Clean(dir)
}

// sessionCWD resolves the active session's immutable cwd header. A persisted
// workspace path wins over legacy/stale cwd values, matching dsh's invariant
// that tools use the workspace directory attached to the session.
func (a *app) sessionCWD() string {
	if a.store != nil && a.currentID != "" {
		if meta, err := a.store.GetSessionMeta(context.Background(), a.currentID); err == nil {
			if meta.WorkspaceID != "" {
				if workspaces, listErr := a.store.ListWorkspaces(context.Background()); listErr == nil {
					for _, ws := range workspaces {
						if ws.ID == meta.WorkspaceID && strings.TrimSpace(ws.Path) != "" {
							if abs, absErr := filepath.Abs(ws.Path); absErr == nil {
								if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
									return filepath.Clean(abs)
								}
							}
						}
					}
				}
			}
			if strings.TrimSpace(meta.CWD) != "" {
				return filepath.Clean(meta.CWD)
			}
		}
	}
	return a.defaultWorkdir()
}

func (a *app) setSessionCWD(ctx context.Context, sessionID, cwd string) error {
	hs, ok := a.store.(store.SessionHeaderStore)
	if !ok {
		return nil
	}
	return hs.SetSessionCWD(ctx, sessionID, cwd)
}
