package spill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileProviderPersistsAcrossOpenAndKeepsCreatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	first, err := NewFileProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC)
	memo := Memo{ID: "memo-1", Content: "user prefers Go", Source: "session:4", CreatedAt: created}
	if _, err := first.Add(context.Background(), memo); err != nil {
		t.Fatal(err)
	}

	second, err := NewFileProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Get(context.Background(), memo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != memo {
		t.Fatalf("reopened memo = %+v, want %+v", got, memo)
	}
	if err := second.Delete(context.Background(), memo.ID); err != nil {
		t.Fatal(err)
	}

	third, err := NewFileProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Get(context.Background(), memo.ID); !errors.Is(err, ErrUnknownMemo) {
		t.Fatalf("deleted memo error = %v, want ErrUnknownMemo", err)
	}
}

func TestFileProviderRejectsMalformedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileProvider(path); err == nil {
		t.Fatal("malformed store opened successfully")
	}
}
