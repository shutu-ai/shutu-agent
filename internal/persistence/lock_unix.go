//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package persistence

import (
	"context"
	"os"
	"syscall"
	"time"
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
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &processLock{file: file}, nil
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
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
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
