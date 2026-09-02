// Package pathsecure contains the shared existing-path resolver used by
// workspace-bound tools. Windows hosts can deny Go's EvalSymlinks even for a
// normal directory (for example under a managed temporary directory). In
// that narrow case the Windows implementation falls back only after checking
// every path component for a reparse point.
package pathsecure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolveExisting returns an absolute, cleaned path with links resolved. The
// target must already exist. A Windows access-denied EvalSymlinks result may
// use the platform fallback, but only when no component is a reparse point.
func ResolveExisting(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		return filepath.Clean(resolved), nil
	} else if !isAccessDenied(resolveErr) {
		return "", resolveErr
	} else if err := verifyNoReparsePoints(abs); err != nil {
		return "", fmt.Errorf("resolve %q: %w", abs, err)
	}
	if info == nil {
		return "", errors.New("path information unavailable")
	}
	return fallbackPath(abs), nil
}
