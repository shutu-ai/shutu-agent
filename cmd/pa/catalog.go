package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/tools"
)

// writeToolCatalogManifest exports the release artifact's canonical tool
// inventory. A deterministic file gives packaging and deployment pipelines one
// digest to pin instead of trusting startup logs.
func writeToolCatalogManifest(registry *tools.Registry, path string) error {
	if registry == nil {
		return errors.New("tool registry is unavailable")
	}
	manifest, err := registry.CatalogManifest()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tool catalog manifest: %w", err)
	}
	raw = append(raw, '\n')
	if path == "-" {
		_, err = os.Stdout.Write(raw)
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write tool catalog manifest: %w", err)
	}
	return nil
}

// verifyToolCatalogManifest fail-closes a release/deployment artifact against
// the exact runtime registry that will execute tools. Internal manifest
// integrity is checked first; then digest and revision must match the live
// snapshot, which also catches tool payload drift.
func verifyToolCatalogManifest(registry *tools.Registry, path string) error {
	if registry == nil {
		return errors.New("tool registry is unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read tool catalog manifest: %w", err)
	}
	var artifact tools.CatalogManifest
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return fmt.Errorf("decode tool catalog manifest: %w", err)
	}
	if err := tools.ValidateCatalogManifest(artifact); err != nil {
		return fmt.Errorf("invalid tool catalog manifest: %w", err)
	}
	live, err := registry.CatalogManifest()
	if err != nil {
		return err
	}
	if artifact.Digest != live.Digest || artifact.Revision != live.Revision {
		return fmt.Errorf(
			"tool catalog manifest mismatch: artifact digest/revision %s/%d, runtime %s/%d",
			artifact.Digest, artifact.Revision, live.Digest, live.Revision,
		)
	}
	return nil
}
