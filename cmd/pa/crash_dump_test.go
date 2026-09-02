package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
)

// TestCrashDumpPolicyExternalIsExplicit keeps the non-equivalent OS-owned dump
// profile available for constrained hosts while making it impossible to select
// accidentally through the empty/default composition path.
func TestCrashDumpPolicyExternalIsExplicit(t *testing.T) {
	if err := enforceCrashDumpPolicy(config.CrashDumpPolicyExternal); err != nil {
		t.Fatalf("explicit external profile rejected: %v", err)
	}
	for _, policy := range []string{"", "enabled", "DISABLED"} {
		if err := enforceCrashDumpPolicy(policy); err == nil {
			t.Fatalf("policy %q was accepted; only disabled/external are valid", policy)
		}
	}
}

// TestDisabledCrashDumpPolicyIsPlatformEnforced exercises the actual default
// gate. On Windows, a machine policy that enables WER LocalDumps must fail
// closed; on Unix, the process must be able to set RLIMIT_CORE to zero.
func TestDisabledCrashDumpPolicyIsPlatformEnforced(t *testing.T) {
	if err := enforceCrashDumpPolicy(config.CrashDumpPolicyDisabled); err != nil {
		if runtime.GOOS == "windows" && strings.Contains(err.Error(), "LocalDumps is enabled") {
			t.Skipf("host machine already enables WER LocalDumps; startup correctly fails closed: %v", err)
		}
		t.Fatalf("disabled crash-dump profile could not be enforced: %v", err)
	}
}
