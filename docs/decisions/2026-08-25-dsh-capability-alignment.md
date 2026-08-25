# dsh 能力协议对齐补充

状态：已实施

本次补充保留 shutu 的编译期扩展边界；其余已发现的行为协议按 dsh 收敛：

- Skill 工具规范名为 `skill`。模型目录只展示 `model-invocable` 技能，工具入口再次执行该策略校验；用户 `/skill-name` 仍使用独立的 user-invocable 通道。
- Skill provider 可以在候选项上发布 invocation policy；未发布 policy 的第三方 provider 采用 dsh 默认值（模型和用户均可调用）。
- Ralph 使用 `maxRounds` 参数和 `runId/agentsStarted/result` 结构化结果。每轮必须返回完整的 `continue|complete|blocked` handoff，字段为 `summary/evidence/nextSteps/blocker`，严格校验未知字段、状态语义和 16K 上限。
- Ralph 下一轮携带上一轮完整结构化 handoff；自由文本和 `DONE:`/`BLOCKED:` 标记不再作为成功协议。

以下仍属于架构边界而非行为偏差：运行时 Cordis plugin/bundle/HMR、自修改运行时扩展和 dsh 的动态 provider 生命周期不在 Go 编译期 Service/Provider/Tool 设计内。
