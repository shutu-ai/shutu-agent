//go:build windows

package code

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const windowsACLDeleteHelperEnv = "SHUTU_ACL_DELETE_HELPER"

func TestWindowsACLDeleteHelper(t *testing.T) {
	target := os.Getenv(windowsACLDeleteHelperEnv)
	if target == "" {
		t.Skip("ACL delete diagnostic helper")
	}
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		fmt.Printf("UTF16_ERROR=%d\n", win32ErrorCode(err))
		os.Exit(121)
	}
	attrs, err := windows.GetFileAttributes(path)
	if err != nil {
		fmt.Printf("GET_ATTRIBUTES_ERROR=%d\n", win32ErrorCode(err))
		os.Exit(122)
	}
	fmt.Printf("ATTRIBUTES=0x%08x READONLY=%v HIDDEN=%v SYSTEM=%v REPARSE=%v DIRECTORY=%v\n",
		attrs,
		attrs&windows.FILE_ATTRIBUTE_READONLY != 0,
		attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0,
		attrs&windows.FILE_ATTRIBUTE_SYSTEM != 0,
		attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0,
		attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
	)
	if err := windows.DeleteFile(path); err != nil {
		code := win32ErrorCode(err)
		fmt.Printf("DELETEFILEW_ERROR=%d\n", code)
		switch code {
		case 5:
			os.Exit(125)
		case 32:
			os.Exit(126)
		case 2:
			os.Exit(127)
		default:
			os.Exit(123)
		}
	}
	fmt.Println("DELETEFILEW=SUCCESS")
}

func win32ErrorCode(err error) uint32 {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	const unusable = 0xffffffff
	return unusable
}

func TestWindowsACLDeleteFileWDiagnostic(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create cwd: %v", err)
	}
	target := filepath.Join(cwd, "renamed.txt")
	run := func(stage, command string) Result {
		t.Helper()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		if err != nil {
			t.Fatalf("%s transport error: %v", stage, err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("%s failed: stdout=%q stderr=%q result=%+v", stage, result.Stdout, result.Stderr, result)
		}
		return result
	}
	run("create", "echo data>target.txt")
	run("rename", "move /Y target.txt renamed.txt")
	run("powershell-smoke", "powershell -NoProfile -Command Write-Output PS-SMOKE")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("renamed target missing: %v", err)
	}

	targetLiteral := strings.ReplaceAll(target, "'", "''")
	powerShell := fmt.Sprintf(
		`try { [System.IO.File]::Delete('%s'); Write-Output 'DELETEFILEW=SUCCESS' } catch { $code = $_.Exception.HResult -band 0xffff; Write-Output ('DELETEFILEW_ERROR=' + $code); Write-Output ('MESSAGE=' + $_.Exception.Message) }`,
		targetLiteral,
	)
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second,
		Code:    fmt.Sprintf("powershell -NoProfile -ExecutionPolicy Bypass -Command %s", windowsCommandQuote(powerShell)),
	})
	t.Logf("DeleteFileW diagnostic: stdout=%q stderr=%q result=%+v", result.Stdout, result.Stderr, result)
	if err != nil {
		t.Fatalf("helper transport error: %v", err)
	}
	if strings.Contains(result.Stdout, "DELETEFILEW=SUCCESS") {
		return
	}
	if _, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("DeleteFileW reported failure but target was removed: stdout=%q", result.Stdout)
	}
	t.Fatalf("DeleteFileW failed; stdout=%q stderr=%q", result.Stdout, result.Stderr)
}
