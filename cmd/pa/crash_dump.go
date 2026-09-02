package main

import (
	"fmt"

	"github.com/jabing/shutu-agent/internal/config"
)

// enforceCrashDumpPolicy is the A6.4 OS-boundary gate. The default disabled
// profile asks the platform to prevent core images before any credential is
// loaded. The explicit external profile is a documented non-equivalent
// deployment profile: the OS, not shutu, owns dump generation and retention.
func enforceCrashDumpPolicy(policy string) error {
	switch policy {
	case config.CrashDumpPolicyDisabled:
		return preventOSCoreDumps()
	case config.CrashDumpPolicyExternal:
		return nil
	case "":
		return fmt.Errorf("crash dump policy is required (want %s|%s)",
			config.CrashDumpPolicyDisabled, config.CrashDumpPolicyExternal)
	default:
		return fmt.Errorf("invalid crash dump policy %q (want %s|%s)",
			policy, config.CrashDumpPolicyDisabled, config.CrashDumpPolicyExternal)
	}
}
