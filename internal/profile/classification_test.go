package profile

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestCapabilityInventoryMatchesPinnedReferenceSeams(t *testing.T) {
	capabilities := Classifications()
	if len(capabilities) != 58 || len(CapabilityIDs) != 58 {
		t.Fatalf("capability inventory = %d rows / %d IDs, want 58", len(capabilities), len(CapabilityIDs))
	}
	ids := make([]string, 0, len(capabilities))
	classes := map[Classification]int{}
	states := map[State]int{}
	for _, capability := range capabilities {
		ids = append(ids, capability.ID)
		classes[capability.Classification]++
		states[capability.State]++
		if err := capability.validate(); err != nil {
			t.Fatalf("validate %s: %v", capability.ID, err)
		}
		if capability.Classification == ClassificationRequired {
			var claimed bool
			for _, name := range []string{ProfileDSHBase, ProfileWeb, ProfileHeadless} {
				if slices.Contains(capability.Profiles, name) {
					claimed = true
					break
				}
			}
			if !claimed {
				t.Fatalf("required capability %s has profiles %v", capability.ID, capability.Profiles)
			}
		}
	}
	for _, want := range CapabilityIDs {
		if !slices.Contains(ids, want) {
			t.Fatalf("inventory is missing pinned seam %q", want)
		}
	}
	if classes[ClassificationRequired] == 0 || classes[ClassificationOptional] == 0 {
		t.Fatalf("classification distribution = %#v", classes)
	}
	if states[StateAvailable] == 0 || states[StateUnsupported] == 0 {
		t.Fatalf("subject state distribution = %#v", states)
	}
}

func TestCapabilityInventoryAgreesWithRuntimeProfileAuthority(t *testing.T) {
	registry := Local()
	cases := map[string]string{
		"ctx.e2b":                 IDSandboxesE2B,
		"ctx.dynamicCordisRunner": IDCordisDynamicRunner,
		"ctx.cordisInspect":       IDCordisInspect,
	}
	for capabilityID, profileID := range cases {
		capability, err := GetCapability(capabilityID)
		if err != nil {
			t.Fatalf("capability %s: %v", capabilityID, err)
		}
		descriptor, err := registry.Get(profileID)
		if err != nil {
			t.Fatalf("profile %s: %v", profileID, err)
		}
		if capability.State != StateUnsupported || descriptor.State != StateUnsupported {
			t.Fatalf("%s/%s = %+v/%+v, want unsupported", capabilityID, profileID, capability, descriptor)
		}
		if err := registry.Use(profileID); !errors.Is(err, ErrProfileUnsupported) {
			t.Fatalf("use %s = %v, want ErrProfileUnsupported", profileID, err)
		}
	}
	if _, err := GetCapability("ctx.not-in-reference"); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown capability = %v, want ErrUnknownProfile", err)
	}
}

func TestCapabilityWireShapeIsStable(t *testing.T) {
	encoded, err := json.Marshal(Classifications())
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Capability
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 58 {
		t.Fatalf("wire round trip has %d capabilities", len(decoded))
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(reencoded) {
		t.Fatal("capability JSON round trip is not stable")
	}
}
