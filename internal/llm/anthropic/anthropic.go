// Package anthropic implements the llm.Provider for the Anthropic Messages
// API (M8-2b, dispatch-m8-2b §2): streaming SSE with tool use and thinking
// (reasoning) passthrough. Serialization follows dispatch-m8-2b §2.1 (system
// extraction, user/assistant/tool-result blocks, thinking→reasoning, tool_use
// input), and the SSE reader follows §2.2 (content_block_* / message_* /
// error events). The HTTP client semantics (x-api-key + anthropic-version
// headers, redirects blocked, ctx cancellation, bounded error body) reuse the
// M7 internal/web/deepseek.go Anthropic-compatible client. Credentials are
// env-only (ANTHROPIC_API_KEY, 纪律 6): the composition root passes the value
// in so this package stays testable.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

const (
	// defaultBaseURL is the Anthropic Messages API base URL; "/messages" is
	// appended (dispatch-m8-2b §2.1, default https://api.anthropic.com/v1).
	defaultBaseURL = "https://api.anthropic.com/v1"
	// defaultModel is the default model when Config.Model is empty. Kept in
	// sync with config.DefaultAnthropicModel (dispatch-m8-2b §3).
	defaultModel = "claude-sonnet-4-5"
	// defaultMaxTokens is the route-level request default used by the
	// reference pi-ai adapter. It is separate from ModelInfo.MaxTokens, which
	// describes model capacity rather than a per-request budget.
	defaultMaxTokens = 32768
	// apiVersion is the anthropic-version header (dispatch-m8-2b §2.1).
	apiVersion = "2023-06-01"
	// providerID is the stable provider id (dispatch-m8-2b §2.3).
	providerID = "anthropic"
	// maxErrorBody bounds the non-2xx error body read (dispatch-m8-2b §2.3,
	// 1 MiB).
	maxErrorBody = 1 << 20
	// defaultMaxRequestImageBytes is the per-request image byte budget applied
	// by New when Config.MaxRequestImageBytes is non-positive (dispatch-m8-3b
	// §4.2: 默认 20MiB). Over-budget images are offloaded (oldest replaced by
	// the placeholder) inside Stream.
	defaultMaxRequestImageBytes = 20 * 1024 * 1024 // 20 MiB
	// noOutputPlaceholder is emitted for a user message whose content is empty
	// after conversion (Anthropic rejects empty content, dispatch-m8-2b §2.1
	// rule 5, 照 dsh 同款).
	noOutputPlaceholder = "(no output)"
)

// errRedirectDetected is returned by the CheckRedirect callback to turn
// "follow the redirect" into an error: any 3xx is never followed nor read
// (dispatch-m8-2b §2.3, mirroring M7 web/deepseek.go).
var errRedirectDetected = errors.New("anthropic: redirect not followed")

// Config configures the Anthropic provider. APIKey must come from the
// environment (ANTHROPIC_API_KEY only, 纪律 6).
type Config struct {
	// ID is the provider's registry id; empty defaults to "anthropic". A
	// non-empty ID lets the composition root register arbitrary Anthropic
	// Messages-compatible endpoints (M11-pi-ai: minimax / minimax-cn /
	// kimi-coding / vercel-ai-gateway) under their own route.
	ID                      string
	BaseURL                 string // default https://api.anthropic.com/v1 ("/messages" appended)
	APIKey                  string // ANTHROPIC_API_KEY value; empty means absent
	CredentialProvider      llm.CredentialProvider
	CredentialLeaseProvider llm.CredentialLeaseProvider
	Model                   string // default claude-sonnet-4-5
	ModelCatalog            []llm.ModelInfo
	// MaxTokens is the route-level response token budget; <= 0 uses the
	// reference default 32768
	// (advanced max_tokens/temperature/stop knobs are out of scope this
	// milestone, dispatch-m8-2b §1).
	MaxTokens int
	// HTTPClient is optional; defaults to http.DefaultClient. The provider
	// copies it with a no-redirect CheckRedirect, never mutating the caller's
	// shared client.
	HTTPClient *http.Client
	// SupportsImages is the model's input-modality capability declaration,
	// passed from config llm.model_input_modalities by the composition root
	// (dispatch-m8-3b §4.2). false (the default) means an image request fails
	// closed inside Stream — the image is never silently dropped.
	SupportsImages bool
	// MaxRequestImageBytes is the per-request image byte budget
	// (dispatch-m8-3b §4.2): images whose cumulative bytes exceed it are
	// offloaded inside Stream (oldest replaced by the OffloadedImageText
	// placeholder). Non-positive uses the default 20MiB.
	MaxRequestImageBytes int
}

// anthropicProvider is the llm.Provider implementing the Anthropic Messages
// API (dispatch-m8-2b §2.3).
type anthropicProvider struct {
	id                      string
	baseURL                 string
	apiKey                  string
	apiKeyMu                sync.RWMutex
	credentialProvider      llm.CredentialProvider
	credentialLeaseProvider llm.CredentialLeaseProvider
	model                   string
	maxTokens               int
	client                  *http.Client

	supportsImages       bool
	maxRequestImageBytes int
	modelCatalog         []llm.ModelInfo
}

// New returns an anthropicProvider with defaults applied (base URL, model,
// max_tokens).
func New(cfg Config) *anthropicProvider {
	if cfg.ID == "" {
		cfg.ID = providerID
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.MaxRequestImageBytes <= 0 {
		cfg.MaxRequestImageBytes = defaultMaxRequestImageBytes
	}
	return &anthropicProvider{
		id:                      cfg.ID,
		baseURL:                 strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:                  cfg.APIKey,
		credentialProvider:      cfg.CredentialProvider,
		credentialLeaseProvider: cfg.CredentialLeaseProvider,
		model:                   cfg.Model,
		maxTokens:               cfg.MaxTokens,
		client:                  cfg.HTTPClient,
		supportsImages:          cfg.SupportsImages,
		maxRequestImageBytes:    cfg.MaxRequestImageBytes,
		modelCatalog:            llm.CopyModelCatalog(cfg.ModelCatalog),
	}
}

// ID returns the stable provider id ("anthropic" for the built-in adapter, or
// the custom route configured via Config.ID).
func (p *anthropicProvider) ID() string { return p.id }

func (p *anthropicProvider) SupportsImages() bool { return p.supportsImages }

func (p *anthropicProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return llm.CopyModelCatalog(p.modelCatalog), nil
}

func (p *anthropicProvider) ResolveModelInfo(_ context.Context, model string) (llm.ModelInfo, error) {
	info, err := llm.ResolveModelFromCatalog(p.id, p.modelCatalog, model)
	if err != nil {
		return llm.ModelInfo{}, err
	}
	info.DefaultMaxTokens = llm.ModelDefaultMaxTokens(p.modelCatalog, model)
	return info, nil
}

func (p *anthropicProvider) Close() error {
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

func (p *anthropicProvider) keySnapshot(ctx context.Context) (string, error) {
	p.apiKeyMu.RLock()
	provider := p.credentialProvider
	key := p.apiKey
	p.apiKeyMu.RUnlock()
	if provider != nil {
		return provider(ctx)
	}
	return key, nil
}

func (p *anthropicProvider) keyLease(ctx context.Context) (string, llm.CredentialLease, error) {
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
// never performs a network call — apiKey present and base URL parseable (same
// shape as deepseek.Client.Available / web.DeepSeekSearchProvider.Available,
// dispatch-m8-2b §2.3).
func (p *anthropicProvider) Available() bool {
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

// Stream starts a streaming Messages request and returns an incremental
// reader (D6). The request is serialized per dispatch-m8-2b §2.1, POSTed to
// {baseURL}/messages with the Anthropic headers, and the SSE response is
// decoded per §2.2. ctx cancellation runs through the HTTP request and the
// body reads.
func (p *anthropicProvider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if err := llm.ValidateRequestBlocks(p.ID(), req.Messages); err != nil {
		return nil, err
	}
	apiKey, credentialLease, err := p.keyLease(ctx)
	if err != nil {
		return nil, llm.NewFailureError("anthropic: credential resolution failed", "CREDENTIAL_UNAVAILABLE", err)
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && credentialLease != nil {
			credentialLease.Release()
		}
	}()
	if apiKey == "" {
		return nil, llm.NewFailureError("anthropic: credential unavailable", "CREDENTIAL_UNAVAILABLE", nil)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.model
	}
	if err := llm.ValidateReasoningEffortForModel(p.ID(), p.modelCatalog, model, req.ReasoningEffort); err != nil {
		return nil, err
	}
	// M8-3b image fail-closed check FIRST (dispatch-m8-3b §3): a model that
	// does not declare image input must error on an image request, never
	// silently drop it. The check runs before offload so offloading cannot
	// mask an image.
	if !llm.ModelSupportsImages(p.supportsImages, p.modelCatalog, model) {
		for _, m := range req.Messages {
			if m.HasImage() {
				return nil, fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", p.ID())
			}
		}
	}
	// M8-3b: apply the request image-byte budget (over-budget images, oldest
	// first, are replaced by the OffloadedImageText placeholder) before
	// serialization.
	msgs := llm.OffloadRequestImages(req.Messages, p.maxRequestImageBytes)

	messages, err := toWireMessages(msgs)
	if err != nil {
		return nil, err
	}
	maxTokens := p.maxTokens
	if catalogDefault := llm.ModelDefaultMaxTokens(p.modelCatalog, model); catalogDefault > 0 {
		maxTokens = catalogDefault
	}
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = llm.ModelDefaultReasoningEffort(p.modelCatalog, model)
	}
	reasoning, reasoningKnown := llm.ModelReasoningCapability(p.modelCatalog, model)
	thinking := thinkingForEffort(effort, req.ReasoningBudgetTokens, reasoningKnown && reasoning)
	temperature := req.Temperature
	if thinking != nil && thinking.Type == "enabled" {
		// Anthropic rejects temperature together with extended thinking.
		temperature = nil
	}
	body := requestBody{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stop:        append([]string(nil), req.Stop...),
		System:      extractSystem(msgs),
		Messages:    messages,
		Tools:       toWireTools(req.Tools),
		Stream:      true,
		Thinking:    thinking,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
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
	return nil, llm.ClassifyHTTPFailureWithMetadata("anthropic", resp.StatusCode, resp.Status, detail,
		llm.RetryAfterMilliseconds(resp.Header.Get("Retry-After"), time.Now()), resp.Header.Get("request-id"))
}

// doNoRedirect issues httpReq with a no-follow redirect policy (any 3xx is
// blocked at CheckRedirect and mapped to an error, mirroring M7
// web/deepseek.go). ctx cancellation is reported as a cancellation error.
func (p *anthropicProvider) doNoRedirect(req *http.Request) (*http.Response, error) {
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
		if errors.Is(err, errRedirectDetected) {
			return nil, llm.NewFailureError("anthropic: redirect blocked (3xx not followed)", "TRANSPORT", err)
		}
		if req.Context().Err() != nil {
			return nil, llm.NewFailureError("anthropic: cancelled: "+req.Context().Err().Error(), "ABORTED", req.Context().Err())
		}
		return nil, llm.NewFailureError("anthropic: request failed: "+err.Error(), "TRANSPORT", err)
	}
	return resp, nil
}

// errorDetail extracts the server-provided message from a non-2xx response
// (bounded 1 MiB read; shapes {"error":{"message":...}} or {"message":...},
// mirroring M7 web/deepseek.go anthropicErrorDetail). Empty string when it
// cannot be parsed.
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

// —— wire shapes for the Messages API request body (dispatch-m8-2b §2.1) ——

// wireMessage is one entry of the "messages" array. Content is an ordered
// block list; blocks are map[string]any so thinking / tool_use / tool_result
// can carry their own shapes without a double-track type.
type wireMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// wireTool is one entry of the "tools" array.
type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type requestBody struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stop        []string      `json:"stop_sequences,omitempty"`
	System      string        `json:"system,omitempty"` // extracted RoleSystem text
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	Thinking    *thinkingWire `json:"thinking,omitempty"`
}

type thinkingWire struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

func thinkingForEffort(effort string, budget int, reasoningKnownTrue bool) *thinkingWire {
	switch strings.TrimSpace(effort) {
	case "off":
		if !reasoningKnownTrue {
			return nil
		}
		return &thinkingWire{Type: "disabled"}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		if budget <= 0 {
			budget = 1024
		}
		return &thinkingWire{Type: "enabled", BudgetTokens: budget}
	default:
		return nil
	}
}

// extractSystem joins every RoleSystem message's text into the top-level
// "system" field (dispatch-m8-2b §2.1 rule 1); system messages never enter
// the messages array.
func extractSystem(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role != llm.RoleSystem {
			continue
		}
		if t := m.Text(); t != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// toWireMessages serializes a chat history into the Messages API "messages"
// array (dispatch-m8-2b §2.1 rules 2–5 / M8-3b §4.2):
//   - RoleSystem messages are extracted to the top-level system field
//     (extractSystem) and never enter the array;
//   - user messages become text blocks (M8-3b adds image blocks);
//   - assistant messages keep their block order — reasoning blocks become
//     thinking blocks before text blocks (dsh 范式), and ToolCalls become
//     tool_use blocks with the parsed arguments JSON;
//   - RoleTool messages (tool results) are grouped into a single user message
//     of tool_result blocks at their position in the sequence (consecutive
//     results merge into one message, dispatch-m8-2b §2.1 rule 4);
//   - a user message whose content is empty after conversion gets the
//     "(no output)" placeholder (Anthropic rejects empty content, rule 5).
func toWireMessages(msgs []llm.Message) ([]wireMessage, error) {
	var out []wireMessage
	var pendingToolResults []map[string]any
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, wireMessage{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			// Extracted to the top-level system field; not a wire message.
		case llm.RoleTool:
			pendingToolResults = append(pendingToolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Text(),
			})
		case llm.RoleUser:
			flushToolResults()
			blocks, err := textBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, wireMessage{Role: "user", Content: blocks})
		case llm.RoleAssistant:
			flushToolResults()
			out = append(out, wireMessage{Role: "assistant", Content: assistantBlocks(m)})
		}
	}
	flushToolResults()
	return out, nil
}

// textBlocks converts a user message's content parts to wire blocks
// (dispatch-m8-2b §2.1 rule 2, M8-3b §4.2): BlockText → {"type":"text"},
// BlockImage → {"type":"image","source":{"type":"base64","media_type",...,
// "data":<base64>}} (reading the image bytes at the ImageRef path at request
// time; a read failure is an error — fail-closed, 不静默丢图). An empty result
// yields the "(no output)" placeholder (rule 5).
func textBlocks(blocks []llm.ContentBlock) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case llm.BlockText:
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case llm.BlockImage:
			data, err := imageBase64(b.Image)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.Image.MediaType,
					"data":       data,
				},
			})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": noOutputPlaceholder})
	}
	return out, nil
}

// imageBase64 reads the image bytes at ref.Path and base64-encodes them for an
// Anthropic image block source (dispatch-m8-3b §4.2). The bytes are read only
// at request-serialization time and are never logged or kept in memory (dsh
// 7078918 范式). A read failure returns an error (fail-closed, 不静默丢图). llm
// does not depend on attachment — the provider reads the file directly from
// the ImageRef path (M8-3a 单向依赖纪律).
func imageBase64(ref llm.ImageRef) (string, error) {
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return "", fmt.Errorf("anthropic: read image %s: %w", ref.Path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// assistantBlocks serializes an assistant message's content in order
// (dispatch-m8-2b §2.1 rule 3, dsh 范式: reasoning before text):
// BlockReasoning → thinking, BlockText → text, then ToolCalls → tool_use.
func assistantBlocks(m llm.Message) []map[string]any {
	out := make([]map[string]any, 0, len(m.Content)+len(m.ToolCalls))
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockReasoning:
			out = append(out, map[string]any{"type": "thinking", "thinking": b.Text})
		case llm.BlockText:
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": parseArguments(tc.Arguments),
		})
	}
	return out
}

// parseArguments unmarshals a tool call's raw JSON arguments into the tool_use
// "input" object (dispatch-m8-2b §2.1 rule 3). An empty argument string maps
// to an empty object (the no-arguments case); any parse failure or a
// non-object result falls back to {"_raw": <raw>} so the arguments are never
// silently dropped.
func parseArguments(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v == nil {
		return map[string]any{"_raw": raw}
	}
	return v
}

// toWireTools converts the tool schemas to the Messages API "tools" array
// (dispatch-m8-2b §2.1).
func toWireTools(tools []llm.ToolSchema) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	return out
}
