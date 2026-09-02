// tools.go — the M5d-2 Consumer half of the skill seam (design.md §8 Consumer /
// D2, dispatch-m5d-2 §3): skill is registered into the tools.Registry by
// the composition root (cmd/pa) when skill.enabled, and auto-whitelisted by
// config.applyDefaults the same way the job_*/subagent_* tools are. It
// implements the tools.Tool method set structurally (Go structural typing), so
// this package never imports the tools package — the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false, name pattern ^[a-z0-9]+(-[a-z0-9]+)*$) before this code runs; the
// kebab-case check is repeated here so a direct call can never bypass it.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 ① 实施说明 /
// dispatch-m5d-2 §3): skill emits skill/load on a successful load through
// the injected onEvent sink (the composition root wires it to the session log),
// and the loaded body reaches the model through the returned text, which the
// loop logs as tool/result. Every append happens inside a tool Execute — the
// serial main-loop path (D5). Skills are trusted local files loaded as model
// instruction text — never executed.
package skill

import (
	"context"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"

	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

// ToolName is the skill consumer tool name (whitelisted when skill.enabled;
// see config.skillToolNames).
const ToolName = "skill"

// SkillTools bundles the shared state of the skill consumer tools: the
// Registry service, the model-facing body bound, and the event sink.
type SkillTools struct {
	reg          Registry
	bodyMaxChars int
	onEvent      func(typ string, data any)
}

// NewSkillTools returns the shared skill-tool bundle bound to a skill
// Registry. bodyMaxChars bounds the returned skill body in runes (the
// composition root passes config.skill.body_max_chars; <= 0 means no bound).
// onEvent, when non-nil, receives the skill/* event payloads; the composition
// root wires it to the session log (D3).
func NewSkillTools(reg Registry, bodyMaxChars int, onEvent func(typ string, data any)) *SkillTools {
	return &SkillTools{reg: reg, bodyMaxChars: bodyMaxChars, onEvent: onEvent}
}

// Load returns the skill tool.
func (t *SkillTools) Load() SkillLoadTool { return SkillLoadTool{t: t} }

// emit forwards one skill/* event payload to the injected sink (D3).
func (t *SkillTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *SkillTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// SkillLoadTool loads one skill's full body for the model.
type SkillLoadTool struct {
	t *SkillTools
}

func (SkillLoadTool) Name() string { return ToolName }

func (SkillLoadTool) Description() string {
	return "Load the full instructions for an available skill. Call this with the exact skill name from the session skill catalog before acting on a task that names or clearly matches that skill."
}

func (SkillLoadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"pattern":     "^[a-z0-9]+(-[a-z0-9]+)*$",
				"description": "the kebab-case skill name from the available skills catalog",
			},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
}

func (t SkillLoadTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("skill: %w", err)
	}
	if !IsSkillName(a.Name) {
		return "", fmt.Errorf("skill: invalid skill name %q (kebab-case expected)", a.Name)
	}
	def, err := t.t.reg.Get(ctx, a.Name)
	if err != nil {
		return "", fmt.Errorf("skill: %w", err)
	}
	if def == nil {
		return "", fmt.Errorf("skill: unknown skill %q", a.Name)
	}
	if !DefinitionModelInvocable(def) {
		return "", fmt.Errorf("skill: %q is not available for model invocation", a.Name)
	}
	body := TruncateSkillBody(def.Content, t.t.bodyMaxChars)
	// skill/load is a log-only fact (D3); the body the model sees is bounded
	// to 200 runes in the payload by session.NewSkillLoad. The full returned
	// text is what the loop logs as tool/result.
	if err := t.t.emitContext(ctx, session.EventSkillLoad, session.NewSkillLoad(def.Name, def.Source, body)); err != nil {
		return "", fmt.Errorf("skill: persist event: %w", err)
	}
	return renderLoadedSkill(def, body), nil
}

// ExecuteResult returns DSH's canonical object value while retaining the
// rendered <skill_content> projection in Output for the model transcript.
func (t SkillLoadTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("skill: %w", err)
	}
	if !IsSkillName(a.Name) {
		return agenttools.ToolResult{}, fmt.Errorf("skill: invalid skill name %q (kebab-case expected)", a.Name)
	}
	def, err := t.t.reg.Get(ctx, a.Name)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("skill: %w", err)
	}
	if def == nil {
		return agenttools.ToolResult{}, fmt.Errorf("skill: unknown skill %q", a.Name)
	}
	if !DefinitionModelInvocable(def) {
		return agenttools.ToolResult{}, fmt.Errorf("skill: %q is not available for model invocation", a.Name)
	}
	body := TruncateSkillBody(def.Content, t.t.bodyMaxChars)
	value := map[string]any{
		"name":     def.Name,
		"provider": def.Provider,
		"content":  body,
	}
	if value["provider"] == "" {
		value["provider"] = def.Source
	}
	if base := resourceBaseValue(def.ResourceBase); base != nil {
		value["resourceBase"] = base
	}
	if err := t.t.emitContext(ctx, session.EventSkillLoad, session.NewSkillLoad(def.Name, def.Source, body)); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("skill: persist event: %w", err)
	}
	rendered := renderLoadedSkill(def, body)
	return agenttools.ToolResult{Value: value, Output: rendered}, nil
}

func resourceBaseValue(base *ResourceBase) map[string]any {
	if base == nil || base.Kind == "" {
		return nil
	}
	value := map[string]any{"kind": base.Kind}
	switch base.Kind {
	case "directory":
		value["path"] = base.Path
	case "url":
		value["url"] = base.URL
	case "opaque":
		value["description"] = base.Description
	default:
		return nil
	}
	return value
}

func renderLoadedSkill(def *Definition, body string) string {
	provider := def.Provider
	if provider == "" {
		provider = def.Source
	}
	return RenderSkillContentWithResource(def.Name, provider, def.ResourceBase, body)
}

// TruncateSkillBody shortens body to at most max runes, never splitting a
// UTF-8 sequence (Unicode 安全, dispatch-m5d-2 §3 正文有长度上限防超长注入).
// max <= 0 means no bound.
func TruncateSkillBody(body string, max int) string {
	if max <= 0 || len(body) == 0 {
		return body
	}
	runes := []rune(body)
	if len(runes) <= max {
		return body
	}
	return string(runes[:max])
}

// RenderSkillContent renders one loaded skill for the model as a
// <skill_content> block (mirrors dsh renderSkillContent, Go 裁剪): the name
// rides an attribute and the body is embedded verbatim under
// <skill_instructions>. The name is kebab-case-validated (IsSkillName), so it
// carries no character that needs escaping. Skills are trusted local content
// returned as instruction text — never executed.
func RenderSkillContent(name, body string) string {
	return RenderSkillContentWithResource(name, "", nil, body)
}

// RenderSkillContentWithResource mirrors DSH's resource-aware skill renderer.
func RenderSkillContentWithResource(name, provider string, base *ResourceBase, body string) string {
	resourceLines := []string{}
	if base == nil || base.Kind == "" {
		resourceLines = []string{"Resources for this skill are managed by provider \"" + escapeText(provider) + "\".", "Load referenced resources only as needed."}
	} else {
		switch base.Kind {
		case "directory":
			resourceLines = []string{"Base directory for this skill: " + escapeText(base.Path), "Resolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed."}
		case "url":
			resourceLines = []string{"Base URL for this skill: " + escapeText(base.URL), "Resolve relative URLs mentioned by this skill against the base URL before using them. Load referenced resources only as needed."}
		case "opaque":
			resourceLines = []string{"Resources for this skill: " + escapeText(base.Description), "Load referenced resources only as needed."}
		}
	}
	return "<skill_content name=\"" + escapeAttr(name) + "\">\n" +
		"<skill_resources>\n" + strings.Join(resourceLines, "\n") + "\n</skill_resources>\n\n" +
		"<skill_instructions>\n" +
		body +
		"\n</skill_instructions>\n" +
		"</skill_content>"
}

func escapeAttr(value string) string {
	return strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;").Replace(value)
}

func escapeText(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}
