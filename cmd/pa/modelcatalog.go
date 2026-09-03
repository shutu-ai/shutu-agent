package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// defaultModelContextWindow is the one composition-root fallback for routes
// whose catalog entry has no locally verifiable capacity. It matches dsh's
// llm-deepseek default and is deliberately applied here, not independently by
// Web, ACP, SDK or the loop.
const defaultModelContextWindow = 1_000_000

// modelCapability is the runtime view of one effective provider/model route.
// Sources resolve in this order: configured model-directory entry, known
// built-in model catalog, then protocol/config fallbacks.
type modelCapability struct {
	Provider               string
	Model                  string
	ContextWindow          int
	MaxTokens              int
	DefaultMaxTokens       int
	Reasoning              bool
	ReasoningEfforts       map[string]*string
	DefaultReasoningEffort string
	// ReasoningKnown is true when the selected catalog entry explicitly owns
	// the answer. Undeclared free-form routes keep their current pass-through
	// behavior until the provider catalog can classify them.
	ReasoningKnown bool
	Tools          bool
	Vision         bool
	Audio          bool
}

// modelCapabilityFor resolves facts consumed by the loop and transport
// projections. CLI, Web, ACP and SDK turns pass a session ID; blank is global.
func (a *app) modelCapabilityFor(sessionID string) modelCapability {
	provider, model, _ := a.sessionProviderModelStrict(sessionID)
	return a.modelCapabilityForRoute(provider, model)
}

// modelCapabilityForRoute resolves a candidate selection without requiring it
// to be persisted first; model-selection admission uses this exact pair.
func (a *app) modelCapabilityForRoute(provider, model string) modelCapability {
	cfg := a.providerConfigSnapshot()
	return a.modelCapabilityForRouteWithConfig(cfg, provider, model)
}

// modelCapabilityForRouteWithConfig admits a candidate against a caller-held
// config snapshot. The model switch uses this because its publication lock
// already serializes the configuration being changed.
func (a *app) modelCapabilityForRouteWithConfig(cfg config.Config, provider, model string) modelCapability {
	capability := modelCapability{
		Provider:      provider,
		Model:         model,
		ContextWindow: defaultModelContextWindow,
		Tools:         true,
	}
	entry, found := a.modelDirectoryEntry(provider, model)
	// A built-in profile override may replace only capacity; inherited
	// first-party modality/reasoning facts must not silently become unknown.
	base, baseFound := builtinModelEntry(provider, model)
	if found {
		if entry.ContextWindow > 0 {
			capability.ContextWindow = entry.ContextWindow
		} else if baseFound && base.ContextWindow > 0 {
			capability.ContextWindow = base.ContextWindow
		}
		if entry.MaxTokens > 0 {
			capability.MaxTokens = entry.MaxTokens
		} else if baseFound && base.MaxTokens > 0 {
			capability.MaxTokens = base.MaxTokens
		}
		if entry.DefaultMaxTokens > 0 {
			capability.DefaultMaxTokens = entry.DefaultMaxTokens
		} else if baseFound && base.DefaultMaxTokens > 0 {
			capability.DefaultMaxTokens = base.DefaultMaxTokens
		}
		if entry.Reasoning != nil {
			capability.ReasoningKnown = true
			capability.Reasoning = *entry.Reasoning
		} else if baseFound && base.Reasoning != nil {
			capability.ReasoningKnown = true
			capability.Reasoning = *base.Reasoning
		}
		if entry.ReasoningEfforts != nil {
			capability.ReasoningKnown = true
			capability.Reasoning = true
			capability.ReasoningEfforts = cloneReasoningEfforts(entry.ReasoningEfforts)
		} else if baseFound && base.ReasoningEfforts != nil {
			capability.ReasoningKnown = true
			capability.Reasoning = true
			capability.ReasoningEfforts = cloneReasoningEfforts(base.ReasoningEfforts)
		}
		capability.DefaultReasoningEffort = modelDefaultReasoningEffort(entry)
		if capability.DefaultReasoningEffort == "" && baseFound && entry.Reasoning == nil && entry.ReasoningEfforts == nil {
			capability.DefaultReasoningEffort = modelDefaultReasoningEffort(base)
		}
		if entry.Tools != nil {
			capability.Tools = *entry.Tools
		} else if baseFound && base.Tools != nil {
			capability.Tools = *base.Tools
		}
		if vision, declared := customModelVision(entry); declared {
			capability.Vision = vision
		} else if entry.Vision != nil {
			capability.Vision = *entry.Vision
		} else if baseFound && base.Vision != nil {
			capability.Vision = *base.Vision
		}
		if entry.Audio != nil {
			capability.Audio = *entry.Audio
		} else if baseFound && base.Audio != nil {
			capability.Audio = *base.Audio
		}
	} else {
		if entry, ok := builtinModelEntry(provider, model); ok {
			if entry.ContextWindow > 0 {
				capability.ContextWindow = entry.ContextWindow
			}
			if entry.DefaultMaxTokens > 0 {
				capability.DefaultMaxTokens = entry.DefaultMaxTokens
			}
			if entry.Reasoning != nil {
				capability.ReasoningKnown = true
				capability.Reasoning = *entry.Reasoning
			}
			if entry.ReasoningEfforts != nil {
				capability.ReasoningKnown = true
				capability.Reasoning = true
				capability.ReasoningEfforts = cloneReasoningEfforts(entry.ReasoningEfforts)
			}
			capability.DefaultReasoningEffort = modelDefaultReasoningEffort(entry)
			if entry.Tools != nil {
				capability.Tools = *entry.Tools
			}
			if vision, declared := customModelVision(entry); declared {
				capability.Vision = vision
			} else if entry.Vision != nil {
				capability.Vision = *entry.Vision
			}
			if entry.Audio != nil {
				capability.Audio = *entry.Audio
			}
		}
	}
	return capability
}

func (a *app) modelDirectoryEntry(provider, model string) (customModel, bool) {
	if provider == "" || model == "" {
		return customModel{}, false
	}
	a.providerMu.RLock()
	profiles := cloneBuiltinProfiles(a.builtinProfiles)
	customProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	if profile, ok := profiles[provider]; ok {
		for _, entry := range profile.Models {
			if entry.ID == model {
				return entry, true
			}
		}
	}
	if entry, ok := builtinModelEntry(provider, model); ok {
		return entry, true
	}
	for _, custom := range customProviders {
		if custom.ID != provider {
			continue
		}
		for _, entry := range custom.Models {
			if entry.ID == model {
				return entry, true
			}
		}
	}
	return customModel{}, false
}

// modelCatalogRows is the composition-root projection consumed by native
// model selectors. It deliberately publishes only catalog facts we actually
// own: explicit provider/profile declarations and built-in entries carrying
// capability metadata. ID-only suggestions remain settings hints and do not
// become falsely authoritative session routes.
func modelCatalogRows(provider string, profiles map[string]builtinProviderProfile, customProviders []customProviderProfile) []map[string]any {
	entries := make([]customModel, 0)
	if profile, ok := profiles[provider]; ok && len(profile.Models) > 0 {
		entries = append(entries, profile.Models...)
	} else {
		for _, entry := range builtinModelCatalog[provider] {
			if entry.ContextWindow > 0 || entry.MaxTokens > 0 || entry.DefaultMaxTokens > 0 || entry.Reasoning != nil || len(entry.ReasoningEfforts) > 0 || entry.DefaultReasoningEffort != "" || entry.Tools != nil || entry.Vision != nil || len(entry.Input) > 0 || entry.Audio != nil {
				entries = append(entries, entry)
			}
		}
		for _, custom := range customProviders {
			if custom.ID == provider && len(custom.Models) > 0 {
				entries = append(entries, custom.Models...)
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(entries))
	rows := make([]map[string]any, 0, len(entries))
	for _, model := range entries {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		row := map[string]any{"id": id}
		if name := strings.TrimSpace(model.Name); name != "" {
			row["name"] = name
		}
		if model.ContextWindow > 0 {
			row["contextWindow"] = model.ContextWindow
		}
		if model.MaxTokens > 0 {
			row["maxTokens"] = model.MaxTokens
		}
		if model.DefaultMaxTokens > 0 {
			row["defaultMaxTokens"] = model.DefaultMaxTokens
		}
		if model.Reasoning != nil {
			row["reasoning"] = *model.Reasoning
		}
		if model.ReasoningEfforts != nil {
			row["reasoningEfforts"] = cloneReasoningEfforts(model.ReasoningEfforts)
		}
		if effort := modelDefaultReasoningEffort(model); effort != "" {
			row["defaultEffort"] = effort
		}
		if model.Tools != nil {
			row["tools"] = *model.Tools
		}
		if model.Vision != nil {
			row["vision"] = *model.Vision
		}
		if len(model.Input) > 0 {
			row["input"] = append([]string(nil), model.Input...)
		}
		if model.Audio != nil {
			row["audio"] = *model.Audio
		}
		rows = append(rows, row)
	}
	return rows
}

func modelCatalogInfos(provider string, profiles map[string]builtinProviderProfile, customProviders []customProviderProfile) []llm.ModelInfo {
	rows := modelCatalogRows(provider, profiles, customProviders)
	infos := make([]llm.ModelInfo, 0, len(rows))
	for _, row := range rows {
		info := llm.ModelInfo{
			Provider:         provider,
			ID:               catalogStringValue(row["id"]),
			Name:             catalogStringValue(row["name"]),
			Input:            catalogStringSliceValue(row["input"]),
			ContextWindow:    catalogIntValue(row["contextWindow"]),
			MaxTokens:        catalogIntValue(row["maxTokens"]),
			DefaultMaxTokens: catalogIntValue(row["defaultMaxTokens"]),
		}
		info.Reasoning = catalogBoolValue(row["reasoning"])
		info.ReasoningEfforts = catalogReasoningEffortsValue(row["reasoningEfforts"])
		info.DefaultReasoningEffort = catalogStringValue(row["defaultEffort"])
		info.Tools = catalogBoolValue(row["tools"])
		info.Vision = catalogBoolValue(row["vision"])
		if info.Vision == nil && len(info.Input) > 0 {
			vision := false
			for _, modality := range info.Input {
				if strings.EqualFold(strings.TrimSpace(modality), "image") {
					vision = true
					break
				}
			}
			info.Vision = &vision
		}
		info.Audio = catalogBoolValue(row["audio"])
		if info.ID != "" {
			infos = append(infos, info)
		}
	}
	return infos
}

// providerModelCatalogInfos is the runtime provider catalog projection. Unlike
// the selector projection above, it retains ID-only catalog rows as explicit
// unknown-fact entries, so a configured provider can list every owned route
// without turning a suggestion into invented capability metadata.
func providerModelCatalogInfos(provider string, profiles map[string]builtinProviderProfile, customProviders []customProviderProfile) []llm.ModelInfo {
	entries := make([]customModel, 0)
	if profile, ok := profiles[provider]; ok && len(profile.Models) > 0 {
		entries = append(entries, profile.Models...)
	} else {
		entries = append(entries, builtinModelCatalog[provider]...)
		for _, custom := range customProviders {
			if custom.ID == provider && len(custom.Models) > 0 {
				entries = append(entries, custom.Models...)
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(entries))
	infos := make([]llm.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		info := llm.ModelInfo{
			Provider: provider, ID: id, Name: entry.Name,
			Input:         append([]string(nil), entry.Input...),
			ContextWindow: entry.ContextWindow, MaxTokens: entry.MaxTokens, DefaultMaxTokens: entry.DefaultMaxTokens,
			ReasoningEfforts:       cloneReasoningEfforts(entry.ReasoningEfforts),
			DefaultReasoningEffort: strings.ToLower(strings.TrimSpace(entry.DefaultReasoningEffort)),
			Reasoning:              cloneCatalogBool(entry.Reasoning), Tools: cloneCatalogBool(entry.Tools),
			Vision: cloneCatalogBool(entry.Vision), Audio: cloneCatalogBool(entry.Audio),
		}
		if vision, declared := customModelVision(entry); declared {
			info.Vision = &vision
		}
		infos = append(infos, info)
	}
	return infos
}

// customModelVision resolves the DSH model-level input declaration before the
// legacy Vision pointer. This is important when a provider declares
// input:["text"] while the deployment-wide config enables images.
func customModelVision(model customModel) (bool, bool) {
	if len(model.Input) > 0 {
		for _, modality := range model.Input {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				return true, true
			}
		}
		return false, true
	}
	if model.Vision != nil {
		return *model.Vision, true
	}
	return false, false
}

func catalogStringSliceValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func catalogReasoningEffortsValue(value any) map[string]*string {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]*string); ok {
		return cloneReasoningEfforts(typed)
	}
	return nil
}

func validateCustomModels(models []customModel) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("model %q is declared more than once", id)
		}
		seen[id] = struct{}{}
		if model.ContextWindow < 0 || model.MaxTokens < 0 || model.DefaultMaxTokens < 0 {
			return fmt.Errorf("model %q has negative capacity metadata", id)
		}
		if model.Reasoning != nil && !*model.Reasoning && len(model.ReasoningEfforts) > 0 {
			return fmt.Errorf("model %q declares reasoning efforts while reasoning is disabled", id)
		}
		if effort := strings.ToLower(strings.TrimSpace(model.DefaultReasoningEffort)); effort != "" {
			if !validModelReasoningEffort(effort) {
				return fmt.Errorf("model %q declares unsupported default reasoning effort %q", id, model.DefaultReasoningEffort)
			}
			if model.Reasoning != nil && !*model.Reasoning {
				return fmt.Errorf("model %q declares a reasoning default while reasoning is disabled", id)
			}
			if model.ReasoningEfforts != nil {
				if !reasoningEffortMapContains(model.ReasoningEfforts, effort) {
					return fmt.Errorf("model %q default reasoning effort %q is not in its catalog", id, effort)
				}
			}
		}
		for effort, wire := range model.ReasoningEfforts {
			effort = strings.ToLower(strings.TrimSpace(effort))
			switch effort {
			case "off":
				if wire != nil && strings.TrimSpace(*wire) == "" {
					return fmt.Errorf("model %q has an empty wire value for reasoning effort %q", id, effort)
				}
			case "minimal", "low", "medium", "high", "xhigh", "max":
				if wire == nil || strings.TrimSpace(*wire) == "" {
					return fmt.Errorf("model %q has an empty wire value for reasoning effort %q", id, effort)
				}
			default:
				return fmt.Errorf("model %q declares unsupported reasoning effort %q", id, effort)
			}
		}
		seenInput := make(map[string]struct{}, len(model.Input))
		for _, modality := range model.Input {
			modality = strings.ToLower(strings.TrimSpace(modality))
			if modality != "text" && modality != "image" {
				return fmt.Errorf("model %q declares unsupported input modality %q", id, modality)
			}
			if _, exists := seenInput[modality]; exists {
				return fmt.Errorf("model %q declares duplicate input modality %q", id, modality)
			}
			seenInput[modality] = struct{}{}
		}
	}
	return nil
}

func effectiveProfileModel(profile builtinProviderProfile, fallback string) string {
	if model := strings.TrimSpace(profile.Model); model != "" {
		return model
	}
	for _, entry := range profile.Models {
		if model := strings.TrimSpace(entry.ID); model != "" {
			return model
		}
	}
	return strings.TrimSpace(fallback)
}

func effectiveCustomProviderModel(profile customProviderProfile) string {
	return effectiveProfileModel(builtinProviderProfile{Model: profile.Model, Models: profile.Models}, "")
}

func cloneCatalogBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func catalogStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func catalogIntValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func catalogBoolValue(value any) *bool {
	if typed, ok := value.(bool); ok {
		return &typed
	}
	return nil
}

// effectiveModelOutputLimit keeps an explicit runtime/config budget
// authoritative, then applies only an explicitly owned DSH-style model
// default. MaxTokens is a model capability ceiling, not a request default;
// when neither budget exists, zero lets the selected provider decide.
func effectiveModelOutputLimit(configured, defaultTokens, catalog int) int {
	_ = catalog // retained in the signature for callers compiled against the old seam
	if configured > 0 {
		return configured
	}
	if defaultTokens > 0 {
		return defaultTokens
	}
	return 0
}

// effectiveModelReasoningEffort keeps an explicitly declared reasoning route
// and clears a stale effort when the exact model does not own that capability.
func effectiveModelReasoningEffort(capability modelCapability, requested string) string {
	if strings.TrimSpace(requested) == "" && capability.ReasoningKnown && capability.Reasoning {
		return strings.TrimSpace(capability.DefaultReasoningEffort)
	}
	if requested != "" && capability.ReasoningKnown && !capability.Reasoning {
		return ""
	}
	return requested
}

func modelDefaultReasoningEffort(model customModel) string {
	if effort := strings.ToLower(strings.TrimSpace(model.DefaultReasoningEffort)); effort != "" {
		return effort
	}
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if reasoningEffortMapContains(model.ReasoningEfforts, effort) {
			return effort
		}
	}
	if model.Reasoning != nil && *model.Reasoning {
		return "high"
	}
	return ""
}

func reasoningEffortMapContains(efforts map[string]*string, wanted string) bool {
	for effort := range efforts {
		if strings.EqualFold(strings.TrimSpace(effort), wanted) {
			return true
		}
	}
	return false
}

func validModelReasoningEffort(effort string) bool {
	switch effort {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// validateModelCapabilityForSelection rejects an explicit unsupported feature
// at selection time. Clearing an effort is always allowed.
func validateModelCapabilityForSelection(capability modelCapability, requestedEffort string) error {
	if requestedEffort == "" {
		return nil
	}
	reasoning := capability.Reasoning
	catalog := []llm.ModelInfo{{
		ID: capability.Model, Reasoning: &reasoning,
		ReasoningEfforts: cloneReasoningEfforts(capability.ReasoningEfforts),
	}}
	if !capability.ReasoningKnown {
		catalog[0].Reasoning = nil
	}
	if err := llm.ValidateReasoningEffortForModel(capability.Provider, catalog, capability.Model, requestedEffort); err != nil {
		return fmt.Errorf("%w: %v", llm.ErrCapabilityUnavailable, err)
	}
	return nil
}

// modelToolSpecs is the model-facing projection used by native, SDK and ACP
// loop assembly. A catalog route without tool capability has no wire tools,
// even though host tools remain available to non-model workflows.
func modelToolSpecs(capability modelCapability, mode string, specs []llm.ToolSchema) []llm.ToolSchema {
	if !capability.Tools {
		return []llm.ToolSchema{}
	}
	return toolSpecsForMode(mode, specs)
}

// validateSessionModelSelection is the shared admission seam for durable
// session overrides. Provider availability and effort policy are checked now;
// free-form models remain valid because several gateways accept routes that a
// local catalog cannot enumerate.
func (a *app) validateSessionModelSelection(ctx context.Context, sessionID, provider, model, effort string) error {
	if strings.TrimSpace(provider) == "" {
		return fmt.Errorf("%w: provider is required", llm.ErrProviderUnavailable)
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	switch effort {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("unknown reasoning effort %q (want off|low|high|max)", effort)
	}
	runtime := a.providerRuntimeSnapshotPinned(provider)
	if runtime.release != nil {
		defer runtime.release()
	}
	if runtime.selectedID == "" {
		return fmt.Errorf("%w: %q is not registered", llm.ErrProviderUnavailable, provider)
	}
	if unavailable, ok := runtime.selected.(unavailableLLM); ok {
		return unavailable.err
	}
	capability := a.modelCapabilityFor(sessionID)
	if capability.Provider != provider || capability.Model != model {
		capability = a.modelCapabilityForRoute(provider, model)
	}
	if err := validateModelCapabilityForSelection(capability, effort); err != nil {
		return err
	}
	// DSH refuses a text-only route while the session's current model-visible
	// history still contains images. This belongs at the shared selection
	// boundary, not only in the prompt path: native session.selectModel, the
	// Web model switch, ACP and SDK must not durably select a route that the
	// next replayed turn cannot encode.
	if strings.TrimSpace(sessionID) != "" {
		log, err := a.sessionLogForAgent(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("session history unavailable: %w", err)
		}
		projected, err := projection.Build(log.Events())
		if err != nil {
			return fmt.Errorf("session history projection unavailable: %w", err)
		}
		historyHasImages := false
		for _, message := range projected.History {
			if message.HasImage() {
				historyHasImages = true
				break
			}
		}
		pendingHasImages, err := pendingSessionHasImages(log)
		if err != nil {
			return fmt.Errorf("session inbox unavailable: %w", err)
		}
		if (historyHasImages || pendingHasImages) && !a.llmSupportsImagesForRoute(provider, model) {
			return fmt.Errorf("%w: model %q does not accept image input while this session contains images", llm.ErrCapabilityUnavailable, model)
		}
	}
	return nil
}

// pendingSessionHasImages replays the durable inbox splice journal instead of
// inspecting only a live Agent handle. Model selection can race a queued
// prompt before its turn claims it, and dsh admits a route only if both the
// durable history and pending model-visible input are encodable by that route.
func pendingSessionHasImages(log *session.Log) (bool, error) {
	if log == nil {
		return false, nil
	}
	events, err := replaySessionInbox(log.Events())
	if err != nil {
		return false, err
	}
	inbox, err := agent.NewDurableInbox(nil, events)
	if err != nil {
		return false, err
	}
	for _, message := range inbox.PendingMessages() {
		if (llm.Message{Content: message.Content}).HasImage() {
			return true, nil
		}
	}
	return false, nil
}
