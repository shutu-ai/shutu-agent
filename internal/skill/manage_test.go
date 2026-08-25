package skill

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mgr builds a Manager pinned to temp project/home/custom roots.
func mgr(t *testing.T) (*Manager, string, string) {
	t.Helper()
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m, err := NewManager(FSOpts{ProjectRoot: proj, UserHome: home})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, proj, home
}

// seedSkill writes a bundle or flat skill into the given scope of m.
func seedSkill(t *testing.T, m *Manager, proj, home, scope, name, desc string) {
	t.Helper()
	var root string
	if scope == ScopeProject {
		root = filepath.Join(proj, ".dsh", "skills")
	} else {
		root = filepath.Join(home, ".dsh", "skills")
	}
	writeFile(t, filepath.Join(root, name, "SKILL.md"),
		"---\ndescription: "+desc+"\n---\n# "+name+"\nBody of "+name+".\n")
}

func namesInScope(t *testing.T, m *Manager, scope string) []string {
	t.Helper()
	all, err := m.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	var names []string
	for _, e := range all {
		if e.Scope == scope {
			names = append(names, e.Name)
		}
	}
	return names
}

// TestManageListAllScopesAndDisabled covers recursive discovery with scope,
// rel, and disabled entries kept visible.
func TestManageListAllScopesAndDisabled(t *testing.T) {
	m, proj, home := mgr(t)
	seedSkill(t, m, proj, home, ScopeProject, "proj-skill", "project skill")
	seedSkill(t, m, proj, home, ScopeGlobal, "user-skill", "user skill")
	// A disabled bundle: rename its SKILL.md to SKILL.md.disabled.
	disabled := filepath.Join(home, ".dsh", "skills", "off-skill", "SKILL.md")
	writeFile(t, disabled, "---\ndescription: off\n---\n# off\n")
	if err := os.Rename(disabled, disabled+DISABLED_SUFFIX); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// A nested 分类 folder keeps a skill inside a subfolder (rel set).
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "category", "nested-skill", "SKILL.md"),
		"---\ndescription: nested\n---\n# nested\n")

	all, err := m.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	byKey := map[string]ManageEntry{}
	for _, e := range all {
		byKey[e.Scope+"\u0000"+e.Name] = e
	}
	projEntry, ok := byKey[ScopeProject+"\u0000proj-skill"]
	if !ok {
		t.Fatalf("project skill not listed: %+v", all)
	}
	if projEntry.Enabled != true || projEntry.Kind != "bundle" {
		t.Errorf("proj entry enabled=%v kind=%v", projEntry.Enabled, projEntry.Kind)
	}
	globalEntry, ok := byKey[ScopeGlobal+"\u0000user-skill"]
	if !ok {
		t.Fatalf("global skill not listed: %+v", all)
	}
	if globalEntry.Scope != ScopeGlobal || globalEntry.Source != SourceUserDSH {
		t.Errorf("global entry scope=%s source=%s", globalEntry.Scope, globalEntry.Source)
	}
	off, ok := byKey[ScopeGlobal+"\u0000off-skill"]
	if !ok {
		t.Fatalf("disabled skill not listed: %+v", all)
	}
	if off.Enabled != false {
		t.Errorf("disabled entry enabled=%v", off.Enabled)
	}
	nested, ok := byKey[ScopeProject+"\u0000nested-skill"]
	if !ok {
		t.Fatalf("nested skill not listed: %+v", all)
	}
	if nested.Rel != "category/nested-skill" {
		t.Errorf("nested rel=%q", nested.Rel)
	}
}

// TestManageSetEnabledHotToggles covers .disabled rename in the exact scope:
// disabling flips the entry's Enabled flag (the entry stays listed so the page
// can re-enable it) and does not touch the other scope's copy.
func TestManageSetEnabledHotToggles(t *testing.T) {
	m, proj, home := mgr(t)
	seedSkill(t, m, proj, home, ScopeProject, "same", "project copy")
	seedSkill(t, m, proj, home, ScopeGlobal, "same", "global copy")

	if err := m.SetEnabled(context.Background(), "same", ScopeGlobal, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	all, err := m.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	var globalEnabled, projectEnabled bool
	for _, e := range all {
		if e.Name != "same" {
			continue
		}
		if e.Scope == ScopeGlobal {
			globalEnabled = e.Enabled
		} else if e.Scope == ScopeProject {
			projectEnabled = e.Enabled
		}
	}
	if globalEnabled {
		t.Errorf("global copy should be disabled after disable")
	}
	if !projectEnabled {
		t.Errorf("project copy should stay enabled")
	}
	if err := m.SetEnabled(context.Background(), "same", ScopeGlobal, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	all, err = m.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	globalEnabled = false
	for _, e := range all {
		if e.Name == "same" && e.Scope == ScopeGlobal {
			globalEnabled = e.Enabled
		}
	}
	if !globalEnabled {
		t.Errorf("global copy should be enabled after re-enable")
	}
}

// TestManageDeleteScoped covers deleting the exact scope's copy.
func TestManageDeleteScoped(t *testing.T) {
	m, proj, home := mgr(t)
	seedSkill(t, m, proj, home, ScopeProject, "doomed", "p")
	seedSkill(t, m, proj, home, ScopeGlobal, "doomed", "g")
	if err := m.Delete(context.Background(), "doomed", ScopeGlobal); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if names := namesInScope(t, m, ScopeGlobal); len(names) != 0 {
		t.Errorf("global still lists %v", names)
	}
	if names := namesInScope(t, m, ScopeProject); len(names) != 1 {
		t.Errorf("project copy deleted too: %v", names)
	}
	if err := m.Delete(context.Background(), "doomed", ScopeProject); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if err := m.Delete(context.Background(), "doomed", ScopeGlobal); err == nil {
		t.Errorf("delete of absent skill should error")
	}
}

// TestManageAddFlatAndBundle covers adding a flat skill, a bundle, duplicate
// rejection, and invalid frontmatter rejection.
func TestManageAddFlatAndBundle(t *testing.T) {
	m, _, _ := mgr(t)
	valid := "---\nname: fresh-skill\ndescription: a brand new skill\n---\n# Fresh\nDo things.\n"
	base64Of := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	added, err := m.AddSkill(context.Background(), "flat", []AddFile{
		{Path: "fresh-skill.md", Base64: base64Of(valid)},
	}, ScopeGlobal)
	if err != nil {
		t.Fatalf("add flat: %v", err)
	}
	if !strings.Contains(added, "fresh-skill") {
		t.Errorf("added=%q", added)
	}
	if names := namesInScope(t, m, ScopeGlobal); len(names) != 1 || names[0] != "fresh-skill" {
		t.Errorf("global names after add: %v", names)
	}

	// Duplicate name is refused (already exists).
	if _, err := m.AddSkill(context.Background(), "flat", []AddFile{
		{Path: "fresh-skill.md", Base64: base64Of(valid)},
	}, ScopeGlobal); err == nil {
		t.Errorf("duplicate add should error")
	}

	// Invalid frontmatter is refused before any write.
	bad := "---\nname: no-desc\n---\nNo description here.\n"
	if _, err := m.AddSkill(context.Background(), "flat", []AddFile{
		{Path: "no-desc.md", Base64: base64Of(bad)},
	}, ScopeGlobal); err == nil {
		t.Errorf("invalid frontmatter add should error")
	}
	if names := namesInScope(t, m, ScopeGlobal); len(names) != 1 {
		t.Errorf("invalid add left artifacts: %v", names)
	}

	// Bundle add: one top-level folder with SKILL.md.
	bundleMD := "---\nname: bundled-skill\ndescription: bundle skill\n---\n# B\nBody.\n"
	added, err = m.AddSkill(context.Background(), "bundle", []AddFile{
		{Path: "bundled-skill/SKILL.md", Base64: base64Of(bundleMD)},
		{Path: "bundled-skill/reference.md", Base64: base64Of("# ref")},
	}, ScopeProject)
	if err != nil {
		t.Fatalf("add bundle: %v", err)
	}
	if names := namesInScope(t, m, ScopeProject); len(names) != 1 || names[0] != "bundled-skill" {
		t.Errorf("project names after bundle add: %v", names)
	}
}

// TestManageAddZipAutoDetect covers zip mode: a single top-level folder with
// SKILL.md imports as a bundle; a __MACOSX junk entry is skipped.
func TestManageAddZipAutoDetect(t *testing.T) {
	m, _, _ := mgr(t)
	zipBytes := buildZip(t, map[string]string{
		"my-zipped/SKILL.md":        "---\nname: my-zipped\ndescription: zipped skill\n---\n# Z\nBody.\n",
		"my-zipped/notes.md":        "# notes",
		"__MACOSX/my-zipped/._junk": "junk",
	})
	if _, err := m.AddSkill(context.Background(), "zip", []AddFile{
		{Path: "my-zipped.zip", Base64: base64.StdEncoding.EncodeToString(zipBytes)},
	}, ScopeGlobal); err != nil {
		t.Fatalf("add zip: %v", err)
	}
	if names := namesInScope(t, m, ScopeGlobal); len(names) != 1 || names[0] != "my-zipped" {
		t.Errorf("zip add names: %v", names)
	}
}

// TestManageMigrateCopyAndMove covers cross-scope migration in both modes.
func TestManageMigrateCopyAndMove(t *testing.T) {
	m, proj, home := mgr(t)
	seedSkill(t, m, proj, home, ScopeGlobal, "copy-skill", "copy me")
	seedSkill(t, m, proj, home, ScopeGlobal, "move-skill", "move me")

	// copy: source stays.
	if err := m.Migrate(context.Background(), "copy-skill", ScopeGlobal, ScopeProject, "copy"); err != nil {
		t.Fatalf("migrate copy: %v", err)
	}
	if len(namesInScope(t, m, ScopeGlobal)) != 2 || len(namesInScope(t, m, ScopeProject)) != 1 {
		t.Errorf("copy should leave both scopes populated (global=%v project=%v)",
			namesInScope(t, m, ScopeGlobal), namesInScope(t, m, ScopeProject))
	}

	// move: source disappears.
	if err := m.Migrate(context.Background(), "move-skill", ScopeGlobal, ScopeProject, "move"); err != nil {
		t.Fatalf("migrate move: %v", err)
	}
	globalNames := namesInScope(t, m, ScopeGlobal)
	if containsName(globalNames, "move-skill") {
		t.Errorf("move should clear source scope: %v", globalNames)
	}
	projectNames := namesInScope(t, m, ScopeProject)
	if !containsName(projectNames, "move-skill") {
		t.Errorf("move target missing: %v", projectNames)
	}
}

// containsName reports whether names contains name.
func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// TestManageGroupsPersist covers save/delete/rename of group config.
func TestManageGroupsPersist(t *testing.T) {
	m, _, _ := mgr(t)
	rows, err := m.Groups()
	if err != nil || len(rows) != 0 {
		t.Fatalf("initial groups: %v %v", rows, err)
	}
	rows, err = m.SaveGroup("", "Dev", "", []string{"a", "b"})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "Dev" || len(rows[0].Scopes["global"]) != 2 {
		t.Errorf("saved group: %+v", rows)
	}
	id := rows[0].ID
	// Rename via same id.
	rows, err = m.SaveGroup(id, "DevOps", "project", []string{"x"})
	if err != nil {
		t.Fatalf("rename group: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "DevOps" || len(rows[0].Scopes["project"]) != 1 {
		t.Errorf("renamed group: %+v", rows)
	}
	rows, err = m.DeleteGroup(id)
	if err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("groups after delete: %+v", rows)
	}
}

// writeZipFile writes a zip archive at path with the given path→content map.
func writeZipFile(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, content := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// buildZip writes a zip archive with the given path→content map (for tests).
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "a.zip")
	// Use the package-level zip writer helper in a separate temp file.
	writeZipFile(t, zipPath, files)
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	return data
}
