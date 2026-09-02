//go:build windows

package tools

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var processJobs sync.Map // map[*exec.Cmd]windows.Handle

// prepareProcessGroup is a no-op on Windows. The process is placed into a
// short-lived Job Object at cancellation time; unlike taskkill this does not
// require a shell command or elevated process-tree permission.
func prepareProcessGroup(cmd *exec.Cmd) {}

// attachProcessGroup captures descendants from process creation onward. A job
// attached only when cancellation fires can miss a child that the shell has
// already created, so all foreground/background command paths call this
// immediately after Start. Failure keeps the explicit direct-kill fallback.
func attachProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := newProcessJob(cmd)
	if err == nil {
		processJobs.Store(cmd, job)
	}
}

func releaseProcessGroup(cmd *exec.Cmd) {
	if value, ok := processJobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		_, _ = windows.WaitForSingleObject(job, 5000)
		_ = windows.CloseHandle(job)
	}
}

func newProcessJob(cmd *exec.Cmd) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	assignErr := windows.AssignProcessToJobObject(job, processHandle)
	_ = windows.CloseHandle(processHandle)
	if assignErr != nil {
		_ = windows.CloseHandle(job)
		return 0, assignErr
	}
	return job, nil
}

// killTree assigns the running command to a private Windows Job Object and
// closes/terminates that job. JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE includes all
// descendants of cmd.exe/PowerShell, so cancellation cannot leave a grandchild
// alive after the owning tool has settled. Assignment can fail when an external
// parent already placed the process in an incompatible job; retain the direct
// kill as a conservative fallback in that case.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if value, ok := processJobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		waitResult, _ := windows.WaitForSingleObject(job, 5000)
		if waitResult != windows.WAIT_OBJECT_0 {
			killDescendants(uint32(cmd.Process.Pid))
			_ = cmd.Process.Kill()
		}
		_ = windows.CloseHandle(job)
		return
	}
	// Nested-job restrictions can reject assignment even though the command's
	// descendants are still visible to this process. Enumerate and terminate
	// those descendants before the direct fallback so a sandbox-hosted Agent
	// does not leak an inherited pipe/cwd holder.
	killDescendants(uint32(cmd.Process.Pid))
	_ = cmd.Process.Kill()
}

func killDescendants(root uint32) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snapshot)
	children := make(map[uint32][]uint32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return
	}
	for {
		if entry.ParentProcessID != 0 {
			children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	var reap func(uint32)
	reap = func(parent uint32) {
		for _, child := range children[parent] {
			reap(child)
			terminateProcess(child)
		}
	}
	reap(root)
}

func terminateProcess(pid uint32) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_ = windows.TerminateProcess(handle, 1)
	_, _ = windows.WaitForSingleObject(handle, 1000)
}
