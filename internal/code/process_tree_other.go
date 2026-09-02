//go:build !windows && !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package code

import "os/exec"

// processTree is the platform hook used by the local provider after the
// child has been started. Unix providers currently rely on the subprocess
// backend's existing process-group behavior; keeping the hook explicit makes
// the lifecycle contract visible and lets Windows use a Job Object without
// weakening other platforms' build surface.
type processTree struct{}

func prepareProcessTree(_ *exec.Cmd) {}

func attachProcessTree(_ *exec.Cmd, _ processTreeLimits) (processTree, error) {
	return processTree{}, nil
}

func (processTree) Close() error { return nil }
