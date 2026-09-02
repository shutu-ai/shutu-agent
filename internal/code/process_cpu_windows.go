//go:build windows

package code

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func processCPUTime(pid int) (time.Duration, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("code: process times: %w", err)
	}
	// Windows FILETIME is expressed in 100-nanosecond units.
	return time.Duration(kernel.Nanoseconds() + user.Nanoseconds()), nil
}
