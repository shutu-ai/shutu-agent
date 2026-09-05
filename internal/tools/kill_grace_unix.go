//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// terminateTree sends the DSH-compatible graceful first tier, waits for the
// configured grace, then forces the process group to quiescence. monitorCtx's
// caller owns cmd.Wait; signal delivery is best-effort for already-exited pids.
func terminateTree(cmd *exec.Cmd, graceMS int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if graceMS > 0 {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(time.Duration(graceMS) * time.Millisecond)
	}
	killTree(cmd)
}
