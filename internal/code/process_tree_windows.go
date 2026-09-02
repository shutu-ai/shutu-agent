//go:build windows

package code

import (
	"fmt"
	"math"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree owns a Windows Job Object. JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// ensures descendants of the shell are torn down when the provider finishes a
// timed-out run, instead of only terminating cmd.exe and leaving a grandchild
// alive outside the agent's lifecycle.
type processTree struct{ job windows.Handle }

func prepareProcessTree(_ *exec.Cmd) {}

func attachProcessTree(cmd *exec.Cmd, limits processTreeLimits) (processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return processTree{}, fmt.Errorf("code: process tree requires a started process")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return processTree{}, fmt.Errorf("code: create process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if limits.perProcessCPU > 0 {
		// Windows Job Object CPU limits use 100-nanosecond units, represented by
		// a signed LARGE_INTEGER. Keep the conversion explicit and reject values
		// that cannot be represented instead of silently wrapping a policy limit.
		const windowsTick = int64(100 * time.Nanosecond)
		cpuTicks := limits.perProcessCPU.Nanoseconds() / windowsTick
		if cpuTicks <= 0 || cpuTicks > math.MaxInt64 {
			_ = windows.CloseHandle(job)
			return processTree{}, fmt.Errorf("code: invalid process CPU limit %s", limits.perProcessCPU)
		}
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_TIME
		info.BasicLimitInformation.PerProcessUserTimeLimit = cpuTicks
	}
	if limits.maxProcesses > 0 {
		if limits.maxProcesses > math.MaxUint32 {
			_ = windows.CloseHandle(job)
			return processTree{}, fmt.Errorf("code: invalid process count limit %d", limits.maxProcesses)
		}
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = uint32(limits.maxProcesses)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return processTree{}, fmt.Errorf("code: configure process job: %w", err)
	}
	// os.Process.WithHandle is only available in newer Go releases than the
	// module's declared toolchain. Open a short-lived process handle directly
	// instead; the job retains ownership of the process association after the
	// handle is closed.
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return processTree{}, fmt.Errorf("code: open process for job: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, processHandle)
	_ = windows.CloseHandle(processHandle)
	if assignErr != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return processTree{}, fmt.Errorf("code: assign process job: %w", assignErr)
	}
	return processTree{job: job}, nil
}

func (tree processTree) Close() error {
	if tree.job == 0 {
		return nil
	}
	// The kill-on-close limit applies even when the direct child already exited.
	// This is intentionally the final operation after Wait, so queued output has
	// already been collected while the descendant tree is still owned.
	return windows.CloseHandle(tree.job)
}
