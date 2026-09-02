//go:build !windows

package code

// The interface is declared in windows_acl_contract.go; this file supplies
// the no-op platform selection only.

func windowsACLBackendAvailable() bool { return false }

func prepareWindowsACLRun(_ SandboxMode, _ string) (windowsACLHandle, error) {
	return nil, nil
}

func RecoverWindowsACLSandboxes() error { return nil }
