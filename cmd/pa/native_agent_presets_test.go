package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
)

func TestNativeAgentPresetStoreCopyReadListRemove(t *testing.T) {
	root := t.TempDir()
	prompts := filepath.Join(root, "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "10-persona.md"), []byte("standard persona"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newNativeAgentPresetStore(filepath.Join(root, "data"), prompts, config.ModeStandard)

	catalog, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.Authorable || len(catalog.Presets) != 3 {
		t.Fatalf("initial catalog = %+v", catalog)
	}
	if _, err := store.Copy(context.Background(), config.ModeStandard, "my-preset", "My preset"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	details, err := store.Read(context.Background(), "my-preset")
	if err != nil {
		t.Fatalf("read copied preset: %v", err)
	}
	if details.Trust != "user" || details.Name != "My preset" || details.Content != "standard persona" {
		t.Fatalf("copied details = %+v", details)
	}
	catalog, err = store.List(context.Background())
	if err != nil || len(catalog.Presets) != 4 {
		t.Fatalf("catalog after copy = %+v err=%v", catalog, err)
	}
	if err := store.Remove(context.Background(), config.ModeStandard); err == nil {
		t.Fatal("removing a system preset must fail")
	}
	if err := store.Remove(context.Background(), "my-preset"); err != nil {
		t.Fatalf("remove copied preset: %v", err)
	}
	if _, err := store.Read(context.Background(), "my-preset"); err == nil {
		t.Fatal("removed preset must not be readable")
	}
}

func TestNativeAgentPresetStoreRejectsUnsafeIDsAndBrokenEntries(t *testing.T) {
	store := newNativeAgentPresetStore(t.TempDir(), t.TempDir(), config.ModeStandard)
	if _, err := store.Copy(context.Background(), config.ModeStandard, "../escape", ""); err == nil {
		t.Fatal("path traversal id must fail")
	}
	broken := filepath.Join(store.root, "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, nativeAgentPresetPromptFile), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presets) != 4 || catalog.Presets[3].ID != "broken" || catalog.Presets[3].Broken == "" {
		t.Fatalf("broken preset catalog = %+v", catalog)
	}
}
