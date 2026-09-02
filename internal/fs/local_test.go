package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestFS(t *testing.T) *localFS {
	t.Helper()
	return NewLocalFS(t.TempDir())
}

// TestLocalFSWriteReadListRoundTrip covers the Read/Write/List happy path
// (dispatch-m6f-3 自测): write a file, read it back verbatim, and list the
// containing directory with the expected entry.
func TestLocalFSWriteReadListRoundTrip(t *testing.T) {
	fsys := newTestFS(t)
	ctx := context.Background()

	if err := fsys.Write(ctx, "notes.txt", "hello fs"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := fsys.Read(ctx, "notes.txt", 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello fs" {
		t.Fatalf("read = %q, want hello fs", got)
	}

	// A nested write lands in a created parent directory.
	if err := fsys.Write(ctx, filepath.Join("a", "b", "deep.txt"), "deep"); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	got, err = fsys.Read(ctx, filepath.Join("a", "b", "deep.txt"), 0)
	if err != nil {
		t.Fatalf("nested read: %v", err)
	}
	if got != "deep" {
		t.Fatalf("nested read = %q, want deep", got)
	}

	// Overwrite is allowed.
	if err := fsys.Write(ctx, "notes.txt", "rewritten"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := fsys.Read(ctx, "notes.txt", 0); got != "rewritten" {
		t.Fatalf("overwritten read = %q, want rewritten", got)
	}

	entries, err := fsys.List(ctx, ".")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "notes.txt" {
		t.Fatalf("root entries = %v, want [a notes.txt] (sorted, non-recursive)", names)
	}
	var notes *Entry
	for i := range entries {
		if entries[i].Name == "notes.txt" {
			notes = &entries[i]
		}
	}
	if notes == nil || notes.IsDir || notes.Size != int64(len("rewritten")) {
		t.Fatalf("notes.txt entry = %+v, want a file of %d bytes", notes, len("rewritten"))
	}
	if notes.Path != "notes.txt" {
		t.Fatalf("notes.txt Path = %q, want relative to the root", notes.Path)
	}
}

// TestLocalFSReadMissingFile verifies a missing path errors normally (never
// panics) and that the error is an os error the caller can surface to the
// model.
func TestLocalFSReadMissingFile(t *testing.T) {
	fsys := newTestFS(t)
	if _, err := fsys.Read(context.Background(), "nope.txt", 0); err == nil {
		t.Fatal("reading a missing file must error")
	}
}

// TestLocalFSPathEscapeRejected verifies the containment boundary
// (dispatch-m6f-3 §1 / 自测: 路径越界，.. 逃逸拒绝): every operation rejects a
// path that cleans to a location outside the root, before any file or
// directory is touched.
func TestLocalFSPathEscapeRejected(t *testing.T) {
	fsys := newTestFS(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(fsys.Root()), "outside.txt")

	// A plain ".." escape, with and without extra segments.
	for _, p := range []string{
		"..",
		"../outside.txt",
		filepath.Join("a", "..", "..", "outside.txt"),
		filepath.Join("..", "..", "x"),
	} {
		if _, err := fsys.Read(ctx, p, 0); !errors.Is(err, ErrPathOutsideRoot) {
			t.Errorf("Read(%q) err = %v, want ErrPathOutsideRoot", p, err)
		}
		if err := fsys.Write(ctx, p, "x"); !errors.Is(err, ErrPathOutsideRoot) {
			t.Errorf("Write(%q) err = %v, want ErrPathOutsideRoot", p, err)
		}
		if _, err := fsys.List(ctx, p); !errors.Is(err, ErrPathOutsideRoot) {
			t.Errorf("List(%q) err = %v, want ErrPathOutsideRoot", p, err)
		}
	}

	// An absolute path outside the root is rejected too.
	if _, err := fsys.Read(ctx, outside, 0); !errors.Is(err, ErrPathOutsideRoot) {
		t.Errorf("Read(abs outside) err = %v, want ErrPathOutsideRoot", err)
	}
	if err := fsys.Write(ctx, outside, "x"); !errors.Is(err, ErrPathOutsideRoot) {
		t.Errorf("Write(abs outside) err = %v, want ErrPathOutsideRoot", err)
	}
	if _, err := fsys.List(ctx, filepath.Dir(fsys.Root())); !errors.Is(err, ErrPathOutsideRoot) {
		t.Errorf("List(abs outside) err = %v, want ErrPathOutsideRoot", err)
	}

	// An absolute path inside the root is accepted.
	if err := fsys.Write(ctx, "abs.txt", "x"); err != nil {
		t.Fatalf("seed abs.txt: %v", err)
	}
	inside := filepath.Join(fsys.Root(), "abs.txt")
	if got, err := fsys.Read(ctx, inside, 0); err != nil || got != "x" {
		t.Fatalf("Read(abs inside) = %q, %v, want x", got, err)
	}

	// Nothing was created outside the root by the rejected writes.
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("rejected write must not create %s", outside)
	}
}

// TestLocalFSReadSizeCap verifies the Read bound (dispatch-m6f-3 §1 / 自测:
// Read 大小上限): a file exceeding the cap (an explicit maxSize here) is
// rejected with ErrTooLarge before its content is read, while a file within
// the cap reads normally.
func TestLocalFSReadSizeCap(t *testing.T) {
	fsys := newTestFS(t)
	ctx := context.Background()
	if err := fsys.Write(ctx, "big.txt", strings.Repeat("x", 100)); err != nil {
		t.Fatalf("seed big.txt: %v", err)
	}
	if _, err := fsys.Read(ctx, "big.txt", 50); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read with cap 50 err = %v, want ErrTooLarge", err)
	}
	got, err := fsys.Read(ctx, "big.txt", 200)
	if err != nil || len(got) != 100 {
		t.Fatalf("Read with cap 200 = %d bytes, %v, want the full 100 bytes", len(got), err)
	}
	// A zero maxSize applies DefaultMaxReadSize (1MiB).
	if _, err := fsys.Read(ctx, "big.txt", 0); err != nil {
		t.Fatalf("Read with cap 0 (default) err = %v, want success", err)
	}
	// The default cap is 1MiB: a file above it is rejected.
	if err := fsys.Write(ctx, "huge.txt", strings.Repeat("x", DefaultMaxReadSize+1)); err != nil {
		t.Fatalf("seed huge.txt: %v", err)
	}
	if _, err := fsys.Read(ctx, "huge.txt", 0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read of a >1MiB file err = %v, want ErrTooLarge", err)
	}
}

// TestLocalFSListIsNonRecursiveAndRejectsMissingDir verifies List returns only
// direct children and errors on a missing directory (dispatch-m6f-3 §1).
func TestLocalFSListIsNonRecursiveAndRejectsMissingDir(t *testing.T) {
	fsys := newTestFS(t)
	ctx := context.Background()
	if err := fsys.Write(ctx, filepath.Join("d", "inner.txt"), "x"); err != nil {
		t.Fatalf("seed nested: %v", err)
	}
	entries, err := fsys.List(ctx, ".")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "d" || !entries[0].IsDir {
		t.Fatalf("root entries = %+v, want exactly the d directory (non-recursive)", entries)
	}
	inner, err := fsys.List(ctx, "d")
	if err != nil {
		t.Fatalf("list d: %v", err)
	}
	if len(inner) != 1 || inner[0].Name != "inner.txt" || inner[0].IsDir {
		t.Fatalf("d entries = %+v, want the inner.txt file", inner)
	}
	if _, err := fsys.List(ctx, "missing"); err == nil {
		t.Fatal("listing a missing directory must error")
	}
}

// TestLocalFSCloseIsIdempotent verifies the Close contract (dispatch-m6f-3
// §1 / 自测): Close succeeds repeatedly, and operations after Close are
// rejected with ErrClosed.
func TestLocalFSCloseIsIdempotent(t *testing.T) {
	fsys := newTestFS(t)
	ctx := context.Background()
	if err := fsys.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("second Close must be a no-op: %v", err)
	}
	if err := fsys.Write(ctx, "x.txt", "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close err = %v, want ErrClosed", err)
	}
	if _, err := fsys.Read(ctx, "x.txt", 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after Close err = %v, want ErrClosed", err)
	}
	if _, err := fsys.List(ctx, "."); !errors.Is(err, ErrClosed) {
		t.Fatalf("List after Close err = %v, want ErrClosed", err)
	}
}

// TestLocalFSRootDefaultsToWorkingDirectory verifies an empty root resolves
// to the process working directory (<project>), the documented default
// (dispatch-m6f-3 §1 / 决策: Root 默认 <项目>).
func TestLocalFSRootDefaultsToWorkingDirectory(t *testing.T) {
	fsys := NewLocalFS("")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if fsys.Root() != filepath.Clean(wd) {
		t.Fatalf("Root = %q, want the working directory %q", fsys.Root(), filepath.Clean(wd))
	}
}

func TestLocalFSRejectsSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "secret.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink unavailable in this environment: %v", err)
	}
	fsys := NewLocalFS(root)
	ctx := context.Background()
	if _, err := fsys.Read(ctx, "secret.txt", 0); !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("Read through outside symlink error = %v, want ErrPathOutsideRoot", err)
	}
	if err := fsys.Write(ctx, "secret.txt", "overwrite"); !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("Write through outside symlink error = %v, want ErrPathOutsideRoot", err)
	}

	outsideDir := filepath.Join(outside, "new-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked-dir")
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("directory symlink unavailable in this environment: %v", err)
	}
	if err := fsys.Write(ctx, filepath.Join("linked-dir", "created.txt"), "escape"); !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("Write below outside symlink error = %v, want ErrPathOutsideRoot", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created through symlink: %v", err)
	}
}
