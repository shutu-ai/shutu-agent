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
	"strconv"
	"testing"
	"time"

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
	p := NewLocalProvider()
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
	run("delete", "del /f ..\\renamed.txt 2>delete-error.txt & type delete-error.txt")
	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed target remains after DeleteFileW/cmd delete")
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
	p := NewLocalProvider()
	defer p.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "read.txt"), []byte("read-only-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
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
		"del ..\\read.txt",
		"ren ..\\read.txt renamed.txt",
		"mkdir ..\\created",
	} {
		result, err := p.Run(context.Background(), RunRequest{
			Mode: SandboxReadOnly, Root: root, Cwd: cwd,
			Timeout: 5 * time.Second, Code: command,
		})
		requireFailedRun(t, result, err, "read-only "+command)
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
	before, protected := aclBytesForPath(t, root)

	result, err := p.Run(context.Background(), RunRequest{
		Mode: SandboxReadOnly, Root: root, Cwd: cwd,
		Timeout: 100 * time.Millisecond, Code: `ping -n 20 127.0.0.1`,
	})
	if err != nil {
		t.Fatalf("timeout run transport error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("timeout result = %+v", result)
	}
	after, afterProtected := aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("timeout did not restore ACL")
	}

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
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled run did not settle")
	}
	after, afterProtected = aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("cancel did not restore ACL")
	}

	run, err := prepareWindowsACLRun(SandboxWorkspaceWrite, root)
	if err != nil {
		t.Fatalf("prepare partial-failure/rollback fixture: %v", err)
	}
	if err := run.close(); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if err := run.close(); err != nil {
		t.Fatalf("second cleanup was not idempotent: %v", err)
	}
	after, afterProtected = aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("partial-failure rollback did not restore exact ACL")
	}
}

func TestWindowsACLCrashRecovery(t *testing.T) {
	requireWindowsACL(t)
	root := secureWindowsACLTestRoot(t, false)
	before, protected := aclBytesForPath(t, root)
	capability, err := windows.StringToSid(workspaceWriteSID(root))
	if err != nil {
		t.Fatal(err)
	}
	state := windowsACLJournal{
		Version:       1,
		Path:          root,
		OriginalACL:   "bad-base64",
		DACLProtected: protected,
		TrusteeSID:    capability.String(),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := grantWindowsACL(root, capability); err != nil {
		t.Fatalf("simulate stale grant: %v", err)
	}
	afterGrant, _ := aclBytesForPath(t, root)
	if bytes.Equal(before, afterGrant) {
		t.Fatal("simulation did not mutate ACL")
	}
	state.OriginalACL = base64.StdEncoding.EncodeToString(before)
	journalPath, err := writeWindowsACLJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverWindowsACLJournalFor(filepath.Base(journalPath)); err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	after, afterProtected := aclBytesForPath(t, root)
	if !bytes.Equal(before, after) || protected != afterProtected {
		t.Fatalf("crash recovery did not restore exact ACL")
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery journal remains: %v", err)
	}
}
