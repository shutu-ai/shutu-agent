// tools.go — the M7-2 Consumer half of the web seam (design.md §10 D2,
// dispatch-m7-2 §4): web_search and web_fetch are registered into the
// tools.Registry by the composition root (cmd/pa) when web.enabled, and
// auto-whitelisted by config.applyDefaults the same way the kb_*/job_*/fs_*/
// code_* tools are. The tools implement the tools.Tool method set structurally
// (Go structural typing), so this package never imports the tools package —
// the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; queries/url as plain strings, maxItems = SearchMaxQueries) before
// this code runs; the checks are repeated here so a direct call can never
// bypass them.
//
// D3: web/search-request is logged by the DeepSeek provider's OnRequest
// (M7-1), never repeated by the tool layer; web_fetch has no web event (the
// result goes through tool/result). The injected onEvent sink is part of the
// seam contract and reserved for a future consumer — nothing is emitted here
// in M7-2.
package web

import (
	"context"
	"errors"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Tool names — the two web consumer tools (whitelisted when web.enabled; see
// config.webToolNames).
const (
	ToolSearchName = "web_search"
	ToolFetchName  = "web_fetch"
)

// 工具层产品上限的默认值（与 internal/config WebConfig 的默认值一致；
// NewWebTools 对 <=0 的值落回这些默认，使直接构造也安全 — 镜像 provider 的
// 0→默认规则）。搜索/抓取 provider id 直接复用同包 provider 的稳定 id；
// defaultFetchTimeoutMs 复用 httpfetch.go 已声明的同值常量。
const (
	defaultSearchMaxResults    = 8
	defaultSearchMaxQueries    = 4
	defaultSearchTimeoutMs     = 30000
	defaultFetchMaxOutputChars = 200000
)

// Options 是工具层的产品上限（来自 WebConfig，默认见 NewWebTools）。SearchID/
// FetchID 为空串时落到 provider 的稳定 id（deepseek-official / http）。
type Options struct {
	SearchID            string // 默认 "deepseek-official"
	FetchID             string // 默认 "http"
	SearchMaxResults    int    // 单次/合并后来源上限，默认 8
	SearchMaxQueries    int    // 一次调用查询数上限，默认 4
	SearchTimeoutMs     int    // 外层搜索超时，默认 30000
	FetchTimeoutMs      int    // 外层抓取超时，默认 30000
	FetchMaxOutputChars int    // web_fetch 返回给模型的 body 上限，默认 200000
}

// WebTools 捆绑两个 web 工具共享状态：Engine、选择的 provider id、上限、事件
// sink。工具实现 tools.Tool 方法集（结构性实现，不 import tools 包，D2）。
type WebTools struct {
	engine  *Engine
	opts    Options
	onEvent func(typ string, data any)
}

// NewWebTools 返回绑定到 engine 的 web 工具 bundle。onEvent，当非 nil 时接收
// web/* 事件载荷；M7-2 中工具层不发射任何 web 事件（D3，事件由 provider 自己
// 落日志），该 sink 保留给未来消费方。
func NewWebTools(engine *Engine, opts Options, onEvent func(typ string, data any)) *WebTools {
	t := &WebTools{engine: engine, opts: opts, onEvent: onEvent}
	if t.opts.SearchID == "" {
		t.opts.SearchID = deepseekProviderID
	}
	if t.opts.FetchID == "" {
		t.opts.FetchID = httpFetchProviderID
	}
	if t.opts.SearchMaxResults <= 0 {
		t.opts.SearchMaxResults = defaultSearchMaxResults
	}
	if t.opts.SearchMaxQueries <= 0 {
		t.opts.SearchMaxQueries = defaultSearchMaxQueries
	}
	if t.opts.SearchTimeoutMs <= 0 {
		t.opts.SearchTimeoutMs = defaultSearchTimeoutMs
	}
	if t.opts.FetchTimeoutMs <= 0 {
		t.opts.FetchTimeoutMs = defaultFetchTimeoutMs
	}
	if t.opts.FetchMaxOutputChars <= 0 {
		t.opts.FetchMaxOutputChars = defaultFetchMaxOutputChars
	}
	return t
}

// Search 返回 web_search 工具。
func (t *WebTools) Search() WebSearchTool { return WebSearchTool{t: t} }

// Fetch 返回 web_fetch 工具。
func (t *WebTools) Fetch() WebFetchTool { return WebFetchTool{t: t} }

// WebSearchTool 执行一次或多次搜索并合并结果（派发 §4）。单查询直接透传
// provider 结果；多查询顺序扇出（D5，for 逐个 Search，出错即返回第一个错误、
// 不再继续）后经 mergeSearchResults 合并。外层 context.WithTimeout 包裹整个
// 调用（所有查询共享一个预算）。
type WebSearchTool struct {
	t *WebTools
}

func (WebSearchTool) Name() string { return ToolSearchName }

func (WebSearchTool) Description() string {
	return "search the web for current information; returns an optional summary answer and a list of source URLs"
}

func (t WebSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"queries": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    t.t.opts.SearchMaxQueries,
				"description": fmt.Sprintf("required search queries; accepts 1\u2013%d items and merges their results", t.t.opts.SearchMaxQueries),
			},
		},
		"required":             []string{"queries"},
		"additionalProperties": false,
	}
}

func (t WebSearchTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Queries []string `json:"queries"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	queries, err := parseSearchArgs(a.Queries, t.t.opts.SearchMaxQueries)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.t.opts.SearchTimeoutMs)*time.Millisecond)
	defer cancel()

	var result WebSearchResult
	if len(queries) == 1 {
		// 单查询：透传 provider 结果（Engine 已按 MaxResults 截断）。
		result, err = t.t.engine.Search(ctx, t.t.opts.SearchID, WebSearchRequest{
			Query:      queries[0],
			MaxResults: t.t.opts.SearchMaxResults,
		})
	} else {
		// 多查询：顺序扇出（D5，无后台 goroutine），首个错误即中止。
		items := make([]searchItem, 0, len(queries))
		for _, q := range queries {
			res, serr := t.t.engine.Search(ctx, t.t.opts.SearchID, WebSearchRequest{
				Query:      q,
				MaxResults: t.t.opts.SearchMaxResults,
			})
			if serr != nil {
				return "", mapSearchError(serr, t.t.opts.SearchID)
			}
			items = append(items, searchItem{query: q, result: res})
		}
		result = mergeSearchResults(items, t.t.opts.SearchMaxResults)
	}
	if err != nil {
		return "", mapSearchError(err, t.t.opts.SearchID)
	}
	return formatSearchOutput(result), nil
}

// parseSearchArgs 净化并校验 web_search 参数（派发 §4）：去掉空/全空白串、
// 去重保序（首个出现优先），净化后为空返回错误。maxQueries 上限在这里重复
// 检查（D7 已由注册表 schema 的 maxItems 校验；重复以保证直接调用也无法绕过，
// 照 fs_* 同款防御）。
func parseSearchArgs(queries []string, maxQueries int) ([]string, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("queries must contain at least one query")
	}
	if maxQueries > 0 && len(queries) > maxQueries {
		noun := "queries"
		if maxQueries == 1 {
			noun = "query"
		}
		return nil, fmt.Errorf("queries must contain at most %d %s", maxQueries, noun)
	}
	var out []string
	seen := make(map[string]bool)
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			continue
		}
		if seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("queries must contain at least one non-empty query")
	}
	return out, nil
}

// searchItem 把一次查询与其规范化结果绑在一起，使 merge 能重建每查询的
// content 标题。
type searchItem struct {
	query  string
	result WebSearchResult
}

// mergeSearchResults 合并多查询结果（照 dsh round-robin）：按 rank 轮询每个
// 结果的 sources，URL 未见过则加入；到达 max 停止并置 droppedSource。
// Truncated = 任一 result.Truncated || droppedSource；Content 为各 result 的
// 非空 Content 拼 "### <query>\n\n<content>"。
func mergeSearchResults(items []searchItem, max int) WebSearchResult {
	maxRank := 0
	for _, it := range items {
		if len(it.result.Sources) > maxRank {
			maxRank = len(it.result.Sources)
		}
	}
	seen := make(map[string]bool)
	var sources []WebSearchSource
	dropped := false
merge:
	for rank := 0; rank < maxRank; rank++ {
		for _, it := range items {
			if rank >= len(it.result.Sources) {
				continue
			}
			src := it.result.Sources[rank]
			if seen[src.URL] {
				continue
			}
			seen[src.URL] = true
			if len(sources) == max {
				dropped = true
				break merge
			}
			sources = append(sources, src)
		}
	}

	var sb strings.Builder
	for _, it := range items {
		if it.result.Content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "### %s\n\n%s", it.query, it.result.Content)
	}
	truncated := dropped
	for _, it := range items {
		if it.result.Truncated {
			truncated = true
			break
		}
	}
	return WebSearchResult{Content: sb.String(), Sources: sources, Truncated: truncated}
}

// formatSearchOutput 把搜索结果渲染为模型可读文本（照 dsh formatSearchOutput）：
// provider 回答（有则在前）、来源列表、截断提示、固定的引用提示。无来源且无
// content → "No results found."。
func formatSearchOutput(result WebSearchResult) string {
	var parts []string
	if result.Content != "" {
		parts = append(parts, result.Content)
	}
	if len(result.Sources) > 0 {
		lines := make([]string, 0, len(result.Sources))
		for _, src := range result.Sources {
			label := sourceLabel(src.URL, src.Title)
			var meta []string
			if src.Snippet != "" {
				meta = append(meta, src.Snippet)
			}
			if src.PublishedAt != "" {
				meta = append(meta, "("+src.PublishedAt+")")
			}
			suffix := ""
			if len(meta) > 0 {
				suffix = " \u2014 " + strings.Join(meta, " ")
			}
			lines = append(lines, fmt.Sprintf("- [%s](%s)%s", label, src.URL, suffix))
		}
		parts = append(parts, "Sources:\n"+strings.Join(lines, "\n"))
	} else if result.Content == "" {
		parts = append(parts, "No results found.")
	}
	if result.Truncated {
		parts = append(parts, fmt.Sprintf("(Showing the first %d sources. Refine the query for more.)", len(result.Sources)))
	}
	parts = append(parts, "Cite the relevant URLs above as markdown links in your answer.")
	return strings.Join(parts, "\n\n")
}

// sourceLabel 是来源的展示标签：title 非空则用 title，否则 hostname(url)；
// URL 解析失败或 host 为空时回落原始 URL（照 dsh sourceLabel）。
func sourceLabel(rawURL, title string) string {
	if title != "" {
		return title
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return rawURL
	}
	return u.Hostname()
}

// mapSearchError 按 errors.Is 把搜索 sentinel 错误映射为给模型的可读文本。
func mapSearchError(err error, id string) error {
	switch {
	case errors.Is(err, ErrNoProvider):
		return fmt.Errorf("web_search: no search provider %q is available (enable web and set DEEPSEEK_API_KEY)", id)
	case errors.Is(err, ErrCredential):
		return fmt.Errorf("web_search: search provider not usable: %v", err)
	case errors.Is(err, ErrAborted):
		return errors.New("web_search: search aborted (cancelled)")
	case errors.Is(err, ErrProvider):
		return fmt.Errorf("web_search: search provider error: %v", err)
	default:
		return fmt.Errorf("web_search: search failed: %v", err)
	}
}

// WebFetchTool 抓取单个 URL 并返回解码后的内容（派发 §4）：html body 经
// HTMLToMarkdown 转成 Markdown，text body 原样；body 截到 FetchMaxOutputChars
// （超了加 "[truncated: showing first N chars]"）。外层 context.WithTimeout
// 包裹整个调用。
type WebFetchTool struct {
	t *WebTools
}

func (WebFetchTool) Name() string { return ToolFetchName }

func (WebFetchTool) Description() string {
	return "fetch the content of a specific HTTP(S) URL and return it decoded to text"
}

func (WebFetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "the HTTP(S) URL to fetch",
			},
		},
		"required":             []string{"url"},
		"additionalProperties": false,
	}
}

func (t WebFetchTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	if strings.TrimSpace(a.URL) == "" {
		return "", fmt.Errorf("web_fetch: empty url")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.t.opts.FetchTimeoutMs)*time.Millisecond)
	defer cancel()

	res, err := t.t.engine.Fetch(ctx, t.t.opts.FetchID, WebFetchRequest{URL: a.URL})
	if err != nil {
		return "", mapFetchError(err, t.t.opts.FetchID)
	}
	return formatFetchOutput(res, t.t.opts.FetchMaxOutputChars), nil
}

// formatFetchOutput 渲染抓取结果为模型可读文本（派发 §4）：
//
//	<url>
//	HTTP <statusCode>
//
//	<body>
//
// html kind 先经 HTMLToMarkdown；body 截到 maxChars（Unicode-safe，不劈开
// 多字节字符），截断（工具层或 provider）时追加
// "[truncated: showing first N chars]"（N = 实际展示字符数）。
func formatFetchOutput(res WebFetchResult, maxChars int) string {
	body := res.Body.Content
	if res.Body.Kind == "html" {
		body = HTMLToMarkdown(body)
	}
	shown, cut := truncateChars(body, maxChars)
	truncated := res.Truncated || cut

	var sb strings.Builder
	sb.WriteString(res.URL)
	sb.WriteByte('\n')
	fmt.Fprintf(&sb, "HTTP %d\n\n", res.StatusCode)
	sb.WriteString(shown)
	if truncated {
		fmt.Fprintf(&sb, "\n\n[truncated: showing first %d chars]", utf8.RuneCountInString(shown))
	}
	return sb.String()
}

// truncateChars 把 s 截到至多 max rune（Unicode-safe），返回前缀与是否发生了
// 截断。max <= 0 表示不截断。
func truncateChars(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return string([]rune(s)[:max]), true
}

// mapFetchError 按 errors.Is 把抓取 sentinel 错误映射为给模型的可读文本。
func mapFetchError(err error, id string) error {
	switch {
	case errors.Is(err, ErrNoProvider):
		return fmt.Errorf("web_fetch: no fetch provider %q is available", id)
	case errors.Is(err, ErrInvalidURL):
		return fmt.Errorf("web_fetch: invalid URL: %v", err)
	case errors.Is(err, ErrRedirectBlocked):
		// provider 已给出可读的跨源提示（"fetch that URL directly"）。
		return fmt.Errorf("web_fetch: %v", err)
	case errors.Is(err, ErrTimeout):
		return errors.New("web_fetch: timed out fetching the URL")
	case errors.Is(err, ErrAborted):
		return errors.New("web_fetch: fetch aborted (cancelled)")
	case errors.Is(err, ErrProvider):
		return fmt.Errorf("web_fetch: fetch provider error: %v", err)
	default:
		return fmt.Errorf("web_fetch: fetch failed: %v", err)
	}
}
