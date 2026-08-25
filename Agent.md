# Agent 工作指南

本文是 `shutu-agent` 的当前工作入口。项目目标是实现一个遵循 dsh 架构原则的纯 Go Agent：薄核心、日志即事实、能力通过接缝接入。

## 当前状态

- M1–M10 已完成并通过对应验收：REPL、持久会话、工具安全边界、后台任务、子代理、压缩、技能、调度、计划、spill、审批、代码沙箱、MCP、文件操作、Web 搜索、终端、评测、模式预设和 Web 工作台。
- 项目只保留 dsh 对齐的 Agent 能力；不运行时加载插件，不在运行时自修改执行逻辑。
- 新能力必须先检查 `../deepseek-harness` 是否已有对应实现，再通过明确的 Service / Provider / Tool 接缝接入。
- 当前没有已批准但未完成的里程碑。

## 不可违反的边界

- 删除或改变既有行为前必须取得用户确认；已确认的变更在提交说明中标注。
- API key 只允许来自环境变量，不写入配置、代码、日志或提交。
- 会话使用追加式事件日志；历史由日志派生，不维护第二份权威状态。
- 工具必须经过统一注册、schema 校验、白名单、超时和输出边界。
- 默认关闭高风险能力；只通过显式白名单启用。
- 核心循环保持同步、可取消、可测试；并发、后台任务和外部执行必须有明确的 owner、取消语义和验收用例。
- Go 代码保持 CGO-free；外部服务通过已有接缝接入。

## 工作流程

1. 先阅读本文、`docs/design.md` 和相关 ADR。
2. 对照 `../deepseek-harness` 的源码、README 和子系统文档，记录行为差异。
3. 涉及核心模型、事件、循环、依赖方向或安全边界时，先更新 ADR 和设计基线。
4. 先补测试，再实现；能力按 Service、Provider、Tool 拆分。
5. 完成后运行相关测试，以及 `go vet ./...`、`go test ./...`、`go build ./...`。
6. 验收同时检查事件日志、历史派生、schema、取消、重启、失败路径、权限边界和 dsh 对齐情况。

## 常用命令

```sh
go build ./...
go test ./...
go vet ./...
go run ./cmd/pa
go run ./cmd/pa --web-only
```

API key 示例：

```powershell
$env:DEEPSEEK_API_KEY = "..."
go run ./cmd/pa
```

## 文档入口

- 设计基线：[`docs/design.md`](docs/design.md)
- ADR：[`docs/decisions/`](docs/decisions/)
- 历史派发与交接：[`docs/`](docs/)
- dsh 参考：`../deepseek-harness/docs/`

当前状态只在本文维护；详细实现过程留在 ADR、派发记录和提交记录中。
