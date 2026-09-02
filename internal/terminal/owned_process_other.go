//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package terminal

import (
	"errors"
	"os/exec"
)

type ownedProcess struct{}

func prepareOwnedProcess(_ *exec.Cmd) {}

func attachOwnedProcess(_ *exec.Cmd) (*ownedProcess, error) { return &ownedProcess{}, nil }

func (o *ownedProcess) interrupt() error { return errors.New("terminal: interrupt unsupported") }

func terminateOwnedProcess(_ *ownedProcess, cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
