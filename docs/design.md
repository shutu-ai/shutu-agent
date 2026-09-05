# shutu-agent 设计基线

## 1. 目标与边界

`shutu-agent` 是一个纯 Go、CGO-free 的个人 Agent，沿用 dsh 的三条原则：薄核心、日志即事实、能力即接缝。核心由会话日志、LLM 适配器、工具注册表、提示词组装器和 Agent loop 组成，其他能力通过明确的 seam 接入。

明确不做运行时插件加载、运行时自修改执行逻辑，以及第二份权威会话历史。API key 只来自环境变量。

## 2. 模块结构

```text
cmd/sta/              组合根、REPL、Web 与 ACP 入口
internal/loop/       turn/step 生命周期、取消、工具循环
internal/session/    追加式事件日志、历史派生、持久化
internal/llm/        流式消息与 Provider 适配
internal/tools/      注册、schema、白名单、超时、输出边界
internal/prompt/     persona、skills 与工具目录组装
internal/jobs/       有 owner 边界的后台任务
internal/subagent/   子 Agent 生命周期与报告
internal/compaction/ 上下文压缩
internal/skill/      本地技能发现和按需加载
internal/agentinstructions/ 工作区指令基线与增量
internal/sessionreference/  跨会话引用召回
internal/timecontext/       可选的持久时钟上下文
internal/webserver/  认证后的 Web 工作台与只读事件 API
```

依赖方向保持单向：组合根连接能力；loop 依赖 session、llm、tools、prompt；能力实现不得反向依赖 loop。

## 3. 会话与事件

会话采用追加式事件日志。事件拥有顺序号和版本，持久化通过 `store` 接口完成；历史、统计和 UI 投影都从日志派生。模型可见的输入先落日志，再发起请求。

turn 包含一个或多个 step：追加用户消息，执行有界的 pre-step 注入，调用流式 LLM，持久化文本和工具调用，执行工具并追加结果，直到模型完成或达到步数上限。所有路径都支持取消和 fail-open 的非关键投影。

`loop.Config.PreStep` 是统一的上下文注入接缝。注入器按注册顺序运行，可设置每轮一次、去重和字符上限；运行时快照、压缩、技能目录等能力通过此接缝组合，不修改 turn/step 结构。

`time_context` 是 DSH 兼容的可选时钟上下文。未显式配置时跟随 `schedule.enabled`；启用后每个进入的 step 在所有普通上下文之后追加 durable snapshot。读数携带带时区时间、请求浏览器时区策略和相对正确 baseline 的耗时；浏览器时区来自 native Web user-rpc provenance，queued prompt 也保留同一 provenance。

## 4. 工具与安全

所有工具经过统一注册表和 JSON Schema 校验。工具白名单、单工具超时、全局输出上限、取消传播和敏感操作审批在执行入口统一处理。超过输出上限的结果截断并写入 spill，模型只看到有界结果和定位信息。

高风险能力默认关闭，能力开关与工具白名单保持一致。外部执行必须声明 owner、工作目录、取消和失败语义；凭据不继承到受控子进程。

## 5. 提示词与 Provider

提示词按数字前缀从 `config/prompts/` 加载，当前顺序为 persona、skills、工具目录。空分节不进入最终提示词。Provider 负责流式协议和模型路由，切换 Provider 不改变会话日志和历史派生。

## 6. Web 工作台

Web 使用 Go `net/http` 和嵌入式静态资源。API 默认需要 bearer token，token 只在配置中出现明文，进程内保存摘要；空 token 且启用时拒绝启动。会话和事件 API 只读并输出有界摘要，前端不复制一份权威历史。

## 7. 验收基线

```sh
go test ./...
go vet ./...
go build ./...
```

验收必须覆盖日志持久化与派生历史、工具 schema 和白名单、取消与超时、失败路径、重启行为、权限边界，以及 Web 认证和事件摘要。

## 8. 变更规则

涉及事件类型、核心循环、依赖方向、配置语义或安全边界的变更，先更新 `docs/decisions/` 中的 ADR，再修改代码和测试。历史派发文件只保留与当前实现相关的记录；当前状态以 `Agent.md` 和本设计基线为准。
