//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func main() {
	fmt.Printf("HELPER_STARTED pid=%d\n", os.Getpid())
	target := os.Getenv("SHUTU_ACL_DELETE_HELPER_TARGET")
	kind := os.Getenv("SHUTU_ACL_DELETE_HELPER_KIND")
	if target == "" || kind == "" {
		os.Exit(120)
	}
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		report("UTF16", target, err)
		os.Exit(121)
	}
	if os.Getenv("SHUTU_ACL_DELETE_HELPER_SKIP_ATTRIBUTES") != "1" {
		attrs, err := windows.GetFileAttributes(path)
		if err != nil {
			report("GetFileAttributesW", target, err)
			os.Exit(122)
		}
		fmt.Printf("TARGET_STATE path=%q attributes=0x%08x readonly=%v hidden=%v system=%v reparse=%v directory=%v\n",
			target, attrs,
			attrs&windows.FILE_ATTRIBUTE_READONLY != 0,
			attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0,
			attrs&windows.FILE_ATTRIBUTE_SYSTEM != 0,
			attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0,
			attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
		)
	}
	operation := "DeleteFileW"
	switch kind {
	case "file":
		err = windows.DeleteFile(path)
	case "directory":
		operation = "RemoveDirectoryW"
		err = windows.RemoveDirectory(path)
	default:
		os.Exit(123)
	}
	if err != nil {
		report(operation, target, err)
		os.Exit(124)
	}
	fmt.Printf("WIN32_DELETE operation=%s path=%q result=PASS errno=0\n", operation, target)
}

func report(operation, path string, err error) {
	var errno windows.Errno
	code := uint32(0xffffffff)
	if errors.As(err, &errno) {
		code = uint32(errno)
	}
	fmt.Printf("WIN32_DELETE operation=%s path=%q result=FAIL errno=%d message=%q\n", operation, path, code, err.Error())
}
