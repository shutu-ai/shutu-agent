// Message model (M8, ADR 2026-08-20-m8-message-model.md). Content is a list of
// tagged ContentBlock parts (see content.go) instead of a bare string, so
// reasoning and (in M8-3) image references can ride inside one message. The
// Text()/SetText() helpers keep every plain-text consumer working unchanged.
package llm

import "strings"

// ToolCall is one assistant-emitted tool invocation.
type ToolCall struct {
	ID        string // call correlation id, echoed by tool messages
	Name      string // registered tool name
	Arguments string // raw JSON string of the arguments
}

// Message is one entry of a chat history. Content is the parts list
// (system/user/assistant/tool content); ToolCallID and ToolCalls carry the
// tool round-trip on the Message layer (see field comments).
type Message struct {
	Role       Role
	Content    []ContentBlock // parts; use Text()/SetText() for plain text
	ToolCallID string         // role=tool only: the call id this result answers
	ToolCalls  []ToolCall     // role=assistant only: tool calls this message emitted
	// SourceKind/SourcePlugin are runtime-only provenance carried from a
	// pre-step extension into the durable session event. They are intentionally
	// not part of provider wire encoding.
	SourceKind   string
	SourcePlugin string
	// Team-message provenance is runtime-only until session.NewUserMessageAt
	// projects it into the durable source object. Persisted marks an input
	// already accepted by the target Session receipt transaction, so the loop
	// does not append a second user/message row.
	SourceTeamID     string
	SourceMessageID  string
	SourceSenderID   string
	SourceSenderName string
	Persisted        bool
}

// Text concatenates every BlockText block's Text. Reasoning blocks are
// excluded, so the helper reproduces the old single-string behavior for every
// plain-text consumer (compat for old readers).
func (m Message) Text() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Kind == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// SetText replaces Content with a single text block. ToolCalls and ToolCallID
// are left untouched — used by M8-1's truncation/injection paths, where the
// messages are plain text.
func (m *Message) SetText(s string) {
	m.Content = []ContentBlock{Text(s)}
}

// Reasoning concatenates every BlockReasoning block's Text (the assistant's
// reasoning text), symmetric with Text(). The deepseek wire layer uses it to
// re-encode reasoning_content on replay.
func (m Message) Reasoning() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Kind == BlockReasoning {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// HasImage reports whether Content contains an image block, recursing into
// nested tool-result blocks. M8-3 uses it; it is implemented now for tests.
func (m Message) HasImage() bool {
	return hasImageBlocks(m.Content)
}

func hasImageBlocks(blocks []ContentBlock) bool {
	for _, b := range blocks {
		if b.Kind == BlockImage {
			return true
		}
		if len(b.Blocks) > 0 && hasImageBlocks(b.Blocks) {
			return true
		}
	}
	return false
}

// HasUnsupportedInput reports whether message content contains a block the
// provider request layer cannot encode. The durable log may preserve
// merge-extensible audio/resource/vendor blocks, but preserving those bytes is
// not permission to send them to a provider that only honors the core request
// vocabulary. Checks recurse into nested tool-result content.
func (m Message) HasUnsupportedInput() bool {
	return hasUnsupportedRequestBlocks(m.Content)
}

func hasUnsupportedRequestBlocks(blocks []ContentBlock) bool {
	for _, block := range blocks {
		switch block.Kind {
		case BlockText, BlockReasoning, BlockImage, BlockToolCall, BlockToolResult:
		default:
			return true
		}
		if len(block.Blocks) > 0 && hasUnsupportedRequestBlocks(block.Blocks) {
			return true
		}
	}
	return false
}

// ValidateRequestBlocks rejects non-core request content before credentials,
// attachment reads, image offloading, and network I/O. The stable code lets
// transports and replay clients classify audio/vendor input without parsing
// provider-specific diagnostics.
func ValidateRequestBlocks(provider string, messages []Message) error {
	for _, message := range messages {
		if message.HasUnsupportedInput() {
			return NewFailureError(
				provider+": request contains an unsupported content block",
				"UNSUPPORTED_INPUT_CONTENT", nil,
			)
		}
	}
	return nil
}
