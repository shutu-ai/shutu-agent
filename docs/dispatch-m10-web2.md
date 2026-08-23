# M10 升级 W1 派发：交互核心 + dsh 式聊天工作台前端

> ADR `docs/decisions/2026-08-20-m10-web-workspace.md`（D-WEB2-A~F）。本文件是 **W1** 契约：① cmd/pa `turnMu` 串行 + `runTurn` + `eventHub`；② `internal/webserver` 注入点（消息处理器 / 会话管理器 / 事件源）+ `POST /api/sessions/{id}/message` + `GET /api/sessions/{id}/events/stream`（SSE）+ `POST /api/sessions`（new）+ `POST /api/sessions/{id}/resume`；③ **前端整体重构为 dsh 式聊天工作台（唯一主界面，用户拍板：只读 UI 不需要）**。W2（settings/config API）与 W3（验收收尾）在后续段。

## 纪律
- **loop.go 零改动（D4）**；REPL 与 web 发消息串行（D5，turnMu）；零新依赖、CGO-free、gofmt；只动 internal/webserver、cmd/pa、internal/config（如需）、config.yaml。
- 默认关 D10（web_server.enabled=false 不监听、message/SSE 不注册）；token 空 fail-closed。
- 认证：全部 `/api/*`（含 message、SSE、sessions 管理）走既有 `requireAuth`；静态 shell 公开（`6c18446` 后）。**token 只在 Authorization header**，SSE 前端用 fetch 流解析（不放 URL）。
- 提交 1 个：`W1: Web 聊天工作台（发消息 + SSE 流式 + 会话管理 + dsh 式前端重构）`

## 现状（实施时通读）
- M10a/c/b 已交付（`9592406`/`6cd21b9`/`df3992f` + 认证修复 `6c18446`）：`internal/webserver/webserver.go`（`New(store, token, addr)` + `requireAuth` + `Handler()` + `/api/sessions` + `/api/sessions/{id}/events` + `/api/stats` + `/api/kb` 501 + `/api/health`）+ `static/{index.html,app.js,style.css}`（登录 + 会话列表 + 事件流 + dashboard/kb 占位）+ webserver_test.go（10 用例）+ config `web_server` + cmd/pa `registerWebServer`（goroutine Serve + defer Close）+ printHelp。
- cmd/pa/main.go：`attachSink`（514-519：`a.log.SetSink(func(ev) { return a.store.AppendEvents(ctx, id, []session.Event{ev}) })`，session 切换时调用）、`newLoop()`（537-555：`loop.Config{... OnText: func(delta){fmt.Print(delta)}, OnError: ...}`）、`repl`（558-：`scanner.Scan()` → 579 `a.newLoop().Run(ctx, line)`）、`newSession`/`resumeSession`（507-509 区域）、`currentID`、`store`、`log`、`cfg`。
- `internal/loop`：`Loop.Run(ctx, text)`（每 chunk 落 `assistant/chunk`，loop.go:240）；`onText`/`onError` 回调（237/229）。**不改**。
- `internal/session`：`Event{Seq, Type, At, Version, Data}`；`SetSink` 是 Log 的唯一外部事件钩子（每个 Append 后调用）。
- store：`ListSessions`/`LoadSession`/`CreateSession`。

## 变更清单（精确）

### 1. cmd/pa/main.go — turnMu + runTurn + REPL 改用
- app 结构加 `turnMu sync.Mutex`（并发注解：REPL 与 web 发消息共用，任何时刻至多一个 Run，D5）。
- 新方法：
```go
// runTurn executes one turn under the global serial lock (D5: REPL and web
// share one loop; at most one Run at a time). interactive=false suppresses the
// stdout stream (web renders from the SSE event stream instead — chunk 已落库).
func (a *app) runTurn(ctx context.Context, text string, interactive bool) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	lp := a.newLoop()
	if !interactive {
		lp = a.newLoopWeb() // 见下: OnText 静默
	}
	return lp.Run(ctx, text)
}
```
- `newLoopWeb()`：同 newLoop 但 `OnText: func(string) {}`（静默；SSE 事件流负责回显）、`OnError: func(error) {}`（或 stderr 保留——决策：静默，错误经 SSE tool/error / assistant 落库体现；SSE 端也可推 stream error 事件——实施者按简单：OnError 静默）。
  - 更简洁：`newLoop()` 抽一个 `buildLoop(onText, onError func(string), func(error))`，REPL 传 print，web 传 no-op。实施者任选，保持 REPL 现行为不变。
- `repl` 579 行 `a.newLoop().Run(ctx, line)` → `a.runTurn(ctx, line, true)`（REPL 流式到 stdout 行为不变）。
- `attachSink`（514-519）：sink 里加 `a.hub.Publish(ev)`（见 3），保持 store.AppendEvents 返回语义（sink 错误仍要阻断？——Publish 不返回错误，忽略；store 错误仍返回）。

### 2. cmd/pa/webserver.go — eventHub + 注入接线
- 新 `eventHub`（cmd/pa 内）：`type eventHub struct { mu sync.Mutex; subs map[string]map[chan session.Event]struct{} }`；方法：
  - `Publish(ev session.Event)`：广播给该 sessionID 的所有订阅者 chan（非阻塞：chan 缓冲 256，满则丢订阅者——用 select default，防慢订阅者阻塞 loop 持久化路径；诚实：极端下 SSE 丢事件，前端以快照+后续为准）。
  - `Subscribe(sessionID string) (chan session.Event, func())`：返回 chan + 退订闭包。
  - `NewEventHub() *eventHub`。
- app 字段 `hub *eventHub`（NewEventHub 初始化于 app 构造或 registerWebServer）。
- `registerWebServer()`（现有）：在 `webserver.New` 后注入：
  - `srv.SetMessageHandler(func(ctx, sessionID, text string) error { return a.webMessage(ctx, sessionID, text) })`
  - `srv.SetSessionManager(func(ctx, action, id string) (string, error) { ... new/resume ... })`
  - `srv.SetEventSource(func(sessionID string, sink func(session.Event)) func() { return a.hub.SubscribeInto(sessionID, sink) })`（或 Subscribe 后 goroutine 转发——实施者按 hub 设计简单实现）。
- 新方法 `webMessage(ctx, sessionID, text string) error`：`if sessionID != "" && sessionID != a.currentID { a.resumeSession(ctx, sessionID) }`；`return a.runTurn(ctx, text, false)`。空 text → error。
- **注意**：`a.store`/`a.log`/`a.attachSink` 的 session 切换语义——resumeSession 内部已 attachSink 到新会话（507-509 区域），webMessage 直接复用。

### 3. internal/webserver/webserver.go — 注入点 + message/SSE/session 管理 API
- Server 加字段：`msgFn func(ctx context.Context, sessionID, text string) error`、`sessFn func(ctx context.Context, action, id string) (string, error)`、`evSrc func(sessionID string, sink func(session.Event)) func()`（均可 nil，nil → 对应 API 返回 501）。
- Setter：`SetMessageHandler` / `SetSessionManager` / `SetEventSource`（cmd/pa 在 registerWebServer 调用）。
- 新路由（全在 mux，走 requireAuth）：
  - `POST /api/sessions` → `sessFn(ctx, "new", "")` → 200 `{"id": ...}`；msgFn/sessFn nil → 501。
  - `POST /api/sessions/{id}/resume` → `sessFn(ctx, "resume", id)` → 200 `{"id": ...}`；404/错误 → 对应状态。
  - `POST /api/sessions/{id}/message` body `{"text": "..."}` → `msgFn(ctx, id, text)` → 200 `{"ok":true}`（Run 已完成；前端 SSE 已收到过程）。空 text → 400。msgFn nil → 501。
  - `GET /api/sessions/{id}/events/stream` → **SSE**：写 `Content-Type: text/event-stream`；先 `store.LoadSession(id)` 逐事件发 `data: {json}\n\n`（快照）；再 `evSrc(id, sink)` 订阅，`select` 在 chan/context.Done 上；每事件 `data: ...\n\n` + `retry: 3000`；**每帧可加 `id: <seq>`** 供前端断线续。连接关闭/context 取消 → 退订。evSrc nil → 501（或直接 501，不建流）。**注意**：SSE 端点 handler 内不能 writeJSON（需专用 writer，`http.Flusher` 逐帧 Flush）。
- 事件帧 JSON：`{"seq":N,"type":"...","time":"RFC3339","summary":"..."}`——复用现有 `summarize`（eventView 序列化）。
- **保留**既有 `/api/sessions`（列表）、`/api/sessions/{id}/events`、`/api/stats`、`/api/kb` 501、`/api/health`、`/`、`/static/*`（前端重构后这些 API 仍被聊天工作台内部使用）。

### 4. internal/webserver/static/app.js + style.css + index.html — 前端整体重构为 dsh 式聊天工作台（唯一主界面）
- **去掉**独立只读页导航（sessions 列表页 / 事件流浏览页 / dashboard 页作为主页面）；**唯一主界面** = 聊天工作台：
  - 布局：左侧会话栏（宽 ~260px：会话列表 + 「+ 新建」按钮）+ 主区聊天（消息滚动流 + 底部输入框）+ 顶部栏（当前会话 id / 模型 / provider 显示、设置占位入口、主题切换按钮）。
  - 路由：默认 `#/chat/{id}`（无 id → 第一个会话或新建）；历史 `#/chat` 重定向。
  - 消息流渲染：
    - `user/message` → 右侧气泡（文本）。
    - `assistant/message` → 左侧气泡（文本 + 若有 tool_calls 显示调用列表）。渲染「完整消息」用 SSE 快照 + 新事件；流式期间用 `assistant/chunk` 逐字追加到「当前助手气泡」。
    - `assistant/chunk` → 追加到当前未完成的 assistant 气泡（streaming 状态）。
    - `tool/result` / `tool/error` → 工具卡片（折叠：标题 `<name>`，展开显有界 output；error 红色）。工具卡片插入在触发它的 assistant 气泡之后。
  - **SSE 消费**：`fetch('/api/sessions/{id}/events/stream', {headers:{Authorization:'Bearer '+token}})` → `response.body.getReader()` + `TextDecoder` 按 `\n\n` 拆帧解析 `data: {json}`；收到 chunk 更新 DOM；连接中断自动重连（带 `Last-Event-ID`/seq 续传或简单 3s 重连 + 重新快照）。**不要用 EventSource**（无法带 header，ADR D-WEB2-B）。
  - 输入框：Enter 发送 → `POST /api/sessions/{id}/message {text}` → 乐观插入 user 气泡 → 等 SSE。
  - 会话栏：`GET /api/sessions` 渲染列表（当前高亮）；「+ 新建」→ `POST /api/sessions` → 跳 `#/chat/{id}`；点会话 → `POST /api/sessions/{id}/resume` → 跳转。当前会话 id 存内存/localStorage。
  - 主题：深/浅切换（CSS 变量 + localStorage），默认深色（现风格）。
  - 登录视图保留（token 输入存 localStorage）。
  - `#/kb` 占位页保留（KB 空壳）；`#/settings` 占位（W2 填）。
- style.css：网格布局（侧栏+主区）、气泡、工具卡片、输入框、主题变量。可重写，保持零依赖。

### 5. 测试
- `internal/webserver/webserver_test.go` 新增：
  - `TestMessageRequiresAuth`：POST message 无 token → 401。
  - `TestMessageHandlerInvoked`：注入 fake msgFn（记录 (id,text) 返回 nil）→ POST 200 `{"ok":true}` 且参数正确；空 text → 400；未注入（nil）→ 501。
  - `TestSessionNewResume`：注入 fake sessFn → POST /api/sessions → 200 id；POST resume → 200；nil → 501。
  - `TestEventsStreamSSE`：注入 fake evSrc（sink 收到后推一个 event）+ store 预置会话 → GET stream（httptest）→ 响应 `text/event-stream`，body 含快照事件 + 订阅推送事件帧 `data: {...}`；无 evSrc → 501。（httptest 读 SSE：直接读 Body，无需真流式——fake evSrc 同步推一次即可断言。）
- `cmd/pa` 新增/扩展：
  - `runTurn` 串行测试：fake LLM（scripted）+ 并发两 goroutine 调 `runTurn` → 断言执行串行（可用 LLM 调用计数或 lock 时序；实施者用简单计数 + 并发 sleep 验证不并发）。
  - `webMessage` 测试：fake LLM → `webMessage(ctx, currentID, "hi")` → log 有新 user/message + assistant 事件；`sessionID != currentID` → resume 后执行。
  - `eventHub` 测试：Publish → 订阅者收到；慢订阅者不阻塞（缓冲满丢）。
  - `registerWebServer` 注入断言：enabled + token → `a.webserver` 的 msgFn/sessFn/evSrc 非 nil（加 getter 或同包直接断言字段）。
- **保留**既有 10 个 webserver 用例通过（前端重构不影响后端测试）。

## 验证
`go build ./...` + `go vet ./internal/webserver/ ./cmd/pa/` + `go test -count=1 -timeout 90s ./internal/webserver/ ./cmd/pa/ -run 'Message|SessionNew|SessionResume|EventsStream|RunTurn|WebMessage|EventHub|RegisterWebServer|SSE' -v` 全 PASS 后提交；随后 `go test -count=1 -timeout 90s ./...` 全绿确认。env 同 M10a。

## 环境
- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。
- **警告**：不要用 PowerShell 的 Set-Content/Add-Content 改写含 UTF-8 的文件（会破坏编码 → illegal UTF-8）。改前端/Go 用文件编辑能力写 UTF-8；误改坏就删除重建。
- **尽快产出**：按 1→5 顺序实现，写完核心（1-3）即可先提交一部分再补前端？——**不**：契约要求一次提交。但实现顺序建议 1→2→3（后端+测试可独立验证）→4（前端）→5 测试→提交。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明（hub 丢事件策略、SSE 重连实现、前端布局、注入点实现）。不要贴代码。

---

# M10 升级 W2 派发：设置页 + 脱敏 config API（ADR D-WEB2-D）

> W1 已完成（`5455efa`：发消息 + SSE 流式 + 会话管理 + dsh 式前端重构）。本段在其上追加：`GET /api/config`（脱敏配置视图）+ 前端 `#/settings` 页（配置展示 + 提示重启生效）。主题切换 W1 已做。

## 纪律
- W1 提交（`5455efa`）上工作，工作树干净开始（config.yaml 除外——保持 M 不动不提交）；零新依赖、CGO-free、gofmt；只动 internal/webserver、cmd/pa；不改 loop。
- **永不返回 token/key**（ADR D-WEB2-D）：`GET /api/config` 输出中 `web_server.token` 等敏感字段一律 `"***"` 或省略；key 本就在 env 不在此 config。
- API 走既有 requireAuth；nil 注入 → 501。

## 变更清单

### 1. cmd/pa/webserver.go — SetConfigProvider 注入
- `registerWebServer` 增加 `srv.SetConfigProvider(a.webConfig)`。
- 新方法 `webConfig() map[string]any`：从 `a.cfg` 构造**脱敏**扁平视图：
  - `model`、`base_url`、`llm_provider`、`mode`；
  - 各能力 cap：`terminal/fs/fs_search/ralph/workflow/kb/jobs/subagent/web/eval/code/interact/mcp/skill/schedule/plan/spill/compaction/multimodal` 的 enabled bool；
  - `web_server_addr`；
  - `tools_enabled_count`（len(cfg.Tools.Enabled)）+ `tools_enabled`（列表，最多前 30 个 + "…"）；
  - **不含** `web_server.token`（或置 `"***"`）、任何 key。
- 字段名 snake_case。

### 2. internal/webserver/webserver.go — GET /api/config
- Server 加 `cfgFn func() map[string]any`（nil 默认）+ `SetConfigProvider(fn)`。
- 新路由 `GET /api/config`（requireAuth）：`cfgFn` nil → 501；否则返回 `cfgFn()` 的 JSON（map 直接 `writeJSON`）。

### 3. internal/webserver/static/app.js + index.html — #/settings 页
- `#/settings` 从占位改为真实渲染：`fetch('/api/config')` → 分组展示（模型/provider/mode / 能力开关 / web_server / 工具白名单计数）；**只读**，附提示「修改 config.yaml 后重启生效」。
- 顶栏「设置」入口链接到 `#/settings`；保留主题切换（W1 已有）。
- `#/kb` 仍占位。

### 4. 测试
- `internal/webserver/webserver_test.go`：`TestConfigAPI`——注入 fake cfgFn（含 `web_server.token:"secret"` 键）→ GET 200 且 token 值被 `***` 或缺失（**断言脱敏**）；无 token → 401；nil cfgFn → 501。
- `cmd/pa`：`TestWebConfigRedacts`——构造 cfg（WebServer.Token 非空 + 若干 cap）→ `webConfig()` 返回 map：`web_server.token` 键不存在或值为 `"***"`；cap/mode/model 正确；`registerWebServer` enabled 后 `a.webserver` 的 cfgFn 非 nil（Handlers() getter）。
- 既有 W1/webserver 用例保持通过。

## 验证
`go build ./...` + `go vet ./internal/webserver/ ./cmd/pa/` + `go test -count=1 -timeout 90s ./internal/webserver/ ./cmd/pa/ -run 'Config' -v` 全 PASS 后提交（`W2: 设置页 + 脱敏 config API（GET /api/config）`），再全量 `go test -count=1 -timeout 90s ./...` 全绿。env 同 W1。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明。不要贴代码。

---

# M10 升级 W4 派发：dsh 级工作区（认证已可选；思维链 + 会话模式 + 子代理/后台任务列表 + 设置全功能 + 精致 UI）

> 前置已完成：W1（`5455efa` 交互核心+聊天工作台）、W2（`238a329` 设置+config API）、W3（`d317375` ctx 修复+recover+--web-only）、认证可选（`02d64a8`/`5de9ea2`，ADR D-WEB-2 变更 + D-WEB2-G/H）。本段实现 D-WEB2-H：dsh web 工作区全功能面。

## 纪律
- W4 分**两个提交**（各独立可验证）：
  1. `W4a: 状态 API + events 扩展（/api/subagents、/api/jobs、reasoning/tool_name）`
  2. `W4b: dsh 式工作区前端大重构（思维链 + 会话模式 + 侧栏 tabs + 设置全功能 + 精致 UI）`
- loop.go 零改动（D4）；认证已可选（token 空直开，D-WEB2-G——不要加回强制登录）；只动 internal/webserver、cmd/pa、static/；零新依赖、CGO-free、gofmt。
- 状态 API 只读 + **脱敏**（D7）：不暴露密钥、会话事件正文、完整输出；只给 id/状态/时间/标签/摘要。例外：user/assistant 消息正文完整返回（前端整条渲染，dsh 行为）——消息文本是 UI 必显数据，不是日志正文泄露面。
- 提交用 `git add` 明确列文件（config.yaml 保持 M 不动不提交）。

## 现状（实施时通读）
- `internal/webserver/webserver.go`：`Server{store, tokenHash, authOn, addr, srv, msgFn, sessFn, evSrc, cfgFn}` + `New(store, token, addr)`（token 空 → authOn=false 直开）+ `requireAuth`（authOn false 放行，panicSafeWriter recover）+ `Handlers()` getter + 路由（sessions/events/stats/kb/message/resume/config/SSE）。
- `eventView{Seq,Type,Time,Summary}`（events API 每事件视图）；`summarize(ev)`（有界文本；user/assistant 消息正文例外，完整返回）。
- 事件 Data 的 JSON tag 是公开契约（session.go）：`assistantMessageData{Text, ToolCalls, FinishReason, Reasoning json:"reasoning,omitempty"}`、`toolResultData{CallID, Name, Output, Spill}`、`toolErrorData{...}`——webserver 可自建同构 struct Unmarshal 提取（不依赖 session 内部类型）。
- `cmd/pa/main.go` app 字段：`jobs *jobs.Local`（nil 当 jobs 关）、`subagents subagent.Runtime`（nil 当 subagent 关）、`currentID`。
- `internal/jobs`：`(*Local).List(ctx, callerSession) ([]JobSnapshot, error)`；`JobSnapshot{ID, Kind, Label, OwnerSession, Status, Detail, StartedAt, FinishedAt, OutputLimitBytes}`。
- `internal/subagent`：`Runtime.ListChildren(ctx, parentSessionID) ([]ChildSummary, error)`；ChildSummary 字段见 `internal/subagent/service.go`。
- 前端 `static/{index.html,app.js,style.css}`：现为 dsh 式聊天工作台（侧栏会话 + 气泡 + 工具卡片 + SSE fetch 流 + 主题 + #/settings）；登录视图已按 D-WEB2-G 改为「默认直进，401 才提示」。

## 提交 1：W4a 后端状态 API + events 扩展

### 1. internal/webserver — events 扩展 + 两个状态路由 + 注入点
- `eventView` 加字段：
  - `Reasoning string \`json:"reasoning,omitempty"\``：仅 `assistant/message` 时从 Data 的 `reasoning` 提取（有界 `maxSummary` runes；空则省略）。
  - `ToolName string \`json:"tool_name,omitempty"\``：`tool/result`、`tool/error` 时从 Data 的 `name` 提取（工具卡片标题）。
  - `ToolOutput string \`json:"tool_output,omitempty"\``：`tool/result` 时从 Data 的 `output` 提取（有界 maxSummary；工具卡片展开内容，替代/补充 summary）。
  - 实现：webserver 内自建 `type evAssistant struct{ Reasoning string \`json:"reasoning"\` }`、`type evTool struct{ Name, Output string }` 等，按 Type 分支 `json.Unmarshal(ev.Data, &x)` 提取。保持 `summarize` 行为不变（兼容）。
- 新注入字段 + Setter：
  - `subFn func(ctx context.Context) ([]map[string]any, error)` + `SetSubagentProvider(fn)`。
  - `jobsFn func(ctx context.Context) ([]map[string]any, error)` + `SetJobsProvider(fn)`。
  - 加入 `Handlers()` getter（Subagents / Jobs 字段）。
- 新路由（requireAuth）：
  - `GET /api/subagents` → subFn nil → 501；否则返回 `{"subagents": [...]}`。
  - `GET /api/jobs` → jobsFn nil → 501；否则返回 `{"jobs": [...]}`。

### 2. cmd/pa/webserver.go — 注入脱敏 providers
- `registerWebServer` 加：
  - `srv.SetSubagentProvider(a.webSubagents)`
  - `srv.SetJobsProvider(a.webJobs)`
- `webSubagents(ctx) ([]map[string]any, error)`：`if a.subagents == nil { return []map[string]any{}, nil }`（关 → 空数组）；`a.subagents.ListChildren(ctx, a.currentID)` → 每 ChildSummary 脱敏视图 `{id, name, status, created_at}`（字段名按 ChildSummary 实际字段映射；**不**含 prompt/输出）。ListChildren 错误 → 返回 err。
- `webJobs(ctx) ([]map[string]any, error)`：`if a.jobs == nil { return []map[string]any{}, nil }`；`a.jobs.List(ctx, a.currentID)` → 每 JobSnapshot `{id, kind, label, status, detail, started_at, finished_at}`（**不**含 OwnerSession 细节、输出内容）。

### 3. 测试（W4a）
- `internal/webserver/webserver_test.go`：
  - `TestEventsExtendedFields`：seed `assistant/message`（Data 含 reasoning）+ `tool/result`（Data 含 name/output）→ GET events → 断言 eventView 含 reasoning/tool_name/tool_output（有界）。
  - `TestSubagentsJobsAPI`：注入 fake subFn/jobsFn → 200 返回数组；无 token（配置 token 时）401；nil → 501。
- `cmd/pa`：`TestWebSubagentsJobsProviders`——a.subagents/a.jobs 为 nil → 空数组不报错；注入断言（Handlers().Subagents/Jobs 非 nil）。

## 提交 2：W4b 前端 dsh 式工作区大重构（static/）

### 布局（dsh web 工作区对齐）
- **左侧面板**（~280px，可折叠）：三个 tab——**会话**（列表 + 新建/恢复/切换，当前高亮）/ **子代理**（GET /api/subagents 渲染：id/状态/时间，5s 轮询）/ **后台任务**（GET /api/jobs 渲染：id/kind/状态/时间，5s 轮询）。
- **主聊天区**：消息流 + 底部输入框（Enter 发送）+ 空态引导。
- **顶部栏**：当前会话 id、**会话模式徽标**（`/api/config` 的 mode：standard/minimal/code，badge 颜色区分；提示「改 config.yaml 重启生效」tooltip）、模型·provider（config API）、主题切换（深/浅）、⚙ 设置入口。
- `#/chat/{id}` 默认工作台；`#/settings` 完整设置页；`#/kb` 占位保留。

### 消息流全元素
- `user/message` 右侧气泡（文本 + 时间戳）；`assistant/message` 左侧气泡（时间戳）。
- **思维链**：`assistant/message` 的 `reasoning` 非空 → 折叠块「💭 思考过程」（点击展开，等宽/浅色块，有界）；流式期间若出现 reasoning 用同样折叠块。
- **工具卡片**：`tool/result`/`tool/error` → 折叠卡片（标题 `<tool_name>` + 状态徽标；展开显示有界 output；error 红色）。
- **流式**：`assistant/chunk` 逐字进当前 assistant 气泡（现有逻辑保留/增强）。
- 时间戳格式化（HH:MM:SS）健壮（Invalid Date 守卫）。

### 设置全功能（#/settings）
- `/api/config` 分组渲染：模型/provider/mode / 19 个能力开关（enabled ✓/✗）/ web_server / tools 白名单（计数+列表）/ base_url；只读 + 「修改 config.yaml 后重启生效」提示；主题切换。

### 视觉（精致深色，对齐 dsh 观感）
- CSS 变量主题（--bg/--surface/--border/--text/--muted/--accent/--danger…），深色为默认；浅色变量同套。
- 卡片/气泡圆角 + 细边框 + 柔和阴影；侧栏 tab 激活态；滚动条样式；工具卡片/思维链块 hover 态；输入框聚焦 ring；整体间距节奏统一。
- 动画克制（气泡入场 fade/slide、流式光标、面板切换过渡）。
- 零外部资源（无图标库/无字体 CDN；用 unicode 符号如 💭/⚙/🔍/+）。

### 前端逻辑（app.js）
- 复用现有 fetch 封装（api()）、SSE fetch 流解析（fetch+getReader，token 可选）、路由。
- 新增：侧栏 tab 切换 + 子代理/任务轮询渲染（AbortController 防泄漏）、思维链/工具卡片渲染、mode 徽标、设置分组渲染。
- 保持：默认直进（无 token 不弹登录；401 → showLogin）、主题记忆（localStorage）、会话新建/恢复/切换。

## 验证（每提交后）
- `go build ./...` + `go vet ./internal/webserver/ ./cmd/pa/` + `go test -count=1 -timeout 90s ./internal/webserver/ ./cmd/pa/ -run 'Events|Subagents|Jobs|Config|Web' -v` 全 PASS 后提交对应提交；两提交完成后全量 `go test -count=1 -timeout 90s ./...` 全绿确认。
- 前端：无构建验证（静态文件直接读）；自查 DOM id 一致、无 JS 语法错误（可用 node --check 若环境有；无则人工核对）。

## 环境
- Go：`C:\Program Files\Go\bin\go.exe`；env（每个 go 命令都要设）：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。
- **警告**：不要用 PowerShell Set-Content/Add-Content 改 UTF-8 文件（破坏编码）；改文件用文件编辑能力；误改坏删除重建。前端文件建议整文件重写时用文件编辑（write 全量）保证 UTF-8。

## 报告（简短）
两个提交 hash + go test 结果 + 前端改动要点 + 偏离说明。不要贴代码。

---

# W4-REV 逐页移植 dsh web 工作台（用户指示 2026-08-20：参照 `D:\dev-projects\Agent\deepseek-harness` 源码逐页复制）

> **取代上面的原 W4 契约。** dsh web 源码位置：`D:\dev-projects\Agent\deepseek-harness\packages\client\<ui-*>\src\client\`（400 个源码文件已检出，**只读参照，绝不修改**）。dsh web 是三栏工作台：`ui-layout/AppFrame.tsx`（sidebar | conversation | details，可拖拽 + 窄屏自动折叠）+ 各 `ui-*` 模块。移植原则：**页面结构/布局/功能/视觉/中文文案对齐 dsh**，实现语言为 vanilla JS + Go 后端（React 代码不照搬——架构零依赖纪律）。

## 页面移植清单（逐页）

| 页 | dsh 源码 | github.com/jabing/shutu-agent 移植内容 | 后端依赖 |
|---|---|---|---|
| **P1 工作台布局 + 聊天核心页** | `ui-layout/AppFrame.tsx` + `ui-conversation/src/client/{chat/*, skeleton/*, queue/*}` | 三栏框架（可拖拽/窄屏折叠）+ 会话消息流（MessageItem/ReasoningRow/ToolNode/ContextMeter/EmptyHero）+ 输入栏（InputBar/自动增高/Enter 发送）+ 队列 dock + 会话顶栏 | sessions/events(+reasoning/tool)/message/resume/SSE（已就绪 + 下方 W4a 扩展） |
| **P2 侧栏 + 会话管理** | `ui-sidebar` + `ui-conversation/src/client/stores.ts` | 左侧会话栏（列表/新建/恢复/切换/标题/时间，当前高亮） | sessions 列表 |
| **P3 设置 + 模型选择** | `ui-settings` + `ui-settings-general` + `ui-settings-models` + `ui-model-selection` | 设置页（模型/provider/mode/caps/tools/web）+ 模型选择器 | /api/config（已就绪，可扩展） |
| **P4 子代理 + 后台任务** | `ui-subagent` + `ui-jobs` | 子代理列表 + 后台任务列表（状态徽标/时间） | /api/subagents + /api/jobs（下方 W4a） |
| **P5 主题 + 反馈 + 附件** | `ui-theme/src/styles`（--dsw-* token）+ `ui-message-feedback` + `ui-attachment` | 深/浅主题 token 对齐 + 消息反馈按钮 + 图片/附件显示 | token 对齐前端；feedback 需新 API（可选）；attachment 需 events 含图片 |

**架构排除**（github.com/jabing/shutu-agent 无 Cordis/Slots/运行时插件，用户已拍板）：`ui-slots`、`ui-settings-plugins`、`ui-settings-plugin-inventory`、`ui-renderer` 动态机制、`ui-commands` popup 服务面、`ui-permission-presets`、`ui-directory-picker-*`（依赖沙箱 FS 服务）。

## 前置：W4a 后端（events 扩展 + 子代理/任务 API）
按上面「提交 1：W4a」实施（eventView 加 reasoning/tool_name/tool_output；`GET /api/subagents` + `GET /api/jobs` 注入 provider nil→501；cmd/pa webSubagents/webJobs 脱敏）。

## 实施纪律（每页）
- **每页一个提交**（`P1: …`…`P5: …`），提交前 `go build ./...` + `go test -count=1 -timeout 90s ./...` 全绿（后端如有改动）+ 前端自查（DOM id 一致、无 JS 语法错）。
- 先由**研究规格**驱动：读 dsh 对应 `ui-*` 源码 → 产出该页移植规格（布局/DOM 层级/组件外观/交互/中文文案/数据需求）→ 再写 vanilla JS。规格进 `D:\dev-projects\Agent\shutu-agent\.web-port/`（供控制器验收与后续页复用）。
- 视觉：CSS 变量主题对齐 dsh `--dsw-*` token 语义（见 `ui-theme/src/styles/`）；零外部资源（无图标库/字体 CDN，unicode 符号）；产品文案中文。
- loop.go 零改动（D4）；认证默认直开已生效（D-WEB2-G，不要加回登录）；`config.yaml` 保持 M 不提交；零新依赖；CGO-free；gofmt。

## 环境
- Go：`C:\Program Files\Go\bin\go.exe`；env（每个 go 命令都要设）：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。
- **警告**：不要用 PowerShell Set-Content/Add-Content 改 UTF-8 文件（破坏编码）；前端文件整文件重写用文件编辑（write 全量）保证 UTF-8；中文产品文案保持 UTF-8。

## 报告（简短）
每页提交 hash + go test 结果 + 移植要点 + 偏离说明。不要贴代码。