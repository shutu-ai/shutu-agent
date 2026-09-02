// Package openai implements the llm.Provider for OpenAI-compatible SSE
// endpoints (M8-2, dispatch-m8-2 §4). It deliberately reuses the deepseek
// client: the DeepSeek API is OpenAI compatible — identical wire, including
// the reasoning_content passthrough, which OpenAI-compatible reasoning models
// also use, so the M8 reasoning semantics hold naturally. There is zero new
// serialization/parsing code here.
package openai

import (
	"context"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/llm/deepseek"
)

const (
	// DefaultBaseURL is the OpenAI-compatible base URL when llm.openai.base_url
	// is empty (dispatch-m8-2 §4).
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultModel is the chat model when llm.openai.model is empty
	// (dispatch-m8-2 §4).
	DefaultModel = "gpt-4o-mini"

	// providerID is the stable provider id of this adapter.
	providerID = "openai"
	// defaultMaxTokens is the generic OpenAI-compatible route default. The
	// DeepSeek adapter is shared for wire compatibility, but its 256K default
	// must not leak into ordinary OpenAI-compatible routes.
	defaultMaxTokens = 32768
)

// Config configures the OpenAI-compatible provider. APIKey must come from the
// environment (OPENAI_API_KEY only, design.md §6). ID is the provider's
// registry id; empty defaults to "openai" (the built-in OpenAI adapter). A
// non-empty ID lets the composition root register arbitrary OpenAI-compatible
// custom providers (M11: 增加自定义提供方) under their own route.
type Config struct {
	ID                      string
	BaseURL                 string
	APIKey                  string
	CredentialProvider      llm.CredentialProvider
	CredentialLeaseProvider llm.CredentialLeaseProvider
	Model                   string
	ModelCatalog            []llm.ModelInfo
	// DefaultMaxTokens is the route-level request default. Non-positive uses
	// the generic OpenAI-compatible default, not DeepSeek's special default.
	DefaultMaxTokens int
	// SupportsImages is the model's input-modality capability declaration,
	// passed from config llm.model_input_modalities by the composition root
	// (dispatch-m8-3b §4.1). false (the default) means an image request fails
	// closed inside the shared OpenAI-compatible client.
	SupportsImages bool
	// MaxRequestImageBytes is the per-request image byte budget (dispatch-m8-3b
	// §4.1); non-positive uses the default 20MiB.
	MaxRequestImageBytes int
	// DisableRetry delegates request retrying to the shared wrapper used by
	// the composition root.
	DisableRetry bool
}

// openaiProvider is an llm.Provider delegating the OpenAI-compatible SSE wire
// to a shared deepseek.Client (dispatch-m8-2 §4: 零新 wire 代码).
type openaiProvider struct {
	id string
	c  *deepseek.Client
}

// New returns an openaiProvider built on a deepseek.Client with OpenAI-compatible
// defaults applied (base_url https://api.openai.com/v1, model gpt-4o-mini,
// both configurable). Stream/Available delegate to the shared client.
func New(cfg Config) *openaiProvider {
	if cfg.ID == "" {
		cfg.ID = providerID
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = defaultMaxTokens
	}
	return &openaiProvider{
		id: cfg.ID,
		c: deepseek.New(deepseek.Config{
			ProviderName:            cfg.ID,
			BaseURL:                 cfg.BaseURL,
			APIKey:                  cfg.APIKey,
			CredentialProvider:      cfg.CredentialProvider,
			CredentialLeaseProvider: cfg.CredentialLeaseProvider,
			Model:                   cfg.Model,
			ModelCatalog:            cfg.ModelCatalog,
			DefaultMaxTokens:        cfg.DefaultMaxTokens,
			SupportsImages:          cfg.SupportsImages,
			MaxRequestImageBytes:    cfg.MaxRequestImageBytes,
			DisableRetry:            cfg.DisableRetry,
		}),
	}
}

// ID returns the provider's registry id ("openai" for the built-in adapter, or
// the custom route configured via Config.ID).
func (p *openaiProvider) ID() string { return p.id }

func (p *openaiProvider) SupportsImages() bool { return p.c.SupportsImages() }

func (p *openaiProvider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return p.c.ListModels(ctx)
}

func (p *openaiProvider) ResolveModelInfo(ctx context.Context, model string) (llm.ModelInfo, error) {
	return p.c.ResolveModelInfo(ctx, model)
}

func (p *openaiProvider) Close() error {
	if p == nil || p.c == nil {
		return nil
	}
	return p.c.Close()
}

// Available reports whether the provider can be used: a cheap local check (API
// key present and base URL parseable) that never performs a network call —
// exactly the deepseek.Client.Available semantics, which already validates the
// key and the (defaulted, never empty) base URL (dispatch-m8-2 §4).
func (p *openaiProvider) Available() bool {
	return p.c.Available()
}

// Stream delegates to the shared OpenAI-compatible SSE implementation
// (dispatch-m8-2 §4).
func (p *openaiProvider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	return p.c.Stream(ctx, req)
}
