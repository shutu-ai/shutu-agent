package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jabing/shutu-agent/internal/pathsecure"
)

// constrainWorkdir applies a host-supplied workspace authority to a resolved
// command directory. Both paths are symlink-resolved so an in-workspace link
// cannot redirect a process outside the session workspace.
func constrainWorkdir(workdir, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return filepath.Clean(workdir), nil
	}
	root, err := resolvedExistingDirectory(root, "workspace root")
	if err != nil {
		return "", err
	}
	workdir, err = resolvedExistingDirectory(workdir, "workdir")
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, workdir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("workdir %q escapes workspace root", workdir)
	}
	return workdir, nil
}

func resolvedExistingDirectory(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s %q is unavailable: %w", label, path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", label, path)
	}
	resolved, err := pathsecure.ResolveExisting(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return filepath.Clean(resolved), nil
}
