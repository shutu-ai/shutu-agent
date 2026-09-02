//go:build linux

package code

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// processCPUTime reads the child process' user+system CPU clock from procfs.
// The Code Runtime has one Node process, so this measures the same busy time
// that the reference worker budgets while a host binding is not awaited.
func processCPUTime(pid int) (time.Duration, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 || end+2 > len(raw) {
		return 0, fmt.Errorf("code: malformed process stat")
	}
	fields := strings.Fields(string(raw)[end+2:])
	// The suffix starts at field 3 (state); utime and stime are fields 14/15.
	if len(fields) <= 12 {
		return 0, fmt.Errorf("code: incomplete process stat")
	}
	user, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	system, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(user+system) * time.Second / 100, nil
}
