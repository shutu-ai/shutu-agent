// search_test.go — the D-GAP-1 search-engine tests (docs/dispatch-gap-1.md
// §2, 对齐 dsh tool-fs-search). The cases build a throwaway tree with
// t.TempDir() and exercise Search directly: regex hits and ordering, the
// case-sensitive default (ripgrep semantics), the ignored-directory and
// binary skips, the MaxResults/MaxFiles caps (ErrLimit + partial hits), the
// dsh-style Include glob, the MaxFileBytes skip, the error paths (missing
// path / empty query / invalid regex) and context cancellation.
package fssearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates a file (and any missing parents) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSearchRegexHits covers the happy path (dispatch-gap-1 §2 #1): a query
// (always a regular expression, dsh grep) matches across multiple files and
// lines, every Hit carries the absolute path, the 1-based line number and the
// trimmed line, and the result is ordered file-then-line (files in lexical
// walk order, lines ascending).
func TestSearchRegexHits(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello world\nneedle one\ntail\nneedle two\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "no match\nneedle b\n")

	hits, err := Search(context.Background(), "needle", Options{Path: dir})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []struct {
		base string
		line int
		text string
	}{
		{"a.txt", 2, "needle one"},
		{"a.txt", 4, "needle two"},
		{"b.txt", 2, "needle b"},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %d, want %d (%v)", len(hits), len(want), hits)
	}
	for i, w := range want {
		if filepath.Base(hits[i].Path) != w.base || hits[i].Line != w.line || hits[i].Text != w.text {
			t.Errorf("hit[%d] = %+v, want %+v", i, hits[i], w)
		}
		if !filepath.IsAbs(hits[i].Path) {
			t.Errorf("hit[%d].Path = %q, want absolute", i, hits[i].Path)
		}
	}

	// A regex query matches by pattern, not substring (dsh: pattern is a
	// regex).
	hits, err = Search(context.Background(), `ne+dle \w+`, Options{Path: dir})
	if err != nil {
		t.Fatalf("Search (regex): %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("regex hits = %d, want 3 (%v)", len(hits), hits)
	}
}

// TestSearchCaseSensitiveDefault covers the ripgrep default: matching is
// case-sensitive — an uppercase query matches only uppercase lines (dsh grep
// has no case-insensitive switch).
func TestSearchCaseSensitiveDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "NEEDLE upper\nneedle lower\n")

	hits, err := Search(context.Background(), "NEEDLE", Options{Path: dir})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Line != 1 || hits[0].Text != "NEEDLE upper" {
		t.Fatalf("hits = %+v, want the uppercase line only (case-sensitive default)", hits)
	}
}

// TestSearchInvalidRegex verifies a malformed pattern fails closed with an
// error, never a silent no-match.
func TestSearchInvalidRegex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "apple pie\nbanana split\n")

	if _, err := Search(context.Background(), `(`, Options{Path: dir}); err == nil {
		t.Fatal("an invalid regex must error, not silently match nothing")
	} else if !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("regex error = %v, want it to mention the invalid pattern", err)
	}
}

// TestSearchSkipsIgnoredDirs covers #4: the .git and node_modules subtrees are
// skipped while sibling and nested non-ignored files are still searched.
func TestSearchSkipsIgnoredDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "config"), "needle hidden\n")
	writeFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "needle hidden\n")
	writeFile(t, filepath.Join(dir, "keep.txt"), "needle visible\n")
	writeFile(t, filepath.Join(dir, "sub", "ok.go"), "needle nested\n")

	hits, err := Search(context.Background(), "needle", Options{Path: dir})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want only keep.txt and sub/ok.go (ignored dirs skipped)", hits)
	}
	bases := map[string]bool{}
	for _, h := range hits {
		bases[filepath.Base(h.Path)] = true
	}
	if !bases["keep.txt"] || !bases["ok.go"] {
		t.Fatalf("hits = %v, want keep.txt and sub/ok.go", hits)
	}
}

// TestSearchSkipsBinary covers #5: a file containing a NUL byte in its first
// 8 KiB is skipped without producing a hit and without an error.
func TestSearchSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), []byte("needle\x00 rest of binary"), 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	writeFile(t, filepath.Join(dir, "text.txt"), "needle here\n")

	hits, err := Search(context.Background(), "needle", Options{Path: dir})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || filepath.Base(hits[0].Path) != "text.txt" {
		t.Fatalf("hits = %v, want only text.txt (binary file skipped, no error)", hits)
	}
}

// TestSearchLimits covers #6: MaxResults and MaxFiles each stop the scan and
// return ErrLimit together with the partial hits.
func TestSearchLimits(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "many.txt"), "needle\nneedle\nneedle\nneedle\nneedle\n")

	// MaxResults: stop after 3 hits and return ErrLimit with the partial set.
	hits, err := Search(context.Background(), "needle", Options{Path: dir, MaxResults: 3})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("Search (MaxResults) error = %v, want ErrLimit", err)
	}
	if len(hits) != 3 {
		t.Fatalf("MaxResults partial hits = %d, want 3", len(hits))
	}
	for i, h := range hits {
		if h.Line != i+1 {
			t.Errorf("partial hit[%d].Line = %d, want %d", i, h.Line, i+1)
		}
	}

	// MaxFiles: scan only the first file (lexical order) and stop with ErrLimit.
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "a.txt"), "needle in a\n")
	writeFile(t, filepath.Join(dir2, "b.txt"), "needle in b\n")
	hits, err = Search(context.Background(), "needle", Options{Path: dir2, MaxFiles: 1})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("Search (MaxFiles) error = %v, want ErrLimit", err)
	}
	if len(hits) != 1 || filepath.Base(hits[0].Path) != "a.txt" {
		t.Fatalf("MaxFiles partial hits = %v, want only a.txt", hits)
	}
}

// TestSearchInclude covers the dsh-style Include glob: a pattern with no "/"
// restricts by basename at ANY depth ("*.go" finds every .go file), a pattern
// with "/" is anchored at the search root ("src/*.go"), and brace alternation
// works.
func TestSearchInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "needle go\n")
	writeFile(t, filepath.Join(dir, "a.txt"), "needle txt\n")
	writeFile(t, filepath.Join(dir, "sub", "b.go"), "needle nested go\n")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "needle src go\n")

	// No "/": basenames at any depth.
	hits, err := Search(context.Background(), "needle", Options{Path: dir, Include: "*.go"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("include *.go hits = %d, want 3 (%v)", len(hits), hits)
	}
	for _, h := range hits {
		if filepath.Ext(h.Path) != ".go" {
			t.Errorf("hit %+v filtered in despite include *.go", h)
		}
	}

	// With "/": anchored at the search root.
	hits, err = Search(context.Background(), "needle", Options{Path: dir, Include: "src/*.go"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Path, "src"+string(filepath.Separator)+"main.go") {
		t.Fatalf("include src/*.go hits = %v, want only src/main.go", hits)
	}

	// Brace alternation.
	hits, err = Search(context.Background(), "needle", Options{Path: dir, Include: "*.{go,txt}"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 4 {
		t.Fatalf("include *.{go,txt} hits = %d, want 4", len(hits))
	}
}

// TestSearchMaxFileBytes covers #8: files larger than MaxFileBytes are skipped
// while smaller files are still searched.
func TestSearchMaxFileBytes(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 2000) + "needle tail\n"
	writeFile(t, filepath.Join(dir, "big.txt"), big)
	writeFile(t, filepath.Join(dir, "small.txt"), "needle small\n")

	hits, err := Search(context.Background(), "needle", Options{Path: dir, MaxFileBytes: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || filepath.Base(hits[0].Path) != "small.txt" {
		t.Fatalf("hits = %v, want only small.txt (oversized file skipped)", hits)
	}
}

// TestSearchErrors covers #9: a missing path, an empty query and an invalid
// regex all error. A whitespace-only pattern is a legitimate regex (dsh) and
// runs.
func TestSearchErrors(t *testing.T) {
	if _, err := Search(context.Background(), "needle", Options{Path: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("a nonexistent path must error")
	}
	if _, err := Search(context.Background(), "", Options{Path: t.TempDir()}); err == nil {
		t.Fatal("an empty query must error")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "three spaces   here\n")
	hits, err := Search(context.Background(), "   ", Options{Path: dir})
	if err != nil {
		t.Fatalf("a whitespace-only pattern is a legitimate regex and must run: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("whitespace-pattern hits = %d, want 1 (the line with three spaces)", len(hits))
	}
}

// TestSearchContextCancel covers #10: a cancelled context aborts the search
// and returns ctx.Err().
func TestSearchContextCancel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "needle here\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the search must bail out immediately
	if _, err := Search(ctx, "needle", Options{Path: dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search error = %v, want context.Canceled", err)
	}
}
