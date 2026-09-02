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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/credential"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/llm/anthropic"
	"github.com/jabing/shutu-agent/internal/llm/deepseek"
	"github.com/jabing/shutu-agent/internal/llm/google"
	"github.com/jabing/shutu-agent/internal/llm/openai"
	"github.com/jabing/shutu-agent/internal/llm/openairesponses"
	"github.com/jabing/shutu-agent/internal/llm/retry"
	"github.com/jabing/shutu-agent/internal/store"
)

type unavailableLLM struct{ err error }

func (u unavailableLLM) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return nil, u.err
}

// providerGeneration is the ownership boundary for one published provider
// registry. Providers retain credential material, so replacing a registry is
// not enough: the old generation must stay alive for already-started streams
// and then be closed exactly once after the last stream settles.
type providerGeneration struct {
	mu       sync.Mutex
	registry *llm.Registry
	refs     int
	retired  bool
	closed   bool
}

func (g *providerGeneration) acquire() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired || g.closed {
		return false
	}
	g.refs++
	return true
}

func (g *providerGeneration) release() {
	if g == nil {
		return
	}
	var registry *llm.Registry
	g.mu.Lock()
	if g.refs > 0 {
		g.refs--
	}
	if g.retired && g.refs == 0 && !g.closed {
		g.closed = true
		registry = g.registry
	}
	g.mu.Unlock()
	if registry != nil {
		_ = registry.Close()
	}
}

func (g *providerGeneration) retire() {
	if g == nil {
		return
	}
	var registry *llm.Registry
	g.mu.Lock()
	if !g.retired {
		g.retired = true
	}
	if g.refs == 0 && !g.closed {
		g.closed = true
		registry = g.registry
	}
	g.mu.Unlock()
	if registry != nil {
		_ = registry.Close()
	}
}

// leasedLLM keeps a generation alive for the duration of one assembled Agent
// turn. Non-held instances acquire a short stream lease, which gives
// background consumers (title/eval/compaction/subagents) the same safe
// retirement behavior without forcing them to know about generations.
type leasedLLM struct {
	inner llm.LLM
	gen   *providerGeneration
	held  bool
}

func (l *leasedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if l == nil || l.inner == nil {
		return nil, errors.New("pa: nil LLM")
	}
	if l.held || l.gen == nil {
		return l.inner.Stream(ctx, req)
	}
	if !l.gen.acquire() {
		return nil, errors.New("pa: provider generation retired")
	}
	reader, err := l.inner.Stream(ctx, req)
	if err != nil {
		l.gen.release()
		return nil, err
	}
	return &leasedStreamReader{StreamReader: reader, release: l.gen.release}, nil
}

// leasedStreamReader releases on both normal EOF and transport failure. The
// underlying StreamReader has no Close method, so consumers must continue
// reading until EOF/error; all in-tree consumers already follow that contract.
type leasedStreamReader struct {
	llm.StreamReader
	once    sync.Once
	release func()
}

// routedLLM resolves the route at operation start. Long-lived services such
// as subagent runtimes may outlive a model switch; retaining a concrete old
// provider there would either keep credentials forever or fail every later
// request after that generation is retired.
type routedLLM struct {
	resolve func() llm.LLM
}

func (r *routedLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if r == nil || r.resolve == nil {
		return nil, errors.New("pa: provider route unavailable")
	}
	provider := r.resolve()
	if provider == nil {
		return nil, errors.New("pa: provider route unavailable")
	}
	return provider.Stream(ctx, req)
}

func (r *routedLLM) SupportsImages() bool {
	if r == nil || r.resolve == nil {
		return false
	}
	capability, ok := r.resolve().(llm.ImageCapability)
	return ok && capability.SupportsImages()
}

func (r *leasedStreamReader) Next() (llm.StreamEvent, error) {
	event, err := r.StreamReader.Next()
	// The loop's stream contract treats StreamFinish as terminal and may
	// legitimately stop without issuing a follow-up Next that would produce
	// io.EOF. Release at that protocol boundary as well as on EOF/transport
	// failure, otherwise a retired provider generation can retain credentials
	// indefinitely after an otherwise successful request.
	if err != nil || event.Kind == llm.StreamFinish {
		r.once.Do(r.release)
	}
	return event, err
}

func (l *leasedLLM) SupportsImages() bool {
	capability, ok := l.inner.(llm.ImageCapability)
	return ok && capability.SupportsImages()
}

// providerRuntimeSnapshot is the immutable pair consumed when assembling a
// turn. Reading config and the selected registry generation under one barrier
// prevents an Agent from combining a newly selected provider/model with the
// previous live adapter during a concurrent Web model switch.
type providerRuntimeSnapshot struct {
	cfg        config.Config
	provider   string
	selected   llm.LLM
	selectedID string
	release    func()
}

func (a *app) providerRuntimeSnapshot(requested string) providerRuntimeSnapshot {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.providerMu.RLock()
	cfg := a.cfg.Clone()
	a.providerMu.RUnlock()
	provider := strings.TrimSpace(requested)
	if provider == "" {
		provider = cfg.LLM.Provider
	}
	var selected llm.LLM
	selectedID := ""
	a.llmMu.RLock()
	if provider == "" {
		selected = a.llm
	} else if a.llmReg != nil {
		if resolved, err := a.llmReg.Get(provider); err == nil {
			selected = resolved
		}
	}
	a.llmMu.RUnlock()
	if selected == nil {
		selected = unavailableLLM{err: fmt.Errorf("pa: llm provider %q is not registered", provider)}
	} else if err := llmRouteAvailable(provider, selected); err != nil {
		selected = unavailableLLM{err: err}
	} else {
		selectedID = provider
	}
	return providerRuntimeSnapshot{cfg: cfg, provider: provider, selected: selected, selectedID: selectedID}
}

func wrapProvider(p llm.Provider, cfg *config.Config) llm.Provider {
	policy := retry.Config{
		Mode:           cfg.LLM.Retry.Mode,
		MaxRetries:     cfg.LLM.Retry.MaxRetries,
		InitialBackoff: cfg.LLM.Retry.InitialBackoff.Duration,
		MaxBackoff:     cfg.LLM.Retry.MaxBackoff.Duration,
		JitterRatio:    cfg.LLM.Retry.JitterRatio,
		RetryableCodes: append([]string(nil), cfg.LLM.Retry.RetryableCodes...),
	}
	if route, ok := cfg.LLM.Retry.Providers[p.ID()]; ok {
		if route.Mode != nil {
			policy.Mode = *route.Mode
		}
		if route.MaxRetries != nil {
			policy.MaxRetries = *route.MaxRetries
			policy.MaxRetriesSet = true
		}
		if route.InitialBackoff != nil {
			policy.InitialBackoff = route.InitialBackoff.Duration
		}
		if route.MaxBackoff != nil {
			policy.MaxBackoff = route.MaxBackoff.Duration
		}
		if route.JitterRatio != nil {
			policy.JitterRatio = *route.JitterRatio
		}
		if route.RetryableCodes != nil {
			policy.RetryableCodes = append([]string(nil), (*route.RetryableCodes)...)
		}
	}
	// Production Agents retry at the durable loop/request-error boundary. The
	// compatibility WrapProvider API still exists for standalone embedders,
	// but using it here would hide provider attempts inside one Stream call.
	return retry.WrapProviderForLoop(p, policy)
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
	a.providerStateMu.Lock()
	defer a.providerStateMu.Unlock()
	return a.registerLLMUnlocked()
}

// registerLLMUnlocked builds and publishes a provider generation while the
// caller owns providerStateMu. Keeping the build and publication under one
// barrier makes config + registry selection an atomic runtime snapshot.
func (a *app) registerLLMUnlocked() error {
	// Build the complete registry from one immutable configuration snapshot.
	// A concurrent Web status request or Agent spawn must never observe a
	// half-updated provider profile while this potentially expensive assembly
	// is in progress.
	a.providerMu.RLock()
	cfg := a.cfg.Clone()
	keys := cloneStringMap(a.llmKeys)
	profiles := cloneBuiltinProfiles(a.builtinProfiles)
	customProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	for provider, profile := range profiles {
		if err := validateCustomModels(profile.Models); err != nil {
			return fmt.Errorf("pa: invalid model catalog for provider %q: %w", provider, err)
		}
	}
	for _, profile := range customProviders {
		if err := validateCustomModels(profile.Models); err != nil {
			return fmt.Errorf("pa: invalid model catalog for provider %q: %w", profile.ID, err)
		}
	}
	reg := llm.NewRegistry()
	registryPublished := false
	defer func() {
		if !registryPublished {
			_ = reg.Close()
		}
	}()

	// The deepseek provider is always registered; its parameters come from the
	// legacy top-level model/base_url (the deepseek default configuration,
	// dispatch-m8-2 §5) and DEEPSEEK_API_KEY from the environment (a configured
	// llm.key.deepseek-official setting wins, M11). A persisted
	// llm.profile.deepseek-official override (dsh ProviderEditor 自定义设置)
	// wins over config.yaml for base URL / model / model list.
	dsProfile := profiles["deepseek-official"]
	dsBaseURL, dsModel := cfg.BaseURL, cfg.Model
	if dsProfile.BaseURL != "" {
		dsBaseURL = dsProfile.BaseURL
	}
	dsModel = effectiveProfileModel(dsProfile, dsModel)
	openaiProfile := profiles["openai"]
	openaiBaseURL, openaiModel := cfg.LLM.OpenAI.BaseURL, cfg.LLM.OpenAI.Model
	if openaiProfile.BaseURL != "" {
		openaiBaseURL = openaiProfile.BaseURL
	}
	openaiModel = effectiveProfileModel(openaiProfile, openaiModel)
	anthropicProfile := profiles["anthropic"]
	anthropicBaseURL, anthropicModel := cfg.LLM.Anthropic.BaseURL, cfg.LLM.Anthropic.Model
	if anthropicProfile.BaseURL != "" {
		anthropicBaseURL = anthropicProfile.BaseURL
	}
	anthropicModel = effectiveProfileModel(anthropicProfile, anthropicModel)
	if err := reg.Register(wrapProvider(deepseek.New(deepseek.Config{
		APIKey:                  providerKeyFromSnapshot(keys, "deepseek-official"),
		CredentialProvider:      a.credentialProvider("deepseek-official"),
		CredentialLeaseProvider: a.credentialLeaseProvider("deepseek-official"),
		UserIDProvider:          func(context.Context) (string, error) { return a.feedbackAnonymousUserID() },
		BaseURL:                 dsBaseURL,
		Model:                   dsModel,
		DefaultReasoningEffort:  "high",
		ModelCatalog:            providerModelCatalogInfos("deepseek-official", profiles, customProviders),
		DefaultMaxTokens:        dsProfile.DefaultMaxTokens,
		MaxRetries:              2,
		DisableRetry:            true,
		SupportsImages:          strings.Contains(cfg.LLM.ModelInputModalities, "image"),
		MaxRequestImageBytes:    cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
	}), &cfg)); err != nil {
		return fmt.Errorf("pa: register deepseek provider: %w", err)
	}

	// The openai provider is registered only when its credential is present
	// (OPENAI_API_KEY env-only, dispatch-m8-2 §6; configured llm.key.openai
	// wins, M11). It reuses the deepseek OpenAI-compatible client — zero new
	// wire code (dispatch-m8-2 §4).
	if key := providerKeyFromSnapshot(keys, "openai"); key != "" {
		if err := reg.Register(wrapProvider(openai.New(openai.Config{
			APIKey:                  key,
			CredentialProvider:      a.credentialProvider("openai"),
			CredentialLeaseProvider: a.credentialLeaseProvider("openai"),
			BaseURL:                 openaiBaseURL,
			Model:                   openaiModel,
			ModelCatalog:            providerModelCatalogInfos("openai", profiles, customProviders),
			DefaultMaxTokens:        openaiProfile.DefaultMaxTokens,
			DisableRetry:            true,
			SupportsImages:          strings.Contains(cfg.LLM.ModelInputModalities, "image"),
			MaxRequestImageBytes:    cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
		}), &cfg)); err != nil {
			return fmt.Errorf("pa: register openai provider: %w", err)
		}
	}

	// The anthropic provider is registered only when its credential is present
	// (ANTHROPIC_API_KEY env-only, dispatch-m8-2b §3; configured llm.key.anthropic
	// wins, M11). Its parameters come from llm.anthropic.base_url/model
	// (defaults https://api.anthropic.com/v1 / claude-sonnet-4-5, M8-2b §3).
	if key := providerKeyFromSnapshot(keys, "anthropic"); key != "" {
		if err := reg.Register(wrapProvider(anthropic.New(anthropic.Config{
			ID:                      "anthropic",
			APIKey:                  key,
			CredentialProvider:      a.credentialProvider("anthropic"),
			CredentialLeaseProvider: a.credentialLeaseProvider("anthropic"),
			BaseURL:                 anthropicBaseURL,
			Model:                   anthropicModel,
			ModelCatalog:            providerModelCatalogInfos("anthropic", profiles, customProviders),
			MaxTokens:               anthropicProfile.DefaultMaxTokens,
			SupportsImages:          strings.Contains(cfg.LLM.ModelInputModalities, "image"),
			MaxRequestImageBytes:    cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
		}), &cfg)); err != nil {
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
		key := providerKeyFromSnapshot(keys, bp.id)
		if key == "" {
			continue
		}
		if err := registerBuiltinByProtocol(reg, bp, key, &cfg, providerModelCatalogInfos(bp.id, profiles, customProviders), profiles[bp.id].DefaultMaxTokens, a.credentialProvider(bp.id), a.credentialLeaseProvider(bp.id)); err != nil {
			return err
		}
	}

	// M11: custom providers declared through the Model settings page
	// (llm.custom.<id> in the settings table). Each carries its own route id,
	// display name, base URL, model and wire protocol; its key follows the same
	// precedence (configured llm.key.<id> > env <ROUTE>_API_KEY). The protocol
	// is dispatched to the matching adapter (M11-pi-ai 四协议); empty protocol
	// falls back to OpenAI-compatible.
	for _, cp := range customProviders {
		model := effectiveCustomProviderModel(cp)
		if model == "" {
			return fmt.Errorf("pa: custom provider %q has no model", cp.ID)
		}
		bp := builtinProvider{
			id:       cp.ID,
			protocol: providerProtocol(cp.Protocol),
			baseURL:  cp.BaseURL,
			model:    model,
		}
		if !validProtocol(cp.Protocol) {
			bp.protocol = protocolCompletions
		}
		if err := registerBuiltinByProtocol(reg, bp, providerKeyFromSnapshot(keys, cp.ID), &cfg, providerModelCatalogInfos(cp.ID, profiles, customProviders), cp.DefaultMaxTokens, a.credentialProvider(cp.ID), a.credentialLeaseProvider(cp.ID)); err != nil {
			return fmt.Errorf("pa: register custom provider %q: %w", cp.ID, err)
		}
	}

	// Select by cfg.LLM.Provider; an unknown id is a fail-closed startup error
	// (dispatch-m8-2 §5/§6).
	p, err := reg.Get(cfg.LLM.Provider)
	if err != nil {
		return fmt.Errorf("pa: %w (llm.provider=%q; registered: %s)", err, cfg.LLM.Provider, llmProviderIDs(reg))
	}
	if !p.Available() {
		return fmt.Errorf("pa: llm provider %q is not available (missing %s or invalid base_url)", p.ID(), llmCredentialEnv(p.ID()))
	}

	a.llmMu.Lock()
	oldGeneration := a.providerGeneration
	a.llm = p
	a.llmReg = reg
	a.providerGeneration = &providerGeneration{registry: reg}
	a.llmMu.Unlock()
	if oldGeneration != nil {
		oldGeneration.retire()
	}
	registryPublished = true
	return nil
}

// currentLLM returns the currently selected provider under the read lock. Every
// consumer that wires a.llm into a component (loop, compaction,
// subagent spawn, eval judge) reads through this so the live model switch can
// swap the pointer safely (P5.1). Loop is re-wired every turn (buildLoop), so
// a model switch takes effect on the very next message.
func (a *app) currentLLM() llm.LLM {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.llmMu.RLock()
	defer a.llmMu.RUnlock()
	if a.providerGeneration == nil {
		return a.llm
	}
	provider := ""
	a.providerMu.RLock()
	provider = a.cfg.LLM.Provider
	a.providerMu.RUnlock()
	return &routedLLM{resolve: func() llm.LLM { return a.resolvePublishedLLM(provider) }}
}

// providerConfigSnapshot returns the config used by one runtime assembly. It
// is intentionally detached from app.cfg so a concurrent model switch cannot
// change the provider, mode or limits halfway through constructing a session.
func (a *app) providerConfigSnapshot() config.Config {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.providerMu.RLock()
	defer a.providerMu.RUnlock()
	return a.cfg.Clone()
}

// llmFor resolves the adapter a turn should talk to (dsh ModelSelection 对齐:
// a session pinned to a provider routes its turns through that provider).
// An empty id uses the global LLM. An unknown provider returns a fail-closed
// adapter so a stale or forged per-session selection cannot silently route a
// turn to another provider.
func (a *app) llmFor(provider string) llm.LLM {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.llmMu.RLock()
	defer a.llmMu.RUnlock()
	if provider == "" {
		if a.providerGeneration == nil {
			return a.llm
		}
		return &routedLLM{resolve: func() llm.LLM { return a.resolvePublishedLLM("") }}
	}
	reg := a.llmReg
	if reg != nil {
		if p, err := reg.Get(provider); err == nil {
			if a.providerGeneration == nil {
				return p
			}
			return &routedLLM{resolve: func() llm.LLM { return a.resolvePublishedLLM(provider) }}
		}
	}
	return unavailableLLM{err: fmt.Errorf("pa: llm provider %q is not registered", provider)}
}

func (a *app) resolvePublishedLLM(provider string) llm.LLM {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.providerMu.RLock()
	configured := a.cfg.LLM.Provider
	a.providerMu.RUnlock()
	if strings.TrimSpace(provider) == "" {
		provider = configured
	}
	a.llmMu.RLock()
	defer a.llmMu.RUnlock()
	if provider == "" {
		if err := llmRouteAvailable(provider, a.llm); err != nil {
			return unavailableLLM{err: err}
		}
		return a.wrapPublishedLLM(a.llm, a.providerGeneration)
	}
	if a.llmReg == nil {
		return unavailableLLM{err: fmt.Errorf("pa: llm provider %q is not registered", provider)}
	}
	p, err := a.llmReg.Get(provider)
	if err != nil {
		return unavailableLLM{err: fmt.Errorf("pa: llm provider %q is not registered", provider)}
	}
	if err := llmRouteAvailable(provider, p); err != nil {
		return unavailableLLM{err: err}
	}
	return a.wrapPublishedLLM(p, a.providerGeneration)
}

func (a *app) wrapPublishedLLM(provider llm.LLM, generation *providerGeneration) llm.LLM {
	if provider == nil {
		return nil
	}
	if generation == nil {
		return provider
	}
	return &leasedLLM{inner: provider, gen: generation}
}

func (a *app) closeProviderGenerations() error {
	a.llmMu.RLock()
	generation := a.providerGeneration
	reg := a.llmReg
	a.llmMu.RUnlock()
	if generation != nil {
		generation.retire()
		return nil
	}
	if reg != nil {
		return reg.Close()
	}
	return nil
}

// providerRuntimeSnapshotPinned acquires a generation lease that spans one
// assembled turn. The caller must invoke the returned release function once.
func (a *app) providerRuntimeSnapshotPinned(requested string) providerRuntimeSnapshot {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.providerMu.RLock()
	cfg := a.cfg.Clone()
	a.providerMu.RUnlock()
	provider := strings.TrimSpace(requested)
	if provider == "" {
		provider = cfg.LLM.Provider
	}
	var selected llm.LLM
	var generation *providerGeneration
	a.llmMu.RLock()
	if provider == "" {
		selected = a.llm
		generation = a.providerGeneration
	} else if a.llmReg != nil {
		if resolved, err := a.llmReg.Get(provider); err == nil {
			selected = resolved
			generation = a.providerGeneration
		}
	}
	a.llmMu.RUnlock()
	if selected == nil {
		return providerRuntimeSnapshot{cfg: cfg, provider: provider, selected: unavailableLLM{err: fmt.Errorf("pa: llm provider %q is not registered", provider)}}
	}
	if err := llmRouteAvailable(provider, selected); err != nil {
		return providerRuntimeSnapshot{cfg: cfg, provider: provider, selected: unavailableLLM{err: err}}
	}
	if generation == nil {
		return providerRuntimeSnapshot{cfg: cfg, provider: provider, selected: selected, selectedID: provider}
	}
	if !generation.acquire() {
		return providerRuntimeSnapshot{cfg: cfg, provider: provider, selected: unavailableLLM{err: fmt.Errorf("pa: llm provider %q generation is unavailable", provider)}}
	}
	var once sync.Once
	return providerRuntimeSnapshot{
		cfg: cfg, provider: provider, selected: &leasedLLM{inner: selected, gen: generation, held: true}, selectedID: provider,
		release: func() { once.Do(generation.release) },
	}
}

// llmRouteAvailable is the single route-admission check for runtime snapshots
// and routed adapters. A registered provider can still become dormant when a
// credential is removed or rotated away; such routes fail before turn assembly.
func llmRouteAvailable(provider string, selected llm.LLM) error {
	route, ok := selected.(llm.Provider)
	if !ok {
		// Standalone embedders may supply an opaque llm.LLM. They opt out of
		// registry-backed route admission; concrete providers still enforce it.
		return nil
	}
	if !route.Available() {
		return fmt.Errorf("%w: %q", llm.ErrProviderUnavailable, provider)
	}
	return nil
}

// llmSupportsImages resolves the exact provider selected for a session. The
// global modality setting is only an admission prerequisite; capability is
// never inferred for an unavailable or opaque provider wrapper.
func (a *app) llmSupportsImages(provider string) bool {
	a.providerStateMu.RLock()
	defer a.providerStateMu.RUnlock()
	a.providerMu.RLock()
	modalities := a.cfg.LLM.ModelInputModalities
	a.providerMu.RUnlock()
	if !modelAcceptsImages(modalities) {
		return false
	}
	if provider == "" {
		a.llmMu.RLock()
		selected := a.llm
		a.llmMu.RUnlock()
		capability, ok := selected.(llm.ImageCapability)
		return ok && capability.SupportsImages()
	}
	a.llmMu.RLock()
	reg := a.llmReg
	a.llmMu.RUnlock()
	if reg == nil {
		return false
	}
	selected, err := reg.Get(provider)
	if err != nil {
		return false
	}
	capability, ok := selected.(llm.ImageCapability)
	return ok && capability.SupportsImages()
}

// llmSupportsImagesForSession applies the image gate to the exact durable
// route selected by one session.  Provider-level capability is still the
// final implementation check, but an absent/invalid model route must not be
// treated as equivalent to the process default (the ACP admission contract
// is session-scoped, not current-REPL-scoped).
func (a *app) llmSupportsImagesForSession(sessionID string) bool {
	provider, model, err := a.sessionProviderModelStrict(sessionID)
	if err != nil || strings.TrimSpace(model) == "" {
		return false
	}
	if !a.modelCapabilityFor(sessionID).Vision {
		return false
	}
	return a.llmSupportsImages(provider)
}

// llmSupportsImagesForRoute applies the same catalog-plus-provider gate to an
// initialized transport route before that route has a durable session row
// (the SDK initialize/prompt boundary).
func (a *app) llmSupportsImagesForRoute(provider, model string) bool {
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if provider == "" || model == "" {
		return false
	}
	if !a.modelCapabilityForRoute(provider, model).Vision {
		return false
	}
	return a.llmSupportsImages(provider)
}

// sessionProviderModel reads the durable provider/model selection without
// mutating the active registry policy. Child runtimes use it when a spawn is
// admitted by an Agent belonging to a non-current session.
func (a *app) sessionProviderModel(sessionID string) (string, string) {
	provider, model, err := a.sessionProviderModelStrict(sessionID)
	if err != nil {
		// Callers predating the error-returning seam must still fail closed: an
		// impossible provider id selects unavailableLLM instead of the global
		// adapter. They may not silently cross a session boundary.
		return "__session_config_unavailable__", ""
	}
	return provider, model
}

func (a *app) sessionProviderModelStrict(sessionID string) (string, string, error) {
	baseCfg := a.providerConfigSnapshot()
	provider := baseCfg.LLM.Provider
	model := llmProviderModel(baseCfg, provider)
	if scs, ok := a.store.(store.SessionConfigStore); ok && sessionID != "" {
		sessionCfg, err := scs.GetSessionConfig(context.Background(), sessionID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", "", fmt.Errorf("pa: load session runtime %q: %w", sessionID, err)
		}
		if err == nil {
			if sessionCfg.Provider != "" {
				provider = sessionCfg.Provider
			}
			if sessionCfg.Model != "" {
				model = sessionCfg.Model
			} else {
				model = llmProviderModel(baseCfg, provider)
			}
		}
	}
	return provider, model, nil
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
func registerBuiltinByProtocol(reg *llm.Registry, bp builtinProvider, key string, cfg *config.Config, modelCatalog []llm.ModelInfo, routeDefaultMaxTokens int, credentials llm.CredentialProvider, credentialLeases llm.CredentialLeaseProvider) error {
	images := strings.Contains(cfg.LLM.ModelInputModalities, "image")
	maxBytes := cfg.LLM.Multimodal.MaxRequestImageBytes
	switch bp.protocol {
	case protocolCompletions:
		return reg.Register(wrapProvider(openai.New(openai.Config{
			ID:                      bp.id,
			APIKey:                  key,
			CredentialProvider:      credentials,
			CredentialLeaseProvider: credentialLeases,
			BaseURL:                 bp.baseURL,
			Model:                   bp.model,
			ModelCatalog:            modelCatalog,
			DefaultMaxTokens:        routeDefaultMaxTokens,
			SupportsImages:          images,
			MaxRequestImageBytes:    maxBytes,
		}), cfg))
	case protocolMessages:
		return reg.Register(wrapProvider(anthropic.New(anthropic.Config{
			ID:                      bp.id,
			APIKey:                  key,
			CredentialProvider:      credentials,
			CredentialLeaseProvider: credentialLeases,
			BaseURL:                 bp.baseURL,
			Model:                   bp.model,
			ModelCatalog:            modelCatalog,
			MaxTokens:               routeDefaultMaxTokens,
			SupportsImages:          images,
			MaxRequestImageBytes:    maxBytes,
		}), cfg))
	case protocolGemini:
		return reg.Register(wrapProvider(google.New(google.Config{
			ID:                      bp.id,
			APIKey:                  key,
			CredentialProvider:      credentials,
			CredentialLeaseProvider: credentialLeases,
			BaseURL:                 bp.baseURL,
			Model:                   bp.model,
			ModelCatalog:            modelCatalog,
			MaxOutputTokens:         routeDefaultMaxTokens,
			SupportsImages:          images,
			MaxRequestImageBytes:    maxBytes,
		}), cfg))
	case protocolResponses:
		return reg.Register(wrapProvider(openairesponses.New(openairesponses.Config{
			ID:                      bp.id,
			APIKey:                  key,
			CredentialProvider:      credentials,
			CredentialLeaseProvider: credentialLeases,
			BaseURL:                 bp.baseURL,
			Model:                   bp.model,
			ModelCatalog:            modelCatalog,
			MaxOutputTokens:         routeDefaultMaxTokens,
			SupportsImages:          images,
			MaxRequestImageBytes:    maxBytes,
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
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Input is the authoritative model-level modality declaration. An empty
	// value preserves legacy route/config fallback; a non-empty value is
	// fail-closed and currently supports text and image only.
	Input                  []string           `json:"input,omitempty"`
	ReasoningEfforts       map[string]*string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string             `json:"default_reasoning_effort,omitempty"`
	DefaultMaxTokens       int                `json:"default_max_tokens,omitempty"`
	ContextWindow          int                `json:"context_window,omitempty"`
	MaxTokens              int                `json:"max_tokens,omitempty"`
	// Capability pointers distinguish "not declared" from an explicit false.
	// Runtime fallbacks remain protocol/config driven until the catalog owns
	// the answer.
	Reasoning *bool `json:"reasoning,omitempty"`
	Tools     *bool `json:"tools,omitempty"`
	Vision    *bool `json:"vision,omitempty"`
	Audio     *bool `json:"audio,omitempty"`
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	Protocol string `json:"protocol"`
	// DefaultMaxTokens is the route-level request default used when a selected
	// model has no explicit defaultMaxTokens entry.
	DefaultMaxTokens int           `json:"default_max_tokens,omitempty"`
	Models           []customModel `json:"models,omitempty"`
}

// builtinProviderProfile is the persisted override for a built-in provider
// (settings row llm.profile.<id> = JSON, dsh ProviderEditor 自定义设置 对齐): a
// base URL override, an effective model, and an optional multi-model list. A
// built-in provider with no profile row keeps its config.yaml defaults; the
// profile lets the Model settings page override API 地址 / 模型 per provider
// (like dsh's llm-deepseek settings section) without touching config.yaml.
type builtinProviderProfile struct {
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
	// DefaultMaxTokens is the route-level request default used when a selected
	// model has no explicit defaultMaxTokens entry.
	DefaultMaxTokens int           `json:"default_max_tokens,omitempty"`
	Models           []customModel `json:"models,omitempty"`
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
	if a.credentials != nil {
		if key, err := a.credentials.Resolve(context.Background(), llmKeyEnv(id)); err == nil && key != "" {
			return key
		}
	}
	a.providerMu.RLock()
	defer a.providerMu.RUnlock()
	return providerKeyFromSnapshot(a.llmKeys, id)
}

// credentialProvider is the per-operation credential seam shared by every
// production adapter. The initial APIKey still preserves the standalone
// adapter constructor contract; this callback makes live settings/env
// rotation visible to an already-published provider generation.
func (a *app) credentialProvider(id string) llm.CredentialProvider {
	return func(ctx context.Context) (string, error) {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if a.credentials != nil {
			if key, err := a.credentials.Resolve(ctx, llmKeyEnv(id)); err == nil && key != "" {
				return key, nil
			}
		}
		return a.providerKey(id), nil
	}
}

// credentialLeaseProvider binds a provider generation to the dedicated vault
// when one is installed. Standalone/test apps without a vault continue to use
// the legacy string callback and therefore do not claim release-aware
// credential semantics.
func (a *app) credentialLeaseProvider(id string) llm.CredentialLeaseProvider {
	if a.credentials == nil {
		return nil
	}
	return func(ctx context.Context) (llm.CredentialLease, error) {
		lease, err := a.credentials.Acquire(ctx, llmKeyEnv(id))
		if errors.Is(err, credential.ErrNotFound) {
			if key := a.providerKey(id); key != "" {
				return staticCredentialLease(key), nil
			}
		}
		return lease, err
	}
}

// staticCredentialLease adapts an environment-sourced key to the release-aware
// provider seam. Environment keys are never persisted by the credential vault.
type staticCredentialLease string

func (lease staticCredentialLease) Value() string { return string(lease) }
func (staticCredentialLease) Release()            {}

func providerKeyFromSnapshot(keys map[string]string, id string) string {
	if keys != nil {
		if k, ok := keys[id]; ok && k != "" {
			return k
		}
	}
	return os.Getenv(llmKeyEnv(id))
}

// builtinProfile returns the persisted override profile for a built-in provider
// (llm.profile.<id>, dsh ProviderEditor 自定义设置 对齐), or the zero value when
// none is stored.
func (a *app) builtinProfile(id string) builtinProviderProfile {
	a.providerMu.RLock()
	defer a.providerMu.RUnlock()
	if a.builtinProfiles != nil {
		profile := a.builtinProfiles[id]
		profile.Models = append([]customModel(nil), profile.Models...)
		return profile
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
	a.llmMu.RLock()
	reg := a.llmReg
	a.llmMu.RUnlock()
	if reg == nil {
		fmt.Println("llm: no provider registry (registerLLM did not run)")
		return nil
	}
	cfg := a.providerConfigSnapshot()
	sel := cfg.LLM.Provider
	fmt.Println("llm: enabled")
	for _, p := range reg.List() {
		marker := "  "
		if p.ID() == sel {
			marker = "* "
		}
		avail := "available"
		if !p.Available() {
			avail = "unavailable"
		}
		fmt.Printf("%s%s: %s (model=%s)\n", marker, p.ID(), avail, llmProviderModel(cfg, p.ID()))
	}
	fmt.Printf("  modalities: %s\n", llmModalitiesValue(cfg))
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
// model string. The IDs are projections of the single built-in model catalog.
func modelCandidates(id string) []string {
	entries := builtinModelCatalog[id]
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ID)
	}
	return out
}
