# M7-2 派发：HttpFetchProvider + HTML→Markdown + `web_search`/`web_fetch` 工具 + config + 组合根接线

> 里程碑 M7 web 搜索（ADR `docs/decisions/2026-08-20-m7-web-search.md`）。本文件是 **M7-2（后半）** 的自包含契约，在 **M7-1 已交付的 `internal/web` 接缝**（`service.go`：SearchProvider/FetchProvider/Engine/WebError + `deepseek.go`：DeepSeekSearchProvider + `SearchRequestEvent`）之上实现抓取、工具、配置与接线。
> 只读参考：`../deepseek-harness/packages/web/{web-fetch-http,tool-web}/`（借鉴思路，不照搬 TS 代码）。

## 0. 纪律（与历次派发一致）

- **不改 `internal/loop/loop.go`**（D4）；主循环串行（D5，**无后台 goroutine**）；**零新第三方依赖**；CGO-free；原有测试全绿。
- **工具参数入口校验**（D7）：`web_search`/`web_fetch` 的 Execute 由 `tools.Registry` 按编译好的 JSON Schema 统一校验后才进入实现。
- **D3**：搜索请求经 DeepSeek provider 的 `OnRequest` 落 `web/search-request`（M7-1 已定义）；工具结果走通用 `tool/result`。工具层不再重复发 web 事件。
- 本 half 可改：`internal/config/config.go`、`internal/config/config.yaml`（文档）、`cmd/sta/main.go`（register 链 + /help 状态行）、新增 `internal/web/{httpfetch.go,policy.go,html.go,tools.go}` 及其测试。**不改** `internal/web/{service.go,deepseek.go}`（M7-1 已验收，只读）。
- 每模块完成后**阶段提交**（commit message 前缀 `M7-2`）。

## 1. 交付清单

1. `httpfetch.go` + `policy.go`：`HttpFetchProvider`（URL 校验、同源重定向、超时、字节/字符上限、content-type 分类、UTF-8 解码）。
2. `html.go`：轻量 HTML→Markdown 转换（零依赖，手写扫描器）。
3. `tools.go`：`web_search` / `web_fetch` 工具（D7 schema、多查询**顺序扇出**合并、输出格式化）。
4. `config.go`：`WebConfig` + 默认值 + `applyDefaults` 白名单（`web_search`/`web_fetch`）。
5. `config.yaml`：`web:` 段文档（`enabled: false`）。
6. `cmd/sta/main.go`：`registerWeb()` + 注册链插入（registerFs 之后、registerInteracts 之前）+ `/help` 状态行。
7. 全部测试。

## 2. httpfetch.go + policy.go 契约

```go
// HttpFetchProvider 是抓取能力默认后端（id "http"）：只取公开 http(s) 资源，
// 无 cookie/凭据；跟随同源重定向；强制超时与大小上限；按 content-type 分类
// 并解码。SSRF/内网保护不实现（ADR 后果：个人单机默认可信，已知限制）。
type HttpFetchProvider struct{ ... }

// FetchLimits 是 HttpFetchProvider 的硬上限（来自 WebConfig，默认值见 §5）。
type FetchLimits struct {
    MaxURLBytes    int    // 请求 URL 最大长度（默认 2048）
    MaxResponseBytes int  // 响应体最大字节（读超即截断；默认 2MiB）
    MaxBodyChars   int    // 解码后最大字符（截断；默认 200000）
    TimeoutMs      int    // 单次抓取超时毫秒（默认 30000）
    MaxRedirects   int    // 同源重定向最大跳数（默认 5）
    UserAgent      string // 默认 "shutu-agent/0.1 (M7)"
}

// NewHttpFetchProvider 返回 HttpFetchProvider（Available 恒 true，匿名公开抓取）。
func NewHttpFetchProvider(limits FetchLimits) *HttpFetchProvider

func (p *HttpFetchProvider) ID() string // "http"
func (p *HttpFetchProvider) Available() bool // true
func (p *HttpFetchProvider) Fetch(ctx context.Context, req WebFetchRequest) (WebFetchResult, error)
```

**Fetch 行为**：
1. **URL 校验**（`validateFetchURL`）：仅 `http:`/`https:`；host 非空；总长 ≤ `MaxURLBytes`；其余（userinfo、非法字符等）→ `ErrInvalidURL`。
2. 单次 GET 请求（`http.Client` 自定义 `CheckRedirect` 返回 `http.ErrUseLastResponse` 即不自动跟随、由本 provider 手动处理重定向；`User-Agent` 与 `Accept: text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8`）。
3. **重定向**：3xx 且 ≤ `MaxRedirects` 且 `Location` 存在且解析后**同源**（scheme+host+port 相同，`isSameOrigin`）→ 取消当前 body、跟随（re-validate 目标 URL）；超跳数 / 跨源 / 无 Location → `ErrRedirectBlocked`（跨源消息提示"不自动跟随，请直接对该 URL 抓取"）；其他 3xx 处理失败 → `ErrProvider`。
4. **响应读取**：`io.LimitReader(resp.Body, MaxResponseBytes+1)` 读取，超 `MaxResponseBytes` → 截断标记 + `Truncated`（不报错）。
5. **非 2xx 是结果**：`StatusCode` 记录，body 仍按 §6 分类/解码返回（尽力读，不报错）。传输/网络错误（连接失败、读超时）→ 包装：ctx 取消 → `ErrAborted`；超时 → `ErrTimeout`；其余 → `ErrProvider`。
6. 超时：用 `context.WithTimeout(ctx, TimeoutMs)` 包裹整个请求+读取。
7. 返回 `WebFetchResult{URL: 最终 URL, StatusCode, Body, Truncated}`。

**policy.go 纯函数**（可单测）：

```go
// classifyContentType 按 Content-Type 返回 "html" / "text" / "unsupported"。
// html: text/html, application/xhtml+xml
// text: 其他 text/*、application/json、application/xml、application/javascript、text/plain 等
// unsupported: 其余（二进制 image/*、application/pdf 等）
func classifyContentType(ct string) string

// parseCharset 提取 charset 参数（小写化），无声明返回 "utf-8"。
func parseCharset(ct string) string

// decodeBody 按 charset 解码字节→字符串：仅支持 utf-8 / us-ascii（原样按 UTF-8 容错读）；
// 其他声明编码不转码（按原始字节转 string，乱码风险记为已知裁剪，零依赖取舍）。
// 返回解码字符串（可能已含替换符，不失败）。
func decodeBody(b []byte, charset string) string

// isSameOrigin 判断两 URL scheme+host+port 相同。
func isSameOrigin(a, b *url.URL) bool
```

## 3. html.go 契约（轻量 HTML→Markdown）

```go
// HTMLToMarkdown 把一段 HTML 转成简化 Markdown：覆盖常见文档结构、够模型阅读；
// 不做完整规范（表格/嵌套/属性白名单/CSS 不支持，ADR 后果记录）。零依赖手写扫描器。
func HTMLToMarkdown(htmlStr string) string
```

**转换规则**（`<`…`>` 扫描器状态机，标签名小写化）：
- `<h1>`…`<h6>` → `#`…`######` + 内容 + 换行×2
- `<p>` → 内容 + 空行；`<div>`/`<section>`/`<article>`/`<li>` 等块级 → 换行分隔
- `<a href="…">text</a>` → `[text](href)`（href 相对/空则只留 text）
- `<strong>`/`<b>` → `**…**`；`<em>`/`<i>` → `*…*`
- `<code>` → `` `…` ``；`<pre>` → 前后加 ``` 代码块围栏
- `<ul>`：`<li>` 前加 `- `；`<ol>`：`<li>` 前加 `1. `（简化编号）
- `<blockquote>` → 每行 `> `
- `<br>` / `<br/>` → 换行
- `<img alt="…" src="…">` → `![alt](src)`（alt 缺省空）
- `<script>`/`<style>` 内容整体丢弃
- 实体：`&lt;`/`&gt;`/`&amp;`/`&quot;`/`&#39;` 等用标准库 `html.UnescapeString`
- 其余未知标签：剥离标签、保留文本
- 连续空行压缩为至多一个空行；输出前后 trim

**明确不做**：表格转 markdown、嵌套列表缩进、属性处理（除 a/img）、CSS/内联样式、编码嗅探。

## 4. tools.go 契约（web_search / web_fetch）

```go
// WebTools 捆绑两个 web 工具共享状态：Engine、选择的 provider id、上限、事件 sink。
// 工具实现 tools.Tool 方法集（结构性实现，不 import tools 包，D2）。
type WebTools struct{ ... }

// Options 是工具层的产品上限（来自 WebConfig，默认见 §5）。
type Options struct {
    SearchID        string // 默认 "deepseek-official"
    FetchID         string // 默认 "http"
    SearchMaxResults int   // 单次/合并后来源上限，默认 8
    SearchMaxQueries  int  // 一次调用查询数上限，默认 4
    SearchTimeoutMs   int  // 外层搜索超时，默认 30000
    FetchTimeoutMs    int  // 外层抓取超时，默认 30000
    FetchMaxOutputChars int // web_fetch 返回给模型的 body 上限，默认 200000
}

func NewWebTools(engine *Engine, opts Options, onEvent func(typ string, data any)) *WebTools
func (t *WebTools) Search() WebSearchTool  // 返回 web_search 工具
func (t *WebTools) Fetch() WebFetchTool    // 返回 web_fetch 工具
```

**`web_search`**（name `web_search`）：
- Schema（JSON Schema）：`{ type: "object", additionalProperties: false, properties: { queries: { type: "array", items: { type: "string" }, minItems: 1, maxItems: <SearchMaxQueries>, description: "required search queries; 1–N items" } }, required: ["queries"] }`。
- Execute（D7 已校验后）：
  1. 查询净化：去掉空/全空白串；去重保序（首个出现优先）。净化后为空 → 返回错误。
  2. 单查询：`engine.Search(ctx, SearchID, {Query, MaxResults})`。
  3. 多查询：**顺序扇出**（D5，`for` 逐个 Search，出错即返回第一个错误、不再继续）；收集结果 → `mergeSearchResults`。
  4. 格式化（见下）返回。
- 错误：`ErrNoProvider`/`ErrCredential`/`ErrProvider`/`ErrAborted` 等按 errors.Is 映射为给模型的可读错误文本。
- 超时：`context.WithTimeout(ctx, SearchTimeoutMs)` 包裹。

**`mergeSearchResults`**（纯函数，照 dsh round-robin）：
- 按 rank 轮询每个结果：rank=0..maxLen，对每个 result 取 sources[rank]，URL 未见过则加入；到达 `SearchMaxResults` 停止并置 `droppedSource`。
- `Truncated = 任一 result.Truncated || droppedSource`。
- `Content`：各 result 的非空 Content 拼 `### <query>\n\n<content>`。

**`web_search` 输出格式**（照 dsh `formatSearchOutput`）：
```
<content>            # 有 answer 时

Sources:
- [label](url) — snippet (publishedAt)    # label=title 非空否则 hostname(url)；meta 可选
...
(Showing the first N sources. Refine the query for more.)   # 仅 truncated 时
Cite the relevant URLs above as markdown links in your answer.
```
无来源且无 content → `No results found.` + cite 提示。

**`web_fetch`**（name `web_fetch`）：
- Schema：`{ type: "object", additionalProperties: false, properties: { url: { type: "string" } }, required: ["url"] }`。
- Execute：`engine.Fetch(ctx, FetchID, {URL})`（外层 `context.WithTimeout(ctx, FetchTimeoutMs)`）。结果 body 截到 `FetchMaxOutputChars`（超了加 `[truncated: showing first N chars]`）。
- 输出：
```
<url>
HTTP <statusCode>

<body>
```
- 非 2xx 正常返回（状态码可见）；错误（ErrInvalidURL/ErrRedirectBlocked/ErrTimeout/ErrProvider/ErrAborted）映射为可读文本。

**D3**：`web_search` 每次查询的 `web/search-request` 已由 DeepSeek provider 的 `OnRequest` 落日志（M7-1）；工具层不重复。`web_fetch` 无独立 web 事件（结果走 `tool/result`）。

## 5. config.go + config.yaml 契约

```go
// WebConfig 是联网能力策略（ADR 2026-08-20-m7-web-search.md）。默认关（D10）：
// disabled 时组合根不创建 Engine、不注册/白名单 web_* 工具。
type WebConfig struct {
    Enabled bool `yaml:"enabled"`

    SearchMaxResults   int    `yaml:"search_max_results"`   // 默认 8
    SearchMaxQueries   int    `yaml:"search_max_queries"`   // 默认 4
    SearchTimeoutMs    int    `yaml:"search_timeout_ms"`    // 默认 30000
    DeepSeek           DeepSeekWebConfig `yaml:"deepseek"`  // 搜索 provider 参数

    FetchTimeoutMs        int    `yaml:"fetch_timeout_ms"`            // 默认 30000
    FetchMaxOutputChars   int    `yaml:"fetch_max_output_chars"`      // 默认 200000
    FetchMaxResponseBytes int    `yaml:"fetch_max_response_bytes"`    // 默认 2097152 (2MiB)
    FetchMaxURLBytes      int    `yaml:"fetch_max_url_bytes"`         // 默认 2048
    FetchMaxRedirects     int    `yaml:"fetch_max_redirects"`         // 默认 5
    FetchUserAgent        string `yaml:"fetch_user_agent"`            // 默认 "shutu-agent/0.1 (M7)"
}

type DeepSeekWebConfig struct {
    BaseURL    string `yaml:"base_url"`     // 默认 https://api.deepseek.com/anthropic/v1
    Model      string `yaml:"model"`        // 默认 deepseek-v4-flash
    APIVersion string `yaml:"api_version"`  // 默认 2023-06-01
    MaxTokens  int    `yaml:"max_tokens"`   // 默认 4096
    MaxUses    int    `yaml:"max_uses"`     // 默认 5
}
```

- `Config` 增加字段 `Web WebConfig \`yaml:"web"\``（放在 `Fs` 后）。
- `applyDefaults`：`web.enabled` true → `cfg.Tools.Enabled = append(..., "web_search", "web_fetch")`（照 fs_*/mcp_* 同款）；各默认值按上表填充（`<=0` → 默认）。
- `var webToolNames = []string{"web_search", "web_fetch"}`（照 fsToolNames 同款，配注释）。
- `config.yaml` 增加 `web:` 段（`enabled: false`，字段中文注释照 fs 段风格，含"默认关 D10"说明）。

## 6. cmd/sta 接线契约

**`cmd/sta/web.go`（新文件）**：
```go
// registerWeb 在 web.enabled 时创建 Engine + provider + 注册 web_* 工具（白名单已由
// config.applyDefaults 加入）；disabled 零操作（D10）。放在 registerFs 之后、
// registerInteracts 之前。所有执行走串行工具路径（D5，无后台 goroutine）。
func (a *app) registerWeb() error {
    if !a.cfg.Web.Enabled { return nil }
    engine := web.NewEngine()
    // 搜索 provider：DEEPSEEK_API_KEY env-only（纪律 6），缺失时 provider 不可用、
    // Search 返回 ErrCredential。
    if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
        onReq := func(ev web.SearchRequestEvent) error {
            if _, err := a.log.Append("web/search-request", ev); err != nil { return err }
            return nil
        }
        sp := web.NewDeepSeekProvider(web.Config{
            APIKey: key,
            BaseURL: ..., Model: ..., APIVersion: ..., MaxTokens: ..., MaxUses: ...,
            OnRequest: onReq,
        })
        _ = engine.RegisterSearchProvider(sp)
    }
    if a.cfg.Web.FetchEnabled 按默认 true（无需单独开关，遵循 web.enabled）:
        fp := web.NewHttpFetchProvider(web.FetchLimits{...from cfg.Web...})
        _ = engine.RegisterFetchProvider(fp)
    wt := web.NewWebTools(engine, web.Options{...}, nil)
    for _, tl := range []tools.Tool{wt.Search(), wt.Fetch()} { a.reg.Register(tl) }
    a.web = engine
    return nil
}
```
（fetch 是否单独开关：为控制 config 面，`web.enabled` 即同时开搜索与抓取；不设 search_enabled/fetch_enabled 独立开关——按 dsh `{search:true, fetch:true}` 语义本项目简化为 web.enabled 总开关。**决策记录于注释**。）

- `app` 增加字段 `web *web.Engine // nil when web disabled (D10)`（无资源，无 deferred Close）。
- `main.go`：在 `registerFs()` 之后、`registerInteracts()` 之前插入 `registerWeb()`（照 M6f 同款注释块）。
- `printHelp` 增加状态行（照 fs 段同款）：
  ```
  if a.cfg.Web.Enabled { fmt.Printf("web: enabled (search_max_results=%d, search_max_queries=%d)\n", ...) } else { fmt.Println("web: disabled (web.enabled=false)") }
  ```
- `/help` 命令表无需新增命令（web 是纯工具能力）。

## 7. 测试要求

- `httpfetch_test.go`（httptest 假服务）：成功 html 抓取分类正确 + body 解码；非 2xx 是结果（StatusCode 保留）；跨源重定向 → `ErrRedirectBlocked`；同源重定向跟随（≤ 上限）；超跳数 → `ErrRedirectBlocked`；超 `MaxResponseBytes` → Truncated；ctx 取消 → `ErrAborted`；非法 URL（非 http/https、超长）→ `ErrInvalidURL`；`classifyContentType`/`parseCharset`/`isSameOrigin` 单测。
- `html_test.go`：标题/链接/粗斜体/代码/代码块/列表/引用/图片/script-style 丢弃/实体解码/未知标签剥离。
- `tools_test.go`：web_search 单查询透传；多查询合并（去重 + round-robin + 截断）；净化（空串/重复）；超 maxQueries 被 D7 拒绝（通过注册表校验路径或 schema 断言）；provider 错误透出；web_fetch 截断输出。工具与 fake Engine（可注入的内存 provider）联测。
- `config_test.go`：web 段默认值；`enabled: true` 白名单追加 `web_search`/`web_fetch`；缺省字段回落默认。
- 组合根测试（若 wiring_test 模式存在）：enabled 时工具注册、disabled 时零注册（照 fs 同款）。

## 8. 提交与报告

- 每模块完成即阶段提交（commit message `M7-2: ...`）。
- 完成后跑 `go vet ./...`、`go test -count=1 ./...`、`go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
