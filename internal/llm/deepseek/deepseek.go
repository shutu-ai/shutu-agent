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
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

const (
	defaultBaseURL    = "https://api.deepseek.com"
	defaultModel      = "deepseek-v4-flash"
	defaultMaxRetries = 2
	// defaultBackoffBase is the first backoff delay; each later attempt doubles
	// it, capped at maxBackoff.
	defaultBackoffBase = 500 * time.Millisecond
	maxBackoff         = 8 * time.Second

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
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client // optional; defaults to http.DefaultClient
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
	baseURL      string
	apiKey       string
	model        string
	client       *http.Client
	maxRetries   int
	backoff      func(attempt int) time.Duration
	disableRetry bool

	supportsImages       bool
	maxRequestImageBytes int
}

// New returns a Client with defaults applied (base URL, model, retries).
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
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
	return &Client{
		baseURL:              strings.TrimSuffix(cfg.BaseURL, "/"),
		apiKey:               cfg.APIKey,
		model:                cfg.Model,
		client:               cfg.HTTPClient,
		maxRetries:           cfg.MaxRetries,
		backoff:              backoff,
		disableRetry:         cfg.DisableRetry,
		supportsImages:       cfg.SupportsImages,
		maxRequestImageBytes: cfg.MaxRequestImageBytes,
	}
}

// ID returns the stable provider id "deepseek-official" (M8-2, dispatch-m8-2 §3).
func (c *Client) ID() string { return providerID }

// Available reports whether the client is usable: a cheap local check that
// never performs a network call — apiKey present and baseURL parseable (same
// shape as web.DeepSeekSearchProvider.Available, dispatch-m8-2 §3). baseURL is
// never empty after New (it defaults to api.deepseek.com), so this is purely
// the key-present + URL-parseable check.
func (c *Client) Available() bool {
	if c.apiKey == "" {
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

type chatBody struct {
	Model           string        `json:"model"`
	Messages        []wireMessage `json:"messages"`
	Tools           []wireTool    `json:"tools,omitempty"`
	Stream          bool          `json:"stream"`
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

	parts, err := imageURLParts(m.Content)
	if err != nil {
		return wireMessage{}, err
	}
	if len(parts) == 0 {
		// No image blocks: the plain-text string wire, exactly as before M8-3.
		w.Content = m.Text()
		return w, nil
	}
	out := make([]any, 0, len(parts)+1)
	if text := m.Text(); text != "" {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	out = append(out, parts...)
	w.Content = out
	return w, nil
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
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("deepseek: cancelled: %w", err)
	}
	// M8-3b image fail-closed check FIRST (dispatch-m8-3b §3): a model that
	// does not declare image input must error on an image request, never
	// silently drop it. The check runs before offload so offloading cannot
	// mask an image.
	if !c.supportsImages {
		for _, m := range req.Messages {
			if m.HasImage() {
				return nil, fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", c.ID())
			}
		}
	}
	// M8-3b: apply the request image-byte budget (over-budget images, oldest
	// first, are replaced by the OffloadedImageText placeholder) before
	// serialization.
	msgs := llm.OffloadRequestImages(req.Messages, c.maxRequestImageBytes)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("deepseek: cancelled: %w", err)
	}

	body := chatBody{Model: c.model, Stream: true}
	// dsh 思考强度 (ModelSelect effort): "off" 明确关掉思考 (请求不带
	// reasoning_effort 字段, 与 wire 契约一致); 其它合法档位透传。
	switch req.ReasoningEffort {
	case "", "off":
		// absent / off → no reasoning_effort field on the wire
	default:
		body.ReasoningEffort = req.ReasoningEffort
	}
	for _, m := range msgs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("deepseek: cancelled: %w", err)
		}
		wm, err := toWireMessage(m)
		if err != nil {
			return nil, err
		}
		body.Messages = append(body.Messages, wm)
	}
	for _, t := range req.Tools {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("deepseek: cancelled: %w", err)
		}
		body.Tools = append(body.Tools, toWireTool(t))
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("deepseek: cancelled: %w", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("deepseek: cancelled: %w", err)
		}
		reader, retryable, err := c.streamOnce(ctx, payload)
		if err == nil {
			return reader, nil
		}
		if !retryable || c.disableRetry || attempt >= c.maxRetries {
			return nil, err
		}
		if err := sleepCtx(ctx, c.backoff(attempt+1)); err != nil {
			return nil, fmt.Errorf("deepseek: retry aborted: %w", err)
		}
	}
}

// streamOnce performs a single HTTP attempt. On success it returns the
// streamReader. On failure it returns (nil, retryable, err): retryable is true
// for network failures, HTTP 429, and HTTP 5xx; everything else (4xx) is not.
func (c *Client) streamOnce(ctx context.Context, payload []byte) (llm.StreamReader, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("deepseek: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		// Network-level failure: retryable.
		return nil, true, fmt.Errorf("deepseek: request failed: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return &streamReader{
			dec:       newSSEDecoder(resp.Body),
			resp:      resp,
			toolIndex: map[int]int{},
		}, false, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	err = fmt.Errorf("deepseek: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	return nil, retryable, err
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
