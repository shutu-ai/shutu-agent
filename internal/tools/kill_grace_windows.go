//go:build windows

package tools

import (
	"os/exec"
	"strconv"
	"time"
)

// terminateTree asks Windows to close the tree without /F, waits for the
// configured grace, then falls back to the Job Object/descendant force kill.
// monitorCtx's caller owns cmd.Wait; this function only stages termination.
func terminateTree(cmd *exec.Cmd, graceMS int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if graceMS > 0 {
		graceful := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T")
		_ = graceful.Run()
		time.Sleep(time.Duration(graceMS) * time.Millisecond)
	}
	killTree(cmd)
}
