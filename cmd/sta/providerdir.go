// providerdir.go — the built-in provider directory (M11-pi-ai, 对齐 dsh
// llm-pi-ai catalogProviderIds). This is the source of truth for the Model
// settings page: every provider pi-ai can authenticate with an API key is
// listed here, tagged with its wire protocol, so 增加提供方 offers the same
// dormant providers dsh does and registerLLM can wire each by protocol.
//
// Four protocols are supported (pi-ai 范式, user 2026-09): openai-completions
// (OpenAI-compatible chat.completions SSE), anthropic-messages (Messages SSE),
// google-generative-ai (streamGenerateContent?alt=sse), and openai-responses
// (POST /responses SSE). reasoning/thinking travels through the shared
// llm.StreamReasoningDelta channel in every protocol.
//
// Default models are honest suggestions for the add card (the editor lets the
// user type any model); they mirror the current mainstream model each provider
// ships. baseURL values come from the pi-ai catalog (packages/ai/src/providers).
package main

import "strings"

// providerProtocol is one wire protocol a provider speaks.
type providerProtocol string

const (
	protocolCompletions providerProtocol = "openai-completions"   // openai.New (deepseek-compatible SSE)
	protocolMessages    providerProtocol = "anthropic-messages"   // anthropic.New (Messages SSE)
	protocolGemini      providerProtocol = "google-generative-ai" // google.New (streamGenerateContent SSE)
	protocolResponses   providerProtocol = "openai-responses"     // openairesponses.New (/responses SSE)
)

// builtinProvider is one directory entry.
type builtinProvider struct {
	id       string
	name     string
	protocol providerProtocol
	env      string // canonical credential env var
	baseURL  string // default base URL; "" means the adapter's default
	model    string // suggested default model
}

// builtinProviders is the full directory, in pi-ai catalog order (OpenAI
// completions family first, then Anthropic Messages, Gemini, Responses).
// deepseek/openai/anthropic keep their config-driven base_url/model (config.yaml
// llm.* sections); every other entry carries the pi-ai catalog default.
var builtinProviders = []builtinProvider{
	// openai-completions family (OpenAI-compatible SSE, openai adapter).
	{id: "openai", name: "OpenAI", protocol: protocolCompletions, env: "OPENAI_API_KEY", baseURL: "https://api.openai.com/v1", model: "gpt-4o-mini"},
	{id: "openrouter", name: "OpenRouter", protocol: protocolCompletions, env: "OPENROUTER_API_KEY", baseURL: "https://openrouter.ai/api/v1", model: "openai/gpt-4o-mini"},
	{id: "together", name: "Together", protocol: protocolCompletions, env: "TOGETHER_API_KEY", baseURL: "https://api.together.ai/v1", model: "meta-llama/Llama-3.3-70B-Instruct-Turbo"},
	{id: "groq", name: "Groq", protocol: protocolCompletions, env: "GROQ_API_KEY", baseURL: "https://api.groq.com/openai/v1", model: "llama-3.3-70b-versatile"},
	{id: "mistral", name: "Mistral", protocol: protocolCompletions, env: "MISTRAL_API_KEY", baseURL: "https://api.mistral.ai/v1", model: "mistral-large-latest"},
	{id: "nvidia", name: "NVIDIA NIM", protocol: protocolCompletions, env: "NVIDIA_API_KEY", baseURL: "https://integrate.api.nvidia.com/v1", model: "meta/llama-3.3-70b-instruct"},
	{id: "cerebras", name: "Cerebras", protocol: protocolCompletions, env: "CEREBRAS_API_KEY", baseURL: "https://api.cerebras.ai/v1", model: "llama-3.3-70b"},
	{id: "baseten", name: "Baseten", protocol: protocolCompletions, env: "BASETEN_API_KEY", baseURL: "https://inference.baseten.co/v1", model: ""},
	{id: "huggingface", name: "Hugging Face", protocol: protocolCompletions, env: "HF_TOKEN", baseURL: "https://router.huggingface.co/v1", model: "meta-llama/Llama-3.3-70B-Instruct"},
	{id: "zai", name: "Z.AI", protocol: protocolCompletions, env: "ZAI_API_KEY", baseURL: "https://api.z.ai/api/coding/paas/v4", model: "glm-z1-32b-coding"},
	{id: "zai-coding-cn", name: "Z.AI Coding CN", protocol: protocolCompletions, env: "ZAI_CODING_CN_API_KEY", baseURL: "https://open.bigmodel.cn/api/coding/paas/v4", model: "glm-z1-32b-coding"},
	{id: "moonshotai", name: "Moonshot AI", protocol: protocolCompletions, env: "MOONSHOT_API_KEY", baseURL: "https://api.moonshot.ai/v1", model: "kimi-k2"},
	{id: "moonshotai-cn", name: "Moonshot AI CN", protocol: protocolCompletions, env: "MOONSHOT_API_KEY", baseURL: "https://api.moonshot.cn/v1", model: "kimi-k2"},
	{id: "qwen-token-plan", name: "Qwen Token Plan", protocol: protocolCompletions, env: "QWEN_TOKEN_PLAN_API_KEY", baseURL: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", model: "qwen3-coder-plus"},
	{id: "qwen-token-plan-cn", name: "Qwen Token Plan CN", protocol: protocolCompletions, env: "QWEN_TOKEN_PLAN_CN_API_KEY", baseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", model: "qwen3-coder-plus"},
	{id: "xiaomi", name: "Xiaomi MiMo", protocol: protocolCompletions, env: "XIAOMI_API_KEY", baseURL: "https://api.xiaomimimo.com/v1", model: "MiMo-7B-RL"},
	{id: "ant-ling", name: "Ant Ling", protocol: protocolCompletions, env: "ANT_LING_API_KEY", baseURL: "https://api.ant-ling.com/v1", model: "antling-3.5"},
	{id: "fireworks", name: "Fireworks", protocol: protocolCompletions, env: "FIREWORKS_API_KEY", baseURL: "https://api.fireworks.ai/inference/v1", model: "accounts/fireworks/models/llama-v3p3-70b-instruct"},
	{id: "deepseek-official", name: "DeepSeek", protocol: protocolCompletions, env: "DEEPSEEK_API_KEY", baseURL: "https://api.deepseek.com", model: "deepseek-v4-flash"},

	// anthropic-messages family (Messages SSE, anthropic adapter).
	{id: "anthropic", name: "Anthropic", protocol: protocolMessages, env: "ANTHROPIC_API_KEY", baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-4-5"},
	{id: "minimax", name: "MiniMax", protocol: protocolMessages, env: "MINIMAX_API_KEY", baseURL: "https://api.minimax.io/anthropic", model: "MiniMax-M2"},
	{id: "minimax-cn", name: "MiniMax CN", protocol: protocolMessages, env: "MINIMAX_CN_API_KEY", baseURL: "https://api.minimaxi.com/anthropic", model: "MiniMax-M2"},
	{id: "kimi-coding", name: "Kimi For Coding", protocol: protocolMessages, env: "KIMI_API_KEY", baseURL: "https://api.kimi.com/coding", model: "kimi-k2"},
	{id: "vercel-ai-gateway", name: "Vercel AI Gateway", protocol: protocolMessages, env: "AI_GATEWAY_API_KEY", baseURL: "https://ai-gateway.vercel.sh", model: "anthropic/claude-sonnet-4-5"},

	// google-generative-ai family (Gemini streamGenerateContent, google adapter).
	{id: "google", name: "Google Gemini", protocol: protocolGemini, env: "GEMINI_API_KEY", baseURL: "https://generativelanguage.googleapis.com/v1beta", model: "gemini-2.5-flash"},

	// openai-responses family (/responses SSE, openairesponses adapter).
	{id: "xai", name: "xAI", protocol: protocolResponses, env: "XAI_API_KEY", baseURL: "https://api.x.ai/v1", model: "grok-4"},
}

// builtinProviderByID indexes the directory by id.
func builtinProviderByID(id string) (builtinProvider, bool) {
	for _, p := range builtinProviders {
		if p.id == id {
			return p, true
		}
	}
	return builtinProvider{}, false
}

// providerEnv returns the canonical credential env var for provider id:
// the directory's declared env when known, otherwise the derived
// <UPPER_ROUTE>_API_KEY (custom providers, 纪律 6).
func providerEnv(id string) string {
	if p, ok := builtinProviderByID(id); ok && p.env != "" {
		return p.env
	}
	return strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_API_KEY"
}

// protocolLabel returns the human label for a provider protocol (shown in the
// settings page route line).
func protocolLabel(p providerProtocol) string {
	switch p {
	case protocolCompletions:
		return "OpenAI 兼容"
	case protocolMessages:
		return "Anthropic Messages"
	case protocolGemini:
		return "Google Generative AI"
	case protocolResponses:
		return "OpenAI Responses"
	}
	return string(p)
}
