// Package google implements the llm.Provider for the Google Gemini Generative
// AI API (pi-ai api "google-generative-ai"): streaming SSE over
// streamGenerateContent?alt=sse. Wire follows the Google AI SDK for REST:
// contents are role/parts arrays (user/model), thinking parts carry
// `thought: true` (Gemini 2.5+ reasoning, mirrored onto llm.StreamReasoningDelta),
// tool calls are functionCall parts and tool results functionResponse parts,
// tools are functionDeclarations. Image parts use inlineData base64. There is
// zero new dependency: the HTTP client and SSE reader mirror the existing
// deepseek/anthropic providers. Credentials are env-only (GEMINI_API_KEY, 纪律
// 6): the composition root passes the value in so this package stays testable.
package google

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
	// defaultBaseURL is the Gemini API base URL; "/models/{model}:streamGenerateContent?alt=sse"
	// is appended (default https://generativelanguage.googleapis.com/v1beta).
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// defaultModel is the default model when Config.Model is empty.
	defaultModel = "gemini-2.5-flash"
	// defaultMaxOutputTokens is the route-level request default used by the
	// reference pi-ai adapter. ModelInfo.MaxTokens remains capacity metadata
	// and is not promoted into a request budget.
	defaultMaxOutputTokens = 32768
	// providerID is the stable provider id.
	providerID = "google"
	// maxErrorBody bounds the non-2xx error body read (1 MiB).
	maxErrorBody = 1 << 20
	// defaultMaxRequestImageBytes is the per-request image byte budget applied
	// by New when Config.MaxRequestImageBytes is non-positive (默认 20MiB).
	defaultMaxRequestImageBytes = 20 * 1024 * 1024 // 20 MiB
	// apiKeyHeader is the Google API key header.
	apiKeyHeader = "x-goog-api-key"
)

// errRedirectDetected turns "follow the redirect" into an error: any 3xx is
// never followed nor read (mirroring the deepseek/anthropic providers).
var errRedirectDetected = fmt.Errorf("google: redirect not followed")

// Config configures the Gemini provider. APIKey must come from the environment
// (GEMINI_API_KEY only, 纪律 6).
type Config struct {
	// ID is the provider's registry id; empty defaults to "google".
	ID                      string
	BaseURL                 string // default https://generativelanguage.googleapis.com/v1beta
	APIKey                  string // GEMINI_API_KEY value; empty means absent
	CredentialProvider      llm.CredentialProvider
	CredentialLeaseProvider llm.CredentialLeaseProvider
	Model                   string // default gemini-2.5-flash
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

// googleProvider is the llm.Provider implementing the Gemini Generative AI API.
type googleProvider struct {
	id                      string
	baseURL                 string
	apiKey                  string
	apiKeyMu                sync.RWMutex
	credentialProvider      llm.CredentialProvider
	credentialLeaseProvider llm.CredentialLeaseProvider
	model                   string
	maxOutputTokens         int
	client                  *http.Client

	supportsImages       bool
	maxRequestImageBytes int
	modelCatalog         []llm.ModelInfo
}

// New returns a googleProvider with defaults applied.
func New(cfg Config) *googleProvider {
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
	return &googleProvider{
		id:                      cfg.ID,
		baseURL:                 strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:                  cfg.APIKey,
		credentialProvider:      cfg.CredentialProvider,
		credentialLeaseProvider: cfg.CredentialLeaseProvider,
		model:                   cfg.Model,
		maxOutputTokens:         cfg.MaxOutputTokens,
		client:                  cfg.HTTPClient,
		supportsImages:          cfg.SupportsImages,
		maxRequestImageBytes:    cfg.MaxRequestImageBytes,
		modelCatalog:            llm.CopyModelCatalog(cfg.ModelCatalog),
	}
}

// ID returns the stable provider id ("google" for the built-in adapter, or the
// custom route configured via Config.ID).
func (p *googleProvider) ID() string { return p.id }

func (p *googleProvider) SupportsImages() bool { return p.supportsImages }

func (p *googleProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return llm.CopyModelCatalog(p.modelCatalog), nil
}

func (p *googleProvider) ResolveModelInfo(_ context.Context, model string) (llm.ModelInfo, error) {
	info, err := llm.ResolveModelFromCatalog(p.id, p.modelCatalog, model)
	if err != nil {
		return llm.ModelInfo{}, err
	}
	info.DefaultMaxTokens = llm.ModelDefaultMaxTokens(p.modelCatalog, model)
	return info, nil
}

func (p *googleProvider) Close() error {
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

func (p *googleProvider) keySnapshot(ctx context.Context) (string, error) {
	p.apiKeyMu.RLock()
	provider := p.credentialProvider
	key := p.apiKey
	p.apiKeyMu.RUnlock()
	if provider != nil {
		return provider(ctx)
	}
	return key, nil
}

func (p *googleProvider) keyLease(ctx context.Context) (string, llm.CredentialLease, error) {
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
func (p *googleProvider) Available() bool {
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

// Stream starts a streaming generateContent request and returns an incremental
// reader. The request is serialized per the Gemini REST wire, POSTed to
// {base}/models/{model}:streamGenerateContent?alt=sse with the x-goog-api-key
// header, and the SSE response is decoded into llm.StreamEvents. ctx
// cancellation runs through the HTTP request and the body reads.
func (p *googleProvider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if err := llm.ValidateRequestBlocks(p.ID(), req.Messages); err != nil {
		return nil, err
	}
	apiKey, credentialLease, err := p.keyLease(ctx)
	if err != nil {
		return nil, llm.NewFailureError("google: credential resolution failed", "CREDENTIAL_UNAVAILABLE", err)
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && credentialLease != nil {
			credentialLease.Release()
		}
	}()
	if apiKey == "" {
		return nil, llm.NewFailureError("google: credential unavailable", "CREDENTIAL_UNAVAILABLE", nil)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.model
	}
	if err := llm.ValidateReasoningEffortForModel(p.ID(), p.modelCatalog, model, req.ReasoningEffort); err != nil {
		return nil, err
	}
	// Image fail-closed check FIRST (dispatch-m8-3b §3): a model that does not
	// declare image input must error on an image request, never silently drop it.
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
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = llm.ModelDefaultReasoningEffort(p.modelCatalog, model)
	}
	reasoning, reasoningKnown := llm.ModelReasoningCapability(p.modelCatalog, model)
	thinking := thinkingForEffort(effort, req.ReasoningBudgetTokens, reasoningKnown && reasoning)
	body := requestBody{
		Model:            model,
		System:           extractSystem(msgs),
		Contents:         toContents(msgs, p.maxRequestImageBytes),
		Tools:            toTools(req.Tools),
		GenerationConfig: generationConfig{MaxOutputTokens: maxOutputTokens, Temperature: req.Temperature, StopSequences: append([]string(nil), req.Stop...), ThinkingConfig: thinking},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("google: marshal request: %w", err)
	}

	endpoint := p.baseURL + "/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("google: build request: %w", err)
	}
	httpReq.Header.Set(apiKeyHeader, apiKey)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
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
	return nil, llm.ClassifyHTTPFailureWithMetadata("google", resp.StatusCode, resp.Status, detail,
		llm.RetryAfterMilliseconds(resp.Header.Get("Retry-After"), time.Now()), resp.Header.Get("x-request-id"))
}

// doNoRedirect issues httpReq with a no-follow redirect policy (any 3xx is
// blocked at CheckRedirect and mapped to an error). ctx cancellation is
// reported as a cancellation error.
func (p *googleProvider) doNoRedirect(req *http.Request) (*http.Response, error) {
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
			return nil, llm.NewFailureError("google: redirect blocked (3xx not followed)", "TRANSPORT", err)
		}
		if req.Context().Err() != nil {
			return nil, llm.NewFailureError("google: cancelled: "+req.Context().Err().Error(), "ABORTED", req.Context().Err())
		}
		return nil, llm.NewFailureError("google: request failed: "+err.Error(), "TRANSPORT", err)
	}
	return resp, nil
}

// errorDetail extracts the server-provided message from a non-2xx response
// (bounded 1 MiB read; shapes {"error":{"message":...}}).
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

// —— wire shapes for the Gemini generateContent request body ——

type part struct {
	Text           string              `json:"text,omitempty"`
	Thought        bool                `json:"thought,omitempty"`
	InlineData     *inlineData         `json:"inlineData,omitempty"`
	FunctionCall   *wireFunctionCall   `json:"functionCall,omitempty"`
	FunctionResult *wireFunctionResult `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type wireFunctionResult struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type functionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parametersJsonSchema,omitempty"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type generationConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts"`
	ThinkingBudget  int  `json:"thinkingBudget"`
}

func thinkingForEffort(effort string, budget int, reasoningKnownTrue bool) *thinkingConfig {
	switch strings.TrimSpace(effort) {
	case "off":
		if !reasoningKnownTrue {
			return nil
		}
		return &thinkingConfig{ThinkingBudget: 0}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		if budget <= 0 {
			budget = 1024
		}
		return &thinkingConfig{IncludeThoughts: true, ThinkingBudget: budget}
	default:
		return nil
	}
}

type requestBody struct {
	Model            string           `json:"model"`
	System           *systemPart      `json:"systemInstruction,omitempty"`
	Contents         []content        `json:"contents"`
	Tools            []tool           `json:"tools,omitempty"`
	GenerationConfig generationConfig `json:"generationConfig,omitempty"`
}

type systemPart struct {
	Parts []part `json:"parts"`
}

// extractSystem joins every RoleSystem message's text into the top-level
// systemInstruction; system messages never enter the contents array.
func extractSystem(msgs []llm.Message) *systemPart {
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
	if sb.Len() == 0 {
		return nil
	}
	return &systemPart{Parts: []part{{Text: sb.String()}}}
}

// toContents serializes a chat history into Gemini Content[] (pi-ai
// convertMessages 范式): user → role "user" parts (text + inlineData images),
// assistant → role "model" parts (thinking blocks become thought:true text,
// then text blocks, then functionCall parts), tool results → role "user"
// functionResponse parts.
//
// A tool-result part needs the tool's name, which the provider-neutral Message
// layer does not carry on RoleTool messages (only ToolCallID). The mapping is
// reconstructed from the assistant messages' ToolCalls as the history is
// walked; a result whose id has no seen call falls back to the last assistant
// tool call's name (the single-call round-trip case).
func toContents(msgs []llm.Message, maxRequestImageBytes int) []content {
	var out []content
	callName := map[string]string{}
	lastCallName := ""
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			// Extracted to the top-level systemInstruction; not a content part.
		case llm.RoleTool:
			name := callName[m.ToolCallID]
			if name == "" {
				name = lastCallName
			}
			if name == "" {
				name = "unknown_tool"
			}
			// A tool result answers the assistant's functionCall with a
			// functionResponse part in a user turn.
			out = append(out, content{Role: "user", Parts: []part{{
				FunctionResult: &wireFunctionResult{
					Name:     name,
					Response: map[string]any{"output": m.Text()},
				},
			}}})
		case llm.RoleUser:
			out = append(out, content{Role: "user", Parts: userParts(m)})
		case llm.RoleAssistant:
			for _, tc := range m.ToolCalls {
				callName[tc.ID] = tc.Name
				lastCallName = tc.Name
			}
			out = append(out, content{Role: "model", Parts: assistantParts(m)})
		}
	}
	return out
}

// userParts converts a user message's content parts to Gemini parts: BlockText
// → text, BlockImage → inlineData base64 (reading the bytes at the ImageRef
// path at request time; a read failure is an error — fail-closed). An empty
// result yields a single empty text part so Gemini never sees an empty parts
// list.
func userParts(m llm.Message) []part {
	var out []part
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockText:
			out = append(out, part{Text: b.Text})
		case llm.BlockImage:
			data, err := imageBase64(b.Image)
			if err != nil {
				out = append(out, part{Text: "(image read failed: " + err.Error() + ")"})
				continue
			}
			out = append(out, part{InlineData: &inlineData{MimeType: b.Image.MediaType, Data: data}})
		}
	}
	if len(out) == 0 {
		out = append(out, part{Text: "(no output)"})
	}
	return out
}

// assistantParts serializes an assistant message's content in order
// (pi-ai 范式: reasoning before text, then tool calls): BlockReasoning →
// thought:true text, BlockText → text, then ToolCalls → functionCall parts.
func assistantParts(m llm.Message) []part {
	var out []part
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockReasoning:
			out = append(out, part{Thought: true, Text: b.Text})
		case llm.BlockText:
			out = append(out, part{Text: b.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		out = append(out, part{FunctionCall: &wireFunctionCall{Name: tc.Name, Args: parseArguments(tc.Arguments)}})
	}
	return out
}

// parseArguments unmarshals a tool call's raw JSON arguments into the
// functionCall "args" object. An empty argument string maps to an empty object;
// any parse failure falls back to {"_raw": <raw>} so the arguments are never
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

// toTools converts the tool schemas to Gemini functionDeclarations.
func toTools(tools []llm.ToolSchema) []tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]tool, 0, 1)
	decls := make([]functionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	out = append(out, tool{FunctionDeclarations: decls})
	return out
}

// imageBase64 reads the image bytes at ref.Path and base64-encodes them for a
// Gemini inlineData part. The bytes are read only at request-serialization
// time and are never logged or kept in memory. A read failure returns an error
// (fail-closed).
func imageBase64(ref llm.ImageRef) (string, error) {
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return "", fmt.Errorf("google: read image %s: %w", ref.Path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
