# DSH 工具能力对齐：P7 Session Query

P7 已完成，五个只读 `session_*` 工具已按 DSH 的模型调用边界补齐：

- `session_search` 支持会话 ID、创建时间、父会话、根会话、availability、事件序号/时间、事件类型和 event surface 过滤。
- `session_event_search` 支持目标会话、事件序号/时间、类型和 surface 过滤；搜索当前会话时严格排除当前 step 及其自身调用。
- 两个全文搜索工具不暴露模型可控 `limit`，由部署配置固定最多 100 条结果，并使用 30 秒协作超时。
- `session_trace`、`session_event_trace`、`session_event_read` 支持 DSH 并发安全分类；全文搜索保持 exclusive。
- 追踪结果补齐 availability、replacement chain、surface；事件读取保留完整目标事件和可选邻居。
- 所有过滤数组、时间戳、序号范围和 surface/availability 枚举均在工具边界校验。
- `minimal` 模式仍强制关闭 Session Query；标准模式按配置启用并自动加入白名单。

验证：

- `go test ./internal/sessionquery ./internal/config ./cmd/pa`
- `go test ./...`

