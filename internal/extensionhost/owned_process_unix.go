//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package extensionhost

import (
	"fmt"
	"os/exec"
	"syscall"
)

type unixOwnedProcess struct{ pgid int }

func prepareOwnedProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func attachOwnedProcess(cmd *exec.Cmd) (ownedProcess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("extension: process tree requires a started process")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return nil, fmt.Errorf("extension: get process group: %w", err)
	}
	return &unixOwnedProcess{pgid: pgid}, nil
}

func (p *unixOwnedProcess) Close() error {
	if p == nil || p.pgid <= 0 || p.pgid == syscall.Getpid() {
		return nil
	}
	if err := syscall.Kill(-p.pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
