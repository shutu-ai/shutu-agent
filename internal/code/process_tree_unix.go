//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package code

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Unix process groups give the provider an owned tree even when the shell
// backgrounds a helper. The group is killed only after the direct child has
// settled, so ordinary output and exit status are preserved while descendants
// cannot survive provider teardown.
type processTree struct{ pgid int }

func prepareProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func attachProcessTree(cmd *exec.Cmd, _ processTreeLimits) (processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return processTree{}, fmt.Errorf("code: process tree requires a started process")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return processTree{}, fmt.Errorf("code: get process group: %w", err)
	}
	return processTree{pgid: pgid}, nil
}

func (tree processTree) Close() error {
	if tree.pgid <= 0 {
		return nil
	}
	// The provider owns this process group. A negative pid addresses the group;
	// never target our own group if a platform failed to honor Setpgid.
	if tree.pgid == syscall.Getpid() {
		return nil
	}
	if err := syscall.Kill(-tree.pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
