package llm

import (
	"fmt"
	"strings"
)

// CopyModelInfo returns a detached copy of one provider-owned model record.
// ModelInfo contains slices and pointer-valued capability facts; copying the
// outer struct alone would let a catalog consumer mutate the provider's live
// request policy through an alias.
func CopyModelInfo(model ModelInfo) ModelInfo {
	model.Input = append([]string(nil), model.Input...)
	if model.ReasoningEfforts != nil {
		source := model.ReasoningEfforts
		model.ReasoningEfforts = make(map[string]*string, len(source))
		for effort, wire := range source {
			if wire == nil {
				model.ReasoningEfforts[effort] = nil
				continue
			}
			value := *wire
			model.ReasoningEfforts[effort] = &value
		}
	}
	model.Reasoning = copyBool(model.Reasoning)
	model.Tools = copyBool(model.Tools)
	model.Vision = copyBool(model.Vision)
	model.Audio = copyBool(model.Audio)
	return model
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// CopyModelCatalog returns a detached provider-owned catalog snapshot.
func CopyModelCatalog(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, CopyModelInfo(model))
	}
	return out
}

// ResolveModelFromCatalog resolves an exact listed model and preserves the
// dynamic-route behavior for an unlisted model without inventing its facts.
func ResolveModelFromCatalog(provider string, models []ModelInfo, model string) (ModelInfo, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelInfo{}, fmt.Errorf("%w: model id is required", ErrModelUnavailable)
	}
	for _, info := range models {
		if info.ID == model {
			return CopyModelInfo(info), nil
		}
	}
	return ModelInfo{Provider: provider, ID: model}, nil
}

// ModelSupportsImages applies an exact catalog vision fact when one is owned;
// otherwise it preserves the adapter's standalone capability declaration.
func ModelSupportsImages(defaultValue bool, models []ModelInfo, model string) bool {
	for _, info := range models {
		if info.ID == model && info.Vision != nil {
			return *info.Vision
		}
	}
	return defaultValue
}

// ModelReasoningCapability returns the exact catalog reasoning fact when one
// is owned. The second result is false for unlisted or ID-only routes.
func ModelReasoningCapability(models []ModelInfo, model string) (supported bool, known bool) {
	for _, info := range models {
		if info.ID != model {
			continue
		}
		if info.ReasoningEfforts != nil {
			return true, true
		}
		if info.Reasoning != nil {
			return *info.Reasoning, true
		}
	}
	return false, false
}

// ModelDefaultMaxTokens returns only an explicitly owned request default.
// ModelInfo.MaxTokens is a model capability ceiling and must not be promoted
// into a per-request cap; DSH keeps those two facts separate. A zero result
// means that the provider's own protocol default remains authoritative.
func ModelDefaultMaxTokens(models []ModelInfo, model string) int {
	for _, info := range models {
		if info.ID == model && info.DefaultMaxTokens > 0 {
			return info.DefaultMaxTokens
		}
	}
	return 0
}

// ModelDefaultReasoningEffort returns the model-owned default effort. A
// catalog with an explicit effort map uses its first canonical non-off entry
// as the compatibility default when an older catalog omitted the dedicated
// field; this keeps legacy pinned facts equivalent to the DSH selector.
func ModelDefaultReasoningEffort(models []ModelInfo, model string) string {
	for _, info := range models {
		if info.ID != model {
			continue
		}
		if effort := strings.ToLower(strings.TrimSpace(info.DefaultReasoningEffort)); effort != "" {
			return effort
		}
		for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
			for candidate := range info.ReasoningEfforts {
				if strings.EqualFold(strings.TrimSpace(candidate), effort) {
					return effort
				}
			}
		}
		if info.Reasoning != nil && *info.Reasoning {
			return "high"
		}
	}
	return ""
}
