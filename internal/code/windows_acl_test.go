//go:build windows

package code

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/jabing/shutu-agent/internal/pathsecure"
	"golang.org/x/sys/windows"
)

func secureWindowsACLTestRoot(t *testing.T, writable bool) string {
	t.Helper()
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(workdir, ".audit-acl-workspaces")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(parent, "test-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = pathsecure.ResolveExisting(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "root")
	var capability *windows.SID
	if writable {
		capability, err = windows.StringToSid(workspaceWriteSID(root))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := createWindowsSecureDirectory(root, capability, !writable); err != nil {
		t.Fatalf("create capability-granted test workspace: %v", err)
	}
	if writable {
		sd, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		has, err := windowsDACLGrants(sd, capability, windows.ACCESS_MASK(grantMask))
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("capability grant missing: sid=%s sddl=%s", capability.String(), sd.String())
		}
	}
	if user := windowsCurrentUser(); user != nil {
		sd, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		has, err := windowsDACLGrants(sd, user, windows.ACCESS_MASK(fileAllAccess))
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("host user ACE missing: user=%s", user.String())
		}
	}
	return root
}

func requireWindowsACL(t *testing.T) {
	t.Helper()
	p := newLocalProvider()
	t.Cleanup(func() { _ = p.Close() })
	if !hasSandboxMode(p.Capabilities(), SandboxWorkspaceWrite) {
		t.Skipf("Windows ACL containment backend is unavailable on this host: %v", windowsACLProbe())
	}
}

func aclBytesForPath(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	raw, protected, err := windowsACLBytes(sd)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), raw...), protected
}

func aclSDDL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func windowsSecurityDescriptorBytesForPath(t *testing.T, path string) []byte {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, sd.Length())
	copy(raw, unsafe.Slice((*byte)(unsafe.Pointer(sd)), len(raw)))
	runtime.KeepAlive(sd)
	return raw
}

func windowsSecurityDescriptorControlForPath(t *testing.T, path string) uint16 {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	return uint16(control)
}

func windowsACLJournalControlForPath(t *testing.T, path string) uint16 {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, err := windowsACLControl(sd)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func TestWindowsACLBytesHeapIntegrity(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	t.Logf("01 fixture created path=%s", root)
	for index := 0; index < 100; index++ {
		raw, protected := aclBytesForPath(t, root)
		if len(raw) == 0 {
			t.Fatalf("iteration %d produced an empty DACL", index)
		}
		if index%10 == 0 {
			t.Logf("02 snapshot iteration=%d bytes=%d protected=%v", index, len(raw), protected)
		}
	}
	t.Log("03 snapshot loop completed")
}

func TestWindowsACLRestoreControlSetFileSecurityDiagnostic(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	before, protected := aclBytesForPath(t, root)
	beforeControl := windowsSecurityDescriptorControlForPath(t, root)
	beforeDACLControl := windowsACLJournalControlForPath(t, root)
	capability, err := windows.StringToSid(workspaceWriteSID(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := grantWindowsACL(root, capability); err != nil {
		t.Fatal(err)
	}
	if err := restoreWindowsACLBytes(root, before, protected, beforeDACLControl); err != nil {
		t.Fatal(err)
	}
	restoredControl := windowsSecurityDescriptorControlForPath(t, root)
	t.Logf("named-security restore control before=0x%04x after=0x%04x", beforeControl, restoredControl)

	current, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	current, err = current.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	owner, ownerDefaulted, err := current.Owner()
	if err != nil {
		t.Fatal(err)
	}
	group, groupDefaulted, err := current.Group()
	if err != nil {
		t.Fatal(err)
	}
	dacl := (*windows.ACL)(unsafe.Pointer(&before[0]))
	sd, err := newWindowsSecurityDescriptorWithDACL(dacl, protected)
	if err != nil {
		t.Fatal(err)
	}
	if err := sd.SetOwner(owner, ownerDefaulted); err != nil {
		t.Fatal(err)
	}
	if err := sd.SetGroup(group, groupDefaulted); err != nil {
		t.Fatal(err)
	}
	desired := windows.SECURITY_DESCRIPTOR_CONTROL(beforeControl) & (windows.SE_DACL_AUTO_INHERITED | windows.SE_DACL_PROTECTED)
	if err := sd.SetControl(windows.SE_DACL_AUTO_INHERITED|windows.SE_DACL_PROTECTED, desired); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	r1, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("SetFileSecurityW").Call(
		uintptr(unsafe.Pointer(path)), uintptr(windows.DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(sd)),
	)
	if r1 == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.GetLastError()
		}
		t.Fatalf("SetFileSecurityW failed: %v", callErr)
	}
	afterControl := windowsSecurityDescriptorControlForPath(t, root)
	beforeDescriptor := windowsSecurityDescriptorBytesForPath(t, root)
	afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root)
	t.Logf("file-security restore control expected=0x%04x actual=0x%04x descriptorEqual=%v", beforeControl, afterControl, bytes.Equal(beforeDescriptor, afterDescriptor))
	if afterControl != beforeControl {
		t.Fatalf("SetFileSecurityW did not restore control: expected=0x%04x actual=0x%04x", beforeControl, afterControl)
	}
}

func requireFailedRun(t *testing.T, result Result, err error, stage string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected transport error: %v", stage, err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("%s unexpectedly succeeded: %+v", stage, result)
	}
}

func TestWindowsACLWorkspaceRW(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := newLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create readonly cwd: %v; root=%s", err, aclSDDL(t, root))
	}
	if err := os.WriteFile(filepath.Join(root, "read.txt"), []byte("read-ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(stage, command string) Result {
		t.Helper()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		if err != nil {
			t.Fatalf("%s: transport error: %v", stage, err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("%s failed: %+v", stage, result)
		}
		return result
	}
	helperBinary := buildWindowsACLDeleteHelper(t, cwd)
	result := run("read", "type ..\\read.txt")
	if !bytes.Contains([]byte(result.Stdout), []byte("read-ok")) {
		t.Fatalf("read result = %+v", result)
	}
	run("create/write", "echo data>..\\write.txt")
	run("rename", "move /Y ..\\write.txt ..\\renamed.txt")
	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	out, err := exec.Command("icacls", filepath.Join(root, "renamed.txt")).CombinedOutput()
	t.Logf("target ACL: %s err=%v", out, err)
	t.Setenv(windowsACLDeleteKindEnv, "file")
	t.Setenv(windowsACLDeleteHelperEnv, filepath.Join(root, "renamed.txt"))
	p.diagnosticArgv = []string{helperBinary}
	deleteResult, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second, Code: "native DeleteFileW",
	})
	p.diagnosticArgv = nil
	t.Logf("native delete: result=%+v err=%v", deleteResult, err)
	if err != nil || deleteResult.ExitCode != 0 {
		t.Fatalf("DeleteFileW failed: err=%v result=%+v", err, deleteResult)
	}
	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed target remains after DeleteFileW")
	}
	// if _, err := os.Stat(filepath.Join(root, "renamed.txt")); !errors.Is(err, os.ErrNotExist) {
	// 	t.Fatalf("deleted file remains: %v", err)
	// }
	run("mkdir", "mkdir ..\\created")
	if info, err := os.Stat(filepath.Join(root, "created")); err != nil || !info.IsDir() {
		t.Fatalf("created directory missing: %v", err)
	}
	// Workspace delete oracle above intentionally proves delete before the
	// separate directory-create contract below.
}

func TestWindowsACLOutsideAndTraversalContainment(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatalf("create readonly cwd: %v; root=%s", err, aclSDDL(t, root))
	}
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDenied := func(stage, command string) Result {
		t.Helper()
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		requireFailedRun(t, result, err, stage)
		return result
	}
	runDenied("outside create", "echo denied>..\\..\\outside-created.txt")
	runDenied("outside write", "echo denied>"+windowsCommandQuote(outside))
	runDenied("outside rename", "ren "+windowsCommandQuote(outside)+" denied.txt")
	runDenied("outside delete", "del "+windowsCommandQuote(outside))
	runDenied("traversal write", "echo denied>..\\..\\traversal.txt")
	if got, err := os.ReadFile(outside); err != nil || string(got) != "original" {
		t.Fatalf("outside sentinel changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside-created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside created file exists: %v", err)
	}
}

func TestWindowsACLReadOnlyOperations(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	p := newLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "read.txt"), []byte("read-only-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	helperBinary := buildWindowsACLDeleteHelper(t, cwd)
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second, Code: "type ..\\read.txt",
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("read-only read = %+v, err=%v", result, err)
	}
	if !bytes.Contains([]byte(result.Stdout), []byte("read-only-ok")) {
		t.Fatalf("read-only read output = %+v", result)
	}
	for _, command := range []string{
		"echo denied>..\\write.txt",
		"ren ..\\read.txt renamed.txt",
		"mkdir ..\\created",
	} {
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxReadOnly, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		requireFailedRun(t, result, err, "read-only "+command)
	}
	t.Setenv(windowsACLDeleteKindEnv, "file")
	t.Setenv(windowsACLDeleteHelperEnv, filepath.Join(root, "read.txt"))
	p.diagnosticArgv = []string{helperBinary}
	result, err = p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second, Code: "native DeleteFileW",
	})
	p.diagnosticArgv = nil
	if err != nil {
		t.Fatalf("read-only DeleteFileW transport error: %v", err)
	}
	t.Logf("read-only DeleteFileW: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	if result.ExitCode == 0 || !strings.Contains(result.Stdout, "result=FAIL errno=5") {
		t.Fatalf("read-only DeleteFileW did not return ERROR_ACCESS_DENIED: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "read.txt")); err != nil {
		t.Fatalf("read-only file changed/missing: %v", err)
	}
}

func TestWindowsACLJunctionContainment(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, true)
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "junction-target")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	command := fmt.Sprintf(
		"New-Item -ItemType Junction -Path %s -Target %s | Out-Null",
		strconv.Quote(link), strconv.Quote(outside),
	)
	if out, err := exec.Command("powershell", "-NoProfile", "-Command", command).CombinedOutput(); err != nil {
		t.Skipf("junction unavailable: %v: %s", err, out)
	}
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxWorkspaceWrite, Root: root, Cwd: cwd,
		Timeout: 5 * time.Second, Code: "echo denied>..\\link\\escaped.txt",
	})
	requireFailedRun(t, result, err, "junction write")
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("junction target changed: %v", err)
	}
}

func TestWindowsACLTimeoutCancelCleanupAndIdempotence(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Log("01 fixture created")
	before, protected := aclBytesForPath(t, root)
	beforeDescriptor := windowsSecurityDescriptorBytesForPath(t, root)
	t.Logf("02 original descriptor captured bytes=%d protected=%v", len(before), protected)

	t.Log("03 timeout run launching")
	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly, Root: root, Cwd: cwd,
		Timeout: 100 * time.Millisecond, Code: `ping -n 20 127.0.0.1`,
	})
	t.Logf("04 timeout run returned result=%+v err=%v", result, err)
	if err != nil {
		t.Fatalf("timeout run transport error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("timeout result = %+v", result)
	}
	t.Log("05 timeout result verified")
	after, afterProtected := aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("timeout did not restore ACL")
	}
	if afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root); !bytes.Equal(beforeDescriptor, afterDescriptor) {
		t.Fatalf("timeout did not restore the complete security descriptor")
	}
	t.Log("06 timeout descriptor restored/verified")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := p.Run(ctx, RunRequest{
			Mode: SandboxReadOnly, Root: root, Cwd: cwd,
			Timeout: 10 * time.Second, Code: `ping -n 20 127.0.0.1`,
		})
		done <- struct {
			result Result
			err    error
		}{result, err}
	}()
	t.Log("07 cancel run launched")
	time.Sleep(50 * time.Millisecond)
	cancel()
	t.Log("08 cancel requested")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled run did not settle")
	}
	t.Log("09 cancelled run settled")
	after, afterProtected = aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("cancel did not restore ACL")
	}
	if afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root); !bytes.Equal(beforeDescriptor, afterDescriptor) {
		t.Fatalf("cancel did not restore the complete security descriptor")
	}
	t.Log("10 cancel descriptor restored/verified")

	t.Log("11 idempotent cleanup preparation starting")
	run, err := prepareWindowsACLRun(SandboxWorkspaceWrite, root)
	if err != nil {
		t.Fatalf("prepare partial-failure/rollback fixture: %v", err)
	}
	t.Log("12 idempotent cleanup fixture prepared")
	if err := run.close(); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	t.Log("13 first cleanup completed")
	if err := run.close(); err != nil {
		t.Fatalf("second cleanup was not idempotent: %v", err)
	}
	t.Log("14 second cleanup completed")
	after, afterProtected = aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("partial-failure rollback did not restore exact ACL")
	}
	if afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root); !bytes.Equal(beforeDescriptor, afterDescriptor) {
		t.Fatalf("partial-failure rollback did not restore the complete security descriptor")
	}
	t.Log("15 rollback descriptor restored/verified")
}

func TestWindowsACLCrashRecovery(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	t.Logf("01 fixture created path=%s", root)
	before, protected := aclBytesForPath(t, root)
	beforeDescriptor := windowsSecurityDescriptorBytesForPath(t, root)
	beforeControl := windowsACLJournalControlForPath(t, root)
	t.Logf("02 original descriptor captured bytes=%d protected=%v", len(before), protected)
	capability, err := windows.StringToSid(workspaceWriteSID(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("03 capability SID created sid=%s", capability.String())
	state := windowsACLJournal{
		Version:       1,
		Path:          root,
		OriginalACL:   "bad-base64",
		DACLProtected: protected,
		DACLControl:   beforeControl,
		TrusteeSID:    capability.String(),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := grantWindowsACL(root, capability); err != nil {
		t.Fatalf("simulate stale grant: %v", err)
	}
	t.Log("04 stale grant applied")
	afterGrant, _ := aclBytesForPath(t, root)
	if bytes.Equal(before, afterGrant) {
		t.Fatal("simulation did not mutate ACL")
	}
	t.Logf("05 ACL mutation verified bytes=%d", len(afterGrant))
	state.OriginalACL = base64.StdEncoding.EncodeToString(before)
	journalPath, err := writeWindowsACLJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("06 recovery journal persisted path=%s", journalPath)
	if err := recoverWindowsACLJournalFor(filepath.Base(journalPath)); err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	t.Log("07 recovery completed")
	after, afterProtected := aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("crash recovery did not restore exact ACL")
	}
	if afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root); !bytes.Equal(beforeDescriptor, afterDescriptor) {
		t.Fatalf("crash recovery did not restore the complete security descriptor")
	}
	t.Log("08 descriptor restored/verified")
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery journal remains: %v", err)
	}
	t.Log("09 recovery journal finalized")
}

func TestWindowsACLRecoveryFinalizesMissingWorkspaceJournal(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "removed-workspace")
	capability, err := windows.StringToSid(workspaceWriteSID(missing))
	if err != nil {
		t.Fatal(err)
	}
	state := windowsACLJournal{
		Version:       1,
		Path:          missing,
		OriginalACL:   base64.StdEncoding.EncodeToString([]byte("missing-workspace")),
		DACLProtected: true,
		DACLControl:   windows.SE_DACL_PROTECTED,
		TrusteeSID:    capability.String(),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	journalPath, err := writeWindowsACLJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverWindowsACLJournalFor(filepath.Base(journalPath)); err != nil {
		t.Fatalf("recovery with missing workspace: %v", err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-workspace journal remains: %v", err)
	}
}

func TestWindowsACLCrashRecoveryFaultMatrix(t *testing.T) {
	requireWindowsACL(t)
	for _, tc := range []struct {
		name  string
		phase string
	}{
		{name: "case1", phase: "afterJournalPersist"},
		{name: "case2", phase: "afterACLApply"},
		{name: "case3", phase: "duringRestore"},
	} {
		t.Run(tc.name+"/"+tc.phase, func(t *testing.T) {
			root := secureWindowsACLTestRoot(t, false)
			before, protected := aclBytesForPath(t, root)
			beforeDescriptor := windowsSecurityDescriptorBytesForPath(t, root)
			beforeControl := windowsSecurityDescriptorControlForPath(t, root)
			beforeDACLControl := windowsACLJournalControlForPath(t, root)
			capability, err := windows.StringToSid(workspaceWriteSID(root))
			if err != nil {
				t.Fatal(err)
			}
			state := windowsACLJournal{
				Version:       1,
				Path:          root,
				OriginalACL:   base64.StdEncoding.EncodeToString(before),
				DACLProtected: protected,
				DACLControl:   beforeDACLControl,
				TrusteeSID:    capability.String(),
				CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			}
			journalPath, err := writeWindowsACLJournal(state)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("01 journal persisted path=%s", journalPath)
			if tc.phase != "afterJournalPersist" {
				if err := grantWindowsACL(root, capability); err != nil {
					t.Fatal(err)
				}
				t.Log("02 ACL mutation applied")
			}
			if tc.phase == "duringRestore" {
				if err := restoreWindowsACLBytes(root, before, protected, beforeDACLControl); err != nil {
					t.Fatal(err)
				}
				t.Log("03 restore started and completed; crash before journal finalize")
				if _, err := os.Stat(journalPath); err != nil {
					t.Fatalf("fault journal disappeared before simulated crash: %v", err)
				}
			}
			t.Logf("04 recovery starting from phase=%s", tc.phase)
			if err := recoverWindowsACLJournalFor(filepath.Base(journalPath)); err != nil {
				t.Fatal(err)
			}
			after, afterProtected := aclBytesForPath(t, root)
			if !bytes.Equal(before, after) || protected != afterProtected {
				t.Fatalf("phase %s did not restore exact ACL", tc.phase)
			}
			if afterDescriptor := windowsSecurityDescriptorBytesForPath(t, root); !bytes.Equal(beforeDescriptor, afterDescriptor) {
				t.Logf("descriptor mismatch beforeControl=0x%x afterControl=0x%x", beforeControl, windowsSecurityDescriptorControlForPath(t, root))
				t.Fatalf("phase %s did not restore complete descriptor", tc.phase)
			}
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("phase %s journal remains: %v", tc.phase, err)
			}
			t.Logf("05 recovery completed phase=%s", tc.phase)
		})
	}
}
