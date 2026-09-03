# ADR 2026-08-20：标准模式缺口补齐（fs-search / workflow / ralph / subagent 外部 provider）

- 状态：已接受
- 日期：2026-08-20
- 决策驱动者：用户要求「还有标准模式的缺口需要实现」——补齐 2026-08-20 能力面核对中列出的四个标准模式缺口。

## 背景

对照 dsh `standard` 预设（`apps/cli/config/agent-presets/standard/agent.cordis.yml`），github.com/shutu-ai/shutu-agent 缺四项：文件内容检索（dsh `tool-fs-search`）、多 agent 编排（dsh `tool-workflow`）、fresh-agent 循环（dsh `tool-ralph`）、subagent 外部 provider 变体（dsh `subagent_codex`/`subagent_claude_code`）。全部按编译期接缝实现（config 驱动、重启生效、零新依赖、CGO-free、loop.go 不动 D4）。

## 决策

### D-GAP-1 fs-search（文件内容全文检索）
- 新包 `internal/fssearch`：递归目录扫描 + 文件内容匹配（默认子串；`regex: true` 时按正则）。`Search(ctx, query, opts) ([]Hit, error)`，`Hit{Path, Line, Text}`。
- 有界与安全：跳过忽略目录（`.git`/`.hg`/`.svn`/`node_modules`/`vendor` 等）；跳过二进制文件（前 8KB 含 NUL）；超大文件跳过（`max_file_bytes` 默认 1 MiB）；命中上限（`max_results` 默认 50）；扫描文件数上限（`max_files` 默认 20000）；大小写默认不敏感（`case_sensitive` 可选）。
- 工具 `fs_search`：schema `{path?, query, pattern?, regex?, max_results?, case_sensitive?}`。输出 `Path:Line: text` 行 + 命中计数；超上限截断注明。
- config：独立 cap `fs_search.enabled`（默认关 D10）+ applyDefaults 白名单追加 `fs_search`；**Mode-1 minimal 分支同步关闭 `FsSearch.Enabled`**（minimal 不含搜索，需在 fs-search 落地时补进 config.go 的 minimal 分支）。
- 诚实边界：dsh fs-search 走 fs service 的统一策略；本项目是独立只读搜索工具，无写路径。

### D-GAP-2 workflow（多 agent 编排，JSON DAG）
- 新包 `internal/workflow`：**声明式 DAG 编排**（用户 2026-08-20 拍板「JSON DAG 声明式编排」）。模型提交 `tasks[]`（每项 `{id, prompt, depends_on: []string, description?}`），引擎拓扑排序 → 无依赖任务并发 spawn 子代理（复用 subagent spawn 能力，组合根注入）→ 依赖满足后派发 → 汇总裁决。
- API：`Engine` 持有 `Spawn func(ctx, prompt string) (Result, error)`（或 subagent 接口）；`Run(ctx, spec Spec) (Report, error)`；`Spec{Tasks []Task}`；`Task{ID, Prompt, DependsOn []string}`；`Report{Tasks []TaskReport}`（`TaskReport{ID, Status, Output, Error}`）。
- 并发：`max_concurrent` 默认 4（D5 串行 loop 内，workflow 一次执行是单工具调用，内部并发；并发上限可配）。
- 循环依赖检测：拓扑排序前检测，环形 → error（fail-closed）。
- 工具 `workflow_run`：schema `{tasks: [{id, prompt, depends_on?}]}`（JSON 数组直接入参）。输出逐任务结果摘要（id/status/output 头）；D3 事件 `workflow/run`（只记元数据：任务数/成功/失败/时长，不落输出全文）。
- config：独立 cap `workflow.enabled`（默认关 D10）+ 白名单；**minimal 分支同步关闭**。
- 诚实边界：dsh workflow 是模型写 JS 脚本（agent/pipeline/parallel hooks）；本项目无 JS 引擎，用 JSON DAG 等价表达依赖与并发（用户已确认）。不含 `phase`/`log` 等 UI 语义。

### D-GAP-3 ralph（fresh-agent 循环）
- 新包 `internal/ralph`：不可变 objective 的多轮 fresh-agent 循环。`Engine` 持有 spawn 能力；`Run(ctx, objective string, maxRounds int) (Report, error)`。
- 每轮：spawn 一个**全新子代理**（无父会话继承、无先前轮次会话），注入 objective + 上轮的结构化报告（跨轮桥，作为长期记忆的替代）；worker 报告完成 / 具体阻塞 / 轮上限。报告有界（不携带全量对话）。
- 工具 `ralph`：schema `{objective, max_rounds?}`（默认 3）。输出最终报告。
- config：独立 cap `ralph.enabled`（默认关 D10）+ 白名单；**minimal 分支同步关闭**。
- 诚实边界：与 dsh tool-ralph 同语义（fresh 每轮、共享工作区为长期记忆、有界报告跨轮）。

### D-GAP-4 subagent 外部 provider 变体
- `internal/subagent` 的 spawn 增加外部 provider 支持（用户 2026-08-20 拍板「可选、默认关、CLI 探测」）：`subagent_spawn` 增加 `provider` 字段（`spawn` 默认 | `codex` | `claude-code`）。
- 外部 provider：`exec` 启动外部 CLI（`codex exec --json <prompt>` / `claude -p <prompt>` 风格），stdin 传 prompt、stdout 收结果；CLI 不存在（exec.LookPath 失败）→ fail-closed（工具返回明确错误，不静默回退本地）。
- config：`subagent.external_providers: {codex: {enabled: false, command: "codex"}, claude_code: {enabled: false, command: "claude"}}`（默认关 D10）；provider 白名单推导（启用才可被选中）。
- 子代理 spawn 时 provider 未启用/未知 → fail-closed。
- 诚实边界：外部 CLI 行为由各 CLI 决定（本项目不保证 codex/claude 具体协议，只做 prompt→stdout 桥）；Windows 未装 CLI 则不可用（文档记录）。max_depth 等策略沿用现有 subagent 框架。

## 后果
- 标准模式四缺口全部补齐，`standard`/`code` 模式能力面与 dsh standard 对齐（workflow/ralph/fs-search 均为可启用 cap，默认关 D10，用户经 config 开启）。
- 三个新 cap（fs_search/workflow/ralph）均进入 D-MODE-2 minimal 分支的关闭列表（minimal 只留持久 shell + 文件编辑）。
- 全部零新依赖、CGO-free、loop.go 不动（D4）；编排/循环在单工具调用内串行执行（D5）。
