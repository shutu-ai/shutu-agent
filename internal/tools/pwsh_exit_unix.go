//go:build !windows

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

// exitSignalName reports the terminating signal of an ExitError on POSIX
// platforms (the dsh [killed by signal: N] marker): a process killed by the
// process-group SIGKILL in killTree settles with a signal, not an exit code.
func exitSignalName(err error) (string, bool) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return "", false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", false
	}
	return ws.Signal().String(), true
}
