package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/store"
)

func TestRuntimeContextForUsesRequestedSessionWorkspace(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "target-workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	st, err := store.OpenSQLite(filepath.Join(root, "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.CreateWorkspaceWithPath(ctx, "ws-target", "Target", workspacePath); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := st.CreateSession(ctx, "target", time.Now()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.SetSessionWorkspace(ctx, "target", "ws-target"); err != nil {
		t.Fatalf("set session workspace: %v", err)
	}

	a := &app{store: st, currentID: "different-active-session"}
	messages := a.runtimeContextFor(ctx, "target")
	if len(messages) != 1 {
		t.Fatalf("runtime context messages = %d, want 1", len(messages))
	}
	if got := messages[0].Text(); !strings.Contains(got, filepath.Clean(workspacePath)) {
		t.Fatalf("runtime context = %q, want target workspace %q", got, workspacePath)
	}
	if strings.Contains(messages[0].Text(), "different-active-session") {
		t.Fatal("runtime context must not use the process-global current session")
	}
	if messages[0].SourceKind != "plugin" || messages[0].SourcePlugin != "@shutu-ai/system-prompt" ||
		messages[0].SourceForm != "snapshot" {
		t.Fatalf("runtime source = %q/%q/%q, want plugin/@shutu-ai/system-prompt/snapshot",
			messages[0].SourceKind, messages[0].SourcePlugin, messages[0].SourceForm)
	}
	if len(messages[0].SourceSections) != 1 || messages[0].SourceSections[0].Name != "workspace" ||
		messages[0].SourceSections[0].Text != strings.SplitN(messages[0].Text(), "\n\n", 2)[1] {
		t.Fatalf("runtime sections = %#v, want one exact workspace section", messages[0].SourceSections)
	}
}
