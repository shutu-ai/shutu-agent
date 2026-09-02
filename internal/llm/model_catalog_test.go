package llm

import (
	"context"
	"errors"
	"testing"
)

func TestValidateReasoningEffortForModelValidatesVocabulary(t *testing.T) {
	if err := ValidateReasoningEffortForModel("anthropic", nil, "", ""); err != nil {
		t.Fatalf("empty effort = %v", err)
	}
	if err := ValidateReasoningEffortForModel("anthropic", nil, "", "off"); err != nil {
		t.Fatalf("off effort = %v", err)
	}
	err := ValidateReasoningEffortForModel("anthropic", nil, "", "bogus")
	failure, ok := FailureFacts(err)
	if !ok || failure.Code != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("failure = %+v (typed=%v), want UNSUPPORTED_REASONING_EFFORT", failure, ok)
	}
}

func TestModelCatalogRejectsUnsupportedInputModalities(t *testing.T) {
	err := validateModelInfo("test", ModelInfo{ID: "m", Input: []string{"audio"}})
	if err == nil {
		t.Fatal("audio modality must not be advertised by a text/image-only runtime")
	}
}

func TestCopyModelInfoDetachesNestedCatalogFacts(t *testing.T) {
	reasoning, vision := true, false
	wire := "high"
	original := ModelInfo{
		ID: "m", Input: []string{"text", "image"}, Reasoning: &reasoning, Vision: &vision,
		ReasoningEfforts: map[string]*string{"high": &wire, "off": nil},
	}
	copy := CopyModelInfo(original)
	original.Input[0] = "audio"
	*original.Reasoning = false
	*original.ReasoningEfforts["high"] = "changed"
	copy.Input[1] = "text"
	*copy.Vision = true
	if original.Input[0] != "audio" || copy.Input[0] != "text" {
		t.Fatalf("input aliases remain: original=%v copy=%v", original.Input, copy.Input)
	}
	if *copy.Reasoning != true || *original.Vision != false {
		t.Fatalf("capability pointers alias: original reasoning=%v vision=%v copy reasoning=%v", *original.Reasoning, *original.Vision, *copy.Reasoning)
	}
	originalWire, originalOK := original.ReasoningEfforts["high"]
	copyWire, copyOK := copy.ReasoningEfforts["high"]
	if !originalOK || !copyOK || originalWire == nil || copyWire == nil || *copyWire != "high" || *originalWire != "changed" {
		t.Fatalf("reasoning map aliases remain: original=%v copy=%v", original.ReasoningEfforts, copy.ReasoningEfforts)
	}
}

func TestReasoningEffortCatalogMapsCanonicalToWireAndRejectsMissingLevels(t *testing.T) {
	wire := "high"
	models := []ModelInfo{{ID: "mapped", ReasoningEfforts: map[string]*string{
		"off": nil, "xhigh": &wire,
	}}}
	if got, err := ResolveReasoningEffortWire("test", models, "mapped", "xhigh"); err != nil || got != "high" {
		t.Fatalf("mapped effort = %q, err=%v", got, err)
	}
	if got, err := ResolveReasoningEffortWire("test", models, "mapped", "off"); err != nil || got != "" {
		t.Fatalf("off effort = %q, err=%v", got, err)
	}
	if err := ValidateReasoningEffortForModel("test", models, "mapped", "low"); err == nil {
		t.Fatal("effort missing from an authoritative map must be rejected")
	}
}

func TestModelDefaultReasoningEffortUsesOwnedMetadataAndLegacyFacts(t *testing.T) {
	if got := ModelDefaultReasoningEffort([]ModelInfo{{ID: "explicit", DefaultReasoningEffort: "low"}}, "explicit"); got != "low" {
		t.Fatalf("explicit default effort = %q", got)
	}
	if got := ModelDefaultReasoningEffort([]ModelInfo{{ID: "mapped", ReasoningEfforts: map[string]*string{"off": nil, "high": strptr("high")}}}, "mapped"); got != "high" {
		t.Fatalf("mapped default effort = %q", got)
	}
	reasoning := true
	if got := ModelDefaultReasoningEffort([]ModelInfo{{ID: "legacy", Reasoning: &reasoning}}, "legacy"); got != "high" {
		t.Fatalf("legacy default effort = %q", got)
	}
}

func TestModelDefaultMaxTokensDoesNotUseCapacity(t *testing.T) {
	models := []ModelInfo{
		{ID: "capacity-only", MaxTokens: 16384},
		{ID: "explicit", MaxTokens: 65536, DefaultMaxTokens: 8192},
	}
	if got := ModelDefaultMaxTokens(models, "capacity-only"); got != 0 {
		t.Fatalf("capacity-only default = %d, want zero", got)
	}
	if got := ModelDefaultMaxTokens(models, "explicit"); got != 8192 {
		t.Fatalf("explicit default = %d, want 8192", got)
	}
	if got := ModelDefaultMaxTokens(models, "unlisted"); got != 0 {
		t.Fatalf("unlisted default = %d, want zero", got)
	}
}

func strptr(value string) *string { return &value }

func TestModelCatalogRejectsInvalidDefaultReasoningEffort(t *testing.T) {
	reasoning := true
	if err := validateModelInfo("test", ModelInfo{ID: "m", Reasoning: &reasoning, DefaultReasoningEffort: "bogus"}); err == nil {
		t.Fatal("invalid default reasoning effort must be rejected")
	}
	if err := validateModelInfo("test", ModelInfo{ID: "m", Reasoning: &reasoning, ReasoningEfforts: map[string]*string{"high": strptr("high")}, DefaultReasoningEffort: "low"}); err == nil {
		t.Fatal("default reasoning effort absent from authoritative map must be rejected")
	}
}

func TestReasoningEffortCatalogMatchesCaseInsensitiveProviderKeys(t *testing.T) {
	wire := "HIGH"
	models := []ModelInfo{{ID: "m", ReasoningEfforts: map[string]*string{"HIGH": &wire}, DefaultReasoningEffort: "high"}}
	if got, err := ResolveReasoningEffortWire("test", models, "m", "high"); err != nil || got != "HIGH" {
		t.Fatalf("case-insensitive effort = %q, err=%v", got, err)
	}
	if err := validateModelInfo("test", models[0]); err != nil {
		t.Fatalf("case-insensitive catalog should validate: %v", err)
	}
}

type catalogTestProvider struct {
	id     string
	models []ModelInfo
	err    error
}

func (p catalogTestProvider) ID() string      { return p.id }
func (p catalogTestProvider) Available() bool { return true }
func (p catalogTestProvider) Stream(context.Context, ChatRequest) (StreamReader, error) {
	return nil, errors.New("not used")
}
func (p catalogTestProvider) ListModels(context.Context) ([]ModelInfo, error) {
	return p.models, p.err
}
func (p catalogTestProvider) ResolveModelInfo(_ context.Context, id string) (ModelInfo, error) {
	for _, model := range p.models {
		if model.ID == id {
			return model, nil
		}
	}
	return ModelInfo{}, errors.New("not found")
}

func TestRegistryModelCatalogPreservesProviderFailuresAndMetadata(t *testing.T) {
	r := NewRegistry()
	vision := true
	if err := r.Register(catalogTestProvider{id: "good", models: []ModelInfo{{Provider: "good", ID: "m", ContextWindow: 32000, Vision: &vision}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(catalogTestProvider{id: "broken", err: errors.New("offline")}); err != nil {
		t.Fatal(err)
	}
	results := r.ListModelCatalog(context.Background())
	if len(results) != 2 || len(results[0].Models) != 1 || results[0].Models[0].ContextWindow != 32000 || results[0].Models[0].Vision == nil || !*results[0].Models[0].Vision {
		t.Fatalf("catalog results = %#v", results)
	}
	if results[1].Error == nil || !errors.Is(results[1].Error, ErrModelCatalogUnavailable) {
		t.Fatalf("broken catalog error = %v", results[1].Error)
	}
	resolved, err := r.ResolveModelInfo(context.Background(), "good", "m")
	if err != nil || resolved.ContextWindow != 32000 {
		t.Fatalf("resolved model = %+v, err=%v", resolved, err)
	}
}

func TestModelReasoningCapabilityPreservesUnknownAndExplicitFalse(t *testing.T) {
	trueValue, falseValue := true, false
	models := []ModelInfo{
		{ID: "thinking", Reasoning: &trueValue},
		{ID: "text-only", Reasoning: &falseValue},
		{ID: "id-only"},
	}
	if supported, known := ModelReasoningCapability(models, "thinking"); !supported || !known {
		t.Fatalf("thinking capability = %v, %v", supported, known)
	}
	if supported, known := ModelReasoningCapability(models, "text-only"); supported || !known {
		t.Fatalf("text-only capability = %v, %v", supported, known)
	}
	if supported, known := ModelReasoningCapability(models, "id-only"); supported || known {
		t.Fatalf("id-only capability = %v, %v", supported, known)
	}
}

func TestRegistryModelCatalogRejectsDuplicateAndCrossProviderRows(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(catalogTestProvider{id: "bad", models: []ModelInfo{{ID: "same"}, {ID: "same"}}}); err != nil {
		t.Fatal(err)
	}
	if results := r.ListModelCatalog(context.Background()); len(results) != 1 || results[0].Error == nil {
		t.Fatalf("duplicate result = %#v", results)
	}
	if err := r.Register(catalogTestProvider{id: "wrong", models: []ModelInfo{{Provider: "other", ID: "m"}}}); err != nil {
		t.Fatal(err)
	}
	if results := r.ListModelCatalog(context.Background()); len(results) != 2 || results[1].Error == nil {
		t.Fatalf("cross-provider result = %#v", results)
	}
}
