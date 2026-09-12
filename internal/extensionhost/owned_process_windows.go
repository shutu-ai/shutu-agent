//go:build windows

package extensionhost

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsOwnedProcess struct {
	job  windows.Handle
	once sync.Once
	err  error
}

func prepareOwnedProcess(_ *exec.Cmd) {}

func attachOwnedProcess(cmd *exec.Cmd) (ownedProcess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("extension: process tree requires a started process")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("extension: create process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("extension: configure process job: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("extension: open process for job: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, h)
	_ = windows.CloseHandle(h)
	if assignErr != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("extension: assign process job: %w", assignErr)
	}
	return &windowsOwnedProcess{job: job}, nil
}

func (p *windowsOwnedProcess) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		if p.job != 0 {
			p.err = windows.CloseHandle(p.job)
			p.job = 0
		}
	})
	return p.err
}
