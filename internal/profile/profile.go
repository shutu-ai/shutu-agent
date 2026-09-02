// Package profile is the single fail-closed authority for optional runtime
// deployment profiles. Transport surfaces must consult this registry instead of
// returning empty successes for capabilities that are not implemented.
package profile

import (
	"errors"
	"fmt"
	"sort"

	"github.com/jabing/shutu-agent/internal/code"
)

type State string

const (
	StateAvailable   State = "available"
	StateUnsupported State = "unsupported"
)

const (
	IDStorageSQLite       = "storage-sqlite"
	IDFileLocal           = "file-local"
	IDSessionReference    = "session-reference"
	IDCodeLocalJavaScript = "code-local-javascript"
	IDCodeLocalShell      = "code-local-shell"
	IDSandboxesE2B        = "sandbox-e2b"
	IDCodePython          = "code-python"
	IDCordisDynamicRunner = "cordis-dynamic-runner"
	IDCordisInspect       = "cordis-inspect"
)

var (
	ErrUnknownProfile      = errors.New("profile is unknown")
	ErrProfileUnsupported  = errors.New("profile is unsupported")
	ErrProfileNotAvailable = errors.New("profile is not available")
)

type Descriptor struct {
	ID             string `json:"id"`
	State          State  `json:"state"`
	Enforcement    string `json:"enforcement,omitempty"`
	Implementation string `json:"implementation,omitempty"`
	Replay         string `json:"replay,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type Registry struct {
	profiles map[string]Descriptor
}

func Local() *Registry {
	codeModeAvailable, codeModeReason := code.TypeScriptRuntimeStatus()
	shellModeAvailable, shellModeReason := code.LocalSandboxStatus()
	sandboxDiagnostic := code.LocalSandboxDiagnostic()
	codeMode := Descriptor{
		ID:     IDCodeLocalJavaScript,
		State:  StateUnsupported,
		Reason: codeModeReason,
	}
	if codeModeAvailable {
		codeMode = Descriptor{
			ID: IDCodeLocalJavaScript, State: StateAvailable,
			Enforcement:    "Node permission-model worker, empty ambient environment, output/CPU/wall/heap quotas and cancellation",
			Implementation: "internal/code",
			Replay:         "local code worker lifecycle, cancellation and invalid-output tests",
		}
	}
	shellMode := Descriptor{
		ID:     IDCodeLocalShell,
		State:  StateUnsupported,
		Reason: shellModeReason,
	}
	if shellModeAvailable {
		shellMode = Descriptor{
			ID: IDCodeLocalShell, State: StateAvailable,
			Enforcement:    sandboxDiagnostic.Summary + " Credential-scrubbed environment, bounded output and process-tree teardown; ACL/Seatbelt containment does not satisfy RequireStrongIsolation.",
			Implementation: "internal/code; internal/tools; internal/jobs; internal/terminal",
			Replay:         "shell cancellation, ownership and bounded-output tests",
		}
	}
	descriptors := []Descriptor{
		{
			ID: IDStorageSQLite, State: StateAvailable,
			Enforcement:    "SQLite transactions, OS process locking and contiguous durable sequences",
			Implementation: "internal/store; internal/persistence",
			Replay:         "SQLite/JSONL persistence contract and corruption suites",
		},
		{
			ID: IDFileLocal, State: StateAvailable,
			Enforcement:    "workspace-root containment with bounded reads and durable fs events",
			Implementation: "internal/fs",
			Replay:         "filesystem contract and negative-path tests",
		},
		{
			ID: IDSessionReference, State: StateAvailable,
			Enforcement:    "durable session headers, lineage metadata and store-backed reference authorization",
			Implementation: "internal/store; internal/sessionquery",
			Replay:         "session reference authorization and cold replay tests",
		},
		codeMode,
		shellMode,
		{
			ID: IDSandboxesE2B, State: StateUnsupported,
			Reason: "optional e2b remote sandbox backend is not composed in this deployment",
		},
		{
			ID: IDCodePython, State: StateUnsupported,
			Reason: "optional Python code runtime backend is not composed in this deployment",
		},
		{
			ID: IDCordisDynamicRunner, State: StateUnsupported,
			Reason: "optional Cordis dynamic runner backend is not composed in this deployment",
		},
		{
			ID: IDCordisInspect, State: StateUnsupported,
			Reason: "optional Cordis inspect backend is not composed in this deployment",
		},
	}
	registry := &Registry{profiles: make(map[string]Descriptor, len(descriptors))}
	for _, descriptor := range descriptors {
		if err := descriptor.validate(); err != nil {
			panic(err)
		}
		registry.profiles[descriptor.ID] = descriptor
	}
	return registry
}

func (d Descriptor) validate() error {
	switch d.State {
	case StateAvailable:
		if d.ID == "" || d.Enforcement == "" || d.Implementation == "" || d.Replay == "" || d.Reason != "" {
			return fmt.Errorf("profile %q has an invalid available descriptor", d.ID)
		}
	case StateUnsupported:
		if d.ID == "" || d.Enforcement != "" || d.Implementation != "" || d.Replay != "" || d.Reason == "" {
			return fmt.Errorf("profile %q has an invalid unsupported descriptor", d.ID)
		}
	default:
		return fmt.Errorf("profile %q has invalid state %q", d.ID, d.State)
	}
	return nil
}

func (r *Registry) Get(id string) (Descriptor, error) {
	if r == nil {
		return Descriptor{}, ErrUnknownProfile
	}
	descriptor, ok := r.profiles[id]
	if !ok {
		return Descriptor{}, fmt.Errorf("%w: %q", ErrUnknownProfile, id)
	}
	return descriptor, nil
}

func (r *Registry) List() []Descriptor {
	if r == nil {
		return nil
	}
	out := make([]Descriptor, 0, len(r.profiles))
	for _, descriptor := range r.profiles {
		out = append(out, descriptor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) Available(id string) bool {
	descriptor, err := r.Get(id)
	return err == nil && descriptor.State == StateAvailable
}

func (r *Registry) Use(id string) error {
	descriptor, err := r.Get(id)
	if err != nil {
		return err
	}
	if descriptor.State != StateAvailable {
		return fmt.Errorf("%w: %s", ErrProfileUnsupported, descriptor.ID)
	}
	return nil
}
