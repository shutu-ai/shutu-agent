//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package extensionhost

import "os"

func processStillAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process != nil
}

func terminateTestProcess(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
