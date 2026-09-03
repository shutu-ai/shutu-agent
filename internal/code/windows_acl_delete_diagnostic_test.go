//go:build windows

package code

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsACLDeleteHelperEnv = "SHUTU_ACL_DELETE_HELPER_TARGET"
	windowsACLDeleteKindEnv   = "SHUTU_ACL_DELETE_HELPER_KIND"
)

func TestWindowsACLNativeDeleteHelper(t *testing.T) {
	target := os.Getenv(windowsACLDeleteHelperEnv)
	kind := os.Getenv(windowsACLDeleteKindEnv)
	if target == "" || kind == "" {
		t.Skip("Windows ACL native delete diagnostic helper")
	}
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		fmt.Printf("WIN32_DELETE operation=UTF16 path=%q result=FAIL errno=%d message=%q\n", target, win32ErrorCode(err), err.Error())
		os.Exit(121)
	}
	attrs, attrErr := windows.GetFileAttributes(path)
	if attrErr != nil {
		fmt.Printf("WIN32_DELETE operation=GetFileAttributesW path=%q result=FAIL errno=%d message=%q\n", target, win32ErrorCode(attrErr), attrErr.Error())
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
	var deleteErr error
	var operation string
	switch kind {
	case "file":
		operation = "DeleteFileW"
		deleteErr = windows.DeleteFile(path)
	case "directory":
		operation = "RemoveDirectoryW"
		deleteErr = windows.RemoveDirectory(path)
	default:
		fmt.Printf("WIN32_DELETE operation=InvalidKind path=%q result=FAIL errno=0 message=%q\n", target, kind)
		os.Exit(123)
	}
	if deleteErr != nil {
		fmt.Printf("WIN32_DELETE operation=%s path=%q result=FAIL errno=%d message=%q\n", operation, target, win32ErrorCode(deleteErr), deleteErr.Error())
		os.Exit(124)
	}
	fmt.Printf("WIN32_DELETE operation=%s path=%q result=PASS errno=0\n", operation, target)
}

func win32ErrorCode(err error) uint32 {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	const unusable = 0xffffffff
	return unusable
}

func TestWindowsACLNativeDeleteMatrix(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := newLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create cwd: %v", err)
	}
	helperBinary := buildWindowsACLDeleteHelper(t, cwd)
	t.Logf("helper ACL: %s", aclSDDL(t, helperBinary))
	t.Logf("helper parent ACL: %s", aclSDDL(t, filepath.Dir(helperBinary)))
	run := func(stage, command string) Result {
		t.Helper()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		if err != nil {
			t.Fatalf("%s transport error: %v", stage, err)
		}
		t.Logf("%s: exit=%d stdout=%q stderr=%q", stage, result.ExitCode, result.Stdout, result.Stderr)
		if result.ExitCode != 0 {
			t.Fatalf("%s failed", stage)
		}
		return result
	}
	nativeDelete := func(stage, kind, path string) {
		t.Helper()
		t.Setenv(windowsACLDeleteKindEnv, kind)
		t.Setenv(windowsACLDeleteHelperEnv, path)
		p.diagnosticArgv = []string{helperBinary}
		defer func() { p.diagnosticArgv = nil }()
		helper := "native-delete-helper"
		run(stage, helper)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: target remains: stat=%v", stage, err)
		}
	}

	fileTarget := filepath.Join(cwd, "renamed-native-file.txt")
	run("create file", "echo data>native-file.txt")
	run("rename file", "move /Y native-file.txt renamed-native-file.txt")
	nativeDelete("DeleteFileW workspace", "file", fileTarget)

	dirTarget := filepath.Join(cwd, "native-dir")
	run("create directory", "mkdir native-dir")
	nativeDelete("RemoveDirectoryW workspace", "directory", dirTarget)
}

func grantNativeHelperExecute(t *testing.T, helperBinary string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("create helper Everyone SID: %v", err)
	}
	if err := editWindowsACL(helperBinary, everyone, windows.GRANT_ACCESS, 0x001200A9); err != nil {
		t.Fatalf("grant helper execute: %v", err)
	}
}

func buildWindowsACLDeleteHelper(t *testing.T, dir string) string {
	t.Helper()
	helperBinary := filepath.Join(dir, "windowsacldeletehelper.exe")
	if out, err := exec.Command("go", "build", "-o", helperBinary, "./windowsacldeletehelper").CombinedOutput(); err != nil {
		t.Fatalf("build native helper: %v: %s", err, out)
	}
	grantNativeHelperExecute(t, helperBinary)
	return helperBinary
}

func TestWindowsACLNativeDeleteDenialMatrix(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	p := newLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create cwd: %v", err)
	}
	helperBinary := buildWindowsACLDeleteHelper(t, cwd)
	fileTarget := filepath.Join(root, "read.txt")
	if err := os.WriteFile(fileTarget, []byte("read-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirTarget := filepath.Join(root, "existing-dir")
	if err := os.Mkdir(dirTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeDeniedDelete := func(stage, kind, path string) {
		t.Helper()
		t.Setenv(windowsACLDeleteKindEnv, kind)
		t.Setenv(windowsACLDeleteHelperEnv, path)
		p.diagnosticArgv = []string{helperBinary}
		defer func() { p.diagnosticArgv = nil }()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxReadOnly, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: "native-delete-helper",
		})
		if err != nil {
			t.Fatalf("%s transport error: %v", stage, err)
		}
		t.Logf("%s: exit=%d stdout=%q stderr=%q", stage, result.ExitCode, result.Stdout, result.Stderr)
		if result.ExitCode == 0 || !strings.Contains(result.Stdout, "result=FAIL errno=5") {
			t.Fatalf("%s: expected ERROR_ACCESS_DENIED", stage)
		}
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Fatalf("%s: target was removed", stage)
		}
	}
	nativeDeniedDelete("readonly DeleteFileW", "file", fileTarget)
	nativeDeniedDelete("readonly RemoveDirectoryW", "directory", dirTarget)

	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second, Code: "ren ..\\read.txt renamed.txt",
	})
	if err != nil {
		t.Fatalf("readonly rename transport error: %v", err)
	}
	t.Logf("readonly rename: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	if result.ExitCode == 0 {
		t.Fatalf("readonly rename unexpectedly succeeded")
	}

	outsideBase := filepath.Dir(root)
	outsideFile := filepath.Join(outsideBase, "native-outside-file.txt")
	outsideDir := filepath.Join(outsideBase, "native-outside-dir")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := secureWindowsACLTestRoot(t, true)
	outsideCwd := filepath.Join(workspaceRoot, "cwd")
	if err := os.MkdirAll(outsideCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideHelper := buildWindowsACLDeleteHelper(t, outsideCwd)
	outsideP := newLocalProvider()
	defer outsideP.Close()
	outsideDelete := func(stage, kind, path string) {
		t.Helper()
		t.Setenv(windowsACLDeleteKindEnv, kind)
		t.Setenv(windowsACLDeleteHelperEnv, path)
		t.Setenv("SHUTU_ACL_DELETE_HELPER_SKIP_ATTRIBUTES", "1")
		outsideP.diagnosticArgv = []string{outsideHelper}
		defer func() { outsideP.diagnosticArgv = nil }()
		result, err := outsideP.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: workspaceRoot, Cwd: outsideCwd,
			Timeout: 5 * time.Second, Code: "native-delete-helper",
		})
		if err != nil {
			t.Fatalf("%s transport error: %v", stage, err)
		}
		t.Logf("%s: exit=%d stdout=%q stderr=%q", stage, result.ExitCode, result.Stdout, result.Stderr)
		if result.ExitCode == 0 || !strings.Contains(result.Stdout, "result=FAIL errno=5") {
			t.Fatalf("%s: expected ERROR_ACCESS_DENIED", stage)
		}
	}
	outsideDelete("outside DeleteFileW", "file", outsideFile)
	outsideDelete("outside RemoveDirectoryW", "directory", outsideDir)
}

func TestWindowsACLDeleteShellWrappers(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create cwd: %v", err)
	}
	run := func(stage, command string) Result {
		t.Helper()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		if err != nil {
			t.Fatalf("%s transport error: %v", stage, err)
		}
		t.Logf("%s: exit=%d stdout=%q stderr=%q", stage, result.ExitCode, result.Stdout, result.Stderr)
		return result
	}
	run("create shell file", "echo data>shell-file.txt")
	result := run("PowerShell Remove-Item", `powershell -NoProfile -ExecutionPolicy Bypass -Command "Remove-Item -LiteralPath shell-file.txt"`)
	_, shellFileStat := os.Stat(filepath.Join(cwd, "shell-file.txt"))
	t.Logf("PowerShell wrapper classification: result=%s targetExists=%v", wrapperResult(result), !os.IsNotExist(shellFileStat))

	run("create cmd file", "echo data>cmd-file.txt")
	result = run("cmd delete", "del /f /q .\\cmd-file.txt")
	_, cmdFileStat := os.Stat(filepath.Join(cwd, "cmd-file.txt"))
	t.Logf("cmd wrapper classification: result=%s targetExists=%v", wrapperResult(result), !os.IsNotExist(cmdFileStat))
}

func wrapperResult(result Result) string {
	if result.ExitCode == 0 {
		return "PASS"
	}
	return fmt.Sprintf("FAIL exit=%d", result.ExitCode)
}
