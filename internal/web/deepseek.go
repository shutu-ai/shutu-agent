package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DeepSeek 官方搜索后端（ADR 2026-08-20-m7-web-search.md）：经 Anthropic 兼容
// Messages API（POST {baseURL}/messages）+ 原生 web_search_20250305 server
// tool 发起一次非流式模型调用，DeepSeek 服务器端执行搜索并返回结构化
// web_search_tool_result blocks——不爬散文。复用 DEEPSEEK_API_KEY，零新密钥；
// 请求语义对照 dsh packages/web/web-search-deepseek/src/provider.ts。

// 默认参数（对照 dsh provider 的默认配置）。
const (
	defaultBaseURL    = "https://api.deepseek.com/anthropic/v1"
	defaultModel      = "deepseek-v4-flash"
	defaultAPIVersion = "2023-06-01"
	defaultMaxTokens  = 4096
	defaultMaxUses    = 5

	deepseekProviderID = "deepseek-official"
)

// errRedirectDetected 由自定义 CheckRedirect 返回，把"跟随重定向"转成
// ErrProvider 错误（对照 dsh 的 redirect: 'error'：任何 3xx 都不跟随、不读
// Location）。
var errRedirectDetected = errors.New("web: redirect not followed")

// Config 配置 DeepSeek 搜索 provider。APIKey 从环境变量读取（纪律 6，
// 由组合根传入；本包不直接读 os 环境，保持可测试）。
type Config struct {
	APIKey     string // DEEPSEEK_API_KEY 值；空串表示缺失
	BaseURL    string // 默认 https://api.deepseek.com/anthropic/v1（/messages 附加）
	Model      string // 默认 deepseek-v4-flash
	APIVersion string // 默认 2023-06-01（anthropic-version 头）
	MaxTokens  int    // 默认 4096
	MaxUses    int    // 默认 5（web_search server tool 单请求最大使用次数）
	// HTTPClient 可选；默认 http.DefaultClient。注意：provider 会在其基础上
	// 复制出不跟随重定向的 client，不改动调用方传入的共享 client。
	HTTPClient *http.Client
	// OnRequest 在派发前收到 secret-free 请求快照（组合根用它落 web/search-request）。
	// 返回错误则阻止派发（模型可见的辅助输入不能逃过日志，D3）。
	OnRequest        func(SearchRequestEvent) error
	OnRequestContext func(context.Context, SearchRequestEvent) error
}

// NewDeepSeekProvider 返回 DeepSeekSearchProvider（可用性检查见 Available）。
func NewDeepSeekProvider(cfg Config) *DeepSeekSearchProvider {
	p := &DeepSeekSearchProvider{
		apiKey:           cfg.APIKey,
		baseURL:          cfg.BaseURL,
		model:            cfg.Model,
		apiVersion:       cfg.APIVersion,
		maxTokens:        cfg.MaxTokens,
		maxUses:          cfg.MaxUses,
		onRequest:        cfg.OnRequest,
		onRequestContext: cfg.OnRequestContext,
	}
	if p.baseURL == "" {
		p.baseURL = defaultBaseURL
	}
	if p.model == "" {
		p.model = defaultModel
	}
	if p.apiVersion == "" {
		p.apiVersion = defaultAPIVersion
	}
	// 0 表示"未设置"→默认；显式负值保持原样，让 Available 判为不可用。
	if p.maxTokens == 0 {
		p.maxTokens = defaultMaxTokens
	}
	if p.maxUses == 0 {
		p.maxUses = defaultMaxUses
	}
	if cfg.HTTPClient == nil {
		p.httpClient = http.DefaultClient
	} else {
		p.httpClient = cfg.HTTPClient
	}
	return p
}

// DeepSeekSearchProvider 是 SearchProvider 的 DeepSeek 官方实现。
type DeepSeekSearchProvider struct {
	apiKey           string
	baseURL          string
	model            string
	apiVersion       string
	maxTokens        int
	maxUses          int
	httpClient       *http.Client
	onRequest        func(SearchRequestEvent) error
	onRequestContext func(context.Context, SearchRequestEvent) error
}

// ID 返回稳定 id "deepseek-official"。
func (p *DeepSeekSearchProvider) ID() string { return deepseekProviderID }

// Available 是廉价本地可用性检查：apiKey 非空 且 baseURL 可解析 且
// MaxTokens/MaxUses 为正。绝不做网络调用。
func (p *DeepSeekSearchProvider) Available() bool {
	if p.apiKey == "" || p.maxTokens <= 0 || p.maxUses <= 0 {
		return false
	}
	u, err := url.Parse(p.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return true
}

// Search 发起一次搜索（行为规格见派发文档 §4）。ctx 取消贯穿 http 请求。
func (p *DeepSeekSearchProvider) Search(ctx context.Context, req WebSearchRequest) (WebSearchResult, error) {
	if p.apiKey == "" {
		return WebSearchResult{}, fmt.Errorf("%w: set DEEPSEEK_API_KEY to enable DeepSeek search", ErrCredential)
	}

	body := buildSearchBody(p.model, p.maxTokens, req.Query, p.maxUses)
	endpoint := strings.TrimRight(p.baseURL, "/") + "/messages"

	payload, err := json.Marshal(body)
	if err != nil {
		return WebSearchResult{}, fmt.Errorf("%w: marshal request body: %v", ErrProvider, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return WebSearchResult{}, fmt.Errorf("%w: build request: %v", ErrProvider, err)
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("anthropic-version", p.apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")

	// 派发前落日志（D3）：OnRequest 返回错误则中止派发，不发 HTTP 请求。
	requestEvent := NewSearchRequestEvent(endpoint, p.apiVersion, p.model, req.Query, body)
	if p.onRequestContext != nil {
		if err := p.onRequestContext(ctx, requestEvent); err != nil {
			return WebSearchResult{}, err
		}
	} else if p.onRequest != nil {
		if err := p.onRequest(requestEvent); err != nil {
			return WebSearchResult{}, err
		}
	}

	resp, err := p.doNoRedirect(httpReq)
	if err != nil {
		return WebSearchResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := anthropicErrorDetail(resp)
		if detail == "" {
			detail = resp.Status
		}
		return WebSearchResult{}, fmt.Errorf("%w: %s", ErrProvider, detail)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return WebSearchResult{}, fmt.Errorf("%w: read response: %v", ErrProvider, err)
	}
	return mapAnthropicResponse(data)
}

// doNoRedirect 发出 httpReq，重定向策略为"不跟随"（对照 dsh redirect: 'error'）：
// 任何 3xx 都在 CheckRedirect 处被打断并映射为 ErrProvider，不读取 Location。
// 错误映射：ctx 取消 → ErrAborted；其余 → ErrProvider。
func (p *DeepSeekSearchProvider) doNoRedirect(req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Transport: p.httpClient.Transport,
		Jar:       p.httpClient.Jar,
		Timeout:   p.httpClient.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectDetected
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectDetected) {
			return nil, fmt.Errorf("%w: redirect blocked (3xx not followed)", ErrProvider)
		}
		if req.Context().Err() != nil {
			return nil, ErrAborted
		}
		return nil, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	return resp, nil
}

// buildSearchBody 组装 Anthropic 兼容 Messages 请求体（对照 dsh provider.ts）。
func buildSearchBody(model string, maxTokens int, query string, maxUses int) map[string]any {
	return map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Perform a web search for the query: " + query,
					},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type":     "web_search_20250305",
				"name":     "web_search",
				"max_uses": maxUses,
			},
		},
	}
}

// anthropicErrorDetail 从非 2xx 响应体提取错误详情（Anthropic error 形状：
// {"error":{"type":..,"message":..}} 或 {"message":..}）；无法解析返回空串。
func anthropicErrorDetail(resp *http.Response) string {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
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

// —— Anthropic 响应解析（对照 dsh citationSnippets + mapAnthropicResponse）——
// 响应 JSON 形状（只取用到的字段）：
//
//	{ "content": [
//	    { "type": "text", "text": "...", "citations": [{"url": "...", "cited_text": "..."}] },
//	    { "type": "web_search_tool_result", "content": [
//	        { "type": "web_search_result", "url": "...", "title": "...", "page_age": "..." }
//	    ] }
//	] }

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text"`
	Citations []anthropicCitation   `json:"citations"`
	Content   []anthropicResultItem `json:"content"`
}

type anthropicCitation struct {
	URL       string `json:"url"`
	CitedText string `json:"cited_text"`
}

type anthropicResultItem struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age"`
}

// mapAnthropicResponse 把 Anthropic 兼容响应体解析为规范化 WebSearchResult
// （纯函数，独立可测）。规则见派发文档 §5：
//  1. 一个 web_search_tool_result block 都没有 → ErrProvider（fail-closed：
//     请求可能未触发原生搜索；绝不从散文抓 URL）。
//  2. snippet 来源：所有 text block 的 citations[] 建 url→cited_text map
//     （首次出现优先）。
//  3. 每个 result block 的 content[] 里 type=="web_search_result" 的 item，
//     按 url 去重（一次请求 max_uses>1 可能重复暴露同一 URL）。
func mapAnthropicResponse(data []byte) (WebSearchResult, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return WebSearchResult{}, fmt.Errorf("%w: parse response: %v", ErrProvider, err)
	}

	snippets := make(map[string]string)
	for _, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		for _, c := range block.Citations {
			if c.URL == "" || c.CitedText == "" {
				continue
			}
			if _, ok := snippets[c.URL]; !ok {
				snippets[c.URL] = c.CitedText
			}
		}
	}

	var hasResultBlock bool
	seen := make(map[string]bool)
	var sources []WebSearchSource
	for _, block := range resp.Content {
		if block.Type != "web_search_tool_result" {
			continue
		}
		hasResultBlock = true
		for _, item := range block.Content {
			if item.Type != "web_search_result" || item.URL == "" || seen[item.URL] {
				continue
			}
			seen[item.URL] = true
			src := WebSearchSource{URL: item.URL}
			if item.Title != "" {
				src.Title = item.Title
			}
			if s := snippets[item.URL]; s != "" {
				src.Snippet = s
			}
			if item.PageAge != "" {
				src.PublishedAt = item.PageAge
			}
			sources = append(sources, src)
		}
	}
	if !hasResultBlock {
		return WebSearchResult{}, fmt.Errorf("%w: no web_search_tool_result block (native search may not have run)", ErrProvider)
	}

	return WebSearchResult{Sources: sources, Truncated: false}, nil
}
