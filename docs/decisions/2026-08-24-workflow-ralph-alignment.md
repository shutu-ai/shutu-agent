# Workflow/Ralph 协议对齐决策

状态：已定

## 背景

`deepseek-harness` 的 `workflow` 工具执行模型编写的 JavaScript script，依赖 workflow engine、worker thread、`meta/script/args` 请求和 `workflow/*` 生命周期。当前 Go 实现只有固定 JSON task DAG，缺少动态脚本编排能力。

## 决策

1. Ralph 对齐 dsh 的结构化跨轮报告：支持 `continue|complete|blocked`、`summary`、`evidence`、`nextSteps` 和 `blocker`，按状态校验字段语义，单次 handoff 上限为 16K 字符。
2. 保留旧 `DONE:` / `BLOCKED:` 回复以及旧 `result` / `handoff` JSON 作为兼容输入，但新 worker prompt 优先要求 dsh-compatible 报告。
3. 以 dsh JavaScript workflow 为完整能力目标：支持 `meta/script/args`、`agent()`、`parallel()`、`pipeline()`、`phase()`、`log()` 及 workflow 生命周期事件。
4. Go 核心不直接依赖 Node.js；通过外部 Node runtime/provider 按需执行 JavaScript。个人 Agent 当前采用与 dsh 相同的信任模型，JavaScript workflow 默认启用，不因尚未具备 OS 级沙箱而关闭完整能力。
5. 现有 Go-native structured DAG 保留为原生/兼容路径，但不再作为 dsh workflow 的能力替代；两种执行形态的 schema、结果和生命周期必须明确区分。
6. Node worker/process 的职责是执行隔离、取消、资源上限、RPC 和结果收敛，不宣称它本身构成安全沙箱。真正不可信脚本的 OS 级隔离另列后续能力，不阻塞当前 dsh 功能复刻。

## 后果

- Ralph 的报告更适合被父 Agent、评测和后续持久化消费，且旧会话/旧 worker 不回归。
- Go-native DAG 与 dsh script workflow 的协议差异对模型可见，避免两种执行形态互相误解。
- JavaScript workflow 默认依赖目标环境存在可用 Node runtime；Node 不可用时应报告 workflow provider 不可用，不影响 Go 核心启动和其他 Agent 能力。
- 当前允许采用 dsh 的信任模型换取能力完整性；OS 级安全沙箱不作为本阶段验收条件。

## 当前实现复核（2026-08-24）

- 已实现外部 Node runtime：Go 通过 JSONL RPC 启动一次性 Node 进程，Node 侧使用 `vm` 执行脚本；Go 核心编译和启动不依赖 Node。
- 已实现脚本 API 与生命周期：`meta/script/args`、`agent`、`parallel`、`pipeline`、`phase`、`log`，以及 `workflow/start|phase|log|agent-start|agent-end|end` 事件；支持取消、agent 并发/总量、组合器 item 上限和同步执行超时。
- 已保留 Go-native task DAG，并让 `workflow_run` schema 明确区分 script 与 tasks 两条路径；workflow 默认启用，显式 `enabled: false` 和 minimal 模式仍可关闭。
- 已补齐 structured output：`spawn` 为带 `OutputSchema` 的子 Agent 创建独立 registry scope，注入 `structured_output` 工具；现有 D7 JSON Schema gate 校验工具参数，子 Agent 只在成功调用该工具后向 workflow 返回结构化对象，父 registry 不会看到该临时工具。Node runner 侧也校验 schema/选项并在未捕获结构化结果时返回 `null`，与 dsh 的 `agent({schema})` 语义一致。
