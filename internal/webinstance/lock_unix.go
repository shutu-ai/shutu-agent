//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package webinstance

import (
	"os"
	"syscall"
)

func tryLockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func isLockConflict(err error) bool {
	return err == syscall.EWOULDBLOCK || err == syscall.EAGAIN
}
