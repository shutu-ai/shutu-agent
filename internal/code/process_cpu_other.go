//go:build !linux && !windows

package code

import (
	"errors"
	"time"
)

var errProcessCPUUnavailable = errors.New("code: process CPU accounting unavailable")

func processCPUTime(int) (time.Duration, error) { return 0, errProcessCPUUnavailable }
