package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/web"
)

const (
	nativeShellDefaultTimeoutMS       = 120000
	nativeShellMaxTimeoutMS           = 600000
	nativeShellDefaultOutputBytes     = 64000
	nativeShellDefaultSpillBytes      = 67108864
	nativeShellDefaultGraceMS         = 3000
	nativeAgentLoopDefaultMaxParallel = 10
)

type nativeWebSearchSettings struct {
	APIKeyEnv  string
	BaseURL    string
	Model      string
	APIVersion string
	MaxTokens  int
	MaxUses    int
}

func (a *app) nativeWebSearchBase() nativeWebSearchSettings {
	base := a.cfg.Web.DeepSeek
	return nativeWebSearchSettings{
		APIKeyEnv:  "DEEPSEEK_API_KEY",
		BaseURL:    base.BaseURL,
		Model:      base.Model,
		APIVersion: base.APIVersion,
		MaxTokens:  base.MaxTokens,
		MaxUses:    base.MaxUses,
	}
}

func (a *app) nativeWebSearchSettingsSnapshot() nativeWebSearchSettings {
	a.nativeRuntimeSettingsMu.RLock()
	defer a.nativeRuntimeSettingsMu.RUnlock()
	settings := a.nativeWebSearch
	if strings.TrimSpace(settings.APIKeyEnv) == "" {
		settings.APIKeyEnv = "DEEPSEEK_API_KEY"
	}
	if strings.TrimSpace(settings.BaseURL) == "" {
		settings.BaseURL = a.cfg.Web.DeepSeek.BaseURL
	}
	if strings.TrimSpace(settings.Model) == "" {
		settings.Model = a.cfg.Web.DeepSeek.Model
	}
	if strings.TrimSpace(settings.APIVersion) == "" {
		settings.APIVersion = a.cfg.Web.DeepSeek.APIVersion
	}
	if settings.MaxTokens <= 0 {
		settings.MaxTokens = a.cfg.Web.DeepSeek.MaxTokens
	}
	if settings.MaxUses <= 0 {
		settings.MaxUses = a.cfg.Web.DeepSeek.MaxUses
	}
	return settings
}

func (a *app) nativeWebSearchConfig(ctx context.Context) web.Config {
	settings := a.nativeWebSearchSettingsSnapshot()
	apiKey := os.Getenv(settings.APIKeyEnv)
	if a.credentials != nil {
		if resolved, err := a.credentials.Resolve(ctx, settings.APIKeyEnv); err == nil && resolved != "" {
			apiKey = resolved
		}
	}
	return web.Config{
		APIKey:           apiKey,
		BaseURL:          settings.BaseURL,
		Model:            settings.Model,
		APIVersion:       settings.APIVersion,
		MaxTokens:        settings.MaxTokens,
		MaxUses:          settings.MaxUses,
		OnRequestContext: a.webSearchRequestEventLogger,
	}
}

func (a *app) applyNativeWebSearchSettings(resolved map[string]any) {
	settings := a.nativeWebSearchBase()
	if value, ok := resolved["apiKeyEnv"].(string); ok && strings.TrimSpace(value) != "" {
		settings.APIKeyEnv = strings.TrimSpace(value)
	}
	if value, ok := resolved["baseURL"].(string); ok && strings.TrimSpace(value) != "" {
		settings.BaseURL = strings.TrimSpace(value)
	}
	if value, ok := resolved["model"].(string); ok && strings.TrimSpace(value) != "" {
		settings.Model = strings.TrimSpace(value)
	}
	if value, ok := resolved["apiVersion"].(string); ok && strings.TrimSpace(value) != "" {
		settings.APIVersion = strings.TrimSpace(value)
	}
	if value := nativeRuntimePositiveInt(resolved["maxTokens"]); value > 0 {
		settings.MaxTokens = value
	}
	if value := nativeRuntimePositiveInt(resolved["maxUses"]); value > 0 {
		settings.MaxUses = value
	}
	a.nativeRuntimeSettingsMu.Lock()
	a.nativeWebSearch = settings
	a.nativeRuntimeSettingsMu.Unlock()
}

// loadNativeRuntimeSettings restores executor-owned native settings before
// shell registration. The webserver owns the document; this composition-root
// boundary owns the live runtime facts.
func (a *app) loadNativeRuntimeSettings(ctx context.Context) error {
	if a.store == nil {
		return nil
	}
	settings, err := a.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	a.applyNativeShellSettings(map[string]any{})
	a.applyNativeAgentLoopSettings(map[string]any{})
	for namespace, apply := range map[string]func(map[string]any){
		"shell":               a.applyNativeShellSettings,
		"agent-loop":          a.applyNativeAgentLoopSettings,
		"web-search-deepseek": a.applyNativeWebSearchSettings,
	} {
		raw := strings.TrimSpace(settings["native.settings."+namespace])
		if raw == "" {
			continue
		}
		var document struct {
			Value map[string]any `json:"value"`
		}
		if json.Unmarshal([]byte(raw), &document) != nil || document.Value == nil {
			continue
		}
		apply(document.Value)
	}
	return nil
}

func (a *app) nativeShellSettings() tools.ShellSettings {
	a.nativeRuntimeSettingsMu.RLock()
	defer a.nativeRuntimeSettingsMu.RUnlock()
	settings := a.nativeShell
	if settings.TimeoutMS <= 0 {
		settings.TimeoutMS = nativeShellDefaultTimeoutMS
	}
	if settings.MaxTimeoutMS <= 0 {
		settings.MaxTimeoutMS = nativeShellMaxTimeoutMS
	}
	if settings.MaxOutputBytes <= 0 {
		settings.MaxOutputBytes = nativeShellDefaultOutputBytes
	}
	if settings.MaxSpillBytes <= 0 {
		settings.MaxSpillBytes = nativeShellDefaultSpillBytes
	}
	if settings.GraceMS < 0 {
		settings.GraceMS = 0
	}
	return settings
}

func (a *app) applyNativeShellSettings(resolved map[string]any) {
	settings := a.nativeShellSettings()
	if value := nativeRuntimePositiveInt(resolved["timeoutMs"]); value > 0 {
		settings.TimeoutMS = value
	}
	if value := nativeRuntimePositiveInt(resolved["maxTimeoutMs"]); value > 0 {
		settings.MaxTimeoutMS = value
	}
	if settings.MaxTimeoutMS < settings.TimeoutMS {
		settings.TimeoutMS = settings.MaxTimeoutMS
	}
	if value := nativeRuntimePositiveInt(resolved["maxOutputBytes"]); value > 0 {
		settings.MaxOutputBytes = value
	}
	if value := nativeRuntimePositiveInt(resolved["maxSpillBytes"]); value > 0 {
		settings.MaxSpillBytes = value
	}
	if raw, exists := resolved["graceMs"]; exists {
		if value := nativeRuntimePositiveInt(raw); value >= 0 {
			settings.GraceMS = value
		}
	}
	if value, ok := resolved["cwd"].(string); ok {
		settings.Cwd = strings.TrimSpace(value)
	}
	if value, ok := resolved["pwshPath"].(string); ok {
		settings.PwshPath = strings.TrimSpace(value)
	}

	a.nativeRuntimeSettingsMu.Lock()
	a.nativeShell = settings
	a.nativeRuntimeSettingsMu.Unlock()

	// New session registries clone this base. Use detached policy snapshots so
	// shell settings do not widen the global output cap for unrelated tools.
	if a.reg != nil {
		policy := a.reg.Policy()
		policy.Shell.Timeout = time.Duration(settings.MaxTimeoutMS) * time.Millisecond
		policy.Shell.OutputLimit = settings.MaxOutputBytes
		policy.Shell.MaxSpillBytes = settings.MaxSpillBytes
		a.reg.SetPolicy(policy)
	}
	a.basePolicy.Shell.Timeout = time.Duration(settings.MaxTimeoutMS) * time.Millisecond
	a.basePolicy.Shell.OutputLimit = settings.MaxOutputBytes
	a.basePolicy.Shell.MaxSpillBytes = settings.MaxSpillBytes
}

func (a *app) nativeAgentLoopMaxParallel() int {
	a.nativeRuntimeSettingsMu.RLock()
	defer a.nativeRuntimeSettingsMu.RUnlock()
	if a.nativeAgentLoopMax <= 0 {
		return nativeAgentLoopDefaultMaxParallel
	}
	return a.nativeAgentLoopMax
}

func (a *app) applyNativeAgentLoopSettings(resolved map[string]any) {
	value := nativeRuntimePositiveInt(resolved["maxParallelToolCalls"])
	if value <= 0 {
		value = nativeAgentLoopDefaultMaxParallel
	}
	a.nativeRuntimeSettingsMu.Lock()
	a.nativeAgentLoopMax = value
	a.nativeRuntimeSettingsMu.Unlock()
}

func nativeRuntimePositiveInt(value any) int {
	switch number := value.(type) {
	case int:
		if number > 0 {
			return number
		}
	case int64:
		if number > 0 {
			return int(number)
		}
	case float64:
		if number > 0 && number == float64(int64(number)) {
			return int(number)
		}
	}
	return 0
}
