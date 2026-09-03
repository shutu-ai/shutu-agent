# M7-1 派发：web 能力接缝 + DeepSeek 官方搜索 Provider + `web/search-request` 事件

> 里程碑 M7 web 搜索（ADR `docs/decisions/2026-08-20-m7-web-search.md`）。本文件是 **M7-1（前半）** 的自包含契约，先实现接缝与搜索 Provider；M7-2（fetch provider + HTML 转换 + `web_search`/`web_fetch` 工具 + config + 组合根接线）在后续派发。
> 只读参考：`../deepseek-harness/packages/web/web/src/types.ts`、`web-search-deepseek/src/provider.ts`（借鉴思路，不照搬 TS 代码）。

## 0. 纪律（与历次派发一致）

- **不改 `internal/loop/loop.go`**（D4）；主循环保持串行（D5）；**零新第三方依赖**；CGO-free；原有测试全绿。
- **模型可见 ⇒ 已落日志**（D3）：先定义事件类型，再实现。
- **工具参数入口校验**（D7）：工具在 M7-2 才做；本 half 只做接缝 + Provider，**不注册任何工具、不改 config、不改 cmd/pa**。
- 包内依赖单向：`internal/web` 不依赖 `internal/loop`；消费方只依赖接口（D2）。
- 每个模块完成后**阶段提交**（commit message 前缀 `M7-1`）。

## 1. 背景与范围

github.com/shutu-ai/shutu-agent 需要联网能力。M7 定 DeepSeek 官方搜索：通过 DeepSeek **Anthropic 兼容 Messages API**（`POST /anthropic/v1/messages`）发起一次携带原生 `web_search_20250305` server tool 的模型调用，DeepSeek 服务器端执行搜索并返回**结构化** `web_search_tool_result` blocks——不爬散文。复用 `DEEPSEEK_API_KEY`（env-only，纪律 6）。

**本 half 交付**（`internal/web/`）：
1. `service.go`：seam 契约（SearchProvider / FetchProvider 接口 + 注册表 + Engine + `WebError` sentinel）+ `WebSearchRequest`/`WebSearchResult`/`WebSearchSource`/`WebFetchRequest`/`WebFetchResult`/`WebFetchBody` 类型。
2. `deepseek.go`：`DeepSeekSearchProvider`（Anthropic 兼容 Messages API 非流式 POST + `web_search_20250305` + 解析）。
3. 事件载荷类型：`SearchRequestEvent`（供 M7-2 组合根把 `web/search-request` 落日志；本 half 只定义类型 + 构造函数，不接线）。
4. 单元测试。

**不做（本 half）**：fetch provider、HTML→markdown、`web_search`/`web_fetch` 工具、config、cmd/pa 接线（全在 M7-2）。

## 2. 包结构

```
internal/web/
├── service.go     # 类型 + Provider 接口 + 注册表 + Engine + WebError + 事件载荷类型
├── service_test.go
├── deepseek.go    # DeepSeekSearchProvider
└── deepseek_test.go
```

## 3. service.go 契约（完整签名）

```go
// Package web 定义联网能力接缝（design.md §10 D2，ADR 2026-08-20-m7-web-search.md）。
// Search 与 Fetch 共享一个接缝（单一属主：provider 选择 / 取消 / 错误 / 上限），
// 但各自独立 request/result 类型。消费方（M7-2 的 web_* 工具与组合根接线）
// 只依赖本包的接口（D2）。
package web

// WebSearchRequest 是一次搜索的输入：单查询。maxResults 是返回来源数上限
// （由 web_* 工具层传入，Engine 在返回路径强制截断；缺省=不截断）。
type WebSearchRequest struct {
    Query      string
    MaxResults int // <=0 表示不截断
}

// WebSearchSource 是一个可引用来源：url 必有；title/snippet/publishedAt 可选。
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
    URL        string       // 跟随重定向后的最终 URL
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
```

**Engine（注册表 + 选择 + 截断）**：

```go
// Engine 是 web 能力接缝的注册表 + 选择器（D2）：注册多个 SearchProvider /
// FetchProvider，按 id 选择执行，并在返回路径强制 maxResults 截断。
type Engine struct{ ... }

// NewEngine 返回空 Engine（未注册任何 provider）。
func NewEngine() *Engine

// RegisterSearchProvider 注册一个搜索 provider（id 重复返回错误）。
func (e *Engine) RegisterSearchProvider(p SearchProvider) error
// RegisterFetchProvider 注册一个抓取 provider（id 重复返回错误）。
func (e *Engine) RegisterFetchProvider(p FetchProvider) error

// Search 按 id 选择 provider 执行一次搜索。找不到 provider 返回 ErrNoProvider。
// 结果在返回前按 req.MaxResults 截断（>0 时），并置 Truncated。
func (e *Engine) Search(ctx context.Context, id string, req WebSearchRequest) (WebSearchResult, error)
// Fetch 按 id 选择 provider 执行一次抓取。
func (e *Engine) Fetch(ctx context.Context, id string, req WebFetchRequest) (WebFetchResult, error)
```

**WebError sentinel**（调用方可 errors.Is 区分，不解析文本）：

```go
var (
    // 提供方执行失败（HTTP 错误、无 result block、响应不可解析等）。
    ErrProvider       = errors.New("web: provider error")
    // 调用方取消（ctx 取消）。
    ErrAborted        = errors.New("web: aborted")
    // 凭证缺失（如 DEEPSEEK_API_KEY 未设置）。
    ErrCredential     = errors.New("web: provider credential missing")
    // 重定向被阻断（跨源 / 超限 / 无 Location）。
    ErrRedirectBlocked = errors.New("web: redirect blocked")
    // 抓取超时。
    ErrTimeout        = errors.New("web: fetch timeout")
    // URL 非法 / 超过长度上限。
    ErrInvalidURL     = errors.New("web: invalid url")
    // 响应体超限（字节或字符上限）。
    ErrTooLarge       = errors.New("web: response too large")
    // 未注册指定 id 的 provider。
    ErrNoProvider     = errors.New("web: no such provider")
)
```

**事件载荷类型**（供 M7-2 落 `web/search-request`，本 half 只定义）：

```go
// SearchRequestEvent 是 web/search-request 事件的载荷（D3，secret-free）：
// 一次搜索请求在派发前落库的快照，绝不含 API key。
type SearchRequestEvent struct {
    Endpoint   string
    APIVersion string
    Model      string
    Query      string
    Body       map[string]any // 实际发送给 provider 的请求体（JSON 化后快照）
}
```

## 4. deepseek.go 契约（DeepSeekSearchProvider）

```go
// Config 配置 DeepSeek 搜索 provider。APIKey 从环境变量读取（纪律 6，
// 由组合根传入；本包不直接读 os 环境，保持可测试）。
type Config struct {
    APIKey     string // DEEPSEEK_API_KEY 值；空串表示缺失
    BaseURL    string // 默认 https://api.deepseek.com/anthropic/v1（/messages 附加）
    Model      string // 默认 deepseek-v4-flash
    APIVersion string // 默认 2023-06-01（anthropic-version 头）
    MaxTokens  int    // 默认 4096
    MaxUses    int    // 默认 5（web_search server tool 单请求最大使用次数）
    HTTPClient *http.Client // 可选；默认 http.DefaultClient
    // OnRequest 在派发前收到 secret-free 请求快照（组合根用它落 web/search-request）。
    // 返回错误则阻止派发（模型可见的辅助输入不能逃过日志，D3）。
    OnRequest func(SearchRequestEvent) error
}

// NewDeepSeekProvider 返回 DeepSeekSearchProvider（可用性检查见下）。
func NewDeepSeekProvider(cfg Config) *DeepSeekSearchProvider

type DeepSeekSearchProvider struct{ ... }

func (p *DeepSeekSearchProvider) ID() string        // 返回 "deepseek-official"
func (p *DeepSeekSearchProvider) Available() bool   // apiKey 非空 且 baseURL 可解析 且 MaxTokens/MaxUses 为正
func (p *DeepSeekSearchProvider) Search(ctx context.Context, req WebSearchRequest) (WebSearchResult, error)
```

**Search 行为**（照 dsh `provider.ts` 语义）：
1. 组装请求：`POST {baseURL}/messages`，headers：
   - `x-api-key: <key>`
   - `authorization: Bearer <key>`（Anthropic 兼容代理可能用这个，两个都发）
   - `anthropic-version: <apiVersion>`
   - `content-type: application/json`
   - `accept: application/json`
2. body（JSON）：
   ```json
   {
     "model": "<model>",
     "max_tokens": <maxTokens>,
     "messages": [{"role": "user", "content": [{"type": "text", "text": "Perform a web search for the query: <query>"}]}],
     "tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": <maxUses>}]
   }
   ```
3. **派发前**：调用 `OnRequest`（若提供）；返回错误则中止派发并返回该错误。
4. 发送（ctx 取消贯穿 http 请求）；**redirect 策略：不跟随**（Go 里用自定义 CheckRedirect 返回 http.ErrUseLastResponse 并把它当错误处理——dsh 是 `redirect: 'error'`，任何 3xx 都是 WEB_PROVIDER_ERROR，不读取 Location）。
5. 非 2xx：尝试解析错误体 JSON（Anthropic error 形状：`{"error": {"type":..,"message":..}}` 或 `{"message":..}`）；用其中的 message 作为错误详情，否则用 HTTP 状态；包装 `ErrProvider`。
6. 解析成功响应（见 §5），返回规范化 `WebSearchResult`。
7. 错误映射：ctx 取消 → `ErrAborted`；凭证为空 → `ErrCredential`（消息提示设置 DEEPSEEK_API_KEY）；其余 → `ErrProvider`（包装细节消息）。

**Anthropic 响应解析**（`mapAnthropicResponse` 纯函数，独立可测）：

```go
// 响应 JSON 形状（只取用到的字段）：
// { "content": [
//     { "type": "text", "text": "...", "citations": [{"url": "...", "cited_text": "..."}] },
//     { "type": "web_search_tool_result", "content": [
//         { "type": "web_search_result", "url": "...", "title": "...", "page_age": "..." }
//     ] }
// ] }

// 解析规则（照 dsh citationSnippets + mapAnthropicResponse）：
// 1. 收集所有 type=="web_search_tool_result" 的 block。
// 2. 若一个都没有 → 返回 ErrProvider（fail-closed：请求可能未触发原生搜索；绝不从散文抓 URL）。
// 3. snippet 来源：遍历所有 type=="text" block 的 citations[]，建 url→cited_text map（首次出现优先）。
// 4. 遍历每个 result block 的 content[] 里 type=="web_search_result" 的 item：
//    - url 非空 且 未见过 → 加入 sources：URL=url，Title=title（非空），Snippet=map[url]（非空），PublishedAt=page_age（非空）。
//    - 按 url 去重（一次请求 max_uses>1 可能重复暴露同一 URL）。
// 5. 返回 { Sources, Truncated: false }（截断由 Engine 负责）。
```

## 5. 测试要求

`service_test.go`：
- 注册表：注册重复 id 报错；未注册 id 的 Search/Fetch 返回 `ErrNoProvider`。
- Engine.Search 截断：provider 返回 5 个 source、`MaxResults=3` → 3 个 + Truncated=true；`MaxResults<=0` → 不截断不置位。
- Engine.Search 透传 ctx 取消（provider 收到取消应返回 `ErrAborted`）。

`deepseek_test.go`（用 `httptest.Server` 假服务，不联网）：
- 请求体断言：method POST、path `/messages`、headers（x-api-key / authorization Bearer / anthropic-version / content-type）、body 含 model/max_tokens/messages[0].content[0].text 含 query/tools[0].type=="web_search_20250305"/max_uses。
- `OnRequest` 在派发前被调用且载荷含 query/endpoint/body；OnRequest 返回错误 → Search 中止、不发 HTTP 请求。
- 成功响应（含 web_search_tool_result + text citations）→ 正确映射 url/title/snippet/publishedAt + 去重。
- 无 result block → `ErrProvider`（fail-closed）。
- 非 2xx（500 + JSON error 体）→ `ErrProvider` 且消息含服务端 detail。
- 凭证空 → `ErrCredential`。
- ctx 取消 → `ErrAborted`。
- 默认值：空 Config → baseURL/model/apiVersion/maxTokens/maxUses 落到默认。

## 6. 提交与报告

- 每模块完成即阶段提交（commit message `M7-1: ...`）。
- 完成后跑 `go vet ./...`、`go test -count=1 ./...`、`go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
