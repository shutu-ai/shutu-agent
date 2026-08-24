# P2 ACP session-owned compaction

日期：2026-08-24

## 决策

ACP session 在 `compaction.enabled` 开启时各自创建 `compaction.BasicEngine`，并把只属于该 session 的日志作为 `SessionLike` 传入。ACP loop 的 `PreStep` 在首个模型请求前执行压力检测；需要压缩时追加原有的 `compaction/start`、summary marker、`compaction/summary`、`compaction/end` 事件。

压缩仍然是 append-only：旧事件不删除，`surfaceOp.replace` summary marker 由 `session.DeriveHistory` 折叠。压缩失败 fail-open，不阻断当前回答。

## 隔离边界

ACP 不复用 `app.compaction` 或 REPL 的 `compactionPreStep`，因为后者捕获全局 `app.log`。每个 ACP session 拥有独立 engine、日志和 loop；不同 session 的 token 压力、summary 和压缩事件不会互相影响。

terminal、MCP、subagent 等仍未接入 ACP。它们涉及持久进程、外部 server、子代理树和全局 owner，需要分别设计 session-owned service 生命周期后再开放。
