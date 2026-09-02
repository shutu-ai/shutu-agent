package code

import "os/exec"

// windowsACLHandle is the optional Windows file-effect backend. Keeping the
// seam available on every host makes platform selection fail closed without
// scattering build tags through the execution path.
type windowsACLHandle interface {
	configure(*exec.Cmd) error
	close() error
}
