# Agent.md — 数驼 AI Agent 项目工作指南

> 本文是 `shutu-agent` 的工作入口：先看这里，再看 [`docs/design.md`](docs/design.md)。
> 涉及设计、数据模型、事件或包依赖方向的改动，先更新设计文档/ADR，再改代码。

**文档整理日期：2026-08-25**

## 1. 当前结论

- M1–M6 基础 Agent 能力已完成并通过验收。
- M7 Web 搜索、M8 消息模型升级、M9 持久终端、任务评测接缝、模式预设及标准模式缺口已完成并通过验收。
- M10 Web 工作台已完成：聊天、会话管理、SSE 流式输出、设置、运行状态、主题、反馈和图片附件均已接入。
- dsh 对齐复核清单中的 9 项目标已完成复核；后续新增工作应先建立新的目标或 ADR，不要把历史完成项重新写入路线图。
- 当前项目保留 KB 接缝和已有兼容能力；KB 内容层、真实数据管理台、`kb_import` 批量/大文档导入及其 Job 已移交其他项目，不在本项目排期、实现或验收。
- 项目不做运行时插件或自修改执行。新增能力通过编译期接缝实现；外部工具通过 MCP 接入。

## 2. 不可违反的项目纪律

### 2.1 dsh 对齐与变更确认

1. Web 界面的布局、交互、文案、主题和行为默认以 `../deepseek-harness` 为参照；修改前先阅读对应源码并形成实现规格。
2. 删除已有功能、改变既有行为或缩减实现，必须先向用户确认；确认过的变更在提交说明中标注“已确认”。
3. 新功能先检查 dsh 是否已有对应实现，再决定复用思路、兼容实现或自定义实现，并在设计文档或 ADR 中记录结论。
4. 与 dsh 的任何有意偏差都必须记录偏差、原因和替代验收标准，不得静默偏离。

### 2.2 架构与安全

- 新能力采用 `Service / Provider / Tool` 三件套，消费方依赖接口，不把实现细节塞进核心循环。
- 模型可见内容必须先落事件日志；新增可见输入先定义事件类型（D3）。
- 工具在 `Execute` 入口统一做 JSON Schema 校验（D7）。
- 默认关闭高风险能力，按白名单显式启用（D10）；API key 只允许来自环境变量，不能写入配置、代码或日志。
- 核心保持薄、Go 接口加注册表，不引入运行时插件系统或全局事件总线；Go 代码保持 CGO-free。
- 会话采用追加式事件日志，历史从日志派生，不另存第二份权威历史（D1）。事件带版本号，持久化通过 `store` 接口（D8）。
- 默认循环保持串行同步；并发、后台任务和外部执行必须有明确接缝、owner 边界、取消语义和验收用例（D5）。
- LLM 适配器支持 SSE 流式响应；Provider 切换不能破坏历史回放、reasoning 或附件引用。
- 知识库检索与对话模型解耦；现有 KB 接缝采用可替换 Provider，具体内容层不属于本项目当前范围（D9）。

## 3. 项目定位与范围

这是一个 Go 实现、借鉴 DeepSeek Harness 架构的个人 Agent。核心由会话日志、LLM 适配、工具注册表、提示词组装和循环组成，通过能力接缝扩展调度、规划、记忆、审批、沙箱、MCP、Web 等能力。

参照源：

- Agent 架构与能力：`../deepseek-harness`
- KB 接缝的历史参照：`../dsh-knowledge`（仅用于已有接口/决策背景；KB 内容层由其他项目负责）

明确不在本项目范围内：

- 运行时插件、动态加载插件、运行时自修改执行协议；
- KB 全量内容层、批量/大文档导入及可恢复导入 Job；
- 未经用户确认的行为删减、兼容性破坏或“顺手重构”。

## 4. 已交付能力

| 阶段 | 已交付内容 | 状态 |
|---|---|---|
| M1 | REPL、DeepSeek 流式 LLM、会话日志、工具注册表、串行循环 | ✅ 已完成 |
| M2 | SQLite 持久化、多会话、提示词分节、配置和重试 | ✅ 已完成 |
| M3 | 白名单、安全边界、超时、输出截断、取消和 CLI 完善 | ✅ 已完成 |
| M4 | KB 接缝、FTS5 检索、召回/添加工具、提取回写 | ✅ 已完成；内容层移交外部项目 |
| M5 | 后台任务、子代理、上下文压缩、技能 | ✅ 已完成 |
| M6 | 定时调度、Goal/Plan/Todo、长期记忆、人工审批、代码沙箱、MCP/fs | ✅ 已完成 |
| M7 | Web 搜索接缝、DeepSeek 官方搜索、HTTP fetch、`web_search`/`web_fetch` | ✅ 已完成 |
| M8 | content parts、多 Provider、reasoning 回传、多模态附件引用 | ✅ 已完成 |
| M9 | Windows-first 持久终端、五件套终端工具、owner 隔离、`/term` | ✅ 已完成 |
| 评测与模式 | 任务评测接缝、`standard/minimal/code` 模式、fs-search、workflow、Ralph、外部 subagent Provider | ✅ 已完成 |
| M10 | Go `net/http` Web 工作台：聊天、会话、SSE、设置、运行状态、主题、反馈、图片附件 | ✅ 已完成 |

### 4.1 已知实现边界

- Windows 上持久终端使用 `cmd.exe /Q` 管道，不是真正的 ConPTY；不承诺全屏 TUI，`Ctrl+C` 仅尽力而为，孙进程残留风险需保留文档说明。
- M10 使用 Go 标准库和嵌入式前端，不复制 dsh 的运行时插件/UI slot 体系；以功能和行为对齐为目标。
- 评测接缝提供评测能力，但“不合格后重派”由模型组合工具驱动，不在核心 loop 内自动递归重派。
- 图片落库只保存引用，原始字节按请求或附件接口读取；纯文本模型遇到图片必须 fail-closed。

## 5. 新工作如何进入项目

当前没有已批准但未完成的里程碑。提出新需求时按以下顺序处理：

1. 说明目标、是否改变现有行为、是否涉及 dsh 对齐或范围边界。
2. 阅读 `../deepseek-harness` 对应源码、README 和子系统文档；必要时记录差异规格。
3. 涉及核心模型、事件、循环、依赖方向或安全边界时，先新增/更新 `docs/decisions/` ADR，并同步 `docs/design.md`。
4. 先写或更新测试，再实现；能力按接缝拆分为 Service、Provider、Tool。
5. 完成后至少运行相关包测试，并运行 `go vet ./...`、`go test ./...`、`go build ./...`。
6. 验收时检查事件日志、历史派生、工具 schema、取消/重启/失败路径、权限边界和 dsh 偏差，不以“能编译”为唯一完成标准。
7. 通过验收后只更新本文件的“当前结论”和“已交付能力”；详细过程留在 ADR/dispatch 文档中。

## 6. 开发与验收流程

### 6.1 控制面与实施面

- 控制面：定义契约、范围、验收标准，审查 diff，运行最终验证并更新文档。
- 实施面：阅读本文和 `docs/design.md`，按指定范围改代码、自测并报告改动文件、实现决策、命令和结果。
- 同一里程碑原则上只安排一个实施会话；必须并行时按包目录划分写入所有权。
- 实施报告不是验收依据；控制面必须独立检查代码和测试。

### 6.2 交接要求

交接材料至少包含：

- 目标、范围和明确不做的内容；
- 相关设计基线、ADR、dsh 参照路径；
- 改动目录和禁止触碰的目录；
- 验收标准、测试命令和预期结果。

历史派发材料位于 `docs/dispatch-*.md`，仅作为已完成工作的过程记录，不作为当前路线图。

实施会话可使用以下开场模板：

> 请先阅读 `Agent.md`、`docs/design.md` 和指定 ADR。实现指定目标前，阅读 `../deepseek-harness` 对应源码，记录必要的行为差异。严格限定在本次目标范围内；涉及删除功能、改变行为或超出范围的事项先暂停并请求确认。完成后运行 `go vet ./...`、`go test ./...`、`go build ./...`，并报告改动文件、实现决策、测试结果及未解决问题。

## 7. 常用命令

```sh
go build ./...        # 构建
go test ./...         # 测试
go vet ./...          # 静态检查
go run ./cmd/pa       # 启动 REPL（需要 DEEPSEEK_API_KEY）
go run ./cmd/pa --web-only  # 仅启动 Web（需配置 web_server.enabled）
```

API key 示例（不要写入文件）：

```powershell
$env:DEEPSEEK_API_KEY = "..."
go run ./cmd/pa
```

## 8. 文档与参考入口

### 8.1 本项目文档

- 设计基线：[`docs/design.md`](docs/design.md)
- ADR：[`docs/decisions/`](docs/decisions/)
- 派发/交接记录：[`docs/`](docs/)
- M4 调研背景：[`docs/research-m4-kb.md`](docs/research-m4-kb.md)

### 8.2 dsh 参考文档

- 架构：[`../deepseek-harness/docs/architecture.md`](../deepseek-harness/docs/architecture.md)
- 核心循环：[`../deepseek-harness/docs/subsystems/core.md`](../deepseek-harness/docs/subsystems/core.md)
- 会话日志：[`../deepseek-harness/docs/subsystems/session.md`](../deepseek-harness/docs/subsystems/session.md)
- 能力接缝：[`../deepseek-harness/docs/capability-seams.md`](../deepseek-harness/docs/capability-seams.md)

### 8.3 源码对照表

| 本项目 | dsh 参考 | 重点 |
|---|---|---|
| `loop` | `core/agent-loop/` | turn/step 生命周期、取消和循环驱动 |
| `session` / `store` | `core/session/`、`session/session-persistence*` | 事件日志、历史派生、重启恢复 |
| `tools` | `core/tools/` | 注册、schema 校验、执行管道 |
| `prompt` | `core/system-prompt/` | 提示词分节和能力注入 |
| `llm` | `llm/llm/`、各 provider 包 | 流式协议、消息模型、Provider 路由 |
| `jobs` / `subagent` | `packages/jobs/`、`packages/subagent/` | owner 边界、后台生命周期、父子会话 |
| `compaction` / `skill` | `packages/compaction/`、`packages/skill/` | 压缩、技能发现和按需加载 |
| `schedule` / `goal` / `plan` | `packages/schedule/`、`goal/`、`plan/`、`todo/` | 调度与任务状态 |
| `code` / `mcp` / `fs` | `packages/code-runtime/`、`mcp/`、`fs/` | 外部执行、MCP、工作区边界 |
| `web` / `terminal` | `packages/web/`、`packages/shell/`、`packages/terminal/` | 搜索、fetch、持久 shell |
| `internal/webserver` | dsh Web/UI 相关包 | Web 工作台功能与行为，不复制运行时插件模型 |

## 9. 文档维护规则

- 本文件只保留当前有效的规则、范围、状态、入口和交接要求。
- 提交哈希、逐轮测试日志、已关闭问题和详细实现过程放在 ADR/dispatch/提交记录中。
- 完成状态只在一个地方维护：本文件第 1、4 节；不要同时维护多份路线图副本。
- 每次范围或行为变化都同步检查 `docs/design.md`、相关 ADR 和本文件链接。
- 如本文与 `docs/design.md` 冲突，以最新的用户确认和 ADR 为准，并立即修正文档冲突。
