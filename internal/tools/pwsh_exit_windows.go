//go:build windows

package tools

// exitSignalName reports no signal on Windows: a force-killed command
// settles as [exit code: 1] without a signal marker (dsh: treat a bare exit 1
// after an interruption as a termination, not a command failure).
func exitSignalName(error) (string, bool) { return "", false }
