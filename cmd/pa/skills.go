// skills.go — the M5d-2 composition-root orchestration (dispatch-m5d-2
// §4/§5). This is where the skill capability seam is wired into the REPL:
// registerSkills creates the filesystem provider + Registry and registers
// skill_load when skill.enabled (D10), wires the D3 event sink so skill/*
// events are appended to the active session log, and the "skill" pre-step
// catalog injector (registered after the compaction injector) makes every
// turn's first request carry the bounded skill catalog. The loop's turn/step
// structure is untouched (D4): the catalog injection runs through the existing
// PreStep extension point, the catalog is re-read by the composition root on
// every pre-step (no file watching, ADR 决策 ④), and every append happens on
// the serial pre-step/tool path (D5).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/loop"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/skill"
)

// registerSkills creates the filesystem provider + skill Registry and
// registers skill_load when skill.enabled, and wires the D3 event sink. When
// skill is disabled it creates nothing and registers nothing (D10, mirrors
// registerKB/registerJobs/registerSubagent/registerCompaction). The deferred
// Close in main.go releases the registry and its providers at shutdown.
func (a *app) registerSkills() error {
	if !config.Enabled(a.cfg.Skill.Enabled) {
		return nil
	}
	prov, err := skill.NewFilesystem(skill.FSOpts{
		// Empty roots fall back to the provider defaults (the working
		// directory for the project root, the user home for the user-dsh
		// root); the app-level overrides let tests pin deterministic roots.
		ProjectRoot: a.skillProjectRoot,
		UserHome:    a.skillUserHome,
		Dirs:        a.cfg.Skill.Dirs,
	})
	if err != nil {
		return err
	}
	reg := skill.NewRegistry()
	if err := reg.RegisterProvider(prov); err != nil {
		return fmt.Errorf("pa: register skill provider: %w", err)
	}
	a.skills = reg
	// D3 event sink: skill/* events are appended to the active session log.
	// The callback only ever runs inside a skill_load tool Execute or the
	// pre-step catalog injector — the serial main-loop path (D5). a.log is
	// read at call time, so a session switch (/new, /resume) is honored the
	// same way as the kb/jobs/subagent wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	load := skill.NewSkillTools(reg, a.cfg.Skill.BodyMaxChars, onEvent).Load()
	if err := a.reg.Register(load); err != nil {
		return fmt.Errorf("pa: register %s: %w", load.Name(), err)
	}
	return nil
}

// skillCatalogInjector builds the "skill" pre-step injector (ADR 决策 ④ /
// dispatch-m5d-2 §4): once per turn — after user/message is appended, before
// the first step's model request — it re-reads the skill catalog and injects
// the bounded catalog as a context message.
func (a *app) skillCatalogInjector() loop.PreStepInjector {
	return loop.PreStepInjector{
		Name:   "skill",
		Inject: a.skillCatalogPreStep,
	}
}

// skillInvocationInjector builds the dsh-compatible human skill invocation
// injector. A user-invocable skill referenced as a whitespace-bounded
// /skill-name token is loaded into the first model request; the original user
// text remains unchanged in session history.
func (a *app) skillInvocationInjector() loop.PreStepInjector {
	return loop.PreStepInjector{
		Name:   "skill-invocation",
		Inject: a.skillInvocationPreStep,
	}
}

// skillInvocationPreStep resolves user-invocable /skill-name tokens anywhere
// in the user message. It is deliberately fail-open: an unknown, disabled or
// unreadable skill leaves the literal user text untouched. Built-in Web
// commands win over a same-named skill, matching dsh command adjudication.
func (a *app) skillInvocationPreStep(ctx context.Context, userText string) []llm.Message {
	if a.skills == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var messages []llm.Message
	for _, token := range strings.Fields(userText) {
		token = strings.Trim(token, ",.;:!?()[]{}")
		if len(token) < 2 || token[0] != '/' {
			continue
		}
		name := token[1:]
		if !skill.IsSkillName(name) || isWebCommandName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		def, err := a.skills.Get(ctx, name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[skill invocation failed open]", err)
			continue
		}
		if def == nil || !def.UserInvocable {
			continue
		}
		body := skill.TruncateSkillBody(def.Content, a.cfg.Skill.BodyMaxChars)
		if _, err := a.log.Append(session.EventSkillLoad, session.NewSkillLoad(def.Name, def.Source, body)); err != nil {
			fmt.Fprintln(os.Stderr, "pa: skill/load event:", err)
		}
		messages = append(messages, llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.Text(skill.RenderSkillContent(def.Name, body))},
		})
	}
	return messages
}

// isUserSkillInvocation reports whether a leading slash line belongs to the
// skill plane rather than the Web command plane. It is used before Web's
// generic slash-command dispatch so /skill-name starts a normal model turn.
func (a *app) isUserSkillInvocation(ctx context.Context, text string) bool {
	if a.skills == nil {
		return false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 || len(fields[0]) < 2 || fields[0][0] != '/' {
		return false
	}
	name := strings.Trim(fields[0][1:], ",.;:!?()[]{}")
	if !skill.IsSkillName(name) || isWebCommandName(name) {
		return false
	}
	def, err := a.skills.Get(ctx, name)
	return err == nil && def != nil && def.UserInvocable
}

// skillCatalogPreStep is the "skill" pre-step injector body. It lists the
// current skill catalog (re-read every turn — no file watching), formats the
// sorted name + description list bounded to skill.catalog_max_chars, and
// returns the catalog as a context message. The skill/catalog observation
// event (D3) is appended ONLY when the catalog VERSION changed since the last
// turn (dsh digestCatalogEntries semantics: the catalog is injected once and
// updates are replacements) — so the UI shows the 上下文注入 row once per
// catalog, not every turn. A disabled registry, an empty catalog or any
// failure contributes no context (fail-open, the same contract as the kb
// recall injector); the loop's per-injector budget is a second, larger bound.
func (a *app) skillCatalogPreStep(ctx context.Context, _ string) []llm.Message {
	if a.skills == nil {
		return nil
	}
	cands, err := a.skills.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[skill catalog failed open]", err)
		return nil
	}
	if len(cands) == 0 {
		return nil
	}
	text := formatSkillCatalog(cands, a.cfg.Skill.CatalogMaxChars)
	if text == "" {
		return nil
	}
	version := skillCatalogVersion(cands)
	if version != a.skillCatalogVersion {
		a.skillCatalogVersion = version
		if _, err := a.log.Append(session.EventSkillCatalog, session.NewSkillCatalog(len(cands), version)); err != nil {
			fmt.Fprintln(os.Stderr, "pa: skill/catalog event:", err)
		}
	}
	return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(text)}}}
}

// formatSkillCatalog renders the sorted skill catalog as model-facing context:
// one "- <name>: <description>" line per skill (no bodies/paths/sources),
// bounded to maxChars runes (Unicode-safe; the loop's per-injector budget is a
// second, larger bound). maxChars <= 0 means no bound.
func formatSkillCatalog(cands []skill.Candidate, maxChars int) string {
	if len(cands) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range cands {
		fmt.Fprintf(&sb, "- %s: %s\n", c.Name, c.Description)
	}
	text := strings.TrimSuffix(sb.String(), "\n")
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars])
}

// skillCatalogVersion returns a stable digest over the sorted catalog (name +
// description) so skill/catalog events carry a version that changes exactly
// when the catalog content changes (mirrors dsh digestCatalogEntries; Go
// stdlib sha256 only, zero new dependencies). A truncated digest is ample for
// drift detection in a per-turn log payload.
func skillCatalogVersion(cands []skill.Candidate) string {
	var sb strings.Builder
	for _, c := range cands {
		sb.WriteString(c.Name)
		sb.WriteByte('\t')
		sb.WriteString(c.Description)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:8])
}
