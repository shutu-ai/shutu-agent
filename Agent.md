# Agent 工作指南

本文只维护 `shutu-agent` 的稳定开发约束和工作方式。任务进度与验收结果以
[`docs/dsh-web-parity-tasks.md`](docs/dsh-web-parity-tasks.md)、
[`docs/dsh-web-parity-acceptance.md`](docs/dsh-web-parity-acceptance.md)、ADR 和提交记录为准，
不在本文重复维护历史进度。

## 项目范围

- 项目是遵循 DSH 架构原则的纯 Go Agent：薄核心、事件日志为事实、能力通过明确接缝接入。
- Web 前端使用 DSH 原生 React/Cordis 体系；前端行为、协议和数据投影应与 DSH 对齐。
- P36 当前 Windows 目标范围已完成；Linux/WSL 版本测试已取消，不再作为本轮验收门槛。
- `deepseek-harness` 是只读参考目录，严禁修改其中任何文件。
- 已授权的新功能或重构不需要兼容旧数据、旧接口或旧架构；保持干净的新接口和实现。

## 不可违反的边界

- API key 只允许来自环境变量，不写入配置、代码、日志、测试产物或提交。
- 会话采用追加式事件日志；历史状态从日志派生，不建立第二份权威状态。
- 工具必须经过统一注册、schema 校验、白名单、超时和输出边界；高风险能力默认关闭。
- 核心循环必须同步、可取消、可测试；并发、后台任务和外部执行必须有明确 owner、取消语义和验收用例。
- Go 代码保持 CGO-free；外部服务通过已有 Service、Provider、Tool 接缝接入。
- 删除或改变既有行为前先取得用户确认；已确认的变更在提交说明中标明。
- 不执行未经确认的清理、删除、重置或覆盖操作；保留用户已有的未跟踪测试产物。

## 开发流程

1. 若仓库存在 `.codegraph/`，先使用 `codegraph explore "..."` 理解相关符号和调用路径，再使用 `rg` 或直接阅读文件定位细节。
2. 阅读 [`docs/design.md`](docs/design.md) 和相关 ADR；需要对齐 DSH 行为时，以 `../deepseek-harness` 的源码和文档作为只读参考。
3. 涉及核心模型、事件、循环、依赖方向或安全边界时，先更新设计基线或 ADR。
4. 先补测试，再实现；按 Service、Provider、Tool 拆分能力，并覆盖取消、重启、失败和权限边界。
5. 完成后运行与改动相关的测试，并进行全量验证；文档变更至少校验链接、JSON/YAML 和任务状态一致性。
6. 提交前检查 `git diff --check`、工作区状态、敏感信息和 `deepseek-harness` 是否保持未修改。

## 常用验证命令

```powershell
go build ./...
go test ./...
go vet ./...

cd web
npm.cmd run typecheck
npm.cmd test
npm.cmd run verify
npm.cmd run e2e
```

API key 示例：

```powershell
$env:DEEPSEEK_API_KEY = "..."
go run ./cmd/pa
```

Windows 目标环境的发布、部署和回滚验证按
[`docs/deployment.md`](docs/deployment.md) 与
[`docs/p36-deployment-runbook.md`](docs/p36-deployment-runbook.md) 执行。

## 文档入口

- 任务清单：[`docs/dsh-web-parity-tasks.md`](docs/dsh-web-parity-tasks.md)
- 验收记录：[`docs/dsh-web-parity-acceptance.md`](docs/dsh-web-parity-acceptance.md)
- 设计基线：[`docs/design.md`](docs/design.md)
- ADR：[`docs/decisions/`](docs/decisions/)
- 部署说明：[`docs/deployment.md`](docs/deployment.md)
- DSH 参考：`../deepseek-harness/docs/`
