package main

import (
	"reflect"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
)

// TestDeepSeekReferenceCatalogMetadata pins the first-party facts disclosed by
// the pinned reference adapter. This is not a guess about other providers.
func TestDeepSeekReferenceCatalogMetadata(t *testing.T) {
	cfg := config.Config{LLM: config.LLMConfig{ModelInputModalities: "text,image"}}
	a := &app{cfg: cfg}
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		got := a.modelCapabilityForRoute("deepseek-official", model)
		if got.ContextWindow != 1_000_000 || got.MaxTokens != 384_000 || got.DefaultMaxTokens != 256_000 {
			t.Fatalf("%s capacity = %+v", model, got)
		}
		if !got.ReasoningKnown || !got.Reasoning || !got.Tools {
			t.Fatalf("%s reasoning/tool capability = %+v", model, got)
		}
		if got.Vision || got.Audio {
			t.Fatalf("%s modality capability = %+v, want text-only", model, got)
		}
	}

	// The reference treats an uncatalogued endpoint as text-only rather than
	// letting the deployment invent an image capability from global settings.
	got := a.modelCapabilityForRoute("deepseek-official", "private-uncatalogued")
	if got.Vision || got.Audio || got.ReasoningKnown {
		t.Fatalf("uncatalogued route capability = %+v, want unverified reasoning and text-only", got)
	}
}

func TestModelProfileOverrideInheritsUnchangedDeepSeekDefaults(t *testing.T) {
	a := &app{builtinProfiles: map[string]builtinProviderProfile{
		"deepseek-official": {Models: []customModel{{ID: "deepseek-v4-flash", MaxTokens: 128000}}},
	}}
	got := a.modelCapabilityForRoute("deepseek-official", "deepseek-v4-flash")
	if got.MaxTokens != 128000 || got.DefaultMaxTokens != 256000 || got.DefaultReasoningEffort != "high" {
		t.Fatalf("profile override inheritance = %+v", got)
	}
}

// TestGeneratedModelCatalogCoversPinnedUpstream ensures the embedded inventory
// is the pinned pi-ai package, not a hand-picked secondary catalog. Curated
// facts must merge by ID while upstream-only routes remain visible.
func TestGeneratedModelCatalogCoversPinnedUpstream(t *testing.T) {
	required := []string{
		"openai", "openrouter", "together", "groq", "mistral", "nvidia",
		"huggingface", "anthropic", "google", "xai", "deepseek-official",
	}
	total := 0
	if len(builtinModelCatalog) < 30 {
		t.Fatalf("pinned catalog provider count = %d, want complete upstream provider inventory", len(builtinModelCatalog))
	}
	for _, models := range builtinModelCatalog {
		total += len(models)
	}
	if total < 1000 {
		t.Fatalf("pinned catalog exposed %d rows, want complete upstream coverage", total)
	}
	for _, provider := range required {
		if len(builtinModelCatalog[provider]) == 0 {
			t.Fatalf("pinned provider %q has no catalog rows", provider)
		}
	}
	for _, provider := range required {
		models := builtinModelCatalog[provider]
		if len(models) == 0 {
			t.Fatalf("pinned provider %q has no catalog rows", provider)
		}
		seen := make(map[string]struct{}, len(models))
		for _, model := range models {
			if model.ID == "" {
				t.Fatalf("%s contains a blank model id", provider)
			}
			if _, duplicate := seen[model.ID]; duplicate {
				t.Fatalf("%s repeats model %q", provider, model.ID)
			}
			seen[model.ID] = struct{}{}
			total++
		}
	}
	if total < 400 {
		t.Fatalf("required providers exposed %d rows, want pinned upstream coverage", total)
	}

	gpt5, found := builtinModelEntry("openai", "gpt-5")
	if !found || gpt5.DefaultReasoningEffort != "minimal" || gpt5.ContextWindow != 400000 {
		t.Fatalf("upstream gpt-5 catalog = %#v found=%v", gpt5, found)
	}
	gpt4o, found := builtinModelEntry("openai", "gpt-4o")
	if !found || gpt4o.Tools == nil || !*gpt4o.Tools {
		t.Fatalf("curated gpt-4o tool fact = %#v found=%v", gpt4o, found)
	}
	deepseek, found := builtinModelEntry("deepseek-official", "deepseek-v4-flash")
	if !found || deepseek.DefaultMaxTokens != 256000 {
		t.Fatalf("curated DeepSeek default = %#v found=%v", deepseek, found)
	}
}

func TestModelCatalogRowsOnlyExposeOwnedFacts(t *testing.T) {
	rows := modelCatalogRows("deepseek-official", nil, nil)
	if len(rows) != 2 || rows[0]["id"] != "deepseek-v4-flash" {
		t.Fatalf("DeepSeek catalog rows = %#v", rows)
	}
	if rows[0]["contextWindow"] != 1_000_000 || rows[0]["maxTokens"] != 384_000 || rows[0]["vision"] != false {
		t.Fatalf("DeepSeek owned metadata = %#v", rows[0])
	}
	if got := rows[0]["input"]; !reflect.DeepEqual(got, []string{"text"}) {
		t.Fatalf("DeepSeek input metadata = %#v, want text-only", got)
	}
	rows = modelCatalogRows("openai", nil, nil)
	if len(rows) <= 4 {
		t.Fatalf("OpenAI installed catalog rows = %#v, want the pinned upstream inventory", rows)
	}
	var gpt4o map[string]any
	for _, row := range rows {
		if row["id"] == "gpt-4o" {
			gpt4o = row
			break
		}
	}
	if gpt4o == nil || gpt4o["contextWindow"] != 128000 || gpt4o["vision"] != true {
		t.Fatalf("OpenAI gpt-4o owned metadata = %#v", gpt4o)
	}
	if got := gpt4o["input"]; !reflect.DeepEqual(got, []string{"text", "image"}) {
		t.Fatalf("OpenAI input metadata = %#v, want text+image", got)
	}
	profiles := map[string]builtinProviderProfile{"custom": {Models: []customModel{{ID: "user-model", Tools: catalogBool(false)}}}}
	rows = modelCatalogRows("custom", profiles, nil)
	if len(rows) != 1 || rows[0]["tools"] != false {
		t.Fatalf("profile-owned metadata = %#v", rows)
	}
}

func TestNonDeepSeekCatalogEntriesPreservePinnedFacts(t *testing.T) {
	a := &app{cfg: config.Config{LLM: config.LLMConfig{ModelInputModalities: "text,image"}}}
	tests := []struct {
		provider string
		model    string
		context  int
		max      int
		vision   bool
	}{
		{"openrouter", "openai/gpt-4o", 128000, 16384, true},
		{"together", "meta-llama/Llama-3.3-70B-Instruct-Turbo", 131072, 131072, false},
		{"groq", "llama-3.3-70b-versatile", 131072, 32768, false},
		{"mistral", "mistral-large-latest", 262144, 262144, true},
		{"nvidia", "meta/llama-3.3-70b-instruct", 128000, 4096, false},
		{"huggingface", "meta-llama/Llama-3.3-70B-Instruct", 131072, 4096, false},
	}
	for _, test := range tests {
		got := a.modelCapabilityForRoute(test.provider, test.model)
		if got.ContextWindow != test.context || got.MaxTokens != test.max || !got.ReasoningKnown || got.Reasoning || got.Vision != test.vision || got.Audio {
			t.Fatalf("%s/%s capability = %+v, want context=%d max=%d vision=%v non-reasoning", test.provider, test.model, got, test.context, test.max, test.vision)
		}
	}
}

func TestModelInputDeclarationOverridesDeploymentVisionFallback(t *testing.T) {
	a := &app{cfg: config.Config{LLM: config.LLMConfig{ModelInputModalities: "text,image"}}, customProviders: []customProviderProfile{{
		ID: "catalog-gw", Models: []customModel{{ID: "text-only", Input: []string{"text"}}, {ID: "vision", Input: []string{"text", "image"}}},
	}}}
	textOnly := a.modelCapabilityForRoute("catalog-gw", "text-only")
	if textOnly.Vision {
		t.Fatalf("model-level text input was overridden by deployment modalities: %+v", textOnly)
	}
	vision := a.modelCapabilityForRoute("catalog-gw", "vision")
	if !vision.Vision {
		t.Fatalf("model-level image input was not projected: %+v", vision)
	}
	infos := providerModelCatalogInfos("catalog-gw", nil, a.customProviders)
	if len(infos) != 2 || infos[0].Vision == nil || *infos[0].Vision {
		t.Fatalf("provider catalog did not preserve text-only input: %#v", infos)
	}
}

func TestCustomModelCatalogRejectsUnsupportedOrDuplicateInput(t *testing.T) {
	if err := validateCustomModels([]customModel{{ID: "m", Input: []string{"audio"}}}); err == nil {
		t.Fatal("audio input must remain explicitly unsupported until the runtime can encode it")
	}
	if err := validateCustomModels([]customModel{{ID: "m", Input: []string{"text", "text"}}}); err == nil {
		t.Fatal("duplicate input modalities must be rejected")
	}
	if err := validateCustomModels([]customModel{{ID: "m"}, {ID: "m"}}); err == nil {
		t.Fatal("duplicate model ids must be rejected")
	}
}

func TestProviderModelDefaultsToFirstCatalogEntry(t *testing.T) {
	got := effectiveCustomProviderModel(customProviderProfile{
		ID:     "gateway",
		Models: []customModel{{ID: "first"}, {ID: "second"}},
	})
	if got != "first" {
		t.Fatalf("effective custom provider model = %q, want first catalog entry", got)
	}
	if got := effectiveProfileModel(builtinProviderProfile{Models: []customModel{{ID: "profile-first"}}}, "fallback"); got != "profile-first" {
		t.Fatalf("effective profile model = %q, want profile-first", got)
	}
}

func TestReasoningEffortCatalogIsProjectedToProviderAndWebRows(t *testing.T) {
	wire := "high"
	models := []customModel{{ID: "mapped", ReasoningEfforts: map[string]*string{"off": nil, "xhigh": &wire}, DefaultReasoningEffort: "xhigh"}}
	infos := providerModelCatalogInfos("gateway", nil, []customProviderProfile{{ID: "gateway", Models: models}})
	if len(infos) != 1 || infos[0].ReasoningEfforts["xhigh"] == nil || *infos[0].ReasoningEfforts["xhigh"] != "high" || infos[0].DefaultReasoningEffort != "xhigh" {
		t.Fatalf("provider reasoning map = %#v", infos)
	}
	rows := modelCatalogRows("gateway", nil, []customProviderProfile{{ID: "gateway", Models: models}})
	if len(rows) != 1 || rows[0]["reasoningEfforts"] == nil || rows[0]["defaultEffort"] != "xhigh" {
		t.Fatalf("web reasoning map = %#v", rows)
	}
}
