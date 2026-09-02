//go:build windows

package store

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// sqliteProcessLock serializes write transactions across independent Agent
// processes. The lock is held by an open handle, so a crashed process releases
// it without leaving a stale marker behind.
type sqliteProcessLock struct{ file *os.File }

func acquireSQLiteProcessLock(path string) (*sqliteProcessLock, error) {
	return acquireSQLiteProcessLockContext(context.Background(), path)
}

func acquireSQLiteProcessLockContext(ctx context.Context, path string) (*sqliteProcessLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return &sqliteProcessLock{file: file}, nil
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

func (l *sqliteProcessLock) Close() error {
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
