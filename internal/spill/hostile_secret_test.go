package spill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpillFileRedactsHostileCredentialDiagnostics covers the durable memory
// egress boundary used by both explicit spill_write and AutoSpill.
func TestSpillFileRedactsHostileCredentialDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	provider, err := NewFileProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(provider)
	memo, err := engine.Spill(context.Background(),
		"failed after OPENAI_API_KEY=hostile-secret-value and Authorization: Bearer hostile-bearer-value",
		"hostile-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(memo.Content, "hostile-secret-value") || strings.Contains(memo.Content, "hostile-bearer-value") {
		t.Fatalf("returned memo leaked credential: %q", memo.Content)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hostile-secret-value") || strings.Contains(string(raw), "hostile-bearer-value") {
		t.Fatalf("spill file leaked credential: %s", raw)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	reopenedEngine := NewEngine(reopened)
	defer reopenedEngine.Close()
	memos, err := reopened.List(context.Background())
	if err != nil || len(memos) != 1 {
		t.Fatalf("reopened memos = %#v, %v; want one", memos, err)
	}
	if strings.Contains(memos[0].Content, "hostile-secret-value") || strings.Contains(memos[0].Content, "hostile-bearer-value") {
		t.Fatalf("restarted memory leaked credential: %q", memos[0].Content)
	}
}
