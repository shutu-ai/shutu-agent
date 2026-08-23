// Package fssearch searches file contents under a directory tree
// (D-GAP-1, 对齐 dsh tool-fs-search). It is a read-only, bounded capability:
// ignored VCS/dependency directories and binary files are skipped, per-file
// and aggregate limits bound the scan, and the query is ALWAYS a regular
// expression (dsh grep contract — the pattern is a regex, matched
// case-sensitively like ripgrep). It never writes.
package fssearch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hit is one matching line.
type Hit struct {
	Path string // absolute path
	Line int    // 1-based line number
	Text string // matching line (trailing newline trimmed)
}

// Options bounds a Search. Zero values fall back to the defaults.
type Options struct {
	Path         string // root directory; "" → the caller-supplied default (组合根注入 cwd)
	Include      string // optional dsh-style glob restricting files, e.g. "*.go" or "src/*.go" (no "/" → basename at any depth)
	MaxResults   int    // cap total hits; <=0 → DefaultMaxResults
	MaxFileBytes int64  // skip files larger than this; <=0 → DefaultMaxFileBytes
	MaxFiles     int    // cap files scanned; <=0 → DefaultMaxFiles
}

// Defaults (D-GAP-1 有界与安全; 上限对齐 dsh tool-fs-search 的默认内联保留数).
const (
	// DefaultMaxResults is the grep inline cap (dsh GREP_MAX_MATCHES).
	DefaultMaxResults = 250
	// DefaultGlobMaxResults is the glob inline cap (dsh GLOB_MAX_RESULTS).
	DefaultGlobMaxResults = 100
	DefaultMaxFileBytes   = 1 << 20 // 1 MiB
	DefaultMaxFiles       = 20000
)

// ErrLimit is returned by Search when a scan cap is reached (MaxFiles or
// MaxResults); the returned hits are the partial results collected so far.
var ErrLimit = errors.New("fssearch: search limit reached")

// ignoredDirs are the VCS/dependency directory names whose subtrees are
// skipped entirely (D-GAP-1 有界与安全; dsh keeps VCS metadata out too, while
// node_modules/vendor are a shutu safety addition over dsh's --no-ignore).
var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true,
}

// Search finds Query (a regular expression, dsh grep semantics) in file
// contents under opts.Path and returns hits in file-then-line order. ErrLimit
// is returned when MaxFiles/MaxResults caps are hit (the caller may still use
// the partial hits). Query must be non-empty; a malformed regular expression
// fails closed with an error.
func Search(ctx context.Context, query string, opts Options) ([]Hit, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if query == "" {
		return nil, errors.New("fssearch: empty query")
	}
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("fssearch: empty path")
	}
	re, err := regexp.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("fssearch: invalid pattern %q: %w", query, err)
	}
	var includeRE *regexp.Regexp
	if opts.Include != "" {
		includeRE, err = pathGlobRE(opts.Include)
		if err != nil {
			return nil, fmt.Errorf("fssearch: invalid include glob %q: %w", opts.Include, err)
		}
	}
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = DefaultMaxResults
	}
	maxFileBytes := opts.MaxFileBytes
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultMaxFileBytes
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}

	// Walk from an absolute, cleaned root so every Hit.Path is absolute and
	// the containment of the scan is stable.
	root, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("fssearch: resolve %s: %w", opts.Path, err)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("fssearch: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fssearch: %s is not a directory", root)
	}

	var (
		hits         []Hit
		filesScanned int
	)
	// walk visits every entry. The callback doubles as the limit/cancel signal:
	// returning ErrLimit or ctx.Err() stops the walk (filepath.WalkDir
	// propagates the callback's error), which lets Search hand the partial hits
	// back together with the reason.
	walk := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable entry (permission, vanished mid-walk) is skipped,
			// never a hard failure or a panic (dispatch-m6f-3 §4 原则).
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if path != root && ignoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if includeRE != nil {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			if !includeRE.MatchString(filepath.ToSlash(rel)) {
				return nil
			}
		}
		if filesScanned >= maxFiles {
			return ErrLimit
		}
		filesScanned++
		fileHits, fErr := scanFile(ctx, path, d, re, maxFileBytes, maxResults-len(hits))
		if fErr != nil {
			return fErr
		}
		hits = append(hits, fileHits...)
		if len(hits) >= maxResults {
			return ErrLimit
		}
		return nil
	}
	err = filepath.WalkDir(root, walk)
	if err != nil && !errors.Is(err, ErrLimit) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return hits, ctxErr
		}
		return hits, err
	}
	if errors.Is(err, ErrLimit) {
		return hits, ErrLimit
	}
	return hits, nil
}

// scanFile searches one file for matching lines. Unreadable or binary or
// oversized files are skipped (returning nil, nil); a context cancellation is
// the only error that aborts the whole search.
func scanFile(ctx context.Context, path string, d fs.DirEntry, re *regexp.Regexp, maxFileBytes int64, remaining int) ([]Hit, error) {
	info, err := d.Info()
	if err != nil {
		return nil, nil // an unreadable file is skipped (bounded, never a panic)
	}
	if info.Size() > maxFileBytes {
		return nil, nil // oversized files are skipped (DefaultMaxFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil // unreadable files are skipped
	}
	defer f.Close()

	// Binary detection (D-GAP-1): a NUL byte in the first 8 KiB marks the file
	// as binary and it is skipped without reading further (rg's binary-skip
	// behavior; the invalid-UTF-8-line placeholder of dsh is not ported).
	head := make([]byte, 8192)
	n, readErr := f.Read(head)
	if readErr != nil && readErr != io.EOF {
		return nil, nil
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil
	}

	var hits []Hit
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return hits, ctxErr
		}
		line++
		text := strings.TrimRight(scanner.Text(), "\r\n")
		if re.MatchString(text) {
			hits = append(hits, Hit{Path: path, Line: line, Text: text})
			if len(hits) >= remaining {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A mid-file read error (e.g. an over-long line) keeps the partial
		// hits and does not abort the whole search.
		return hits, nil
	}
	return hits, nil
}

// pathGlobRE translates a dsh-style path glob into an anchored regexp matched
// against a slash-normalized path RELATIVE TO THE SEARCH ROOT (the rg --glob
// semantics dsh's grep include and glob tool use). A pattern with no "/"
// matches basenames at any depth, so "*.go" finds every .go file in the tree;
// a pattern with "/" is anchored at the root. "**" matches across segments
// ("**/" also matches zero segments), "*" and "?" stay within one segment,
// and "{a,b}" expands to alternation.
func pathGlobRE(pattern string) (*regexp.Regexp, error) {
	trans, err := translateGlob(pattern)
	if err != nil {
		return nil, err
	}
	if strings.Contains(pattern, "/") {
		return regexp.Compile("^" + trans + "$")
	}
	return regexp.Compile("(?:^|.*/)" + trans + "$")
}

// translateGlob converts one glob into its regexp body. "*" → [^/]*,
// "?" → [^/], "**" → .* (and "**/" → (?:.*/)?), "{a,b}" → (?:a|b) with each
// part translated recursively, everything else literal. An unbalanced brace
// fails closed.
func translateGlob(pattern string) (string, error) {
	var sb strings.Builder
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
		case '{':
			end := strings.IndexByte(pattern[i+1:], '}')
			if end < 0 {
				return "", fmt.Errorf("unbalanced braces in %q", pattern)
			}
			inner := pattern[i+1 : i+1+end]
			if strings.Contains(inner, "{") {
				return "", fmt.Errorf("nested brace alternation is not supported in %q", pattern)
			}
			parts := strings.Split(inner, ",")
			translated := make([]string, 0, len(parts))
			for _, part := range parts {
				t, err := translateGlob(part)
				if err != nil {
					return "", err
				}
				translated = append(translated, t)
			}
			sb.WriteString("(?:" + strings.Join(translated, "|") + ")")
			i += end + 2
			continue
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
		i++
	}
	return sb.String(), nil
}
