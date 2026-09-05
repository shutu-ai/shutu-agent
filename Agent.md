# Agent 工作指南

本文维护 `shutu-agent` 的稳定开发约束、审计边界和验证流程。它不是进度日志；当前等价状态、任务状态和发布阻断项一律以 [`docs/equivalence-manifest.yaml`](docs/equivalence-manifest.yaml) 与 [`docs/equivalence-task-register.yaml`](docs/equivalence-task-register.yaml) 为准。

## 项目范围

- Shutu Agent 是独立的 Go Agent 运行时。DSH 是 pinned 行为参考，不是运行时依赖。
- `.reference/dsh` 是项目内只读参考目录，禁止修改、导入或打包。参考版本由 equivalence manifest 固定。
- 必须保持的外部可观察核心包括：turn/step waterfall、持久会话与 replay、工具策略、审批、沙箱/代码运行时、子代理与 owner、跨进程恢复，以及 ACP/MCP/SDK/Web 生命周期。
- Team、E2B、Python runtime、dynamic Cordis runner、client modules/HMR、Codex provider 和 Claude provider 属于 profile 级可选或明确排除能力。它们只能以 manifest/profile 中的 optional、unsupported 或 architecture-excluded 身份出现，不能伪装成已支持。
- 当前主要交付平台是 Windows。其他平台只有在 manifest 声明为 claimed platform 且对应验收通过后才进入发布口径。

## 当前状态口径

- 不要在本文件追加按日期的状态流水。状态变化必须同步更新 task register、manifest 和必要的 status/evidence 文档。
- 阅读结论前先运行或查看 equivalence gate；不要用旧报告、测试名存在或能力目录存在推断当前通过状态。
- 一项只能在这些条件下标记为 done：实现存在、负路径证据存在、验收命令可复现通过，且 task register/manifest/status 没有矛盾。
- `claimAllowed: true` 只表示 pinned 参考范围内的等价验收通过。新增能力、平台、profile 或安全边界时必须重新评估范围和证据。

## 不可违反的边界

### 架构与状态

- 遵循薄核心、事件日志为事实、能力通过明确 Service/Provider/Tool 接缝接入。
- 会话历史只有一份权威来源：append-only durable event log。projection、缓存、运行时内存和 UI 状态都从它派生。
- durable log 使用封闭事件词汇。live append、atomic append、persistence incorporation、restore 和 replay 都必须在推进序号前拒绝未知事件。native wire 的 `ignorable:true` 不是持久化未知事件的通道。
- 会话采用 owner/addressed runtime context。并发 Agent、子代理、job、approval、provider generation 和 MCP connection 不得共享可变全局状态。
- Agent publication、disposal、session creation、approval、credential rotation、job completion 和外部协议 receipt 必须是原子事务或明确的可恢复协议。

### 能力与失败语义

- 配置偏好、catalog 条目、路由存在或 metadata 声明都不是能力证明。能力广告必须来自与执行相同的真实 host probe。
- 不支持、未启用、不可探测或权限不足的能力必须返回稳定失败并保持 startup 健康；禁止静默降级、隐式 fallback 或把 full access 报告成 sandbox。
- 外部副作用遵循 at-most-once 或显式 retryable receipt 语义。连接错误、超时、进程死亡或重启后不得自动重放可能已提交的调用；恢复由 owner/supervisor 显式完成。
- 进程树从启动到退出必须有 owner。Windows command、PowerShell、background job 和 Code Mode 相关树由 Job Object 约束；Job Object 拒绝时使用有界 descendant kill fallback 并保留平台测试。只杀直接子进程不等于关闭进程树。
- API key 和凭据只允许通过环境变量或显式 credential vault 进入。不得写入配置、代码、日志、事件、测试产物、发布包或提交；受控子进程必须 scrub credential-shaped environment。

### Projection 与控制面

- CLI、Native Web、ACP、SDK 和模型 admission 共享 canonical projection snapshot。plan/goal/todo/permission/sandbox/session metadata 不得使用 transport-local 解析或第二份权威状态。
- malformed durable control fact 必须失败重建，不能把非法布尔、缺失枚举或损坏状态解释成 inactive/default。
- `session/title` 是 canonical event operation。用户重命名、fallback title 和 provider title 共用同一 append 权威；SQLite projection、history、mux 和 cold restart 不得看到不同标题。
- session list 的 blank、prompt recency 和 `updated_at` 由共享 session metadata projection 决定。physical last append、event count 或 transport shortcut 不能替代 human prompt fold。
- bounded event tail 只能用于 sidebar 类元数据；创建或复用 full session revision checkpoint 前必须 exact-revision replay 完整原始会话。
- approval restoration 是控制面 projection，读取 raw committed event seam，不因启动恢复重放无关 session lifecycle。实际打开 session 时仍保持严格 lifecycle 和 interrupted-tail recovery。

### 工具与取消

- 所有模型可见工具经过统一 registry：schema validation、policy whitelist、approval/pre-hook、timeout、output boundary、result observer 和 durable tool result。
- `Prepare` 必须固化已解码参数，防止 caller 在授权和 dispatch 之间改变请求。prepared 调用进入 tool body 前重新检查 cancellation。
- pre-cancelled 调用在 whitelist、schema、approval 和 pre-hook 前短路，返回 `ABORTED_BEFORE_DISPATCH`；argument materialization 错误保留原有优先级。
- late cancellation 按 body 是否已启动分类：已启动成功调用返回 `ABORTED` 并保留 deferred context；wrapper short-circuit 返回 `ABORTED_BEFORE_DISPATCH`。
- specific tool/wrapper failure 优先于 concurrent caller cancellation；只有 plain context cancellation 转换为 abort code。
- process-backed cancellation 必须在 tool boundary 显式归类为 `AbortError/ABORTED`，不得从平台 exit-status 文本推断。
- 每次直接 registry call 恰好向 result observer 发送一个 terminal result，包括 unknown tool、argument、policy、stale generation 和 pre-dispatch cancellation failure。

### Provider 与模型

- provider/model route 在构造 loop 前解析；explicit route 不得继承全局模型的能力或输出预算。
- durable model-visible history 包含 image block 时，text-only route 必须在写 session configuration 前返回稳定 `model-unavailable`。unclaimed pending inbox 中的 image prompt 已经是 model-visible input，不能绕过 admission。
- 模型能力只能来自 provider-owned catalog 或显式 pinned fact。禁止把 ID-only suggestion 转成 context window、max output、reasoning、tools、vision 或 audio 能力。
- audio、resource 和 vendor block 可在 durable log 中保留，但当前声明不支持它们的 provider 必须在 credentials、attachment offload 或网络 I/O 前拒绝请求。
- reasoning effort、thinking budget、default max tokens 和 route-level budget 通过 canonical Loop/ChatRequest/catalog seam 传递；不得在 transport-local 默认值中覆盖 composition-level 设置。
- Provider wire 必须验证 SSE 完整性、保留同一 payload 的 delta、规范 usage 字段、区分 timeout 与 caller abort，并保留 route-aware 错误诊断。

### ACP、MCP 与 SDK

- ACP、MCP 和 SDK wire 格式允许不同，但 session/tool/terminal fact 的语义必须共享同一 canonical authority。
- ACP session log 是 session runtime log。title、prompt 和 lifecycle 共享一个 append authority；resume 原子替换 runtime log，并从 canonical projection cursor 派生恢复位置。
- ACP client EOF 是 ownership boundary：`session/new` 提交后断开必须 cancel and close owned session 恰好一次。unknown session cancel 保持幂等，不得把 stale cancellation 报成协议参数错误。
- ACP transport teardown 必须在 join prompt worker 前 cancel server-initiated permission request，避免异常输入管道关闭后 server 永久等待。
- MCP `tools/call` 的 connection error/timeout 不能触发同步 retry。replacement generation 不得接收 replayed call；dynamic `mcp_call` 先发现当前 task metadata，`taskSupport=required` 必须在 side-effect boundary 前拒绝。
- MCP reconnect 默认保持 pinned reference 行为：500ms 初始 delay、30s 最大 delay、10 次 attempt。
- SDK line transport 忽略 malformed JSON line，不产生无法关联的 null-id parse error。每个注册后 abandoned request 必须在 timeout、late response 或 close 时 settle，pending set 不得残留。
- SDK `session/event` 输出 reference envelope，不是内部 storage shape。`surfaceOp`、`sourceEventSeqs`、opaque data、generated schema 和 client type 必须同步。

## 开发流程

1. 若仓库存在 `.codegraph/`，先使用 `codegraph explore "<symbols or question>"` 理解符号与调用路径，再用 `rg` 或直接阅读源码验证当前磁盘状态。
2. 先读 [`docs/design.md`](docs/design.md)、相关 ADR 和 [`docs/dsh-equivalence-contract.md`](docs/dsh-equivalence-contract.md)。需要对照参考行为时，只读 pinned `.reference/dsh`。
3. 涉及事件词汇、核心 loop、依赖方向、安全边界、协议语义、profile 范围或 release claim 时，先补测试，再实现，并同步 ADR/design/manifest/register。
4. 最小修改；不为架构重写、可选能力或无关格式化扩大 diff。不要删除既有未跟踪测试产物，不执行未经确认的清理、删除、重置或覆盖。
5. 完成后运行相关测试和对应 gate。文档变更校验链接、JSON/YAML、profile 范围、任务状态和证据路径一致性。
6. 提交前检查 `git diff --check`、工作区状态、敏感信息、参考目录未修改，以及没有把环境失败或 skipped test 记录为通过。

## 验证命令

### Go 与 Web 基线

```powershell
go test -count=1 ./...
go vet ./...
go build ./...

cd web
npm run typecheck
npm test -- --run
npm run build
npm run verify
npm run e2e
```

### 等价与发布门禁

```powershell
go test -p 1 ./...
CGO_ENABLED=1 go test -race ./... -count=1
node scripts/verify-reference-replay.mjs

powershell -NoProfile -ExecutionPolicy Bypass -File scripts/equivalence-register-lint.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/equivalence-gate.ps1
```

严格 race、reference replay、claimed platform 或 release gate 缺少工具链/环境时记录为 `unverified` 或失败；不能记录为 `passed`。

### 本机验证环境注意

某些主机策略会把默认 `%TEMP%` 或仓库内临时目录变成不合法 fixture 环境，例如 path/symlink probe、skill discovery 或子进程访问失败。如果必须使用外部 writable root，请把 exact command、环境变量和结果记录在 evidence/manifest 中。这是验证环境条件，不能用来削弱 path、symlink 或子进程 fail-closed 检查。

## 本地运行

```powershell
$env:DEEPSEEK_API_KEY = "<your-key>"
go build -o sta.exe ./cmd/sta
.\sta.exe --config config.yaml
```

Web-only 入口：

```powershell
.\sta.exe --web-only --config config.yaml
```

其他 CLI flags 和部署/回滚流程见 [README.md](README.md)、[`docs/deployment.md`](docs/deployment.md) 和 [`docs/p36-deployment-runbook.md`](docs/p36-deployment-runbook.md)。

## 文档入口

- 状态 source of truth：[`docs/equivalence-manifest.yaml`](docs/equivalence-manifest.yaml)
- 任务登记与验收：[`docs/equivalence-task-register.yaml`](docs/equivalence-task-register.yaml)
- 等价任务说明：[`docs/dsh-equivalence-tasks.md`](docs/dsh-equivalence-tasks.md)
- 状态校正记录：[`docs/dsh-equivalence-status.md`](docs/dsh-equivalence-status.md)
- 等价契约：[`docs/dsh-equivalence-contract.md`](docs/dsh-equivalence-contract.md)
- 设计基线：[`docs/design.md`](docs/design.md)
- ADR：[`docs/decisions/`](docs/decisions/)
- Web parity 记录：[`docs/dsh-web-parity-tasks.md`](docs/dsh-web-parity-tasks.md)、[`docs/dsh-web-parity-acceptance.md`](docs/dsh-web-parity-acceptance.md)
- 部署说明：[`docs/deployment.md`](docs/deployment.md)
