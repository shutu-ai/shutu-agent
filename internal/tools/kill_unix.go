//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup starts the command in its own process group so the whole
// group (the shell and every child it spawns) can be signalled together.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachProcessGroup(_ *exec.Cmd) {}

func releaseProcessGroup(_ *exec.Cmd) {}

// killTree signals the whole process group with SIGKILL, stopping the shell
// and any grandchild (e.g. sleep) that would otherwise keep the pipes open.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// A negative pid targets the process group (Setpgid above).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
