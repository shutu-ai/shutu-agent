// llm.go — the M8-2 composition-root LLM wiring (dispatch-m8-2 §6 / M8-2b §3).
// This is where the multi-provider registry is built and the selected provider
// is injected into the REPL: registerLLM registers the deepseek provider
// (always; DEEPSEEK_API_KEY env-only), the openai provider (only when
// OPENAI_API_KEY is present), and the anthropic provider (only when
// ANTHROPIC_API_KEY is present, M8-2b), resolves cfg.LLM.Provider against the
// registry (unknown id ⇒ fail-closed startup error, no silent fallback), and
// injects the selected provider into a.llm — the single llm.LLM that the loop,
// compaction and subagent all consume. The registry is kept on app.llmReg for
// /llm-status, which reports provider/model/modalities. The loop's turn/step
// structure is untouched (D4):
// the loop keeps calling a.llm.Stream and never sees the registry.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/llm/anthropic"
	"github.com/jabing/shutu-agent/internal/llm/deepseek"
	"github.com/jabing/shutu-agent/internal/llm/google"
	"github.com/jabing/shutu-agent/internal/llm/openai"
	"github.com/jabing/shutu-agent/internal/llm/openairesponses"
	"github.com/jabing/shutu-agent/internal/llm/retry"
)

func wrapProvider(p llm.Provider, cfg *config.Config) llm.Provider {
	return retry.WrapProvider(p, retry.Config{
		MaxRetries:     cfg.LLM.Retry.MaxRetries,
		InitialBackoff: cfg.LLM.Retry.InitialBackoff.Duration,
		MaxBackoff:     cfg.LLM.Retry.MaxBackoff.Duration,
	})
}

// registerLLM builds the provider registry and injects the selected provider
// into a.llm. Fail-closed contract (dispatch-m8-2 §6):
//   - an unknown cfg.LLM.Provider is a startup error (no silent fallback);
//   - a selected provider whose credential is missing is a startup error too —
//     this preserves the M8-1-before behavior of a missing DEEPSEEK_API_KEY
//     failing at startup, made provider-aware (纪律 6: 凭证 env-only).
//
// Other registered providers may be unavailable (their key absent) — /llm-status
// reports them as such; only the selected one must be usable.
//
// M11 (增加提供方 / 增加自定义提供方, dsh-synced): every provider id can carry a
// configured API key persisted in the settings table (llm.key.<id>); a configured
// key wins over the env var (配置后以配置的为准, user 2026-09). Custom
// OpenAI-compatible providers are declared in settings (llm.custom.<id> JSON) and
// registered here under their route.
func (a *app) registerLLM() error {
	reg := llm.NewRegistry()

	// The deepseek provider is always registered; its parameters come from the
	// legacy top-level model/base_url (the deepseek default configuration,
	// dispatch-m8-2 §5) and DEEPSEEK_API_KEY from the environment (a configured
	// llm.key.deepseek-official setting wins, M11). A persisted
	// llm.profile.deepseek-official override (dsh ProviderEditor 自定义设置)
	// wins over config.yaml for base URL / model / model list.
	dsProfile := a.builtinProfile("deepseek-official")
	dsBaseURL, dsModel := a.cfg.BaseURL, a.cfg.Model
	if dsProfile.BaseURL != "" {
		dsBaseURL = dsProfile.BaseURL
	}
	if dsProfile.Model != "" {
		dsModel = dsProfile.Model
	}
	if err := reg.Register(wrapProvider(deepseek.New(deepseek.Config{
		APIKey:               a.providerKey("deepseek-official"),
		BaseURL:              dsBaseURL,
		Model:                dsModel,
		MaxRetries:           2,
		DisableRetry:         true,
		SupportsImages:       strings.Contains(a.cfg.LLM.ModelInputModalities, "image"),
		MaxRequestImageBytes: a.cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
	}), &a.cfg)); err != nil {
		return fmt.Errorf("pa: register deepseek provider: %w", err)
	}

	// The openai provider is registered only when its credential is present
	// (OPENAI_API_KEY env-only, dispatch-m8-2 §6; configured llm.key.openai
	// wins, M11). It reuses the deepseek OpenAI-compatible client — zero new
	// wire code (dispatch-m8-2 §4).
	if key := a.providerKey("openai"); key != "" {
		if err := reg.Register(wrapProvider(openai.New(openai.Config{
			APIKey:               key,
			BaseURL:              a.cfg.LLM.OpenAI.BaseURL,
			Model:                a.cfg.LLM.OpenAI.Model,
			DisableRetry:         true,
			SupportsImages:       strings.Contains(a.cfg.LLM.ModelInputModalities, "image"),
			MaxRequestImageBytes: a.cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
		}), &a.cfg)); err != nil {
			return fmt.Errorf("pa: register openai provider: %w", err)
		}
	}

	// The anthropic provider is registered only when its credential is present
	// (ANTHROPIC_API_KEY env-only, dispatch-m8-2b §3; configured llm.key.anthropic
	// wins, M11). Its parameters come from llm.anthropic.base_url/model
	// (defaults https://api.anthropic.com/v1 / claude-sonnet-4-5, M8-2b §3).
	if key := a.providerKey("anthropic"); key != "" {
		if err := reg.Register(wrapProvider(anthropic.New(anthropic.Config{
			ID:                   "anthropic",
			APIKey:               key,
			BaseURL:              a.cfg.LLM.Anthropic.BaseURL,
			Model:                a.cfg.LLM.Anthropic.Model,
			SupportsImages:       strings.Contains(a.cfg.LLM.ModelInputModalities, "image"),
			MaxRequestImageBytes: a.cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
		}), &a.cfg)); err != nil {
			return fmt.Errorf("pa: register anthropic provider: %w", err)
		}
	}

	// M11-pi-ai: every other built-in provider from the directory (openai and
	// anthropic above are config-driven; the remaining catalog entries are wired
	// by protocol here). A provider registers only when its key is present
	// (configured llm.key.<id> > env <ENV>); without one it stays dormant, and
	// the settings page offers it through 增加提供方.
	for _, bp := range builtinProviders {
		if bp.id == "deepseek-official" || bp.id == "openai" || bp.id == "anthropic" {
			continue // registered above (config-driven)
		}
		key := a.providerKey(bp.id)
		if key == "" {
			continue
		}
		if err := registerBuiltinByProtocol(reg, bp, key, &a.cfg); err != nil {
			return err
		}
	}

	// M11: custom providers declared through the Model settings page
	// (llm.custom.<id> in the settings table). Each carries its own route id,
	// display name, base URL, model and wire protocol; its key follows the same
	// precedence (configured llm.key.<id> > env <ROUTE>_API_KEY). The protocol
	// is dispatched to the matching adapter (M11-pi-ai 四协议); empty protocol
	// falls back to OpenAI-compatible.
	for _, cp := range a.customProviders {
		bp := builtinProvider{
			id:       cp.ID,
			protocol: providerProtocol(cp.Protocol),
			baseURL:  cp.BaseURL,
			model:    cp.Model,
		}
		if !validProtocol(cp.Protocol) {
			bp.protocol = protocolCompletions
		}
		if err := registerBuiltinByProtocol(reg, bp, a.providerKey(cp.ID), &a.cfg); err != nil {
			return fmt.Errorf("pa: register custom provider %q: %w", cp.ID, err)
		}
	}

	// Select by cfg.LLM.Provider; an unknown id is a fail-closed startup error
	// (dispatch-m8-2 §5/§6).
	p, err := reg.Get(a.cfg.LLM.Provider)
	if err != nil {
		return fmt.Errorf("pa: %w (llm.provider=%q; registered: %s)", err, a.cfg.LLM.Provider, llmProviderIDs(reg))
	}
	if !p.Available() {
		return fmt.Errorf("pa: llm provider %q is not available (missing %s or invalid base_url)", p.ID(), llmCredentialEnv(p.ID()))
	}

	a.llmMu.Lock()
	a.llm = p
	a.llmReg = reg
	a.llmMu.Unlock()
	return nil
}

// currentLLM returns the currently selected provider under the read lock. Every
// consumer that wires a.llm into a component (loop, compaction,
// subagent spawn, eval judge) reads through this so the live model switch can
// swap the pointer safely (P5.1). Loop is re-wired every turn (buildLoop), so
// a model switch takes effect on the very next message.
func (a *app) currentLLM() llm.LLM {
	a.llmMu.RLock()
	defer a.llmMu.RUnlock()
	return a.llm
}

// llmFor resolves the adapter a turn should talk to (dsh ModelSelection 对齐:
// a session pinned to a provider routes its turns through that provider).
// An empty id or an unknown provider falls back to the global LLM (fail-open,
// same spirit as the session-runtime fallbacks).
func (a *app) llmFor(provider string) llm.LLM {
	if provider == "" {
		return a.currentLLM()
	}
	a.llmMu.RLock()
	reg := a.llmReg
	a.llmMu.RUnlock()
	if reg != nil {
		if p, err := reg.Get(provider); err == nil {
			return p
		}
	}
	return a.currentLLM()
}

// llmProviderIDs returns the registered provider ids as a comma-joined list
// (for the fail-closed error message).
func llmProviderIDs(reg *llm.Registry) string {
	ids := make([]string, 0, len(reg.List()))
	for _, p := range reg.List() {
		ids = append(ids, p.ID())
	}
	return strings.Join(ids, ", ")
}

// registerBuiltinByProtocol wires one directory provider into the registry
// through the adapter that speaks its protocol (M11-pi-ai 四协议, user 2026-09):
// openai-completions → the openai adapter (deepseek-compatible SSE),
// anthropic-messages → the anthropic adapter (its Config.ID carries the route),
// google-generative-ai → the google adapter, openai-responses → the
// openairesponses adapter. SupportsImages / image budget come from the global
// multimodal policy (shared by every provider, dispatch-m8-3).
func registerBuiltinByProtocol(reg *llm.Registry, bp builtinProvider, key string, cfg *config.Config) error {
	images := strings.Contains(cfg.LLM.ModelInputModalities, "image")
	maxBytes := cfg.LLM.Multimodal.MaxRequestImageBytes
	switch bp.protocol {
	case protocolCompletions:
		return reg.Register(wrapProvider(openai.New(openai.Config{
			ID:                   bp.id,
			APIKey:               key,
			BaseURL:              bp.baseURL,
			Model:                bp.model,
			SupportsImages:       images,
			MaxRequestImageBytes: maxBytes,
		}), cfg))
	case protocolMessages:
		return reg.Register(wrapProvider(anthropic.New(anthropic.Config{
			ID:                   bp.id,
			APIKey:               key,
			BaseURL:              bp.baseURL,
			Model:                bp.model,
			SupportsImages:       images,
			MaxRequestImageBytes: maxBytes,
		}), cfg))
	case protocolGemini:
		return reg.Register(wrapProvider(google.New(google.Config{
			ID:                   bp.id,
			APIKey:               key,
			BaseURL:              bp.baseURL,
			Model:                bp.model,
			SupportsImages:       images,
			MaxRequestImageBytes: maxBytes,
		}), cfg))
	case protocolResponses:
		return reg.Register(wrapProvider(openairesponses.New(openairesponses.Config{
			ID:                   bp.id,
			APIKey:               key,
			BaseURL:              bp.baseURL,
			Model:                bp.model,
			SupportsImages:       images,
			MaxRequestImageBytes: maxBytes,
		}), cfg))
	default:
		return fmt.Errorf("pa: provider %q: unknown protocol %q", bp.id, bp.protocol)
	}
}

// customModel is one model of a custom provider's multi-model list
// (M11-pi-ai ModelListEditor 对齐): an id plus optional display name and
// capacities (context window / max output tokens, as disclosed by the probe
// listing). Capacities are suggestions; they are not enforced at the wire.
type customModel struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
}

// customProviderProfile is the persisted M11 custom-provider declaration
// (settings row llm.custom.<route> = JSON). A custom provider is a
// user-declared endpoint: route id, display name, base URL, wire protocol
// (M11-pi-ai 四协议) and the model list. Model is the effective default model
// (the first entry of Models, or a legacy single-model value); Models is the
// multi-model list a probe fills or the user edits by hand (empty keeps the
// legacy single Model). Its API key follows the standard precedence
// (llm.key.<route> setting > env <ROUTE>_API_KEY).
type customProviderProfile struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	BaseURL  string        `json:"base_url"`
	Model    string        `json:"model"`
	Protocol string        `json:"protocol"`
	Models   []customModel `json:"models,omitempty"`
}

// builtinProviderProfile is the persisted override for a built-in provider
// (settings row llm.profile.<id> = JSON, dsh ProviderEditor 自定义设置 对齐): a
// base URL override, an effective model, and an optional multi-model list. A
// built-in provider with no profile row keeps its config.yaml defaults; the
// profile lets the Model settings page override API 地址 / 模型 per provider
// (like dsh's llm-deepseek settings section) without touching config.yaml.
type builtinProviderProfile struct {
	BaseURL string        `json:"base_url,omitempty"`
	Model   string        `json:"model,omitempty"`
	Models  []customModel `json:"models,omitempty"`
}

// validProtocol reports whether protocol is one of the four supported wire
// protocols (M11-pi-ai, user 2026-09). Empty is valid and means the default
// openai-completions.
func validProtocol(protocol string) bool {
	switch providerProtocol(protocol) {
	case protocolCompletions, protocolMessages, protocolGemini, protocolResponses:
		return true
	}
	return false
}

// llmKeyEnv returns the environment variable that carries provider id's API
// key. Built-ins map to their canonical credential variable from the directory
// (providerEnv — DEEPSEEK_API_KEY, OPENAI_API_KEY, ANTHROPIC_API_KEY, HF_TOKEN,
// KIMI_API_KEY, AI_GATEWAY_API_KEY, ...); a custom provider id derives one by
// upper-casing the route (my-llm → MY_LLM_API_KEY). This is the env-only
// default (纪律 6); a key configured through the Model settings page
// (llm.key.<id>, M11) takes precedence over it (配置后以配置的为准).
func llmKeyEnv(id string) string {
	return providerEnv(id)
}

// providerKey returns provider id's effective API key: a key configured through
// the Model settings page wins (llm.key.<id>, persisted in the settings table),
// otherwise the environment variable (llmKeyEnv). nil llmKeys (direct-constructed
// apps in tests) falls straight back to the env default.
func (a *app) providerKey(id string) string {
	if a.llmKeys != nil {
		if k, ok := a.llmKeys[id]; ok && k != "" {
			return k
		}
	}
	return os.Getenv(llmKeyEnv(id))
}

// builtinProfile returns the persisted override profile for a built-in provider
// (llm.profile.<id>, dsh ProviderEditor 自定义设置 对齐), or the zero value when
// none is stored.
func (a *app) builtinProfile(id string) builtinProviderProfile {
	if a.builtinProfiles != nil {
		return a.builtinProfiles[id]
	}
	return builtinProviderProfile{}
}

// llmCredentialEnv returns the environment variable that carries provider id's
// API key (env-only, 纪律 6).
func llmCredentialEnv(id string) string {
	return providerEnv(id)
}

// llmStatus prints the /llm-status report: the selected provider (marked *),
// every registered provider with its availability, the input modalities
// (cfg.LLM.ModelInputModalities: text / text,image), and the multimodal gate
// (enabled|disabled, D10). An unconfigured provider (key absent / bad base_url)
// is shown as unavailable.
func (a *app) llmStatus() error {
	if a.llmReg == nil {
		fmt.Println("llm: no provider registry (registerLLM did not run)")
		return nil
	}
	sel := a.cfg.LLM.Provider
	fmt.Println("llm: enabled")
	for _, p := range a.llmReg.List() {
		marker := "  "
		if p.ID() == sel {
			marker = "* "
		}
		avail := "available"
		if !p.Available() {
			avail = "unavailable"
		}
		fmt.Printf("%s%s: %s (model=%s)\n", marker, p.ID(), avail, llmProviderModel(a.cfg, p.ID()))
	}
	fmt.Printf("  modalities: %s\n", llmModalitiesValue(a.cfg))
	mm := "disabled"
	if a.multimodalEnabled() {
		mm = "enabled"
	}
	fmt.Printf("  multimodal: %s\n", mm)
	return nil
}

// llmModalitiesValue returns the effective model_input_modalities declaration,
// falling back to the default "text" when the config field is empty (the
// applyDefaults path always fills it, but direct-constructed configs in tests
// and defensive callers read the fallback). /llm-status and printHelp display
// it (dispatch-m8-3 §3/§4: "text" | "text,image").
func llmModalitiesValue(cfg config.Config) string {
	if cfg.LLM.ModelInputModalities == "" {
		return config.DefaultModelInputModalities
	}
	return cfg.LLM.ModelInputModalities
}

// llmProviderModel returns the configured model for provider id: the legacy
// top-level model for deepseek, llm.openai.model for openai, llm.anthropic.model
// for anthropic (dispatch-m8-2 §5 / M8-2b §3: top-level model/base_url stay as
// the deepseek default configuration).
func llmProviderModel(cfg config.Config, id string) string {
	switch id {
	case "openai":
		return cfg.LLM.OpenAI.Model
	case "anthropic":
		return cfg.LLM.Anthropic.Model
	}
	return cfg.Model
}

// llmProviderBaseURL returns the effective base URL for provider id (the web
// Model editor shows it read-only; an empty value means the provider default,
// rendered as "提供方默认"). It mirrors llmProviderModel's routing: the legacy
// top-level base_url stays the deepseek default configuration.
func llmProviderBaseURL(cfg config.Config, id string) string {
	switch id {
	case "openai":
		return cfg.LLM.OpenAI.BaseURL
	case "anthropic":
		return cfg.LLM.Anthropic.BaseURL
	}
	return cfg.BaseURL
}

// modelCandidates returns the suggested model names for provider id (P5.1 live
// model picker). These are honest suggestions — the picker also allows a free
// model string. Candidates mirror the M8-1/M8-2/M8-2b defaults plus the current
// mainstream models from the pi-ai catalogs (M11-pi-ai).
func modelCandidates(id string) []string {
	switch id {
	case "deepseek-official":
		return []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	case "openai":
		return []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini"}
	case "openrouter":
		return []string{"openai/gpt-4o-mini", "openai/gpt-4o", "anthropic/claude-sonnet-4-5", "deepseek/deepseek-chat"}
	case "together":
		return []string{"meta-llama/Llama-3.3-70B-Instruct-Turbo", "meta-llama/Llama-3.1-8B-Instruct-Turbo", "Qwen/Qwen2.5-72B-Instruct-Turbo"}
	case "groq":
		return []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant", "mixtral-8x7b-32768"}
	case "mistral":
		return []string{"mistral-large-latest", "mistral-small-latest", "open-mistral-nemo"}
	case "nvidia":
		return []string{"meta/llama-3.3-70b-instruct", "meta/llama-3.1-8b-instruct", "deepseek-ai/deepseek-r1"}
	case "cerebras":
		return []string{"llama-3.3-70b", "llama-3.1-8b", "llama-3.1-70b"}
	case "baseten":
		return nil // deployment platform: model is fully user-defined
	case "huggingface":
		return []string{"meta-llama/Llama-3.3-70B-Instruct", "mistralai/Mistral-7B-Instruct-v0.3"}
	case "zai":
		return []string{"glm-z1-32b-coding", "glm-4.5-air"}
	case "zai-coding-cn":
		return []string{"glm-z1-32b-coding", "glm-4.5-air"}
	case "moonshotai", "moonshotai-cn":
		return []string{"kimi-k2", "kimi-latest", "moonshot-v1-8k"}
	case "qwen-token-plan", "qwen-token-plan-cn":
		return []string{"qwen3-coder-plus", "qwen3-coder", "qwen-max-latest"}
	case "xiaomi":
		return []string{"MiMo-7B-RL", "MiMo-7B"}
	case "ant-ling":
		return []string{"antling-3.5", "antling-4.5"}
	case "fireworks":
		return []string{"accounts/fireworks/models/llama-v3p3-70b-instruct", "accounts/fireworks/models/qwen3-coder-480b-a35b-instruct"}
	case "anthropic":
		return []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"}
	case "minimax", "minimax-cn":
		return []string{"MiniMax-M2", "MiniMax-Text-01"}
	case "kimi-coding":
		return []string{"kimi-k2"}
	case "vercel-ai-gateway":
		return []string{"anthropic/claude-sonnet-4-5", "openai/gpt-4o", "anthropic/claude-opus-4-1"}
	case "google":
		return []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash"}
	case "xai":
		return []string{"grok-4", "grok-4-mini", "grok-3"}
	}
	return nil
}
