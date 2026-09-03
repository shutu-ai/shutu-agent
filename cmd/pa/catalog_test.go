package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestToolCatalogManifestArtifactRoundTrip(t *testing.T) {
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	reg.SetPolicy(tools.Policy{Profile: "standard", Enabled: []string{"get_time"}})

	path := filepath.Join(t.TempDir(), "tool-catalog-manifest.json")
	if err := writeToolCatalogManifest(reg, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var artifact tools.CatalogManifest
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	if err := tools.ValidateCatalogManifest(artifact); err != nil {
		t.Fatalf("exported manifest invalid: %v", err)
	}
	if len(artifact.Tools) != 1 || artifact.Tools[0].Name != "get_time" || artifact.Revision == 0 {
		t.Fatalf("exported manifest = %+v", artifact)
	}
	if err := verifyToolCatalogManifest(reg, path); err != nil {
		t.Fatalf("artifact verification failed: %v", err)
	}

	// A deployment artifact must reject both in-file tampering and runtime
	// drift after a new registration generation is published.
	tampered := strings.Replace(string(raw), artifact.Digest, strings.Repeat("0", len(artifact.Digest)), 1)
	tamperedPath := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyToolCatalogManifest(reg, tamperedPath); err == nil {
		t.Fatal("tampered artifact accepted")
	}

	if err := reg.Unregister("get_time"); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterWithInfo(tools.GetTime{}, tools.RegistrationInfo{Owner: "reload", Plugin: "demo", Generation: artifact.Revision + 1}); err != nil {
		t.Fatal(err)
	}
	if err := verifyToolCatalogManifest(reg, path); err == nil {
		t.Fatal("runtime generation drift accepted")
	}
}
