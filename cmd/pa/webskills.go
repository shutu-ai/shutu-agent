// webskills.go — the 技能 settings-page management API (dsh-skill-mcp-panel
// 对齐, user 2026-09). It adapts the webserver's /api/config/skills dispatcher
// to the internal/skill Manager: list (readonly catalog + groups), content,
// set_enabled (hot *.disabled rename), delete, add (bundle/flat/zip), migrate
// (copy/move between the two scopes) and the group save/delete calls. Every
// view map carries only leaf fields — never the Manager or any Host reference.
//
// Scope is a string ("global" | "project"): the plugin's per-workspace scope
// object collapses to our two roots (project root vs user root, 有差异对齐,
// 显式记录). The manager is created lazily so the page works even when
// skill.enabled is off (the model-facing catalog stays gated; the page manages
// the same files the catalog would discover).
package main

import (
	"context"
	"fmt"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/skill"
	"github.com/jabing/shutu-agent/internal/webserver"
)

// webSkillManager returns the lazily-created skill Manager, building it from
// the same root overrides the model-facing provider uses.
func (a *app) webSkillManager() (*skill.Manager, error) {
	if a.skillManager != nil {
		return a.skillManager, nil
	}
	m, err := skill.NewManager(skill.FSOpts{
		ProjectRoot: a.skillProjectRoot,
		UserHome:    a.skillUserHome,
		Dirs:        a.cfg.Skill.Dirs,
	})
	if err != nil {
		return nil, fmt.Errorf("skill: build manager: %w", err)
	}
	a.skillManager = m
	return m, nil
}

// webSkills is the webserver's skill-management dispatcher (POST
// /api/config/skills with an "action", GET /api/config/skills for the boot
// list). It delegates every action to the Manager and returns a JSON-safe map.
func (a *app) webSkills(ctx context.Context, action string, req webserver.SkillRequest) (map[string]any, error) {
	m, err := a.webSkillManager()
	if err != nil {
		return nil, err
	}
	scopes := []map[string]any{
		{"id": "global", "label": "全局"},
		{"id": "project", "label": "项目"},
	}
	switch action {
	case "list":
		skills, err := m.ListAll(ctx)
		if err != nil {
			return nil, err
		}
		groups, err := m.Groups()
		if err != nil {
			return nil, err
		}
		view := make([]map[string]any, 0, len(skills))
		for _, e := range skills {
			view = append(view, skillEntryView(e))
		}
		return map[string]any{
			"skills":  view,
			"groups":  groupRowsView(groups),
			"scopes":  scopes,
			"enabled": config.Enabled(a.cfg.Skill.Enabled),
		}, nil

	case "content":
		entry, err := m.GetEntry(ctx, req.Name, req.Scope)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			return nil, fmt.Errorf("技能 %q 在作用域 %q 中不存在", req.Name, req.Scope)
		}
		content, err := m.Content(ctx, req.Name, req.Scope)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"name":        entry.Name,
			"description": entry.Description,
			"content":     content,
			"source":      entry.Source,
			"path":        entry.File,
			"scope":       entry.Scope,
		}, nil

	case "set_enabled":
		if err := m.SetEnabled(ctx, req.Name, req.Scope, req.Enabled); err != nil {
			return nil, err
		}
		return map[string]any{"name": req.Name, "scope": req.Scope, "enabled": req.Enabled}, nil

	case "delete":
		if err := m.Delete(ctx, req.Name, req.Scope); err != nil {
			return nil, err
		}
		return map[string]any{"name": req.Name, "scope": req.Scope}, nil

	case "add":
		files := make([]skill.AddFile, 0, len(req.Files))
		for _, f := range req.Files {
			files = append(files, skill.AddFile{Path: f.Path, Base64: f.Base64})
		}
		names, err := m.AddSkill(ctx, req.Kind, files, req.Scope)
		if err != nil {
			return nil, err
		}
		return map[string]any{"names": names, "kind": req.Kind, "scope": req.Scope}, nil

	case "migrate":
		if err := m.Migrate(ctx, req.Name, req.From, req.To, req.Mode); err != nil {
			return nil, err
		}
		return map[string]any{"name": req.Name, "from": req.From, "to": req.To}, nil

	case "group_save":
		rows, err := m.SaveGroup(req.GroupID, req.GroupName, req.Scope, req.Names)
		if err != nil {
			return nil, err
		}
		return map[string]any{"groups": groupRowsView(rows)}, nil

	case "group_delete":
		rows, err := m.DeleteGroup(req.GroupID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"groups": groupRowsView(rows)}, nil

	default:
		return nil, fmt.Errorf("unknown skill action %q", action)
	}
}

// skillEntryView projects a ManageEntry to its wire map (leaf fields only).
func skillEntryView(e skill.ManageEntry) map[string]any {
	return map[string]any{
		"name":            e.Name,
		"description":     e.Description,
		"when_to_use":     e.WhenToUse,
		"enabled":         e.Enabled,
		"kind":            e.Kind,
		"source":          e.Source,
		"scope":           e.Scope,
		"rel":             e.Rel,
		"model_invocable": e.ModelInvocable,
		"user_invocable":  e.UserInvocable,
	}
}

// groupRowsView projects GroupRows to their wire maps.
func groupRowsView(rows []skill.GroupRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, g := range rows {
		scopes := make(map[string][]string, len(g.Scopes))
		for k, v := range g.Scopes {
			scopes[k] = v
		}
		out = append(out, map[string]any{"id": g.ID, "name": g.Name, "scopes": scopes})
	}
	return out
}
