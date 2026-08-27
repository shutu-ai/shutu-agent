# DSH 工具能力对齐：P8 最终核验

P8 已完成最终工具目录与模式边界核验：

- 标准模式：保留已注册的 DSH 对齐工具，排除仅 PTC 使用的 `run_code`，并包含 `str_replace_editor`。
- PTC 模式：模型外层只看到并执行 `run_code`；其 TypeScript `tools.*` 内部绑定使用标准模式工具集，包含 `str_replace_editor`。
- 极简模式：只保留平台 Shell 与 `str_replace_editor`，不暴露 Session Query、Workflow、Subagent、Goal/Todo、搜索、MCP 等扩展能力。
- 已核验 DSH 命名迁移：`goal`/`todo`、`subagent`/`fork`/控制工具、`workflow`、`str_replace_editor`、`session_*`；旧 `plan_*`、`eval_*`、`spill_*` 不进入新模式工具面。
- 已核验工具执行边界：模型面向参数 schema、结构化结果、错误分类、取消/超时、并发安全分类和 PTC 内部调用策略均有对应测试覆盖。
- `deepseek-harness` 目录本轮及此前均未修改，仅作为只读参考。

最终验证：

- `go test ./...`

