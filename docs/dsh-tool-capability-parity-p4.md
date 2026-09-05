# DSH 工具能力对齐：P4 实施记录

P4 已完成：

- `subagent`：支持 DSH 的 `description`、`prompt`、`run_in_background` 参数及 foreground/continuable/background 结构化结果。
- `subagent_fork`：使用独立 `fork` provider，默认一次性前台执行，并支持从父会话事件历史建立上下文种子。
- `send_message`：改用 `subagent_id`，只允许直接子级，返回 `{messageId}`。
- `interrupt_agent`：改用 `agent_id`，允许直接及更深层后代，已完成目标按 DSH 作为可接受空操作，返回 `{accepted:true}`。
- `list_agents`：替换模型侧旧列表能力，支持 `children`/`descendants`，只投影可继续子代理并返回结构化状态。
- 结算通知：子代理结束时自动写入 `subagent/end`，不再依赖轮询 `status`。

验证：

- `go test ./internal/subagent ./cmd/sta ./internal/config`

