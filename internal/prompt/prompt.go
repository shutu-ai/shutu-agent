// Package prompt assembles the system prompt from ordered sections (design.md
// §7): persona → skills → an automatic tool catalog. Sections are
// loaded from files in a prompts directory, so sections can be added, removed,
// or re-ordered without touching the loop (dispatch-m2 §3).
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
)

// Section is one ordered block of the system prompt. Order places the section
// relative to its siblings; Name identifies it for Add/Remove.
type Section struct {
	Name  string
	Order int
	Text  string
}

// Builder renders a system prompt from its ordered sections, optionally
// appending an automatic tool catalog when a tool provider is installed.
type Builder struct {
	sections []Section
	tools    func() []llm.ToolSchema
	vars     map[string]string
}

// New returns a Builder with a single persona section. It exists for M1
// compatibility and tests; the REPL loads sections from the prompts directory.
func New(persona string) *Builder {
	return FromSections([]Section{{Name: "persona", Text: persona}})
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{}
}

// FromSections returns a Builder assembled from the given sections.
func FromSections(sections []Section) *Builder {
	return &Builder{sections: append([]Section(nil), sections...)}
}

// SetVariables installs the small prompt-template environment used by the
// DSH-compatible persona sections. Variables are rendered at Build time so a
// shared prompt definition can be reused by sessions with different models or
// working directories. Unknown placeholders are intentionally preserved.
func (b *Builder) SetVariables(vars map[string]string) *Builder {
	b.vars = make(map[string]string, len(vars))
	for key, value := range vars {
		b.vars[key] = value
	}
	return b
}

// Add appends a section, or replaces an existing section with the same Name.
func (b *Builder) Add(s Section) *Builder {
	for i := range b.sections {
		if b.sections[i].Name == s.Name {
			b.sections[i] = s
			return b
		}
	}
	b.sections = append(b.sections, s)
	return b
}

// Remove drops a section by Name (no-op when absent).
func (b *Builder) Remove(name string) *Builder {
	out := b.sections[:0]
	for _, s := range b.sections {
		if s.Name != name {
			out = append(out, s)
		}
	}
	b.sections = out
	return b
}

// SetTools installs a provider for the automatic tool catalog section. The
// provider is called on every Build, so the catalog always reflects the live
// registry without the loop knowing about tools (design.md §7).
func (b *Builder) SetTools(provider func() []llm.ToolSchema) *Builder {
	b.tools = provider
	return b
}

// Clone returns an independent builder with the same sections and tool
// provider. Per-session overlays can add sections without mutating the base.
func (b *Builder) Clone() *Builder {
	if b == nil {
		return NewBuilder()
	}
	vars := make(map[string]string, len(b.vars))
	for key, value := range b.vars {
		vars[key] = value
	}
	return &Builder{sections: append([]Section(nil), b.sections...), tools: b.tools, vars: vars}
}

// Build renders the system prompt: sections ordered by Order then Name, empty
// sections skipped, joined by blank lines, with the tool catalog appended last
// when a provider is installed and returns schemas.
func (b *Builder) Build() string {
	ordered := make([]Section, len(b.sections))
	copy(ordered, b.sections)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Order != ordered[j].Order {
			return ordered[i].Order < ordered[j].Order
		}
		return ordered[i].Name < ordered[j].Name
	})
	var parts []string
	for _, s := range ordered {
		if text := strings.TrimSpace(renderVariables(s.Text, b.vars)); text != "" {
			parts = append(parts, text)
		}
	}
	if b.tools != nil {
		if catalog := renderToolsCatalog(b.tools()); catalog != "" {
			parts = append(parts, catalog)
		}
	}
	return strings.Join(parts, "\n\n")
}

func renderVariables(text string, vars map[string]string) string {
	for key, value := range vars {
		text = strings.ReplaceAll(text, "{{"+key+"}}", value)
	}
	return text
}

// LoadDir reads prompt sections from dir. Each file named "NNN-name.md" (a
// numeric prefix followed by "-") becomes one section: NNN is its Order and
// "name" its section Name; the file body is its Text. Files without the
// numeric prefix are ignored, so documentation (e.g. README.md) can live in
// the same directory. A missing directory yields an empty Builder. Adding or
// removing a section is just adding or removing such a file — no code change.
func LoadDir(dir string) (*Builder, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return NewBuilder(), nil
		}
		return nil, fmt.Errorf("prompt: read dir %s: %w", dir, err)
	}
	var sections []Section
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		order, name, ok := parseSectionFile(e.Name())
		if !ok {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("prompt: read %s: %w", e.Name(), err)
		}
		sections = append(sections, Section{Name: name, Order: order, Text: string(body)})
	}
	sort.Slice(sections, func(i, j int) bool {
		if sections[i].Order != sections[j].Order {
			return sections[i].Order < sections[j].Order
		}
		return sections[i].Name < sections[j].Name
	})
	return FromSections(sections), nil
}

var sectionFileRe = regexp.MustCompile(`^(\d+)-([^/]+?)\.md$`)

// parseSectionFile splits "NNN-name.md" into (order, name, ok). Non-conforming
// filenames (e.g. "README.md") return ok=false.
func parseSectionFile(filename string) (order int, name string, ok bool) {
	m := sectionFileRe.FindStringSubmatch(filename)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", false
	}
	return n, m[2], true
}

func renderToolsCatalog(specs []llm.ToolSchema) string {
	if len(specs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Available tools:")
	for _, t := range specs {
		sb.WriteString("\n- ")
		sb.WriteString(t.Name)
		if t.Description != "" {
			sb.WriteString(": ")
			sb.WriteString(t.Description)
		}
	}
	return sb.String()
}
