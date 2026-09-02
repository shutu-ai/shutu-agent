package main

import (
	"encoding/json"
	"strings"

	"github.com/jabing/shutu-agent/internal/config"
)

// defaultModelSettingKey mirrors DSH's agent-default-model Settings section.
// The value is stored as JSON in the local settings table because the Shutu
// host persists runtime settings there rather than rewriting config.yaml.
const defaultModelSettingKey = "agent_default_model"

type persistedModelSelection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

func parsePersistedModelSelection(raw string) (persistedModelSelection, bool) {
	var selection persistedModelSelection
	if json.Unmarshal([]byte(raw), &selection) != nil {
		return persistedModelSelection{}, false
	}
	selection.Provider = strings.TrimSpace(selection.Provider)
	selection.Model = strings.TrimSpace(selection.Model)
	selection.ReasoningEffort = strings.TrimSpace(selection.ReasoningEffort)
	if selection.Provider == "" || selection.Model == "" {
		return persistedModelSelection{}, false
	}
	switch selection.ReasoningEffort {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return selection, true
	default:
		return persistedModelSelection{}, false
	}
}

func encodePersistedModelSelection(provider, model, effort string) (string, error) {
	encoded, err := json.Marshal(persistedModelSelection{
		Provider:        strings.TrimSpace(provider),
		Model:           strings.TrimSpace(model),
		ReasoningEffort: strings.TrimSpace(effort),
	})
	return string(encoded), err
}

// applyModelSelectionToConfig applies the shared DSH default to the config
// view used by new sessions. Existing sessions still take precedence from
// their durable session config.
func applyModelSelectionToConfig(cfg *config.Config, selection persistedModelSelection) {
	cfg.LLM.Provider = selection.Provider
	switch selection.Provider {
	case "openai":
		cfg.LLM.OpenAI.Model = selection.Model
	case "anthropic":
		cfg.LLM.Anthropic.Model = selection.Model
	default:
		cfg.Model = selection.Model
	}
	cfg.ReasoningEffort = selection.ReasoningEffort
}
