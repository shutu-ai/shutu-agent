package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ModelInfo is provider-owned metadata for one exact model route. Nil
// capability pointers mean unknown; false is an explicit negative and must
// not be replaced by a transport default.
type ModelInfo struct {
	Provider string
	ID       string
	Name     string
	// Input is the provider-owned set of model input modalities. It is kept
	// alongside the legacy capability pointers so transports can preserve the
	// DSH catalog fact instead of inferring vision support from a global flag.
	Input []string
	// ReasoningEfforts maps DSH effort ids to provider wire values. A nil map
	// keeps the legacy provider vocabulary; a non-nil map is authoritative,
	// including an explicit off:null entry.
	ReasoningEfforts map[string]*string
	// DefaultReasoningEffort is the model-owned default used when a request
	// omits an explicit effort. It mirrors DSH's reasoning.defaultEffort
	// metadata and must not be inferred independently by a transport.
	DefaultReasoningEffort string
	// DefaultMaxTokens is the provider/model default output budget. MaxTokens
	// remains the model's advertised capacity; the two are intentionally
	// separate because DSH resolves a request default independently from a
	// model ceiling.
	DefaultMaxTokens int
	ContextWindow    int
	MaxTokens        int
	Reasoning        *bool
	Tools            *bool
	Vision           *bool
	Audio            *bool
}

// ModelCatalogProvider is the provider-owned catalog seam used by dsh's
// listModels/resolveModelInfo contract. Listing may be local or remote; the
// caller supplies cancellation and must preserve provider errors as a
// per-provider catalog failure rather than dropping the route silently.
type ModelCatalogProvider interface {
	ListModels(context.Context) ([]ModelInfo, error)
	ResolveModelInfo(context.Context, string) (ModelInfo, error)
}

var (
	ErrModelCatalogUnavailable = errors.New("llm model catalog is unavailable")
	ErrModelUnavailable        = errors.New("llm model is unavailable")
)

// ValidateReasoningEffortForModel validates the shared effort vocabulary and
// rejects an explicit effort for a catalog model that owns an authoritative
// non-reasoning fact. Unknown dynamic routes remain pass-through so a gateway
// may support a model that is not in the local catalog.
func ValidateReasoningEffortForModel(provider string, catalog []ModelInfo, model, effort string) error {
	_, err := ResolveReasoningEffortWire(provider, catalog, model, effort)
	return err
}

// ResolveReasoningEffortWire validates one canonical DSH effort and returns
// the model-owned provider spelling. The result is empty for an omitted effort
// or an explicit off:null mapping.
func ResolveReasoningEffortWire(provider string, catalog []ModelInfo, model, effort string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return "", nil
	}
	if !validReasoningEffort(effort) {
		return "", NewFailureError(fmt.Sprintf("%s: unsupported reasoning effort %q", provider, effort), "UNSUPPORTED_REASONING_EFFORT", nil)
	}
	for _, info := range catalog {
		if info.ID != model {
			continue
		}
		if info.ReasoningEfforts != nil {
			var wire *string
			ok := false
			for candidate, candidateWire := range info.ReasoningEfforts {
				if strings.EqualFold(strings.TrimSpace(candidate), effort) {
					wire, ok = candidateWire, true
					break
				}
			}
			if !ok {
				return "", NewFailureError(fmt.Sprintf("%s: model %q does not support reasoning effort %q", provider, model, effort), "UNSUPPORTED_REASONING_EFFORT", nil)
			}
			if wire == nil {
				return "", nil
			}
			value := strings.TrimSpace(*wire)
			if value == "" {
				return "", NewFailureError(fmt.Sprintf("%s: model %q has an empty wire value for reasoning effort %q", provider, model, effort), "UNSUPPORTED_REASONING_EFFORT", nil)
			}
			return value, nil
		}
		if info.Reasoning != nil && !*info.Reasoning && effort != "off" {
			return "", NewFailureError(fmt.Sprintf("%s: model %q does not support reasoning effort %q", provider, model, effort), "UNSUPPORTED_REASONING_EFFORT", nil)
		}
		break
	}
	if effort == "minimal" || effort == "medium" || effort == "xhigh" {
		return "", NewFailureError(fmt.Sprintf("%s: reasoning effort %q requires a model catalog mapping", provider, effort), "UNSUPPORTED_REASONING_EFFORT", nil)
	}
	return effort, nil
}

func validReasoningEffort(effort string) bool {
	switch effort {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// ProviderModelCatalog is one registry provider's catalog result. Error is
// intentionally retained beside successful groups so one broken provider does
// not erase other providers from a selector.
type ProviderModelCatalog struct {
	Provider string
	Models   []ModelInfo
	Error    error
}

// ListModelCatalog gathers provider-owned model lists in registry order. A
// provider without the optional seam produces a stable unavailable result;
// callers must not manufacture metadata from a global model setting.
func (r *Registry) ListModelCatalog(ctx context.Context) []ProviderModelCatalog {
	if r == nil {
		return nil
	}
	providers := r.List()
	out := make([]ProviderModelCatalog, 0, len(providers))
	for _, provider := range providers {
		item := ProviderModelCatalog{Provider: provider.ID()}
		catalog, ok := provider.(ModelCatalogProvider)
		if !ok {
			item.Error = fmt.Errorf("%w: provider %q does not expose listModels", ErrModelCatalogUnavailable, provider.ID())
			out = append(out, item)
			continue
		}
		models, err := catalog.ListModels(ctx)
		if err != nil {
			item.Error = fmt.Errorf("%w: provider %q: %v", ErrModelCatalogUnavailable, provider.ID(), err)
			out = append(out, item)
			continue
		}
		if err := validateModelList(provider.ID(), models); err != nil {
			item.Error = err
			out = append(out, item)
			continue
		}
		item.Models = append([]ModelInfo(nil), models...)
		out = append(out, item)
	}
	return out
}

// ResolveModelInfo resolves one exact provider/model pair through the
// provider-owned seam. It is deliberately separate from list results because
// dynamic providers may accept an unlisted model.
func (r *Registry) ResolveModelInfo(ctx context.Context, providerID, modelID string) (ModelInfo, error) {
	provider, err := r.Get(providerID)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("%w: provider %q is not registered", ErrModelUnavailable, providerID)
	}
	catalog, ok := provider.(ModelCatalogProvider)
	if !ok {
		return ModelInfo{}, fmt.Errorf("%w: provider %q does not expose resolveModelInfo", ErrModelCatalogUnavailable, providerID)
	}
	info, err := catalog.ResolveModelInfo(ctx, strings.TrimSpace(modelID))
	if err != nil {
		return ModelInfo{}, err
	}
	if err := validateModelInfo(providerID, info); err != nil {
		return ModelInfo{}, err
	}
	return info, nil
}

func validateModelList(provider string, models []ModelInfo) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if err := validateModelInfo(provider, model); err != nil {
			return err
		}
		if _, exists := seen[model.ID]; exists {
			return fmt.Errorf("llm model catalog for provider %q contains duplicate model %q", provider, model.ID)
		}
		seen[model.ID] = struct{}{}
	}
	return nil
}

func validateModelInfo(provider string, model ModelInfo) error {
	if strings.TrimSpace(model.ID) == "" {
		return fmt.Errorf("llm model catalog for provider %q contains an empty model id", provider)
	}
	if model.Provider != "" && model.Provider != provider {
		return fmt.Errorf("llm model %q belongs to provider %q, not %q", model.ID, model.Provider, provider)
	}
	if model.ContextWindow < 0 || model.MaxTokens < 0 || model.DefaultMaxTokens < 0 {
		return fmt.Errorf("llm model %q has negative capacity metadata", model.ID)
	}
	if model.Reasoning != nil && !*model.Reasoning && len(model.ReasoningEfforts) > 0 {
		return fmt.Errorf("llm model %q declares reasoning efforts while reasoning is explicitly disabled", model.ID)
	}
	if defaultEffort := strings.ToLower(strings.TrimSpace(model.DefaultReasoningEffort)); defaultEffort != "" {
		if !validReasoningEffort(defaultEffort) {
			return fmt.Errorf("llm model %q declares unsupported default reasoning effort %q", model.ID, model.DefaultReasoningEffort)
		}
		if model.Reasoning != nil && !*model.Reasoning {
			return fmt.Errorf("llm model %q declares a reasoning default while reasoning is explicitly disabled", model.ID)
		}
		if model.ReasoningEfforts != nil {
			found := false
			for candidate := range model.ReasoningEfforts {
				if strings.EqualFold(strings.TrimSpace(candidate), defaultEffort) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("llm model %q default reasoning effort %q is not in its catalog", model.ID, defaultEffort)
			}
		}
	}
	for effort, wire := range model.ReasoningEfforts {
		if !validReasoningEffort(strings.ToLower(strings.TrimSpace(effort))) {
			return fmt.Errorf("llm model %q declares unsupported reasoning effort %q", model.ID, effort)
		}
		if strings.EqualFold(strings.TrimSpace(effort), "off") && wire == nil {
			continue
		}
		if wire == nil || strings.TrimSpace(*wire) == "" {
			return fmt.Errorf("llm model %q has an empty wire value for reasoning effort %q", model.ID, effort)
		}
	}
	seenInput := make(map[string]struct{}, len(model.Input))
	for _, modality := range model.Input {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if modality != "text" && modality != "image" {
			return fmt.Errorf("llm model %q declares unsupported input modality %q", model.ID, modality)
		}
		if _, exists := seenInput[modality]; exists {
			return fmt.Errorf("llm model %q declares duplicate input modality %q", model.ID, modality)
		}
		seenInput[modality] = struct{}{}
	}
	return nil
}
