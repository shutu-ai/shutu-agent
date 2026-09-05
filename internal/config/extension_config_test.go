package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionConfigDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("extensions:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Extensions.Enabled || cfg.Extensions.StartupTimeoutMS != 10000 || cfg.Extensions.GlobalContextChars != 4000 || cfg.Extensions.GlobalContextTokens != 1000 || cfg.Extensions.MaxContributionTokens != 500 {
		t.Fatalf("extension defaults = %#v", cfg.Extensions)
	}
	if err := os.WriteFile(path, []byte("extensions:\n  enabled: true\n  sources:\n    - manifest: one.yaml\n    - manifest: one.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("duplicate extension source was accepted")
	}
}
