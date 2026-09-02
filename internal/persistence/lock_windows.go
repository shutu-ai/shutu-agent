//go:build windows

package persistence

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

type processLock struct {
	file *os.File
}

func acquireProcessLock(path string) (*processLock, error) {
	return acquireProcessLockContext(context.Background(), path)
}

func acquireProcessLockContext(ctx context.Context, path string) (*processLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Lock one byte for the lifetime of the handle. LockFileEx blocks until
	// another process releases the same byte and works across independent
	// service processes, unlike a Go mutex or O_EXCL lock marker.
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return &processLock{file: file}, nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
