package skill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// skillPolicy whitelists skill_load so the tools.Registry Execute gate can run
// it (in production config.applyDefaults + PolicyFromConfig do this).
func skillPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{ToolName},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// newLoadedRegistry builds a skill Registry with one registered fake provider
// exposing the given candidate and its loaded definition.
func newLoadedRegistry(t *testing.T, cand Candidate, def *Definition) Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterProvider(&fakeProvider{
		name:  "fake",
		cands: []Candidate{cand},
		defs:  map[string]*Definition{cand.Name: def},
	}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	return reg
}

// TestSkillLoadSchemaValidation covers the D7 gate: the skill_load schema
// (additionalProperties: false, kebab-case name pattern) rejects bad
// model-generated arguments before any tool code runs.
func TestSkillLoadSchemaValidation(t *testing.T) {
	reg := tools.New()
	reg.SetPolicy(skillPolicy())
	st := NewSkillTools(NewRegistry(), 0, nil)
	if err := reg.Register(st.Load()); err != nil {
		t.Fatalf("register skill_load: %v", err)
	}
	for _, args := range []string{
		`{}`,                          // missing required name
		`{"name":123}`,                // name must be a string
		`{"name":"Not-Kebab"}`,        // uppercase breaks the kebab-case pattern
		`{"name":"has_underscore"}`,   // underscore breaks the pattern
		`{"name":"a b"}`,              // space breaks the pattern
		`{"name":"review","extra":1}`, // additional properties rejected
	} {
		if _, err := reg.Execute(context.Background(), ToolName, json.RawMessage(args)); err == nil {
			t.Errorf("skill_load with args %s must be rejected (D7)", args)
		}
	}
}

// TestSkillLoadLoadsAndEmits covers the happy path: a valid call returns the
// body wrapped in <skill_content> (bounded by body_max_chars, Unicode-safe),
// and the skill/load event lands through the onEvent sink with the skill name,
// source and a bounded body summary (D3, serial tool path).
func TestSkillLoadLoadsAndEmits(t *testing.T) {
	body := "# Review Bash\n\nAlways run: go vet ./...\n"
	reg := NewRegistry()
	if err := reg.RegisterProvider(&fakeProvider{
		name:  "fake",
		cands: []Candidate{{Name: "review-bash", Description: "review bash scripts", Source: SourceProjectDSH, Rank: RankProjectDSH}},
		defs: map[string]*Definition{
			"review-bash": {Name: "review-bash", Description: "review bash scripts", Content: body, Source: SourceProjectDSH, Path: "/p/review-bash.md", ModelInvocable: true, UserInvocable: true},
		},
	}); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	var events []struct {
		typ  string
		data any
	}
	st := NewSkillTools(reg, 0, func(typ string, data any) {
		events = append(events, struct {
			typ  string
			data any
		}{typ, data})
	})

	treg := tools.New()
	treg.SetPolicy(skillPolicy())
	if err := treg.Register(st.Load()); err != nil {
		t.Fatalf("register skill_load: %v", err)
	}
	res, err := treg.Execute(context.Background(), ToolName, json.RawMessage(`{"name":"review-bash"}`))
	if err != nil {
		t.Fatalf("skill_load: %v", err)
	}
	// The model-facing result is the <skill_content>-wrapped body (which the
	// loop logs as tool/result, D3).
	if !strings.HasPrefix(res.Output, "<skill_content name=\"review-bash\">") ||
		!strings.Contains(res.Output, "<skill_instructions>\n"+body+"\n</skill_instructions>") {
		t.Fatalf("output = %q, want a <skill_content> block with the body", res.Output)
	}
	// Exactly one skill/load event, with name/source and a bounded summary.
	if len(events) != 1 || events[0].typ != session.EventSkillLoad {
		t.Fatalf("events = %+v, want exactly one skill/load", events)
	}
	var d struct {
		Name    string `json:"name"`
		Source  string `json:"source"`
		Summary string `json:"summary"`
	}
	raw, err := json.Marshal(events[0].data)
	if err != nil {
		t.Fatalf("marshal event data: %v", err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if d.Name != "review-bash" || d.Source != SourceProjectDSH {
		t.Fatalf("skill/load payload = %+v, want name review-bash source project-dsh", d)
	}
	if !strings.Contains(d.Summary, "go vet") {
		t.Fatalf("skill/load summary = %q, want it to carry the body head", d.Summary)
	}
}

// TestSkillLoadBodyBoundAndUnicodeSafe covers the body_max_chars bound: the
// returned body is truncated to the bound runes and never splits a UTF-8
// sequence.
func TestSkillLoadBodyBoundAndUnicodeSafe(t *testing.T) {
	body := "步骤一。步骤二。步骤三。步骤四。" // 4 CJK runes each, 16 runes total
	reg := newLoadedRegistry(t,
		Candidate{Name: "steps", Description: "steps skill", Source: SourceUserDSH, Rank: RankUserDSH},
		&Definition{Name: "steps", Description: "steps skill", Content: body, Source: SourceUserDSH},
	)
	st := NewSkillTools(reg, 6, nil) // bound of 6 runes lands mid-CJK-rune
	treg := tools.New()
	treg.SetPolicy(skillPolicy())
	if err := treg.Register(st.Load()); err != nil {
		t.Fatalf("register skill_load: %v", err)
	}
	res, err := treg.Execute(context.Background(), ToolName, json.RawMessage(`{"name":"steps"}`))
	if err != nil {
		t.Fatalf("skill_load: %v", err)
	}
	if !utf8.ValidString(res.Output) {
		t.Fatal("output is not valid UTF-8 (a sequence was split)")
	}
	// The embedded body is at most the bound runes.
	inner := res.Output[strings.Index(res.Output, "<skill_instructions>\n")+len("<skill_instructions>\n") : strings.Index(res.Output, "\n</skill_instructions>")]
	if got := utf8.RuneCountInString(inner); got != 6 {
		t.Fatalf("embedded body runes = %d, want 6 (bounded)", got)
	}
	if inner != "步骤一。步骤" {
		t.Fatalf("embedded body = %q, want the first 6 runes intact", inner)
	}
}

// TestSkillLoadNoBodyBoundWhenZero covers body_max_chars <= 0 meaning no bound:
// the full body is returned.
func TestSkillLoadNoBodyBoundWhenZero(t *testing.T) {
	reg := newLoadedRegistry(t,
		Candidate{Name: "full", Description: "full skill", Source: SourceUserDSH, Rank: RankUserDSH},
		&Definition{Name: "full", Description: "full skill", Content: strings.Repeat("x", 20000), Source: SourceUserDSH},
	)
	st := NewSkillTools(reg, 0, nil)
	out, err := st.Load().Execute(context.Background(), json.RawMessage(`{"name":"full"}`))
	if err != nil {
		t.Fatalf("skill_load: %v", err)
	}
	if !strings.Contains(out, strings.Repeat("x", 20000)) {
		t.Fatal("body must not be truncated when body_max_chars is 0")
	}
}

// TestSkillLoadUnknownSkill covers the unknown-skill path: a valid kebab-case
// name with no candidate returns an error message (tool-layer handling, not a
// panic), and no skill/load event is emitted.
func TestSkillLoadUnknownSkill(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterProvider(&fakeProvider{name: "fake"}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	var events []string
	st := NewSkillTools(reg, 0, func(typ string, data any) { events = append(events, typ) })
	_, err := st.Load().Execute(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("unknown skill err = %v, want an unknown-skill message", err)
	}
	if len(events) != 0 {
		t.Fatalf("skill/load emitted for an unknown skill: %+v", events)
	}
}

// TestSkillLoadRegistryGetErrorPropagates covers a provider Get failure being
// surfaced as a tool error rather than a panic.
func TestSkillLoadRegistryGetErrorPropagates(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterProvider(&fakeProvider{
		name:   "bad",
		cands:  []Candidate{{Name: "foo", Description: "d", Rank: RankCustom}},
		getErr: errors.New("boom"),
	}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	st := NewSkillTools(reg, 0, nil)
	if _, err := st.Load().Execute(context.Background(), json.RawMessage(`{"name":"foo"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("get error = %v, want the provider error surfaced", err)
	}
}
