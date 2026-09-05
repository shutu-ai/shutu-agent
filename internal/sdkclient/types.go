package sdkclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/projection"
)

// ImageAttachmentRef is the reference SDK's durable image metadata shape.
type ImageAttachmentRef struct {
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	Bytes        int64  `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Name         string `json:"name,omitempty"`
}

// ContentBlock mirrors the reference LLM ContentBlock union on the SDK wire.
type ContentBlock struct {
	Type       string              `json:"type"`
	Text       string              `json:"text,omitempty"`
	Attachment *ImageAttachmentRef `json:"attachment,omitempty"`
	ID         string              `json:"id,omitempty"`
	Name       string              `json:"name,omitempty"`
	Arguments  string              `json:"arguments,omitempty"`
	ToolCallID string              `json:"toolCallId,omitempty"`
	Content    []ContentBlock      `json:"content,omitempty"`
	IsError    *bool               `json:"isError,omitempty"`

	// Data/MimeType are a Shutu compatibility extension for clients that upload
	// canonical base64 bytes directly. The reference image shape is Attachment.
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`

	// Raw preserves an unknown merge-extensible ContentBlockMap entry.
	Raw json.RawMessage `json:"-"`
}

// TextContent constructs one plain text input block.
func TextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ReasoningContent constructs a reasoning block.
func ReasoningContent(text string) ContentBlock {
	return ContentBlock{Type: "reasoning", Text: text}
}

// ImageAttachmentContent constructs the reference durable image block.
func ImageAttachmentContent(ref ImageAttachmentRef) ContentBlock {
	return ContentBlock{Type: "image", Attachment: &ref}
}

// ToolCallContent constructs a provider-neutral tool invocation block.
func ToolCallContent(id, name, arguments string) ContentBlock {
	return ContentBlock{Type: "tool-call", ID: id, Name: name, Arguments: arguments}
}

// ToolResultContent constructs a provider-neutral tool result block.
func ToolResultContent(toolCallID string, content []ContentBlock, isError bool) ContentBlock {
	return ContentBlock{Type: "tool-result", ToolCallID: toolCallID, Content: content, IsError: &isError}
}

func (b *ContentBlock) UnmarshalJSON(raw []byte) error {
	type plain ContentBlock
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*b = ContentBlock(decoded)
	switch b.Type {
	case "text", "reasoning", "image", "tool-call", "tool-result":
		return nil
	default:
		b.Raw = append(json.RawMessage(nil), raw...)
		return nil
	}
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) != 0 {
		return b.Raw, nil
	}
	type plain ContentBlock
	return json.Marshal(plain(b))
}

// InitializeParams is the process-wide SDK handshake.
type InitializeParams struct {
	CWD       string `json:"cwd"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"maxTokens,omitempty"`
}

// ToolCatalogEntry is the SDK-visible projection of one registered capability.
type ToolCatalogEntry struct {
	Name       string `json:"name"`
	Profile    string `json:"profile"`
	Provenance string `json:"provenance"`
	Generation uint64 `json:"generation"`
	Visible    bool   `json:"visible"`
}

// ToolCatalog carries the optional SDK inventory revision and digest.
type ToolCatalog struct {
	SchemaVersion int                `json:"schemaVersion"`
	Revision      uint64             `json:"revision"`
	Digest        string             `json:"digest"`
	Tools         []ToolCatalogEntry `json:"tools"`
}

// InitializeResult carries the runtime's stable identity.
type InitializeResult struct {
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	// ToolCatalog is an optional shutu runtime extension. Reference clients
	// may ignore it; shutu clients use revision/digest to observe reloads.
	ToolCatalog *ToolCatalog `json:"toolCatalog,omitempty"`
}

// SessionPromptParams queues one user turn.
type SessionPromptParams struct {
	SessionID     string         `json:"sessionId"`
	ContentBlocks []ContentBlock `json:"contentBlocks"`
}

// SessionPromptResult is the durable inbox enqueue receipt.
type SessionPromptResult struct {
	MessageID string `json:"messageId"`
}

// SessionSnapshot is the optional shutu SDK query extension. Its payload is
// the same durable-event projection consumed by the Native and Web adapters;
// it is not a second client-side history authority.
type SessionSnapshot struct {
	SessionID string              `json:"sessionId"`
	Snapshot  projection.Snapshot `json:"snapshot"`
}

// SessionEvent is the durable event envelope streamed by the runtime.
type SessionEvent struct {
	Seq             uint64          `json:"seq"`
	Type            string          `json:"type"`
	At              int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	Ignorable       bool            `json:"ignorable,omitempty"`
	SourceEventSeqs []uint64        `json:"sourceEventSeqs,omitempty"`
	// SurfaceOp is a union in the reference protocol: "append" or an object
	// describing a replacement range. Keep the raw JSON so the SDK does not
	// collapse that distinction or lose future compatible variants.
	SurfaceOp json.RawMessage `json:"surfaceOp,omitempty"`
}

// RunResult is one owned prompt-to-idle activity interval.
type RunResult struct {
	SessionID     string
	FinalResponse string
	Events        []SessionEvent
	Notifications []Notification
}

// HarnessOptions configures the high-level runtime wrapper.
type HarnessOptions struct {
	Launch    ClientOptions
	CWD       string
	Provider  string
	Model     string
	MaxTokens int
}

// Harness lazily owns one runtime and memoizes a successful initialize
// handshake. A failed handshake is reaped and retried on a fresh process.
type Harness struct {
	options HarnessOptions

	mu          sync.Mutex
	client      *Client
	initialized bool
	closed      bool
}

// NewHarness creates the high-level SDK facade.
func NewHarness(options HarnessOptions) *Harness { return &Harness{options: options} }

// Start launches and initializes the runtime. A later call retries a failed
// handshake with a fresh process until Close makes the wrapper terminal.
func (h *Harness) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("SDK harness is closed")
	}
	if h.initialized {
		return nil
	}
	if h.client == nil {
		h.client = NewClient(h.options.Launch)
	}
	if err := h.client.Start(); err != nil {
		// A spawn failure leaves the low-level client permanently failed: it
		// has already crossed its start boundary and recorded the process
		// error. Reap that failed attempt and replace it so Harness.Start
		// retains dsh's fresh-process retry contract.
		_ = h.client.Close()
		h.client = nil
		return err
	}
	cwd := h.options.CWD
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			_ = h.client.Close()
			h.client = nil
			return err
		}
	}
	provider := h.options.Provider
	if provider == "" {
		provider = "deepseek-official"
	}
	model := h.options.Model
	if model == "" {
		model = "deepseek-v4-flash"
	}
	params := InitializeParams{CWD: cwd, Provider: provider, Model: model, MaxTokens: h.options.MaxTokens}
	if _, err := h.client.Initialize(ctx, params); err != nil {
		_ = h.client.Close()
		h.client = nil
		return err
	}
	h.initialized = true
	return nil
}

func (h *Harness) currentClient() (*Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || !h.initialized || h.client == nil {
		return nil, errors.New("SDK harness is not initialized")
	}
	return h.client, nil
}

// Close shuts down and reaps the owned runtime. It is idempotent and terminal.
func (h *Harness) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	client := h.client
	h.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close()
}

// Session returns a named session handle, or mints a fresh opaque id.
func (h *Harness) Session(sessionID string) (*HarnessSession, error) {
	if sessionID == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		sessionID = "sdk-" + hex.EncodeToString(raw[:])
	}
	if sessionID == "" {
		return nil, errors.New("session id must be non-empty")
	}
	return &HarnessSession{Harness: h, ID: sessionID}, nil
}

// Run is shorthand for minting and running one fresh session.
func (h *Harness) Run(ctx context.Context, input string, sessionID string) (RunResult, error) {
	session, err := h.Session(sessionID)
	if err != nil {
		return RunResult{}, err
	}
	return session.Run(ctx, []ContentBlock{TextContent(input)})
}

// HarnessSession owns prompt-to-idle intervals on one runtime session.
type HarnessSession struct {
	Harness *Harness
	ID      string
}

// Run queues input, waits for its durable inbox receipt, and collects the
// whole root session plus discovered descendants through the next idle state.
// Context cancellation abandons only collection; the SDK has no per-prompt
// wire cancellation, matching the reference SDK client contract.
func (s *HarnessSession) Run(ctx context.Context, blocks []ContentBlock) (RunResult, error) {
	if err := s.Harness.Start(ctx); err != nil {
		return RunResult{}, err
	}
	client, err := s.Harness.currentClient()
	if err != nil {
		return RunResult{}, err
	}
	subscription := client.SubscribeSessionTree(s.ID)
	defer subscription.Close()
	messageID, err := client.Prompt(ctx, s.ID, blocks)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{SessionID: s.ID}
	received := false
	for {
		notification, err := subscription.Next(ctx)
		if err != nil {
			return RunResult{}, err
		}
		rootEvent, event := decodeSessionEvent(notification, s.ID)
		if rootEvent && notification.Method == "session.event" && !received {
			if !isInboxReceipt(event, messageID) {
				continue
			}
			received = true
		}
		if !received {
			continue
		}
		result.Notifications = append(result.Notifications, notification)
		if rootEvent && notification.Method == "session.event" {
			result.Events = append(result.Events, event)
		}
		if rootEvent && notification.Method == "session.status" && sessionStatus(notification) == "idle" {
			break
		}
	}
	result.FinalResponse = finalResponse(result.Events)
	return result, nil
}

// Snapshot rebuilds one session's history, trajectory surface, and durable
// control state from the runtime's canonical projection. It is a shutu
// extension; reference-compatible clients may simply ignore the method.
func (s *HarnessSession) Snapshot(ctx context.Context) (SessionSnapshot, error) {
	if err := s.Harness.Start(ctx); err != nil {
		return SessionSnapshot{}, err
	}
	client, err := s.Harness.currentClient()
	if err != nil {
		return SessionSnapshot{}, err
	}
	return client.Snapshot(ctx, s.ID)
}

func decodeSessionEvent(notification Notification, sessionID string) (bool, SessionEvent) {
	if notification.Method != "session.event" {
		return isRootNotification(notification, sessionID), SessionEvent{}
	}
	var params struct {
		SessionID string       `json:"sessionId"`
		Event     SessionEvent `json:"event"`
	}
	if json.Unmarshal(notification.Params, &params) != nil || params.SessionID != sessionID {
		return false, SessionEvent{}
	}
	return true, params.Event
}

func isRootNotification(notification Notification, sessionID string) bool {
	values := notificationString(notification, "sessionId")
	return values["sessionId"] == sessionID
}

func isInboxReceipt(event SessionEvent, messageID string) bool {
	if event.Type != "agent/inbox/spliced" {
		return false
	}
	var data struct {
		Inserted []struct {
			ID string `json:"id"`
		} `json:"inserted"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return false
	}
	for _, message := range data.Inserted {
		if message.ID == messageID {
			return true
		}
	}
	return false
}

func sessionStatus(notification Notification) string {
	var params struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(notification.Params, &params)
	return params.Status
}

func finalResponse(events []SessionEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "assistant/message" {
			continue
		}
		var data struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(events[i].Data, &data) != nil {
			continue
		}
		response := ""
		for _, block := range data.Message.Content {
			if block.Type == "text" {
				response += block.Text
			}
		}
		return response
	}
	return ""
}
