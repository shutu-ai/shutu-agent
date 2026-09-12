// Package webinstance provides crash-releasing, non-blocking locks for the
// web-only Agent instance. The lock is advisory only; the bound listener is
// the final authority for address ownership.
package webinstance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrAlreadyRunning = errors.New("web-only instance is already running")

type Lock struct {
	file *os.File
	once sync.Once
}

func TryAcquire(path string) (*Lock, error) {
	if path == "" {
		return nil, errors.New("web-only lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create web-only lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open web-only lock: %w", err)
	}
	if err := tryLockFile(file); err != nil {
		_ = file.Close()
		if isLockConflict(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock web-only instance: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	var first error
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		if err := unlockFile(l.file); err != nil {
			first = err
		}
		if err := l.file.Close(); err != nil && first == nil {
			first = err
		}
		l.file = nil
	})
	return first
}
