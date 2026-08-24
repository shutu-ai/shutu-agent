# P2 session-query 第一阶段决策

日期：2026-08-24

## 范围

本阶段实现 dsh `tool-session-query` 的五个只读工具：

- `session_search`
- `session_event_search`
- `session_trace`
- `session_event_trace`
- `session_event_read`

工具只读取现有 `store.Store` 的已提交会话元数据和事件，不修改 event log，不进入 loop，也不新增事件类型。

## 实现决策

1. 复用 SQLite `SearchSessions` 和 `LoadSession`，保持 D1 单一事实源与 D8 重放语义。
2. 结果有数量、窗口和文本摘要上限；`session_event_read` 默认只读目标事件，邻居由 `before`/`after` 显式请求。
3. 查询能力默认关闭，`session_query.enabled: true` 时由配置自动加入五个工具白名单；minimal 模式强制关闭。
4. 组合根只注入 Store 和当前 session id，工具包不依赖 cmd/pa 或 loop，保持 Service / Consumer 边界。

## 已知偏差

本地 `SessionMeta` 尚未保存 dsh 的 cwd、完整 parent session header 和 live/persisted 双源信息，因此本阶段：

- 跨会话搜索已按 `WorkspaceID + cwd` 做同项目隔离；旧库没有 cwd 的会话会在有 cwd 的调用方视角中被隐藏；
- session trace 已从同 workspace 的 `subagent/start` 事件派生 parent/descendant，缺少 header 的关系仍返回边界提示；
- event trace 识别现有 `surfaceOp.replace`，并从 assistant/tool 的 call ID 派生 source/derived 关系；仍没有 live event source，因为本地事件模型没有该字段；
- SQLite 搜索增加了 offset/limit provider 分页；模型面向的工具仍收集页并返回 bounded 结果，因此不暴露内部 cursor；
- 该能力保持 opt-in，后续补齐 session header/权限字段后再对齐 dsh 的 cwd、live/persisted 和完整 lineage 过滤。

## 验收

单元测试覆盖：空白/NUL 查询拒绝、跨会话搜索、会话内事件搜索、事件窗口边界和替换关系输出。全量 `go vet ./...`、`go test ./...`、`go build ./...` 作为本阶段交付检查。
