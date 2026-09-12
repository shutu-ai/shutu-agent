//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package extensionhost

import (
	"os"
	"syscall"
)

func processStillAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func terminateTestProcess(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
