package llm

import (
	"encoding/json"
	"testing"
)

func TestTextBuildsTextBlock(t *testing.T) {
	b := Text("hello")
	if b.Kind != BlockText || b.Text != "hello" {
		t.Fatalf("Text() = %+v, want {kind:text text:hello}", b)
	}
}

func TestMessageTextConcatenatesAndExcludesReasoning(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []ContentBlock{
		{Kind: BlockReasoning, Text: "reasoning here"},
		Text("Hello "),
		Text("world"),
	}}
	if got := m.Text(); got != "Hello world" {
		t.Fatalf("Text() = %q, want %q (reasoning excluded)", got, "Hello world")
	}
	if got := m.Reasoning(); got != "reasoning here" {
		t.Fatalf("Reasoning() = %q, want %q", got, "reasoning here")
	}
}

func TestMessageTextEmpty(t *testing.T) {
	if got := (Message{}).Text(); got != "" {
		t.Fatalf("empty message Text() = %q, want empty", got)
	}
}

func TestMessageSetTextPreservesToolFields(t *testing.T) {
	m := Message{
		Role:       RoleAssistant,
		Content:    []ContentBlock{{Kind: BlockReasoning, Text: "r"}, Text("old")},
		ToolCallID: "ignored",
		ToolCalls:  []ToolCall{{ID: "c1", Name: "get_time", Arguments: "{}"}},
	}
	m.SetText("new")
	if len(m.Content) != 1 || m.Content[0].Kind != BlockText || m.Content[0].Text != "new" {
		t.Fatalf("SetText content = %+v, want a single text block", m.Content)
	}
	if m.Text() != "new" {
		t.Fatalf("Text() = %q, want new", m.Text())
	}
	// Tool fields must be untouched.
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "c1" || m.ToolCallID != "ignored" {
		t.Fatalf("SetText clobbered tool fields: %+v", m)
	}
}

func TestMessageHasImage(t *testing.T) {
	if (Message{Content: []ContentBlock{Text("x")}}).HasImage() {
		t.Fatal("plain text message must not have an image")
	}
	if !(Message{Content: []ContentBlock{{Kind: BlockImage}}}).HasImage() {
		t.Fatal("message with a top-level image block must have an image")
	}
	// Nested tool-result block (M8-3 recursion).
	nested := Message{Content: []ContentBlock{{
		Kind:   BlockToolResult,
		Blocks: []ContentBlock{{Kind: BlockImage}},
	}}}
	if !nested.HasImage() {
		t.Fatal("message with a nested image block must have an image")
	}
}

func TestValidateRequestBlocksRejectsMergeExtensibleInput(t *testing.T) {
	tests := []struct {
		name    string
		message Message
	}{
		{"audio", Message{Content: []ContentBlock{{
			Kind: "audio", Raw: json.RawMessage(`{"type":"audio","data":"aGk=","mimeType":"audio/wav"}`),
		}}}},
		{"nested tool result", Message{Role: RoleTool, Content: []ContentBlock{{
			Kind: BlockToolResult, CallID: "call", Blocks: []ContentBlock{{Kind: "resource"}},
		}}}},
		{"unknown extension", Message{Content: []ContentBlock{{
			Kind: "x-plugin/card", Raw: json.RawMessage(`{"type":"x-plugin/card"}`),
		}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.message.HasUnsupportedInput() {
				t.Fatalf("message = %#v, want unsupported request input", test.message)
			}
			err := ValidateRequestBlocks("test-provider", []Message{test.message})
			facts, ok := FailureFacts(err)
			if !ok || facts.Code != "UNSUPPORTED_INPUT_CONTENT" {
				t.Fatalf("error = %v, want UNSUPPORTED_INPUT_CONTENT", err)
			}
		})
	}
}

func TestValidateRequestBlocksAcceptsCoreVocabulary(t *testing.T) {
	messages := []Message{
		{Content: []ContentBlock{Text("hello"), {Kind: BlockReasoning, Text: "why"}}},
		{Role: RoleTool, Content: []ContentBlock{{
			Kind: BlockToolResult, CallID: "call", Blocks: []ContentBlock{Text("result")},
		}}},
	}
	if err := ValidateRequestBlocks("test-provider", messages); err != nil {
		t.Fatalf("core request content rejected: %v", err)
	}
}

func TestStreamEventReasoningField(t *testing.T) {
	ev := StreamEvent{Kind: StreamFinish, Reasoning: "accumulated", FinishReason: "stop"}
	if ev.Reasoning != "accumulated" || ev.FinishReason != "stop" {
		t.Fatalf("finish event = %+v", ev)
	}
	// The reasoning delta kind exists and is distinct from the text delta kind.
	if StreamReasoningDelta == StreamTextDelta {
		t.Fatal("StreamReasoningDelta must be a distinct kind")
	}
	d := StreamEvent{Kind: StreamReasoningDelta, Text: "t"}
	if d.Kind != StreamReasoningDelta {
		t.Fatalf("delta kind = %v, want StreamReasoningDelta", d.Kind)
	}
}
