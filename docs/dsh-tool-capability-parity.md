# DSH 工具能力对齐任务

目标：在不修改 `deepseek-harness` 源目录的前提下，使 `shutu-agent` 的模型可见工具不仅名称一致，能力、参数协议、生命周期和异常行为也与 DSH 对齐。

## 任务清单

- [x] P1 极简模式持久化 Shell：平台 `bash/pwsh` 使用 Agent 级持久进程，保持工作目录和环境状态，补齐超时、退出、截断和 Shell 重置行为。
- [x] P2 Todo：对齐完整列表替换、重复内容校验、状态约束、Agent 归属、事件和返回文本。
- [ ] P3 Goal：对齐 goal 返回结构、revision、激活/恢复、权限边界、轮次计数和 blocked 三轮规则。
- [ ] P4 Subagent：对齐 spawn/fork provider、可继续子会话、上下文继承、结算通知及控制工具参数。
- [ ] P5 str_replace_editor：逐项对齐 schema、路径/权限策略、版本校验、边界输入、错误和输出协议。
- [ ] P6 Workflow：以 DSH 的 `meta/script/args` JavaScript 编排为主，补齐执行、取消、结果和异常语义。
- [ ] P7 Session：核对 DSH 的宿主查询能力与 Shutu 保留的模型可调用 `session_*` 能力，明确最终接口边界并补齐差异。
- [ ] P8 全量验证：按 standard/minimal/PTC 分别核对工具 schema、调用结果、错误分类、提示词和 Web/ACP 入口，完成最终提交。

每个任务完成后必须运行相关单元测试和 `go test ./...`，并单独提交。
