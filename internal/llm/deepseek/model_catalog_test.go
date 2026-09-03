package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

func TestModelCatalogPreservesOwnedFactsAndAllowsUnlistedRoutes(t *testing.T) {
	vision := false
	client := New(Config{ModelCatalog: []llm.ModelInfo{{
		Provider: "deepseek-official", ID: "owned", ContextWindow: 1000000, Vision: &vision,
	}}})
	models, err := client.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ContextWindow != 1000000 || models[0].Vision == nil || *models[0].Vision {
		t.Fatalf("models = %#v, err=%v", models, err)
	}
	resolved, err := client.ResolveModelInfo(context.Background(), "owned")
	if err != nil || resolved.ID != "owned" || resolved.ContextWindow != 1000000 {
		t.Fatalf("owned resolve = %+v, err=%v", resolved, err)
	}
	if resolved.DefaultMaxTokens != 256000 {
		t.Fatalf("owned default max tokens = %d, want 256000", resolved.DefaultMaxTokens)
	}
	unlisted, err := client.ResolveModelInfo(context.Background(), "private-route")
	if err != nil || unlisted.ID != "private-route" || unlisted.ContextWindow != 0 {
		t.Fatalf("unlisted resolve = %+v, err=%v", unlisted, err)
	}
	if unlisted.DefaultMaxTokens != 256000 || unlisted.Name != "private-route" || len(unlisted.Input) != 1 || unlisted.Input[0] != "text" {
		t.Fatalf("unlisted DSH defaults = %+v", unlisted)
	}
}

func TestStreamMaterializesProviderDefaultMaxTokens(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	reader, err := client.Stream(context.Background(), llm.ChatRequest{Model: "private-route"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("next: %v", nextErr)
		}
	}
	if got, ok := gotBody["max_tokens"].(float64); !ok || int(got) != 256000 {
		t.Fatalf("max_tokens = %#v, want 256000", gotBody["max_tokens"])
	}
}

func TestModelCatalogSnapshotDoesNotAliasConfig(t *testing.T) {
	reasoning := true
	wire := "high"
	input := []string{"text"}
	catalog := []llm.ModelInfo{{
		Provider: "deepseek-official", ID: "owned", Input: input,
		Reasoning: &reasoning, ReasoningEfforts: map[string]*string{"high": &wire},
	}}
	client := New(Config{ModelCatalog: catalog})
	input[0] = "image"
	reasoning = false
	wire = "changed"
	models, err := client.ListModels(context.Background())
	modelWire, modelWireOK := models[0].ReasoningEfforts["high"]
	if err != nil || len(models) != 1 || models[0].Input[0] != "text" || models[0].Reasoning == nil || *models[0].Reasoning != true || !modelWireOK || modelWire == nil || *modelWire != "high" {
		t.Fatalf("catalog was mutated through config aliases: models=%+v err=%v", models, err)
	}
	models[0].Input[0] = "image"
	*models[0].Reasoning = false
	*models[0].ReasoningEfforts["high"] = "changed-again"
	again, err := client.ResolveModelInfo(context.Background(), "owned")
	againWire, againWireOK := again.ReasoningEfforts["high"]
	if err != nil || again.Input[0] != "text" || again.Reasoning == nil || *again.Reasoning != true || !againWireOK || againWire == nil || *againWire != "high" {
		t.Fatalf("catalog was mutated through ListModels result: model=%+v err=%v", again, err)
	}
}

func TestStreamKnownCatalogImageNegativeOverridesGlobalCapability(t *testing.T) {
	vision := false
	client := New(Config{
		APIKey:         "test-key",
		Model:          "text-model",
		SupportsImages: true,
		ModelCatalog: []llm.ModelInfo{{
			Provider: "deepseek-official", ID: "text-model", Vision: &vision,
		}},
	})
	_, err := client.Stream(context.Background(), llm.ChatRequest{
		Model: "text-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png"},
		}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "model does not support image input") {
		t.Fatalf("err = %v, want catalog-owned negative image capability", err)
	}
}
