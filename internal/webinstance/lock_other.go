//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package webinstance

import (
	"errors"
	"os"
)

func tryLockFile(*os.File) error {
	return errors.New("platform does not support crash-releasing web-only locks")
}
func unlockFile(*os.File) error { return nil }
func isLockConflict(error) bool { return false }
