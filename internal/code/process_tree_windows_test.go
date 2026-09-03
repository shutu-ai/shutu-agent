//go:build windows

package code

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestProcessTreeEnforcesPerProcessCPU(t *testing.T) {
	if os.Getenv("SHUTU_CPU_HELPER") == "1" {
		// Keep the child quiescent until the parent has attached it to the Job
		// Object. This removes the start/attach scheduling race from the test;
		// production code still attaches immediately after Start.
		for {
			if _, err := os.Stat(os.Getenv("SHUTU_CPU_GO")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		for {
		}
	}
	// This loop is intentionally CPU-bound and has no output. The job limit is
	// the assertion under test; Close below is only the leak-safe fallback if a
	// platform fails to enforce it.
	goFile := t.TempDir() + "\\go"
	cmd := exec.Command(os.Args[0], "-test.run", "^TestProcessTreeEnforcesPerProcessCPU$")
	cmd.Env = append(os.Environ(), "SHUTU_CPU_HELPER=1", "SHUTU_CPU_GO="+goFile)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start CPU-bound child: %v", err)
	}
	tree, err := attachProcessTree(cmd, processTreeLimits{perProcessCPU: 250 * time.Millisecond})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("attach CPU-limited process tree: %v", err)
	}
	defer tree.Close()
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatalf("query CPU-limited job: %v", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_PROCESS_TIME == 0 || info.BasicLimitInformation.PerProcessUserTimeLimit <= 0 {
		t.Fatalf("job CPU limit = flags 0x%x time %d, want process-time limit", info.BasicLimitInformation.LimitFlags, info.BasicLimitInformation.PerProcessUserTimeLimit)
	}
	if err := os.WriteFile(goFile, []byte("go"), 0600); err != nil {
		t.Fatalf("release CPU-bound child: %v", err)
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("CPU-bound child exited successfully instead of hitting the job limit")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CPU-bound child was not terminated by the Job Object CPU limit")
	}
}

func TestProcessTreeEnforcesActiveProcessLimit(t *testing.T) {
	root := t.TempDir()
	started := root + `\started`
	report := root + `\report`
	stop := root + `\stop`
	child := exec.Command(os.Args[0], "-test.run", "^TestProcessTreeActiveProcessLimitChild$")
	child.Env = append(os.Environ(),
		"SHUTU_TREE_LIMIT_CHILD=1",
		"SHUTU_TREE_LIMIT_STARTED="+started,
		"SHUTU_TREE_LIMIT_REPORT="+report,
		"SHUTU_TREE_LIMIT_STOP="+stop,
	)
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		t.Fatalf("start process-limit child: %v", err)
	}
	tree, err := attachProcessTree(child, processTreeLimits{maxProcesses: 2})
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("attach process-count-limited tree: %v", err)
	}
	defer tree.Close()

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatalf("query active-process-limited job: %v", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS == 0 ||
		info.BasicLimitInformation.ActiveProcessLimit != 2 {
		t.Fatalf("job active-process limit = flags 0x%x limit %d, want flag and limit 2",
			info.BasicLimitInformation.LimitFlags, info.BasicLimitInformation.ActiveProcessLimit)
	}
	if err := os.WriteFile(started, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, readErr := os.ReadFile(report)
		if readErr == nil {
			count, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil {
				t.Fatalf("parse descendant report %q: %v", raw, parseErr)
			}
			if count != 1 {
				t.Fatalf("hostile descendant starts = %d, want only the one allowed by the job", count)
			}
			break
		}
		if !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("process-limit child did not report descendant admission")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := os.WriteFile(stop, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	closeErr := tree.Close()
	waitErr := child.Wait()
	if closeErr != nil {
		t.Fatalf("close process tree: %v", waitErr)
	}
}

func TestProcessTreeEnforcesProcessMemory(t *testing.T) {
	if os.Getenv("SHUTU_MEMORY_HELPER") == "1" {
		goFile := os.Getenv("SHUTU_MEMORY_GO")
		for {
			if _, err := os.Stat(goFile); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		data := make([]byte, 128*1024*1024)
		for index := range data {
			data[index] = byte(index)
		}
		if len(data) != 128*1024*1024 {
			os.Exit(1)
		}
		return
	}

	goFile := t.TempDir() + `\go`
	cmd := exec.Command(os.Args[0], "-test.run", "^TestProcessTreeEnforcesProcessMemory$")
	cmd.Env = append(os.Environ(), "SHUTU_MEMORY_HELPER=1", "SHUTU_MEMORY_GO="+goFile)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start memory-bound child: %v", err)
	}
	tree, err := attachProcessTree(cmd, processTreeLimits{memoryBytes: 64 * 1024 * 1024})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("attach memory-limited process tree: %v", err)
	}
	defer tree.Close()
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatalf("query memory-limited job: %v", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY == 0 ||
		info.ProcessMemoryLimit != 64*1024*1024 {
		t.Fatalf("job memory limit = flags 0x%x bytes %d, want flag and 64MiB", info.BasicLimitInformation.LimitFlags, info.ProcessMemoryLimit)
	}
	if err := os.WriteFile(goFile, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("memory-bound child exited successfully instead of hitting the job limit")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("memory-bound child was not terminated by the Job Object memory limit")
	}
}

func TestProcessTreeActiveProcessLimitChild(t *testing.T) {
	if os.Getenv("SHUTU_TREE_LIMIT_CHILD") != "1" {
		t.Skip("process-tree active-limit child")
	}
	started := os.Getenv("SHUTU_TREE_LIMIT_STARTED")
	report := os.Getenv("SHUTU_TREE_LIMIT_REPORT")
	stop := os.Getenv("SHUTU_TREE_LIMIT_STOP")
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	descendant := exec.Command(os.Args[0], "-test.run", "^TestProcessTreeLimitDescendant$")
	descendant.Env = append(os.Environ(),
		"SHUTU_TREE_LIMIT_DESCENDANT=1",
		"SHUTU_TREE_LIMIT_STOP="+stop,
	)
	successes := 0
	var handles []*os.Process
	for attempt := 0; attempt < 4; attempt++ {
		if err := descendant.Start(); err == nil {
			successes++
			handles = append(handles, descendant.Process)
		}
	}
	if err := os.WriteFile(report, []byte(fmt.Sprint(successes)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, process := range handles {
		if process != nil {
			_ = process.Release()
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessTreeLimitDescendant(t *testing.T) {
	if os.Getenv("SHUTU_TREE_LIMIT_DESCENDANT") != "1" {
		t.Skip("process-tree limit descendant")
	}
	stop := os.Getenv("SHUTU_TREE_LIMIT_STOP")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
