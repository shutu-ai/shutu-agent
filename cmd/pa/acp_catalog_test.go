package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

type catalogContextProvider struct{}

func (catalogContextProvider) ID() string      { return "catalog-gw" }
func (catalogContextProvider) Available() bool { return true }
func (catalogContextProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return &catalogContextReader{}, nil
}

type catalogContextReader struct{ done bool }

func (r *catalogContextReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	r.done = true
	return llm.StreamEvent{Kind: llm.StreamFinish, FinishReason: "stop"}, nil
}
func (r *catalogContextReader) Close() error { return nil }

// TestACPTurnUsesCatalogContextWindow catches a transport-local context
// default: ACP must emit the same durable request/context capacity as native
// and SDK loop assembly for the identical catalog route.
func TestACPTurnUsesCatalogContextWindow(t *testing.T) {
	a := &app{
		cfg: config.Config{LLM: config.LLMConfig{ModelInputModalities: "text"}},
		customProviders: []customProviderProfile{{
			ID: "catalog-gw", Model: "catalog-model",
			Models: []customModel{{
				ID: "catalog-model", ContextWindow: 32000, MaxTokens: 1024,
			}},
		}},
		llmReg: llm.NewRegistry(),
	}
	if err := a.llmReg.Register(catalogContextProvider{}); err != nil {
		t.Fatal(err)
	}
	s := &acpSession{
		app:      a,
		id:       "acp-catalog-context",
		log:      session.New(),
		registry: tools.New(),
		prompt:   prompt.New("You are helpful."),
		provider: "catalog-gw",
		model:    "catalog-model",
		mode:     "standard",
	}
	if err := s.executePromptMessages(context.Background(), []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("catalog context")},
	}}); err != nil {
		t.Fatalf("ACP turn: %v", err)
	}
	var sawContext bool
	for _, event := range s.log.Events() {
		if event.Type != session.EventRequestContext {
			continue
		}
		var data struct {
			ContextWindow int `json:"contextWindow"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.ContextWindow != 32000 {
			t.Fatalf("ACP request context = %d, want 32000", data.ContextWindow)
		}
		sawContext = true
	}
	if !sawContext {
		t.Fatalf("ACP events lack request/context: %+v", s.log.Events())
	}
}
