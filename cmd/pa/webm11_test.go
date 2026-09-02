// webm11_test.go — the M11 (增加提供方 / 增加自定义提供方, dsh-synced) composition-
// root tests: provider API-key override persistence with env fallback, custom
// OpenAI-compatible provider registration, deletion, and the webProviders view
// that lists every known provider (built-ins always, customs from settings)
// with configured/registered/available state. The webserver-side API tests live
// in internal/webserver.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/credential"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/store"
)

// m11App builds an app with a real sqlite store and a registered registry so
// the provider-management endpoints can persist and rebuild. deepseek is the
// selected provider, so its key must be present for registerLLM to succeed.
func m11App(t *testing.T) (*app, *store.SQLiteStore) {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mm := true
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider:   "deepseek-official",
				OpenAI:     config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
				Anthropic:  config.AnthropicProviderConfig{BaseURL: "https://api.anthropic.com/v1", Model: "claude-sonnet-4-5"},
				Multimodal: config.MultimodalConfig{Enabled: &mm, MaxImageBytes: 1 << 20},
			},
		},
		store: st,
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	return a, st
}

// vaultedM11App is the production credential boundary: Web provider edits must
// write dedicated credential records, never generic llm.key.* settings.
func vaultedM11App(t *testing.T) (*app, *store.SQLiteStore) {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "vaulted.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	vault, err := credential.New(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		store:       st,
		credentials: vault,
		llmKeys:     map[string]string{"deepseek-official": "env-key"},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	return a, st
}

func TestWebProviderCredentialUsesDedicatedVault(t *testing.T) {
	a, st := vaultedM11App(t)
	ctx := context.Background()

	if err := a.webSaveProvider(ctx, "openai", "vault-secret", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider: %v", err)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["llm.key.openai"]; ok {
		t.Fatalf("Web provider key leaked into generic settings: %q", settings["llm.key.openai"])
	}
	record, err := st.GetCredentialRecord(ctx, "OPENAI_API_KEY")
	if err != nil || string(record.Value) != "vault-secret" || record.Revoked {
		t.Fatalf("dedicated credential = %+v, %v; want active vault-secret", record, err)
	}
	if got := a.providerKey("openai"); got != "vault-secret" {
		t.Fatalf("provider key = %q, want vault-secret", got)
	}

	if err := a.webSaveProvider(ctx, "openai", "", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider(clear): %v", err)
	}
	if _, err := st.GetCredentialRecord(ctx, "OPENAI_API_KEY"); err == nil {
		t.Fatal("cleared credential remained in dedicated backend")
	}
	if got := a.providerKey("openai"); got != "" {
		t.Fatalf("provider key after clear = %q, want empty", got)
	}
}

func TestWebProviderCredentialRollbackKeepsDedicatedVault(t *testing.T) {
	a, st := vaultedM11App(t)
	ctx := context.Background()
	if err := a.webSaveProvider(ctx, "deepseek-official", "before-secret", "", "", nil); err != nil {
		t.Fatalf("save initial credential: %v", err)
	}

	// An unusable profile makes registerLLM fail after the credential mutation.
	err := a.webSaveProvider(ctx, "deepseek-official", "failed-secret", "http://", "broken-model", nil)
	if err == nil {
		t.Fatal("invalid provider profile unexpectedly succeeded")
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["llm.key.deepseek-official"]; ok {
		t.Fatal("rollback leaked a provider credential into generic settings")
	}
	record, err := st.GetCredentialRecord(ctx, "DEEPSEEK_API_KEY")
	if err != nil || string(record.Value) != "before-secret" || record.Revoked {
		t.Fatalf("rolled-back credential = %+v, %v; want active before-secret", record, err)
	}
	if got := a.providerKey("deepseek-official"); got != "before-secret" {
		t.Fatalf("provider key after rollback = %q, want before-secret", got)
	}
}

func TestWebCustomProviderCredentialUsesDedicatedVault(t *testing.T) {
	a, st := vaultedM11App(t)
	ctx := context.Background()
	if err := a.webSaveCustomProvider(ctx, "vault-custom", "Vault", "http://localhost:11434/v1", "model", "custom-secret", "", nil); err != nil {
		t.Fatalf("webSaveCustomProvider: %v", err)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["llm.key.vault-custom"]; ok {
		t.Fatal("custom provider key leaked into generic settings")
	}
	if _, ok := settings["llm.custom.vault-custom"]; !ok {
		t.Fatal("custom provider profile was not persisted")
	}
	record, err := st.GetCredentialRecord(ctx, "VAULT_CUSTOM_API_KEY")
	if err != nil || string(record.Value) != "custom-secret" || record.Revoked {
		t.Fatalf("custom credential = %+v, %v; want active custom-secret", record, err)
	}

	if err := a.webDeleteCustomProvider(ctx, "vault-custom"); err != nil {
		t.Fatalf("webDeleteCustomProvider: %v", err)
	}
	settings, err = st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["llm.custom.vault-custom"]; ok {
		t.Fatal("custom provider profile remained after delete")
	}
	if _, err := st.GetCredentialRecord(ctx, "VAULT_CUSTOM_API_KEY"); err == nil {
		t.Fatal("custom credential remained after delete")
	}
}

// TestWebProviderKeyOverride verifies M11 key precedence: a key configured via
// webSaveProvider is persisted (llm.key.<id>), beats the env var, is visible as
// configured in webProviders, and deleting it falls back to the env default.
func TestWebProviderKeyOverride(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	a, st := m11App(t)

	// Before any override: configured via env, key from env.
	cfg := a.webConfig()
	if p := findProvider(cfg["providers"].([]map[string]any), "deepseek-official"); p == nil || p["configured"] != true {
		t.Fatalf("deepseek should be configured via env before override")
	}

	// Save an override key — it wins over the env var.
	if err := a.webSaveProvider(context.Background(), "deepseek-official", "ui-key", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider: %v", err)
	}
	got, err := st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["llm.key.deepseek-official"] != "ui-key" {
		t.Fatalf("llm.key.deepseek-official = %q, want ui-key (persisted)", got["llm.key.deepseek-official"])
	}
	if k := a.providerKey("deepseek-official"); k != "ui-key" {
		t.Fatalf("providerKey(deepseek-official) = %q, want ui-key (override wins)", k)
	}

	// Clearing the override falls back to the env var.
	if err := a.webSaveProvider(context.Background(), "deepseek-official", "", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider(clear): %v", err)
	}
	if k := a.providerKey("deepseek-official"); k != "env-key" {
		t.Fatalf("providerKey(deepseek-official) after clear = %q, want env-key (fallback)", k)
	}
	if got, _ := st.GetSettings(context.Background()); got["llm.key.deepseek-official"] != "" {
		t.Fatalf("llm.key.deepseek-official should be deleted after clear, got %q", got["llm.key.deepseek-official"])
	}
}

func TestWebProviderCatalogNeverProjectsCredentialValues(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "env-only-secret")
	a, _ := m11App(t)
	if err := a.webSaveProvider(context.Background(), "deepseek-official", "persisted-secret", "", "", nil); err != nil {
		t.Fatalf("persist provider key: %v", err)
	}
	view, err := json.Marshal(a.webConfig())
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(view)
	for _, secret := range []string{"env-only-secret", "persisted-secret"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("web provider catalog projects credential %q: %s", secret, serialized)
		}
	}
}

// TestWebBuiltinProfileOverride verifies the dsh ProviderEditor 自定义设置
// alignment for a built-in provider: saving base_url / model / model list
// persists llm.profile.<id>, overrides the registered provider's base URL and
// model, shows in webProviders, and clears back to config.yaml defaults.
func TestWebBuiltinProfileOverride(t *testing.T) {
	a, st := m11App(t)
	ctx := context.Background()

	// Save a profile override (API 地址 + 模型目录).
	models := []customModel{
		{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash"},
		{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro"},
	}
	if err := a.webSaveProvider(ctx, "deepseek-official", "", "https://custom.example/v1", "deepseek-v4-flash", models); err != nil {
		t.Fatalf("webSaveProvider(profile): %v", err)
	}
	got, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := got["llm.profile.deepseek-official"]
	if raw == "" {
		t.Fatal("llm.profile.deepseek-official should be persisted")
	}
	var bp builtinProviderProfile
	if err := json.Unmarshal([]byte(raw), &bp); err != nil {
		t.Fatal(err)
	}
	if bp.BaseURL != "https://custom.example/v1" || bp.Model != "deepseek-v4-flash" || len(bp.Models) != 2 {
		t.Fatalf("profile = %+v", bp)
	}
	// The registered deepseek provider picked up the override.
	if p, err := a.llmReg.Get("deepseek-official"); err != nil {
		t.Fatalf("deepseek not registered: %v", err)
	} else if p.ID() != "deepseek-official" || !p.Available() {
		t.Fatalf("deepseek wrong id/availability: %v", p.ID())
	}
	// webProviders reflects the override + models + flag.
	providers := a.webConfig()["providers"].([]map[string]any)
	p := findProvider(providers, "deepseek-official")
	if p == nil {
		t.Fatal("deepseek-official missing from providers")
	}
	if p["base_url"] != "https://custom.example/v1" || p["model"] != "deepseek-v4-flash" || p["profile_override"] != true {
		t.Fatalf("provider view = %#v", p)
	}
	if ms, ok := p["models"].([]customModel); !ok || len(ms) != 2 {
		t.Fatalf("provider models = %#v", p["models"])
	}

	// Clearing (all-empty edit) removes the profile → back to config defaults.
	if err := a.webSaveProvider(ctx, "deepseek-official", "", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider(clear profile): %v", err)
	}
	if got, _ := st.GetSettings(ctx); got["llm.profile.deepseek-official"] != "" {
		t.Fatal("llm.profile.deepseek-official should be deleted after clear")
	}
	providers = a.webConfig()["providers"].([]map[string]any)
	if p := findProvider(providers, "deepseek-official"); p == nil || p["profile_override"] == true || p["base_url"] != "" {
		t.Fatalf("provider view after clear = %#v", p)
	}
}

// TestWebCustomProviderLifecycle verifies M11 增加自定义提供方: saving a custom
// OpenAI-compatible provider persists the profile, registers it in the live
// registry, lists it in webProviders, and DELETE removes it again.
func TestWebCustomProviderLifecycle(t *testing.T) {
	a, st := m11App(t)
	t.Setenv("OLLAMA_API_KEY", "ollama-key")

	if err := a.webSaveCustomProvider(context.Background(), "ollama", "Ollama", "http://localhost:11434/v1", "llama3.1", "", "", nil); err != nil {
		t.Fatalf("webSaveCustomProvider: %v", err)
	}

	// Persisted profile.
	got, _ := st.GetSettings(context.Background())
	if got["llm.custom.ollama"] == "" {
		t.Fatal("llm.custom.ollama should be persisted")
	}

	// Registered in the live registry under its route.
	p, err := a.llmReg.Get("ollama")
	if err != nil {
		t.Fatalf("custom provider not registered: %v", err)
	}
	if p.ID() != "ollama" {
		t.Fatalf("custom provider id = %q, want ollama", p.ID())
	}

	// Listed in webProviders as a custom, configured (env key present), available.
	cfg := a.webConfig()
	op := findProvider(cfg["providers"].([]map[string]any), "ollama")
	if op == nil {
		t.Fatal("custom provider missing from webProviders")
	}
	if op["custom"] != true || op["name"] != "Ollama" || op["base_url"] != "http://localhost:11434/v1" {
		t.Fatalf("custom provider view = %#v, want custom/name/base_url", op)
	}
	if op["configured"] != true || op["available"] != true {
		t.Fatalf("custom provider should be configured+available (env key), got %#v", op)
	}

	// Delete removes it from the registry, settings and webProviders.
	if err := a.webDeleteCustomProvider(context.Background(), "ollama"); err != nil {
		t.Fatalf("webDeleteCustomProvider: %v", err)
	}
	if _, err := a.llmReg.Get("ollama"); err == nil {
		t.Fatal("custom provider should be removed from the registry")
	}
	got, _ = st.GetSettings(context.Background())
	if got["llm.custom.ollama"] != "" || got["llm.key.ollama"] != "" {
		t.Fatalf("custom provider settings should be removed, got %q %q", got["llm.custom.ollama"], got["llm.key.ollama"])
	}
	if findProvider(a.webConfig()["providers"].([]map[string]any), "ollama") != nil {
		t.Fatal("custom provider should be gone from webProviders")
	}
}

func TestWebProviderEditRollsBackSettingsWhenRegistryRebuildFails(t *testing.T) {
	a, st := m11App(t)
	// Force the final registry selection to fail after the edit has touched
	// durable settings. The edit must not leave a half-applied key/profile.
	a.cfg.LLM.Provider = "missing-provider"
	if err := a.webSaveProvider(context.Background(), "deepseek-official", "temporary-key", "https://changed.example/v1", "changed-model", nil); err == nil {
		t.Fatal("provider edit with an unresolvable selected route should fail")
	}
	settings, err := st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings["llm.key.deepseek-official"] != "" || settings["llm.profile.deepseek-official"] != "" {
		t.Fatalf("failed provider edit left settings: %#v", settings)
	}
	if _, err := a.llmReg.Get("deepseek-official"); err != nil {
		t.Fatalf("old registry disappeared after failed edit: %v", err)
	}
}

// TestWebCustomProviderValidation verifies the M11 fail-closed rules: id/name/
// base_url/model are required, a custom route cannot shadow a built-in, an
// invalid route is rejected, and deleting a built-in is rejected.
func TestWebCustomProviderValidation(t *testing.T) {
	a, _ := m11App(t)
	cases := []struct {
		name string
		edit func() error
	}{
		{"missing fields", func() error { return a.webSaveCustomProvider(context.Background(), "x", "", "", "", "", "", nil) }},
		{"builtin shadow", func() error {
			return a.webSaveCustomProvider(context.Background(), "openai", "X", "http://x/v1", "m", "", "", nil)
		}},
		{"invalid route", func() error {
			return a.webSaveCustomProvider(context.Background(), "Bad Route!", "X", "http://x/v1", "m", "", "", nil)
		}},
		{"trailing dash", func() error {
			return a.webSaveCustomProvider(context.Background(), "bad-", "X", "http://x/v1", "m", "", "", nil)
		}},
		{"doubled dash", func() error {
			return a.webSaveCustomProvider(context.Background(), "bad--route", "X", "http://x/v1", "m", "", "", nil)
		}},
		{"digit leading", func() error {
			return a.webSaveCustomProvider(context.Background(), "1st-route", "X", "http://x/v1", "m", "", "", nil)
		}},
		{"unknown protocol", func() error {
			return a.webSaveCustomProvider(context.Background(), "my-llm", "X", "http://x/v1", "m", "", "bogus-protocol", nil)
		}},
		{"delete builtin", func() error { return a.webDeleteCustomProvider(context.Background(), "deepseek-official") }},
		{"delete unknown", func() error { return a.webDeleteCustomProvider(context.Background(), "nope") }},
	}
	for _, tc := range cases {
		if err := tc.edit(); err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
		}
	}
	// Valid route characters are accepted.
	if err := a.webSaveCustomProvider(context.Background(), "my-llm-2", "X", "http://x/v1", "m", "", "", nil); err != nil {
		t.Errorf("valid route rejected: %v", err)
	}
}

func TestWebDeleteCustomProviderRollsBackOnRegistryFailure(t *testing.T) {
	a, st := m11App(t)
	if err := a.webSaveCustomProvider(context.Background(), "rollback-provider", "Rollback", "http://gw.example/v1", "model-1", "rollback-key", "", nil); err != nil {
		t.Fatalf("webSaveCustomProvider: %v", err)
	}

	// Make the provider being deleted the selected provider. Once removed, the
	// registry build must fail; the delete operation must then restore both the
	// settings rows and the live registry snapshot.
	a.providerMu.Lock()
	a.cfg.LLM.Provider = "rollback-provider"
	a.providerMu.Unlock()
	if err := a.webDeleteCustomProvider(context.Background(), "rollback-provider"); err == nil {
		t.Fatal("deleting the selected provider should fail closed")
	}
	settings, err := st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings["llm.custom.rollback-provider"] == "" || settings["llm.key.rollback-provider"] != "rollback-key" {
		t.Fatalf("provider settings after rollback = %#v", settings)
	}
	a.providerMu.RLock()
	providerCount := len(a.customProviders)
	a.providerMu.RUnlock()
	if providerCount != 1 {
		t.Fatalf("custom provider count after rollback = %d, want 1", providerCount)
	}
	if _, err := a.llmReg.Get("rollback-provider"); err != nil {
		t.Fatalf("live registry after rollback: %v", err)
	}
}

// TestWebProvidersListsDormantBuiltins verifies M11 webProviders lists every
// directory built-in even when its env key is absent (dormant, so 增加提供方 can
// add them), the active deepseek stays configured, each entry carries its
// protocol and canonical env var, and a provider becomes registered as soon as
// its key is configured (by protocol).
func TestWebProvidersListsDormantBuiltins(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "env-key") // every other key absent
	a, _ := m11App(t)
	providers := a.webConfig()["providers"].([]map[string]any)

	seen := map[string]map[string]any{}
	for _, p := range providers {
		seen[p["id"].(string)] = p
	}
	// The full directory is listed, dormant (not registered/configured) except
	// deepseek, and every entry carries its protocol + env var.
	if len(seen) != len(builtinProviders) {
		t.Errorf("webProviders lists %d providers, want %d (full directory)", len(seen), len(builtinProviders))
	}
	for _, bp := range builtinProviders {
		p, ok := seen[bp.id]
		if !ok {
			t.Errorf("built-in provider %q missing from webProviders", bp.id)
			continue
		}
		if p["protocol"] != string(bp.protocol) {
			t.Errorf("%s protocol = %v, want %s", bp.id, p["protocol"], bp.protocol)
		}
		if p["env_var"] != bp.env {
			t.Errorf("%s env_var = %v, want %s", bp.id, p["env_var"], bp.env)
		}
	}
	if seen["deepseek-official"]["registered"] != true || seen["deepseek-official"]["configured"] != true {
		t.Fatalf("deepseek should be registered+configured, got %#v", seen["deepseek-official"])
	}
	for _, id := range []string{"openai", "anthropic", "google", "xai", "groq"} {
		if seen[id]["registered"] != false || seen[id]["configured"] != false {
			t.Fatalf("%s should be dormant (not registered, not configured), got %#v", id, seen[id])
		}
	}
	// openai/anthropic stay dormant: their keys were never set.
	if seen["openai"]["model"] != "gpt-4o" || seen["openai"]["base_url"] != "https://api.openai.com/v1" {
		t.Fatalf("openai should keep its config-driven model/base_url, got %#v", seen["openai"])
	}
}

// TestWebProvidersRegisterByProtocol verifies a directory provider becomes
// registered through the adapter that speaks its protocol once a key is
// configured (M11-pi-ai 四协议): a completions provider (groq), a messages
// provider (minimax), the Gemini provider (google) and a Responses provider
// (xai) all register; the env-var form and the settings-override form both work.
func TestWebProvidersRegisterByProtocol(t *testing.T) {
	a, st := m11App(t)

	// Env-var key only → provider registers, stays configured.
	t.Setenv("GROQ_API_KEY", "groq-key")
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM after GROQ_API_KEY: %v", err)
	}
	if p, err := a.llmReg.Get("groq"); err != nil {
		t.Fatalf("groq not registered: %v", err)
	} else if !p.Available() {
		t.Fatal("groq should be available with its env key")
	}
	if op := findProvider(a.webConfig()["providers"].([]map[string]any), "groq"); op == nil || op["registered"] != true || op["configured"] != true {
		t.Fatalf("groq should be registered+configured, got %#v", op)
	}

	// Settings override key (llm.key.<id>) for a messages-protocol provider.
	if err := a.webSaveProvider(context.Background(), "minimax", "minimax-ui-key", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider(minimax): %v", err)
	}
	if p, err := a.llmReg.Get("minimax"); err != nil {
		t.Fatalf("minimax not registered: %v", err)
	} else if p.ID() != "minimax" || !p.Available() {
		t.Fatalf("minimax registered under wrong id or unavailable: %v", p.ID())
	}
	if got, _ := st.GetSettings(context.Background()); got["llm.key.minimax"] != "minimax-ui-key" {
		t.Fatalf("llm.key.minimax = %q, want minimax-ui-key", got["llm.key.minimax"])
	}

	// Gemini protocol provider registers via its env var.
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM after GEMINI_API_KEY: %v", err)
	}
	if p, err := a.llmReg.Get("google"); err != nil {
		t.Fatalf("google not registered: %v", err)
	} else if !p.Available() {
		t.Fatal("google should be available with its env key")
	}

	// Responses protocol provider registers via its env var.
	t.Setenv("XAI_API_KEY", "xai-key")
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM after XAI_API_KEY: %v", err)
	}
	if p, err := a.llmReg.Get("xai"); err != nil {
		t.Fatalf("xai not registered: %v", err)
	} else if !p.Available() {
		t.Fatal("xai should be available with its env key")
	}

	// Removing the override returns the provider to dormant.
	if err := a.webSaveProvider(context.Background(), "minimax", "", "", "", nil); err != nil {
		t.Fatalf("webSaveProvider(minimax clear): %v", err)
	}
	if p, err := a.llmReg.Get("minimax"); err == nil || p != nil {
		t.Fatalf("minimax should be back to dormant after clearing, got %v", err)
	}
}

// TestProviderEnvSpecialCases verifies the directory's canonical env vars are
// honored (HF_TOKEN, KIMI_API_KEY, AI_GATEWAY_API_KEY — not the derived
// <UPPER>_API_KEY form).
func TestProviderEnvSpecialCases(t *testing.T) {
	cases := map[string]string{
		"huggingface":       "HF_TOKEN",
		"kimi-coding":       "KIMI_API_KEY",
		"vercel-ai-gateway": "AI_GATEWAY_API_KEY",
		"deepseek-official": "DEEPSEEK_API_KEY",
		"groq":              "GROQ_API_KEY",
		"custom-route":      "CUSTOM_ROUTE_API_KEY",
	}
	for id, want := range cases {
		if got := providerEnv(id); got != want {
			t.Errorf("providerEnv(%s) = %q, want %q", id, got, want)
		}
	}
}

// TestWebCustomProviderProtocolDispatch verifies the M11-pi-ai protocol field:
// a custom provider declared with a non-completions protocol registers through
// the matching adapter (anthropic-messages → anthropic adapter under the custom
// route; google-generative-ai → google adapter), the profile persists the
// protocol, webProviders surfaces protocol + protocol_label, and an empty
// display name defaults to the route id (dsh CustomProviderCard 范式).
func TestWebCustomProviderProtocolDispatch(t *testing.T) {
	a, st := m11App(t)

	// anthropic-messages custom provider with a key override.
	if err := a.webSaveCustomProvider(context.Background(), "my-messages", "", "https://gw.example/anthropic", "claude-sonnet-4-5", "msg-key", "anthropic-messages", nil); err != nil {
		t.Fatalf("webSaveCustomProvider(messages): %v", err)
	}
	// Display name defaults to the route id.
	got, _ := st.GetSettings(context.Background())
	var cp customProviderProfile
	if err := json.Unmarshal([]byte(got["llm.custom.my-messages"]), &cp); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	if cp.Name != "my-messages" || cp.Protocol != "anthropic-messages" {
		t.Fatalf("profile = %#v, want name defaulted to route + protocol persisted", cp)
	}
	// Registered under its route via the anthropic adapter.
	p, err := a.llmReg.Get("my-messages")
	if err != nil {
		t.Fatalf("custom provider not registered: %v", err)
	}
	if p.ID() != "my-messages" || !p.Available() {
		t.Fatalf("custom provider id = %q available=%v", p.ID(), p.Available())
	}
	// webProviders surfaces protocol + label.
	op := findProvider(a.webConfig()["providers"].([]map[string]any), "my-messages")
	if op == nil {
		t.Fatal("custom provider missing from webProviders")
	}
	if op["protocol"] != "anthropic-messages" || op["protocol_label"] != "Anthropic Messages" {
		t.Fatalf("protocol view = %#v", op)
	}
	if op["name"] != "my-messages" {
		t.Fatalf("name = %v, want defaulted route", op["name"])
	}

	// google-generative-ai custom provider (no key override; env fallback).
	t.Setenv("MY_GEMINI_API_KEY", "gem-key")
	if err := a.webSaveCustomProvider(context.Background(), "my-gemini", "My Gemini", "https://gem.example", "gemini-2.5-flash", "", "google-generative-ai", nil); err != nil {
		t.Fatalf("webSaveCustomProvider(gemini): %v", err)
	}
	if p, err := a.llmReg.Get("my-gemini"); err != nil {
		t.Fatalf("gemini custom provider not registered: %v", err)
	} else if !p.Available() {
		t.Fatal("gemini custom provider should be available with its env key")
	}
	og := findProvider(a.webConfig()["providers"].([]map[string]any), "my-gemini")
	if og == nil || og["protocol_label"] != "Google Generative AI" {
		t.Fatalf("gemini protocol view = %#v", og)
	}

	// Delete both; registry clears.
	if err := a.webDeleteCustomProvider(context.Background(), "my-messages"); err != nil {
		t.Fatalf("delete my-messages: %v", err)
	}
	if _, err := a.llmReg.Get("my-messages"); err == nil {
		t.Fatal("my-messages should be gone")
	}
}

// TestWebCustomProviderMultiModel verifies the M11-pi-ai multi-model list: a
// custom provider persists its models array, the effective default model is the
// first entry (dsh ModelListEditor: the list is authoritative), webProviders
// surfaces the models view, and an empty model list is rejected while a legacy
// single model still works.
func TestWebCustomProviderMultiModel(t *testing.T) {
	a, st := m11App(t)
	models := []customModel{
		{ID: "gpt-4o-mini", Name: "GPT-4o mini", ContextWindow: 128000, MaxTokens: 16384},
		{ID: "gpt-4o"},
	}
	if err := a.webSaveCustomProvider(context.Background(), "my-multi", "My Multi", "http://gw.example/v1", "", "multi-key", "openai-completions", models); err != nil {
		t.Fatalf("webSaveCustomProvider(multi): %v", err)
	}
	got, _ := st.GetSettings(context.Background())
	var cp customProviderProfile
	if err := json.Unmarshal([]byte(got["llm.custom.my-multi"]), &cp); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	if len(cp.Models) != 2 || cp.Models[0].ID != "gpt-4o-mini" || cp.Models[0].ContextWindow != 128000 {
		t.Fatalf("persisted models = %#v", cp.Models)
	}
	// Effective default model = first list entry.
	if cp.Model != "gpt-4o-mini" {
		t.Fatalf("effective model = %q, want first list entry", cp.Model)
	}
	// Registered with the default model.
	p, err := a.llmReg.Get("my-multi")
	if err != nil {
		t.Fatalf("custom provider not registered: %v", err)
	}
	if p.ID() != "my-multi" || !p.Available() {
		t.Fatalf("registered id=%q available=%v", p.ID(), p.Available())
	}
	// webProviders surfaces the models array.
	op := findProvider(a.webConfig()["providers"].([]map[string]any), "my-multi")
	if op == nil {
		t.Fatal("custom provider missing from webProviders")
	}
	om, ok := op["models"].([]customModel)
	if !ok || len(om) != 2 || om[1].ID != "gpt-4o" {
		t.Fatalf("webProviders models view = %#v (%T)", op["models"], op["models"])
	}
	if op["model"] != "gpt-4o-mini" {
		t.Fatalf("webProviders model = %v", op["model"])
	}

	// Empty model list + no legacy model → rejected.
	if err := a.webSaveCustomProvider(context.Background(), "my-empty", "X", "http://x/v1", "", "", "", nil); err == nil {
		t.Fatal("empty model list should be rejected")
	}
	// Blank-id rows are dropped; a surviving list still saves.
	if err := a.webSaveCustomProvider(context.Background(), "my-clean", "X", "http://x/v1", "", "", "", []customModel{{ID: "  "}, {ID: "real"}}); err != nil {
		t.Fatalf("clean models: %v", err)
	}
	got2, _ := st.GetSettings(context.Background())
	var cp2 customProviderProfile
	if err := json.Unmarshal([]byte(got2["llm.custom.my-clean"]), &cp2); err != nil {
		t.Fatal(err)
	}
	if len(cp2.Models) != 1 || cp2.Models[0].ID != "real" || cp2.Model != "real" {
		t.Fatalf("cleaned models = %#v", cp2.Models)
	}
}

func findProvider(list []map[string]any, id string) map[string]any {
	for _, p := range list {
		if p["id"] == id {
			return p
		}
	}
	return nil
}

// TestContextWindowOf verifies the ContextMeter budget resolution (dsh
// resolveModelInfo): the configured model-directory entry's capacity wins over
// the one composition-root fallback (1,000,000, dsh DEFAULT_CONTEXT_WINDOW),
// and the per-session selection beats the globals. The webserver no longer owns
// a second unknown-model default.
func TestContextWindowOf(t *testing.T) {
	a, st := m11App(t)
	ctx := context.Background()

	// Global model: the DeepSeek V4 catalog default (1M).
	a.cfg.Model = "deepseek-v4-flash"
	if w := a.contextWindowOf(""); w != 1000000 {
		t.Fatalf("global v4 window = %d, want 1000000", w)
	}
	// A model outside the catalog → 0 (webserver default applies).
	a.cfg.Model = "deepseek-chat"
	if w := a.contextWindowOf(""); w != 1000000 {
		t.Fatalf("unknown model window = %d, want 1000000", w)
	}

	// Per-session provider+model selection is honored.
	if err := st.CreateSession(ctx, "s1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionConfig(ctx, "s1", store.SessionConfig{Provider: "deepseek-official", Model: "deepseek-v4-pro"}); err != nil {
		t.Fatal(err)
	}
	if w := a.contextWindowOf("s1"); w != 1000000 {
		t.Fatalf("session v4 window = %d, want 1000000", w)
	}

	// A built-in profile model-directory entry wins over the catalog for its
	// own models only.
	a.builtinProfiles = map[string]builtinProviderProfile{
		"deepseek-official": {Models: []customModel{{ID: "my-custom-1", ContextWindow: 64000}}},
	}
	if w := a.contextWindowOf("s1"); w != 1000000 {
		t.Fatalf("v4-pro with profile = %d, want 1000000 (no directory entry)", w)
	}
	if err := st.SetSessionConfig(ctx, "s1", store.SessionConfig{Provider: "deepseek-official", Model: "my-custom-1"}); err != nil {
		t.Fatal(err)
	}
	if w := a.contextWindowOf("s1"); w != 64000 {
		t.Fatalf("profile directory window = %d, want 64000", w)
	}

	// A custom provider's model list is the directory for its models.
	a.customProviders = []customProviderProfile{
		{ID: "my-gw", Name: "My Gateway", Models: []customModel{{ID: "gw-a"}, {ID: "gw-b", ContextWindow: 32000}}},
	}
	if err := st.SetSessionConfig(ctx, "s1", store.SessionConfig{Provider: "my-gw", Model: "gw-b"}); err != nil {
		t.Fatal(err)
	}
	if w := a.contextWindowOf("s1"); w != 32000 {
		t.Fatalf("custom directory window = %d, want 32000", w)
	}
	if err := st.SetSessionConfig(ctx, "s1", store.SessionConfig{Provider: "my-gw", Model: "gw-a"}); err != nil {
		t.Fatal(err)
	}
	if w := a.contextWindowOf("s1"); w != 1000000 {
		t.Fatalf("capacity-less custom model window = %d, want 1000000", w)
	}

	// The same fallback must reach loop assembly for context. The model's output
	// capacity is deliberately not promoted into a request cap.
	route := a.buildLoop(func(string) {}, func(error) {}, "s1", "my-gw", "gw-a", "", "standard", nil)
	if got := route.ContextWindow(); got != 1000000 {
		t.Fatalf("loop fallback window = %d, want 1000000", got)
	}
}

func TestModelCapabilityDrivesLoopRoute(t *testing.T) {
	a, st := m11App(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "catalog-route", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	a.customProviders = []customProviderProfile{{
		ID: "my-gw", Name: "My Gateway", Model: "gw-a",
		Models: []customModel{{
			ID: "gw-a", ContextWindow: 32000, MaxTokens: 1024,
			Reasoning: &yes, Tools: &no, Vision: &yes, Audio: &no,
		}},
	}}
	if err := st.SetSessionConfig(ctx, "catalog-route", store.SessionConfig{
		Provider: "my-gw", Model: "gw-a",
	}); err != nil {
		t.Fatal(err)
	}

	capability := a.modelCapabilityFor("catalog-route")
	if capability.ContextWindow != 32000 || capability.MaxTokens != 1024 ||
		!capability.Reasoning || capability.Tools || !capability.Vision || capability.Audio {
		t.Fatalf("catalog capability = %+v", capability)
	}

	route := a.buildLoop(func(string) {}, func(error) {}, "catalog-route", "my-gw", "gw-a", "", "standard", nil)
	if route.MaxTokens() != 0 || route.ContextWindow() != 32000 {
		t.Fatalf("loop route limits = max %d/context %d, want 0/32000", route.MaxTokens(), route.ContextWindow())
	}
	if specs := modelToolSpecs(capability, "standard", []llm.ToolSchema{{Name: "read"}}); len(specs) != 0 {
		t.Fatalf("tool-disabled route specs = %#v, want empty", specs)
	}
}

func TestValidateSessionModelSelectionUsesRuntimeAdmission(t *testing.T) {
	a, st := m11App(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "model-validator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := a.validateSessionModelSelection(ctx, "model-validator", "deepseek-official", "deepseek-v4-flash", "high"); err != nil {
		t.Fatalf("available route rejected: %v", err)
	}
	err := a.validateSessionModelSelection(ctx, "model-validator", "dormant-provider", "model", "")
	if !errors.Is(err, llm.ErrProviderUnavailable) {
		t.Fatalf("unavailable provider error = %v, want ErrProviderUnavailable", err)
	}
	err = a.validateSessionModelSelection(ctx, "model-validator", "deepseek-official", "deepseek-v4-flash", "sometimes")
	if err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("invalid effort error = %v", err)
	}
}

func TestModelSelectionRejectsExplicitUnsupportedReasoning(t *testing.T) {
	a, st := m11App(t)
	ctx := context.Background()
	if err := st.CreateSession(ctx, "no-reasoning", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	a.customProviders = []customProviderProfile{{
		ID: "my-gw", Name: "My Gateway", Model: "basic",
		Models: []customModel{{ID: "basic", ContextWindow: 8000, Reasoning: &no, Tools: &yes, Vision: &no, Audio: &no}},
	}}
	capability := a.modelCapabilityForRoute("my-gw", "basic")
	err := validateModelCapabilityForSelection(capability, "high")
	if !errors.Is(err, llm.ErrCapabilityUnavailable) {
		t.Fatalf("unsupported reasoning error = %v, want ErrCapabilityUnavailable", err)
	}
	if got := effectiveModelReasoningEffort(a.modelCapabilityForRoute("my-gw", "basic"), "high"); got != "" {
		t.Fatalf("fallback effort = %q, want empty", got)
	}
	if err := validateModelCapabilityForSelection(capability, ""); err != nil {
		t.Fatalf("clearing effort rejected: %v", err)
	}
}
