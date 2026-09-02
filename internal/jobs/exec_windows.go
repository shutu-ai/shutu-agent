//go:build windows

package jobs

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var jobProcessJobs sync.Map // map[*exec.Cmd]windows.Handle

// prepareJobProcessGroup is a no-op on Windows. killJobTree creates a private
// Job Object when cancellation is requested, avoiding taskkill's permission
// dependency while still owning cmd.exe's descendants.
func prepareJobProcessGroup(cmd *exec.Cmd) {}

func attachJobProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := newJobProcessJob(cmd)
	if err == nil {
		jobProcessJobs.Store(cmd, job)
	}
}

func releaseJobProcessGroup(cmd *exec.Cmd) {
	if value, ok := jobProcessJobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		_, _ = windows.WaitForSingleObject(job, 5000)
		_ = windows.CloseHandle(job)
	}
}

func newJobProcessJob(cmd *exec.Cmd) (windows.Handle, error) {
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

// killJobTree terminates the command and its descendants through a Windows Job
// Object. If the process cannot be assigned (for example, an incompatible
// external job already owns it), fall back to the direct child kill rather than
// allowing cancellation to fail or hang.
func killJobTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if value, ok := jobProcessJobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		waitResult, _ := windows.WaitForSingleObject(job, 5000)
		if waitResult != windows.WAIT_OBJECT_0 {
			killJobDescendants(uint32(cmd.Process.Pid))
			_ = cmd.Process.Kill()
		}
		_ = windows.CloseHandle(job)
		return
	}
	killJobDescendants(uint32(cmd.Process.Pid))
	_ = cmd.Process.Kill()
}

func killJobDescendants(root uint32) {
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
			terminateJobProcess(child)
		}
	}
	reap(root)
}

func terminateJobProcess(pid uint32) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_ = windows.TerminateProcess(handle, 1)
	_, _ = windows.WaitForSingleObject(handle, 1000)
}
