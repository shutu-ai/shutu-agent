//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ownedProcess struct {
	job  windows.Handle
	pid  int
	once sync.Once
}

func prepareOwnedProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func (o *ownedProcess) interrupt() error {
	if o == nil || o.pid <= 0 {
		return errors.New("terminal: no interruptible process group")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(o.pid))
}

func attachOwnedProcess(cmd *exec.Cmd) (*ownedProcess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("terminal: process tree requires a started process")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("terminal: create process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("terminal: configure process job: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("terminal: open process for job: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, h)
	_ = windows.CloseHandle(h)
	if assignErr != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("terminal: assign process job: %w", assignErr)
	}
	return &ownedProcess{job: job, pid: cmd.Process.Pid}, nil
}

func terminateOwnedProcess(owner *ownedProcess, cmd *exec.Cmd) {
	if owner != nil {
		owner.once.Do(func() {
			if owner.job != 0 {
				_ = windows.CloseHandle(owner.job)
			}
		})
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
