package code

import "time"

// processTreeLimits contains limits that must be applied to the provider's
// owned process tree by the platform backend. A zero value deliberately means
// "no additional OS limit"; policy validation remains in the caller.
type processTreeLimits struct {
	// perProcessCPU is a cumulative CPU-time ceiling for the direct worker.
	// Windows Job Objects enforce this at the kernel boundary, including when
	// the worker creates descendants. Other platforms retain their existing
	// accounting/enforcement hooks until they expose an equivalent primitive.
	perProcessCPU time.Duration
	// maxProcesses is the concurrent active-process ceiling for the complete
	// job, including the direct worker. Backends without an equivalent kernel
	// primitive must reject the run rather than treat this as best effort.
	maxProcesses int
	// memoryBytes is the per-process commit/address-space ceiling. Windows
	// Job Objects expose this as PROCESS_MEMORY_LIMIT; POSIX controlled shells
	// install RLIMIT_AS before sandbox exec.
	memoryBytes int64
}
