//go:build windows

package pathsecure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func isAccessDenied(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func verifyNoReparsePoints(path string) error {
	volume := filepath.VolumeName(path)
	rest := strings.TrimLeft(path[len(volume):], `\\/`)
	current := volume
	if current != "" && rest != "" {
		current += string(filepath.Separator)
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		current = filepath.Join(current, part)
		name, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(name)
		if err != nil {
			return err
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("reparse point at %q", current)
		}
	}
	return nil
}

// fallbackPath expands an 8.3 path when Windows permits the metadata query
// but denies EvalSymlinks. PowerShell can reject the short form as a working
// directory under some managed profiles even though the long form is valid.
// If the lookup is unavailable, retaining the already-verified absolute path
// is safer than failing open or inventing a path.
func fallbackPath(path string) string {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	for size := uint32(260); size <= 32768; {
		buf := make([]uint16, size)
		n, err := windows.GetLongPathName(name, &buf[0], size)
		if err == nil && n > 0 && n < size {
			return filepath.Clean(windows.UTF16ToString(buf[:n]))
		}
		if n < size {
			return path
		}
		size = n + 1
	}
	return path
}
