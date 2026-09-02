//go:build !windows

package terminal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloseTerminatesForegroundDescendant(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	s, err := NewSession(SessionOpts{Shell: "/bin/sh", Workdir: t.TempDir(), IdleMS: 50, TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("sleep 1; echo escaped > "+marker, true); err != nil {
		t.Fatalf("write descendant command: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived terminal close; stat err=%v", err)
	}
}
