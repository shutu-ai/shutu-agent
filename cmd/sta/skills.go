// skills.go — the M5d-2 composition-root orchestration (dispatch-m5d-2
// §4/§5). This is where the skill capability seam is wired into the REPL:
// registerSkills creates the filesystem provider + Registry and registers
// skill when skill.enabled (D10), wires the D3 event sink so skill/*
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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/skill"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

// sessionSkillRegistry resolves the filesystem skill registry from the runtime
// session identity. DSH addresses model-facing skill lookup by the live
// session's canonical cwd; a fixed process-startup registry would leak one Web
// workspace's project skills into another.
type sessionSkillRegistry struct {
	fallbackRoot string
	userHome     string
	dirs         []string

	mu     sync.Mutex
	closed bool
	byRoot map[string]skill.Registry
	cwdFor func(ctx context.Context) string
}

func newSessionSkillRegistry(fallbackRoot, userHome string, dirs []string, cwdFor func(ctx context.Context) string) *sessionSkillRegistry {
	return &sessionSkillRegistry{
		fallbackRoot: fallbackRoot,
		userHome:     userHome,
		dirs:         append([]string(nil), dirs...),
		byRoot:       map[string]skill.Registry{},
		cwdFor:       cwdFor,
	}
}

func (r *sessionSkillRegistry) resolveRoot(ctx context.Context) (string, error) {
	if r == nil {
		return "", errors.New("skill: registry is unavailable")
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return "", skill.ErrProviderClosed
	}
	root := filepath.Clean(r.fallbackRoot)
	if r.cwdFor != nil {
		root = filepath.Clean(r.cwdFor(ctx))
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("skill: resolve session root %q: %w", root, err)
	}
	return abs, nil
}

func (r *sessionSkillRegistry) registryFor(ctx context.Context) (skill.Registry, error) {
	root, err := r.resolveRoot(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, skill.ErrProviderClosed
	}
	if reg, ok := r.byRoot[root]; ok {
		r.mu.Unlock()
		return reg, nil
	}
	r.mu.Unlock()

	provider, err := skill.NewFilesystem(skill.FSOpts{
		ProjectRoot:  root,
		RootBoundary: root,
		UserHome:     r.userHome,
		Dirs:         r.dirs,
	})
	if err != nil {
		return nil, err
	}
	reg := skill.NewRegistry()
	if err := reg.RegisterProvider(provider); err != nil {
		_ = reg.Close()
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = reg.Close()
		return nil, skill.ErrProviderClosed
	}
	// A concurrent caller may have inserted the same canonical root while this
	// registry was being built; prefer that instance and release ours.
	if existing, ok := r.byRoot[root]; ok {
		r.mu.Unlock()
		_ = reg.Close()
		return existing, nil
	}
	r.byRoot[root] = reg
	r.mu.Unlock()
	return reg, nil
}

func (r *sessionSkillRegistry) List(ctx context.Context) ([]skill.Candidate, error) {
	reg, err := r.registryFor(ctx)
	if err != nil {
		return nil, err
	}
	return reg.List(ctx)
}

func (r *sessionSkillRegistry) Get(ctx context.Context, name string) (*skill.Definition, error) {
	reg, err := r.registryFor(ctx)
	if err != nil {
		return nil, err
	}
	return reg.Get(ctx, name)
}

func (r *sessionSkillRegistry) RegisterProvider(skill.Provider) error {
	return errors.New("skill: session registry providers are fixed at construction")
}

func (r *sessionSkillRegistry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	regs := make([]skill.Registry, 0, len(r.byRoot))
	for _, reg := range r.byRoot {
		regs = append(regs, reg)
	}
	r.byRoot = map[string]skill.Registry{}
	r.mu.Unlock()
	var first error
	for _, reg := range regs {
		if err := reg.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// registerSkills creates the session-scoped filesystem skill Registry and
// registers skill when skill.enabled, and wires the D3 event sink. When
// skill is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerSubagent/registerCompaction). The deferred
// Close in main.go releases every per-root registry at shutdown.
func (a *app) registerSkills() error {
	if !config.Enabled(a.cfg.Skill.Enabled) {
		return nil
	}
	// Empty roots fall back to the provider defaults (the working directory for
	// legacy/non-runtime calls, the user home for the user-dsh root); runtime
	// contexts resolve their own durable session CWD. The app-level overrides
	// let tests pin deterministic roots.
	reg := newSessionSkillRegistry(a.skillProjectRoot, a.skillUserHome, a.cfg.Skill.Dirs, func(ctx context.Context) string {
		sessionID := runtimectx.SessionID(ctx)
		// A durable app owns the session cwd. Legacy embedders/tests without a
		// store keep their current-session fixture/project root unchanged.
		if sessionID == "" || (a.store == nil && sessionID == a.currentID) {
			return a.skillProjectRoot
		}
		return a.sessionCWDFor(sessionID)
	})
	a.skills = reg
	// D3 event sink: skill/* events are appended to the active session log.
	// The callback only ever runs inside a skill tool Execute or the
	// pre-step catalog injector — the serial main-loop path (D5). a.log is
	// read at call time, so a session switch (/new, /resume) is honored the
	// same way as the other session-bound event wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", err)
		}
	}
	load := skill.NewSkillTools(reg, a.cfg.Skill.BodyMaxChars, onEvent).Load()
	if err := a.reg.Register(load); err != nil {
		return fmt.Errorf("sta: register %s: %w", load.Name(), err)
	}
	return nil
}

// skillCatalogInjector builds the "skill" pre-step injector (ADR 决策 ④ /
// dispatch-m5d-2 §4): it re-reads the skill catalog at every step and projects
// complete catalog as a durable context message; descriptions are bounded
// individually. Unchanged catalogs are deduplicated against the visible
// session surface.
func (a *app) skillCatalogInjector() loop.PreStepInjector {
	return loop.PreStepInjector{
		Name: "skill", Inject: a.skillCatalogPreStep, Unbounded: true,
	}
}

func (a *app) skillCatalogInjectorFor(log *session.Log) loop.PreStepInjector {
	return loop.PreStepInjector{
		Name:      "skill",
		Inject:    func(ctx context.Context, text string) []llm.Message { return a.skillCatalogPreStepFor(ctx, text, log) },
		Unbounded: true,
	}
}

// skillInvocationInjector builds the dsh-compatible human skill invocation
// injector. A user-invocable skill referenced as a whitespace-bounded
// /skill-name token is loaded into the first model request; the original user
// text remains unchanged in session history.
func (a *app) skillInvocationInjector() loop.PreStepInjector {
	return loop.PreStepInjector{
		Name: "skill-invocation", Inject: a.skillInvocationPreStep, OncePerTurn: true,
	}
}

func (a *app) skillInvocationInjectorFor(log *session.Log) loop.PreStepInjector {
	return loop.PreStepInjector{
		Name: "skill-invocation",
		Inject: func(ctx context.Context, text string) []llm.Message {
			return a.skillInvocationPreStepFor(ctx, text, log)
		},
		OncePerTurn: true,
	}
}

// skillInvocationPreStep resolves user-invocable /skill-name tokens anywhere
// in the user message. It is deliberately fail-open: an unknown, disabled or
// unreadable skill leaves the literal user text untouched. Built-in Web
// commands win over a same-named skill, matching dsh command adjudication.
func (a *app) skillInvocationPreStep(ctx context.Context, userText string) []llm.Message {
	return a.skillInvocationPreStepFor(ctx, userText, a.log)
}

func (a *app) skillInvocationPreStepFor(ctx context.Context, userText string, log *session.Log) []llm.Message {
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
		if def == nil || (def.Invocation != nil && !def.Invocation.UserInvocable) {
			continue
		}
		body := skill.TruncateSkillBody(def.Content, a.cfg.Skill.BodyMaxChars)
		if log == nil {
			continue
		}
		if _, err := log.Append(session.EventSkillLoad, session.NewSkillLoad(def.Name, def.Source, body)); err != nil {
			fmt.Fprintln(os.Stderr, "sta: skill/load event:", err)
		}
		messages = append(messages, llm.Message{
			Role:       llm.RoleUser,
			Content:    []llm.ContentBlock{llm.Text(skill.RenderSkillContent(def.Name, body))},
			SourceKind: "skill-invocation",
			SourceName: def.Name,
			SourceForm: "instructions",
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

// skillCatalogPreStep is the "skill" pre-step injector body. It publishes the
// current catalog once per visible catalog version and publishes a complete
// replacement after a change. Descriptions are individually bounded; the
// catalog list is not truncated. The skill/catalog observation event (D3)
// accompanies each durable context publication, so the UI shows one row per
// catalog version. A disabled registry or an initial empty catalog contributes
// no context (fail-open); the loop's per-injector budget is a second, larger
// bound.
func (a *app) skillCatalogPreStep(ctx context.Context, _ string) []llm.Message {
	return a.skillCatalogPreStepFor(ctx, "", a.log)
}

func (a *app) skillCatalogPreStepFor(ctx context.Context, _ string, log *session.Log) []llm.Message {
	if a.skills == nil {
		return nil
	}
	cands, err := a.skills.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[skill catalog failed open]", err)
		return nil
	}
	modelCands := make([]skill.Candidate, 0, len(cands))
	for _, c := range cands {
		if skill.CandidateModelInvocable(c) {
			modelCands = append(modelCands, c)
		}
	}
	// Catalog publication is session-scoped and follows the durable visible
	// catalog message, not a process-level digest. This matches DSH: a fresh
	// session publishes its initial catalog, an identical visible catalog is a
	// no-op, and a later A→B→A sequence publishes a replacement at each change.
	published, visibleVersion := skillCatalogHistory(log)
	version := skillCatalogVersion(modelCands, a.cfg.Skill.DescriptionMaxChars)
	if published && visibleVersion == version {
		return nil
	}
	isUpdate := published
	text := formatSkillCatalog(modelCands, a.cfg.Skill.DescriptionMaxChars, isUpdate)
	if text == "" {
		return nil
	}
	if log != nil {
		if _, err := log.Append(session.EventSkillCatalog, session.NewSkillCatalog(len(modelCands), version)); err != nil {
			fmt.Fprintln(os.Stderr, "sta: skill/catalog event:", err)
		}
	}
	return []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(text)},
		SourceKind: "skill-catalog", SourceForm: "catalog",
		SourceUpdate:  isUpdate,
		SourceEntries: skillCatalogEntries(modelCands, a.cfg.Skill.DescriptionMaxChars),
	}}
}

func skillCatalogEventVersion(log *session.Log) string {
	if log == nil {
		return ""
	}
	events := log.Events()
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type != session.EventSkillCatalog {
			continue
		}
		var data struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			return data.Version
		}
	}
	return ""
}

// skillCatalogHistory reports whether this session has published a durable
// skill catalog and, when one is visible, the digest of that catalog. It reads
// user/message source metadata rather than the adjacent skill/catalog observer
// event, so publication state cannot advance before the model-facing context is
// committed. Compaction replacements are honored by excluding their explicitly
// cited source events. Legacy source-less catalogs are recognized by framing and
// treated as an unknown prior version.
func skillCatalogHistory(log *session.Log) (published bool, visibleVersion string) {
	if log == nil {
		return false, ""
	}
	events := log.Events()
	shadowed := make(map[uint64]bool)
	for _, ev := range events {
		if ev.Type != session.EventUserMessage {
			continue
		}
		var replacement struct {
			SurfaceOp *struct {
				Op    string `json:"op"`
				Start uint64 `json:"start"`
				End   uint64 `json:"end"`
			} `json:"surfaceOp"`
			SourceEventSeqs []uint64 `json:"sourceEventSeqs"`
		}
		if json.Unmarshal(ev.Data, &replacement) != nil || replacement.SurfaceOp == nil {
			continue
		}
		if replacement.SurfaceOp.Op != "replace" {
			continue
		}
		if len(replacement.SourceEventSeqs) != 0 {
			for _, seq := range replacement.SourceEventSeqs {
				shadowed[seq] = true
			}
			continue
		}
		if replacement.SurfaceOp.Start != 0 || replacement.SurfaceOp.End != 0 {
			for seq := replacement.SurfaceOp.Start; seq <= replacement.SurfaceOp.End; seq++ {
				shadowed[seq] = true
			}
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Text   string `json:"text"`
			Source *struct {
				Kind    string `json:"kind"`
				Entries *[]struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"entries"`
			} `json:"source"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		isCatalog := data.Source != nil && data.Source.Kind == "skill-catalog"
		if !isCatalog && data.Source == nil && strings.HasPrefix(data.Text, "<system-reminder>\n") {
			// A legacy source-less catalog proves publication but does not carry
			// enough structured state to compare versions.
			published = true
			if !shadowed[ev.Seq] {
				return published, ""
			}
			continue
		}
		if !isCatalog {
			continue
		}
		published = true
		if shadowed[ev.Seq] {
			continue
		}
		if data.Source.Entries == nil {
			return published, ""
		}
		entries := make([]map[string]string, 0, len(*data.Source.Entries))
		for _, entry := range *data.Source.Entries {
			if entry.Name == "" {
				return published, ""
			}
			entries = append(entries, map[string]string{"name": entry.Name, "description": entry.Description})
		}
		return published, skillCatalogEntriesVersion(entries)
	}
	return published, ""
}

// skillCatalogEntriesVersion uses the same canonical projection as
// skillCatalogVersion so durable entries can be compared with the live catalog.
func skillCatalogEntriesVersion(entries []map[string]string) string {
	var sb strings.Builder
	for _, entry := range entries {
		encoded, err := json.Marshal([]string{entry["name"], entry["description"]})
		if err != nil {
			continue
		}
		sb.Write(encoded)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:8])
}

// formatSkillCatalog renders the complete sorted skill catalog as
// dsh-compatible model-facing context with <system-reminder>/<available_skills>
// framing and one "- `<name>`: <description>" line per skill (no
// bodies/paths/sources). maxChars bounds each normalized description, not the
// catalog: DSH publishes every entry, even when descriptions are truncated.
func formatSkillCatalog(cands []skill.Candidate, maxChars int, update bool) string {
	if len(cands) == 0 && !update {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	if update {
		sb.WriteString("The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:\n\n")
	} else {
		sb.WriteString("A skill is a reusable set of task-specific instructions. The following skills are available in this session:\n\n")
	}
	sb.WriteString("<available_skills>\n")
	descriptionMax := maxChars
	if descriptionMax > 0 && descriptionMax < 3 {
		// Keep the ellipsis contract well-defined; DSH's configuration
		// validator also requires at least this minimum.
		descriptionMax = 3
	}
	for _, c := range cands {
		fmt.Fprintf(&sb, "- `%s`: %s\n", c.Name,
			escapeSkillCatalogText(catalogDescription(c.Description, descriptionMax)))
	}
	sb.WriteString("</available_skills>\n\n")
	if update {
		if len(cands) == 0 {
			sb.WriteString("No skills are currently available through the `skill` tool. Do not use names from earlier skill catalogs.\n")
			sb.WriteString("A user may still invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool for it.\n")
		} else {
			sb.WriteString("Use only names in this replacement catalog. If the user names a listed skill, or the task clearly matches its description, call the `skill` tool with the exact name before acting.\n")
			sb.WriteString("A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.\n")
		}
	} else {
		sb.WriteString("If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. Load all applicable skills, then follow their full instructions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.\n")
		sb.WriteString("A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.\n")
	}
	sb.WriteString("</system-reminder>")
	return sb.String()
}

func escapeSkillCatalogText(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, ">", "&gt;")
}

// catalogDescription mirrors DSH's per-entry contract: normalize internal
// whitespace, then bound the description with an ellipsis. maxLength governs
// one description only; the catalog remains complete.
func catalogDescription(value string, maxLength int) string {
	normalized := strings.Join(strings.Fields(value), " ")
	runes := []rune(normalized)
	if maxLength <= 0 || len(runes) <= maxLength {
		return normalized
	}
	return string(runes[:maxLength-3]) + "..."
}

// skillCatalogVersion returns a stable digest over the sorted catalog (name +
// description) so skill/catalog events carry a version that changes exactly
// when the catalog content changes (mirrors dsh digestCatalogEntries; Go
// stdlib sha256 only, zero new dependencies). A truncated digest is ample for
// drift detection in a per-turn log payload.
func skillCatalogVersion(cands []skill.Candidate, descriptionMaxChars int) string {
	return skillCatalogEntriesVersion(skillCatalogEntries(cands, descriptionMaxChars))
}

// skillCatalogEntries projects the exact name/description pairs published to
// the model. DSH stores this list beside the catalog prose so presentation can
// render the durable fact without re-parsing model-facing markup.
func skillCatalogEntries(cands []skill.Candidate, maxChars int) []map[string]string {
	descriptionMax := maxChars
	if descriptionMax > 0 && descriptionMax < 3 {
		descriptionMax = 3
	}
	entries := make([]map[string]string, 0, len(cands))
	for _, c := range cands {
		entries = append(entries, map[string]string{
			"name":        c.Name,
			"description": catalogDescription(c.Description, descriptionMax),
		})
	}
	return entries
}

// nativeSkillCatalog implements DSH's session-addressed read-only skill view.
// Each call resolves the session cwd into a fresh layered filesystem view; this
// avoids leaking one Web workspace's project skills into another while still
// sharing the user and configured global roots.
func (a *app) nativeSkillCatalog(ctx context.Context, cwd string) ([]webserver.SkillCatalogEntry, error) {
	if a.skills == nil {
		return nil, errors.New("skill registry is absent: skill capability is disabled")
	}
	provider, err := skill.NewFilesystem(skill.FSOpts{
		ProjectRoot:  cwd,
		RootBoundary: cwd,
		UserHome:     a.skillUserHome,
		Dirs:         a.cfg.Skill.Dirs,
	})
	if err != nil {
		return nil, err
	}
	registry := skill.NewRegistry()
	if err := registry.RegisterProvider(provider); err != nil {
		_ = registry.Close()
		return nil, err
	}
	candidates, err := registry.List(ctx)
	closeErr := registry.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	entries := make([]webserver.SkillCatalogEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Invocation != nil && !candidate.Invocation.UserInvocable {
			continue
		}
		entries = append(entries, webserver.SkillCatalogEntry{
			Name:           candidate.Name,
			Description:    candidate.Description,
			WhenToUse:      candidate.WhenToUse,
			ModelInvocable: skill.CandidateModelInvocable(candidate),
		})
	}
	return entries, nil
}
