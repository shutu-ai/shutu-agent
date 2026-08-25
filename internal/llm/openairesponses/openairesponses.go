// Package openairesponses implements the llm.Provider for the OpenAI Responses
// API (pi-ai api "openai-responses"): streaming SSE over POST {base}/responses.
// Wire follows the OpenAI Responses streaming protocol: input items are
// user/developer/system message items (input_text), assistant message items
// (output_text), reasoning items (reasoning_text/summary), function_call and
// function_call_output items; tools are function tools; the SSE event stream
// carries response.output_item.added / response.output_text.delta /
// response.reasoning_text.delta / response.reasoning_summary_text.delta /
// response.function_call_arguments.delta / response.output_item.done /
// response.completed. Reasoning deltas map onto llm.StreamReasoningDelta
// (OpenAI reasoning_content 范式), so reasoning flows through the same
// provider-neutral channel the deepseek adapter uses. There is zero new
// dependency; credentials are env-only (OPENAI_API_KEY, 纪律 6).
package openairesponses

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

	"github.com/jabing/shutu-agent/internal/llm"
)

const (
	// defaultBaseURL is the OpenAI API base URL; "/responses" is appended.
	defaultBaseURL = "https://api.openai.com/v1"
	// defaultModel is the default model when Config.Model is empty.
	defaultModel = "gpt-4o-mini"
	// providerID is the stable provider id.
	providerID = "openai-responses"
	// maxErrorBody bounds the non-2xx error body read (1 MiB).
	maxErrorBody = 1 << 20
	// defaultMaxRequestImageBytes is the per-request image byte budget applied
	// by New when Config.MaxRequestImageBytes is non-positive (默认 20MiB).
	defaultMaxRequestImageBytes = 20 * 1024 * 1024 // 20 MiB
)

// errRedirectDetected turns "follow the redirect" into an error.
var errRedirectDetected = fmt.Errorf("openairesponses: redirect not followed")

// Config configures the OpenAI Responses provider. APIKey must come from the
// environment (OPENAI_API_KEY only, 纪律 6).
type Config struct {
	// ID is the provider's registry id; empty defaults to "openai-responses".
	// A non-empty ID lets the composition root register OpenAI Responses
	// endpoints (openai / xai) under their own route.
	ID      string
	BaseURL string // default https://api.openai.com/v1 ("/responses" appended)
	APIKey  string // OPENAI_API_KEY value; empty means absent
	Model   string // default gpt-4o-mini
	// HTTPClient is optional; defaults to http.DefaultClient. The provider
	// copies it with a no-redirect CheckRedirect, never mutating the caller's
	// shared client.
	HTTPClient *http.Client
	// SupportsImages is the model's input-modality capability declaration.
	// false (the default) means an image request fails closed inside Stream.
	SupportsImages bool
	// MaxRequestImageBytes is the per-request image byte budget; non-positive
	// uses the default 20MiB.
	MaxRequestImageBytes int
}

// openaiResponsesProvider is the llm.Provider implementing the Responses API.
type openaiResponsesProvider struct {
	id                   string
	baseURL              string
	apiKey               string
	model                string
	client               *http.Client
	supportsImages       bool
	maxRequestImageBytes int
}

// New returns an openaiResponsesProvider with defaults applied.
func New(cfg Config) *openaiResponsesProvider {
	if cfg.ID == "" {
		cfg.ID = providerID
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.MaxRequestImageBytes <= 0 {
		cfg.MaxRequestImageBytes = defaultMaxRequestImageBytes
	}
	return &openaiResponsesProvider{
		id:                   cfg.ID,
		baseURL:              strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:               cfg.APIKey,
		model:                cfg.Model,
		client:               cfg.HTTPClient,
		supportsImages:       cfg.SupportsImages,
		maxRequestImageBytes: cfg.MaxRequestImageBytes,
	}
}

// ID returns the stable provider id.
func (p *openaiResponsesProvider) ID() string { return p.id }

// Available reports whether the provider can be used: a cheap local check that
// never performs a network call — apiKey present and base URL parseable.
func (p *openaiResponsesProvider) Available() bool {
	if p.apiKey == "" {
		return false
	}
	u, err := url.Parse(p.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return true
}

// Stream starts a streaming Responses request and returns an incremental
// reader. The request is serialized per the Responses wire, POSTed to
// {base}/responses with the Bearer header, and the SSE event stream is decoded
// into llm.StreamEvents. ctx cancellation runs through the HTTP request and
// the body reads.
func (p *openaiResponsesProvider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if !p.supportsImages {
		for _, m := range req.Messages {
			if m.HasImage() {
				return nil, fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", p.ID())
			}
		}
	}
	msgs := llm.OffloadRequestImages(req.Messages, p.maxRequestImageBytes)

	body := requestBody{
		Model:  p.model,
		Input:  toInput(msgs, p.maxRequestImageBytes),
		Tools:  toTools(req.Tools),
		Stream: true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openairesponses: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openairesponses: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.doNoRedirect(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return newStreamReader(resp), nil
	}
	defer resp.Body.Close()
	detail := errorDetail(resp)
	if detail == "" {
		detail = resp.Status
	}
	return nil, fmt.Errorf("openairesponses: provider error: %s", detail)
}

// doNoRedirect issues httpReq with a no-follow redirect policy.
func (p *openaiResponsesProvider) doNoRedirect(req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Transport: p.client.Transport,
		Jar:       p.client.Jar,
		Timeout:   p.client.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectDetected
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if err == errRedirectDetected {
			return nil, fmt.Errorf("openairesponses: redirect blocked (3xx not followed)")
		}
		if req.Context().Err() != nil {
			return nil, fmt.Errorf("openairesponses: cancelled: %w", req.Context().Err())
		}
		return nil, fmt.Errorf("openairesponses: request failed: %w", err)
	}
	return resp, nil
}

// errorDetail extracts the server-provided message from a non-2xx response
// (bounded 1 MiB read).
func errorDetail(resp *http.Response) string {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return envelope.Message
}

// —— wire shapes for the Responses request body ——

type requestBody struct {
	Model  string           `json:"model"`
	Input  []map[string]any `json:"input"`
	Tools  []wireTool       `json:"tools,omitempty"`
	Stream bool             `json:"stream"`
}

type wireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// toInput serializes a chat history into the Responses "input" items array
// (pi-ai convertResponsesMessages 范式): system → a developer/system message
// item, user → user input_text item(s), assistant → message items (reasoning
// as a reasoning item, text as output_text, tool calls as function_call
// items), tool results → function_call_output items.
func toInput(msgs []llm.Message, maxRequestImageBytes int) []map[string]any {
	var out []map[string]any
	// map tool call id -> item id, to pair function_call_output with the call.
	callItemID := map[string]string{}
	itemIndex := 0
	nextID := func() string {
		itemIndex++
		return fmt.Sprintf("item_%d", itemIndex)
	}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			if t := m.Text(); t != "" {
				out = append(out, map[string]any{
					"role":    "developer",
					"content": []any{map[string]any{"type": "input_text", "text": t}},
				})
			}
		case llm.RoleTool:
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Text(),
			})
		case llm.RoleUser:
			out = append(out, userItem(m, maxRequestImageBytes))
		case llm.RoleAssistant:
			// Reasoning travels as a reasoning item (the OpenAI Responses
			// reasoning block, dsh/pi-ai 范式), before the message item.
			if r := m.Reasoning(); r != "" {
				out = append(out, map[string]any{
					"type":    "reasoning",
					"id":      nextID(),
					"content": []any{map[string]any{"type": "reasoning_text", "text": r}},
				})
			}
			if t := m.Text(); t != "" {
				out = append(out, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"id":      nextID(),
					"content": []any{map[string]any{"type": "output_text", "text": t, "annotations": []any{}}},
				})
			}
			for _, tc := range m.ToolCalls {
				id := nextID()
				callItemID[tc.ID] = id
				out = append(out, map[string]any{
					"type":      "function_call",
					"id":        id,
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": tc.Arguments,
				})
			}
		}
	}
	return out
}

// userItem converts a user message into a Responses user input item (input_text
// blocks; images become input_image data URLs, pi-ai convertResponsesMessages).
func userItem(m llm.Message, maxRequestImageBytes int) map[string]any {
	var content []any
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockText:
			content = append(content, map[string]any{"type": "input_text", "text": b.Text})
		case llm.BlockImage:
			data, err := imageDataURL(b.Image)
			if err != nil {
				content = append(content, map[string]any{"type": "input_text", "text": "(image read failed: " + err.Error() + ")"})
				continue
			}
			content = append(content, map[string]any{"type": "input_image", "detail": "auto", "image_url": data})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "input_text", "text": "(no output)"})
	}
	return map[string]any{"role": "user", "content": content}
}

// toTools converts the tool schemas to Responses function tools.
func toTools(tools []llm.ToolSchema) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// imageDataURL reads the image bytes at ref.Path and encodes them as a data
// URL — data:<mediaType>;base64,<base64(bytes)>. The bytes are read only at
// request-serialization time and are never logged or kept in memory. A read
// failure returns an error (fail-closed).
func imageDataURL(ref llm.ImageRef) (string, error) {
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return "", fmt.Errorf("openairesponses: read image %s: %w", ref.Path, err)
	}
	return "data:" + ref.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
