package loop

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
)

// TestRunPreStepInjectorsInOrder verifies that multiple PreStep injectors run
// in registration order and their context messages are persisted after the
// current user message (ADR
// 2026-08-18-m5-agent-core.md 总体决策: unified pre-step injection).
func TestRunPreStepInjectorsInOrder(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	var seenUserText string
	log := session.New()
	loop := New(Config{
		LLM:    model,
		Log:    log,
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{
			{Name: "kb", Inject: func(ctx context.Context, userText string) []llm.Message {
				seenUserText = userText
				return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("ctx-a")}}}
			}},
			{Name: "skills", Inject: func(ctx context.Context, userText string) []llm.Message {
				return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("ctx-b")}}}
			}},
		},
	})

	if err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seenUserText != "hello" {
		t.Fatalf("injector received userText %q, want %q", seenUserText, "hello")
	}
	if len(model.calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(model.calls))
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("first request messages = %+v, want system + ctx-a + ctx-b + user", msgs)
	}
	if msgs[0].Role != llm.RoleSystem || msgs[1].Role != llm.RoleUser || msgs[1].Text() != "hello" ||
		msgs[2].Role != llm.RoleUser || msgs[2].Text() != "ctx-a" ||
		msgs[3].Role != llm.RoleUser || msgs[3].Text() != "ctx-b" {
		t.Fatalf("first request messages out of order: %+v", msgs)
	}
}

// TestRunPreStepIsRebuiltForEveryStep verifies dsh-style pre-step projection:
// the injector is called for every step and its durable context remains in the
// derived history of the tool-call follow-up request.
func TestRunPreStepIsRebuiltForEveryStep(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{
		{ // step 1: model asks for get_time
			{Kind: llm.StreamFinish, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "get_time", Arguments: "{}"},
			}},
		},
		{ // step 2: model answers
			{Kind: llm.StreamTextDelta, Text: "It is now."},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		},
	}}
	var injectCalls int
	loop := New(Config{
		LLM:    model,
		Log:    session.New(),
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{{Name: "ctx", Inject: func(ctx context.Context, userText string) []llm.Message {
			injectCalls++
			return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("KEEP-ME")}}}
		}}},
	})

	if err := loop.Run(context.Background(), "what time is it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if injectCalls != 2 {
		t.Fatalf("pre-step injector called %d times, want 2 (once per step)", injectCalls)
	}
	if len(model.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(model.calls))
	}
	first := model.calls[0].Messages
	if len(first) < 3 || first[0].Role != llm.RoleSystem || first[1].Text() != "what time is it" || first[2].Text() != "KEEP-ME" {
		t.Fatalf("first request messages = %+v, want system + user + injected context", first)
	}
	found := false
	for _, m := range model.calls[1].Messages {
		if strings.Contains(m.Text(), "KEEP-ME") {
			found = true
		}
	}
	if !found {
		t.Fatalf("second request must carry the durable pre-step context: %+v", model.calls[1].Messages)
	}
}

// TestRunRecallRunsBeforePreStep verifies the M4b Recall compatibility alias:
// Recall, when set, is the first pre-step injector and PreStep injectors follow
// it in order (ADR 总体决策: Recall 保留作为兼容别名，PreStep 首项).
func TestRunRecallRunsBeforePreStep(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop := New(Config{
		LLM:    model,
		Log:    session.New(),
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		Recall: func(ctx context.Context, userText string) []llm.Message {
			return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("recall-msg")}}}
		},
		PreStep: []PreStepInjector{{Name: "extra", Inject: func(ctx context.Context, userText string) []llm.Message {
			return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("extra-msg")}}}
		}}},
	})

	if err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("first request messages = %+v, want system + recall-msg + extra-msg + user", msgs)
	}
	if msgs[1].Text() != "hi" || msgs[2].Text() != "recall-msg" || msgs[3].Text() != "extra-msg" {
		t.Fatalf("recall must precede pre-step injectors: %+v", msgs)
	}
}

// TestRunPreStepBudgetTruncation verifies the per-injector budget: a single
// over-budget message is truncated to maxInjectorChars runes, UTF-8-safely, and
// the turn still completes (fail-open, ADR 残余风险: 目录注入体积).
func TestRunPreStepBudgetTruncation(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	big := strings.Repeat("界", maxInjectorChars*2) // multibyte runes, far over budget
	loop := New(Config{
		LLM:    model,
		Log:    session.New(),
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{{Name: "big", Inject: func(ctx context.Context, userText string) []llm.Message {
			return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(big)}}}
		}}},
	})

	if err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("first request messages = %+v, want system + truncated + user", msgs)
	}
	got := msgs[2].Text()
	if utf8.RuneCountInString(got) != maxInjectorChars {
		t.Fatalf("truncated content has %d runes, want %d", utf8.RuneCountInString(got), maxInjectorChars)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated content is not valid UTF-8 (split a rune): %q", got)
	}
	if want := string([]rune(big)[:maxInjectorChars]); got != want {
		t.Fatalf("truncated content must be the UTF-8-safe head of the original")
	}
}

// TestRunPreStepAggregateBudget verifies the total-text budget across the
// messages one injector returns: earlier messages are kept whole until the
// budget is exhausted, the overflowing message is truncated, and the rest are
// dropped (总长有上限).
func TestRunPreStepAggregateBudget(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	first := strings.Repeat("a", 3000)
	second := strings.Repeat("b", 3000)
	loop := New(Config{
		LLM:    model,
		Log:    session.New(),
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{{Name: "multi", Inject: func(ctx context.Context, userText string) []llm.Message {
			return []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(first)}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(second)}},
			}
		}}},
	})

	if err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("first request messages = %+v, want system + first(whole) + second(truncated) + user", msgs)
	}
	if msgs[2].Text() != first {
		t.Fatalf("first message must be kept whole, got %d runes", utf8.RuneCountInString(msgs[2].Text()))
	}
	// 3000 + 1000 = 4000 budget; the second message is truncated to the remainder.
	if utf8.RuneCountInString(msgs[3].Text()) != maxInjectorChars-3000 {
		t.Fatalf("second message truncated to %d runes, want %d",
			utf8.RuneCountInString(msgs[3].Text()), maxInjectorChars-3000)
	}
	if msgs[3].Text() != second[:maxInjectorChars-3000] {
		t.Fatalf("second message must be the UTF-8-safe head: %q", msgs[3].Text())
	}
}

// TestRunPreStepPanicContained verifies a panicking injector is skipped
// (fail-open) and does not abort the turn or drop the other injectors' context.
func TestRunPreStepPanicContained(t *testing.T) {
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "fine"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop := New(Config{
		LLM:    model,
		Log:    session.New(),
		Tools:  newTestRegistry(t),
		Prompt: prompt.New("You are helpful."),
		Model:  "deepseek-chat",
		PreStep: []PreStepInjector{
			{Name: "boom", Inject: func(ctx context.Context, userText string) []llm.Message {
				panic("injector exploded")
			}},
			{Name: "ok", Inject: func(ctx context.Context, userText string) []llm.Message {
				return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("after-boom")}}}
			}},
		},
	})

	if err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run must survive an injector panic, got %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 3 || msgs[2].Text() != "after-boom" {
		t.Fatalf("panicking injector must be skipped and the rest kept: %+v", msgs)
	}
}

// TestRunPreStepNilSafe verifies nil inputs change nothing: no Recall and no
// PreStep injectors (or an injector with a nil Inject) leave the first request
// as a plain system + user history request.
func TestRunPreStepNilSafe(t *testing.T) {
	// No injectors at all.
	model := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamTextDelta, Text: "ok"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop, _, _ := newTestLoop(t, model)
	if err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs := model.calls[0].Messages
	if len(msgs) != 2 || msgs[0].Role != llm.RoleSystem || msgs[1].Role != llm.RoleUser {
		t.Fatalf("request messages = %+v, want system + user only", msgs)
	}

	// An injector with a nil Inject function contributes nothing.
	model2 := &scriptedLLM{steps: [][]llm.StreamEvent{{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}}
	loop2 := New(Config{
		LLM:     model2,
		Log:     session.New(),
		Tools:   newTestRegistry(t),
		Prompt:  prompt.New("You are helpful."),
		Model:   "deepseek-chat",
		PreStep: []PreStepInjector{{Name: "nil-inject", Inject: nil}},
	})
	if err := loop2.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs2 := model2.calls[0].Messages
	if len(msgs2) != 2 || msgs2[0].Role != llm.RoleSystem || msgs2[1].Role != llm.RoleUser {
		t.Fatalf("request messages = %+v, want system + user only", msgs2)
	}
}
