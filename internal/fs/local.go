package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// localFS is the default FileService backend (ADR 决策 M6f / dispatch-m6f-3
// §1): direct os/filepath access constrained to an allowed root, with zero
// third-party dependencies. It holds no OS resources, so Close only flips a
// closed flag (idempotent) and operations after Close are rejected with
// ErrClosed.
type localFS struct {
	root   string // absolute, cleaned allowed root
	mu     sync.Mutex
	closed bool
}

// NewLocalFS returns a local FileService constrained to root. An empty root
// defaults to the process working directory (<project>). The root is stored
// absolute and cleaned so containment checks are stable regardless of how the
// caller spells it.
func NewLocalFS(root string) *localFS {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		// filepath.Abs can only fail through os.Getwd for a relative root; a
		// failing Getwd leaves no usable filesystem anyway, so fall back to
		// the cleaned input and let every operation surface the failure.
		abs = filepath.Clean(root)
	}
	return &localFS{root: filepath.Clean(abs)}
}

// Root returns the absolute, cleaned allowed root.
func (l *localFS) Root() string { return l.root }

// Read returns the content of the file at path (within Root), capped at
// maxSize (DefaultMaxReadSize when maxSize <= 0). A file that exceeds the cap
// is rejected before it is read; a missing file or a directory read errors
// normally.
func (l *localFS) Read(ctx context.Context, path string, maxSize int) (string, error) {
	if err := l.checkOpen(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	full, err := l.resolve(path)
	if err != nil {
		return "", err
	}
	limit := maxSize
	if limit <= 0 {
		limit = DefaultMaxReadSize
	}
	fi, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", &os.PathError{Op: "read", Path: full, Err: os.ErrInvalid}
	}
	if fi.Size() > int64(limit) {
		return "", ErrTooLarge
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		// The file grew past the cap between Stat and Read (or a race); never
		// return an unbounded blob.
		return "", ErrTooLarge
	}
	return string(data), nil
}

func (l *localFS) ReadBytes(ctx context.Context, path string, maxSize int) ([]byte, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := l.resolve(path)
	if err != nil {
		return nil, err
	}
	limit := maxSize
	if limit <= 0 {
		limit = DefaultMaxReadSize
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &os.PathError{Op: "read", Path: full, Err: os.ErrInvalid}
	}
	if info.Size() > int64(limit) {
		return nil, ErrTooLarge
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Fingerprint returns a content hash for the current file version. It is used
// by the dsh-style observation policy before write/edit; hashing the bytes
// catches same-size changes that metadata-only checks would miss.
func (l *localFS) Fingerprint(ctx context.Context, path string) (string, error) {
	if err := l.checkOpen(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	full, err := l.resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", &os.PathError{Op: "fingerprint", Path: full, Err: os.ErrInvalid}
	}
	if info.Size() > DefaultMaxReadSize {
		return "", ErrTooLarge
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Write creates or overwrites the file at path (within Root) with content,
// creating missing parent directories on demand. A path that escapes the root
// is rejected before any directory or file is touched.
func (l *localFS) Write(ctx context.Context, path, content string) error {
	if err := l.checkOpen(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := l.resolve(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if dir != l.root && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// List returns the direct (non-recursive) children of the directory at dir
// (within Root), sorted by name. Path is each entry's path relative to the
// root so it round-trips into Read/Write/List.
func (l *localFS) List(ctx context.Context, dir string) ([]Entry, error) {
	if err := l.checkOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := l.resolve(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, de := range entries {
		info, err := de.Info()
		if err != nil {
			// A child whose stat failed (e.g. deleted mid-list) is skipped
			// rather than failing the whole listing.
			continue
		}
		rel, rerr := filepath.Rel(l.root, filepath.Join(full, de.Name()))
		if rerr != nil {
			continue
		}
		out = append(out, Entry{
			Name:  de.Name(),
			Path:  rel,
			IsDir: de.IsDir(),
			Size:  info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// resolve maps a caller path to an absolute path inside the root, or rejects
// it with ErrPathOutsideRoot. A relative path is joined under the root (Join
// cleans, so ".." collapses) and must still land inside it; an absolute path
// is cleaned and accepted only when it is already inside the root.
func (l *localFS) resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		full := filepath.Clean(path)
		if !within(l.root, full) {
			return "", ErrPathOutsideRoot
		}
		return full, nil
	}
	full := filepath.Join(l.root, path)
	if !within(l.root, full) {
		return "", ErrPathOutsideRoot
	}
	return full, nil
}

// within reports whether p is root itself or lexically inside it (a cleaned
// relative path from root that neither is ".." nor starts with ".."+sep).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkOpen rejects operations on a closed service.
func (l *localFS) checkOpen() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return nil
}

// Close marks the service closed so no further operations are accepted. It is
// idempotent and releases nothing (no OS resources live here).
func (l *localFS) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}
