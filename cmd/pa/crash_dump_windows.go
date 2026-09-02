//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const werLocalDumpsPath = `SOFTWARE\Microsoft\Windows\Windows Error Reporting\LocalDumps`

type registryKeyHandle interface{ Close() error }
type registryOpener func(registry.Key, string, uint32) (registryKeyHandle, error)

// preventOSCoreDumps fail-closes the Windows user-mode dump seam. Go cannot
// revoke a machine policy, so an enabled WER LocalDumps key (including a
// pa.exe per-application key) is an explicit refusal to start the disabled
// profile rather than a silent false claim.
func preventOSCoreDumps() error {
	app := strings.ToLower(filepath.Base(os.Args[0]))
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		enabled, err := werLocalDumpsEnabled(root, app)
		if err != nil {
			return fmt.Errorf("crash dump policy: inspect WER LocalDumps: %w", err)
		}
		if enabled {
			return fmt.Errorf("crash dump policy: Windows Error Reporting LocalDumps is enabled for %q or globally; "+
				"disable that policy or set security.crash_dump_policy: external", app)
		}
	}
	return nil
}

func werLocalDumpsEnabled(root registry.Key, app string) (bool, error) {
	return werLocalDumpsEnabledWith(root, app, openRegistryKey)
}

func werLocalDumpsEnabledWith(root registry.Key, app string, open registryOpener) (bool, error) {
	enabled, err := registryKeyExistsWith(root, werLocalDumpsPath, open)
	if err != nil || enabled {
		return enabled, err
	}
	return registryKeyExistsWith(root, werLocalDumpsPath+`\`+app, open)
}

func registryKeyExists(root registry.Key, path string) (bool, error) {
	return registryKeyExistsWith(root, path, openRegistryKey)
}

func openRegistryKey(root registry.Key, path string, access uint32) (registryKeyHandle, error) {
	key, err := registry.OpenKey(root, path, access)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func registryKeyExistsWith(root registry.Key, path string, open registryOpener) (bool, error) {
	key, err := open(root, path, registry.QUERY_VALUE)
	if err == nil {
		if closeErr := key.Close(); closeErr != nil {
			return true, closeErr
		}
		return true, nil
	}
	if err == registry.ErrNotExist {
		return false, nil
	}
	return false, err
}
