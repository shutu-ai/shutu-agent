# DSH 工具能力对齐任务

## Authoritative equivalence blocker index

`docs/equivalence-manifest.yaml` is the only release-status authority. Its
current release-blocking IDs are:

- A3.1
- A3.2
- A3.3
- A4.4
- A6.3
- A7.1
- A7.2
- A7.3
- A8.1
- A9.1
- A9.2
- A9.3
- A9.4
- A9.5

目标：在不修改 `deepseek-harness` 源目录的前提下，使 `shutu-agent` 的模型可见工具不仅名称一致，能力、参数协议、生命周期和异常行为也与 DSH 对齐。

## 任务清单

- [x] P1 极简模式持久化 Shell：平台 `bash/pwsh` 使用 Agent 级持久进程，保持工作目录和环境状态，补齐超时、退出、截断和 Shell 重置行为。
- [x] P2 Todo：对齐完整列表替换、重复内容校验、状态约束、Agent 归属、事件和返回文本。
- [x] P3 Goal：对齐 goal 返回结构、revision、激活/恢复、权限边界、轮次计数和 blocked 三轮规则。
- [~] P4 Subagent：已补齐主要 spawn/fork/provider 与控制面，但跨进程 owner、Team/CAS、冷恢复和完整 reference contract 仍未闭环。
- [~] P5 str_replace_editor：已有 schema/path/permission 与回归覆盖，仍需完成 reference 边界、版本校验、错误/输出和跨入口 replay 矩阵。
- [~] P6 Workflow：已有 `meta/script/args` JavaScript 执行面，仍需补齐 worker death、取消、receipt replay、child disposal 和资源限制语义。
- [~] P7 Session：已有 durable session/query 工具与 replay seam，仍需完成宿主查询边界、projection cache 和外部入口一致性核对。
- [~] P8 全量验证：本地 Go/Web/reference-core 门禁已通过，但 full contract、fault/security、strict race 和 supported-host sandbox 门禁未完成。

每个任务完成后必须运行相关单元测试和 `go test ./...`，并单独提交；本文件的 `[~]`
只表示局部实现和证据存在，不能作为完整 Harness 等价声明。
