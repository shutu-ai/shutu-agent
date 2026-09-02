// tools.go — the dsh-aligned search tools (D-GAP-1, 对齐 dsh tool-fs-search):
// grep searches file contents with a REGULAR EXPRESSION and returns matches
// grouped by file; glob lists file paths matching a path glob, sorted by
// modification time. The model-facing contract mirrors dsh call-for-call:
// grep takes pattern/path/include (pattern is always a regex, matched
// case-sensitively like ripgrep; include is ONE positive glob filter — no
// lists, no negation), glob takes pattern/path (a pattern with no "/" matches
// basenames at any depth), and the result text uses dsh's shapes ("Found N
// matches" + per-file "path\nLine N: text" blocks, "No matches found", "No
// files found"). Both implement the tools.Tool method set structurally (Go
// structural typing), so this package never imports the tools package — the
// seam stays decoupled (D2). The composition root (cmd/pa) registers them
// when fs_search.enabled, and config.applyDefaults auto-whitelists the names.
package fssearch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/pathsecure"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
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
	cwd             string
	cwdFn           func() string
	CwdContextFunc  func(context.Context) string
	RootContextFunc func(context.Context) string
	searchFn        SearchFunc
}

// NewGrepTool returns the grep tool bound to the agent working directory
// (used as the default search root and the display base).
func NewGrepTool(cwd string) GrepTool {
	return GrepTool{cwd: cwd, searchFn: Search}
}

func NewGrepToolForCWD(cwd func() string) GrepTool {
	return GrepTool{cwdFn: cwd, searchFn: Search}
}

func (GrepTool) Name() string { return GrepToolName }

// CancellationAware is explicit: Search checks both entry and walk context
// state, so a cancelled registry deadline stops the filesystem scan.
func (GrepTool) CancellationAware() bool { return true }

// ConcurrencySafe marks grep as a read-only filesystem query. dsh may run
// independent grep calls in parallel and commit their results in model order.
func (GrepTool) ConcurrencySafe(any) bool { return true }

func (GrepTool) Description() string {
	return "Search file contents with a regular expression. Returns matching lines with line numbers, grouped by file. " +
		"Returns the first 250 matches inline; a capped result reports the limit. " +
		"Use read on a matched file for surrounding context."
}

func (GrepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Regular expression to search for.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to search. Defaults to the agent working directory; a relative path resolves against it.",
			},
			"include": map[string]any{
				"type":        "string",
				"description": "One glob filter for which files to search (e.g. \"*.go\", \"*.{go,ts}\"). Not a list; negation is not supported.",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	}
}

// Execute runs a bounded regex search and formats the hits the dsh way: a
// "Found N matches" header, per-file "path\nLine N: text" sections, and — when
// a scan cap cut the result short — the could-not-save footer. A no-hit
// search reports "No matches found". Read-only — never writes.
func (t GrepTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("grep: pattern must be a non-empty string")
	}
	if a.Path != "" && strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("grep: path must be a non-empty string when given")
	}
	if a.Include != "" {
		if err := validateInclude(a.Include); err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
	}
	root := a.Path
	cwd := t.currentCWD(ctx)
	if strings.TrimSpace(root) == "" {
		root = cwd
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("grep: no search path (pass path or configure the agent working directory)")
	}
	var err error
	root, err = t.constrainPath(ctx, root, cwd)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	hits, err := t.searchFn(ctx, a.Pattern, Options{
		Path:       root,
		Include:    a.Include,
		MaxResults: DefaultMaxResults,
	})
	if err != nil && !errors.Is(err, ErrLimit) {
		return "", fmt.Errorf("grep: %w", err)
	}
	return formatGrepOutput(hits, func(path string) string { return displayRelative(cwd, path) }, errors.Is(err, ErrLimit)), nil
}

func (t GrepTool) currentCWD(ctx ...context.Context) string {
	if len(ctx) > 0 && ctx[0] != nil && t.CwdContextFunc != nil {
		return t.CwdContextFunc(ctx[0])
	}
	if t.cwdFn != nil {
		return t.cwdFn()
	}
	return t.cwd
}

func (t GrepTool) constrainPath(ctx context.Context, path, cwd string) (string, error) {
	if t.RootContextFunc == nil {
		return path, nil
	}
	return constrainSearchPath(path, cwd, t.RootContextFunc(ctx))
}

// validateInclude rejects an include that is not ONE positive glob filter
// (dsh validateInclude): blank strings, negated patterns, and comma-separated
// lists; a comma inside a brace group is fine — "*.{ts,tsx}" is one glob with
// alternation, not a list.
func validateInclude(include string) error {
	if strings.TrimSpace(include) == "" {
		return errors.New("include must be a non-empty glob when given")
	}
	if strings.HasPrefix(include, "!") {
		return errors.New(`include must be a positive glob filter; negated patterns ("!…") are not supported`)
	}
	depth := 0
	for _, r := range include {
		switch r {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return errors.New("include must be one glob, not a comma-separated list (use {a,b} alternation instead)")
			}
		}
	}
	return nil
}

// formatGrepOutput renders the model-facing result (dsh formatGrepOutput): a
// found-count header, the matches grouped by file, and — when capped — the
// could-not-save footer.
func formatGrepOutput(hits []Hit, display func(string) string, limited bool) string {
	if len(hits) == 0 {
		return "No matches found"
	}
	noun := "matches"
	if len(hits) == 1 {
		noun = "match"
	}
	header := fmt.Sprintf("Found %d %s", len(hits), noun)
	if limited {
		header += " (limit reached)"
	}
	body := formatGrepGrouped(hits, display)
	if !limited {
		return header + "\n\n" + body
	}
	return header + "\n\n" + body + "\n\n(The complete result could not be saved; narrow pattern, path, or include to see more.)"
}

// formatGrepGrouped groups flat hits by file (first-seen order) into the
// model-facing body: each file's display path, then one "Line N: <text>" row
// per match (dsh formatGrepMatches).
func formatGrepGrouped(hits []Hit, display func(string) string) string {
	byFile := map[string][]Hit{}
	var order []string
	for _, h := range hits {
		if _, ok := byFile[h.Path]; !ok {
			order = append(order, h.Path)
		}
		byFile[h.Path] = append(byFile[h.Path], h)
	}
	sections := make([]string, 0, len(order))
	for _, p := range order {
		lines := make([]string, 0, len(byFile[p]))
		for _, h := range byFile[p] {
			lines = append(lines, fmt.Sprintf("Line %d: %s", h.Line, h.Text))
		}
		sections = append(sections, display(p)+"\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// displayPath renders an absolute hit path relative to the agent cwd when it
// lies under it (more readable for the model; follow-up-readable by read);
// anything else stays absolute (dsh toWorkdirRelative).
func (t GrepTool) displayPath(p string) string {
	return displayRelative(t.currentCWD(), p)
}

func displayRelative(cwd, p string) string {
	if cwd == "" {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return p
}

// GlobTool lists file paths under a directory tree matching a path pattern
// (dsh glob), sorted by modification time (newest first). The pattern is a
// dsh-style path glob: "**/*.go" or "src/**" work as expected, and a pattern
// with no "/" matches basenames at any depth ("*" and "*.go" both search the
// whole tree). cwd is the default root when the model omits path and the
// display base.
type GlobTool struct {
	cwd             string
	cwdFn           func() string
	CwdContextFunc  func(context.Context) string
	RootContextFunc func(context.Context) string
}

// NewGlobTool returns the glob tool bound to the agent working directory.
func NewGlobTool(cwd string) GlobTool {
	return GlobTool{cwd: cwd}
}

func NewGlobToolForCWD(cwd func() string) GlobTool {
	return GlobTool{cwdFn: cwd}
}

func (GlobTool) Name() string { return GlobToolName }

// CancellationAware is explicit: the walk callback returns context errors.
func (GlobTool) CancellationAware() bool { return true }

// ConcurrencySafe marks glob as a read-only filesystem query.
func (GlobTool) ConcurrencySafe(any) bool { return true }

func (GlobTool) Description() string {
	return "Find files whose paths match a glob pattern. Returns matching file paths — never directories — including hidden files; VCS metadata directories are excluded."
}

func (GlobTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Glob pattern to match file paths against (e.g. \"**/*.go\", \"src/**/*.md\"). A pattern with no \"/\" matches the basename at any depth, so \"*\" and \"*.go\" both search the whole tree; include a separator to anchor the depth.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Root directory to search. Defaults to the agent working directory; a relative path resolves against it.",
			},
		},
		"required":             []string{"pattern"},
		"additionalProperties": false,
	}
}

// Execute walks the tree (skipping VCS/dependency directories and binary
// files are not glob concerns — paths are files only) and returns the
// matching paths, one per line, sorted by modification time (newest first).
// No matches reports "No files found"; an over-cap result shows the first
// page plus the dsh could-not-save footer.
func (t GlobTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("glob: pattern must be a non-empty string")
	}
	if a.Path != "" && strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("glob: path must be a non-empty string when given")
	}
	re, err := pathGlobRE(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("glob: invalid pattern: %w", err)
	}
	root := a.Path
	cwd := t.currentCWD(ctx)
	if strings.TrimSpace(root) == "" {
		root = cwd
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("glob: no search path (pass path or configure the agent working directory)")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("glob: resolve %s: %w", root, err)
	}
	absRoot = filepath.Clean(absRoot)
	if t.RootContextFunc != nil {
		var err error
		absRoot, err = constrainSearchPath(absRoot, cwd, t.RootContextFunc(ctx))
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
		absRoot = filepath.Clean(absRoot)
	}

	matches := []globEntry{}
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
		if !re.MatchString(filepath.ToSlash(rel)) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		matches = append(matches, globEntry{abs: path, rel: filepath.ToSlash(rel), modTime: info.ModTime()})
		return nil
	}
	if err := filepath.WalkDir(absRoot, walk); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if len(matches) == 0 {
		return "No files found", nil
	}
	// dsh --sort=modified: newest first.
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].modTime.After(matches[j].modTime) })

	seen := len(matches)
	page := matches
	if seen > DefaultGlobMaxResults {
		page = matches[:DefaultGlobMaxResults]
	}
	lines := make([]string, 0, len(page))
	for _, m := range page {
		lines = append(lines, t.displayPathWithContext(ctx, m))
	}
	body := strings.Join(lines, "\n")
	if seen <= DefaultGlobMaxResults {
		return body, nil
	}
	return fmt.Sprintf("%s\n\n(Showing %d of %d paths. The complete result could not be saved; narrow pattern or path to see more.)", body, len(page), seen), nil
}

// globEntry is one discovered file: the absolute path (display base), the
// root-relative slash path (pattern matching), and the modification time
// (sort key).
type globEntry struct {
	abs     string
	rel     string
	modTime time.Time
}

// displayPath renders a discovered path relative to the agent cwd when it
// lies under it (dsh prints workdir-relative paths); anything else stays
// relative to the search root.
func (t GlobTool) displayPath(m globEntry) string {
	return t.displayPathWithContext(nil, m)
}

func (t GlobTool) displayPathWithContext(ctx context.Context, m globEntry) string {
	cwd := t.currentCWD(ctx)
	if cwd == "" {
		return m.rel
	}
	rel, err := filepath.Rel(cwd, m.abs)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return m.rel
}

func (t GlobTool) currentCWD(ctx ...context.Context) string {
	if len(ctx) > 0 && ctx[0] != nil && t.CwdContextFunc != nil {
		return t.CwdContextFunc(ctx[0])
	}
	if t.cwdFn != nil {
		return t.cwdFn()
	}
	return t.cwd
}

// constrainSearchPath resolves an existing search path and requires its real
// path to remain under the injected workspace root. Search paths must exist:
// rejecting them before walking avoids a symlink or cwd ambiguity in the
// backend and gives both grep and glob the same host policy.
func constrainSearchPath(path, cwd, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return path, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if !filepath.IsAbs(root) {
		root, _ = filepath.Abs(root)
	}
	if !filepath.IsAbs(path) {
		path, _ = filepath.Abs(path)
	}
	rootReal, err := pathsecure.ResolveExisting(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("workspace root unavailable: %w", err)
	}
	targetReal, err := pathsecure.ResolveExisting(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("search path unavailable: %w", err)
	}
	if !withinSearchRoot(filepath.Clean(rootReal), filepath.Clean(targetReal)) {
		return "", fmt.Errorf("path %q escapes workspace root", path)
	}
	if info, err := os.Stat(targetReal); err != nil {
		return "", err
	} else if !info.IsDir() && info.Mode().IsRegular() == false {
		return "", fmt.Errorf("search path is not a regular file or directory")
	}
	return filepath.Clean(targetReal), nil
}

func withinSearchRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
