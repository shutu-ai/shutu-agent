// tools.go — the M5d-2 Consumer half of the skill seam (design.md §8 Consumer /
// D2, dispatch-m5d-2 §3): skill_load is registered into the tools.Registry by
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
// dispatch-m5d-2 §3): skill_load emits skill/load on a successful load through
// the injected onEvent sink (the composition root wires it to the session log),
// and the loaded body reaches the model through the returned text, which the
// loop logs as tool/result. Every append happens inside a tool Execute — the
// serial main-loop path (D5). Skills are trusted local files loaded as model
// instruction text — never executed.
package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jabing/shutu-agent/internal/session"
)

// ToolName is the skill consumer tool name (whitelisted when skill.enabled;
// see config.skillToolNames).
const ToolName = "skill_load"

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

// Load returns the skill_load tool.
func (t *SkillTools) Load() SkillLoadTool { return SkillLoadTool{t: t} }

// emit forwards one skill/* event payload to the injected sink (D3).
func (t *SkillTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// SkillLoadTool loads one skill's full body for the model.
type SkillLoadTool struct {
	t *SkillTools
}

func (SkillLoadTool) Name() string { return ToolName }

func (SkillLoadTool) Description() string {
	return "load the full instructions of one available skill by name; the catalog of available skills is injected at the start of each turn"
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

func (t SkillLoadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("skill_load: %w", err)
	}
	if !IsSkillName(a.Name) {
		return "", fmt.Errorf("skill_load: invalid skill name %q (kebab-case expected)", a.Name)
	}
	def, err := t.t.reg.Get(ctx, a.Name)
	if err != nil {
		return "", fmt.Errorf("skill_load: %w", err)
	}
	if def == nil {
		return "", fmt.Errorf("skill_load: unknown skill %q", a.Name)
	}
	body := TruncateSkillBody(def.Content, t.t.bodyMaxChars)
	// skill/load is a log-only fact (D3); the body the model sees is bounded
	// to 200 runes in the payload by session.NewSkillLoad. The full returned
	// text is what the loop logs as tool/result.
	t.t.emit(session.EventSkillLoad, session.NewSkillLoad(def.Name, def.Source, body))
	return RenderSkillContent(def.Name, body), nil
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
	return "<skill_content name=\"" + name + "\">\n" +
		"<skill_instructions>\n" +
		body +
		"\n</skill_instructions>\n" +
		"</skill_content>"
}
