package webinstance

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestTryAcquireIsExclusiveAndReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-only.lock")
	first, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TryAcquire(path)
	if !errors.Is(err, ErrAlreadyRunning) || second != nil {
		t.Fatalf("second acquire = %#v, %v; want ErrAlreadyRunning", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
