// tools_test.go — the dsh-aligned grep/glob tool tests (docs/dispatch-gap-1.md
// §3, 对齐 dsh tool-fs-search). A fake searchFn substitutes the engine so the
// Execute output format is asserted without touching the disk: the dsh result
// shapes ("Found N matches" header, per-file "path\nLine N: text" sections,
// "No matches found", the could-not-save footer), argument mapping, and the
// dsh validation messages. Glob tests exercise the real matcher over a temp
// tree: dsh pattern semantics (no "/" → basename at any depth), modification
// time ordering, the page footer, and "No files found".
package fssearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSearcher records the pattern and Options it was handed and returns
// canned hits/error, so Execute's argument mapping and formatting are
// observable.
type fakeSearcher struct {
	fn      SearchFunc
	pattern string
	opts    Options
}

func (f *fakeSearcher) call(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
	f.pattern = pattern
	f.opts = opts
	return f.fn(ctx, pattern, opts)
}

// newFakeTool returns a GrepTool whose searchFn records calls and returns the
// given fn result.
func newFakeTool(cwd string, fn SearchFunc) (*GrepTool, *fakeSearcher) {
	f := &fakeSearcher{fn: fn}
	return &GrepTool{cwd: cwd, searchFn: f.call}, f
}

// TestSearchCancellationClassification pins explicit cooperative-cancellation
// metadata for the bounded filesystem scans.
func TestSearchCancellationClassification(t *testing.T) {
	for _, tool := range []any{NewGrepTool("."), NewGlobTool(".")} {
		classified, ok := tool.(interface{ CancellationAware() bool })
		if !ok || !classified.CancellationAware() {
			t.Fatalf("%T must explicitly classify cooperative cancellation", tool)
		}
	}
}

// TestGrepExecuteFormatsHits asserts the dsh result shape: a "Found N matches"
// header, per-file "path\nLine N: text" sections (paths relative to cwd,
// slash-normalized), and the defaulted root mapping.
func TestGrepExecuteFormatsHits(t *testing.T) {
	cwd := `C:\work`
	pathA := `C:\work\a.txt`
	pathB := `C:\work\sub\b.go`
	if runtime.GOOS != "windows" {
		cwd = "/work"
		pathA = "/work/a.txt"
		pathB = "/work/sub/b.go"
	}
	tool, f := newFakeTool(cwd, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return []Hit{
			{Path: pathA, Line: 2, Text: "needle one"},
			{Path: pathB, Line: 5, Text: "needle two"},
		}, nil
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "Found 2 matches\n\na.txt\nLine 2: needle one\n\n" + filepath.ToSlash(filepath.Join("sub", "b.go")) + "\nLine 5: needle two"
	if out != want {
		t.Fatalf("Execute output = %q, want %q", out, want)
	}
	// No path argument → the search ran against the tool's cwd with the
	// default result cap.
	if f.pattern != "needle" {
		t.Errorf("pattern = %q, want needle", f.pattern)
	}
	if f.opts.Path != cwd {
		t.Errorf("opts.Path = %q, want the tool cwd", f.opts.Path)
	}
	if f.opts.MaxResults != DefaultMaxResults {
		t.Errorf("opts.MaxResults = %d, want %d (defaulted)", f.opts.MaxResults, DefaultMaxResults)
	}
}

// TestGrepExecuteSingularMatch verifies the "Found 1 match" singular header.
func TestGrepExecuteSingularMatch(t *testing.T) {
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return []Hit{{Path: `C:\work\a.txt`, Line: 1, Text: "needle"}}, nil
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "Found 1 match\n\n") {
		t.Fatalf("Execute output = %q, want the singular header", out)
	}
}

// TestGrepExecuteHonorsExplicitPathAndInclude verifies the explicit path and
// include arguments are forwarded to the search (the dsh parameter surface).
func TestGrepExecuteHonorsExplicitPathAndInclude(t *testing.T) {
	tool, f := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return nil, nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"C:\\tree","pattern":"x","include":"*.go"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.opts.Path != `C:\tree` || f.opts.Include != "*.go" {
		t.Fatalf("opts = %+v, want path C:\\tree / include *.go", f.opts)
	}
}

// TestGrepExecuteNoMatches asserts the dsh no-match report.
func TestGrepExecuteNoMatches(t *testing.T) {
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return nil, nil
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","path":"C:\\tree"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "No matches found" {
		t.Fatalf("Execute output = %q, want the no-matches report", out)
	}
}

// TestGrepExecuteLimitReached asserts the capped-result shape: the (limit
// reached) header suffix plus the dsh could-not-save footer.
func TestGrepExecuteLimitReached(t *testing.T) {
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return []Hit{{Path: `C:\work\a.txt`, Line: 1, Text: "needle"}}, ErrLimit
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatalf("Execute must not surface ErrLimit as an error: %v", err)
	}
	if !strings.HasPrefix(out, "Found 1 match (limit reached)") {
		t.Fatalf("Execute output = %q, want the (limit reached) header", out)
	}
	if !strings.HasSuffix(out, "(The complete result could not be saved; narrow pattern, path, or include to see more.)") {
		t.Fatalf("Execute output = %q, want the could-not-save footer", out)
	}
}

// TestGrepExecuteRejectsEmptyPattern asserts an empty pattern is rejected
// before the search runs; a whitespace-only pattern is a legitimate regex
// (dsh parseGrepArgs) and runs.
func TestGrepExecuteRejectsEmptyPattern(t *testing.T) {
	ran := false
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		ran = true
		return nil, nil
	})
	for _, args := range []string{`{}`, `{"pattern":""}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Execute with args %s must error", args)
		}
	}
	if ran {
		t.Fatal("the search must not run for an empty pattern")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"   "}`)); err != nil {
		t.Fatalf("a whitespace-only pattern is a legitimate regex and must run: %v", err)
	}
}

// TestGrepExecuteRejectsEmptyPath asserts a call with no path and no tool cwd
// fails closed.
func TestGrepExecuteRejectsEmptyPath(t *testing.T) {
	tool, _ := newFakeTool("", func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return nil, nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`)); err == nil {
		t.Fatal("Execute with no path and no cwd must error")
	}
}

// TestGrepIncludeValidation pins the dsh validateInclude messages: one
// positive glob only — blank, negated and comma-separated forms are rejected,
// brace alternation is accepted.
func TestGrepIncludeValidation(t *testing.T) {
	ran := false
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		ran = true
		return nil, nil
	})
	for _, args := range []string{
		`{"pattern":"x","include":"  "}`,
		`{"pattern":"x","include":"!*.go"}`,
		`{"pattern":"x","include":"*.go,*.ts"}`,
	} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Execute with args %s must error", args)
		} else if !strings.Contains(err.Error(), "include must be") {
			t.Errorf("include error = %v, want the include validation message", err)
		}
	}
	if ran {
		t.Fatal("the search must not run for an invalid include")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"x","include":"*.{go,ts}"}`)); err != nil {
		t.Fatalf("brace alternation must be accepted: %v", err)
	}
}

// mkFile creates a file (and any missing parents) with the given content.
func mkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestGlobMatchesPathPatterns exercises the real matcher over a temp tree
// with dsh semantics: "**/*.go" matches nested Go files, "*.md" matches
// basenames at ANY depth (no "/" in the pattern), and "src/**" the subtree.
func TestGlobMatchesPathPatterns(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "a.go"), "x")
	mkFile(t, filepath.Join(root, "sub", "b.go"), "x")
	mkFile(t, filepath.Join(root, "sub", "deep", "c.go"), "x")
	mkFile(t, filepath.Join(root, "README.md"), "x")
	mkFile(t, filepath.Join(root, "sub", "nested.md"), "x")
	mkFile(t, filepath.Join(root, "src", "main.go"), "x")

	tool := NewGlobTool(root)
	ctx := context.Background()

	out, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatalf("glob **/*.go: %v", err)
	}
	for _, want := range []string{"a.go", "sub/b.go", "sub/deep/c.go", "src/main.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("glob **/*.go output missing %q: %q", want, out)
		}
	}

	// A pattern with no "/" matches basenames at any depth (dsh): both the
	// root-level and the nested .md files match.
	out, err = tool.Execute(ctx, json.RawMessage(`{"pattern":"*.md"}`))
	if err != nil {
		t.Fatalf("glob *.md: %v", err)
	}
	if !strings.Contains(out, "README.md") || !strings.Contains(out, "sub/nested.md") {
		t.Errorf("glob *.md output = %q, want README.md and sub/nested.md (basename at any depth)", out)
	}

	out, err = tool.Execute(ctx, json.RawMessage(`{"pattern":"src/**"}`))
	if err != nil {
		t.Fatalf("glob src/**: %v", err)
	}
	if !strings.Contains(out, "src/main.go") {
		t.Errorf("glob src/** output = %q, want src/main.go", out)
	}
}

// TestGlobNoFilesFound asserts the dsh no-match report.
func TestGlobNoFilesFound(t *testing.T) {
	tool := NewGlobTool(t.TempDir())
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*.nope"}`))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if out != "No files found" {
		t.Fatalf("glob output = %q, want No files found", out)
	}
}

// TestGlobSortByModificationTime verifies the dsh --sort=modified contract:
// newest file first.
func TestGlobSortByModificationTime(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "old.txt"), "x")
	mkFile(t, filepath.Join(root, "new.txt"), "x")
	mkFile(t, filepath.Join(root, "mid.txt"), "x")
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"old.txt", "mid.txt", "new.txt"} {
		p := filepath.Join(root, name)
		if err := os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	tool := NewGlobTool(root)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[0] != "new.txt" || lines[2] != "old.txt" {
		t.Fatalf("glob output = %q, want newest first (new.txt, mid.txt, old.txt)", out)
	}
}

// TestGlobPageFooter verifies the over-cap page: the first 100 paths plus the
// dsh could-not-save footer carrying the complete count.
func TestGlobPageFooter(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 105; i++ {
		mkFile(t, filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), "x")
	}
	tool := NewGlobTool(root)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(out, "(Showing 100 of 105 paths. The complete result could not be saved; narrow pattern or path to see more.)") {
		t.Fatalf("glob output tail = %q, want the page footer", out[len(out)-120:])
	}
}

// TestGlobRejectsEmptyPattern asserts glob fails closed on an empty or blank
// pattern (dsh parseGlobArgs).
func TestGlobRejectsEmptyPattern(t *testing.T) {
	tool := NewGlobTool(t.TempDir())
	for _, args := range []string{`{}`, `{"pattern":""}`, `{"pattern":"   "}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("glob with args %s must error", args)
		}
	}
}

func TestSearchToolsRespectInjectedWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	grep := NewGrepTool(root)
	grep.RootContextFunc = func(context.Context) string { return root }
	if _, err := grep.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, outside))); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("grep outside error = %v, want workspace rejection", err)
	}
	if output, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","path":"."}`)); err != nil || !strings.Contains(output, "inside.txt") {
		t.Fatalf("grep inside = %q, %v", output, err)
	}

	glob := NewGlobTool(root)
	glob.RootContextFunc = func(context.Context) string { return root }
	if _, err := glob.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"*","path":%q}`, outside))); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("glob outside error = %v, want workspace rejection", err)
	}
	if output, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`)); err != nil || !strings.Contains(output, "inside.txt") {
		t.Fatalf("glob inside = %q, %v", output, err)
	}
}

func TestSearchToolsRejectSymlinkOutsideInjectedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	grepLink := filepath.Join(root, "linked")
	if err := os.Symlink(outside, grepLink); err != nil {
		t.Skipf("symlink unavailable in this environment: %v", err)
	}
	if _, err := os.Lstat(grepLink); err != nil {
		t.Skipf("symlink unavailable in this environment: %v", err)
	}
	grep := NewGrepTool(root)
	grep.RootContextFunc = func(context.Context) string { return root }
	if _, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"secret","path":"linked"}`)); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("grep symlink error = %v, want workspace rejection", err)
	}
	globLink := filepath.Join(root, "linked")
	if err := os.Symlink(outside, globLink); err != nil {
		t.Skipf("symlink unavailable in this environment: %v", err)
	}
	if _, err := os.Lstat(globLink); err != nil {
		t.Skipf("symlink unavailable in this environment: %v", err)
	}
	glob := NewGlobTool(root)
	glob.RootContextFunc = func(context.Context) string { return root }
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"*","path":"linked"}`)); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("glob symlink error = %v, want workspace rejection", err)
	}
}
