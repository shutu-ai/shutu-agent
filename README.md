# Shutu Agent

Shutu Agent 是一个以 Go 为核心的个人 Agent 运行时，提供 CLI REPL 和原生 Web 工作台。它使用追加式事件日志作为会话事实来源，模型、工具、子代理、后台任务、技能、审批、沙箱和 Web 界面都通过明确的能力接缝组合。

当前发布版本：[v0.2.0](https://github.com/shutu-ai/shutu-agent/releases/tag/v0.2.0)。

## 特性

- **Agent loop 与持久会话**：turn/step 生命周期可取消、可恢复；历史、统计和 UI 投影都从事件日志派生。
- **原生 Web 工作台**：会话、工作区、Trajectory、模型选择、审批、队列、后台任务、子代理、技能和设置界面。
- **工具运行时**：统一 JSON Schema 校验、白名单、超时、取消、输出截断和敏感操作审批。
- **多 Provider 支持**：内置 DeepSeek、OpenAI-compatible 和 Anthropic Messages 路由；API key 只来自环境变量或凭据库。
- **扩展能力**：MCP、后台任务、子代理、本地技能、计划/目标、工作流、Web 搜索与抓取、持久终端和代码执行接缝。
- **多入口协议**：同一运行时可通过 REPL、Web、ACP stdin/stdout 和 Shutu SDK JSON-RPC 访问。

## 仓库结构

```text
cmd/sta/       组合根：REPL、Web、ACP 与 SDK 入口
internal/      Agent loop、session、tools、LLM、Web 及能力实现
web/           前端源码、vendored Shutu UI runtime、构建与验收脚本
config/        提示词资源
docs/          设计基线、等价任务、部署和验收记录
scripts/       发布包、部署 smoke 与验收工具
```

## License

Apache-2.0，详见 [LICENSE](LICENSE)。DeepSeek Harness 相关来源与必需归属见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

项目是独立构建的。`.reference/dsh` 只作为可选的本地只读参考，不进入 Go module、前端依赖或发布运行时。vendored 前端来源与授权信息见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 环境要求

- Go 1.25 或更新版本
- Node.js 24 及 npm
- 可选：PowerShell 7，用于启用 `pwsh` 工具和持久终端
- 可选：Node.js runtime，用于执行 JavaScript workflow

DeepSeek 搜索和模型路由需要：

```powershell
$env:DEEPSEEK_API_KEY = "<your-key>"
```

OpenAI-compatible 与 Anthropic 路由分别使用 `OPENAI_API_KEY` 和 `ANTHROPIC_API_KEY`。API key 不写入 `config.yaml`、日志、事件或发布包。

## 构建与运行

```powershell
go build -o sta.exe ./cmd/sta
.\sta.exe --config config.yaml
```

启动原生 Web 工作台：

```powershell
.\sta.exe --web-only --config config.yaml
```

默认地址是 `http://127.0.0.1:18099`。`config.yaml` 中 `web_server.token` 为空时只在本机开放；生产环境应设置随机 bearer token。其他入口包括：

```powershell
.\sta.exe --acp
.\sta.exe --sdk
.\sta.exe --catalog-manifest -
```

## 前端开发

```powershell
cd web
npm install
npm run typecheck
npm test -- --run
npm run build
npm run verify
npm run e2e
```

`npm run build` 生成 `web/dist`，并由 Go Web 服务按 `web_server.dist_dir` 提供静态资源。

## 后端验证

```powershell
go test -count=1 ./...
go vet ./...
go build ./...
```

更多架构和安全约束见 [Agent.md](Agent.md) 与 [docs/design.md](docs/design.md)。

## 发布包

生成交付包：

```powershell
node scripts/release-package.mjs
```

脚本会构建并校验前端，编译当前平台二进制，复制配置、提示词和部署说明，输出到 `release/`。目录结构见 [docs/deployment.md](docs/deployment.md)。

运行交付包启动、升级、回滚和数据持久化 smoke：

```powershell
node scripts/deployment-smoke.mjs
```

## 配置摘要

`config.yaml` 是默认配置基线。常用顶层开关包括：

- `llm.provider`：选择 `deepseek-official`、`openai` 或 `anthropic`
- `workspace.default_dir`：未分组会话的默认工作目录；为空时使用 `~/shutu`
- `tools.enabled`：模型可执行工具白名单
- `run_command.enabled` / `code.enabled`：受控命令和代码沙箱能力
- `web.enabled`：Web 搜索与抓取
- `web_server.enabled` / `addr` / `token`：原生 Web 工作台

高风险执行能力必须显式启用，并继续受工具白名单、超时、输出边界和审批策略约束。
