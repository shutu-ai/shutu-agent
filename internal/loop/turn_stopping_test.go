package loop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestTurnStoppingCanClaimSameTurnNextStep(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{Kind: llm.StreamTextDelta, Text: "first"}, {Kind: llm.StreamFinish, FinishReason: "stop", Usage: llm.TokenUsage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4}}},
		{{Kind: llm.StreamTextDelta, Text: "second"}, {Kind: llm.StreamFinish, FinishReason: "stop", Usage: llm.TokenUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}}},
	}}
	reg := tools.New()
	log := session.New()
	var usage []llm.TokenUsage
	turn := New(Config{
		LLM: model, Log: log, Tools: reg, Prompt: prompt.New("system"), Model: "m",
		OnUsage: func(_ llm.ChatRequest, got llm.TokenUsage) { usage = append(usage, got) },
		TurnStoppingHooks: []TurnStoppingHook{
			func(ctx context.Context, p TurnStoppingPayload, next TurnStoppingNext) (TurnStoppingDecision, error) {
				if p.Step == 1 {
					return TurnStoppingDecision{Stop: false, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("same turn follow-up")}}}}, nil
				}
				return next(ctx, p)
			},
		},
	})
	if err := turn.Run(context.Background(), "initial"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	if len(usage) != 2 || usage[0].TotalTokens != 4 || usage[1].TotalTokens != 5 {
		t.Fatalf("usage = %+v, want both successful request measurements", usage)
	}
	var userTexts []string
	for _, event := range log.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			Text    string `json:"text"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("decode user event: %v", err)
		}
		text := data.Text
		if text == "" && len(data.Content) > 0 {
			text = data.Content[0].Text
		}
		userTexts = append(userTexts, text)
	}
	if len(userTexts) != 2 || userTexts[0] != "initial" || userTexts[1] != "same turn follow-up" {
		t.Fatalf("user messages = %q, want same-turn messages", userTexts)
	}
}

// TestTurnMaxTokensOutcomeIsStickyAfterNormalStep matches the reference
// waterfall: a tool step that hits the output cap remains max-tokens even if a
// turn-stopping hook drives a later step that finishes normally.
func TestTurnMaxTokensOutcomeIsStickyAfterNormalStep(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{{
			Kind: llm.StreamFinish, FinishReason: "length",
			ToolCalls: []llm.ToolCall{{
				ID: "call-max", Name: "get_time", Arguments: `{}`,
			}},
		}},
		{{Kind: llm.StreamTextDelta, Text: "normal tail"}, {Kind: llm.StreamFinish, FinishReason: "stop"}},
	}}
	reg := tools.New()
	if err := reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	log := session.New()
	turn := New(Config{
		LLM: model, Log: log, Tools: reg, Prompt: prompt.New("system"), Model: "m",
		TurnStoppingHooks: []TurnStoppingHook{
			func(ctx context.Context, p TurnStoppingPayload, next TurnStoppingNext) (TurnStoppingDecision, error) {
				if p.Step == 1 {
					return TurnStoppingDecision{Stop: false, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("same turn follow-up")}}}}, nil
				}
				return next(ctx, p)
			},
		},
	})
	if err := turn.Run(context.Background(), "initial"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want capped step plus normal tail", len(model.calls))
	}
	var end struct {
		Turn   int `json:"turn"`
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	events := log.Events()
	last := events[len(events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %q, want turn/end", last.Type)
	}
	if err := json.Unmarshal(last.Data, &end); err != nil {
		t.Fatalf("decode turn/end: %v", err)
	}
	if end.Turn != 1 || end.Reason.Kind != "max-tokens" {
		t.Fatalf("turn/end = %+v, want sticky max-tokens", end)
	}
}
