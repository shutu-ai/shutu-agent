package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/store"
)

func TestNativeCredentialUnsetRollsBackWhenProviderWouldBecomeUnavailable(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-test-key")
	t.Setenv("OPENAI_API_KEY", "")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SetSetting(ctx, "llm.key.openai", "persisted-openai-key"); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{Model: "gpt-4o-mini", LLM: config.LLMConfig{
			Provider: "openai",
			OpenAI:   config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"},
		}},
		store:   st,
		llmKeys: map[string]string{"openai": "persisted-openai-key"},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("initial provider registration: %v", err)
	}

	if err := a.nativeCredentialUnset(ctx, "OPENAI_API_KEY"); err == nil {
		t.Fatal("unsetting the selected provider credential must fail closed")
	}
	if got, ok := a.credentialOverride("openai"); !ok || got != "persisted-openai-key" {
		t.Fatalf("in-memory credential after rollback = %q, present=%v", got, ok)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings["llm.key.openai"] != "persisted-openai-key" {
		t.Fatalf("durable credential after rollback = %q", settings["llm.key.openai"])
	}
	if !a.hasLLMRegistry() || a.currentLLM() == nil {
		t.Fatal("provider registry was not restored after rejected credential change")
	}
}
