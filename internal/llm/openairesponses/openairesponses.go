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
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

const (
	// defaultBaseURL is the OpenAI API base URL; "/responses" is appended.
	defaultBaseURL = "https://api.openai.com/v1"
	// defaultModel is the default model when Config.Model is empty.
	defaultModel = "gpt-4o-mini"
	// defaultMaxOutputTokens is the route-level request default used by the
	// reference pi-ai adapter. A model's MaxTokens is capacity metadata and is
	// not promoted into a request budget unless DefaultMaxTokens is explicit.
	defaultMaxOutputTokens = 32768
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
	ID                      string
	BaseURL                 string // default https://api.openai.com/v1 ("/responses" appended)
	APIKey                  string // OPENAI_API_KEY value; empty means absent
	CredentialProvider      llm.CredentialProvider
	CredentialLeaseProvider llm.CredentialLeaseProvider
	Model                   string // default gpt-4o-mini
	ModelCatalog            []llm.ModelInfo
	// MaxOutputTokens is the route-level response token budget; <= 0 uses the
	// reference default 32768.
	MaxOutputTokens int
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
	id                      string
	baseURL                 string
	apiKey                  string
	apiKeyMu                sync.RWMutex
	credentialProvider      llm.CredentialProvider
	credentialLeaseProvider llm.CredentialLeaseProvider
	model                   string
	client                  *http.Client
	supportsImages          bool
	maxRequestImageBytes    int
	modelCatalog            []llm.ModelInfo
	maxOutputTokens         int
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
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = defaultMaxOutputTokens
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.MaxRequestImageBytes <= 0 {
		cfg.MaxRequestImageBytes = defaultMaxRequestImageBytes
	}
	return &openaiResponsesProvider{
		id:                      cfg.ID,
		baseURL:                 strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:                  cfg.APIKey,
		credentialProvider:      cfg.CredentialProvider,
		credentialLeaseProvider: cfg.CredentialLeaseProvider,
		model:                   cfg.Model,
		client:                  cfg.HTTPClient,
		supportsImages:          cfg.SupportsImages,
		maxRequestImageBytes:    cfg.MaxRequestImageBytes,
		modelCatalog:            llm.CopyModelCatalog(cfg.ModelCatalog),
		maxOutputTokens:         cfg.MaxOutputTokens,
	}
}

// ID returns the stable provider id.
func (p *openaiResponsesProvider) ID() string { return p.id }

func (p *openaiResponsesProvider) SupportsImages() bool { return p.supportsImages }

func (p *openaiResponsesProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return llm.CopyModelCatalog(p.modelCatalog), nil
}

func (p *openaiResponsesProvider) ResolveModelInfo(_ context.Context, model string) (llm.ModelInfo, error) {
	info, err := llm.ResolveModelFromCatalog(p.id, p.modelCatalog, model)
	if err != nil {
		return llm.ModelInfo{}, err
	}
	info.DefaultMaxTokens = llm.ModelDefaultMaxTokens(p.modelCatalog, model)
	return info, nil
}

func (p *openaiResponsesProvider) Close() error {
	if p == nil {
		return nil
	}
	p.apiKeyMu.Lock()
	p.apiKey = ""
	p.credentialProvider = nil
	p.credentialLeaseProvider = nil
	p.apiKeyMu.Unlock()
	return nil
}

func (p *openaiResponsesProvider) keySnapshot(ctx context.Context) (string, error) {
	p.apiKeyMu.RLock()
	provider := p.credentialProvider
	key := p.apiKey
	p.apiKeyMu.RUnlock()
	if provider != nil {
		return provider(ctx)
	}
	return key, nil
}

func (p *openaiResponsesProvider) keyLease(ctx context.Context) (string, llm.CredentialLease, error) {
	p.apiKeyMu.RLock()
	leaseProvider := p.credentialLeaseProvider
	provider := p.credentialProvider
	key := p.apiKey
	p.apiKeyMu.RUnlock()
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

// Available reports whether the provider can be used: a cheap local check that
// never performs a network call — apiKey present and base URL parseable.
func (p *openaiResponsesProvider) Available() bool {
	key, err := p.keySnapshot(context.Background())
	if err != nil || key == "" {
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
	if err := llm.ValidateRequestBlocks(p.ID(), req.Messages); err != nil {
		return nil, err
	}
	apiKey, credentialLease, err := p.keyLease(ctx)
	if err != nil {
		return nil, llm.NewFailureError("openairesponses: credential resolution failed", "CREDENTIAL_UNAVAILABLE", err)
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && credentialLease != nil {
			credentialLease.Release()
		}
	}()
	if apiKey == "" {
		return nil, llm.NewFailureError("openairesponses: credential unavailable", "CREDENTIAL_UNAVAILABLE", nil)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.model
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = llm.ModelDefaultReasoningEffort(p.modelCatalog, model)
	}
	wireEffort, err := llm.ResolveReasoningEffortWire(p.ID(), p.modelCatalog, model, effort)
	if err != nil {
		return nil, err
	}
	if !llm.ModelSupportsImages(p.supportsImages, p.modelCatalog, model) {
		for _, m := range req.Messages {
			if m.HasImage() {
				return nil, fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", p.ID())
			}
		}
	}
	msgs := llm.OffloadRequestImages(req.Messages, p.maxRequestImageBytes)

	maxOutputTokens := p.maxOutputTokens
	if catalogDefault := llm.ModelDefaultMaxTokens(p.modelCatalog, model); catalogDefault > 0 {
		maxOutputTokens = catalogDefault
	}
	if req.MaxTokens > 0 {
		maxOutputTokens = req.MaxTokens
	}
	body := requestBody{
		Model:           model,
		Input:           toInput(msgs, p.maxRequestImageBytes),
		Tools:           toTools(req.Tools),
		Stream:          true,
		MaxOutputTokens: maxOutputTokens,
		Temperature:     req.Temperature,
		Stop:            append([]string(nil), req.Stop...),
	}
	reasoningSupported, reasoningKnown := llm.ModelReasoningCapability(p.modelCatalog, model)
	reasoningEffort := wireEffort
	if strings.EqualFold(effort, "off") && reasoningEffort == "" {
		reasoningEffort = "off"
	}
	if reasoning := reasoningForEffort(reasoningEffort, reasoningKnown && reasoningSupported); reasoning != nil {
		body.Reasoning = reasoning
		body.Include = []string{"reasoning.encrypted_content"}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openairesponses: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openairesponses: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	llm.ApplyAttributionHeaders(httpReq.Header)

	resp, err := p.doNoRedirect(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		reader := newStreamReader(resp)
		reader.credentialLease = credentialLease
		leaseTransferred = credentialLease != nil
		return reader, nil
	}
	defer resp.Body.Close()
	detail := errorDetail(resp)
	if detail == "" {
		detail = resp.Status
	}
	return nil, llm.ClassifyHTTPFailureWithMetadata("openairesponses", resp.StatusCode, resp.Status, detail,
		llm.RetryAfterMilliseconds(resp.Header.Get("Retry-After"), time.Now()), resp.Header.Get("x-request-id"))
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
			return nil, llm.NewFailureError("openairesponses: redirect blocked (3xx not followed)", "TRANSPORT", err)
		}
		if req.Context().Err() != nil {
			return nil, llm.NewFailureError("openairesponses: cancelled: "+req.Context().Err().Error(), "ABORTED", req.Context().Err())
		}
		return nil, llm.NewFailureError("openairesponses: request failed: "+err.Error(), "TRANSPORT", err)
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
	Model           string           `json:"model"`
	Input           []map[string]any `json:"input"`
	Tools           []wireTool       `json:"tools,omitempty"`
	Stream          bool             `json:"stream"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	Stop            []string         `json:"stop,omitempty"`
	Reasoning       *reasoningConfig `json:"reasoning,omitempty"`
	Include         []string         `json:"include,omitempty"`
}

type reasoningConfig struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

func reasoningForEffort(effort string, reasoningKnownTrue bool) *reasoningConfig {
	switch strings.TrimSpace(effort) {
	case "off":
		if !reasoningKnownTrue {
			return nil
		}
		return &reasoningConfig{Effort: "none"}
	case "low", "high", "max":
		return &reasoningConfig{Effort: effort, Summary: "auto"}
	default:
		return nil
	}
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
