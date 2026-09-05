//go:build windows

package terminal

import (
	"slices"
	"testing"
)

func TestShellCommandWindowsPowerShellIsProfileFree(t *testing.T) {
	cmd := shellCommand(SessionOpts{Shell: "pwsh"})
	want := []string{"-NoLogo", "-NoExit", "-NoProfile"}
	if !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("pwsh args = %v, want %v", cmd.Args[1:], want)
	}
}

func TestShellCommandWindowsPowerShellHonorsConfiguredArgs(t *testing.T) {
	cmd := shellCommand(SessionOpts{Shell: "pwsh", Args: []string{"-NoLogo"}})
	want := []string{"-NoLogo", "-NoExit", "-NoProfile"}
	if !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("configured pwsh args = %v, want %v", cmd.Args[1:], want)
	}

	cmd = shellCommand(SessionOpts{Shell: "pwsh", Args: []string{"-NoLogo", "-Profile"}})
	want = []string{"-NoLogo", "-Profile", "-NoExit"}
	if !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("profile-aware pwsh args = %v, want %v", cmd.Args[1:], want)
	}
}
