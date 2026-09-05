package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/skill"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

// skillsPolicy whitelists skill_load so the registry Execute gate can run it
// (in production config.applyDefaults + PolicyFromConfig do this).
func skillsPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{skill.ToolName},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// writeSkill writes one flat <name>.md skill (frontmatter description + body)
// under a root, creating parents.
func writeSkill(t *testing.T, root, name, desc, body string) {
	t.Helper()
	path := filepath.Join(root, name+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: "+desc+"\n---\n"+body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// skillFixture builds an app wired for skill tests: deterministic project/user
// roots in a temp dir (so discovery only sees the files the test writes), a
// fresh registry with the skill whitelist, and a fresh session log. It returns
// the app and the temp project root (for writing skills).
func skillFixture(t *testing.T, enabled bool) (*app, string) {
	t.Helper()
	root := t.TempDir()
	proj := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	st, err := store.OpenSQLite(filepath.Join(root, "skills.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateSession(context.Background(), "s-test", time.Now()); err != nil {
		t.Fatalf("create current session: %v", err)
	}
	if err := st.SetSessionCWD(context.Background(), "s-test", proj); err != nil {
		t.Fatalf("set current session cwd: %v", err)
	}
	a := &app{
		cfg: config.Config{
			Skill: config.SkillConfig{
				Enabled:             config.Bool(enabled),
				DescriptionMaxChars: 500,
				BodyMaxChars:        8000,
			},
		},
		reg:              tools.New(),
		log:              session.New(),
		currentID:        "s-test",
		store:            st,
		skillProjectRoot: proj,
		skillUserHome:    filepath.Join(root, "home"),
	}
	a.reg.SetPolicy(skillsPolicy())
	return a, proj
}

// persistCatalogMessages mirrors Loop.appendContextMessage for direct injector
// tests. DSH publication state lives on the durable user/message source, not in
// the adjacent skill/catalog observation event.
func persistCatalogMessages(t *testing.T, a *app, msgs []llm.Message) {
	t.Helper()
	for _, msg := range msgs {
		payload := session.NewContextMessageWithFormUpdate(
			msg.Text(), msg.Content, "skill-catalog", msg.SourcePlugin,
			msg.SourceForm, msg.SourceEntries, msg.SourceUpdate,
		)
		_, err := a.log.Append(session.EventUserMessage, payload)
		if err != nil {
			t.Fatalf("persist skill catalog: %v", err)
		}
	}
}

// registeredSkillNames returns the tool names currently registered in a.reg.
func registeredSkillNames(a *app) []string {
	specs := a.reg.Specs()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

// TestRegisterSkillsDisabledRegistersNothing verifies the D10 gate: with
// skill.enabled=false the composition root creates no registry, registers no
// skill_load tool, and wires no skill pre-step injector (dispatch-m5d-2 §5).
func TestRegisterSkillsDisabledRegistersNothing(t *testing.T) {
	a, _ := skillFixture(t, false)
	a.cfg.AgentInstructions.Enabled = config.Bool(false)
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	if a.skills != nil {
		t.Fatal("skill registry must be nil when skill.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if strings.HasPrefix(spec.Name, "skill_") {
			t.Fatalf("skill tool %q registered while skill disabled", spec.Name)
		}
	}
	if got := a.preStepInjectors(); len(got) != 0 {
		t.Fatalf("pre-step injectors = %+v, want none when skill disabled", got)
	}
}

// TestRegisterSkillsEnabledRegistersAndLoads verifies the enabled path: the
// provider + Registry are created, skill_load is registered (project-dsh and
// custom dirs discovered), D7 rejects bad arguments, a valid call loads the
// body and lands skill/load (D3 wiring), and an unknown skill errors.
func TestRegisterSkillsEnabledRegistersAndLoads(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "review-bash", "review bash scripts", "Always run: go vet ./...\n")
	custom := filepath.Join(t.TempDir(), "custom-skills")
	writeSkill(t, custom, "custom-tool", "custom tool", "custom body\n")
	a.cfg.Skill.Dirs = []string{custom}
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	if a.skills == nil {
		t.Fatal("skill registry must be created when skill.enabled=true")
	}
	names := registeredSkillNames(a)
	if !containsStr(names, skill.ToolName) {
		t.Fatalf("registered tools %v lack %s", names, skill.ToolName)
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, args := range []string{
		`{}`,
		`{"name":123}`,
		`{"name":"Bad Name"}`,
		`{"name":"has_underscore"}`,
		`{"name":"x","extra":1}`,
	} {
		if _, err := a.reg.Execute(context.Background(), skill.ToolName, json.RawMessage(args)); err == nil {
			t.Errorf("skill_load with args %s must be rejected (D7)", args)
		}
	}

	// A valid call loads the project-dsh skill and lands skill/load (D3).
	res, err := a.reg.Execute(context.Background(), skill.ToolName, json.RawMessage(`{"name":"review-bash"}`))
	if err != nil {
		t.Fatalf("skill_load via registry: %v", err)
	}
	if !strings.Contains(res.Output, "<skill_content") || !strings.Contains(res.Output, "go vet") {
		t.Fatalf("skill_load output = %q, want a <skill_content> block with the body", res.Output)
	}
	if !hasEvent(a.log, session.EventSkillLoad) {
		t.Fatal("skill/load event missing from the session log after skill_load")
	}

	// The custom-dir skill is loadable too (skill.dirs flows through).
	if _, err := a.reg.Execute(context.Background(), skill.ToolName, json.RawMessage(`{"name":"custom-tool"}`)); err != nil {
		t.Fatalf("skill_load custom-tool: %v", err)
	}

	// An unknown skill errors (tool-layer handling, not a panic).
	if res, err := a.reg.Execute(context.Background(), skill.ToolName, json.RawMessage(`{"name":"nope"}`)); err != nil || !res.IsError {
		t.Fatalf("skill_load of an unknown skill must return a structured error: result=%+v err=%v", res, err)
	}
}

// TestFormatSkillCatalogPerDescriptionBound covers the catalog formatter: one
// "- <name>: <description>" line per candidate (no bodies/paths/sources),
// each description bounded separately, and empty for no candidates.
func TestFormatSkillCatalogPerDescriptionBound(t *testing.T) {
	cands := []skill.Candidate{
		{Name: "alpha", Description: "alpha desc", Source: skill.SourceProjectDSH, Rank: skill.RankProjectDSH, Path: "/p/alpha.md"},
		{Name: "beta", Description: "beta desc", Source: skill.SourceUserDSH, Rank: skill.RankUserDSH, Path: "/u/beta.md"},
	}
	full := formatSkillCatalog(cands, 0, false)
	if !strings.Contains(full, "<available_skills>\n- `alpha`: alpha desc\n- `beta`: beta desc\n</available_skills>") {
		t.Fatalf("full catalog = %q, want dsh available-skills framing", full)
	}
	if strings.Contains(full, "alpha.md") || strings.Contains(full, "project-dsh") || strings.Contains(full, "beta desc\nbody") {
		t.Fatalf("catalog must not carry paths/sources/bodies: %q", full)
	}
	// Each description is bounded independently; neither entry is hidden.
	bounded := formatSkillCatalog(cands, 7, false)
	if !strings.Contains(bounded, "- `alpha`: alph...") || !strings.Contains(bounded, "- `beta`: beta...") {
		t.Fatalf("bounded catalog = %q, want both entries with ellipsized descriptions", bounded)
	}
	if formatSkillCatalog(nil, 0, false) != "" {
		t.Fatal("empty candidate list must render an empty catalog")
	}
}

func TestFormatSkillCatalogReplacement(t *testing.T) {
	cands := []skill.Candidate{
		{Name: "alpha", Description: "alpha desc"},
		{Name: "beta", Description: "beta desc"},
	}
	update := formatSkillCatalog(cands, 0, true)
	if !strings.Contains(update, "The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:\n\n<available_skills>") {
		t.Fatalf("replacement header = %q", update)
	}
	if !strings.Contains(update, "Use only names in this replacement catalog.") || !strings.Contains(update, "- `alpha`: alpha desc") {
		t.Fatalf("non-empty replacement = %q", update)
	}
	empty := formatSkillCatalog(nil, 0, true)
	if !strings.Contains(empty, "<available_skills>\n</available_skills>") ||
		!strings.Contains(empty, "No skills are currently available through the `skill` tool.") ||
		!strings.Contains(empty, "do not call the `skill` tool for it.") {
		t.Fatalf("empty replacement tombstone = %q", empty)
	}
}

func TestSkillCatalogEventVersionScansOneEventSnapshot(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventSkillCatalog, session.NewSkillCatalog(1, "old")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		if _, err := log.Append(session.EventAssistantChunk, session.NewAssistantChunk("opaque")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Append(session.EventSkillCatalog, session.NewSkillCatalog(2, "latest")); err != nil {
		t.Fatal(err)
	}
	if got := skillCatalogEventVersion(log); got != "latest" {
		t.Fatalf("skill catalog version = %q, want latest", got)
	}
}

// TestSkillCatalogVersionStable covers the catalog version: stable for the
// same catalog, different when the catalog content changes.
func TestSkillCatalogVersionStable(t *testing.T) {
	a := []skill.Candidate{{Name: "alpha", Description: "d"}, {Name: "beta", Description: "e"}}
	b := []skill.Candidate{{Name: "alpha", Description: "d"}, {Name: "beta", Description: "e"}}
	c := []skill.Candidate{{Name: "alpha", Description: "d"}, {Name: "beta", Description: "changed"}}
	if skillCatalogVersion(a, 500) != skillCatalogVersion(b, 500) {
		t.Fatal("version must be stable for the same catalog")
	}
	if skillCatalogVersion(a, 500) == skillCatalogVersion(c, 500) {
		t.Fatal("version must change when the catalog content changes")
	}
	if skillCatalogVersion(a, 500) == "" {
		t.Fatal("version must be non-empty")
	}
}

// TestSkillCatalogInjectorInjectsAndLogs verifies the "skill" pre-step
// injector: once per turn it re-reads the catalog, injects a user context
// message carrying only name + description (no body/path/source), and appends
// exactly one skill/catalog event with the entry count and a version (D3,
// serial pre-step path).
func TestSkillCatalogInjectorInjectsAndLogs(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", "alpha desc", "alpha secret body\n")
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "beta", "beta desc", "beta secret body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()

	inj := a.skillCatalogInjector()
	if inj.Name != "skill" {
		t.Fatalf("injector name = %q, want skill", inj.Name)
	}
	msgs := inj.Inject(context.Background(), "hello")
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("injected = %+v, want exactly one user catalog message", msgs)
	}
	content := msgs[0].Text()
	if !strings.Contains(content, "- `alpha`: alpha desc") || !strings.Contains(content, "- `beta`: beta desc") {
		t.Fatalf("catalog = %q, want sorted name+description lines", content)
	}
	if strings.Contains(content, "alpha secret body") || strings.Contains(content, "beta secret body") ||
		strings.Contains(content, "project-dsh") || strings.Contains(content, "SKILL") {
		t.Fatalf("catalog must not carry bodies/paths/sources: %q", content)
	}
	if msgs[0].SourceKind != "skill-catalog" || msgs[0].SourceForm != "catalog" || msgs[0].SourceUpdate {
		t.Fatalf("catalog source = %q/%q update=%v, want skill-catalog/catalog update=false", msgs[0].SourceKind, msgs[0].SourceForm, msgs[0].SourceUpdate)
	}
	entries, ok := msgs[0].SourceEntries.([]map[string]string)
	if !ok || len(entries) != 2 || entries[0]["name"] != "alpha" || entries[1]["name"] != "beta" {
		t.Fatalf("catalog source entries = %#v, want alpha then beta", msgs[0].SourceEntries)
	}
	if n := countEvent(a.log, session.EventSkillCatalog); n != 1 {
		t.Fatalf("skill/catalog count = %d, want exactly 1", n)
	}
	// The event carries the entry count and a non-empty version.
	var d struct {
		EntryCount int    `json:"entryCount"`
		Version    string `json:"version"`
	}
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventSkillCatalog {
			continue
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal skill/catalog: %v", err)
		}
	}
	if d.EntryCount != 2 || d.Version == "" {
		t.Fatalf("skill/catalog payload = %+v, want entryCount 2 + version", d)
	}
}

// TestSkillCatalogDescriptionsBoundedAndComplete verifies the DSH catalog
// contract: description_max_chars bounds one entry, but every model-invocable
// skill remains in the catalog.
func TestSkillCatalogDescriptionsBoundedAndComplete(t *testing.T) {
	a, proj := skillFixture(t, true)
	a.cfg.Skill.DescriptionMaxChars = 10
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", strings.Repeat("a", 20), "alpha body\n")
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "beta", strings.Repeat("b", 20), "beta body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	msgs := a.skillCatalogInjector().Inject(context.Background(), "hi")
	if len(msgs) != 1 {
		t.Fatalf("injected = %+v, want one message", msgs)
	}
	content := msgs[0].Text()
	if !strings.Contains(content, "- `alpha`: aaaaaaa...") || !strings.Contains(content, "- `beta`: bbbbbbb...") {
		t.Fatalf("catalog = %q, want every skill with an individually bounded description", content)
	}
}

// TestSkillCatalogInjectorEmptyNoEvent verifies the no-op path: an empty
// catalog injects no context and logs no skill/catalog event.
func TestSkillCatalogInjectorEmptyNoEvent(t *testing.T) {
	a, _ := skillFixture(t, true)
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	if msgs := a.skillCatalogInjector().Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("injected %+v for an empty catalog, want nil", msgs)
	}
	if n := countEvent(a.log, session.EventSkillCatalog); n != 0 {
		t.Fatalf("skill/catalog logged %d times for an empty catalog, want 0", n)
	}
}

// TestSkillCatalogEventDedupByVersion verifies DSH catalog lifecycle semantics:
// the first non-empty catalog is an initial publication, an unchanged visible
// catalog is not republished, and a changed catalog publishes a complete
// replacement with update provenance.
func TestSkillCatalogEventDedupByVersion(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", "alpha desc", "alpha body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()

	inj := a.skillCatalogInjector()
	msgs := inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 || msgs[0].SourceUpdate {
		t.Fatalf("initial injected %+v, want one initial catalog message", msgs)
	}
	persistCatalogMessages(t, a, msgs)
	if msgs := inj.Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("unchanged injected %+v, want nil", msgs)
	}
	if n := countEvent(a.log, session.EventSkillCatalog); n != 1 {
		t.Fatalf("skill/catalog events after an unchanged repeat = %d, want exactly 1", n)
	}

	// A catalog change (a second skill) fires the replacement event.
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "beta", "beta desc", "beta body\n")
	msgs = inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 || !msgs[0].SourceUpdate {
		t.Fatalf("after change injected %+v, want the catalog message", msgs)
	}
	persistCatalogMessages(t, a, msgs)
	if text := msgs[0].Text(); !strings.Contains(text, "The available skill catalog changed.") || !strings.Contains(text, "- `beta`: beta desc") {
		t.Fatalf("replacement text = %q, want a complete replacement catalog", text)
	}
	entries, ok := msgs[0].SourceEntries.([]map[string]string)
	if !ok || len(entries) != 2 {
		t.Fatalf("replacement entries = %#v, want both skills", msgs[0].SourceEntries)
	}
	if n := countEvent(a.log, session.EventSkillCatalog); n != 2 {
		t.Fatalf("skill/catalog events after a catalog change = %d, want exactly 2", n)
	}
	if msgs := inj.Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("post-change repeat injected %+v, want nil", msgs)
	}
	if n := countEvent(a.log, session.EventSkillCatalog); n != 2 {
		t.Fatalf("skill/catalog events after an unchanged repeat = %d, want still 2", n)
	}
}

// TestSkillCatalogReversionPublishesReplacement covers the failure mode of
// text-only deduplication: returning from B to A must publish a replacement
// even though the original A text remains earlier in durable history.
func TestSkillCatalogReversionPublishesReplacement(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", "alpha desc", "alpha body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	inj := a.skillCatalogInjector()
	msgs := inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 || msgs[0].SourceUpdate {
		t.Fatalf("initial injection = %+v, want one initial catalog", msgs)
	}
	persistCatalogMessages(t, a, msgs)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "beta", "beta desc", "beta body\n")
	bMsgs := inj.Inject(context.Background(), "hi")
	if len(bMsgs) != 1 || !bMsgs[0].SourceUpdate {
		t.Fatalf("B injection = %+v, want one replacement", bMsgs)
	}
	persistCatalogMessages(t, a, bMsgs)
	if err := os.Remove(filepath.Join(proj, ".dsh", "skills", "beta.md")); err != nil {
		t.Fatal(err)
	}
	msgs = inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 || !msgs[0].SourceUpdate {
		t.Fatalf("A reversion injection = %+v, want one replacement", msgs)
	}
	if text := msgs[0].Text(); strings.Contains(text, "`beta`") || !strings.Contains(text, "This complete catalog replaces") {
		t.Fatalf("A reversion text = %q, want complete A replacement", text)
	}
}

// TestSkillCatalogEmptyAfterPublication verifies the empty replacement
// tombstone: removing every model-invocable skill after publication must tell
// the model that the old names are no longer available.
func TestSkillCatalogEmptyAfterPublication(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", "alpha desc", "alpha body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	inj := a.skillCatalogInjector()
	msgs := inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 {
		t.Fatalf("initial injection = %+v, want one message", msgs)
	}
	persistCatalogMessages(t, a, msgs)
	if err := os.Remove(filepath.Join(proj, ".dsh", "skills", "alpha.md")); err != nil {
		t.Fatal(err)
	}
	msgs = inj.Inject(context.Background(), "hi")
	if len(msgs) != 1 || !msgs[0].SourceUpdate {
		t.Fatalf("empty replacement = %+v, want one tombstone message", msgs)
	}
	if entries, ok := msgs[0].SourceEntries.([]map[string]string); !ok || len(entries) != 0 {
		t.Fatalf("empty entries = %#v, want an empty array", msgs[0].SourceEntries)
	}
	if text := msgs[0].Text(); !strings.Contains(text, "No skills are currently available") {
		t.Fatalf("empty tombstone = %q", text)
	}
}

// TestSkillCatalogEventIsSessionScoped verifies that the same catalog is
// published again for a fresh session instead of inheriting an app-global
// digest from a previous session.
func TestSkillCatalogEventIsSessionScoped(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "alpha", "alpha desc", "alpha body\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()

	inj := a.skillCatalogInjector()
	if got := inj.Inject(context.Background(), "hi"); len(got) != 1 {
		t.Fatalf("first session injection = %+v, want one", got)
	}
	if countEvent(a.log, session.EventSkillCatalog) != 1 {
		t.Fatalf("first session catalog events = %d, want 1", countEvent(a.log, session.EventSkillCatalog))
	}

	a.log = session.New()
	if got := inj.Inject(context.Background(), "hi"); len(got) != 1 {
		t.Fatalf("fresh session injection = %+v, want one", got)
	}
	if countEvent(a.log, session.EventSkillCatalog) != 1 {
		t.Fatalf("fresh session catalog events = %d, want 1", countEvent(a.log, session.EventSkillCatalog))
	}
}

// TestSkillCatalogInjectorNilRegistryNoOp verifies the injector is inert when
// the registry is absent (the disabled guard, D10).
func TestSkillCatalogInjectorNilRegistryNoOp(t *testing.T) {
	a, _ := skillFixture(t, false)
	if msgs := a.skillCatalogInjector().Inject(context.Background(), "hi"); msgs != nil {
		t.Fatalf("injected %+v with a nil registry, want nil", msgs)
	}
	if got := len(a.log.Events()); got != 0 {
		t.Fatalf("log grew to %d events with a nil registry, want 0", got)
	}
}

func TestSkillCatalogAndInvocationFollowSessionCWD(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "startup-only", "startup skill", "startup body\n")
	other := filepath.Join(t.TempDir(), "workspace")
	writeSkill(t, filepath.Join(other, ".dsh", "skills"), "session-only", "session skill", "session body\n")
	if err := a.store.CreateSession(context.Background(), "session-other", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.setSessionCWD(context.Background(), "session-other", other); err != nil {
		t.Fatal(err)
	}
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()

	startupCtx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: a.currentID})
	otherCtx := runtimectx.With(context.Background(), runtimectx.Runtime{SessionID: "session-other"})
	names := func(ctx context.Context) []string {
		t.Helper()
		cands, err := a.skills.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out := make([]string, 0, len(cands))
		for _, cand := range cands {
			out = append(out, cand.Name)
		}
		return out
	}
	if got := names(startupCtx); len(got) != 1 || got[0] != "startup-only" {
		t.Fatalf("startup catalog = %#v, want startup-only", got)
	}
	if got := names(otherCtx); len(got) != 1 || got[0] != "session-only" {
		t.Fatalf("session catalog = %#v, want session-only", got)
	}
	def, err := a.skills.Get(otherCtx, "session-only")
	if err != nil || def == nil || !strings.Contains(def.Content, "session body") {
		t.Fatalf("Get session skill = %#v, %v", def, err)
	}
	if def, err := a.skills.Get(otherCtx, "startup-only"); err != nil || def != nil {
		t.Fatal("loaded startup skill with session-scoped registry")
	}

	inj := a.skillInvocationInjectorFor(session.New())
	msgs := inj.Inject(otherCtx, "/session-only explain")
	if len(msgs) != 1 || msgs[0].SourceName != "session-only" ||
		!strings.Contains(msgs[0].Text(), "session body") {
		t.Fatalf("session invocation = %+v, want session-only body", msgs)
	}
}

// TestLoopPreStepSkillCatalog is the end-to-end wiring test: a turn driven
// through app.newLoop() runs the "skill" pre-step injector before the first
// step's model request (catalog injected into the first request only), and the
// model calling skill_load lands skill/load + the <skill_content> body in
// tool/result (D3) — the turn/step structure is unchanged (D4).
func TestLoopPreStepSkillCatalog(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "review-bash", "review bash scripts", "Always run: go vet ./...\n")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	a.prompt = prompt.New("You are helpful.")
	model := &compactScriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: the model calls skill_load
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: skill.ToolName, Arguments: `{"name":"review-bash"}`},
			}},
		},
		{ // step 2: the model answers
			{Kind: llm.StreamTextDelta, Text: "done"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	a.llm = model

	lp := a.newLoop()
	captureStdout(func() {
		if err := lp.Run(context.Background(), "check my bash"); err != nil {
			t.Errorf("loop run: %v", err)
		}
	})
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2 (tool call then answer)", len(model.calls))
	}
	// The first request carries the catalog; the follow-up step does not.
	first := model.calls[0].Messages
	var catalogSeen bool
	for _, m := range first {
		if strings.Contains(m.Text(), "review-bash") && strings.Contains(m.Text(), "review bash scripts") {
			catalogSeen = true
		}
	}
	if !catalogSeen {
		t.Fatalf("first request = %+v, want the skill catalog injected", first)
	}
	// skill/catalog logged once; skill/load logged once; tool/result carries the
	// <skill_content> body (D3).
	if n := countEvent(a.log, session.EventSkillCatalog); n != 1 {
		t.Fatalf("skill/catalog count = %d, want exactly 1", n)
	}
	if n := countEvent(a.log, session.EventSkillLoad); n != 1 {
		t.Fatalf("skill/load count = %d, want exactly 1", n)
	}
	foundResult := false
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventToolResult {
			continue
		}
		var d struct {
			Name   string `json:"name"`
			Output string `json:"output"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Name == skill.ToolName &&
			strings.Contains(d.Output, "<skill_content") && strings.Contains(d.Output, "go vet") {
			foundResult = true
		}
	}
	if !foundResult {
		t.Fatal("tool/result with the <skill_content> body missing after skill_load in the loop")
	}
}

func TestWebMessageUserSkillRunsTurnAndInjectsBody(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "review-bash", "review bash scripts", "Always run shellcheck.")
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()
	a.prompt = prompt.New("You are helpful.")
	model := &compactScriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "done"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	a.llm = model

	if err := a.webMessage(context.Background(), a.currentID, "/review-bash inspect this", nil, webserver.PromptMeta{}); err != nil {
		t.Fatalf("webMessage: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("LLM calls = %d, want one normal model turn", len(model.calls))
	}
	probe := a.skillInvocationInjectorFor(session.New()).Inject(context.Background(), "/review-bash inspect this")
	if len(probe) != 1 || probe[0].SourceKind != "skill-invocation" ||
		probe[0].SourceName != "review-bash" || probe[0].SourceForm != "instructions" {
		t.Fatalf("skill invocation injector = %+v, want one DSH skill-invocation message", probe)
	}
	var bodySeen, originalSeen bool
	for _, msg := range model.calls[0].Messages {
		if strings.Contains(msg.Text(), "<skill_content name=\"review-bash\">") && strings.Contains(msg.Text(), "Always run shellcheck.") {
			bodySeen = true
		}
		if msg.Text() == "/review-bash inspect this" {
			originalSeen = true
		}
	}
	if !bodySeen || !originalSeen {
		t.Fatalf("first request = %+v, want skill body plus original user text", model.calls[0].Messages)
	}
	if n := countEvent(a.log, session.EventSkillLoad); n != 1 {
		t.Fatalf("skill/load events = %d, want one", n)
	}
	if history := a.log.DeriveHistory(); len(history) == 0 || history[0].Text() != "/review-bash inspect this" {
		t.Fatalf("history = %+v, want literal slash skill invocation", history)
	}
	var durableSource *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Form string `json:"form"`
	}
	for _, ev := range a.log.Events() {
		if ev.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Text    string `json:"text"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Source *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				Form string `json:"form"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		text := data.Text
		for _, block := range data.Content {
			text += block.Text
		}
		if strings.Contains(text, "<skill_content name=\"review-bash\">") {
			durableSource = data.Source
		}
	}
	if durableSource == nil || durableSource.Kind != "skill-invocation" ||
		durableSource.Name != "review-bash" || durableSource.Form != "instructions" {
		t.Fatalf("durable skill source = %+v, want skill-invocation/review-bash/instructions", durableSource)
	}
}
