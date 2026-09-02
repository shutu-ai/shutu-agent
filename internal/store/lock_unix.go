//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package store

import (
	"context"
	"os"
	"syscall"
	"time"
)

// sqliteProcessLock serializes write transactions across independent Agent
// processes. SQLite already protects pages, but an explicit lock also makes
// the session event append coordinator deterministic when two hosts share the
// same database path.
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
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &sqliteProcessLock{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
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
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
