package code

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	localSandboxStatusOnce sync.Once
	localSandboxAvailable  bool
	localSandboxReason     string
	localDiagnosticOnce    sync.Once
	localDiagnostic        SandboxDiagnostic
	typeScriptStatusMu     sync.Mutex
	typeScriptStatusReady  bool
	typeScriptAvailable    bool
	typeScriptReason       string
)

// LocalSandboxStatus reports whether the default provider can enforce the
// workspace boundary used by the default code mode. Full host access is an
// explicit escape hatch and does not make the controlled profile available.
// The result is probed once per process so profile listing cannot repeatedly
// launch a privileged-backend probe.
func LocalSandboxStatus() (available bool, reason string) {
	localSandboxStatusOnce.Do(func() {
		provider := newLocalProvider()
		capabilities := provider.Capabilities()
		for _, mode := range capabilities.SupportedModes {
			if mode == SandboxWorkspaceWrite {
				localSandboxAvailable = true
				return
			}
		}
		localSandboxReason = "no enforcing workspace-write backend is available"
	})
	return localSandboxAvailable, localSandboxReason
}

// LocalSandboxDiagnostic reports the active backend and its explicit security
// contract. A containment backend remains usable for controlled shell modes,
// but `RequireStrongIsolation` continues to fail closed.
func LocalSandboxDiagnostic() SandboxDiagnostic {
	localDiagnosticOnce.Do(func() {
		p := newLocalProvider()
		capabilities := p.Capabilities()
		localDiagnostic = SandboxDiagnostic{
			Available:        capabilities.Available && len(capabilities.SupportedModes) > 1,
			Backend:          capabilities.Backend,
			IsolationLevel:   capabilities.IsolationLevel,
			StrongIsolation:  capabilities.StrongIsolation,
			NetworkIsolation: capabilities.NetworkIsolation,
			Reason:           localSandboxReason,
		}
		switch {
		case !localDiagnostic.Available:
			localDiagnostic.Backend = "none"
			localDiagnostic.IsolationLevel = IsolationNone
			localDiagnostic.Summary = "No enforcing controlled-shell backend is available; danger-full-access remains explicit."
		case capabilities.Backend == "bubblewrap":
			localDiagnostic.Summary = "Sandbox backend: bubblewrap; isolation level: strong."
		case capabilities.Backend == "windows-acl":
			localDiagnostic.Summary = "Sandbox backend: windows-acl; isolation level: containment."
		default:
			localDiagnostic.Summary = "Sandbox backend: " + capabilities.Backend + "; isolation level: containment."
		}
	})
	return localDiagnostic
}

// TypeScriptRuntimeStatus reports whether Code Mode can start its enforcing
// Node permission-model worker. Construction of TypeScriptRuntime is cheap,
// but registering run_code without this capability would advertise a tool
// that cannot execute safely on the host.
func TypeScriptRuntimeStatus() (available bool, reason string) {
	typeScriptStatusMu.Lock()
	defer typeScriptStatusMu.Unlock()
	if typeScriptStatusReady {
		return typeScriptAvailable, typeScriptReason
	}

	node, err := exec.LookPath("node")
	if err != nil {
		typeScriptAvailable = false
		typeScriptReason = fmt.Sprintf("node executable not found: %v", err)
		typeScriptStatusReady = true
		return typeScriptAvailable, typeScriptReason
	}
	// Node startup can be delayed by a full repository test/build running
	// alongside the capability probe. Keep the probe bounded, but do not turn
	// ordinary host contention into a permanently cached false classification.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--help").Output()
	if err != nil {
		typeScriptAvailable = false
		typeScriptReason = fmt.Sprintf("cannot verify Node permission model: %v", err)
		if ctx.Err() == nil {
			typeScriptStatusReady = true
		}
		return typeScriptAvailable, typeScriptReason
	}
	if !bytes.Contains(out, []byte("--permission")) {
		typeScriptAvailable = false
		typeScriptReason = "Node permission model is unavailable"
		typeScriptStatusReady = true
		return typeScriptAvailable, typeScriptReason
	}
	if err := probeNodePermissionModel(ctx, node); err != nil {
		typeScriptAvailable = false
		typeScriptReason = err.Error()
		if !errors.Is(err, context.DeadlineExceeded) {
			typeScriptStatusReady = true
		}
		return typeScriptAvailable, typeScriptReason
	}
	typeScriptAvailable = true
	typeScriptReason = ""
	typeScriptStatusReady = true
	return typeScriptAvailable, typeScriptReason
}

// probeNodePermissionModel executes the smallest real enforcement check we
// can perform without creating a file or relying on a platform-specific path.
// A help-text match is only an API-shape check; this probe requires Node to
// deny access to its own executable when no fs-read grant is supplied. The
// runtime later grants only the generated program file, so accepting a Node
// binary that merely advertises --permission would be a false capability.
func probeNodePermissionModel(ctx context.Context, node string) error {
	const source = `const fs=require('node:fs'); try { fs.readFileSync(process.execPath); process.stdout.write('UNEXPECTED_ACCESS'); process.exitCode=1 } catch (error) { if (!error || error.code !== 'ERR_ACCESS_DENIED') { process.stderr.write(String(error && error.code || error)); process.exitCode=2 } }`
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, node, "--permission", "-e", source)
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if probeCtx.Err() != nil {
		return fmt.Errorf("cannot verify Node permission enforcement: %w", probeCtx.Err())
	}
	if err != nil {
		return fmt.Errorf("Node permission enforcement probe failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("Node permission enforcement probe produced unexpected output: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
