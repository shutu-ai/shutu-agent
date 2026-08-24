# P2 ACP session-owned runtime

日期：2026-08-24

## 决策

ACP 不再调用 REPL 的 `app.newSession` / `app.runTurn`，因为这两个入口依赖进程级 `currentID`、`log`、registry owner 和全局 turn lock。ACP factory 为每个 `session/new` 创建独立 runtime：

- 独立 session ID、`session.Log` 和 durable append sink；
- 独立 cwd，并写入 session header；
- 独立工具 registry、spill owner 与 system-prompt builder；
- 独立 prompt busy/cancel/close 状态；
- 独立 loop 实例，可在不同 ACP session 间并行运行。

## 能力边界

当前 ACP runtime 只注册无 app 会话闭包的 `get_time` 与 cwd-bound `read`。jobs、schedule、terminal、MCP、subagent、KB recall、compaction、approval 和其他捕获全局 app 状态的工具暂不复制到 ACP registry。这样多会话隔离是真实的，未注入的能力会自然缺席，而不是共享错误的当前会话。

ACP 仍只通过 committed `assistant/message` 发送文本更新；协议层的多 session 已支持，`session/prompt` 每个 session 仍保持单请求串行，session 之间可以并行。

## 后续

下一步若要开放高级工具，应把每个工具的 session 依赖改为显式 runtime 参数或 session-bound service，再逐项加入 ACP capability profile。不能直接复用 REPL 注册闭包。
