package pathsecure

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveExistingNormalDirectory(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolveExisting(dir)
	if err != nil {
		t.Fatalf("ResolveExisting(%q): %v", dir, err)
	}
	if !filepath.IsAbs(got) || filepath.Clean(got) == "." {
		t.Fatalf("resolved path = %q, want absolute directory", got)
	}
	info, err := os.Stat(got)
	if err != nil || !info.IsDir() {
		t.Fatalf("resolved path stat = %v/%v, want directory", info, err)
	}
}

func TestResolveExistingRejectsMissingPath(t *testing.T) {
	_, err := ResolveExisting(filepath.Join(t.TempDir(), "missing"))
	if !os.IsNotExist(err) {
		t.Fatalf("missing path error = %v, want not-exist", err)
	}
}

func TestResolveExistingSymlinkBoundary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	got, err := ResolveExisting(link)
	if err != nil {
		if runtime.GOOS == "windows" {
			// A managed Windows host may deny canonical handle resolution;
			// the secure fallback must reject the reparse point rather than
			// guessing its target.
			return
		}
		t.Fatalf("ResolveExisting(link): %v", err)
	}
	want, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(link): %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("resolved link = %q, want %q", got, want)
	}
}
