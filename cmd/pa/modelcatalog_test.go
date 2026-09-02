package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

type routeImageProvider struct{ id string }

func (p routeImageProvider) ID() string      { return p.id }
func (p routeImageProvider) Available() bool { return true }
func (p routeImageProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return nil, nil
}
func (p routeImageProvider) SupportsImages() bool { return true }

// TestLLMSupportsImagesForRoute proves the SDK's pre-session admission uses
// the exact provider/model catalog route, not only the provider adapter.
func TestLLMSupportsImagesForRoute(t *testing.T) {
	yes := true
	no := false
	a := &app{
		cfg: config.Config{LLM: config.LLMConfig{ModelInputModalities: "text,image"}},
		customProviders: []customProviderProfile{{
			ID: "route-gw", Model: "vision-model",
			Models: []customModel{{ID: "text-model", Vision: &no}},
		}},
		llmReg: llm.NewRegistry(),
	}
	if err := a.llmReg.Register(routeImageProvider{id: "route-gw"}); err != nil {
		t.Fatal(err)
	}
	if a.llmSupportsImagesForRoute("route-gw", "text-model") {
		t.Fatal("catalog text-only route passed image admission")
	}
	a.customProviders[0].Models[0].ID = "vision-model"
	a.customProviders[0].Models[0].Vision = &yes
	if !a.llmSupportsImagesForRoute("route-gw", "vision-model") {
		t.Fatal("catalog vision route failed image admission")
	}
}

// TestSessionModelSelectionRejectsTextRouteForDurableImages pins the DSH
// selection boundary: a route that is valid for text cannot be persisted once
// the current model-visible session history contains an image.
func TestSessionModelSelectionRejectsTextRouteForDurableImages(t *testing.T) {
	no := false
	a := &app{
		cfg:         config.Config{Model: "text-model", LLM: config.LLMConfig{Provider: "route-gw"}},
		runtimeLogs: map[string]*session.Log{},
		customProviders: []customProviderProfile{{
			ID: "route-gw", Model: "text-model",
			Models: []customModel{{ID: "text-model", Vision: &no}},
		}},
		llmReg: llm.NewRegistry(),
	}
	if err := a.llmReg.Register(routeImageProvider{id: "route-gw"}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageWithBlocks("see", []llm.ContentBlock{{Kind: llm.BlockImage}})); err != nil {
		t.Fatal(err)
	}
	a.runtimeLogs["image-session"] = log

	err := a.validateSessionModelSelection(context.Background(), "image-session", "route-gw", "text-model", "")
	if err == nil || !errors.Is(err, llm.ErrCapabilityUnavailable) {
		t.Fatalf("text-only selection error = %v, want ErrCapabilityUnavailable", err)
	}
}

func TestSessionModelSelectionRejectsTextRouteForPendingImages(t *testing.T) {
	no := false
	a := &app{
		cfg:         config.Config{Model: "text-model", LLM: config.LLMConfig{Provider: "route-gw"}},
		runtimeLogs: map[string]*session.Log{},
		customProviders: []customProviderProfile{{
			ID: "route-gw", Model: "text-model",
			Models: []customModel{{ID: "text-model", Vision: &no}},
		}},
		llmReg: llm.NewRegistry(),
	}
	if err := a.llmReg.Register(routeImageProvider{id: "route-gw"}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	inbox, err := agent.NewDurableInbox(sessionInboxJournal{log: log}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.SendContent([]llm.ContentBlock{{Kind: llm.BlockImage}}, nil); err != nil {
		t.Fatal(err)
	}
	pending := inbox.PendingMessages()
	if len(pending) != 1 {
		t.Fatalf("pending messages = %d, want 1", len(pending))
	}
	a.runtimeLogs["pending-image-session"] = log

	err = a.validateSessionModelSelection(context.Background(), "pending-image-session", "route-gw", "text-model", "")
	if err == nil || !errors.Is(err, llm.ErrCapabilityUnavailable) {
		t.Fatalf("pending image selection error = %v, want ErrCapabilityUnavailable", err)
	}
}

// TestBuildLoopUsesExplicitRouteCatalog verifies that every capability-derived
// loop field follows the final provider/model pair passed to assembly. In
// particular, an explicit route must not inherit the global session model's
// output budget just because the caller has not persisted a session override.
func TestBuildLoopUsesExplicitRouteCatalog(t *testing.T) {
	no := false
	a := &app{
		cfg: config.Config{
			Model: "global-model",
			LLM:   config.LLMConfig{Provider: "global-gw"},
		},
		customProviders: []customProviderProfile{
			{ID: "global-gw", Model: "global-model", Models: []customModel{{ID: "global-model", MaxTokens: 111, ContextWindow: 1111, Vision: &no}}},
			{ID: "explicit-gw", Model: "explicit-model", Models: []customModel{{ID: "explicit-model", MaxTokens: 222, DefaultMaxTokens: 333, ContextWindow: 2222, Vision: &no}}},
		},
		llmReg: llm.NewRegistry(),
	}
	for _, id := range []string{"global-gw", "explicit-gw"} {
		if err := a.llmReg.Register(routeImageProvider{id: id}); err != nil {
			t.Fatal(err)
		}
	}

	route := a.buildLoopBoundWithProvider(nil, nil, "", "explicit-gw", "explicit-model", "", "standard", nil, nil, nil, nil)
	if route.MaxTokens() != 333 || route.ContextWindow() != 2222 {
		t.Fatalf("explicit route limits = max %d/context %d, want 333/2222", route.MaxTokens(), route.ContextWindow())
	}
}

func TestModelCapacityIsNotPromotedToRequestDefault(t *testing.T) {
	if got := effectiveModelOutputLimit(0, 0, 222); got != 0 {
		t.Fatalf("implicit request budget = %d, want zero when only model capacity is known", got)
	}
	if got := effectiveModelOutputLimit(0, 333, 222); got != 333 {
		t.Fatalf("model request default = %d, want 333", got)
	}
}
