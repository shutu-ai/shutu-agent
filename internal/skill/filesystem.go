package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Root ranks (dispatch-m5d 发现优先级; ADR 决策 ④): lower ranks win same-name
// conflicts at the Registry. The filesystem provider emits candidates with
// these ranks; it never resolves same-name conflicts itself.
const (
	RankProjectDSH    = 100 // <projectRoot>/.dsh/skills
	RankProjectAgents = 200 // <projectRoot>/.agents/skills
	RankCustom        = 300 // FSOpts.Dirs
	RankUserDSH       = 400 // <userHome>/.dsh/skills
)

// FSOpts configures the default filesystem skill Provider (dispatch-m5d-1).
type FSOpts struct {
	// ProjectRoot is the directory to start the project-root search from
	// (normally the working directory). The provider walks upward to the
	// nearest ancestor containing .git and uses it as the project root; with
	// no such ancestor it uses ProjectRoot itself. Empty defaults to the
	// process working directory.
	ProjectRoot string
	// UserHome is the user's home directory whose .dsh/skills is the user-dsh
	// root. Empty defaults to os.UserHomeDir().
	UserHome string
	// Dirs are additional custom skill directories (source "custom", rank 300),
	// scanned in order.
	Dirs []string
}

// Filesystem is the default skill Provider (dispatch-m5d-1): it discovers
// skill bundles (<name>/SKILL.md) and flat files (<name>.md) directly under
// each configured root — never recursively. The skill name is the kebab-case
// entry name; the description comes from frontmatter `description` or the
// first body line. Skills are trusted local files that are read and returned
// as text, never executed.
type Filesystem struct {
	roots []fsRoot
}

// fsRoot is one scan root with its discovery metadata.
type fsRoot struct {
	path   string
	source string
	rank   int
}

// NewFilesystem builds the default filesystem skill Provider. Roots are
// resolved eagerly so List and Get only read skill files.
func NewFilesystem(opts FSOpts) (*Filesystem, error) {
	start := opts.ProjectRoot
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("skill: determine working directory: %w", err)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, fmt.Errorf("skill: resolve project root %q: %w", start, err)
	}
	projectRoot := findProjectRoot(abs)

	userHome := opts.UserHome
	if userHome == "" {
		userHome, _ = os.UserHomeDir()
	}
	if userHome == "" {
		userHome = "."
	}
	userHomeAbs, err := filepath.Abs(userHome)
	if err != nil {
		return nil, fmt.Errorf("skill: resolve user home %q: %w", userHome, err)
	}

	roots := []fsRoot{
		{path: filepath.Join(projectRoot, ".dsh", "skills"), source: SourceProjectDSH, rank: RankProjectDSH},
		{path: filepath.Join(projectRoot, ".agents", "skills"), source: SourceProjectAgents, rank: RankProjectAgents},
	}
	for _, dir := range opts.Dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("skill: resolve custom skill dir %q: %w", dir, err)
		}
		roots = append(roots, fsRoot{path: absDir, source: SourceCustom, rank: RankCustom})
	}
	roots = append(roots, fsRoot{path: filepath.Join(userHomeAbs, ".dsh", "skills"), source: SourceUserDSH, rank: RankUserDSH})

	return &Filesystem{roots: roots}, nil
}

// Name returns the provider name.
func (f *Filesystem) Name() string { return "filesystem" }

// List discovers candidates from every configured root in rank order. A
// missing root yields no candidates, not an error.
func (f *Filesystem) List(ctx context.Context) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Candidate
	for _, root := range f.roots {
		cands, err := f.scanRoot(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("skill: scan %s root %s: %w", root.source, root.path, err)
		}
		out = append(out, cands...)
	}
	return out, nil
}

// Get re-reads the candidate's file and returns its full definition. It
// returns (nil, nil) when the file disappeared or is no longer a skill.
func (f *Filesystem) Get(ctx context.Context, c Candidate) (*Definition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := parseSkillFile(c.Path, skillNameFromPath(c.Path))
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, nil
	}
	return &Definition{
		Name:           parsed.name,
		Description:    parsed.description,
		Content:        parsed.content,
		Source:         c.Source,
		Path:           c.Path,
		ModelInvocable: parsed.modelInvocable,
		UserInvocable:  parsed.userInvocable,
		Invocation: &InvocationPolicy{
			ModelInvocable: parsed.modelInvocable,
			UserInvocable:  parsed.userInvocable,
		},
	}, nil
}

// scanRoot reads one level of a root directory (never recursing) and returns a
// candidate per valid skill. A directory entry contributes only when it holds
// a SKILL.md; a file entry contributes only when it ends in .md.
func (f *Filesystem) scanRoot(ctx context.Context, root fsRoot) ([]Candidate, error) {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Deterministic local order (bundle and flat entries interleaved by name),
	// which the Registry uses to break equal-rank same-name ties.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var out []Candidate
	for _, entry := range entries {
		name := entry.Name()
		var path string
		switch {
		case entry.IsDir():
			path = filepath.Join(root.path, name, "SKILL.md")
		case strings.HasSuffix(name, ".md"):
			path = filepath.Join(root.path, name)
			name = strings.TrimSuffix(name, ".md")
		default:
			continue
		}
		parsed, err := parseSkillFile(path, name)
		if err != nil {
			return nil, fmt.Errorf("skill: read %s: %w", path, err)
		}
		if parsed == nil {
			continue
		}
		out = append(out, Candidate{
			Name:        parsed.name,
			Description: parsed.description,
			Source:      root.source,
			Rank:        root.rank,
			Path:        path,
			Invocation: &InvocationPolicy{
				ModelInvocable: parsed.modelInvocable,
				UserInvocable:  parsed.userInvocable,
			},
		})
	}
	return out, nil
}

// parsedSkill is the result of parsing one skill file.
type parsedSkill struct {
	name           string
	description    string
	content        string
	modelInvocable bool
	userInvocable  bool
}

// parseSkillFile reads and parses one skill file. It returns (nil, nil) when
// the file does not exist, the entry name is not a valid kebab-case skill
// name, or the file has no usable description — in every case the entry is
// simply not a skill.
func parseSkillFile(path, name string) (*parsedSkill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !IsSkillName(name) {
		return nil, nil
	}

	body := string(raw)
	front := ""
	if f, b, ok := splitFrontmatter(body); ok {
		front, body = f, b
	}
	meta := map[string]any{}
	if front != "" {
		// Invalid YAML frontmatter makes the file unparsable as a skill.
		if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
			return nil, nil
		}
		if meta == nil {
			meta = map[string]any{}
		}
	}

	content := strings.TrimSpace(body)
	description, ok := stringField(meta, "description")
	if !ok {
		description = firstBodyLine(content)
	}
	if description == "" {
		return nil, nil
	}

	modelInvocable := true
	if v, present := frontmatterBool(meta, "disable-model-invocation"); present && v {
		modelInvocable = false
	}
	userInvocable := true
	if v, present := frontmatterBool(meta, "user-invocable"); present && !v {
		userInvocable = false
	}

	return &parsedSkill{
		name:           name,
		description:    description,
		content:        content,
		modelInvocable: modelInvocable,
		userInvocable:  userInvocable,
	}, nil
}

// skillNameFromPath derives the kebab-case skill name from a skill file path:
// the bundle directory name for <name>/SKILL.md, or the file basename minus
// .md for a flat <name>.md file.
func skillNameFromPath(path string) string {
	if filepath.Base(path) == "SKILL.md" {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

// splitFrontmatter splits raw skill text into its YAML frontmatter block and
// body. The frontmatter must be a leading `---` line followed by a closing
// `---` line (CRLF tolerated); ok is false otherwise.
func splitFrontmatter(raw string) (front, body string, ok bool) {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// stringField returns the trimmed string value of key when it is a non-empty
// string.
func stringField(meta map[string]any, key string) (string, bool) {
	v, ok := meta[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// firstBodyLine returns the first non-empty line of body, trimmed.
func firstBodyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// frontmatterBool reads a boolean frontmatter field. It accepts YAML booleans
// and the common true/false spellings (true/yes/on/1, false/no/off/0). A
// present-but-unparseable value is treated as absent so the caller keeps its
// default.
func frontmatterBool(meta map[string]any, key string) (bool, bool) {
	v, ok := meta[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	case int:
		return b != 0, true
	case float64:
		return b != 0, true
	}
	return false, false
}

// findProjectRoot walks from start upward to the nearest ancestor containing a
// .git entry (directory or file); with no such ancestor it returns start.
func findProjectRoot(start string) string {
	cur := start
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return start
		}
		cur = parent
	}
}
