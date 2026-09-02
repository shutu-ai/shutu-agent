//go:build !windows

package jobs

import (
	"os/exec"
	"syscall"
)

// prepareJobProcessGroup starts the command in its own process group so the
// whole group (the shell and every child it spawns) can be signalled together.
func prepareJobProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachJobProcessGroup(_ *exec.Cmd) {}

func releaseJobProcessGroup(_ *exec.Cmd) {}

// killJobTree signals the whole process group with SIGKILL, stopping the shell
// and any grandchild (e.g. sleep) that would otherwise keep the pipes open.
func killJobTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// A negative pid targets the process group (Setpgid above).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
