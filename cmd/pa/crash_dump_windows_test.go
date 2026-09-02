//go:build windows

package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

type fakeRegistryKey struct{}

func (fakeRegistryKey) Close() error { return nil }

func fakeRegistryOpener(existing map[string]bool) registryOpener {
	return func(_ registry.Key, path string, _ uint32) (registryKeyHandle, error) {
		if existing[strings.ToLower(path)] {
			return fakeRegistryKey{}, nil
		}
		return nil, registry.ErrNotExist
	}
}

// TestWERLocalDumpsProbeHandlesMissingKey covers the registry helper without
// requiring (or mutating) machine dump policy.
func TestWERLocalDumpsProbeHandlesMissingKey(t *testing.T) {
	enabled, err := registryKeyExists(
		registry.CURRENT_USER,
		`SOFTWARE\shutu-agent-tests\definitely-not-a-real-wer-key`,
	)
	if err != nil {
		t.Fatalf("probe missing registry key: %v", err)
	}
	if enabled {
		t.Fatal("missing registry key was reported as enabled")
	}
}

// TestWERLocalDumpsEnabledCoversGlobalAndPerAppKeys proves the Windows gate
// cannot be bypassed by moving WER configuration to a per-application key.
func TestWERLocalDumpsEnabledCoversGlobalAndPerAppKeys(t *testing.T) {
	existing := map[string]bool{
		strings.ToLower(werLocalDumpsPath): true,
	}
	enabled, err := werLocalDumpsEnabledWith(registry.CURRENT_USER, "pa.exe", fakeRegistryOpener(existing))
	if err != nil || !enabled {
		t.Fatalf("global LocalDumps detection = %v, %v; want true,nil", enabled, err)
	}

	existing = map[string]bool{
		strings.ToLower(werLocalDumpsPath + `\pa.exe`): true,
	}
	enabled, err = werLocalDumpsEnabledWith(registry.CURRENT_USER, "pa.exe", fakeRegistryOpener(existing))
	if err != nil || !enabled {
		t.Fatalf("per-app LocalDumps detection = %v, %v; want true,nil", enabled, err)
	}
}
