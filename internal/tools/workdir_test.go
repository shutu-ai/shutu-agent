package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/pathsecure"
)

func TestConstrainWorkdirRejectsOutsideAndAcceptsChild(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := constrainWorkdir(child, root)
	if err != nil {
		t.Fatalf("constrain child: %v", err)
	}
	want, err := pathsecure.ResolveExisting(child)
	if err != nil {
		t.Fatalf("resolve expected child: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("constrained child = %q, want %q", got, child)
	}
	if _, err := constrainWorkdir(filepath.Dir(root), root); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("outside workdir error = %v, want workspace escape", err)
	}
}
