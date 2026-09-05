package main

import (
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
)

func TestPersistedModelSelectionRoundTrip(t *testing.T) {
	raw, err := encodePersistedModelSelection("openai", "gpt-5", "high")
	if err != nil {
		t.Fatalf("encode selection: %v", err)
	}
	got, ok := parsePersistedModelSelection(raw)
	if !ok {
		t.Fatalf("parse selection failed for %q", raw)
	}
	if got.Provider != "openai" || got.Model != "gpt-5" || got.ReasoningEffort != "high" {
		t.Fatalf("selection = %+v", got)
	}
}

func TestApplyModelSelectionToConfigUsesProviderModelField(t *testing.T) {
	cfg := config.Config{
		Model: "old-deepseek",
		LLM: config.LLMConfig{
			Provider: "deepseek-official",
			OpenAI:   config.OpenAIProviderConfig{Model: "old-openai"},
		},
	}
	applyModelSelectionToConfig(&cfg, persistedModelSelection{
		Provider:        "openai",
		Model:           "gpt-5",
		ReasoningEffort: "low",
	})
	if cfg.LLM.Provider != "openai" || cfg.LLM.OpenAI.Model != "gpt-5" || cfg.ReasoningEffort != "low" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Model != "old-deepseek" {
		t.Fatalf("deepseek model changed unexpectedly: %q", cfg.Model)
	}
}

func TestParsePersistedModelSelectionRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"provider":"openai"}`,
		`{"provider":"openai","model":"gpt-5","reasoningEffort":"invalid"}`,
		`not-json`,
	} {
		if _, ok := parsePersistedModelSelection(raw); ok {
			t.Fatalf("parse accepted invalid selection %q", raw)
		}
	}
}
