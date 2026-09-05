// Package web 定义联网能力接缝（design.md §10 D2，ADR 2026-08-20-m7-web-search.md）。
// Search 与 Fetch 共享一个接缝（单一属主：provider 选择 / 取消 / 错误 / 上限），
// 但各自独立 request/result 类型。消费方（M7-2 的 web_* 工具与组合根接线）
// 只依赖本包的接口（D2）。
package web

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// WebSearchRequest 是一次搜索的输入：单查询。MaxResults 是返回来源数上限
// （由 web_* 工具层传入，Engine 在返回路径强制截断；<=0 表示不截断）。
type WebSearchRequest struct {
	Query      string
	MaxResults int // <=0 表示不截断
}

// WebSearchSource 是一个可引用来源：URL 必有；Title/Snippet/PublishedAt 可选。
type WebSearchSource struct {
	URL         string
	Title       string // 缺省空串
	Snippet     string
	PublishedAt string // provider 提供的 ISO-8601 字符串
}

// WebSearchResult 是规范化搜索结果：Content 为可选 provider 生成回答/摘要；
// Sources 已截断到 MaxResults；Truncated 表示 Engine 是否丢弃了来源。
type WebSearchResult struct {
	Content   string
	Sources   []WebSearchSource
	Truncated bool
}

// WebFetchRequest 是一次抓取的输入：单一 URL。取消走 ctx；呈现属工具层。
type WebFetchRequest struct {
	URL string
}

// WebFetchResult 是规范化抓取结果：非 2xx 也是结果而非错误（状态码是资源状态一部分）；
// Body 已按 content-kind 分类解码；Truncated 表示 provider 是否截断了 body。
type WebFetchResult struct {
	URL        string // 跟随重定向后的最终 URL
	StatusCode int
	Body       WebFetchBody
	Truncated  bool
}

// WebFetchBody 是抓取 body 的封闭判别联合：html / text 两臂。
// M7-2 的渲染层 switch kind；新增 kind 是协调变更（见 ADR 后果）。
type WebFetchBody struct {
	Kind    string // "html" 或 "text"
	Content string
}

// SearchProvider 是一个可搜索后端。id 稳定、在搜索能力内唯一。
// Available 是廉价本地可用性检查，绝不做网络调用。
type SearchProvider interface {
	ID() string
	Available() bool
	Search(ctx context.Context, req WebSearchRequest) (WebSearchResult, error)
}

// FetchProvider 是一个可抓取后端。id 稳定、在抓取能力内唯一。
type FetchProvider interface {
	ID() string
	Available() bool
	Fetch(ctx context.Context, req WebFetchRequest) (WebFetchResult, error)
}

// WebError sentinel 错误组：调用方可 errors.Is 区分，不解析文本。
var (
	// 提供方执行失败（HTTP 错误、无 result block、响应不可解析等）。
	ErrProvider = errors.New("web: provider error")
	// 调用方取消（ctx 取消）。
	ErrAborted = errors.New("web: aborted")
	// 凭证缺失（如 DEEPSEEK_API_KEY 未设置）。
	ErrCredential = errors.New("web: provider credential missing")
	// 重定向被阻断（跨源 / 超限 / 无 Location）。
	ErrRedirectBlocked = errors.New("web: redirect blocked")
	// 抓取超时。
	ErrTimeout = errors.New("web: fetch timeout")
	// URL 非法 / 超过长度上限。
	ErrInvalidURL = errors.New("web: invalid url")
	// 响应体超限（字节或字符上限）。
	ErrTooLarge = errors.New("web: response too large")
	// 未注册指定 id 的 provider。
	ErrNoProvider = errors.New("web: no such provider")
)

// Engine 是 web 能力接缝的注册表 + 选择器（D2）：注册多个 SearchProvider /
// FetchProvider，按 id 选择执行，并在返回路径强制 maxResults 截断。
// 主循环串行（D5），注册发生在接线期、搜索发生在运行期，互不并发；
// 仍用 RWMutex 保护两个注册表以防未来并行消费方。
type Engine struct {
	mu     sync.RWMutex
	search map[string]SearchProvider
	fetch  map[string]FetchProvider
}

// NewEngine 返回空 Engine（未注册任何 provider）。
func NewEngine() *Engine {
	return &Engine{
		search: make(map[string]SearchProvider),
		fetch:  make(map[string]FetchProvider),
	}
}

// RegisterSearchProvider 注册一个搜索 provider（id 重复返回错误）。
func (e *Engine) RegisterSearchProvider(p SearchProvider) error {
	if p == nil {
		return errors.New("web: nil search provider")
	}
	id := p.ID()
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, dup := e.search[id]; dup {
		return fmt.Errorf("web: duplicate search provider id %q", id)
	}
	e.search[id] = p
	return nil
}

// RegisterFetchProvider 注册一个抓取 provider（id 重复返回错误）。
func (e *Engine) RegisterFetchProvider(p FetchProvider) error {
	if p == nil {
		return errors.New("web: nil fetch provider")
	}
	id := p.ID()
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, dup := e.fetch[id]; dup {
		return fmt.Errorf("web: duplicate fetch provider id %q", id)
	}
	e.fetch[id] = p
	return nil
}

// Search 按 id 选择 provider 执行一次搜索。找不到 provider 返回 ErrNoProvider。
// 结果在返回前按 req.MaxResults 截断（>0 时），并置 Truncated。
func (e *Engine) Search(ctx context.Context, id string, req WebSearchRequest) (WebSearchResult, error) {
	e.mu.RLock()
	p, ok := e.search[id]
	e.mu.RUnlock()
	if !ok {
		return WebSearchResult{}, ErrNoProvider
	}
	res, err := p.Search(ctx, req)
	if err != nil {
		return WebSearchResult{}, err
	}
	res.Sources, res.Truncated = truncateSources(res.Sources, req.MaxResults)
	return res, nil
}

// Fetch 按 id 选择 provider 执行一次抓取。
func (e *Engine) Fetch(ctx context.Context, id string, req WebFetchRequest) (WebFetchResult, error) {
	e.mu.RLock()
	p, ok := e.fetch[id]
	e.mu.RUnlock()
	if !ok {
		return WebFetchResult{}, ErrNoProvider
	}
	return p.Fetch(ctx, req)
}

// truncateSources 把 sources 截断到 max（>0 时），返回截断后的切片与是否发生了
// 截断。做一次拷贝，避免与 provider 的内部切片共享底层数组。
func truncateSources(sources []WebSearchSource, max int) ([]WebSearchSource, bool) {
	if max <= 0 || len(sources) <= max {
		return sources, false
	}
	out := make([]WebSearchSource, max)
	copy(out, sources[:max])
	return out, true
}

// SearchRequestEvent 是 web/deepseek-search-llm-request 事件的载荷
// （D3，secret-free）：
// 一次搜索请求在派发前落库的快照，绝不含 API key。
type SearchRequestEvent struct {
	Endpoint   string
	APIVersion string
	Model      string
	Query      string
	Body       map[string]any // 实际发送给 provider 的请求体（JSON 化后快照）
}

// NewSearchRequestEvent 构造搜索请求快照。Body 做顶层防御性拷贝，保证事件
// 消费者不会与调用方共享并意外修改同一个 map。
func NewSearchRequestEvent(endpoint, apiVersion, model, query string, body map[string]any) SearchRequestEvent {
	copy := make(map[string]any, len(body))
	for k, v := range body {
		copy[k] = v
	}
	return SearchRequestEvent{
		Endpoint:   endpoint,
		APIVersion: apiVersion,
		Model:      model,
		Query:      query,
		Body:       copy,
	}
}
