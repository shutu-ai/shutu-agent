//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

// preventOSCoreDumps removes permission for the kernel to write a core image.
// RLIMIT_CORE is process-local, needs no extra privilege, and is inherited by
// subprocesses unless a child deliberately raises it (subject to its hard cap).
func preventOSCoreDumps() error {
	limit := syscall.Rlimit{Cur: 0, Max: 0}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		return fmt.Errorf("crash dump policy: set RLIMIT_CORE=0: %w", err)
	}
	return nil
}
