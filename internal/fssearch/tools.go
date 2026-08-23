// tools.go — the dsh-aligned search tools (D-GAP-1): grep searches file
// contents under a directory tree, glob lists file paths matching a path
// pattern. Both implement the tools.Tool method set structurally (Go
// structural typing), so this package never imports the tools package — the
// seam stays decoupled (D2). The composition root (cmd/pa) registers them
// when fs_search.enabled, and config.applyDefaults auto-whitelists the names.
package fssearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Tool names — the dsh-aligned content/path search tools (whitelisted when
// fs_search.enabled; see config.fsSearchToolNames).
const (
	GrepToolName = "grep"
	GlobToolName = "glob"
)

// SearchFunc is the injectable search backend (production wires Search; tests
// substitute a fake to assert the output format without touching the disk).
type SearchFunc func(ctx context.Context, query string, opts Options) ([]Hit, error)

// GrepTool searches file contents under a directory tree (dsh grep). cwd is
// the default search root when the model omits path and the base for
// relative-path display; searchFn defaults to Search and is injectable for
// tests.
type GrepTool struct {
	cwd      string
	searchFn SearchFunc
}

// NewGrepTool returns the grep tool bound to the agent working directory
// (used as the default search root and the display base).
func NewGrepTool(cwd string) GrepTool {
	return GrepTool{cwd: cwd, searchFn: Search}
}

func (GrepTool) Name() string { return GrepToolName }

func (GrepTool) Description() string {
	return "search file contents under a directory tree (substring or regex); returns matching files and lines"
}

func (GrepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "root directory to search; defaults to the agent working directory",
			},
			"pattern": map[string]any{
				"type":        "string",
				"description": "substring (or regular expression when regex:true) to search for in file contents",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "optional glob restricting files by base name, e.g. \"*.go\" (filepath.Match)",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"description": "treat pattern as a regular expression (default false: plain substring)",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "cap on total hits; <=0 means the default 50",
			},
			"case_sensitive": map[string]any{
				"type":        "boolean",
				"description": "case-sensitive match (default false: case-insensitive)",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	}
}

// Execute runs a bounded file-content search and formats the hits as
// "path:line: text" lines (path shown relative to the agent cwd when possible)
// followed by the match count; a no-hit search reports the pattern and the
// root, and a cap-hit search (ErrLimit) keeps the partial result with a
// " (limit reached)" suffix. Read-only — never writes.
func (t GrepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path          string `json:"path"`
		Pattern       string `json:"pattern"`
		Glob          string `json:"glob"`
		Regex         bool   `json:"regex"`
		MaxResults    int    `json:"max_results"`
		CaseSensitive bool   `json:"case_sensitive"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("grep: empty pattern")
	}
	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = t.cwd
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("grep: no search path (pass path or configure the agent working directory)")
	}
	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = DefaultMaxResults
	}
	hits, err := t.searchFn(ctx, a.Pattern, Options{
		Path:          root,
		FilePattern:   a.Glob,
		Regex:         a.Regex,
		MaxResults:    maxResults,
		CaseSensitive: a.CaseSensitive,
	})
	if err != nil && !errors.Is(err, ErrLimit) {
		return "", fmt.Errorf("grep: %w", err)
	}
	out := formatHits(t, hits, a.Pattern, root)
	if errors.Is(err, ErrLimit) {
		out += " (limit reached)"
	}
	return out, nil
}

// formatHits renders hits as one "path:line: text" line each (path relative to
// the agent cwd when possible) followed by "N matches"; a no-hit search
// reports the pattern and the searched root.
func formatHits(t GrepTool, hits []Hit, pattern, root string) string {
	if len(hits) == 0 {
		return fmt.Sprintf("no matches for %q in %s", pattern, root)
	}
	var sb strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&sb, "%s:%d: %s\n", t.displayPath(h.Path), h.Line, h.Text)
	}
	fmt.Fprintf(&sb, "%d matches", len(hits))
	return sb.String()
}

// displayPath renders an absolute hit path relative to the agent cwd when it
// lies under it (more readable for the model); anything else stays absolute.
func (t GrepTool) displayPath(p string) string {
	if t.cwd == "" {
		return p
	}
	rel, err := filepath.Rel(t.cwd, p)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return p
}

// GlobTool lists file paths under a directory tree matching a path pattern
// (dsh glob). The pattern supports '*' (within one segment), '?' and '**'
// (any number of segments), e.g. "**/*.go" or "src/**". cwd is the default
// root when the model omits path.
type GlobTool struct {
	cwd string
}

// NewGlobTool returns the glob tool bound to the agent working directory.
func NewGlobTool(cwd string) GlobTool {
	return GlobTool{cwd: cwd}
}

func (GlobTool) Name() string { return GlobToolName }

func (GlobTool) Description() string {
	return "list file paths under a directory tree matching a path pattern ('*', '?', '**'); returns the matching paths"
}

func (GlobTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "path glob relative to the search root, e.g. \"**/*.go\" or \"src/**/*.md\"",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "root directory to search; defaults to the agent working directory",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "cap on returned paths; <=0 means the default 50",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	}
}

// Execute walks the tree (skipping VCS/dependency directories and binary
// files, bounded like grep) and returns the relative paths matching pattern,
// one per line, followed by the match count.
func (t GlobTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("glob: empty pattern")
	}
	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = t.cwd
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("glob: no search path (pass path or configure the agent working directory)")
	}
	re, err := globRegexp(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("glob: invalid pattern: %w", err)
	}
	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = DefaultMaxResults
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("glob: resolve %s: %w", root, err)
	}
	absRoot = filepath.Clean(absRoot)

	var matches []string
	walk := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if path != absRoot && ignoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			return nil
		}
		if re.MatchString(filepath.ToSlash(rel)) {
			matches = append(matches, filepath.ToSlash(rel))
			if len(matches) >= maxResults {
				return ErrLimit
			}
		}
		return nil
	}
	err = filepath.WalkDir(absRoot, walk)
	if err != nil && !errors.Is(err, ErrLimit) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no paths match %q in %s", a.Pattern, root), nil
	}
	out := strings.Join(matches, "\n") + fmt.Sprintf("\n%d matches", len(matches))
	if errors.Is(err, ErrLimit) {
		out += " (limit reached)"
	}
	return out, nil
}

// globRegexp compiles a path glob into an anchored regexp: '*' and '?' match
// within one path segment, '**' matches across segments (and "**/" also
// matches zero segments, so "**/*.go" includes root-level files). Slashes are
// the segment separator regardless of the host platform (paths are compared
// slash-normalized).
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					// "**/" matches zero or more segments.
					sb.WriteString("(?:.*/)?")
					i += 3
					continue
				}
				sb.WriteString(".*")
				i += 2
				continue
			}
			sb.WriteString("[^/]*")
		case '?':
			sb.WriteString("[^/]")
		case '/':
			sb.WriteString("/")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
		i++
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}
