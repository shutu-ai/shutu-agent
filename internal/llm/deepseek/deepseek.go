// Package deepseek implements the llm.LLM adapter for the DeepSeek chat
// completions API (OpenAI-compatible, SSE streaming). Design.md §6: the
// default provider, base_url=https://api.deepseek.com; streaming is a
// first-class requirement (D6).
package deepseek

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

const (
	defaultBaseURL = "https://api.deepseek.com"
	defaultModel   = "deepseek-v4-flash"
	// defaultMaxTokens is DSH's DeepSeek connection default. It is a request
	// default, not the model's maximum output capacity.
	defaultMaxTokens  = 256000
	defaultMaxRetries = 2
	// defaultBackoffBase is the first backoff delay; each later attempt doubles
	// it, capped at maxBackoff.
	defaultBackoffBase       = 500 * time.Millisecond
	maxBackoff               = 8 * time.Second
	defaultStreamIdleTimeout = 5 * time.Minute

	// defaultMaxRequestImageBytes is the per-request image byte budget applied
	// by New when Config.MaxRequestImageBytes is non-positive (dispatch-m8-3b
	// §4.1: 默认 20MiB). Over-budget images are offloaded (oldest replaced by
	// the placeholder) inside Stream.
	defaultMaxRequestImageBytes = 20 * 1024 * 1024 // 20 MiB

	// providerID is the stable provider id of the deepseek adapter (M8-2,
	// dispatch-m8-2 §3).
	providerID = "deepseek-official"
)

// Config configures the DeepSeek adapter. APIKey must come from the
// environment (design.md §6: keys never enter code, config, or logs).
type Config struct {
	// ProviderName is the diagnostic label used in typed failures. It does not
	// change the stable DeepSeek adapter ID; OpenAI-compatible wrappers use it
	// to keep their errors provider-accurate.
	ProviderName            string
	BaseURL                 string
	APIKey                  string
	CredentialProvider      llm.CredentialProvider
	CredentialLeaseProvider llm.CredentialLeaseProvider
	// UserID is an optional stable anonymous identity for the official
	// DeepSeek route. UserIDProvider is preferred by the composition root so
	// the identity file remains lazy; it is called only when a request is sent.
	UserID         string
	UserIDProvider func(context.Context) (string, error)
	Model          string
	// DefaultMaxTokens is the provider connection's output default. Non-zero
	// model catalog values override it for an exact configured model.
	DefaultMaxTokens int
	// Thinking is an optional deployment policy ("enabled" or "disabled").
	// An explicit request effort resolves to the corresponding wire mode.
	Thinking               string
	DefaultReasoningEffort string
	// StreamIdleTimeout bounds one silent interval while reading an SSE
	// response. Non-positive uses the dsh-compatible five-minute default.
	StreamIdleTimeout time.Duration
	// ModelCatalog is provider-owned metadata supplied by the composition
	// root. Empty keeps the standalone adapter's dynamic-model behavior.
	ModelCatalog []llm.ModelInfo
	HTTPClient   *http.Client // optional; defaults to http.DefaultClient
	// MaxRetries is how many times a transient failure (network error, HTTP
	// 429, or HTTP 5xx) is retried with backoff before returning the error.
	// Zero (the default) uses 2. 4xx errors, including auth failures, are
	// never retried (dispatch-m2 §5).
	MaxRetries int
	// Backoff returns the delay before retry attempt n (1-based). Nil uses an
	// exponential schedule (500ms, 1s, 2s, ... capped at 8s).
	Backoff func(attempt int) time.Duration
	// DisableRetry lets the composition root delegate retries to the shared
	// provider-neutral wrapper. Direct clients keep the historical default.
	DisableRetry bool
	// SupportsImages is the model's input-modality capability declaration,
	// passed from config llm.model_input_modalities by the composition root
	// (dispatch-m8-3b §4.1). false (the default) means a request carrying an
	// image fails closed inside Stream — the image is never silently dropped.
	SupportsImages bool
	// MaxRequestImageBytes is the per-request image byte budget
	// (dispatch-m8-3b §4.1): images whose cumulative bytes exceed it are
	// offloaded inside Stream (oldest replaced by the OffloadedImageText
	// placeholder). Non-positive uses the default 20MiB.
	MaxRequestImageBytes int
}

// Client is a DeepSeek LLM adapter.
type Client struct {
	providerName            string
	baseURL                 string
	apiKey                  string
	apiKeyMu                sync.RWMutex
	credentialProvider      llm.CredentialProvider
	credentialLeaseProvider llm.CredentialLeaseProvider
	userID                  string
	userIDProvider          func(context.Context) (string, error)
	model                   string
	defaultMaxTokens        int
	thinking                string
	defaultReasoningEffort  string
	streamIdleTimeout       time.Duration
	client                  *http.Client
	maxRetries              int
	backoff                 func(attempt int) time.Duration
	disableRetry            bool

	supportsImages       bool
	maxRequestImageBytes int
	modelCatalog         []llm.ModelInfo
}

// New returns a Client with defaults applied (base URL, model, retries).
func New(cfg Config) *Client {
	if strings.TrimSpace(cfg.ProviderName) == "" {
		cfg.ProviderName = "deepseek"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = defaultMaxTokens
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	backoff := cfg.Backoff
	if backoff == nil {
		backoff = exponentialBackoff
	}
	if cfg.MaxRequestImageBytes <= 0 {
		cfg.MaxRequestImageBytes = defaultMaxRequestImageBytes
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = defaultStreamIdleTimeout
	}
	return &Client{
		providerName:            cfg.ProviderName,
		baseURL:                 strings.TrimSuffix(cfg.BaseURL, "/"),
		apiKey:                  cfg.APIKey,
		credentialProvider:      cfg.CredentialProvider,
		credentialLeaseProvider: cfg.CredentialLeaseProvider,
		userID:                  strings.TrimSpace(cfg.UserID),
		userIDProvider:          cfg.UserIDProvider,
		model:                   cfg.Model,
		defaultMaxTokens:        cfg.DefaultMaxTokens,
		thinking:                cfg.Thinking,
		defaultReasoningEffort:  cfg.DefaultReasoningEffort,
		streamIdleTimeout:       cfg.StreamIdleTimeout,
		client:                  cfg.HTTPClient,
		maxRetries:              cfg.MaxRetries,
		backoff:                 backoff,
		disableRetry:            cfg.DisableRetry,
		supportsImages:          cfg.SupportsImages,
		maxRequestImageBytes:    cfg.MaxRequestImageBytes,
		modelCatalog:            llm.CopyModelCatalog(cfg.ModelCatalog),
	}
}

// ListModels returns the exact catalog owned by this provider generation.
// Standalone clients may intentionally leave it empty for an endpoint whose
// model directory is dynamic; that is distinct from inventing model facts.
func (c *Client) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return llm.CopyModelCatalog(c.modelCatalog), nil
}

// ResolveModelInfo preserves free-form DeepSeek route selection while
// returning owned metadata when the exact model is catalogued.
func (c *Client) ResolveModelInfo(_ context.Context, model string) (llm.ModelInfo, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return llm.ModelInfo{}, fmt.Errorf("%w: model id is required", llm.ErrModelUnavailable)
	}
	for _, info := range c.modelCatalog {
		if info.ID == model {
			if info.DefaultMaxTokens <= 0 {
				info.DefaultMaxTokens = c.defaultMaxTokens
			}
			return llm.CopyModelInfo(info), nil
		}
	}
	return llm.ModelInfo{
		Provider: providerID, ID: model, Name: model,
		Input: []string{"text"}, DefaultMaxTokens: c.defaultMaxTokens,
	}, nil
}

func (c *Client) label() string {
	if c.providerName != "" {
		return c.providerName
	}
	return "deepseek"
}

// ID returns the stable provider id "deepseek-official" (M8-2, dispatch-m8-2 §3).
func (c *Client) ID() string { return providerID }

func (c *Client) SupportsImages() bool { return c.supportsImages }

// Close wipes the provider-owned credential. A retired provider generation
// must not keep an API key alive merely because a loop still holds an old
// interface value.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.apiKeyMu.Lock()
	c.apiKey = ""
	c.credentialProvider = nil
	c.credentialLeaseProvider = nil
	c.apiKeyMu.Unlock()
	return nil
}

func (c *Client) keySnapshotWithContext(ctx context.Context) (string, error) {
	c.apiKeyMu.RLock()
	provider := c.credentialProvider
	key := c.apiKey
	c.apiKeyMu.RUnlock()
	if provider != nil {
		return provider(ctx)
	}
	return key, nil
}

func (c *Client) keyLeaseWithContext(ctx context.Context) (string, llm.CredentialLease, error) {
	c.apiKeyMu.RLock()
	leaseProvider := c.credentialLeaseProvider
	provider := c.credentialProvider
	key := c.apiKey
	c.apiKeyMu.RUnlock()
	if leaseProvider != nil {
		lease, err := leaseProvider(ctx)
		if err != nil {
			return "", nil, err
		}
		if lease == nil {
			return "", nil, nil
		}
		value := lease.Value()
		if value == "" {
			lease.Release()
			return "", nil, nil
		}
		return value, lease, nil
	}
	if provider != nil {
		value, err := provider(ctx)
		return value, nil, err
	}
	return key, nil, nil
}

func (c *Client) keySnapshot() string {
	key, _ := c.keySnapshotWithContext(context.Background())
	return key
}

// Available reports whether the client is usable: a cheap local check that
// never performs a network call — apiKey present and baseURL parseable (same
// shape as web.DeepSeekSearchProvider.Available, dispatch-m8-2 §3). baseURL is
// never empty after New (it defaults to api.deepseek.com), so this is purely
// the key-present + URL-parseable check.
func (c *Client) Available() bool {
	key, err := c.keySnapshotWithContext(context.Background())
	if err != nil || key == "" {
		return false
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return true
}

// wire message/tool shapes for the OpenAI-compatible request body.

type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`           // string (text-only) or []any parts (images, M8-3b)
	ReasoningContent string         `json:"reasoning_content,omitempty"` // assistant reasoning (OpenAI-compatible field, M8)
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type thinkingWire struct {
	Type string `json:"type"`
}

type chatBody struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Tools         []wireTool    `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Thinking        *thinkingWire `json:"thinking,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	Stop            []string      `json:"stop,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"` // dsh 思考强度: "low"|"high"|"max"; absent keeps provider default
}

// toWireMessage serializes one chat message. Content follows the M8-3b
// contract (dispatch-m8-3b §4.1): a message without images keeps the
// single-string content (existing wire and tests unchanged); a message with
// image blocks becomes a parts array — a leading text part (the concatenated
// text blocks, only when non-empty) followed by one image_url part per image
// block, reading each image's bytes as a data URL at request time. Nested
// image blocks (tool results) are included too, so no in-budget image is
// silently dropped. Reading an image can fail → error (fail-closed,
// dispatch-m8-3b §3: 读图失败不静默丢图).
func toWireMessage(m llm.Message) (wireMessage, error) {
	parts, err := imageURLParts(m.Content)
	if err != nil {
		return wireMessage{}, err
	}
	return toWireMessageWithParts(m, parts), nil
}

func toWireMessageWithParts(m llm.Message, parts []any) wireMessage {
	w := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
	if m.Role == llm.RoleAssistant {
		// Reasoning travels on the OpenAI-compatible reasoning_content field
		// (dsh llm-deepseek same); only assistant messages carry it.
		w.ReasoningContent = m.Reasoning()
	}
	for _, tc := range m.ToolCalls {
		wtc := wireToolCall{ID: tc.ID, Type: "function"}
		wtc.Function.Name = tc.Name
		wtc.Function.Arguments = tc.Arguments
		w.ToolCalls = append(w.ToolCalls, wtc)
	}

	if len(parts) == 0 {
		// No image blocks: the plain-text string wire, exactly as before M8-3.
		w.Content = m.Text()
		return w
	}
	out := make([]any, 0, len(parts)+1)
	if text := m.Text(); text != "" {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	out = append(out, parts...)
	w.Content = out
	return w
}

const toolResultImageMarker = "Attached image(s) from tool result:"

// toWireMessages keeps tool results textual. DeepSeek accepts image parts on
// user messages, not on role=tool messages; tool-result images are therefore
// projected to one following user multimodal message, matching the reference
// adapter's history serializer.
func toWireMessages(messages []llm.Message) ([]wireMessage, error) {
	var out []wireMessage
	var pendingToolImages []any
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		content := make([]any, 0, len(pendingToolImages)+1)
		content = append(content, map[string]any{"type": "text", "text": toolResultImageMarker})
		content = append(content, pendingToolImages...)
		out = append(out, wireMessage{Role: string(llm.RoleUser), Content: content})
		pendingToolImages = nil
	}
	for _, m := range messages {
		parts, err := imageURLParts(m.Content)
		if err != nil {
			return nil, err
		}
		message := toWireMessageWithParts(m, parts)
		if m.Role != llm.RoleTool || len(parts) == 0 {
			flushToolImages()
			out = append(out, message)
			continue
		}
		text := m.Text()
		if text == "" {
			text = "(see attached image)"
		}
		message.Content = text
		out = append(out, message)
		pendingToolImages = append(pendingToolImages, parts...)
	}
	flushToolImages()
	return out, nil
}

// imageURLParts returns one image_url part per BlockImage found in blocks, in
// content order, recursing into nested tool-result blocks. An empty result
// means the message carries no image.
func imageURLParts(blocks []llm.ContentBlock) ([]any, error) {
	var parts []any
	for _, b := range blocks {
		if b.Kind == llm.BlockImage {
			dataURL, err := imageDataURL(b.Image)
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": dataURL},
			})
			continue
		}
		if len(b.Blocks) > 0 {
			nested, err := imageURLParts(b.Blocks)
			if err != nil {
				return nil, err
			}
			parts = append(parts, nested...)
		}
	}
	return parts, nil
}

// imageDataURL reads the image bytes at ref.Path and encodes them as a data
// URL — data:<mediaType>;base64,<base64(bytes)> (dispatch-m8-3b §4.1). The
// bytes are read only at request-serialization time and are never logged or
// kept in memory (dsh 7078918 范式). A read failure returns an error
// (fail-closed: an image is never silently dropped). llm does not depend on
// attachment — the provider reads the file directly from the ImageRef path
// (M8-3a 单向依赖纪律).
func imageDataURL(ref llm.ImageRef) (string, error) {
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return "", fmt.Errorf("deepseek: read image %s: %w", ref.Path, err)
	}
	return "data:" + ref.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func toWireTool(t llm.ToolSchema) wireTool {
	wt := wireTool{Type: "function"}
	wt.Function.Name = t.Name
	wt.Function.Description = t.Description
	wt.Function.Parameters = t.Parameters
	return wt
}

// Stream starts a streaming chat request and returns an incremental reader.
// Transient failures (network errors, HTTP 429, HTTP 5xx) are retried with
// backoff up to maxRetries; the context is honored both by the HTTP request
// and between attempts. 4xx errors are returned immediately (dispatch-m2 §5).
func (c *Client) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if err := llm.ValidateRequestBlocks(c.ID(), req.Messages); err != nil {
		return nil, err
	}
	apiKey, credentialLease, err := c.keyLeaseWithContext(ctx)
	if err != nil {
		return nil, llm.NewFailureError("deepseek: credential resolution failed", "CREDENTIAL_UNAVAILABLE", err)
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && credentialLease != nil {
			credentialLease.Release()
		}
	}()
	if apiKey == "" {
		return nil, llm.NewFailureError("deepseek: credential unavailable", "CREDENTIAL_UNAVAILABLE", nil)
	}
	userID, err := c.requestUserID(ctx)
	if err != nil {
		return nil, llm.NewFailureError("deepseek: anonymous identity resolution failed", "IDENTITY_UNAVAILABLE", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.model
	}
	supportsImages := llm.ModelSupportsImages(c.supportsImages, c.modelCatalog, model)
	// A supplied provider catalog owns exact model modality facts. Keep the
	// standalone adapter's global capability fallback for unlisted dynamic
	// routes, but never let a known explicit negative be overridden by it.
	if !supportsImages {
		for _, m := range req.Messages {
			if m.HasImage() {
				return nil, fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", c.ID())
			}
		}
	}
	// M8-3b: apply the request image-byte budget (oldest images first) only
	// after the exact capability gate above, so offloading cannot mask an
	// unsupported image request.
	msgs := llm.OffloadRequestImages(req.Messages, c.maxRequestImageBytes)
	if err := ctx.Err(); err != nil {
		return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.defaultMaxTokens
		if catalogDefault := llm.ModelDefaultMaxTokens(c.modelCatalog, model); catalogDefault > 0 {
			maxTokens = catalogDefault
		}
	}
	body := chatBody{Model: model, Stream: true, MaxTokens: maxTokens, Temperature: req.Temperature, Stop: append([]string(nil), req.Stop...)}
	body.StreamOptions.IncludeUsage = true
	// dsh 思考强度 (ModelSelect effort): "off" 明确关掉思考 (请求不带
	// reasoning_effort 字段, 与 wire 契约一致); 其它合法档位透传。
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = strings.TrimSpace(c.defaultReasoningEffort)
	}
	wireEffort, err := llm.ResolveReasoningEffortWire(c.ID(), c.modelCatalog, model, effort)
	if err != nil {
		return nil, err
	}
	switch effort {
	case "":
		switch c.thinking {
		case "enabled":
			body.Thinking = &thinkingWire{Type: "enabled"}
		case "disabled":
			body.Thinking = &thinkingWire{Type: "disabled"}
		}
	case "off":
		body.Thinking = &thinkingWire{Type: "disabled"}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		if c.thinking == "disabled" {
			return nil, llm.NewFailureError("deepseek: reasoning effort is disabled for this deployment", "UNSUPPORTED_REASONING_EFFORT", nil)
		}
		body.Thinking = &thinkingWire{Type: "enabled"}
		body.ReasoningEffort = wireEffort
	default:
		return nil, llm.NewFailureError("deepseek: unsupported reasoning effort "+effort, "UNSUPPORTED_REASONING_EFFORT", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
	}
	wireMessages, err := toWireMessages(msgs)
	if err != nil {
		return nil, err
	}
	body.Messages = append(body.Messages, wireMessages...)
	for _, t := range req.Tools {
		if err := ctx.Err(); err != nil {
			return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
		}
		body.Tools = append(body.Tools, toWireTool(t))
	}
	if err := ctx.Err(); err != nil {
		return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, llm.NewFailureError("deepseek: cancelled: "+err.Error(), "ABORTED", err)
		}
		reader, retryable, err := c.streamOnce(ctx, payload, apiKey, userID, req.SessionID, req.Purpose)
		if err == nil {
			if stream, ok := reader.(*streamReader); ok {
				stream.credentialLease = credentialLease
				leaseTransferred = credentialLease != nil
			} else if credentialLease != nil {
				credentialLease.Release()
				leaseTransferred = true
			}
			return reader, nil
		}
		if !retryable || c.disableRetry || attempt >= c.maxRetries {
			return nil, err
		}
		if err := sleepCtx(ctx, c.backoff(attempt+1)); err != nil {
			return nil, llm.NewFailureError("deepseek: retry aborted: "+err.Error(), "ABORTED", err)
		}
	}
}

// streamOnce performs a single HTTP attempt. On success it returns the
// streamReader. On failure it returns (nil, retryable, err): retryable is true
// for network failures, HTTP 429, and HTTP 5xx; everything else (4xx) is not.
func (c *Client) streamOnce(ctx context.Context, payload []byte, apiKey, userID, sessionID, purpose string) (llm.StreamReader, bool, error) {
	requestCtx := ctx
	cancel := func() {}
	if c.streamIdleTimeout > 0 {
		requestCtx, cancel = context.WithCancel(ctx)
	}
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, false, fmt.Errorf("deepseek: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	llm.ApplyAttributionHeaders(httpReq.Header)
	if c.isOfficialRoute() {
		if userID != "" {
			httpReq.Header.Set("x-shutu-user-id", userID)
		}
		if sessionID != "" {
			httpReq.Header.Set("x-shutu-session-id", sessionID)
		}
		if strings.EqualFold(strings.TrimSpace(purpose), "compaction") {
			httpReq.Header.Set("x-shutu-compact", "1")
		}
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, false, llm.NewFailureError("deepseek: request aborted: "+ctx.Err().Error(), "ABORTED", ctx.Err())
		}
		// Network-level failure: retryable.
		return nil, true, llm.NewFailureError("deepseek: request failed: "+err.Error(), "TRANSPORT", err)
	}
	if resp.StatusCode == http.StatusOK {
		resp.Body = newIdleBody(resp.Body, requestCtx, cancel, c.streamIdleTimeout)
		return &streamReader{
			dec:       newSSEDecoder(resp.Body),
			resp:      resp,
			ctx:       requestCtx,
			provider:  c.label(),
			toolIndex: map[int]int{},
		}, false, nil
	}
	defer func() {
		_ = resp.Body.Close()
		cancel()
	}()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	err = llm.ClassifyHTTPFailureWithMetadata(c.label(), resp.StatusCode, resp.Status, strings.TrimSpace(string(b)),
		llm.RetryAfterMilliseconds(resp.Header.Get("Retry-After"), time.Now()),
		firstHeader(resp.Header.Get("x-request-id"), resp.Header.Get("x-deepseek-request-id")))
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	return nil, retryable, err
}

func (c *Client) requestUserID(ctx context.Context) (string, error) {
	if c == nil || !c.isOfficialRoute() {
		return "", nil
	}
	if c.userIDProvider != nil {
		id, err := c.userIDProvider(ctx)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(id), nil
	}
	return c.userID, nil
}

func (c *Client) isOfficialRoute() bool {
	if c == nil {
		return false
	}
	switch strings.TrimSpace(c.providerName) {
	case "deepseek", "deepseek-official":
		return true
	default:
		return false
	}
}

func firstHeader(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// classifyHTTPFailure maps the provider's unstable HTTP/body vocabulary onto
// the stable failure taxonomy consumed by the loop and durable projections.
// The body is bounded by streamOnce and any configured secret is redacted by
// callers before it can become a diagnostic.
func classifyHTTPFailure(status int, statusText, body string) error {
	return llm.ClassifyHTTPFailure("deepseek", status, statusText, body)
}

// sleepCtx waits d, aborting early when ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// exponentialBackoff returns 500ms * 2^(attempt-1), capped at maxBackoff.
func exponentialBackoff(attempt int) time.Duration {
	d := defaultBackoffBase << (attempt - 1)
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}
