//go:build !windows

package pathsecure

import "os"

func isAccessDenied(err error) bool { return os.IsPermission(err) }

func verifyNoReparsePoints(string) error { return nil }

func fallbackPath(path string) string { return path }
