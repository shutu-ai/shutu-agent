package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/skill"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

// skillWebFixture builds an app wired for the skill-page dispatcher with
// deterministic project/user roots. It does NOT call registerSkills — the
// manager is independent of skill.enabled, so the page works either way.
func skillWebFixture(t *testing.T) (*app, string, string) {
	t.Helper()
	root := t.TempDir()
	proj := filepath.Join(root, "repo")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	a := &app{
		cfg:              config.Config{Skill: config.SkillConfig{Enabled: config.Bool(true)}},
		reg:              tools.New(),
		log:              session.New(),
		currentID:        "s-test",
		skillProjectRoot: proj,
		skillUserHome:    home,
	}
	return a, proj, home
}

// seedSkillFile writes a bundle skill into the given scope root.
func seedSkillFile(t *testing.T, scopeRoot, name, desc string) {
	t.Helper()
	writeFileAt(t, filepath.Join(scopeRoot, name, "SKILL.md"),
		"---\ndescription: "+desc+"\n---\n# "+name+"\nBody of "+name+".\n")
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWebSkillsListBoot covers the boot fetch: list returns skills (with scope
// + enabled), groups and the two scopes.
func TestWebSkillsListBoot(t *testing.T) {
	a, proj, home := skillWebFixture(t)
	seedSkillFile(t, filepath.Join(proj, ".dsh", "skills"), "proj-skill", "project skill")
	seedSkillFile(t, filepath.Join(home, ".dsh", "skills"), "user-skill", "user skill")

	result, err := a.webSkills(context.Background(), "list", webserver.SkillRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// groups + scopes present.
	if result["groups"] == nil || result["scopes"] == nil {
		t.Fatalf("boot view missing groups/scopes: %+v", result)
	}
	skills := result["skills"].([]map[string]any)
	if len(skills) != 2 {
		t.Fatalf("skills len = %d, want 2: %+v", len(skills), skills)
	}
	var sawProject, sawGlobal bool
	for _, s := range skills {
		if s["name"] == "proj-skill" && s["scope"] == "project" {
			sawProject = true
		}
		if s["name"] == "user-skill" && s["scope"] == "global" {
			sawGlobal = true
		}
	}
	if !sawProject || !sawGlobal {
		t.Errorf("missing scope rows: %+v", skills)
	}
}

func TestNativeSkillCatalogUsesRequestedProjectRoot(t *testing.T) {
	a, proj, home := skillWebFixture(t)
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	seedSkillFile(t, filepath.Join(proj, ".dsh", "skills"), "startup-skill", "startup project")
	other := filepath.Join(t.TempDir(), "workspace")
	writeFileAt(t, filepath.Join(other, ".dsh", "skills", "session-skill", "SKILL.md"),
		"---\ndescription: session project\nwhenToUse: when requested\n---\nBody\n")
	seedSkillFile(t, filepath.Join(home, ".dsh", "skills"), "global-skill", "global")

	entries, err := a.nativeSkillCatalog(context.Background(), other)
	if err != nil {
		t.Fatalf("nativeSkillCatalog: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if len(entries) != 2 || names[0] != "global-skill" || names[1] != "session-skill" {
		t.Fatalf("catalog names = %#v, want global + requested session project", names)
	}
	if !entries[1].ModelInvocable {
		t.Fatalf("session skill = %#v, want model invocable", entries[1])
	}
	if entries[1].WhenToUse != "when requested" {
		t.Fatalf("whenToUse = %q, want requested routing hint", entries[1].WhenToUse)
	}
}

// TestWebSkillsEnableDisableDelete covers hot-disable, re-enable and delete via
// the dispatcher, scoped to global.
func TestWebSkillsEnableDisableDelete(t *testing.T) {
	a, _, home := skillWebFixture(t)
	seedSkillFile(t, filepath.Join(home, ".dsh", "skills"), "flip", "to flip")

	// Disable: flip goes enabled=false.
	if _, err := a.webSkills(context.Background(), "set_enabled", webserver.SkillRequest{
		Name: "flip", Scope: "global", Enabled: false,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	result, _ := a.webSkills(context.Background(), "list", webserver.SkillRequest{})
	found := false
	for _, s := range result["skills"].([]map[string]any) {
		if s["name"] == "flip" {
			found = true
			if s["enabled"] != false {
				t.Errorf("flip should be disabled after set_enabled=false")
			}
		}
	}
	if !found {
		t.Fatal("flip missing after disable")
	}
	// Re-enable.
	if _, err := a.webSkills(context.Background(), "set_enabled", webserver.SkillRequest{
		Name: "flip", Scope: "global", Enabled: true,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Delete.
	if _, err := a.webSkills(context.Background(), "delete", webserver.SkillRequest{
		Name: "flip", Scope: "global",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	result, _ = a.webSkills(context.Background(), "list", webserver.SkillRequest{})
	if len(result["skills"].([]map[string]any)) != 0 {
		t.Errorf("skills after delete: %+v", result["skills"])
	}
}

// TestWebSkillsContent covers the content action returning the raw markdown.
func TestWebSkillsContent(t *testing.T) {
	a, _, home := skillWebFixture(t)
	seedSkillFile(t, filepath.Join(home, ".dsh", "skills"), "readme-skill", "read me")

	result, err := a.webSkills(context.Background(), "content", webserver.SkillRequest{
		Name: "readme-skill", Scope: "global",
	})
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	if !strings.Contains(result["content"].(string), "Body of readme-skill") {
		t.Errorf("content = %q", result["content"])
	}
}

// TestWebSkillsAddFlat covers adding a flat skill via the dispatcher.
func TestWebSkillsAddFlat(t *testing.T) {
	a, _, _ := skillWebFixture(t)
	body := "---\nname: fresh-flat\ndescription: a fresh flat skill\n---\n# F\nDo it.\n"
	result, err := a.webSkills(context.Background(), "add", webserver.SkillRequest{
		Kind:  "flat",
		Scope: "global",
		Files: []webserver.SkillFile{{Path: "fresh-flat.md", Base64: base64.StdEncoding.EncodeToString([]byte(body))}},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(result["names"].(string), "fresh-flat") {
		t.Errorf("add names = %q", result["names"])
	}
}

// TestWebSkillsMigrate covers move between scopes via the dispatcher.
func TestWebSkillsMigrate(t *testing.T) {
	a, proj, home := skillWebFixture(t)
	seedSkillFile(t, filepath.Join(home, ".dsh", "skills"), "move-me", "to move")

	if _, err := a.webSkills(context.Background(), "migrate", webserver.SkillRequest{
		Name: "move-me", From: "global", To: "project", Mode: "move",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	result, _ := a.webSkills(context.Background(), "list", webserver.SkillRequest{})
	var inProject bool
	for _, s := range result["skills"].([]map[string]any) {
		if s["name"] == "move-me" && s["scope"] == "project" {
			inProject = true
		}
	}
	if !inProject {
		t.Errorf("move-me should be in project after migrate: %+v", result["skills"])
	}
	_ = proj
}

// TestWebSkillsGroups covers group save/delete via the dispatcher.
func TestWebSkillsGroups(t *testing.T) {
	a, _, _ := skillWebFixture(t)
	result, err := a.webSkills(context.Background(), "group_save", webserver.SkillRequest{
		GroupName: "Dev", Scope: "global", Names: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	groups := result["groups"].([]map[string]any)
	if len(groups) != 1 || groups[0]["name"] != "Dev" {
		t.Fatalf("group after save: %+v", groups)
	}
	id := groups[0]["id"].(string)
	// Serialize to ensure it's JSON-safe.
	raw, err := json.Marshal(groups)
	if err != nil {
		t.Fatalf("group not JSON-safe: %v", err)
	}
	_ = raw
	if _, err := a.webSkills(context.Background(), "group_delete", webserver.SkillRequest{
		GroupID: id,
	}); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	result, _ = a.webSkills(context.Background(), "list", webserver.SkillRequest{})
	if len(result["groups"].([]map[string]any)) != 0 {
		t.Errorf("groups after delete: %+v", result["groups"])
	}
}

// TestWebSkillsUnknownAction rejects an unhandled action.
func TestWebSkillsUnknownAction(t *testing.T) {
	a, _, _ := skillWebFixture(t)
	if _, err := a.webSkills(context.Background(), "bogus", webserver.SkillRequest{}); err == nil {
		t.Fatalf("unknown action should error")
	}
}

// TestWebSkillsAddRejectsInvalidFrontmatter guards the add validation path.
func TestWebSkillsAddRejectsInvalidFrontmatter(t *testing.T) {
	a, _, _ := skillWebFixture(t)
	bad := "---\nname: nodesc\n---\nNo description.\n"
	if _, err := a.webSkills(context.Background(), "add", webserver.SkillRequest{
		Kind:  "flat",
		Scope: "global",
		Files: []webserver.SkillFile{{Path: "nodedesc.md", Base64: base64.StdEncoding.EncodeToString([]byte(bad))}},
	}); err == nil {
		t.Fatalf("invalid frontmatter add should error")
	}
}

// TestWebSkillManagerIndependentOfEnabled verifies the manager is created even
// when skill.enabled=false (the page manages files regardless of the D10 gate).
func TestWebSkillManagerIndependentOfEnabled(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	a := &app{
		cfg:              config.Config{Skill: config.SkillConfig{Enabled: config.Bool(false)}},
		reg:              tools.New(),
		log:              session.New(),
		currentID:        "s-test",
		skillProjectRoot: proj,
		skillUserHome:    filepath.Join(root, "home"),
	}
	m, err := a.webSkillManager()
	if err != nil {
		t.Fatalf("webSkillManager: %v", err)
	}
	if m == nil {
		t.Fatal("manager must be created even when skill disabled")
	}
	_ = skill.ScopeGlobal
}
