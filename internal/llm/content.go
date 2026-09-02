// Content parts for llm.Message (M8, ADR 2026-08-20-m8-message-model.md). A
// message's Content is a tagged union of blocks instead of a bare string, so
// reasoning (BlockReasoning), multimodal attachments (BlockImage, M8-3) and
// tool blocks can coexist in one message without double-track types.
package llm

import "encoding/json"

// ContentBlockKind discriminates ContentBlock values (the tagged-union style
// mirrors WebFetchBody's Kind discriminator, design.md §10 D2).
type ContentBlockKind string

const (
	// BlockText is a plain text block (system/user/assistant/tool content).
	BlockText ContentBlockKind = "text"
	// BlockReasoning is the assistant's reasoning text (DeepSeek
	// reasoning_content). It is provider-neutral in the log (D3) and is
	// re-encoded per provider wire rules when replayed (M8-2).
	BlockReasoning ContentBlockKind = "reasoning"
	// BlockImage is an attachment reference (M8-3 uses it; this milestone only
	// defines the type). The log stores the ImageRef, never base64 bytes.
	BlockImage ContentBlockKind = "image"
	// BlockToolCall and BlockToolResult are reserved vocabulary. Tool calls
	// still travel on the Message layer (ToolCalls) in this milestone.
	BlockToolCall   ContentBlockKind = "tool-call"
	BlockToolResult ContentBlockKind = "tool-result"
)

// ImageRef is a reference to an image attachment (M8-3 uses it; this milestone
// only defines the type). Only the reference is logged or carried in memory —
// the bytes are read from Path at request time and turned into a data URL.
type ImageRef struct {
	ID        string
	MediaType string // image/png|jpeg|webp|gif
	Bytes     int64
	Width     int
	Height    int
	Name      string
	Path      string
}

// ContentBlock is one tagged content part of a Message.
type ContentBlock struct {
	Kind ContentBlockKind
	Text string // BlockText / BlockReasoning

	Image ImageRef // BlockImage (M8-3)

	// Reserved for tool blocks (not used this milestone; ToolCalls travel on
	// the Message layer).
	CallID    string
	Name      string
	Arguments string
	IsError   bool
	Blocks    []ContentBlock // nested tool-result

	// Raw preserves a merge-extensible ContentBlockMap entry verbatim. Known
	// constructors leave it nil; JSON decoding sets it only for unknown types.
	Raw json.RawMessage
}

// Text builds a plain text block.
func Text(s string) ContentBlock {
	return ContentBlock{Kind: BlockText, Text: s}
}

type wireImageRef struct {
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	Bytes        int64  `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Name         string `json:"name,omitempty"`
}

type wireContentBlock struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Attachment *wireImageRef  `json:"attachment,omitempty"`
	ID         string         `json:"id,omitempty"`
	Name       string         `json:"name,omitempty"`
	Arguments  string         `json:"arguments,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Content    []ContentBlock `json:"content,omitempty"`
	IsError    bool           `json:"isError,omitempty"`
}

func wireImage(ref ImageRef) *wireImageRef {
	return &wireImageRef{
		AttachmentID: ref.ID, MediaType: ref.MediaType, Bytes: ref.Bytes,
		Width: ref.Width, Height: ref.Height, Name: ref.Name,
	}
}

// MarshalJSON uses the reference ContentBlock field vocabulary. Runtime-only
// ImageRef.Path is deliberately absent.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) != 0 {
		return b.Raw, nil
	}
	wire := wireContentBlock{
		Type: string(b.Kind), Text: b.Text, ID: b.CallID, Name: b.Name,
		Arguments: b.Arguments, ToolCallID: b.CallID, Content: b.Blocks, IsError: b.IsError,
	}
	if b.Kind == BlockImage {
		wire.Attachment = wireImage(b.Image)
	}
	if b.Kind != BlockToolResult {
		wire.ToolCallID = ""
	}
	return json.Marshal(wire)
}

// UnmarshalJSON accepts known reference blocks and preserves unknown
// merge-extensible entries in Raw for a capable provider/plugin adapter.
func (b *ContentBlock) UnmarshalJSON(raw []byte) error {
	var wire wireContentBlock
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	b.Kind = ContentBlockKind(wire.Type)
	b.Text = wire.Text
	b.CallID = wire.ID
	b.Name = wire.Name
	b.Arguments = wire.Arguments
	b.IsError = wire.IsError
	b.Blocks = wire.Content
	if wire.Attachment != nil {
		b.Image = ImageRef{
			ID: wire.Attachment.AttachmentID, MediaType: wire.Attachment.MediaType,
			Bytes: wire.Attachment.Bytes, Width: wire.Attachment.Width, Height: wire.Attachment.Height,
			Name: wire.Attachment.Name,
		}
	}
	switch b.Kind {
	case BlockText, BlockReasoning, BlockImage, BlockToolCall, BlockToolResult:
		if b.Kind == BlockToolResult {
			b.CallID = wire.ToolCallID
		}
		return nil
	default:
		b.Raw = append(json.RawMessage(nil), raw...)
		return nil
	}
}
