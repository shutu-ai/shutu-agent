package main

import (
	"os"
	"testing"

	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

func TestExampleManifestIsValid(t *testing.T) {
	data, err := os.ReadFile("extension.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := extension.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "demo" || !manifest.Capabilities.ContextProvider || len(manifest.Tools.Definitions) != 1 {
		t.Fatalf("example manifest = %#v", manifest)
	}
}
