# Agent.md — 数驼 AI Agent 项目全局规划

> 本文是项目工作入口：状态、路线图、开发纪律都在这里。
> 设计基线在 [`docs/design.md`](docs/design.md)——**改设计先改那里，再改代码**。

---

## 0. 最高纪律（2026-08-21 用户定）

> **任务 UI 与功能一律按 dsh 对齐；功能的减少或修改必须先确认。**

1. **UI 对齐 dsh**：所有 Web 界面（布局、交互、文案、主题、行为）默认以 dsh 为唯一参照源；实现或修改前先读 dsh 对应源码产出规格，按规格对齐，不凭印象自由发挥。
2. **功能的减少或修改必须先确认**：任何「去掉已有功能 / 改变既有行为 / 缩水实现」在动手前必须向用户确认；未经确认不得自作主张缩减或改动。确认过的改动在提交说明中注明「已确认」。
3. **新增功能同样先对照 dsh**：先查 dsh 是否已有对应实现/形态，再决定「照抄 / 借鉴 / 自定义」，并把结论写进规格或决策记录。
4. **偏差必须显式记录**：与 dsh 的任何偏差（哪怕看似"简化"）都视为待确认项，不得默默偏离。

---

## 1. 项目定位

Go 实现、借鉴 DeepSeek Harness 架构的个人 Agent：薄核心（会话日志 + LLM 适配 + 工具注册表 + 提示词组装 + 循环），后期以"能力接缝"方式接入个人知识库（RAG）。参考实现：`../deepseek-harness`（重点读 `docs/architecture.md`、`docs/subsystems/core.md`、`docs/subsystems/session.md`）。

**两条参照原则（用户定，2026-08-20）**：
1. **Agent 功能**（循环/会话/LLM/工具/各能力族）参考 **dsh-harness** → `../deepseek-harness`；
2. **KB** 不属于本项目实施范围，由另一个项目负责。
二者源码均已在本项目根目录同级子目录中，无需另行下载。

## 2. 设计基线（防漂移摘要，细节见 design.md）

- **D1** 会话 = 追加式事件日志，历史是派生值，永不另存。
- **D2** 新能力 = Service / Provider / Tool 三件套，消费方只依赖接口。
- **D3** 模型可见 ⇒ 已落日志；新模型可见输入 ⇒ 新事件类型。
- **D4** 薄核心；v1 用 Go 接口 + 注册表，不引入插件系统/事件总线。
- **D5** 循环串行同步；并发、后台任务推迟到 M5（有明确用例才做）。
- **D6** LLM 适配器第一天就支持 SSE 流式。
- **D7** 工具参数在 Execute 入口统一 JSON Schema 校验。
- **D8** 持久化走 store 接口（SQLite 后端，CGO-free），事件带版本号。
- **D9** 知识库是能力接缝（kb service + 可换 Provider + kb_* 工具）；检索与对话模型解耦（M4 用 FTS5 全文检索 + 提取回写，向量/embedding 仅作 M5 可选 Provider 预留）。
- **D10** 安全白名单先行；执行类工具（bash 等）M3 才上。

## 3. 当前状态

**2026-08-18**：M3 完成并通过验收（提交 `1dda2ed`，ADR `2026-08-18-m3-sandbox-scope`）。M4 知识库**三段全部完成并通过验收**：M4a 内核（`682f07e`）、M4b 工具与召回（`bdd903d`）、M4c 提取回写（`5e98fa7`）。方向：**参照 dsh-knowledge（已下载 `../dsh-knowledge`，FTS5 全文检索 + 提取回写，非向量 RAG，方案已实测）**，调研见 `docs/research-m4-kb.md`，派发见 `docs/dispatch-m4a/b/c.md`，ADR `2026-08-18-m4-kb-architecture.md` 完整定稿七项决策。

**2026-08-19**：M5 核心能力启动（用户拍板"必须、先实现"；ADR `2026-08-18-m5-agent-core.md` 定稿四段决策）。**M5 四段全部完成并通过验收**（每段由控制会话亲自 vet/test/build 验收、对照 D1–D10 审 diff）：M5a 后台任务（M5a-1 `34bf1e8`+`5f3abd4`、M5a-2 `4c0a25e`+`dbe07fc`+`b1d3535`+`6d91af7`）→ M5b 子代理（`55f1b63`+`34c302c`+`8a3f648`、`8c7f1b3`+`070039e`+`27adaca`+`e8dcec0`+`78fd6a6`）→ M5c 上下文压缩（`76c41db`、`2188b4d`+`0ffa4e4`+`4669c2e`、`a5219ac`、`c4b5e88`+`e9b2b9c`）→ M5d 技能（`b2d93fc`+`453b288`+`0c38de5`+`400e06c`+`75d892c`、`cb09853`+`17cfe10`+`935ffdc`+`6859000`+`07a82ce`）。

**2026-08-19（续）**：与 dsh 差距评估——M5 后除知识库/Web 接口外，个人 Agent 的实质能力缺口为：定时调度、任务规划、长期记忆、人工审批（任务类）+ 代码沙箱、工具生态/fs 封装（代码类）。用户拍板"需要补全"→ 定稿 **M6 能力补全六段**（ADR `2026-08-19-m6-agent-full.md`）：M6a 定时调度 → M6b 任务规划 → M6c 长期记忆 → M6d 人工审批 → M6e 代码沙箱 → M6f 工具生态。全部接缝挂薄核心（D4）、默认关（D10）、零新依赖（M6f MCP 自实现 JSON-RPC over stdio）。**M6 六段全部完成并通过验收**（M6a `85cd9a3`…`3fb43fd`；M6b `e006a9e`…`512896f`；M6c `b087b22`…`717ed92`；M6d `6d32daa`…`0118169`；M6e `a66d33e`…`cb39660`；M6f `764261c`…`f20ae3b`）。

**2026-08-20 冒烟**：真实端到端冒烟（pa.exe 全链路，用户拍板"①"）。A. 无 API key 启动正确报错退出（env-only 约束生效）；发现 `data/pa.db` 留存 M4 时期的真实运行会话（4 轮对话 + `kb/extract`/`kb/recall`/`kb/add`/`tool/result`，D1 落库曾实测）。B. 真实对话（DeepSeek 流式）成功——/help 完整命令表 + 中文回答，会话 332→375 事件落库、重启恢复（resumed session）验证通过。C. 临时 config 启用 fs：模型真实调用 `fs_write` 创建 `smoke-c.txt` + `fs_read` 读回，`fs/write`/`fs/read`/`tool/result` 事件全部落库（工具注册→白名单→D3 事件→D7 校验→执行→落库整链路实测）。冒烟产物已清理（`.smoke/`、`pa-smoke.exe`、`smoke-c.txt`），工作树干净。

**2026-08-20 rc.8 评估**：检查 deepseek-harness 上游更新（本地克隆 rc.7→rc.8，`dsh-v0.1.0-rc.8`，浅克隆 `141eb6f`）。逐项评估后**无必须跟进项**：① SQLite chunk-row 压缩（93b4b98，250 万逻辑事件→6.6 万物理行、体积降 89%）与本项目逐 chunk 落库的行放大同源，但个人规模量级差太大（实测 pa.db 73KB、chunk 占 93% 行但每条 data 仅 16B；5-10 年估算 ~100MB–1GB）、跟进需 Zstandard 第三方依赖，暂缓（触发条件：单库 >数 GB 或可感知变慢；更轻替代：WAL+VACUUM → 会话归档 → 完整 message 落库+断流恢复，而非 chunk packing）；② DeepSeek reasoning 回传（583894f）单 provider 无网关时不必要，但**做多 provider 后成为必要**（并入 M8）。用户拍板：**web 搜索列入路线图**（见 §4 候选小节），后端走 **DeepSeek 官方搜索**（dsh `packages/web/web-search-deepseek/`：Anthropic 兼容 Messages API `POST /anthropic/v1/messages` + 原生 `web_search_20250305` server tool，复用 `DEEPSEEK_API_KEY` 零新密钥，服务器端搜索返回结构化 `web_search_tool_result`，代价=每次搜索一次完整模型调用）。

**2026-08-20 路线图决策**：用户拍板四项列为候选（见 §4）：**pwsh persistent PTY**（dsh rc.8 新增 `tool-pwsh-persistent`，owner-scoped 持久 shell，cwd/env/函数跨调用保留）、**多 LLM provider**（必做）、**deepseek reasoning 回传**（依附多 provider：跨 provider 重编码会话时需要，`llm.Message` 需带 `reasoning_content` 落库并回传）、**多模态**（必要；dsh `7078918` 范式：落库只存 `ImageAttachmentRef`、data URL 仅请求时存在、20MiB 上限、模型能力按 exact-model `inputModalities` 声明）。**组织判断**：多 provider + reasoning 回传 + 多模态三件都改 `llm.Message` 消息模型与 wire 层，打包为 **M8 消息模型升级** 一次设计，避免改三次；persistent PTY 独立为 **M9**。

**2026-08-20 Web 门户决策（翻转"Web 延后"）**：用户拍板——知识库 Web 管理界面 + dsh web 功能都需要，目标是完整的个人工作台（知识库查询、真实解决问题、业务数据查询、写脚本、dashboard 可视化）。**翻转** M1 `design.md:19`"第一版就做 Web UI → REPL 先磨循环"、M3 dispatch"Web 明确不做"、M6 ADR"remote API/SDK 暂缓"三条历史决策。落地形态（见 §4 候选）：**KB 全量**（dsh-knowledge 核心功能层，不含 web 层——web 层由本项目自建）+ **M10 Web 门户**（webServer 基础设施 → 知识库 Web 管理台 → dashboard 工作台）。dsh-knowledge 的 web 层依赖 DSH `webServer`/Client Slots 平台，本项目无此平台，故**借鉴其功能面、用 Go 标准库自建**（零新依赖）。**用户定序（同日）**：先把 Agent 部分（M7 web 搜索 → M8 消息模型升级 → M9 persistent PTY）完成，再做 KB 全量（内容层），最后 M10 Web 门户（呈现层，管理台依赖 KB 全量）。

**2026-08-20 M7 完成（web 搜索，ADR `docs/decisions/2026-08-20-m7-web-search.md`）**：用户拍板"开始"。分两 half 派发实施（dispatch-m7 / dispatch-m7-2），控制面验收通过（vet 0 / build 0 / 22 包 test 全绿；loop.go 未动、go.mod 零 diff、零新依赖）。交付：`internal/web` 接缝（SearchProvider/FetchProvider 接口 + Engine 注册表/选择/返回路径 maxResults 截断 + 8 个 WebError sentinel）+ **DeepSeek 官方搜索 provider**（Anthropic 兼容 Messages API 非流式 POST + `web_search_20250305` server tool + 解析 `web_search_tool_result` blocks + citationSnippets + url 去重 + 无 result block fail-closed，复用 `DEEPSEEK_API_KEY` env-only）+ **HttpFetchProvider**（URL 校验、同源重定向、超时、字节/字符上限、content-type 分类、UTF-8）+ **轻量 HTML→Markdown**（零依赖手写扫描器）+ `web_search`（多查询**顺序扇出** D5、round-robin 合并、去重、截断、D7 maxItems）+ `web_fetch` + `web/search-request` 事件（D3，OnRequest 派发前落库 secret-free）+ config（`web.enabled` 默认关 D10，白名单自动追加）。**遗留**：真实 key 冒烟待用户 rotate `DEEPSEEK_API_KEY` 后执行（离线单测已覆盖 provider 全部行为）。

**2026-08-20 编译期插件边界确认（架构方向）**：用户拍板——**不做运行时插件能力**（D4 保持：薄核心、Go 接口 + 注册表、无插件系统；硬事实：Go `buildmode=plugin` 不支持 Windows），**后续新增能力一律走编译期接缝**（Service 定义 + Provider 后端 + Tool 消费方三件套），知识库、数据连接分析等未来能力都以此方式扩展，不影响后期功能扩展。外部工具生态经 MCP（M6f ✅）进程外接入；知识库换检索后端经 D9 Provider 接缝。数据连接分析（sqlite/CSV/HTTP API 数据源 + 查询/分析工具）定位为未来独立接缝候选（`data` 接缝，或经 MCP server 接入），不属本决策的运行时插件范畴。

**2026-08-20 任务评测接缝列候选（对照 dsh 的闭环差距）**：用户问"Agent 能力实现后能否像 dsh 一样做复杂任务分解/派发/评测"。核对结论：**分解**（M6b plan：goal→plan→todo + `plan_*`）与**派发**（M5b subagent 委托/控制/报告 + M5a jobs 后台 + M6a schedule 定时）**已是完成的闭环**；**评测是真正缺口**——当前只有 子代理自报报告 + 父模型判断 + `interact` 人工审批，**无独立自动评测接缝**（dsh 同样无开箱即用 eval，评测主要报告驱动，非其专有能力）。用户拍板：**记入候选**（编译期接缝，非运行时插件）。设计方向：`eval` 接缝（EvalProvider：规则断言 / LLM judge / 人工回退 + `eval_*` 工具 + `eval/*` 事件 + config 默认关 D10）；**评测标准来源 = `plan_todo` 条目带验收标准字段 → 派发随任务传给子代理 → 评测器对照验收标准**（规则断言优先、LLM judge 兜底、无法自动判定落 `interact` 人工），形成"分解→派发→评测→（不合格重派）"闭环。**建议排期：M9 之后、KB 全量之前**（可在 M8/M9 捎带，因引用 `llm.Message` 与子代理域）。

**2026-08-20 M8-1 段完成（消息模型 content parts + reasoning 落库回传，ADR `docs/decisions/2026-08-20-m8-message-model.md`）**：M8 三段第一段派发并验收通过（dispatch-m8；4 阶段提交 `4fcc57d`/`f3aab40`/`3bcfd35`/`45b6050`）。交付：`internal/llm` `Message.Content` 从 string 升级为 `[]ContentBlock`（text/reasoning/image-ref 预置 + `Text()/SetText()/Reasoning()/HasImage()` helper）；`StreamEvent` 增 `StreamReasoningDelta` + `Finish.Reasoning`；deepseek wire `reasoning_content` 序列化 + SSE `reasoning_content` delta 解析；`assistant/message` 事件增 `reasoning` 载荷、`user/message` 预留 `content` blocks（M8-3）；`DeriveHistory` 折叠 reasoning 前置、旧格式纯字符串回放包 text block（D8）；全使用方一次性迁移（loop/compaction/compact/kb/skills/extract/subagent + 测试断言 `m.Text()`）。验收：vet/build 0、22 包 test 全绿（复跑确认；首跑 cmd/pa kb 测试偶发 flaky，复跑 3 次全绿，判定与 M8-1 无因果）；loop.go turn/step 未动（D4）；零新依赖。**遗留**：kb 既有测试偶发 flaky 现象记录（待复现再深挖）。

**2026-08-20 M8-2 段完成（多 provider 注册表 + anthropic provider）**：M8 三段第二段分两个子派发并验收通过（dispatch-m8-2 / dispatch-m8-2b）。**M8-2a**（6 提交 `983b2f6`~`20ed3d3`）：`llm.Provider` 接口 + `Registry`（D2 三件套）、`ChatRequest.Provider` 字段、deepseek 适配（ID/Available）、openai provider（复用 deepseek 的 OpenAI 兼容 SSE，零新 wire）、`config.LLMConfig{provider + openai/anthropic 子段}`（顶层 model/base_url 保留为 deepseek 默认）、`cmd/pa/registerLLM`（deepseek 恒注册 + openai/anthropic 按 key 非空注册 → 按 cfg 取 provider 注入 loop，未知/不可用 fail-closed 启动报错——启动凭证门 provider-aware）+ `/llm-status`。**M8-2b**（3 提交 `39670d7`~`10f41b3`）：`internal/llm/anthropic` provider（Messages API 流式 + tool use + thinking reasoning 回传；序列化 system 提取/thinking 前置/tool_result 归并原位输出/`_raw` 兜底；SSE 事件映射 input_json_delta 累积；无 message_stop 报错；HTTP 复用 M7 心智）。验收：vet/build 0、24 包含 anthropic 全绿；loop.go 未动（D4）；go.mod 零 diff；默认 deepseek 回归。**剩余 M8-3 多模态**。

**2026-08-20 M8 里程碑完成（消息模型升级，ADR `docs/decisions/2026-08-20-m8-message-model.md`）**：三段（M8-1/M8-2/M8-3）全部派发并逐段验收通过，三段验收标准（ADR §10）全达标。**M8-3 段**（M8-3a `c137a21`~`764ddbc` + M8-3b `a221a15`~`c3914e4`）：`internal/attachment` 图片附件存储（SaveImage/Read，ref-only 落库）+ `/attach <path>` 命令（扩展名/大小校验 fail-closed，落 user/message image block 只存 ImageRef，默认关 D10）+ 多模态 config（model_input_modalities 默认 text、multimodal.enabled 默认 false、max_image_bytes 10MiB、max_request_image_bytes 20MiB）+ 三 provider 图片序列化（deepseek/openai content parts array `image_url`+data URL、anthropic `{type:image,source:{base64}}`，请求时读 Path 转 base64）+ `OffloadRequestImages`（历史顺序累计、超限最老替换 `[image omitted]` 占位、嵌套递归）+ 纯文本模型遇图片 fail-closed（先检查后 offload 再序列化）。M8 验收：vet/build 0、25 包 test 全绿；loop.go 全程未动（D4）；go.mod 零 diff；旧会话回放不回归（D8）；provider 切换 reasoning 保留。**待办**：M8 real-key 冒烟（图片 + anthropic）待 DEEPSEEK_API_KEY 轮换；**下一步 M9 持久 PTY**。

**2026-08-20 M9 里程碑完成（持久终端，ADR `docs/decisions/2026-08-20-m9-terminal.md`）**：用户拍板"开始"后分两 half 派发（dispatch-m9 / dispatch-m9-2）。**Windows-first**：Go 无 ConPTY（`creack/pty` 无 Windows 支持），故用 `cmd.exe /Q` + 管道实现持久 shell 伪终端，诚实记录限制（无全屏 TUI、Ctrl+C 仅 `\x03` 尽力而为、孙进程残留为文档化残余风险）。**M9-1**（3 提交 `c0b973e`/`b78f694`/`c8d9b1a`）：`internal/terminal` 三段——`BoundedTextBuffer`（有界字节+行缓冲、UTF-8 安全保尾截断、truncated/Consume 语义）、`Session`（子进程管道 + 泵 goroutine + wait goroutine + 就绪判定 stdin_read/timeout/session_exit + Close 幂等进程树终止 + scrubbedEnv 凭证过滤）、真实子进程测试（echo/cwd 跨命令保持/Read/Consume/timeout/exit/stop/scrubbedEnv，8 用例）。**M9-2**（6 提交 `aa5fe1e`~`ddd4fdb`）：config `terminal:` 段（默认关 D10 + 工具自动白名单）+ session `terminal/start`/`terminal/stop` D3 事件（只记元数据，不落输出正文）+ 五件套工具（`terminal_start`/`write`/`read`/`signal`/`stop`，TerminalAccess owner 围栏单活跃会话 D5）+ `/term` REPL 命令组（复用同一套工具）+ cmd/pa 接线（printHelp/生命周期 defer 关闭）+ 接线测试（D10 门 + 真实 cmd.exe 会话 E2E + owner 围栏）。M9 验收：vet/build 0、26 包 test 全绿（全量并发时 terminal 真实子进程测试偶发时序 flaky，重跑稳定，与既往 kb 测试 flaky 同款，记录无因果）；loop.go 全程未动（D4）；零新依赖；CGO-free。**遗留**：真实 key 冒烟待 rotate 后补（M7/M8/M9 共用 DEEPSEEK_API_KEY）。**下一步**：任务评测接缝（M9 之后、KB 全量之前）→ KB 全量 → M10。

**2026-08-20 评测接缝里程碑完成（任务评测，ADR `docs/decisions/2026-08-20-eval-seam.md`）**：用户拍板"开始"（此前 `893073a` 列候选）后分 6 段派发（Eval-1a/1b/2a/2b/3a/3b）并逐段验收。**设计**（D-EVAL-1~7）：评测标准来源 = `plan_todo` 条目带 `acceptance` 验收标准字段（Eval-2a：Todo.Acceptance + AddTodo + plan_todo + plan/create 事件载荷）→ `subagent_spawn` 带 `acceptance_criteria` 注入子代理 prompt 尾部"验收标准（交付自检）"段（Eval-2b：StartRequest + spawn withAcceptance）；评测器（Eval-1a/1b）——`internal/eval` Evaluator 接口 + 四实现（Rule 规则断言 contains/not 确定性优先、LLM judge 兜底、Manual 人工回退、Composite 编排：规则违例即 fail → manual 条目落人工 → llm 条目走 judge → 无条目且全过 pass）+ Engine + mem 历史存储（上限淘汰）+ `eval_run`/`eval_result`/`eval_list` 工具 + `eval/run` D3 事件（只记摘要不落输出全文，D-EVAL-5）+ config（默认关 D10，manual_fallback 默认 true 用 *bool 表达、max_records 100）+ cmd/pa 接线（registerEval 用 `a.llm` 的 LLM judge 闭包 + `a.interacts` 的人工回退闭包 approved→pass/rejected→fail + `/eval-status` + 生命周期）。验收：vet/build 0、26 包 test 全绿（全量首跑撞既有 flaky `TestRegisterCodePolicyDeadlineBoundsSandboxRun` 时序敏感，连跑 2 次 PASS，非本里程碑因果）；loop.go 全程未动（D4）；零新依赖；CGO-free。**闭环语义**：评测接缝只提供评测能力，"分解→派发→评测→（不合格重派）"闭环由模型经工具组合驱动（eval_run fail → 模型重派），不做 loop 内自动重派（D4）。**遗留**：真实 key 冒烟待 rotate 后补（LLM judge 真实调用）。**下一步**：KB 全量（内容层，先 `git pull ../dsh-knowledge`）→ M10 Web 门户。

**2026-08-20 模式预设 + 标准模式缺口里程碑完成（对齐 dsh 四模式）**：用户核对当前 Agent 与 dsh 四种模式（标准/PTC/极简/创造）能力差距后拍板——创造模式（运行时插件自修改）架构排除（编译期插件边界，见 `Agent.md:48` 段），需要**标准/极简/创造外三种模式且可经设置修改** + **补齐标准模式四缺口**。**模式预设**（ADR `docs/decisions/2026-08-20-mode-presets.md`，D-MODE-1~6）：`config.yaml` 顶层 `mode: standard|minimal|code`（默认 standard ⇒ 现状零变化；minimal 预设优先关全部非 terminal/fs 能力 + 白名单整体重置；code = standard + 系统提示词注入「程序化操作」段，诚实近似 dsh Code Mode，无 TS SDK）。Mode-1 config 段（`e3e5fef`）→ Mode-2 cmd/pa per-mode prompt 组装（`e8c7e45`：buildPrompt minimal 固定 persona / code 注入 code-mode 段）。**标准模式四缺口**（ADR `docs/decisions/2026-08-20-standard-gaps.md`，D-GAP-1~4）：① **fs-search** 文件内容全文检索（`internal/fssearch` 子串/正则 + 忽略 .git/node_modules/vendor + 二进制跳过 + 边界上限 + `fs_search` 工具，`188d36e`）；② **workflow** JSON DAG 声明式编排（`internal/workflow` Kahn 拓扑 ErrCycle + 并发限流默认 4 + 依赖输出摘要注入 ≤2000 runes + 失败不阻断依赖者 + ctx 取消部分恢复 + `workflow_run` 工具，`324f700`）；③ **ralph** fresh-agent 循环（`internal/ralph` 每轮全新子代理无会话继承 + DONE/BLOCKED 协议 + maxRounds 默认 3 + 有界报告 4000 runes + `ralph` 工具，`f68ba09`）；④ **subagent 外部 provider**（`internal/subagent` `ExternalProvider` codex/claude-code exec 外部 CLI，stdin prompt→stdout、LookPath fail-closed 不回退本地、provider 名称诚实声明能力为空集 + `subagent_spawn` provider 字段默认 spawn + config `subagent.external_providers` 默认关 D10，`2f6cb31`）。四缺口全部：默认关 D10 + minimal 分支关闭 + D3 事件（`ralph/run`、`workflow/run`）+ 复用 subagent 并发 spawn（D5）+ 不改 loop（D4）。验收：vet/build 0、30 包 test 全绿（全量并发时撞既有 terminal/code 时序 flaky，重跑稳定，非本里程碑因果）；零新依赖；CGO-free；loop.go 全程未动。**真实 key 冒烟已完成（2026-08-20，用户轮换 `DEEPSEEK_API_KEY` 后执行）**：三模式切换真实对话验证全通过——standard（默认，真实回答 + `mode: standard` + 会话落库 22 事件重启可恢复）、minimal（极简人设"只能通过终端和文件工具…" + `enabled tools` 恰为 minimal 白名单 10 工具 get_time/read_file/terminal 五件套/fs 三件套 + 其余能力全关、无 fs_search/workflow/ralph）、code（回答体现程序化操作段引导 + `mode: code`）。冒烟产物（`.smoke/`、临时 config、临时 data）已清理，工作树干净。**2026-08-20 M10 Web 门户里程碑完成（统一 webServer 整体先行，用户二次拍板翻转顺序）**：ADR `docs/decisions/2026-08-20-m10-web-portal.md`（D-WEB-1~7）。**M10a**（`9592406`；认证模型修复 `6c18446`）webServer 基础设施：`internal/webserver`（net/http + **API 路由 bearer 认证**（SHA-256 摘要恒时比对；静态 shell 公开以便登录页加载，无 token 直开 `http://127.0.0.1:8080` 见登录表单）+ `go:embed` vanilla JS 前端零构建 + `/api/sessions` + `/api/sessions/{id}/events`（每事件有界 summarize，防泄露完整日志 D-WEB-4）+ `/api/health`）+ config `web_server`（默认关 D10 + minimal 关闭 + addr 默认 127.0.0.1:8080 + 空 token fail-closed 防裸奔）+ cmd/pa 接线 + printHelp `web portal` 状态行。**M10c**（`6cd21b9`）dashboard 工作台：`/api/stats` 只读内存聚合（sessions/events/tool_calls/事件类型分布/last_active，O(全部事件) 诚实注释）+ 前端 `#/dashboard` 原生 DOM 柱状条（无图表库）。**M10b**（`df3992f`）KB 管理台空壳：`/api/kb/*` 501 占位（KB 全量后挂，D-WEB-6）。三段统一：一个 webServer 多路由、只读 API 不写会话（D-WEB-4）、loop 不动（D4）、零新依赖、CGO-free。验收：全量 31 包测试绿（撞既有 code `TestRunTimeout` 时序 flaky，重跑 PASS，非本里程碑因果）+ **真实 HTTP 冒烟通过**（`/api/health` 带 token 200 `{"ok":true}` / 无 token 401 / `/api/sessions` 200 返回真实会话；printHelp `web portal: enabled`）。冒烟产物已清理，工作树干净。**2026-08-20 M10 升级「Web 全功能工作台」里程碑完成（对齐 dsh web，用户实测后拍板：只读 web UI 不需要、要像 dsh 一样）**：ADR `docs/decisions/2026-08-20-m10-web-workspace.md`（D-WEB2-A~F）。**W1**（`5455efa`，8 文件 +1476/−202）：`cmd/pa` `turnMu` 串行 + `runTurn(interactive)`（REPL 改用、web 静默 OnText）+ `eventHub`（256 缓冲丢慢订阅者 + attachSink 广播）+ `internal/webserver` 注入点（`SetMessageHandler`/`SetSessionManager`/`SetEventSource`，可 nil→501）+ `POST /api/sessions`（new）/`POST /api/sessions/{id}/resume`/`POST /api/sessions/{id}/message`（空 text 400）/`GET /api/sessions/{id}/events/stream`（SSE：快照→订阅→Flusher 逐帧，`retry:3000`+`id:<seq>`）+ **前端整体重构为 dsh 式聊天工作台唯一主界面**（左侧会话栏 + 气泡/流式 chunk 追加当前气泡 + 工具卡片折叠 + 输入框 + 深/浅主题；SSE 用 fetch+ReadableStream 解析——token 只在 header，不用 EventSource；旧只读页路由重定向 #/chat）。**W2**（`238a329`）：`GET /api/config` 脱敏视图（model/provider/mode + 19 个 cap *_enabled + tools 白名单计数；**web_server.token 与任何 key 绝不返回**）+ 前端 `#/settings` 只读分组展示 + 顶栏 ⚙ 入口。**W3**（`d317375`，真实 HTTP 冒烟抓到并修复）：① **持久化 ctx bug**——webSessionManager/webMessage 把 HTTP 请求 `r.Context()` 传给 attachSink，handler 返回即取消 → 后续 append 报 `context canceled`（POST message 500）→ 修复 `app.baseCtx`（进程级 signal ctx）+ sink 持久化改用 baseCtx + 回归测试 `TestWebSessionNewThenMessageAfterRequestCtxCancelled`；② `requireAuth` 加 panicSafeWriter recover（panic 不裸断连，JSON 500 + 栈日志）；③ **`pa --web-only` 独立 web 服务模式**（dsh 式，不读 stdin 阻塞至 Ctrl+C，`--web-only` 需 `web_server.enabled=true` 否则 fail-closed 报错）。验收：全量 31 包测试绿；**真实端到端冒烟通过**（`--web-only` 起服务：health/config 脱敏/POST new session/resume/POST message → 200 `{"ok":true}` + 事件序列 `user/message`→9×`assistant/chunk`→`assistant/message` 真实 LLM 回答 + SSE 快照帧 + 无 token 401）；CLI（REPL）对话保留不变；loop.go 零改动（D4）；零新依赖；CGO-free。**下一步**：KB 全量（内容层后补，管理台在门户内挂真实数据）。

**2026-08-20 Web 工作台逐页移植 dsh 完成（ADR `docs/decisions/2026-08-20-m10-web-workspace.md` D-WEB2-A~I；用户拍板「照 dsh web 源码一页页复制」+「去掉 token 登录、默认直开」，架构排除 ui-slots/ui-settings-plugins/ui-renderer/ui-commands popup 等）**：逐页移植（每页先研究子代理读 dsh `packages/client/ui-*` 源码产出规格 `.web-port/<Pn>-spec.md` → 实现 → 真实 HTTP 冒烟 + 全量测试绿，P1 曾试派实现子代理上下文耗尽，此后控制器亲写）。

- **P1 布局 + 聊天核心**（`85636af`）——dsh `ui-chat` 三栏 grid（sidebar|center|details）+ 拖柄 + 窄轨 56px；`--dsw-*` token 全量（暗默认 + 浅覆盖 `body[data-ds-dark-theme="false"]` + `data-ds-dark-theme` 主题属性）；消息流（user 气泡 / assistant 思维链+流式 / 工具卡片）；composer 输入栏 + 空态「探索未至之境」；SSE fetch+getReader 流式（token 不入 URL）。
- **P2 侧栏 + 会话管理**（`260af98`）——dsh `ui-session-list` 会话单行（状态点 + 标题 + 时间 + 菜单）+ 搜索过滤 + 行内重命名/删除（`PATCH /api/sessions/{id}/title`、`DELETE /api/sessions/{id}`，store `SetSessionTitle`/`DeleteSession` 级联删 events）+ 激活流式蓝点；会话列表 title 优先 store.Title。
- **P3 设置 + 模型选择**（`61fd565`）——dsh `SettingsRoot` 两栏（188px 导航轨 + 四段只读：通用/模型/能力/工具，能力按 `typeof === "boolean"` 过滤）；`GET /api/config` 脱敏视图（model/provider/mode/base_url/*_enabled/tools_enabled+count/web_server_addr——token/key 绝不返回）。
- **P4 子代理 + 后台任务**（`6078676`）——侧栏「🧩 运行」tab + 弹层：`GET /api/subagents` + `GET /api/jobs` 只读脱敏视图（id/status/timestamps，无 prompt/输出/会话内容）+ 状态点（10px 像素追逐动画）+ 10s 轮询（visibility 暂停）+ 1s live 时钟 + 外点关闭。
- **P5 主题跟随系统 + 消息反馈 + 图片附件**（`972e96e`）——主题三态 浅色/深色/跟随系统（`matchMedia` 监听 + `color-scheme` + `meta theme-color`，设置页三立方）；assistant 消息 actions 行（复制 + 👍/👎，localStorage `pa_fb:<session>:<seq>` 乐观持久，无后端）；图片附件——拖拽/粘贴入草稿 rail（64px 缩略图，10 张/10MB 前端限额）+ lightbox 原图 + 上传 `POST /api/sessions/{id}/attachments`（multipart → `attachment.Store.SaveImage`，新增 `GetByID`）+ `GET .../attachments/{attID}` 字节回显 + message body `images:[id]` → 落 user/message 图事件（只存 ref 不落字节，复用 /attach M8-3 路径，loop 零改动 D4）+ events `images` 字段（历史图 240px 单张 / 64px 平铺 + 失败重试）。

**配置开关**：`llm.multimodal.enabled`（默认关 D10）→ 附件 API 501 + 带图消息 400（前端 toast 提示「图片上传失败」）；打开后附件链路全通。验收：全量 30 包测试绿（含 P5 上传/回显/带图/eventView images 单测）；真实 HTTP 冒烟（--web-only + multimodal 临时开 → 上传 201 → 回显 200 字节一致 → 恢复 config）；CLI/REPL 不变；loop.go 零改动（D4）；零新依赖；CGO-free。**遗留**：消息发送按钮/输入默认提示与 dsh 细节、轻量渲染器（markdown 手写扫描）按需精修。

## 4. 路线图

| 里程碑 | 交付物 | 验收标准（达标才算完成） | 状态 |
|---|---|---|---|
| **M1 最小循环** | `cmd/pa` REPL；`llm`（DeepSeek 流式）；`session` 内存日志；`tools` 注册表 + `get_time`/`read_file`；`loop` 串行 turn/step | 命令行提问可流式回答；工具可被调用并回写日志；`go vet` + `go test` 干净 | ✅ 2026-08-18 验收通过（`6380163`） |
| **M2 持久化与会话** | `store`（SQLite）+ 多会话（/new /list /resume）；`prompt` 分节组装；`config.yaml`；重试策略 | 重启恢复会话且历史完整回放；新事件类型不改历史结构 | ✅ 2026-08-18 验收通过（`e865aca`） |
| **M3 安全与完善** | 工具白名单/权限；超时与输出截断；取消（Ctrl+C）；CLI 完善（Web 可选） | 未白名单工具拒绝执行；取消即时生效；长输出不爆上下文 | ✅ 2026-08-18 验收通过（`1dda2ed`，ADR `2026-08-18-m3-sandbox-scope`） |
| **M4 知识库**（三段） | 拆为 M4a/b/c 依次验收 | 全部达标才算 M4 完成 | ⬜ |
| **M4a 内核** | `kb` 接口（Search/Add/Recall）+ SQLite FTS5 Provider（BM25 + 中文二元组 LIKE 兜底）+ `kb/recall` 事件类型 + config；主 ADR 定稿检索方案 | 中文/英文/混合检索正确；`Add` 后能检索；换 Provider 不改消费方；零新依赖 | ✅ 2026-08-18 验收通过（`682f07e`，ADR `2026-08-18-m4-kb-architecture.md`） |
| **M4b 工具与召回** | `kb_search`/`kb_read`/`kb_add` 工具（默认关）+ `cmd/pa` 召回注入（catalog + 有界 recall）+ `/kb-status` `/kb-reindex` + `kb/add` 事件 | 工具默认关闭且参数校验；注入走 `kb/recall` 落日志；fail-open | ✅ 2026-08-18 验收通过（`bdd903d`） |
| **M4c 提取回写** | `KB.Extract`（幂等 `session:turn`、严格 JSON fail-closed、不阻断回答）+ `kb/extract` 事件 + config；补 ADR | 对话产生可复用知识能被提取写入并被后续检索；坏输出 fail-closed | ✅ 2026-08-18 验收通过（`5e98fa7`） |
| **M4 知识库**（三段） | 拆为 M4a/b/c 依次验收 | 全部达标才算 M4 完成 | ✅ 三段全部完成 |
| **M5 核心能力**（四段，ADR `2026-08-18-m5-agent-core.md`） | 拆为 M5a/b/c/d 依次验收 | 全部达标才算 M5 完成 | ✅ 四段全部完成（M5a/M5b/M5c/M5d，均 2026-08-19 验收通过） |
| **M5a 后台任务** | `jobs` 接口（owner-fenced 注册表）+ 本地实现 + `job_*` 工具 + `job/*` 事件 + config | 后台工作可观察/取消/等待/通知；owner 隔离；主循环保持串行；默认关闭 | ✅ 2026-08-19 验收通过（M5a-1 `34bf1e8`+`5f3abd4`；M5a-2 `4c0a25e`+`dbe07fc`+`b1d3535`+`6d91af7`） |
| **M5b 子代理** | `subagent` 接口（多 Provider 注册表）+ spawn 实现 + 委托/控制/报告工具 + `subagent/*` 事件 + config | 子代理独立会话日志可回放；结果回传父会话；后台续跑走 job；默认关闭 | ✅ 2026-08-19 验收通过（M5b-1 `55f1b63`+`34c302c`+`8a3f648`；M5b-2 `8c7f1b3`+`070039e`+`27acada`+`e8dcec0`+`78fd6a6`） |
| **M5c 上下文压缩** | `compaction` 接缝 + 摘要 provider + tool-result 剪枝 + `/compact` + `compaction/*` 事件 + config + PreStep 自动压缩 | 超预算触发压缩；摘要经 surfaceOp.replace user/message 遮蔽旧范围且日志仍追加式；tool-call/result 配对不被切断；默认关闭 | ✅ 2026-08-19 验收通过（M5c-1a `76c41db`；M5c-1b `2188b4d`+`0ffa4e4`+`4669c2e`；M5c-2a `a5219ac`；M5c-2b `c4b5e88`+`e9b2b9c`） |
| **M5d 技能** | `skill` 接口（多 Provider 注册表）+ 文件系统发现 + 目录注入 + `skill` 加载工具 + `skill/*` 事件 + config | 目录注入有界；按需加载完整正文；默认关闭 | ✅ 2026-08-19 验收通过（M5d-1 `b2d93fc`+`453b288`+`0c38de5`+`400e06c`+`75d892c`；M5d-2 `cb09853`+`17cfe10`+`935ffdc`+`6859000`+`07a82ce`） |
| **M6 能力补全**（六段，ADR `2026-08-19-m6-agent-full.md`） | 拆为 M6a/b/c/d/e/f 依次验收 | 全部达标才算 M6 完成 | ✅ 六段全部完成（M6a/M6b/M6c/M6d/M6e/M6f，均 2026-08-19 验收通过） |
| **M6a 定时调度** | `schedule` 接口（多 Provider 注册表）+ 间隔/cron 实现 + `schedule_*` 工具 + `schedule/*` 事件 + config | 定时任务到期触发（事件 + 入队 job，D5）；可观察/取消；默认关闭 | ✅ 2026-08-19 验收通过（M6a-1 `85cd9a3`+`5aeb9e5`+`ef9011a`；M6a-2 `2d5aed4`+`d599e4f`+`84b0346`+`3fb43fd`） |
| **M6b 任务规划** | `plan` 接口（goal→plan→todo 三层）+ 规划/推进工具 + `plan/*` 事件 + config | 多步任务拆解/跟踪/推进（执行可委托子代理）；默认关闭 | ✅ 2026-08-19 验收通过（M6b-1 `e006a9e`+`eaf13e9`；M6b-2 `69e57cd`+`437028c`+`1b6b62b`+`512896f`） |
| **M6c 长期记忆** | `spill` 接口（跨会话记忆 Provider）+ 自动沉淀/召回 + `spill/*` 事件 + config | 对话衍生记忆自动沉淀并可召回；与 kb（显式知识）接缝独立；默认关闭 | ✅ 2026-08-19 验收通过（M6c-1 `b087b22`+`949c84e`+`4ae6a42`；M6c-2 `f88ad7b`+`9f80bf8`+`32ec136`+`717ed92`） |
| **M6d 人工审批** | `interact` 接口（审批请求/响应）+ 敏感工具门 + `interact/*` 事件 + config | 敏感操作执行前经人工确认（CLI y/n，fail-closed）；默认关闭 | ✅ 2026-08-19 验收通过（M6d-1 `6d32daa`+`d277ba2`；M6d-2 `8a3ad1b`+`6cd032a`+`0b01683`+`fb578e3`+`0118169`） |
| **M6e 代码沙箱** | `code` 接口（沙箱 Provider）+ 本地子进程隔离实现 + `code_run` 工具 + `code/*` 事件 + config | 模型生成代码在受控沙箱执行（超时/配额/默认无网络）；补强 M3 `run_command`；默认关闭 | ✅ 2026-08-19 验收通过（M6e-1 `a66d33e`+`24d7f1c`；M6e-2 `be9ecf2`+`e850820`+`cf2590f`+`cb39660`） |
| **M6f 工具生态** | `mcp` 接口（MCP 客户端，JSON-RPC 自实现优先）+ `fs`/workspace 统一封装 + 工具 + `mcp/*` 事件 + config | 外部工具/服务经 MCP 接入；文件操作统一封装；默认关闭 | ✅ 2026-08-19 验收通过（M6f-1 `764261c`+`4e474f2`；M6f-2 `29ea541`+`ef92769`+`0e025fc`+`a5a9494`；M6f-3 `8526f59`+`c3a74a0`+`9e09d9e`+`f20ae3b`） |

### 候选里程碑（2026-08-24，当前仅保留 Agent 能力与 Agent Web 工作台）

> **用户定序（2026-08-20）**：先把 Agent 部分（M7 → M8 → M9 → 评测接缝 → 模式预设/四缺口）完成，再做 **M10 Web 工作台**。**2026-08-24 范围调整**：KB 内容层、KB 管理台真实数据、`kb_import` 批量/大文档导入均移交另一个项目，本项目不再排期。
> 依赖关系：M8 打包多 provider + reasoning 回传 + 多模态（同改 `llm.Message` 消息模型与 wire 层）；M9 持久 PTY 经 jobs（M5a）owner-fenced 承载；M10 用 Go 标准库 `net/http` 自建（零新依赖），只负责 Agent Web 工作台。Agent 参照源为 `../deepseek-harness`。

| 阶段 | 候选 | 交付物 | 验收标准（达标才算完成） | 状态 |
|---|---|---|---|---|
| **① Agent 部分** | **M7 web 搜索** | `web` 接缝三件套（web service + WebSearchProvider 注册表 + `web_search`/`web_fetch` 工具）+ `web/*` 事件 + config；**DeepSeek 官方搜索 provider**（照搬 dsh `web-search-deepseek`：Anthropic 兼容 Messages API `POST /anthropic/v1/messages` + `web_search_20250305` server tool，复用 `DEEPSEEK_API_KEY`，解析结构化 `web_search_tool_result`）；多查询一步到位（seam 单查询契约 + 消费者侧扇出/去重/round-robin 合并，借鉴 rc.8） | 真实搜索返回结构化结果与来源；`web/*` 事件落库（D3）；按 D7 校验；默认关闭（D10）；零新依赖 | ✅ 2026-08-20 验收通过（ADR `2026-08-20-m7-web-search.md`；M7-1 `89fddcc`+`fe299f2`；M7-2 `9bb6478`+`1833c3c`+`5818c2e`+`41ac0c4`+`12991a9`+`284562f`+`7262115`；真实 key 冒烟待 rotate 后补） |
| **① Agent 部分** | **M8 消息模型升级**（多 provider + reasoning 回传 + 多模态） | ① `llm.Message` 从 string Content 升级为 content parts（text / image ref / reasoning），assistant 消息支持 `reasoning_content` 落库（D3 新事件类型）并回传；② LLM provider 注册表 + config 选择（deepseek / OpenAI 兼容 / Anthropic Messages——与 M7 复用 Anthropic 兼容 HTTP 客户端）；③ 多模态：user 图片走文件路径→落库只存引用→请求时转 `image_url` data URL（dsh `7078918` 范式：20MiB 上限、最老替换占位符、PNG/JPEG/WebP/GIF、模型能力按 exact-model `inputModalities` 声明） | 可在 config 切换 provider 且会话历史跨 provider 重编码正确（reasoning 签名保留）；图片输入可被模型读取；`llm.Message` 相关 D3 事件类型新增且旧会话回放不受影响（D8）；默认关闭 | ✅ 2026-08-20 完成（ADR `2026-08-20-m8-message-model.md`；M8-1 `4fcc57d`~`45b6050`；M8-2a `983b2f6`~`20ed3d3`；M8-2b `39670d7`~`10f41b3`；M8-3 `c137a21`~`c3914e4`；真实 key 冒烟待 rotate 后补） |
| **① Agent 部分** | **M9 persistent terminal**（持久 shell，ADR `2026-08-20-m9-terminal.md`） | `internal/terminal`（BoundedTextBuffer 有界回滚 + Session 持久 shell 子进程 + Windows-first cmd /Q 管道实现 + 就绪判定 stdin_read/timeout/session_exit）+ 五件套工具（`terminal_start`/`write`/`read`/`signal`/`stop`，owner-fenced 单活跃会话 D5）+ `terminal/*` 事件（D3 元数据，不落输出正文）+ `/term` REPL + config（默认关 D10 + 会话环境 scrubbed） | 多步操作共享 shell 状态；就绪判定可靠；超时/退出检测；输出有界；默认关闭（D10）；零新依赖 | ✅ 2026-08-20 完成（ADR `2026-08-20-m9-terminal.md`；M9-1 `c0b973e`+`b78f694`+`c8d9b1a`；M9-2 `aa5fe1e`+`2c67343`+`2bb5dcc`+`443dd15`+`604cc2d`+`ddd4fdb`；Windows 无 ConPTY 诚实限制已文档化） |
| **① Agent 部分（收尾）** | **任务评测接缝**（ADR `2026-08-20-eval-seam.md`） | `internal/eval`（Evaluator 接口 + rule/llm/manual/composite 四实现 + Engine + mem 历史存储上限淘汰）+ `eval_run`/`eval_result`/`eval_list` 工具 + `eval/run` D3 事件（只记摘要）+ config（默认关 D10）+ 验收标准来源：`plan_todo` 带 `acceptance` 字段、`subagent_spawn` 带 `acceptance_criteria` 注入子代理 prompt | 规则断言优先（确定性）、LLM judge 兜底、无法自动判定落 interact 人工回退（approved→pass/rejected→fail）；"分解→派发→评测→（不合格重派）"闭环由模型驱动（不改 loop，D4）；默认关闭（D10）；零新依赖 | ✅ 2026-08-20 完成（ADR `2026-08-20-eval-seam.md`；Eval-1a `7931fff`；Eval-1b `8be20b6`；Eval-2a `03db1a2`；Eval-2b `8a6feb9`；Eval-3a `06289e9`；Eval-3b `902f04f`） |
| **① Agent 部分（对齐 dsh 模式）** | **模式预设 + 标准模式四缺口**（ADR `2026-08-20-mode-presets.md` + `2026-08-20-standard-gaps.md`） | `config.yaml` 顶层 `mode: standard|minimal|code`（默认 standard，minimal 预设优先 / code 注入程序化操作段，Mode-1 `e3e5fef` + Mode-2 `e8c7e45`）；四缺口：fs-search 全文检索（`188d36e`）、workflow JSON DAG 编排（`324f700`）、ralph fresh 循环（`f68ba09`）、subagent 外部 provider codex/claude-code（`2f6cb31`） | 三种模式可经 config 切换（默认 standard 现状零变化）；四缺口工具注册/白名单/D3 事件/config 齐全；全量测试绿；零新依赖；不改 loop | ✅ 2026-08-20 完成（四缺口全验收，见 `Agent.md` 当前状态段） |
| **③ Agent 呈现层** | **M10 Web 工作台** | webServer 基础设施、dsh 式会话/事件入口、聊天交互、dashboard、设置与工具状态展示 | Agent Web 工作台可用；默认关闭（D10）；零新依赖 | ✅ 已完成 |



## 5. 开发纪律（每轮工作前过一遍）

1. **新功能不改循环**（D4）：能力一律走接缝（接口 + 后端 + 工具）。
2. **模型可见必落日志**（D3）：先加事件类型，再实现。
3. **工具参数入口校验**（D7）：Execute 之前统一 JSON Schema 校验。
4. **先文档后代码**：涉及核心数据模型、循环结构、包依赖方向的变更，先写 `docs/decisions/` 决策记录并更新 design.md。
5. **保持 CGO-free**（Windows 可无工具链构建）；新依赖必须纯 Go 或可无 CGO 使用。
6. **API Key 只走环境变量**，绝不写入代码、配置或日志。
7. **双向同步**：design.md 与本文状态/决策变更必须同步更新。
8. **一里程碑一 PR/提交**：按验收标准检查后才算完成，不达标不进入下一里程碑。
9. **参照源先更新**：里程碑开始前仅更新 Agent 参照源码 `../deepseek-harness`。KB 由另一个项目负责，本项目不再拉取或验收 `../dsh-knowledge`。

## 6. 决策记录（ADR）

路径：`docs/decisions/YYYY-MM-DD-<slug>.md`。模板：状态（提案/已定/废弃）→ 背景 → 决策 → 理由 → 后果（含放弃的方案）。已有决策见 design.md 第 10 节 D1–D10，ADR 只记录其后的增量变更。

## 7. 常用命令

```sh
go build ./...        # 构建
go test ./...         # 单元测试
go vet ./...          # 静态检查
go run ./cmd/pa       # 启动 REPL（M1 后可用，需 DEEPSEEK_API_KEY）
```

## 8. 会话交接协议（控制面 / 实施面）

**分工**：本会话（控制面）定契约、验收、更新状态；实施会话（实施面）读契约、写代码、自测。会话间唯一可靠通信渠道是磁盘文件——新会话看不到控制会话的对话历史，只依赖本文档与 design.md。

**流程**：

1. **交接**：控制会话把开场白模板（见下）发给实施会话，指定里程碑；各里程碑的完整派发消息存于 `docs/dispatch-*.md`（M5 依序：`dispatch-m5a.md` → `dispatch-m5b.md` → `dispatch-m5c.md` → `dispatch-m5d.md`，均已完成派发；历史：`docs/dispatch-m4a/b/c.md`、`docs/dispatch-m3.md`、`docs/dispatch-m2.md`）。
2. **实施**：实施会话按 design.md 实现，自测通过后提交，并报告：改动文件清单、实现决策、跑过的命令、测试结果。
3. **验收**：控制会话亲自跑 `go build` / `go test` / `go vet`，审查 `git diff`，对照 D1–D10 逐条检查（日志先行、工具入口校验、接口隔离、无循环改动、无越界功能）。
4. **收尾**：通过 → 更新第 3/4 节状态 → 准备下一里程碑交接；不通过 → 把问题清单发回实施会话修订。

**实施会话开场白模板**（直接粘贴）：

> 请阅读 `D:\dev-projects\Agent\shutu-agent\Agent.md` 和 `docs/design.md`，按设计基线实现 **M1 最小循环骨架**（里程碑验收标准见 Agent.md 第 4 节）。参考原型 dsh 的源码与文档在 `D:\dev-projects\Agent\deepseek-harness`——实现每个模块前先读 Agent.md 第 9 节对应的 dsh 源码与文档，借鉴其结构与接口设计（注意 dsh 是 TypeScript + 插件框架，只需借鉴思路，不照搬代码，Go 实现按 design.md 的模块地图落地）。完成后运行 `go vet ./...`、`go test ./...`、`go build ./...` 并全部通过，然后报告：改动文件清单、实现决策、测试结果。严格遵守 design.md 第 10 节 D1–D10，不要引入任何超出 M1 范围的功能。

**并行原则**：同一里程碑只派一个实施会话；需要并行时按包目录划分所有权（如 `session`/`store` 与 `kb` 分属不同会话），各会话只写自己负责的目录。

**防跑偏红线**：实施会话的报告不作为验收依据；越界功能（超出里程碑范围）一律退回，不合并。

## 9. 参考链接

**2026-08-24 范围调整**：用户决定将 KB 全量内容层、`kb_import` 批量/大文档导入及其可恢复 Job 移出本项目，由另一个项目负责。本项目不再排期、实现或验收这些任务；本项目保留现有 KB 接缝仅作为兼容边界。Goal scheduler 已恢复为本项目当前自主任务能力，按 dsh v1 语义实现，仍由 schedule.enabled 控制。
## 10. dsh 对齐改进目标（2026-08-23，当前执行目标）

本节是当前 Agent 实施与复核的目标清单，优先级高于历史里程碑中“loop 严格串行、不得修改”的旧约束。目标是逐步提高 `shutu-agent` 与当前 `deepseek-harness` 的行为一致性；不追求把 TypeScript/Cordis 插件运行时原样搬到 Go。

### 实施顺序与验收标准

1. **Loop 持久化与取消语义（P0）**
   - 流式响应被取消或中断时，必须落库带 `interrupted` 标记的 `assistant/message`，不能丢失已生成文本/推理。
   - 一个 assistant 响应包含多个工具调用时，取消发生在中途时，尚未派发的调用必须落库合成的 aborted tool result。
   - 增加 turn/step 生命周期事件，保留现有历史回放兼容性。
   - 在安全边界明确后，再实现 dsh 风格的并行安全工具调用、inbox/followup/steer/inject 和 pre-step 控制。

2. **文件工具 dsh 语义（P0/P1）**
   - 对齐 `read` 的 `offset`、`limit`、行号和输出上限；路径必须受 workspace/root 约束。
   - `write/edit` 增加 observation policy 和文件版本不变检查，避免无确认覆盖外部修改。
   - 增加 `read_image`，并按模型图像能力和附件引用规则工作。
   - 保留 Go 专有扩展时，必须在文档中记录 schema/行为偏差并添加兼容测试。

3. **可持续子 Agent（P1）**
   - child session 和 parent/child 关系持久化，支持进程重启后的冷恢复。
   - 增加 `send_message`、`interrupt_agent`、`report`，并保留 owner/权限边界。
   - 覆盖 running/idle/ready/settled 状态、父子通知和 child-first teardown。

4. **Goal round driver（P1）**
   - 在现有 plan/todo 之上增加 goal 生命周期、同会话继续执行和 round 上限。
   - Goal 推进必须能观察 plan/todo/subagent/eval 状态，并在达成、阻塞、超限时留下可回放事件。

5. **Workflow/Ralph 协议对齐（P1）**
   - 以 dsh 为能力完整性目标，补齐模型编写 JavaScript workflow script、`meta/script/args`、`agent()`、`parallel()`、`pipeline()`、`phase()`、`log()` 与 workflow 生命周期事件。
   - Go 核心不直接依赖 Node.js；由外部 Node runtime 按需执行 workflow。当前个人 Agent 按 dsh 信任模型处理，JavaScript workflow 默认启用，不另设阻断能力的 `workflow_node_unsafe` 安全模式。
   - 保留现有 Go-native JSON DAG 作为原生/兼容执行路径，但不得以它替代 dsh JavaScript workflow 的完整能力。
   - Ralph 使用结构化 report、`continue/complete/blocked` 状态和受限 handoff/result，而不是只解析自由文本。

6. **Plan tree 持久化（P1）**
   - 将当前内存 plan tree 改为可由 session event log 重建的持久化投影，支持 Goal/Plan/Todo 的重启恢复、继续执行、状态查询和幂等更新。
   - KB 直接功能、`kb_import` 及批量/大文档导入 Job 已移出本项目，由另一个项目负责；本项不实现或验收这些内容。
   - Goal scheduler 不属于本项的 plan projection；其 dsh 对齐实现作为第 8 项单独目标，负责定时/周期触发与 Goal continuation。

7. **LLM 请求元数据与重试（P1）**
   - 补充 message/source/usage/provider/model 元数据和请求终态事件。
   - 增加可配置 retry/backoff，并记录 retry 事件；失败必须收敛为可回放的终态。

8. **剩余能力复核（P2）**
   - 复核并按收益排序 `session-query`、LSP、rich ask-user、feedback、hooks、sandbox、ACP/SDK、Web UI 对齐等能力。
   - plugin/bundle/profile/runtime self-modification 继续作为明确的 Go 编译期边界，不默认引入。

### 每项的实施/复核纪律

- 每个目标先对照 `../deepseek-harness` 当前源码和 README，必要时新增 `docs/decisions/` 记录。
- 先写或更新测试，再实现；每项完成后至少运行相关包测试，并运行 `go vet ./...`、`go test ./...`、`go build ./...`。
- 复核必须检查事件日志、模型可见历史、取消/重启/失败路径和工具 schema，不能只以编译通过作为完成标准。
- 若 Go 实现与 dsh 有意不同，记录“偏差、原因、替代验收标准”，不得静默偏离。
- 本节目标的状态由实施会话逐项更新为：`⬜ 未开始`、`🔄 实施中`、`✅ 已实现并复核`、`⚠️ 有明确偏差`。

### 当前状态

| 顺序 | 目标 | 状态 | 最近复核 |
|---|---|---|---|
| 1 | Loop 持久化与取消语义 | ✅ 已实现并复核 | 已完成 interrupted assistant、aborted tool result、turn/step 生命周期；并行/inbox 仍列入后续复核 |
| 2 | 文件工具 dsh 语义 | ✅ 已实现并复核 | read window/root、write/edit observation、rich read_image 已通过定向测试 |
| 3 | 可持续子 Agent | ✅ 已实现并复核 | child log + parent/depth 元数据持久化；Runtime.Resume/subagent_resume 支持冷恢复；continuable 子 Agent 支持 send/interrupt，report 记录 subagent/report；旧 status/cancel 保留兼容 |
| 4 | Goal round driver | ✅ 已实现并复核 | `internal/goal` 已接入 cmd/pa CLI/Web 的外层 turn 完成→idle/followup 生命周期；同 session 逐轮调用 `runTurn`，通过 plan/create 事件定位当前 session 最新未完成 Goal，observer 汇总 plan/subagent/eval 状态；不从工具 Execute 内递归进入 loop；后台 scheduler 由第 8 项负责；plan tree 持久化已在第 6 项完成 |
| 5 | Workflow/Ralph 协议对齐 | ✅ 已实现并复核 | Ralph 已补 dsh-compatible `summary/evidence/nextSteps/blocker` 状态语义、16K handoff 上限和旧 DONE/BLOCKED 兼容；workflow 已新增外部 Node runner、`meta/script/args`、`agent/parallel/pipeline/phase/log`、RPC、取消、并发/总量/item 上限和 `workflow/*` 生命周期事件；本地 spawn provider 已提供 scoped `structured_output` 工具，`agent({schema})` 会校验并返回结构化对象；JS 默认启用，Go-native DAG 保留兼容路径，Go 核心不依赖 Node.js |
| 6 | Plan tree 持久化 | ✅ 已实现并复核 | `plan/create` 写入可重建的 Goal/Plan/Todo 快照，`plan/status/delete` 可重放；启动与 session 切换从 event log 重建内存 projection，支持状态查询、继续执行、幂等 Restore 与 ID 接续；KB 直接功能与 `kb_import` 已移交外部项目 |
| 7 | LLM 元数据与重试 | ✅ 已完成 | 四种流式 provider 均映射 provider-neutral `TokenUsage`；`assistant/message` 与 `llm/request_end` 落 usage/attempts；统一 request-level retry wrapper 覆盖所有 provider，DeepSeek 应用 wiring 关闭内置 retry 防重复；429/网络/5xx 重试、4xx fail-closed、context-aware backoff、`llm/retry` 事件均已接入。边界：流已开始输出后不重放，避免重复内容 |
| 8 | Goal scheduler（dsh v1） | ✅ 已实现并复核 | `after_seconds` / `at` / `every_seconds`（固定周期最短 300 秒）；session-local `schedule/change` 事件折叠恢复；动态 next wake；one-shot 优先单条投递；every 逾期只投递最新 occurrence batch、不重放 backlog；成功 turn 后 dispatch，失败保留可重试；正常 `runTurn` 后接 Goal idle continuation |
| 9 | 剩余能力复核 | ✅ 已完成（ACP 高级能力边界审计） | ACP session 已注入独立 compaction、terminal、MCP client 集合与 subagent runtime；各能力均有显式 ACP 开关、session owner、独立 registry 和关闭生命周期。plugin/bundle/profile/self-modification 已明确保持 Go 编译期边界：不复制全局 registry/profile，不引入运行时插件或自修改执行协议；外部工具生态使用显式 MCP 接缝 |

### 文档

- 设计基线：[`docs/design.md`](docs/design.md)
- 原型架构：[`../deepseek-harness/docs/architecture.md`](../deepseek-harness/docs/architecture.md)
- dsh 循环细节：[`../deepseek-harness/docs/subsystems/core.md`](../deepseek-harness/docs/subsystems/core.md)
- dsh 会话日志：[`../deepseek-harness/docs/subsystems/session.md`](../deepseek-harness/docs/subsystems/session.md)
- dsh 能力接缝：[`../deepseek-harness/docs/capability-seams.md`](../deepseek-harness/docs/capability-seams.md)
- M4 参照插件（知识库，FTS5 + 提取回写）：[`../dsh-knowledge/`](../dsh-knowledge/)（[GitHub](https://github.com/lemoncat7/dsh-knowledge)）+ 调研 [`docs/research-m4-kb.md`](docs/research-m4-kb.md)
- M5 参照四个能力族：[`../deepseek-harness/packages/jobs/`](../deepseek-harness/packages/jobs/)、[`../deepseek-harness/packages/subagent/`](../deepseek-harness/packages/subagent/)、[`../deepseek-harness/packages/compaction/`](../deepseek-harness/packages/compaction/)、[`../deepseek-harness/packages/skill/`](../deepseek-harness/packages/skill/) + 子系统文档 [`docs/subsystems/{jobs,subagent,compaction,skills}.md`](../deepseek-harness/docs/subsystems/jobs.md)；M5 主 ADR `docs/decisions/2026-08-18-m5-agent-core.md`
- M6 参照六个能力族：[`../deepseek-harness/packages/schedule/`](../deepseek-harness/packages/schedule/)、[`goal/`](../deepseek-harness/packages/goal/)、[`plan/`](../deepseek-harness/packages/plan/)、[`todo/`](../deepseek-harness/packages/todo/)、[`spill/`](../deepseek-harness/packages/spill/)、[`interaction/`](../deepseek-harness/packages/interaction/)、[`code-runtime/`](../deepseek-harness/packages/code-runtime/)、[`mcp/`](../deepseek-harness/packages/mcp/)、[`fs/`](../deepseek-harness/packages/fs/)；M6 主 ADR `docs/decisions/2026-08-19-m6-agent-full.md`

### 源码参考（`../deepseek-harness/packages/`）

实现每个模块前先读对应源码，借鉴结构、接口划分与边界设计；dsh 是 TypeScript + Cordis 插件框架，**只借鉴思路，不照搬代码**。

| 本模块 | dsh 参考源码 | 重点看什么 |
|---|---|---|
| `loop` | `core/agent-loop/` | 循环驱动、turn/step 状态机 |
| `session` | `core/session/` | 事件日志、历史派生（deriveMessages） |
| `tools` | `core/tools/` | 工具注册表、参数校验、执行管道 |
| `prompt` | `core/system-prompt/` | 提示词分节组装 |
| `llm`（M8） | `llm/llm/` + `llm/llm-deepseek/` + `llm/llm-pi-ai/` + `llm/llm-retry/` | 适配器接口、流式、DeepSeek 实现、可配置路由 provider、重试包装；多模态 content parts（`7078918` 范式：落库只存 ImageAttachmentRef、请求时转 data URL） |
| `store`（M2） | `session/session-persistence*` | 持久化与重放 |
| `kb`（M4） | `../dsh-knowledge/src/`（domain/local-provider/retrieval/extraction/tools/recall）+ `web/`（seam 三件套模板） | 知识条目模型、FTS5 检索 + 中文二元组兜底、提取回写、能力接缝的包划分 |
| `jobs`（M5a） | `../deepseek-harness/packages/jobs/{jobs,jobs-local,tool-jobs}/` | owner-fenced 后台任务注册表、生命周期契约、模型侧控制工具 |
| `subagent`（M5b） | `../deepseek-harness/packages/subagent/{subagent,subagent-spawn-in-process,tool-subagent,tool-subagent-control,tool-subagent-report}/` | Provider 注册表、委托/控制/报告、子代理会话 |
| `compaction`（M5c） | `../deepseek-harness/packages/compaction/{compaction,compaction-basic,compaction-tool-result-pruner,command-compact}/` | 压缩接缝、摘要 provider、tool-result 剪枝、人工命令 |
| `skill`（M5d） | `../deepseek-harness/packages/skill/{skill,skill-filesystem,tool-skill}/` | 技能 provider 注册表、文件系统发现、目录/加载工具 |
| `schedule`（M6a） | `../deepseek-harness/packages/schedule/` | 定时调度 provider 注册表、触发语义 |
| `plan`（M6b） | `../deepseek-harness/packages/{goal,plan,todo}/` | goal→plan→todo 规划模型、推进工具 |
| `spill`（M6c） | `../deepseek-harness/packages/spill/` | 跨会话记忆、自动沉淀/召回 |
| `interact`（M6d） | `../deepseek-harness/packages/interaction/` | 审批请求/响应交互 |
| `code`（M6e） | `../deepseek-harness/packages/{code-runtime,e2b}/` | 沙箱 provider 接口、代码执行 |
| `mcp`/`fs`（M6f） | `../deepseek-harness/packages/{mcp,fs,workspace}/` | MCP 客户端、文件/工作区封装 |
| `web`（M7 候选） | `../deepseek-harness/packages/web/{web,web-search-deepseek,web-fetch-http,tool-web}/` | web 接缝三件套、DeepSeek 搜索 provider（Anthropic 兼容 Messages API + `web_search_20250305`）、fetch-http provider、`web_search`/`web_fetch` 工具与多查询扇出 |
| `terminals`/persistent shell（M9 候选） | `../deepseek-harness/packages/shell/{tool-pwsh-persistent,tool-bash-persistent}/` + `packages/terminal/{terminal,terminal-bash,tool-terminal}/` | owner-scoped 持久 shell（`ctx.terminals`）、状态保留/超时重置/输出上限、Windows ConPTY 与回显/信号限制 |
| `webServer`/Web 管理台（M10 候选） | `../dsh-knowledge/src/web.ts` + `../dsh-knowledge/web/`（静态管理台）+ `../dsh-knowledge/src/{management-proxy,api,connection,service-settings}.ts` + dsh `packages/host/webserver/` | 静态资源服务 + JSON API 路由 + bearer 认证（SHA-256 摘要）、知识库三栏文档界面、条目维护/候选审核/令牌管理、认证 HTTP API 形态 |
