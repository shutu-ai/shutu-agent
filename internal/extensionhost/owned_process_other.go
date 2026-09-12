//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package extensionhost

import (
	"fmt"
	"os/exec"
)

type fallbackOwnedProcess struct{ cmd *exec.Cmd }

func prepareOwnedProcess(_ *exec.Cmd) {}

func attachOwnedProcess(cmd *exec.Cmd) (ownedProcess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("extension: process tree requires a started process")
	}
	return &fallbackOwnedProcess{cmd: cmd}, nil
}

func (p *fallbackOwnedProcess) Close() error {
	if p != nil && p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}
