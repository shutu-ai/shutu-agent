package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeedbackAnonymousIdentityIsLazyAndStablePerDataDir(t *testing.T) {
	dataDir := t.TempDir()
	first := &app{}
	first.cfg.DataDir = dataDir
	if _, err := os.Stat(filepath.Join(dataDir, anonymousFeedbackIdentityFile)); !os.IsNotExist(err) {
		t.Fatalf("identity file exists before first use: %v", err)
	}
	id, err := first.feedbackAnonymousUserID()
	if err != nil {
		t.Fatal(err)
	}
	if !validAnonymousFeedbackID(id) {
		t.Fatalf("invalid generated id %q", id)
	}

	second := &app{}
	second.cfg.DataDir = dataDir
	got, err := second.feedbackAnonymousUserID()
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("reloaded identity = %q, want %q", got, id)
	}
}
