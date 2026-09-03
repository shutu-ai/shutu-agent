package profile

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/code"
)

func TestLocalRegistryEnforcesProfileDescriptorContract(t *testing.T) {
	registry := Local()
	byID := map[string]Descriptor{}
	for _, descriptor := range registry.List() {
		byID[descriptor.ID] = descriptor
	}
	for _, id := range []string{IDStorageSQLite, IDFileLocal, IDSessionReference} {
		descriptor := byID[id]
		if descriptor.State != StateAvailable || descriptor.Enforcement == "" ||
			descriptor.Implementation == "" || descriptor.Replay == "" {
			t.Fatalf("%s selected profile = %+v, want enforcing/replayable", id, descriptor)
		}
		if err := registry.Use(id); err != nil {
			t.Fatalf("use %s: %v", id, err)
		}
	}
	for _, id := range []string{
		IDSandboxesE2B, IDCodePython, IDCordisDynamicRunner, IDCordisInspect,
	} {
		descriptor := byID[id]
		if descriptor.State != StateUnsupported || descriptor.Reason == "" {
			t.Fatalf("%s optional profile = %+v, want explicit unsupported reason", id, descriptor)
		}
		if err := registry.Use(id); !errors.Is(err, ErrProfileUnsupported) {
			t.Fatalf("use %s = %v, want ErrProfileUnsupported", id, err)
		}
	}
}

func TestLocalRegistryWireShapeIsStableAndFailClosed(t *testing.T) {
	registry := Local()
	encoded, err := json.Marshal(registry.List())
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Descriptor
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(registry.List()) {
		t.Fatalf("wire round trip lost profiles: %d -> %d", len(registry.List()), len(decoded))
	}
	if _, err := registry.Get("not-a-profile"); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown profile error = %v, want ErrUnknownProfile", err)
	}
}

func TestCodeProfilesMatchRuntimeProbes(t *testing.T) {
	registry := Local()

	javascript, err := registry.Get(IDCodeLocalJavaScript)
	if err != nil {
		t.Fatal(err)
	}
	jsAvailable, jsReason := code.TypeScriptRuntimeStatus()
	if (javascript.State == StateAvailable) != jsAvailable {
		t.Fatalf("javascript profile = %+v, runtime available=%v reason=%q", javascript, jsAvailable, jsReason)
	}
	if !jsAvailable && javascript.Reason == "" {
		t.Fatal("unsupported javascript profile has no reason")
	}

	shell, err := registry.Get(IDCodeLocalShell)
	if err != nil {
		t.Fatal(err)
	}
	shellAvailable, shellReason := code.LocalSandboxStatus()
	if (shell.State == StateAvailable) != shellAvailable {
		t.Fatalf("shell profile = %+v, runtime available=%v reason=%q", shell, shellAvailable, shellReason)
	}
	if !shellAvailable && shell.Reason == "" {
		t.Fatal("unsupported shell profile has no reason")
	}
}

func TestCodeRuntimeDescriptorIsArchitectureEquivalentNotDegraded(t *testing.T) {
	capability, err := GetCapability("ctx.codeRuntime")
	if err != nil {
		t.Fatal(err)
	}
	if capability.State != StateAvailable || capability.Enforcement == "" {
		t.Fatalf("code runtime = %+v, want an available enforcing descriptor", capability)
	}
	if strings.Contains(capability.Enforcement, "compatibility degradation") {
		t.Fatalf("code runtime enforcement = %q, still claims degradation", capability.Enforcement)
	}
	if !strings.Contains(capability.Enforcement, "architecture substitute") ||
		!strings.Contains(capability.Enforcement, "permission-denied ambient child/worker") {
		t.Fatalf("code runtime enforcement = %q, want explicit process-isolation substitution and ambient denial", capability.Enforcement)
	}
}
