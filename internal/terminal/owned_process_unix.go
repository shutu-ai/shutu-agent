//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package terminal

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

type ownedProcess struct{ pgid int }

func prepareOwnedProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func attachOwnedProcess(cmd *exec.Cmd) (*ownedProcess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("terminal: process tree requires a started process")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return nil, fmt.Errorf("terminal: get process group: %w", err)
	}
	return &ownedProcess{pgid: pgid}, nil
}

func (o *ownedProcess) interrupt() error {
	if o == nil || o.pgid <= 0 || o.pgid == syscall.Getpid() {
		return errors.New("terminal: no interruptible process group")
	}
	return syscall.Kill(-o.pgid, syscall.SIGINT)
}

func terminateOwnedProcess(owner *ownedProcess, cmd *exec.Cmd) {
	if owner != nil && owner.pgid > 0 && owner.pgid != syscall.Getpid() {
		_ = syscall.Kill(-owner.pgid, syscall.SIGKILL)
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
