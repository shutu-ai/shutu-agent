package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates path (and parents) with content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func listCandidates(t *testing.T, f *Filesystem) []Candidate {
	t.Helper()
	cands, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return cands
}

func candidateByName(t *testing.T, cands []Candidate, name string) Candidate {
	t.Helper()
	for _, c := range cands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("candidate %q not found in %+v", name, cands)
	return Candidate{}
}

// TestFilesystemDiscoveryRanksAndIdentity covers the required filesystem
// discovery: bundle (<name>/SKILL.md) + flat (<name>.md) discovery, rank order
// across roots, non-recursive scanning, and name/description derivation.
func TestFilesystemDiscoveryRanksAndIdentity(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	home := filepath.Join(tmp, "home")
	custom := filepath.Join(tmp, "custom")

	// project-dsh (rank 100): a bundle and two flat files.
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "bundle-one", "SKILL.md"),
		"---\ndescription: bundle one\n---\n# Bundle One\nInstructions.\n")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "flat-one.md"),
		"---\ndescription: flat one\n---\nFlat one body\n")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "flat-body-desc.md"),
		"Flat body desc\nmore lines\n")
	// Non-recursive: nested bundles/flat files must NOT be discovered.
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "nested", "deep", "SKILL.md"), "nested bundle")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "nested", "nested-flat.md"), "nested flat")

	// project-agents (rank 200).
	writeFile(t, filepath.Join(proj, ".agents", "skills", "flat-two.md"), "Flat two\nmore\n")

	// user-dsh (rank 400).
	writeFile(t, filepath.Join(home, ".dsh", "skills", "flat-three.md"),
		"---\ndescription: flat three\n---\nThree body\n")

	// custom (rank 300).
	writeFile(t, filepath.Join(custom, "custom-one.md"),
		"---\ndescription: custom one\n---\nCustom body\n")

	// The project root marker for the .git walk.
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	f, err := NewFilesystem(FSOpts{ProjectRoot: proj, UserHome: home, Dirs: []string{custom}})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	cands := listCandidates(t, f)

	if len(cands) != 6 {
		t.Fatalf("List returned %d candidates, want 6: %+v", len(cands), cands)
	}

	// Rank order across roots: project-dsh 100 < project-agents 200 < custom 300
	// < user-dsh 400.
	wantRanks := map[string]struct {
		source string
		rank   int
	}{
		"bundle-one":     {SourceProjectDSH, RankProjectDSH},
		"flat-one":       {SourceProjectDSH, RankProjectDSH},
		"flat-body-desc": {SourceProjectDSH, RankProjectDSH},
		"flat-two":       {SourceProjectAgents, RankProjectAgents},
		"custom-one":     {SourceCustom, RankCustom},
		"flat-three":     {SourceUserDSH, RankUserDSH},
	}
	for name, want := range wantRanks {
		c := candidateByName(t, cands, name)
		if c.Source != want.source || c.Rank != want.rank {
			t.Fatalf("%s candidate = %+v, want source %q rank %d", name, c, want.source, want.rank)
		}
	}

	// Identity: bundle name comes from the directory, flat name from the file
	// basename; the bundle path points at SKILL.md inside the directory.
	b := candidateByName(t, cands, "bundle-one")
	if b.Path != filepath.Join(proj, ".dsh", "skills", "bundle-one", "SKILL.md") {
		t.Fatalf("bundle-one path = %q, want %q", b.Path, filepath.Join(proj, ".dsh", "skills", "bundle-one", "SKILL.md"))
	}
	if !filepath.IsAbs(b.Path) {
		t.Fatalf("bundle-one path %q is not absolute", b.Path)
	}
	fo := candidateByName(t, cands, "flat-one")
	if fo.Path != filepath.Join(proj, ".dsh", "skills", "flat-one.md") {
		t.Fatalf("flat-one path = %q, want %q", fo.Path, filepath.Join(proj, ".dsh", "skills", "flat-one.md"))
	}

	// Description: frontmatter description wins, else the first body line.
	if c := candidateByName(t, cands, "bundle-one"); c.Description != "bundle one" {
		t.Fatalf("bundle-one description = %q, want frontmatter description", c.Description)
	}
	if c := candidateByName(t, cands, "flat-two"); c.Description != "Flat two" {
		t.Fatalf("flat-two description = %q, want first body line", c.Description)
	}
	if c := candidateByName(t, cands, "flat-body-desc"); c.Description != "Flat body desc" {
		t.Fatalf("flat-body-desc description = %q, want first body line", c.Description)
	}

	// Non-recursive: the nested skills were never discovered.
	for _, name := range []string{"deep", "nested-flat", "nested"} {
		for _, c := range cands {
			if c.Name == name {
				t.Fatalf("non-recursive scan discovered %q", name)
			}
		}
	}
}

// TestFilesystemFrontmatterInvocation covers disable-model-invocation and
// user-invocable frontmatter (defaults true when absent).
func TestFilesystemFrontmatterInvocation(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "model-only.md"),
		"---\ndescription: model only\n---\nA body\n")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "user-only.md"),
		"---\ndescription: user only\nuser-invocable: false\n---\nB body\n")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "human-only.md"),
		"---\ndescription: human only\ndisable-model-invocation: true\n---\nC body\n")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "both-off.md"),
		"---\ndescription: both off\ndisable-model-invocation: true\nuser-invocable: false\n---\nD body\n")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	f, err := NewFilesystem(FSOpts{ProjectRoot: proj, UserHome: filepath.Join(tmp, "home")})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	cands := listCandidates(t, f)

	want := []struct {
		name        string
		model, user bool
	}{
		{"model-only", true, true},
		{"user-only", true, false},
		{"human-only", false, true},
		{"both-off", false, false},
	}
	for _, w := range want {
		c := candidateByName(t, cands, w.name)
		def, err := f.Get(context.Background(), c)
		if err != nil {
			t.Fatalf("Get(%s): %v", w.name, err)
		}
		if def == nil {
			t.Fatalf("Get(%s) = nil", w.name)
		}
		if def.ModelInvocable != w.model || def.UserInvocable != w.user {
			t.Fatalf("%s invocation = (%v, %v), want (%v, %v)", w.name, def.ModelInvocable, def.UserInvocable, w.model, w.user)
		}
	}
}

// TestFilesystemGetFullContent covers Get loading the complete trimmed body.
func TestFilesystemGetFullContent(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "guide", "SKILL.md"),
		"---\ndescription: guide\n---\n# Guide\n\nStep one.\nStep two.\n")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	f, err := NewFilesystem(FSOpts{ProjectRoot: proj, UserHome: filepath.Join(tmp, "home")})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	c := candidateByName(t, listCandidates(t, f), "guide")
	def, err := f.Get(context.Background(), c)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if def == nil {
		t.Fatal("Get = nil")
	}
	wantContent := "# Guide\n\nStep one.\nStep two."
	if def.Content != wantContent {
		t.Fatalf("content = %q, want %q", def.Content, wantContent)
	}
	if def.Name != "guide" || def.Description != "guide" || def.Source != SourceProjectDSH || def.Path != c.Path {
		t.Fatalf("def = %+v, want full identity/source/path", def)
	}
	// Get is idempotent and re-readable.
	if def2, err := f.Get(context.Background(), c); err != nil || def2 == nil || def2.Content != wantContent {
		t.Fatalf("second Get = %v, %v", def2, err)
	}
}

// TestFilesystemCRLFFrontmatter covers CRLF tolerance in frontmatter lines.
func TestFilesystemCRLFFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "crlf.md"),
		"---\r\ndescription: crlf desc\r\n---\r\nbody\r\n")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	f, err := NewFilesystem(FSOpts{ProjectRoot: proj, UserHome: filepath.Join(tmp, "home")})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	c := candidateByName(t, listCandidates(t, f), "crlf")
	if c.Description != "crlf desc" {
		t.Fatalf("crlf description = %q, want %q", c.Description, "crlf desc")
	}
}

// TestFilesystemProjectRootWalk covers the .git ancestor walk and the
// no-.git fallback to the start directory.
func TestFilesystemProjectRootWalk(t *testing.T) {
	tmp := t.TempDir()

	t.Run("walks to nearest .git ancestor", func(t *testing.T) {
		proj := filepath.Join(tmp, "repo")
		writeFile(t, filepath.Join(proj, ".dsh", "skills", "walk.md"), "walk me")
		if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		// Start deep below the repo root; the provider must walk up to repo.
		f, err := NewFilesystem(FSOpts{ProjectRoot: filepath.Join(proj, "sub", "deep"), UserHome: filepath.Join(tmp, "home"), RootBoundary: tmp})
		if err != nil {
			t.Fatalf("NewFilesystem: %v", err)
		}
		wantRoot := filepath.Join(proj, ".dsh", "skills")
		if f.roots[0].path != wantRoot {
			t.Fatalf("project-dsh root = %q, want %q", f.roots[0].path, wantRoot)
		}
		if c := candidateByName(t, listCandidates(t, f), "walk"); c.Path != filepath.Join(wantRoot, "walk.md") {
			t.Fatalf("walk candidate = %+v", c)
		}
	})

	t.Run("no .git ancestor falls back to start", func(t *testing.T) {
		nogit := filepath.Join(tmp, "nogit")
		writeFile(t, filepath.Join(nogit, ".dsh", "skills", "solo.md"), "solo")
		f, err := NewFilesystem(FSOpts{ProjectRoot: nogit, UserHome: filepath.Join(tmp, "home"), RootBoundary: tmp})
		if err != nil {
			t.Fatalf("NewFilesystem: %v", err)
		}
		if f.roots[0].path != filepath.Join(nogit, ".dsh", "skills") {
			t.Fatalf("project-dsh root = %q, want %q", f.roots[0].path, filepath.Join(nogit, ".dsh", "skills"))
		}
		if _, err := NewFilesystem(FSOpts{UserHome: filepath.Join(tmp, "home"), RootBoundary: tmp}); err != nil {
			t.Fatalf("empty ProjectRoot must fall back to the working directory: %v", err)
		}
	})
}

// TestFilesystemMissingRoots covers that absent roots yield an empty catalog
// rather than an error.
func TestFilesystemMissingRoots(t *testing.T) {
	tmp := t.TempDir()
	f, err := NewFilesystem(FSOpts{ProjectRoot: filepath.Join(tmp, "none"), UserHome: filepath.Join(tmp, "nohome"), RootBoundary: tmp})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	if cands := listCandidates(t, f); len(cands) != 0 {
		t.Fatalf("List with missing roots = %+v, want empty", cands)
	}
}

// TestFilesystemInvalidEntriesSkipped covers entries that are not skills:
// non-kebab-case names, non-.md files, bundles without SKILL.md, and invalid
// frontmatter.
func TestFilesystemInvalidEntriesSkipped(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "bad name", "SKILL.md"), "spacey dir")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "notes.txt"), "not a skill")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "Upper.md"), "uppercase")
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "bad-front.md"), "---\n[unclosed\n---\nbody")
	// empty-dir holds no SKILL.md: its bundle has nothing to parse.
	writeFile(t, filepath.Join(proj, ".dsh", "skills", "empty-dir", "readme.txt"), "no skill here")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	f, err := NewFilesystem(FSOpts{ProjectRoot: proj, UserHome: filepath.Join(tmp, "home")})
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	if cands := listCandidates(t, f); len(cands) != 0 {
		t.Fatalf("invalid entries must be skipped, got %+v", cands)
	}
}
