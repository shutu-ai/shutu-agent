package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed model_catalog_generated.json
var generatedModelCatalogBytes []byte

// generatedModelCatalogSource pins the upstream inventory that supplied the
// embedded rows. A package version change is a model-fact baseline change.
const generatedModelCatalogSource = "@earendil-works/pi-ai@0.82.1"

type generatedModelCatalogFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	Source        string `json:"source"`
	Providers     []struct {
		ID     string        `json:"id"`
		Models []customModel `json:"models"`
	} `json:"providers"`
}

// builtinModelCatalog is the single source for built-in candidate IDs and the
// capacity/capability facts we can declare without a provider probe. It starts
// from the pinned upstream installed catalog, then applies the hand-curated
// rows below only where Shutu owns an additional fact (for example DeepSeek
// request defaults or protocol-level tool support). Unknown values are omitted
// so runtime fallbacks remain explicit; adapters and the picker still accept
// free-form models where the provider supports them.
var builtinModelCatalog = map[string][]customModel{
	"openai": {
		{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000, MaxTokens: 16384,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "gpt-4o-mini", Name: "GPT-4o mini", ContextWindow: 128000, MaxTokens: 16384,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "gpt-4.1", Name: "GPT-4.1", ContextWindow: 1047576, MaxTokens: 32768,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "gpt-4.1-mini", Name: "GPT-4.1 mini", ContextWindow: 1047576, MaxTokens: 32768,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
	},
	"openrouter": {
		{ID: "openai/gpt-4o-mini", Name: "OpenAI: GPT-4o-mini", ContextWindow: 128000, MaxTokens: 16384,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "openai/gpt-4o", Name: "OpenAI: GPT-4o", ContextWindow: 128000, MaxTokens: 16384,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "anthropic/claude-sonnet-4-5"}, {ID: "deepseek/deepseek-chat"},
	},
	"together": {
		{ID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Name: "Llama 3.3 70B", ContextWindow: 131072, MaxTokens: 131072,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "meta-llama/Llama-3.1-8B-Instruct-Turbo"},
		{ID: "Qwen/Qwen2.5-72B-Instruct-Turbo"},
	},
	"groq": {
		{ID: "llama-3.3-70b-versatile", Name: "Llama 3.3 70B", ContextWindow: 131072, MaxTokens: 32768,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "llama-3.1-8b-instant", Name: "Llama 3.1 8B", ContextWindow: 131072, MaxTokens: 131072,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "mixtral-8x7b-32768"},
	},
	"mistral": {
		{ID: "mistral-large-latest", Name: "Mistral Large (latest)", ContextWindow: 262144, MaxTokens: 262144,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "mistral-small-latest", Name: "Mistral Small (latest)", ContextWindow: 256000, MaxTokens: 256000,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "open-mistral-nemo", Name: "Open Mistral Nemo", ContextWindow: 128000, MaxTokens: 128000,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
	},
	"nvidia": {
		{ID: "meta/llama-3.3-70b-instruct", Name: "Llama 3.3 70b Instruct", ContextWindow: 128000, MaxTokens: 4096,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "meta/llama-3.1-8b-instruct", Name: "Llama 3.1 8B Instruct", ContextWindow: 16000, MaxTokens: 4096,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "deepseek-ai/deepseek-r1"},
	},
	"cerebras": {
		{ID: "llama-3.3-70b"}, {ID: "llama-3.1-8b"}, {ID: "llama-3.1-70b"},
	},
	// Baseten is a deployment platform: its model is fully user-defined.
	"huggingface": {
		{ID: "meta-llama/Llama-3.3-70B-Instruct", Name: "Llama-3.3-70B-Instruct", ContextWindow: 131072, MaxTokens: 4096,
			Input: []string{"text"}, Reasoning: catalogBool(false), Vision: catalogBool(false), Audio: catalogBool(false)},
		{ID: "mistralai/Mistral-7B-Instruct-v0.3"},
	},
	"zai": {
		{ID: "glm-z1-32b-coding"}, {ID: "glm-4.5-air"},
	},
	"zai-coding-cn": {
		{ID: "glm-z1-32b-coding"}, {ID: "glm-4.5-air"},
	},
	"moonshotai": {
		{ID: "kimi-k2"}, {ID: "kimi-latest"}, {ID: "moonshot-v1-8k"},
	},
	"moonshotai-cn": {
		{ID: "kimi-k2"}, {ID: "kimi-latest"}, {ID: "moonshot-v1-8k"},
	},
	"qwen-token-plan": {
		{ID: "qwen3-coder-plus"}, {ID: "qwen3-coder"}, {ID: "qwen-max-latest"},
	},
	"qwen-token-plan-cn": {
		{ID: "qwen3-coder-plus"}, {ID: "qwen3-coder"}, {ID: "qwen-max-latest"},
	},
	"xiaomi": {
		{ID: "MiMo-7B-RL"}, {ID: "MiMo-7B"},
	},
	"ant-ling": {
		{ID: "antling-3.5"}, {ID: "antling-4.5"},
	},
	"fireworks": {
		{ID: "accounts/fireworks/models/llama-v3p3-70b-instruct"},
		{ID: "accounts/fireworks/models/qwen3-coder-480b-a35b-instruct"},
	},
	"deepseek-official": {
		{
			ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash",
			ContextWindow: 1000000, MaxTokens: 384000, DefaultMaxTokens: 256000,
			Input:     []string{"text"},
			Reasoning: catalogBool(true), Tools: catalogBool(true),
			Vision: catalogBool(false), Audio: catalogBool(false),
		},
		{
			ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro",
			ContextWindow: 1000000, MaxTokens: 384000, DefaultMaxTokens: 256000,
			Input:     []string{"text"},
			Reasoning: catalogBool(true), Tools: catalogBool(true),
			Vision: catalogBool(false), Audio: catalogBool(false),
		},
	},
	"anthropic": {
		{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5 (latest)", ContextWindow: 1000000, MaxTokens: 64000,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "claude-opus-4-1", Name: "Claude Opus 4.1 (latest)", ContextWindow: 200000, MaxTokens: 32000,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5 (latest)", ContextWindow: 200000, MaxTokens: 64000,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
	},
	"minimax": {
		{ID: "MiniMax-M2"}, {ID: "MiniMax-Text-01"},
	},
	"minimax-cn": {
		{ID: "MiniMax-M2"}, {ID: "MiniMax-Text-01"},
	},
	"kimi-coding": {
		{ID: "kimi-k2"},
	},
	"vercel-ai-gateway": {
		{ID: "anthropic/claude-sonnet-4-5"}, {ID: "openai/gpt-4o"}, {ID: "anthropic/claude-opus-4-1"},
	},
	"google": {
		{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", ContextWindow: 1048576, MaxTokens: 65536,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextWindow: 1048576, MaxTokens: 65536,
			Input: []string{"text", "image"}, Reasoning: catalogBool(true), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
		{ID: "gemini-2.0-flash", Name: "Gemini 2.0 Flash", ContextWindow: 1048576, MaxTokens: 8192,
			Input: []string{"text", "image"}, Reasoning: catalogBool(false), Tools: catalogBool(true), Vision: catalogBool(true), Audio: catalogBool(false)},
	},
	"xai": {
		{ID: "grok-4"}, {ID: "grok-4-mini"}, {ID: "grok-3"},
	},
}

func init() {
	var file generatedModelCatalogFile
	if err := json.Unmarshal(generatedModelCatalogBytes, &file); err != nil {
		panic(fmt.Sprintf("sta: decode generated model catalog: %v", err))
	}
	if file.SchemaVersion != 1 || file.Source != generatedModelCatalogSource {
		panic(fmt.Sprintf("sta: unsupported generated model catalog %s version %d, want %s version 1", file.Source, file.SchemaVersion, generatedModelCatalogSource))
	}
	for _, provider := range file.Providers {
		builtinModelCatalog[provider.ID] = append(builtinModelCatalog[provider.ID], provider.Models...)
	}
	mergeCuratedModelFacts(file)
}

func generatedProviderModels(file *generatedModelCatalogFile, providerID string) ([]customModel, bool) {
	for index := range file.Providers {
		if file.Providers[index].ID == providerID {
			return file.Providers[index].Models, true
		}
	}
	return nil, false
}

// mergeCuratedModelFacts lets the checked-in hand rows add deployment facts or
// correct a pinned upstream row without discarding upstream effort maps and
// newly added routes. IDs remain the merge key; no duplicate model is exposed.
func mergeCuratedModelFacts(file generatedModelCatalogFile) {
	for providerID := range builtinModelCatalog {
		generated, ok := generatedProviderModels(&file, providerID)
		if !ok {
			continue
		}
		for _, override := range builtinModelCatalog[providerID] {
			for index := range generated {
				if generated[index].ID != override.ID {
					continue
				}
				generated[index] = mergeCustomModelFact(generated[index], override)
				break
			}
		}
		builtinModelCatalog[providerID] = generated
	}
}

func mergeCustomModelFact(base, override customModel) customModel {
	if override.Name != "" {
		base.Name = override.Name
	}
	if len(override.Input) != 0 {
		base.Input = append([]string(nil), override.Input...)
	}
	if override.ReasoningEfforts != nil {
		base.ReasoningEfforts = cloneReasoningEfforts(override.ReasoningEfforts)
	}
	if override.DefaultReasoningEffort != "" {
		base.DefaultReasoningEffort = override.DefaultReasoningEffort
	}
	if override.ContextWindow > 0 {
		base.ContextWindow = override.ContextWindow
	}
	if override.MaxTokens > 0 {
		base.MaxTokens = override.MaxTokens
	}
	if override.DefaultMaxTokens > 0 {
		base.DefaultMaxTokens = override.DefaultMaxTokens
	}
	if override.Reasoning != nil {
		base.Reasoning = catalogBool(*override.Reasoning)
	}
	if override.Tools != nil {
		base.Tools = catalogBool(*override.Tools)
	}
	if override.Vision != nil {
		base.Vision = catalogBool(*override.Vision)
	}
	if override.Audio != nil {
		base.Audio = catalogBool(*override.Audio)
	}
	return base
}

func catalogBool(value bool) *bool { return &value }

func builtinModelEntry(provider, model string) (customModel, bool) {
	for _, entry := range builtinModelCatalog[provider] {
		if entry.ID == model {
			return entry, true
		}
	}
	return customModel{}, false
}
