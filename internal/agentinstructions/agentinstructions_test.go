package agentinstructions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBaselineDiscoversRootToCWDInstructions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cwd := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "AGENTS.md"), "global rules")
	write(t, filepath.Join(root, "AGENTS.md"), "root rules")
	write(t, filepath.Join(root, "CLAUDE.md"), "claude rules")
	write(t, filepath.Join(cwd, "AGENTS.md"), "app rules")

	files, _, err := LoadBaseline(cwd, Config{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.DisplayPath)
	}
	want := []string{"$SHUTU_HOME/AGENTS.md", "AGENTS.md", "CLAUDE.md", filepath.ToSlash(filepath.Join("packages", "app", "AGENTS.md"))}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("discovery order = %#v, want %#v", paths, want)
	}
}

func TestMessageBuildsDSHSourceAndReminder(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	write(t, filepath.Join(root, ".git"), "")
	write(t, filepath.Join(root, "AGENTS.md"), "use concise code")
	message, err := Message(root, Config{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if message == nil || message.SourceKind != "agent-instructions" ||
		message.SourceForm != "instructions" || !message.SourceBaseline ||
		message.SourceBaselineIdentity == "" {
		t.Fatalf("message source = %+v", message)
	}
	changes, ok := message.SourceChanges.([]Change)
	if !ok || len(changes) != 1 || changes[0].Action != "set" || changes[0].Path != "AGENTS.md" {
		t.Fatalf("source changes = %#v", message.SourceChanges)
	}
	text := message.Text()
	if !strings.Contains(text, "<system-reminder>") ||
		!strings.Contains(text, "Instructions from: AGENTS.md") ||
		!strings.Contains(text, "use concise code") {
		t.Fatalf("instruction text = %q", text)
	}
}

func TestReconcileSameIdentityUsesDeltas(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "AGENTS.md")
	write(t, filepath.Join(workspace, ".git"), "")
	write(t, path, "first rules")

	baseline, err := Message(workspace, Config{Home: home})
	if err != nil || baseline == nil || !baseline.SourceBaseline {
		t.Fatalf("baseline = %#v, err=%v", baseline, err)
	}
	changes, ok := baseline.SourceChanges.([]Change)
	if !ok || len(changes) != 1 {
		t.Fatalf("baseline changes = %#v", baseline.SourceChanges)
	}
	state := &State{Identity: baseline.SourceBaselineIdentity, Changes: map[string]Change{changes[0].Scope: changes[0]}}

	message, next, err := Reconcile(workspace, Config{Home: home}, state)
	if err != nil || message != nil || next == nil || len(next.Changes) != 1 {
		t.Fatalf("unchanged reconcile = %#v, %#v, %v", message, next, err)
	}

	write(t, path, "updated rules")
	message, next, err = Reconcile(workspace, Config{Home: home}, state)
	if err != nil || message == nil || message.SourceBaseline || len(next.Changes) != 1 {
		t.Fatalf("changed reconcile = %#v, %#v, %v", message, next, err)
	}
	updates, ok := message.SourceChanges.([]Change)
	if !ok || len(updates) != 1 || updates[0].Action != "replace" ||
		updates[0].Path != "AGENTS.md" {
		t.Fatalf("change source = %#v", message.SourceChanges)
	}
	if !strings.Contains(message.Text(), "Updated instructions from: AGENTS.md") ||
		!strings.Contains(message.Text(), "updated rules") {
		t.Fatalf("replace text = %q", message.Text())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	message, next, err = Reconcile(workspace, Config{Home: home}, next)
	if err != nil || message == nil || message.SourceBaseline || len(next.Changes) != 0 {
		t.Fatalf("removed reconcile = %#v, %#v, %v", message, next, err)
	}
	removals, ok := message.SourceChanges.([]Change)
	if !ok || len(removals) != 1 || removals[0].Action != "remove" {
		t.Fatalf("remove source = %#v", message.SourceChanges)
	}
	if !strings.Contains(message.Text(), "Instructions removed: AGENTS.md") {
		t.Fatalf("remove text = %q", message.Text())
	}
}

func TestIdentityChangeWithoutFilesRemovesPriorBaselineScopes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := &State{
		Identity: "identity-old",
		Changes: map[string]Change{
			CandidateScope(".", "AGENTS.md"): {
				Action: "set", Path: "AGENTS.md", Digest: "old",
			},
		},
	}
	message, next, err := Reconcile(workspace, Config{Home: t.TempDir(), MaxBytes: 256}, previous)
	if err != nil || message == nil || !message.SourceBaseline {
		t.Fatalf("replacement = %#v, %#v, %v", message, next, err)
	}
	changes, ok := message.SourceChanges.([]Change)
	if !ok || len(changes) != 1 || changes[0].Action != "remove" ||
		changes[0].Path != "AGENTS.md" {
		t.Fatalf("replacement source = %#v", message.SourceChanges)
	}
	if !strings.Contains(message.Text(), "No workspace instructions are currently active.") {
		t.Fatalf("replacement text = %q", message.Text())
	}
	if next == nil || len(next.Changes) != 0 {
		t.Fatalf("replacement state = %#v", next)
	}
}

func TestReconcileTouchDiscoversAndRemovesNestedInstructions(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	write(t, filepath.Join(workspace, ".git"), "")
	write(t, filepath.Join(workspace, "AGENTS.md"), "root rules")
	config := Config{Home: home}

	baseline, err := Message(workspace, config)
	if err != nil || baseline == nil {
		t.Fatalf("baseline = %#v, err=%v", baseline, err)
	}
	changes, ok := baseline.SourceChanges.([]Change)
	if !ok || len(changes) != 1 {
		t.Fatalf("baseline changes = %#v", baseline.SourceChanges)
	}
	state := &State{Identity: baseline.SourceBaselineIdentity, Changes: map[string]Change{changes[0].Scope: changes[0]}}

	write(t, filepath.Join(workspace, "pkg", "AGENTS.md"), "nested rules")
	write(t, filepath.Join(workspace, "pkg", "deep", "file.txt"), "touch")
	message, next, err := ReconcileTouch(workspace, config, state, []string{filepath.Join(workspace, "pkg", "deep", "file.txt")})
	if err != nil || message == nil || message.SourceBaseline {
		t.Fatalf("nested reconcile = %#v, %#v, %v", message, next, err)
	}
	updates, ok := message.SourceChanges.([]Change)
	if !ok || len(updates) != 1 || updates[0].Action != "set" ||
		updates[0].Path != filepath.ToSlash(filepath.Join("pkg", "AGENTS.md")) {
		t.Fatalf("nested source = %#v", message.SourceChanges)
	}
	if !strings.Contains(message.Text(), "Additional instructions from: pkg/AGENTS.md") ||
		!strings.Contains(message.Text(), "nested rules") {
		t.Fatalf("nested text = %q", message.Text())
	}
	if next == nil || len(next.Changes) != 2 {
		t.Fatalf("nested state = %#v", next)
	}

	if err := os.Remove(filepath.Join(workspace, "pkg", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	message, next, err = ReconcileTouch(workspace, config, next, nil)
	if err != nil || message == nil || message.SourceBaseline || len(next.Changes) != 1 {
		t.Fatalf("nested removal = %#v, %#v, %v", message, next, err)
	}
	removals, ok := message.SourceChanges.([]Change)
	if !ok || len(removals) != 1 || removals[0].Action != "remove" ||
		removals[0].Path != filepath.ToSlash(filepath.Join("pkg", "AGENTS.md")) {
		t.Fatalf("nested removal source = %#v", message.SourceChanges)
	}
	if strings.Contains(message.Text(), "nested rules") {
		t.Fatalf("removal still rendered instruction body: %q", message.Text())
	}
}

func TestUnavailableProbePreservesEntirePreviousCandidateGroup(t *testing.T) {
	resolved := Config{}.resolve()
	previous := &State{
		Identity: "identity",
		Changes: map[string]Change{
			CandidateScope("pkg", "AGENTS.md"): {
				Action: "set", Scope: CandidateScope("pkg", "AGENTS.md"), Path: "pkg/AGENTS.md", Digest: "a",
			},
			CandidateScope("pkg", "CLAUDE.md"): {
				Action: "set", Scope: CandidateScope("pkg", "CLAUDE.md"), Path: "pkg/CLAUDE.md", Digest: "b",
			},
			CandidateScope("other", "AGENTS.md"): {
				Action: "set", Scope: CandidateScope("other", "AGENTS.md"), Path: "other/AGENTS.md", Digest: "c",
			},
		},
	}
	unavailable := func(path string) bool {
		return strings.HasSuffix(filepath.ToSlash(path), "/pkg/CLAUDE.md")
	}
	preserved := unavailablePreviousGroups(previous, resolved, t.TempDir(), unavailable)
	if !preserved[CandidateScope("pkg", "AGENTS.md")] ||
		!preserved[CandidateScope("pkg", "CLAUDE.md")] ||
		preserved[CandidateScope("other", "AGENTS.md")] {
		t.Fatalf("preserved = %#v, want the full pkg group only", preserved)
	}
}

func TestBaselineIdentityTracksPrecedenceConfiguration(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cwd := filepath.Join(root, "packages", "app")
	write(t, filepath.Join(root, ".git"), "")
	write(t, filepath.Join(root, "AGENTS.md"), "root agents")
	write(t, filepath.Join(root, "CLAUDE.md"), "root claude")

	_, identity, err := LoadBaseline(cwd, Config{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ProjectRoot                    string   `json:"projectRoot"`
		InstructionFileCandidates      []string `json:"instructionFileCandidates"`
		LocalInstructionFileCandidates []string `json:"localInstructionFileCandidates"`
	}
	if err := json.Unmarshal([]byte(identity), &decoded); err != nil {
		t.Fatal(err)
	}
	if filepath.ToSlash(decoded.ProjectRoot) != filepath.ToSlash(filepath.Join("..", "..")) ||
		strings.Join(decoded.InstructionFileCandidates, "|") != "AGENTS.md|CLAUDE.md" ||
		strings.Join(decoded.LocalInstructionFileCandidates, "|") != "AGENTS.local.md|CLAUDE.local.md" {
		t.Fatalf("baseline identity = %s", identity)
	}

	defaultConfig := Config{Home: home}
	baseline, err := Message(cwd, defaultConfig)
	if err != nil || baseline == nil {
		t.Fatalf("baseline = %#v, err=%v", baseline, err)
	}
	changes, ok := baseline.SourceChanges.([]Change)
	if !ok || len(changes) != 2 {
		t.Fatalf("baseline changes = %#v", baseline.SourceChanges)
	}
	state := &State{Identity: baseline.SourceBaselineIdentity, Changes: changesByID(changes)}

	precedenceConfig := Config{
		Home:                      home,
		InstructionFileCandidates: []string{"AGENTS.md"},
	}
	resumed, next, err := Reconcile(cwd, precedenceConfig, state)
	if err != nil || resumed == nil || !resumed.SourceBaseline {
		t.Fatalf("precedence resume = %#v, %#v, %v", resumed, next, err)
	}
	if resumed.SourceBaselineIdentity == baseline.SourceBaselineIdentity {
		t.Fatal("candidate precedence change did not change baseline identity")
	}
	resumedChanges, ok := resumed.SourceChanges.([]Change)
	if !ok || len(resumedChanges) != 2 {
		t.Fatalf("precedence resume changes = %#v", resumed.SourceChanges)
	}
	if resumedChanges[0].Action != "remove" || resumedChanges[0].Path != "CLAUDE.md" ||
		resumedChanges[1].Action != "set" || resumedChanges[1].Path != "AGENTS.md" {
		t.Fatalf("precedence transitions = %#v", resumedChanges)
	}
	if again, _, err := Reconcile(cwd, precedenceConfig, next); err != nil || again != nil {
		t.Fatalf("stable precedence resume = %#v, err=%v", again, err)
	}
}

func TestRenderBaselineOmitsOlderFilesToFitBudget(t *testing.T) {
	files := []File{
		{DisplayPath: "AGENTS.md", Content: strings.Repeat("root ", 100)},
		{DisplayPath: "app/AGENTS.md", Content: "specific"},
	}
	text, represented := RenderBaseline(files, 384, false)
	if !strings.Contains(text, "omitted AGENTS.md") || !strings.Contains(text, "specific") ||
		len(represented) != 1 || represented[0].DisplayPath != "app/AGENTS.md" {
		t.Fatalf("rendered=%q represented=%#v", text, represented)
	}
}

func TestRenderChangeItemsCommitsOnlyRepresentedTransitions(t *testing.T) {
	set := changeRenderItem{
		change: Change{
			Action: "set", Scope: CandidateScope("pkg", "AGENTS.md"),
			Path: "pkg/AGENTS.md", Digest: "set-digest",
		},
		file: File{
			AbsolutePath: filepath.Join("pkg", "AGENTS.md"), DisplayPath: "pkg/AGENTS.md",
			Content: strings.Repeat("rule ", 120),
		},
	}
	remove := changeRenderItem{
		change: Change{
			Action: "remove", Scope: CandidateScope("pkg", "CLAUDE.md"),
			Path: "pkg/CLAUDE.md",
		},
		file: File{AbsolutePath: "removed:remove", DisplayPath: "pkg/CLAUDE.md"},
	}
	text, changes := renderChangeItems([]changeRenderItem{set, remove}, 320)
	if len(text) > 320 {
		t.Fatalf("rendered %d bytes, want <= 320: %q", len(text), text)
	}
	if len(changes) != 1 || changes[0].Action != "remove" || changes[0].Path != "pkg/CLAUDE.md" {
		t.Fatalf("represented changes = %#v, want only removable suffix", changes)
	}
	if !strings.Contains(text, "Instructions removed: pkg/CLAUDE.md") {
		t.Fatalf("rendered text = %q", text)
	}
	text, changes = renderChangeItems([]changeRenderItem{set}, 1)
	if len(changes) != 0 {
		t.Fatalf("one-byte render = %q, %#v, want no committed transition", text, changes)
	}
}

func TestSameDirectoryDuplicatesCollapseToEarliestCandidate(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git"), "")
	write(t, filepath.Join(root, "AGENTS.md"), "  same rule \n")
	write(t, filepath.Join(root, "CLAUDE.md"), "same rule")
	files, _, err := LoadBaseline(root, Config{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].DisplayPath != "AGENTS.md" {
		t.Fatalf("deduped files = %#v", files)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
