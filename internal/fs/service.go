// Package fs defines the safe-file-operation capability seam (design.md §10
// D2, ADR 2026-08-19-m6-agent-full.md 决策 M6f): a FileService seam for reading,
// writing and listing files inside an allowed root. Consumers (M6f-3's fs_*
// tools and the fs/* event wiring) depend only on the seam's interface (D2),
// so swapping or restricting the backend never touches consumer code.
//
// Security boundary — every operation constrains its path to the allowed root
// (the configured fs.root, defaulting to <project>, the process working
// directory):
//   - all paths are passed through filepath.Clean and a lexical prefix check;
//   - a relative path is resolved under the root and a ".." escape is
//     rejected (ErrPathOutsideRoot) before any file is touched;
//   - an absolute path is accepted only when it cleans to a path inside the
//     root (it is never joined as a new root);
//   - List is non-recursive: it returns the direct children of one directory;
//   - Read is bounded: a file whose size exceeds the read cap (maxSize, or
//     DefaultMaxReadSize = 1MiB when 0) is rejected with ErrTooLarge before
//     its content is read, so a huge file can never blow the model context.
//
// The boundary is lexical containment (Clean + prefix), not symlink
// resolution: a symlink inside the root that points outside it is followed by
// the OS. That is a recorded limitation of the v1 local backend, matching the
// controlled-isolation posture of M3's run_command and M6e's code sandbox —
// this is a convenience seam for the personal agent's own files, not a
// security boundary for hostile input.
//
// Lifecycle: Close marks the service closed and is idempotent. The local
// backend (local.go) holds no OS resources, so Close only flips a flag;
// operations after Close are rejected with ErrClosed. The default backend is
// localFS (NewLocalFS); a nil-root constructor defaults to the process
// working directory.
package fs

import (
	"context"
	"errors"
)

// Entry is one direct child of a listed directory (List is non-recursive).
// Path is the entry's path relative to the allowed root, so a caller can pass
// it straight back into Read/Write/List (the root itself never appears: the
// children of the root list as "name").
type Entry struct {
	Name  string // base name
	Path  string // path relative to the allowed root ("a/b.txt")
	IsDir bool
	Size  int64 // file size in bytes; 0 for directories
}

// FileService is the safe-file-operation Service (design.md §10 D2, ADR
// 决策 M6f). Consumers depend only on this interface, never on a concrete
// backend. Every method is constrained to the allowed root (Root); a path
// that escapes it is rejected. Close is idempotent and rejects further
// operations with ErrClosed.
type FileService interface {
	// Read returns the UTF-8 text content of the file at path (within Root).
	// A path that escapes the root is rejected with ErrPathOutsideRoot, a
	// missing path or directory read errors normally, and a file larger than
	// maxSize (or DefaultMaxReadSize when maxSize <= 0) is rejected with
	// ErrTooLarge before it is read.
	Read(ctx context.Context, path string, maxSize int) (string, error)
	// ReadBytes returns raw bytes for bounded binary resources such as images.
	ReadBytes(ctx context.Context, path string, maxSize int) ([]byte, error)
	// Write creates or overwrites the file at path (within Root) with
	// content. A path that escapes the root is rejected with
	// ErrPathOutsideRoot; missing parent directories are created on demand.
	Write(ctx context.Context, path, content string) error
	// List returns the direct (non-recursive) children of the directory at
	// dir (within Root), sorted by name. A path that escapes the root or a
	// missing directory errors.
	List(ctx context.Context, dir string) ([]Entry, error)
	// Fingerprint returns a stable content version used by observation policy.
	Fingerprint(ctx context.Context, path string) (string, error)
	// Root returns the absolute, cleaned allowed root (the configured fs.root,
	// defaulting to <project>, the process working directory).
	Root() string
	// Close marks the service closed; it is idempotent and operations after
	// Close are rejected with ErrClosed.
	Close() error
}

// DefaultMaxReadSize is the Read content cap applied when maxSize <= 0 (1MiB).
// It bounds what one read returns to the model, keeping a large file from
// blowing the model context (dispatch-m6f-3 §1: 防读大文件爆上下文).
const DefaultMaxReadSize = 1 * 1024 * 1024

// Sentinel errors returned by the seam so callers can distinguish failures
// without parsing message text.
var (
	// ErrPathOutsideRoot rejects a path that cleans to a location outside the
	// allowed root (a ".." escape, or an absolute path outside the root).
	ErrPathOutsideRoot = errors.New("fs: path escapes the allowed root")
	// ErrTooLarge rejects a Read whose file exceeds the size cap.
	ErrTooLarge = errors.New("fs: file exceeds the read size limit")
	// ErrClosed rejects operations on a closed service.
	ErrClosed = errors.New("fs: service closed")
)
