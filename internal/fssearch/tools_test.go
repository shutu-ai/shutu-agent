// tools_test.go — the dsh-aligned grep/glob tool tests (docs/dispatch-gap-1.md
// §3). A fake searchFn substitutes the engine so the Execute output format is
// asserted without touching the disk: hit lines ("path:line: text" relative to
// cwd), the match count, the no-match report, the ErrLimit "(limit reached)"
// suffix, and the empty-pattern rejection. Glob tests exercise the real
// matcher over a temp tree.
package fssearch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestGrepExecuteFormatsHits asserts the hit output shape: each hit as
// "path:line: text" (relative to cwd), the trailing "N matches" line, and the
// defaulted root/max_results mapping.
func TestGrepExecuteFormatsHits(t *testing.T) {
	tool, f := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return []Hit{
			{Path: `C:\work\a.txt`, Line: 2, Text: "needle one"},
			{Path: `C:\work\sub\b.go`, Line: 5, Text: "needle two"},
		}, nil
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The display is relative to cwd using the platform separator.
	relB := filepath.Join("sub", "b.go")
	want := "a.txt:2: needle one\n" + relB + ":5: needle two\n2 matches"
	if out != want {
		t.Fatalf("Execute output = %q, want %q", out, want)
	}
	// No path argument → the search ran against the tool's cwd with the
	// default result cap.
	if f.pattern != "needle" {
		t.Errorf("pattern = %q, want needle", f.pattern)
	}
	if f.opts.Path != `C:\work` {
		t.Errorf("opts.Path = %q, want the tool cwd", f.opts.Path)
	}
	if f.opts.MaxResults != DefaultMaxResults {
		t.Errorf("opts.MaxResults = %d, want %d (defaulted)", f.opts.MaxResults, DefaultMaxResults)
	}
}

// TestGrepExecuteHonorsExplicitPathAndMaxResults verifies the explicit
// path and max_results arguments are forwarded to the search.
func TestGrepExecuteHonorsExplicitPathAndMaxResults(t *testing.T) {
	tool, f := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return nil, nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"C:\\tree","pattern":"x","max_results":7}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.opts.Path != `C:\tree` || f.opts.MaxResults != 7 {
		t.Fatalf("opts = %+v, want path C:\\tree / max_results 7", f.opts)
	}
}

// TestGrepExecuteNoMatches asserts the no-match report format.
func TestGrepExecuteNoMatches(t *testing.T) {
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return nil, nil
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","path":"C:\\tree"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != `no matches for "needle" in C:\tree` {
		t.Fatalf("Execute output = %q, want the no-matches report", out)
	}
}

// TestGrepExecuteLimitReached asserts the ErrLimit suffix on a partial result.
func TestGrepExecuteLimitReached(t *testing.T) {
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		return []Hit{{Path: `C:\work\a.txt`, Line: 1, Text: "needle"}}, ErrLimit
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatalf("Execute must not surface ErrLimit as an error: %v", err)
	}
	if !strings.HasSuffix(out, "1 matches (limit reached)") {
		t.Fatalf("Execute output = %q, want the (limit reached) suffix", out)
	}
}

// TestGrepExecuteRejectsEmptyPattern asserts an empty/blank pattern is
// rejected before the search runs.
func TestGrepExecuteRejectsEmptyPattern(t *testing.T) {
	ran := false
	tool, _ := newFakeTool(`C:\work`, func(ctx context.Context, pattern string, opts Options) ([]Hit, error) {
		ran = true
		return nil, nil
	})
	for _, args := range []string{`{}`, `{"pattern":""}`, `{"pattern":"   "}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Execute with args %s must error", args)
		}
	}
	if ran {
		t.Fatal("the search must not run for an empty pattern")
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

// TestGlobMatchesPathPatterns exercises the real glob matcher over a temp
// tree: "**/*.go" matches nested Go files, "*.md" only the root level, and
// "src/**" the subtree.
func TestGlobMatchesPathPatterns(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("a.go")
	mk("sub/b.go")
	mk("sub/deep/c.go")
	mk("README.md")
	mk("src/main.go")

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
	if !strings.Contains(out, "4 matches") {
		t.Errorf("glob **/*.go output = %q, want 4 matches", out)
	}

	out, err = tool.Execute(ctx, json.RawMessage(`{"pattern":"*.md"}`))
	if err != nil {
		t.Fatalf("glob *.md: %v", err)
	}
	if !strings.Contains(out, "README.md") || strings.Contains(out, "sub/") {
		t.Errorf("glob *.md output = %q, want only the root-level README.md", out)
	}

	out, err = tool.Execute(ctx, json.RawMessage(`{"pattern":"src/**"}`))
	if err != nil {
		t.Fatalf("glob src/**: %v", err)
	}
	if !strings.Contains(out, "src/main.go") {
		t.Errorf("glob src/** output = %q, want src/main.go", out)
	}
}

// TestGlobRejectsEmptyPattern asserts glob fails closed on an empty pattern.
func TestGlobRejectsEmptyPattern(t *testing.T) {
	tool := NewGlobTool(t.TempDir())
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("glob with no pattern must error")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"   "}`)); err == nil {
		t.Fatal("glob with a blank pattern must error")
	}
}
