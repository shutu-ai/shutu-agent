# DeepSeek Harness 能力等价任务清单

状态约定：`[x]` 已有可执行实现和局部证据；`[~]` 有骨架或兼容桥，但仍不能宣称等价；`[ ]` 尚未完成。只有 P0、P1、P2 和门禁全部满足，才允许对外称为 capability-equivalent。

## P0：运行时根基

### T0. 等价契约与证据基线

- [x] 固化 Agent、turn、step、durable event、history、persistence、sandbox、approval、plugin、subagent、ACP/MCP/SDK 的等价边界。
- [x] 固化核心事件顺序 fixture：`turn/start → step/start → user/message → request/header → assistant/* → tool/* → step/end → turn/end`。
- [x] 为每个核心事件补充字段级 JSON Schema、版本兼容策略和 unknown-event 的 `ignorable/required` 分类；`session.ValidateWireEvent` 已在 native 投影契约测试中执行 fail-closed 校验。
- [x] 建立 reference Harness fixture 的双端 replay 对比：`TestCoreTurnReplayMatchesReference` 在设置 `DSH_REFERENCE_ROOT` 时调用 reference `Session`，比较 surface/history；未设置 reference checkout 时明确跳过。

验收：同一 fixture 在 Go runtime 和 reference runtime 上 replay 后，事件类型、嵌套、seq、derived history 完全一致；差异必须有显式 compatibility rule。

### T1. Agent / AgentHandle / Registry / Inbox / Scope

- [x] Agent opaque ID、parent lineage、live status、同步 `Run`、`Cancel`、`WhenIdle`、`Send`、`Followup`、`Steer`、`Inject`。
- [x] next-step、next-turn、steering、quiet-injection 队列及顺序测试。
- [x] parent/child scope 继承、disposer 逆序和 parent-close 级联释放。
- [x] parent context 取消时关闭 Agent、唤醒 waiter、释放 scope。
- [~] native legacy REPL 仍保留 `currentID + legacyTurnMu + runTurnForLegacy`；Agent path 已拥有 session log、registry owner/policy、workspace、pre-step、approval、sandbox/tool runtime context，且配置发布已从 legacy turn lock 拆出 `controlMu`。jobs/plan/subagent/workflow 的 Agent-owned 注册现在使用显式 context resolver；全局兼容字段、legacy schedule 和部分后台生命周期仍未完全移除。
- [x] Agent context 即使与 legacy `currentID` 同名，也优先解析 `runtimeLogs[sessionID]`；仅同 session 才允许 `a.log` 兼容回退，避免 addressed Agent 串用另一 session transcript。
- [~] Agent-owned jobs/plan/subagent/workflow 已移除注册时对 `currentID` 的直接 owner callback，并由 runtime context 解析 addressed session；但 legacy REPL、terminal/session-query/interact 兼容入口和全局 policy/turn state 仍存在，故尚不能勾选彻底删除。
- [x] Registry 的 close 顺序、publication rollback、并发 close/start 做确定性测试。

验收：两个 Agent 并发运行、切换 session、各自注册/卸载工具和 provider 时，不共享可变状态，也不需要全局锁保证正确性。

### T2. Turn / Step / Waterfall 生命周期

- [x] `RunMessages` 保留多条输入，首轮 pre-step 支持 `reject | enter(messages)`。
- [x] request、request-error、turn-stopping around hook 具备 `next()` 组合语义。
- [x] Agent bridge 在 `step/end` 后消费 next-step/steering/quiet，ordinary followup 留到下一 turn。
- [x] request/header、assistant chunk/message、tool call/result 的 durable 顺序。
- [~] pre-step hook 当前主要覆盖首个 external step；内部 tool continuation、error retry、turn-stopping 的 reference 语义仍未逐事件对齐。
- [~] 已覆盖重复 request-error recovery、取消优先级、结构化 provider failure、`normal/always` policy 与 `Retry-After` 基础语义；always 现在先交给 request-error 下游 hook 再使用 provider fallback，并覆盖非 transient failure。仍需完整 reference retry plugin 的 provider-scoped policy、拒答/max-token/abort/cancel 矩阵与 replay invariants。
- [x] 增加 live event 与 durable event 的双层投影，并验证 listener 异常不会破坏 durable order。

验收：生命周期 validator、fault injection、steering race、tool barrier、retry/abort matrix 全通过。

### T2 implementation note (2026-08-28)

- [~] loop now enforces cancellation-over-retry, normalizes unsupported finish
  reasons into typed failures, preserves failure codes at `turn/end`, and maps
  pre-step refusal to `blocked`.
- [~] reference retry-policy limits, complete provider error taxonomy and
  cross-surface fault matrix remain; live/durable event dual projection is
  covered by the canonical durable-commit observer contract.

#### 2026-08-29 retry waterfall delta

- [x] Always-mode provider retry now delegates to request-error hooks before
  scheduling its own fallback, for both stream-open and terminal-finish errors;
  its fallback is not restricted to the normal transient-code allow-list.
- [~] Provider-owned policy schema generation, disposal drain and the complete
  retry invariant/replay matrix remain open.

#### 2026-08-29 durable pre-step failure delta

- [x] Native and ACP compaction injectors now use an error-returning durable
  pre-step entry point in production. Start/summary/end append failures, and
  the error-end append failure, stop the loop before the next provider request.
- [~] The old void injector API remains as a compatibility wrapper, and the
  domain mutation/event append boundary is still not transactional.

#### 2026-08-29 JSONL failed-batch rollback delta

- [x] JSONL append now restores and syncs the committed byte prefix after a
  record-write or final-sync failure, so a failed batch cannot remain as a
  committed tail after restart; a focused prefix-rollback regression is in
  place.
- [~] True process-crash injection, filesystem fault simulation and the full
  JSONL/SQLite corruption matrix remain open.

#### 2026-08-29 approval wiring correction

- [x] Application-created ACP sessions share the app's session-scoped approval
  service and durable decision transition with CLI/Web. Directly-constructed
  ACP test sessions may still use a private fallback engine.
- [~] Cross-process approval transactions and the complete three-answerer
  external replay matrix remain open.

### T3. Session event / history / projection

- [x] 事件 seq 连续、版本字段、canonical request/header、steering/message、todo/write 构造器。
- [x] `ValidateLifecycle` 检查 turn/step 嵌套和终止顺序。
- [x] `DeriveHistory` 作为唯一模型 history 来源，支持 tool call/result、reasoning、surface replacement。
- [~] 部分 legacy event 仍通过 projection 兼容读取；核心 user/context/tool-result content 已统一为 canonical wire blocks（含 attachment ref），但 canonical envelope 元数据尚未全部与 reference 字段级一致。
- [x] 已为 unknown required event、malformed payload、opaque extension 建立拒绝/忽略矩阵；surface replacement 仍需和持久化 envelope 元数据统一。
- [~] web conversation/trajectory/native projection 与 session derive 共用同一事件 schema，消除三套分类逻辑。

#### 2026-08-29 lifecycle delta

- [x] `request-error` waterfall 对 terminal finish failure 可重复重入；每次失败重新携带 normalized failure facts，取消在下一次 provider request 前获胜。
- [~] 仍需把 retry policy 从 provider wrapper 迁移到 provider-scoped request-error policy，并补齐 refusal/max-token/abort/cancel 的完整事件矩阵。
- [x] canonical session rows 已增加 turn/step 坐标、顺序及 `tool/call` → `tool/result` 关系校验；legacy 无坐标日志保持兼容。

验收：cold replay、live replay、web reconnect replay 的 history 和 trajectory 一致。

### T4. SessionPersistence

- [x] JSONL create/append/load/inspect/flush 接口；header、seed、parent、CWD、preset、delegation metadata。
- [x] batch append 的 seq/replay conflict 检查、torn final record 修复、格式/路径错误分类。
- [x] 崩溃恢复补齐缺失的 `step/end`、`turn/end {status: interrupted}` 并再次校验生命周期。
- [x] 现有 SQLite store 具备同步 event append/load，并可作为 app sink。
- [~] JSONL 当前采用同步 append+fsync，`Flush`/`Checkpoint`、`ReadFrom`、`ListSnapshots`、Inspect、repair、backup 和统一 `SessionPersistence` 已有实现；write-behind/压缩、reference normalization 与完整跨进程 fault matrix 仍未完成。
- [~] SQLite 已补齐 lineage header、closed-prefix fork、幂等 replay、seekable revision/cold-recovery、backup/repair、原子 seed 发布及 control-plane 多进程写入锁；JSONL/SQLite 双进程实压和全量 corruption matrix 仍未完成。
- [x] 已增加 SQLite header/fork/replay/repair contract 回归，并通过 JSONL 与 SQLite 共享 backend suite。
- [x] 已拆分 recovery Load、严格只读 Inspect 与原始 live-tail 读取，避免 inspection 改写存储。
- [~] fork/seed boundary、opaque source-qualified revision、bounded history read、corruption recovery 已有跨后端字段级测试；现已补充 JSONL 子进程残尾恢复、跨进程文件锁和 SQLite 未提交事务退出证据；完整 crash injection、文件系统 fault simulation 与 corruption matrix 仍未完成。

#### 2026-08-29 persistence recovery delta

- [x] JSONL/SQLite 共用 session 级 interrupted-tail closer：未启动 assistant tool call 生成 `TOOL_NOT_STARTED`，已记录 `tool/call` 但无结果生成 `TOOL_OUTCOME_UNKNOWN`，随后才补 `step/end` 与 `turn/end`。
- [x] `Inspect` 返回内存 balanced logical view 但不提交修复；`ReadFrom(0)` 返回原始 committed tail；只有 `Load` 改变 durable revision。
- [x] durable repair 后 JSONL revision 正确更新，native retry lifecycle envelope 可通过 schema 校验。
- [~] 已拒绝无损迁移不了的 `request/header-delta`、`mode/set` 与 legacy fallback header，并提供来源限定 opaque revision；reference 的消息/steering normalization、跨进程 crash injection 与完整 corruption matrix 仍未完成。

验收：100k event、进程中断、重复 append、双进程 reader/writer、SQLite/JSONL 互换均保持可恢复且不重排。

## P1：安全、扩展与执行能力

### T5. Scoped plugin / dynamic runtime / HMR

- [x] scoped plugin registry：mount、reload、unmount、disposer、失败回滚。
- [x] registry 已绑定到 root/child scope，并提供 host/client 可消费的只读 inventory；manifest version/dependency/profile/bundle/patch 已纳入校验与发布快照 contract。
- [x] dynamic runtime 返回真实值、错误和取消语义；panic、nil runtime、factory failure 均 fail-closed，不会以空结果伪装成功。
- [x] production reload boundary 采用 generation swap + 读锁排空：in-flight call 完成后才释放旧 generation，并有 disposer/并发 reload 回归测试。

验收：插件升级失败保留旧 generation；成功升级新旧资源不重叠泄漏；并发调用只观察一个 generation。

### T6. Sandbox / code runtime

- [x] local provider 有子进程、timeout、output quota、sandbox cwd、敏感环境变量清理。
- [x] `SandboxCapabilities`、强隔离/网络隔离要求及 `SANDBOX_UNAVAILABLE` fail-closed seam。
- [~] local provider 仍是 controlled subprocess，不是 hostile-code enforcing backend；Windows 网络隔离、文件权限、进程树、资源限制均不充分。
- [~] `run_code` and foreground `bash` now continuously drain subprocess pipes and retain only bounded prefixes; background bash jobs use bounded file pumps while preserving `job_output` polling. Hostile-code enforcement, network isolation, and the complete cross-platform resource/process matrix remain open.
- [~] 实现 read-only、workspace-write、danger-full-access 三种 per-call policy，并携带 mode/root/session/network/process/output limits；当前 readonly 依赖 functional bubblewrap，网络/进程树隔离仍不完整。
- [~] 无 enforcing backend 时，默认 workspace-write 已 fail-closed；danger-full-access 仅在调用者显式选择时保留。真正跨平台 enforcing backend 仍未接入。
- [~] 已补齐 symlink/cwd traversal、credential、quota、timeout/kill-tree 的本地 fault tests；network/hostile-code、跨平台 enforcing backend 及完整 process-tree 安全矩阵仍开放。

验收：安全策略由 backend capability 证明；每次调用均可从 durable code/run/tool/result 重建实际 policy 和结果。

### T7. Approval / interaction

- [x] request/resolve/await、structured questions、`allowed-once|rejected|cancelled|unavailable` 状态及部分恢复。
- [x] service 层已强制 per-session `ask/never` policy、session ownership，并让 app gate 通过该 seam；app 的最终展示仍保留 approved/rejected 兼容字段。
- [x] approval request/decision 已在 native/runtime projection 中支持 `approval/asked|approval/decided`，并保留 interact/* 兼容事件。
- [~] 已实现 session-scoped policy、request id、one-shot consumption、answerer ownership、expiry/cancel/unavailable；SQLite durable projection、三类 answerer 的完整 replay/atomic commit 仍需继续验证。
- [~] approval 请求、决定、拒绝和执行结果已有 session event + SQLite approval projection，并支持 cold restore；provider 与审计事件仍缺跨进程原子事务和完整 replay gate。
- [x] CLI、Web、ACP 三类 answerer 现在共用应用层 `approvalAnswerer` contract；session ownership、CAS、durable `approval/decided` 投影和非原子失败回滚统一，legacy REPL 旧事件形状仅由显式兼容参数保留。

验收：无 answerer、超时、重复决定、session 越权、进程重启、allowed-once 重放均安全失败。

### T8. Tool runtime / registry

- [x] schema validation、whitelist、timeout、output cap/spill、rich result、pre/execute/post/result hooks、并发安全分类。
- [x] owned registration disposer、动态 tool unregister。
- [~] Registry 的 map/policy/hook 已有并发保护和 prepared-call generation 检查；仍不是完整的 scoped concurrent registry，renderer、逐项 provenance 和 tool waterfall 未全部对齐。
- [~] Catalog 现在统一生成 owner/plugin/generation/session、provenance、profile、输入/输出 schema、错误 taxonomy、effective timeout、explicit cancellation、lifecycle events 和 policy projection；registry registration 也保存显式 source，不再只靠 snapshot 推断。
- [~] CLI standard/PTC/minimal mode projection 和 MCP selector/bridge provenance 已验证 canonical catalog admission；fresh shell/code/search/network/question/job-wait/subagent-wait/workflow/LSP 工具已显式分类 cancellation，并区分了有意长期存活的 background/child 工作。plugin-owned production registration、persistent terminal context-abort 实现和全 transport inventory conformance 仍开放。
- [x] Plugin `Spec.Tools` 现在通过 generation-guarded `ToolPublisher` 注册 owner/plugin/provenance，reload replacement 会让旧 prepared call fail-closed，旧 scope cleanup 不会误删新 generation；production app 已绑定 plugin registry。
- [x] persistent terminal foreground write 现在观察调用方 context：取消时发送 SIGINT/CTRL_BREAK 并 fail-safe reset/close process tree；catalog 只对 terminal send 声明 cancellation，lifecycle receipt 保持 non-cancellable。
- [~] Catalog manifest 现在携带 schema version、registration revision 和 SHA-256 digest，并能拒绝 payload/revision/digest drift。Web inventory、Agent-owned native/Web/SDK turn assembly 和 ACP session setup 已接 canonical manifest/projection 校验；legacy CLI fail-open assembly、ACP/SDK first-class inventory fields 和 release artifact digest pinning 仍开放。
- [x] ACP initialize/new/resume/reconnect 和 SDK initialize 现在携带 first-class catalog revision/digest inventory；provider 或 manifest 失败会 fail-closed。release gate 也执行 CLI `--catalog-manifest` 导出和 `--verify-catalog-manifest` 复查。
- [~] error code、output schema、rich content block、spill locator、source event linkage 已在核心工具/Code/MCP 路径统一，并有 fail-closed 校验；失败时产生的 partial rich result/context 也不会被 registry 丢弃；所有 built-in tool 的 metadata/renderer/provenance/错误与输出合同尚未迁移完成。
- [x] 已补充 unknown tool、invalid schema、panic、cancel、timeout、parallel barrier、generation replay 测试；稳定 error-code taxonomy、rich content/attachment wire shape、spill locator/source linkage 和核心 replay fixture 已接入。全量工具仍需继续做逐工具 contract 对比。

### T9. Commands / questions / plan / todo / feedback

- [x] 已有 command routing、structured question、plan/goal/todo、feedback 和 web 投影。
- [~] 部分 command 是 REPL 特有路径；需要证明模型、Web、ACP 对同一状态机的操作等价。
- [~] todo/write、plan 状态、question answer、feedback 已有各自 session/event 投影；`/plan` 现已按 Agent turn 边界提交并可由 command lifecycle 重建，但统一 generated contract 仍未完成。
- [~] 用户输入、steering、question answer 已分别进入 next-turn/next-step/approval seam；跨入口优先级和所有 turn-boundary replay 仍需完整契约测试。

### T10. Subagent / Agent Teams

- [x] spawn/fork、independent child log、depth、tool filter/persona/output schema、continuable child、send/followup/wait/cancel/report。
- [x] Team board 有 task status、CAS revision、blocker DAG、write-scope overlap warning、durable mailbox snapshot/restore。
- [~] `spawn_teammate` 已绑定 shared task board、Agent Registry、durable child Session 与 roster rebind；board 的完整 append-only member fold、fork/ACP persistence 仍需完成。
- [~] 每个 teammate 已通过 Agent Registry 创建独立 scope，并已有 child session、roster snapshot、mailbox 和部分 owner authorization；完整 membership/receipt transaction 与跨进程授权证明仍缺失。
- [~] `subagent_resume/status/cancel/list` 已在进程内和可恢复 directory 路径执行 caller/descendant scope 校验；Team roster 的完整 owner authorization persistence 仍未完成。
- [~] 已增加 session-scoped `task_create/update/list/get`、claim/complete/delete tombstone、CAS conflict、blocker cycle、message delivery 工具与事件适配器；roster/子 Agent 授权、跨 session receipt 仍需完整矩阵。
- [~] Team snapshot 已持久化到 JSONL/SQLite，并支持 restart、roster rebind、fork/cold inspection；完整 append-only membership fold 与跨进程恢复仍未闭环。

#### 2026-08-29 membership projection delta

- [x] `team/member` replay now pre-validates and folds Board and Roster under
  one deterministic Board→Roster lock order; a rejected member transition
  cannot leave only one projection advanced. The regression covers immutable
  identity rejection after provisioning.
- [~] This is an in-process projection atomicity fix only. Durable membership
  transactionality, owner authorization persistence and child-session receipt
  binding remain open.

#### 2026-08-29 cross-process approval and Team provisioning delta

- [x] SQLite approval resolution now has an independent two-store regression:
  concurrent answerers race through the durable CAS and exactly one terminal
  decision plus one `approval/decided` event can commit.
- [x] SQLite Team provisioning now supports one transaction for the child
  Session/header/fork seed and the lead `team/member` provisioning event;
  conflicting root sequence or seed validation rolls back the entire batch.
- [~] Active-member runtime publication remains separate from storage, but
  SQLite now provides a lineage/root-sequence/prior-state checked transaction
  for the later provisioning-to-active/failure event (including idempotent
  replay). Owner authorization is durable-by-replay but not yet a
  cross-process membership service with a complete external receipt/
  authorization matrix.

#### 2026-08-29 subagent completion wake delta

- [x] A settled child emits the parent-scoped `subagent/end` event and, when a
  live parent Agent exists, queues one bounded `Followup` with a stable
  `subagent:end:<child>` dedupe key. Cold parents are not implicitly started;
  their durable event remains the recovery source.
- [~] The event append and parent inbox enqueue are still two commits. Cold
  recovery now reconstructs the missing inbox receipt from `subagent/end` using
  the stable dedupe key; this is at-least-once recovery, not one atomic
  cross-session transaction.

#### 2026-08-29 durable event failure propagation delta

- [x] Agent-runtime `plan/*`, `workflow/*`, `eval/run` and `ralph/run` tool
  paths now return a durable sink error instead of returning an apparently
  successful model result after event persistence failed; focused failure
  contract tests cover workflow, eval and Ralph.
- [~] Domain mutation and its event append are still separate operations, so
  failure propagation is fail-stop for the caller but not transactional
  rollback.

验收：父/兄弟 Agent 不能越权读写 task/mailbox；CAS 冲突可重试；删除任务不会破坏历史引用；依赖图始终无环。

### T11. Code Mode / nested dispatch

#### 2026-08-29 additionalContexts rich-message delta

- [x] Tool results now carry identified rich `llm.Message` contexts in addition
  to the legacy string compatibility field; registry bounding preserves them.
- [x] Code Mode aggregates nested contexts in submission order, retains them
  when the outer program settles as a failure, and records the rich projection
  on `tool/code-dispatch`.
- [x] The loop appends deferred contexts only after the corresponding
  `tool/result`, preserving FIFO order and source attribution for the next
  model step; replay strips runtime-only source fields as expected.
- [x] Tool results now expose the successful `concludesTurn` terminal marker,
  and the loop closes the turn after all already-submitted sibling results are
  committed. A composable post-execute around-hook waterfall is also exposed.
- [~] Full post-execute decision parity, image-content forwarding, worker
  resource enforcement and all tool-by-tool additional-context producers are
  still open.

- [x] TypeScript ProgramRuntime、host binding、ordered logs、return value、failure/truncation/duration 投影。
- [~] nested tools 已贯穿 session/agent scope、approval、sandbox policy、cancel、lossless value 和 rich content；namespace 多绑定的 portable reserved-name、workflow/Code Runtime 进程环境清理、组合器 fatal 标记、结果校验及 Code Runtime serialized output ledger 已有局部对齐，但 worker 资源限制仍未等价。
- [~] parent call ID、registry policy/generation、权限继承/收缩、递归拒绝和关闭回收已有局部验证；reference worker 的 depth/resource/queued-abandon replay 仍未完成。

#### 2026-08-29 structured post-execute decision delta

- [x] Tool registries now expose an explicit `accept|block` post-execute
  decision hook. Accepted decisions enforce value/content exclusivity and
  cannot replace a failed result; accepted contexts prepend tool-deferred
  contexts, while blocked results carry only decision feedback/contexts.
- [~] Full reference waterfall ordering, all producer registrations, and
  cross-surface output/error replay coverage remain open.

#### 2026-08-29 nested Code Mode failure-context delta

- [x] A rejected nested tool now forwards its rich deferred contexts through
  the rejected TypeScript binding into the outer `code/dispatch` and
  `tool/result`; the nested promise remains an error.
- [~] Worker resource enforcement, renderer-owned value replacement, and the
  complete image/content forwarding matrix remain open.

### T12. Token meter / telemetry / compaction pressure

- [x] detached measurement、surface nodes、request fingerprint、usage baseline、log revision。
- [x] Loop 成功 request 已将 usage 回调接入 app meter。
- [~] meter 已成为 pressure、retainTokens 与 shadowed range 的主计费来源，surface provenance/replacement 仍有启发式回退；ACP 多图 admission 已改为全量预校验、批量落盘与失败回滚。
- [x] 将 `baselineEstimatedTokens`、`baselineUsageTokens`、`surfaceDeltaTokens`、`totalTokens`、`surfaceTokens` 和 positional nodes 接入 compaction。
- [~] 已增加 replacement、spill、reasoning、tool schema、image/offload 的基础 pricing，并将 usage 拆为 `cacheReadTokens`/`cacheWriteTokens` 后统一 provider 映射；仍需 completion/output ledger、attachment/offload 全量边界及 cached-input contract tests。

### T13. Settings / credentials / provider selection

- [x] provider/model/effort/session preset/permission 的 SQLite settings 和恢复。
- [~] Agent-scoped provider selection、credential lifetime、plugin ownership、secret redaction 尚未完成统一 scope contract。
- [~] provider/model 切换在 turnMu 下对下一轮生效，provider 编辑失败会补偿恢复 settings、内存索引和旧 registry；仍需并发运行 Agent 的原子快照/race 证明，以及 secret redaction 全链路门禁。

#### 2026-08-29 provider snapshot delta

- [x] Provider rebuild now publishes config-derived registry state behind a
  shared publication barrier; turn assembly consumes one config/provider
  generation snapshot, and the model-switch regression exercises concurrent
  snapshot reads while switching providers.
- [~] This closes the in-process provider/config tearing seam only. Credential
  lifetime, full secret-redaction audit and a working CGO race gate remain open.

### T14. MCP / ACP / SDK protocol

- [x] MCP stdio、tool listing/call、task support rejection、dynamic bridge；ACP initialize/new/prompt/update/cancel/resume 基础路径。
- [~] ACP 已支持按 session 的确切 provider/model route 验证能力并接收 text + image prompt（canonical base64 → 批量 admission/attachment ref → Agent inbox）；audio/embedded context、additionalDirectories、mcpServers、permission request 仍按 reference 的当前行为拒绝。
- [x] ACP permission request 已连接统一 application approval service，并覆盖 server-initiated JSON-RPC、response routing、late response、cancellation cleanup 与 sensitive-tool fail-closed；MCP rich content 已补齐 route-gated、canonical image batch admission。
- [x] MCP 当前 reference 行为是拒绝 `execution.taskSupport: "required"` 而不是实现 task API；Go bridge 同样在发起 tools/call 前失败，回归证明不会触碰 transport。stdio reconnect 与 Streamable HTTP initialize/session/header/list/call/SSE/DELETE/reconnect lifecycle 均有测试。
- [x] ACP resume/reconnect 现在返回 durable metadata（cwd/provider/model/effort/mode/event cursor/next turn），未知 session 分类为 invalid params，resume replacement 与 in-flight prompt 序列化，失败时关闭新 runtime 且保留旧 runtime。外部 wire contract 已覆盖 metadata。
- [~] ACP content blocks、permission callback、resume/reconnect 基础路径与上述 fault semantics 已有；更完整 reference schema/client 边缘矩阵与跨协议故障矩阵仍未完成；SDK reference fixture replay 已接入。
- [~] 已生成外部 ACP wire contract（version、path/method error、resume/reconnect、rich content）和外部 MCP stdio contract；仍需由 reference/generated schema 驱动的全量 client matrix。
- [~] Go 外部 SDK client 已覆盖 newline JSON-RPC、结构化 response error、request context 取消/超时、通知排队与 session-tree 过滤、receipt→idle owned-run，以及 shutdown→stdin EOF→graceful termination→kill/reap；真实 `--sdk` server 的 initialize/shutdown 与真实子进程 lifecycle contract 均有测试。
- [x] 真实 Go `--sdk` server 现在也有外部 client prompt 生命周期契约：SQLite durable receipt/event、Agent runner、assistant message、idle status 和 shutdown 在同一 wire 会话内完成；Agent claim→running 空窗和异步 event hub 导致 status 越前/误 idle 的竞态已由状态边界与转发 sequence barrier 修复。
- [x] Reference SDK notification fixtures（text turn、bash tool、persistent tools、in-process subagent）现在全部通过 Go client 子进程完整 replay；snapshot 暴露的 `SessionEvent.time` 数值毫秒契约已同步到 Go server/client 边界。
- [x] SDK 子进程异常退出会以类型化 closed error 保留 exit code 与 bounded stderr tail；超过 4 MiB 的 wire frame 会关闭 reader 并失败 pending request；launch cwd、完整替换式 env 与 request timeout 均有真实子进程矩阵。
- [x] SDK protocol 现在有共享 JSON Schema：reference notification 全集、client→server request 全方法和 typed result 都会经过 schema；非法时间戳、status、lineage、prompt content、maxTokens、messageId 与 shutdown result 有 fail-closed 矩阵。
- [x] SDK JSON Schema 现在由 reference TS types 通过 `tools/generate-sdk-protocol-schema.mjs` 生成，记录协议/LLM/session/subagent/attachment 类型源 hash；设置 reference checkout 的 Go 测试会重新生成并逐字节检查漂移。
- [x] Go SDK prompt content 已同步 reference ContentBlock 已知并集：text、reasoning、image attachment ref、tool-call、tool-result；旧 canonical base64 image 仅作为 Shutu 兼容 admission 路径，reference image 以 durable attachment metadata 解析。
- [x] Owning SDK Client 现在暴露 pre-start reverse-request handler；真实子进程 runtime callback、参数归一化、结果回传、start 后禁止替换、无 handler `-32601`、结构化 error 与 panic→`-32603` 均有契约。
- [x] hostile 子进程矩阵新增 stdout 提前关闭但进程仍存活的场景：请求和订阅失败关闭，Close 仍完成 shutdown/EOF/kill/reap。
- [x] merge-extensible ContentBlock unknown/插件扩展已在 SDK client、server admission、Agent inbox、session wire replay 和 LLM runtime block 间以 opaque raw carrier 透传；已知 block 的 reference 小写 JSON 与 unknown block 原字节均有回归。
- [x] hostile child matrix now covers a child emitting malformed/noise frames before a valid response, a live child emitting an over-4MiB frame, and stdout loss; all paths either recover the later response or fail closed and reap the child.
- [x] A shared transport-neutral protocol lifecycle fixture now owns the common workspace/session/message/rich prompt/tool/assistant/stage facts and is consumed by external ACP, MCP stdio, and SDK server/client tests rather than each protocol hardcoding its own scenario.
- [ ] T14 remains open for the broader protocol matrix: reference schema/client edge cases, richer ACP fault propagation combinations, MCP transport fault/SSE edge cases, and expanding the shared fixture beyond the first rich-session-tool-turn scenario.

实现注记（2026-08-29）：ACP server 已在 session 边界串行化 prompt，并补充
unsupported option/content、permission late-response 的 wire contract tests；这
Reference 当前明确拒绝 MCP required task execution；剩余缺口是更完整的 rich/transport
fault matrix 与外部 client suite，而不是实现 reference 未提供的 task API。

### T15. Shell / terminal / filesystem / LSP / web

- [x] shell/terminal、filesystem/search、LSP、web search/fetch、attachments、多模态基础能力存在。
- [~] 这些能力的 host adapter 尚未全部使用 Agent-scoped workspace、sandbox、approval 和 capability inventory。
- [ ] 每个能力提供 service/provider/consumer seam、per-call policy、durable event、bounded output 和 cancellation。
- [~] web host 已有 capability inventory、durable cursor、reconnect repair 与 session-scoped mutation seam；全量 mutation authorization 矩阵仍未完成。

#### 2026-08-29 child publication rollback delta

- [x] Spawn/fork runtime publication now happens only after child seed
  restoration, durable header/seed creation and the first lifecycle event all
  succeed; failed initialization cannot leave a ghost child in the runtime
  index. A focused injected-persistence-failure regression passes repeatedly.
- [~] This closes one in-process publication window only. Cross-process Team
  receipts, complete protocol lifecycle and the strict race gate remain open.

#### 2026-08-29 process-wide shutdown coordination delta

- [x] Added and wired a process-owned shutdown Coordinator. Registration order
  is dependency order and teardown runs in reverse order, with admission
  closed before jobs, Agents, schedulers and transport resources drain.
- [x] Concurrent Close calls share one completion barrier and one error result;
  late resource registration is rejected after shutdown begins.
- [~] This coordinates the application composition root and delegates waiting
  to each resource's own close barrier. Cross-process crash, hostile-sandbox,
  and protocol lifecycle gates remain open.

#### 2026-08-29 shared fixture consumer delta

- [x] `contractfixture.CoreTurnEvents` now owns the transport-neutral decoding
  of the checked-in core replay fixture; session, persistence and native Web
  contract tests consume the same decoded records instead of duplicating JSON
  parsing and timestamp conversion.
- [~] The fixture set still does not cover every tool, approval, Team, ACP/MCP
  and SDK lifecycle, so T20 remains open.

### T16. Lifecycle integration

- [~] 已接入现有 goal、schedule、jobs、workflow、skills、compaction、spill、plan 的多数 native path。
- [~] schedule/job/subagent completion、Agent-owned workflow/plan 等主要唤醒已映射到 addressed Agent inbox，jobs Close 也等待 completion observer；skill/compaction/plugin/terminal 与 legacy scheduler 仍有全局兼容路径，尚无统一全资源 quiesce 协调器。
- [~] Local jobs 的 Close 现在会等待 terminal completion observer 完成，避免 `job/done` 已落地但 owner inbox 投递仍在进行时提前关闭 Agent；但 child Agent、scheduler、plugin/terminal、session switch 和进程级统一协调器仍未完成全量 quiesce/await/cleanup 证明。

## P2：存储、Web 与产品投影

### T17. Workspace / storage / deploy

- [x] SQLite session/workspace metadata、CWD、archive/order、JSONL file backend 基础能力。
- [~] workspace ownership、header lineage、team snapshot、plugin manifest、approval records 尚未统一存储模型。
- [~] storage migration/versioning、backup/repair、bounded read、JSONL/SQLite 多进程锁和 Unix deploy-safe permissions 已实现并有回归；Windows ACL、完整备份恢复验证、故障注入与全量 corruption matrix 仍未完成。

### T18. Web host and UI projection

- [x] SSE/event hub、native projection、conversation/trajectory、reconnect、dark/mobile/accessibility 基础测试。
- [~] UI 能看到的 inventory/status 不一定来自真实 Agent capability scope；部分接口仍依赖 app 全局状态。
- [~] cursor 断线后已从 durable seq 精确修复并去重，Web mutation 已覆盖主要 session ownership 路径；全端 authorization matrix 仍未完成。

### T19. Observability

- [x] live status、event hub、部分 request/tool/subagent/approval event projection。
- [~] runtime correlation（agent/session/turn/step/request/call/generation）、structured in-process metrics、bounded span/error-code recorder 已接入 loop；后台 jobs 现已覆盖注册、独立 context 与 settlement span，但外部 export、跨进程 job registry 与 crash recovery 仍未完成。
- [x] 证明观测失败不会影响模型执行，但 durable event 失败会按契约阻止错误继续扩散。

实现注记（2026-08-29）：已接入 reference-compatible 的默认关闭、`FULL` /
`FEEDBACK_ONLY`、`DSH_TELEMETRY_DISABLED` 和 OTLP/HTTP JSON 批量 exporter。
native/Agent/ACP 的 session observer 均接入同一 exporter；队列有界且 collector
失败不会改变 durable append 或模型执行。仍需补 SDK 级 retry/queue 语义、feedback
suffix replay、全量后台 correlation 与部署级 collector contract，故本项仍不能标记为完成。

## P21：等价门禁与持续验证

### T20. Contract tests

- [x] 核心 package 单测、部分 integration/e2e、go vet/build。
- [~] event replay、history derive、persistence backend 与 native Web 已开始复用共享 contract fixture；ACP/MCP/SDK 也已共用首个 rich-session-tool-turn lifecycle fixture。tool schema/output、approval、plugin、subagent/team 与更完整协议矩阵仍未完成。

### T21. Fault / security / performance gates

#### 2026-08-29 ACP durable tool-event delta

- [x] ACP Agent loops now install the addressed `RuntimeSessionID` and
  `RuntimeEmit`; context-aware filesystem/MCP tools therefore use the same
  session-owned durable sink as native Agent runs.
- [x] ACP terminal start/stop, MCP list/call, and ACP subagent settlement now
  return durable append failures instead of silently reporting success; a
  failed terminal-start event also tears down the newly-created process.
- [~] ACP connection-owned teardown, prompt admission/cancel interval,
  complete external client suite, and event-plus-live-projection atomicity
  remain open.

- [~] fault：provider error、stream truncation、cancel race、tool panic、approval unavailable、plugin reload failure、SQLite/JSONL torn write、MCP disconnect、ACP reconnect 已有分项回归；完整 kill-point、文件系统 fault、跨进程与端到端矩阵仍未完成。
- [~] security：workspace traversal、secret leakage、session/Agent/team 越权、stale plugin generation、approval replay 已有核心测试；hostile sandbox escape、Windows ACL/network isolation 与全链路审计仍未完成。
- [~] performance：部分 bounded output、seek read、stream aggregation、multi-Agent concurrency 与 reconnect 已覆盖；100k event、持续压测、append latency 和 memory-growth 基线仍未完成。
- [ ] race detector 在启用 CGO 的环境运行；补充 leak/goroutine/process-tree 检查。
- [ ] CI gate：所有 contract/fault/security/performance suite 通过后，才生成“DeepSeek Harness capability-equivalent”声明。

## 当前不能宣称等价的硬阻断项

1. native app 仍存在全局 `currentID`/`turnMu` compatibility bridge。
2. local code provider 不是 enforcing hostile-code/network sandbox；强隔离只在显式要求时 fail closed。
3. approval service 已强制 per-session `ask/never` 和 ownership，并有 SQLite durable projection；provider/审计事件尚未达到跨进程原子事务和三类 answerer 完整 replay 等价。
4. Agent Teams 已接入 teammate runtime、roster、durable child session 及部分 owner authorization；但成员 reservation 与 domain receipt 尚未原子绑定，跨进程 membership/job/terminal recovery 仍未完成。
5. ACP 的 image prompt 已对齐当前 reference 子集；audio/embedded context、additional directories、MCP session、permission request 仍未形成完整 lifecycle，MCP task/rich lifecycle 也未完整贯通。
6. JSONL/SQLite 尚未共享完整 SessionPersistence contract；meter 尚未成为 compaction 的精确计费源。

## 2026-08-30 清单状态修订：Team member 与 credential lifecycle

- T10/A5.4：Team member identity 现在在 child session、`team/member`
  provisioning event 和 Agent Registry publication 之前通过
  `team-member:<team-id>` durable reservation；直接 Roster 调用也走相同 seam。
  这仍不等于 reservation 与 domain receipt 的单事务，reservation orphan
  recovery/GC 和完整跨进程 authorization 继续保持 `[~]`。
- A6：新增独立 credential Vault、SQLite dedicated table、generation、
  revoke、in-flight lease drain、startup migration；Acquire 会在操作边界
  从 backend 刷新跨进程 rotation/deletion，生产 streaming adapters 在
  terminal/error/body-close 释放 lease。OS keyring/KMS、abandoned reader
  disposal、crash-dump/hostile-memory proof 和全量 provider/Web matrix 仍为
  `[~]`。
- 可执行明细以 `docs/equivalence-task-register.yaml` 为准；截至本修订共
  44 项：3 项 `done`、20 项 `partial`、21 项 `open`，41 项仍是 release
  blocker。`claimAllowed` 必须继续为 `false`，直至所有 required 项均有可
  重放证据。
## 2026-08-28 实现状态校正

以下增量已在代码和局部测试中落地，覆盖本清单中部分原先仍标为 `[~]` 的描述：

- T4：JSONL 与 SQLite 已共享 `SessionPersistence`，包括 `ReadFrom`、`ListSnapshots`、revision、header、fork、flush、inspect 及共同 backend contract suite；进程级锁、迁移/备份/repair 仍未完成。
- T5：plugin manifest、依赖校验、profile/bundle/patch inventory、generation reload 排空及 disposer 已有实现和回归测试。
- T7/T9：session-scoped approval 已能投影 `approval/asked|approval/decided`；CLI 兼容路径仍保留，三类 answerer 的完整 replay contract 尚未闭环。
- T8：工具 registry 已提供稳定错误码、canonical rich content、spill locator/source linkage 与 `VisibleSpecs`；全量工具逐项 contract 尚未完成。
- T10：teammate 已接入 Agent Registry、durable child session、roster rebind 与 subagent control plane；完整 snapshot/cold inspection/authorization contract 仍未完成。
- T14：ACP 已增加 durable factory 的 `session/resume` 与 `session/reconnect` seam，并有 server contract test；ACP/MCP task lifecycle、恢复元数据及外部 client 全覆盖测试仍未完成。
- T15：DeepSeek Search 的 request event 已支持 caller-context 路由，Agent 并发 session 不再必须共享全局 Web 日志；其余 Web mutation/authorization/reconnect repair 仍未完成。
- T6/T13：local sandbox 已实现显式三档 mode admission；provider registry、Web/ACP capability discovery 使用不可变配置快照，SQLite migration 会拒绝更新版本数据库。

## 2026-08-28 增量实现：维护接口与 Team 共享边界

- JSONL/SQLite 已增加显式 integrity check、不可覆盖 backup 和 session repair
  接口及回归测试；这不等于已完成多进程字段级备份门禁。
- 自定义 Provider 删除失败时会补偿恢复 settings、内存索引和 live registry。
- Team child session 持久化 parent lineage，task/mailbox 解析到 lead-owned
  shared board，child Agent identity 仍用于 authorization。

这些校正不改变“只有所有 P0/P1/P2 与门禁完成后才可宣称 capability-equivalent”的总验收规则。

## 2026-08-28 审计追踪修正

- T7 approval: SQLite 决定路径现已将 pending-row CAS 与 canonical audit event 放在同一事务；已覆盖普通会话与 ACP 入口，并有冲突回滚测试。仍需补请求创建、三类 answerer 和跨进程 replay 契约。
- T10 Team: 任务 revision 与 mailbox queued/delivered 已采用 typed append-only 事件，并可从最近 snapshot 折叠尾部事件；成员状态仍是 snapshot 主导，且投递确认没有和子 Agent Session receipt 原子绑定。
- T11 Code Mode: rich value/content 分离、完成值输出计费、Node heap ceiling 已实现；仍不等同参考 worker-thread compute/wall/heap contract 与 queued-abandon 语义。
- T7 approval: SQLite 创建侧现已支持 pending row 与 `approval/asked` 的同事务提交，ACP 已接入并以 `AppendPersisted` 接管已提交事件；CLI/interact 工具的 context-aware atomic creation 与三类 answerer 统一 replay 仍未完成。
- T12 compaction: BasicEngine 已消费 replacement-aware meter 的 total、surface nodes、retainTokens 与 shadowed-token 计量；request header/capacity、provider usage anchor 和全链路计费测试仍未完全闭环。
- 门禁证据：`go test -count=1 ./...`、`go vet ./...`、`go build ./...`、`git diff --check` 通过；配置 `DSH_REFERENCE_ROOT` 时 core reference replay 通过。`go test -race` 在当前 Windows 环境不可执行：CGO 已启用但 gcc 缺失，不得标记为通过。
- T2：Go loop 已按 reference 语义重新进入 `agent/request-error` waterfall；每次失败请求都可独立返回 retry，取消和 recovery listener error 仍优先终止。新增多次失败后成功的回归测试。
- T3：Web conversation 与 trajectory 现在共享 `dsh-event-model.ts` 的事件 ID、时间、详情、文本、请求/调用归一化以及 kind/status/structural 分类；新增 context、tool/error、unknown 的跨投影契约测试。原生 Go projection 尚未与 TypeScript projection 共用生成式 schema，故 T3 保持 `[~]`。
- T2/T7：Session Log 新增 durable-commit 后 observer seam；普通 Append 与 atomic `AppendPersisted` 都在提交后通知 live projection，observer 不参与回滚，也不会在 sink 失败后看到幽灵事件。Approval 的 atomic CLI/ACP 贯通仍未完成。
- T10：Team typed event 的 task/member/message queued payload 已收敛到 reference 的字段边界；task 不再泄漏本地 `deletedAt`，queued message 使用 `content: [{type,text}]` 且不泄漏 `createdAt/delivered`。内部旧 snapshot 仍保留兼容形状，富文本 Team message 尚未完整贯通。
- T12：meter 的 message pricing 已改为 reference 的逐 content-block `ceil(chars/4)+overhead`，并递归计算 tool-result；补充了多 block、短文本不被 floor 截断的测试。request header/capacity/usage-anchor 全链路仍未闭合。

#### 2026-08-29 job completion projection delta

- [x] Local job settlement now projects an idempotent owner-session `job/done`
  before delivering the bounded completion wake, including the no-`job_wait`
  path.
- [x] `job/*` appends now share an application-level commit lock, and a
  32-observer regression verifies exactly one terminal `job/done` projection.
- [~] The event projection and inbox delivery are still separate commits;
  shutdown/quiesce and crash-recovery coverage remain open.

#### 2026-08-29 native command parity delta

- [x] Native REPL now accepts `/feedback <text>` and records the same
  log-only `feedback/record` fact without creating model history.
- [x] Native REPL now accepts `/plan [message]` and `/plan off`; a non-empty
  suffix is submitted as the ordinary turn text after the durable mode switch,
  so `/plan` itself is not sent to the model.
- [x] Blank native slash input is rejected explicitly instead of indexing an
  empty token and panicking.
- [x] Native and Web resolved command paths now record paired
  `command/run`/`command/done` facts; command admission misses remain
  lifecycle-free.
- [~] Telemetry sharing disclosure, attachment admission parity, and the
  complete cross-entry-point command contract remain open.

### 2026-08-29 retry lifecycle delta

- T2：provider retry wrapper 已通过 request context 向 Loop 暴露 scheduled/started 生命周期；`llm/retry` 在 backoff 前落盘，`llm/retry-started` 仅在等待完成且未取消时落盘，并保留 retry identity、turn/step 与 structured failure。
- T2/T13：normal retry 默认值已对齐 reference 的 5 次、500ms、10s、10% jitter，`EMPTY_RESPONSE` 纳入默认可重试失败；native/Web projection 兼容新旧 retry payload。
- 仍未完成：按 provider 的完整 retry policy schema、`always` 模式、Retry-After 规则和 invariant/replay 全量门禁。

#### 2026-08-29 retry policy validation delta

- [x] 配置加载现在拒绝显式的非法 retry mode、负预算、非正/倒置 backoff、越界 jitter、空/重复 retry code 及非法 provider overlay；不再把这些会改变 replay identity 的值静默改写成默认策略。
- [~] Retry policy 仍未完全迁移为 reference 的 immutable provider registration schema；跨 provider route 切换、完整 invariant/recovery/disposal 矩阵仍开放。

#### 2026-08-29 diagnostic redaction delta

- [x] Durable request-end/retry/tool-error constructors now redact bearer and
  credential-shaped diagnostic values before session events and retry observer
  payloads; model/tool content itself is not rewritten as a false audit record.
- [~] Full secret inventory, provider credential lifetime/rotation and
  deployment-level redaction tests remain open.

#### 2026-08-29 retry, persistence and MCP lifecycle delta

- [x] Effective request-hook routing is now the source for retry/request-end
  provider and model facts; canonical replay accepts both Go's top-level
  route fields and the reference `header.config` route without weakening the
  provider-association check. Live retry Append rejects the invalid row and
  restores the prior sequence.
- [x] JSONL `Flush` now takes the per-session process lock and performs a real
  file sync; SQLite `Flush` verifies the session and performs a FULL WAL
  checkpoint. The shared backend contract now tests both a successful barrier
  and cancellation.
- [x] MCP stdio clients now surface `notifications/tools/list_changed` through
  an optional handler; the composition root refreshes and republishes the
  affected bridged tool generation after the in-flight JSON-RPC response.
- [x] ACP text/resource-link prompts no longer require image capability; image
  blocks still require exact session route admission, while unsupported audio
  and embedded resource blocks remain fail-closed as in the reference.
- [~] Provider-scoped retry policy resolution, remote model-catalog lookup,
  cross-process persistence crash injection, MCP session/task lifecycle and
  full external ACP/MCP client suites remain open.
## 2026-08-28 增量实现记录：Team 冷恢复与观测故障隔离

- Team Board 在无 live Agent Registry 的 cold inspection 中保留并重放 `team/member` durable rows。
- `member/provisioning` 恢复时验证 child Session/lineage；可验证时 rebind 并落 active，不可验证时落 failed。
- mailbox 即时投递现按参考顺序先提交目标 `agent/inbox/spliced`，再提交 Lead `team/message/delivered`，最后确认内存队列；跨提交崩溃可由 Team dedupe/recovery 重试。
- Session observer panic 已从 durable append 与 `AppendPersisted` 两条路径隔离。

这些是已验证的增量闭合项；child Session receipt 完整事务、富内容 mailbox 全链路、
telemetry SDK/feedback replay contract、全平台 enforcing sandbox、ACP/MCP 完整
生命周期以及最终 fault/security/performance/race 门禁仍为开放任务。
#### 2026-08-29 后台完成通知冷恢复 delta

- [x] `job/done` 与 `subagent/end` 到 owner Agent inbox 的 terminal-event/wake
  crash window 已有冷启动补投逻辑；补投使用稳定 `dedupe_key`，并覆盖重复恢复
  与已 claim wake 不重复投递。
- [~] 这不等于完整后台生命周期等价：跨进程 owner authorization、完整
  fault/replay matrix、scheduler persistence 及 shutdown/quiesce 证据仍未完成。

#### 2026-08-29 credential update rollback delta

- [x] Native credential set/unset now rolls back the durable setting, in-memory
  overlay and live provider registry together when a selected provider would
  become unavailable.
- [x] LLM provider adapters now expose a disposal seam that wipes their
  provider-owned credential, retry wrappers forward disposal, and application
  shutdown disposes the active provider registry after consumer barriers.
- [~] Credentials are still supplied as startup/configuration strings and old
  provider generations are not yet retired with an in-flight lease barrier;
  deployment-grade secret storage, live rotation and full redaction audit
  remain open.

#### 2026-08-29 scheduler shutdown gate delta

- [x] Lazy per-session durable scheduler creation is now fenced after shutdown
  begins; an already-admitted tool cannot recreate a scheduler after the
  ticker/projection drain, and post-shutdown lookup is rejected deterministically.
- [~] This closes only the scheduler recreation race. Full background
  quiesce/await ordering across jobs, child Agents, plugins and process exit
  still requires an integration harness.

#### 2026-08-29 Agent-scoped job disposal delta

- [x] `jobs.Local.CloseOwner` now implements the Harness `jobs-local` owner
  scope boundary: it marks the exact owner's live jobs stopping, invokes their
  cancellation hooks, waits for both producer settlement and completion
  observers, then removes only that owner's snapshots. Unowned and other-owner
  jobs remain addressable.
- [x] Native/Web/ACP Agent materialization registers this disposer on the
  Agent scope, so closing one session Agent cannot leave its background jobs
  alive in the process-wide registry.
- [~] Child-provider scope attachment, cross-process job backends, and the
  complete shutdown/quiesce fault matrix remain open.

#### 2026-08-29 model terminal workspace/lifecycle delta

- [x] Model-owned persistent terminals now resolve `cwd` against the addressed
  session workspace, reject outside paths, and resolve symlinks before the
  containment check.
- [x] Fresh `bash`/`pwsh` host adapters now accept an injected per-session
  workspace root and apply the same real-path containment check to explicit
  `workdir` values, including background starts.
- [x] Background `bash`/`pwsh` job labels and PTY job labels no longer persist
  the command/input text; they use the bounded user-facing description or a
  generic label, with diagnostic redaction applied to descriptions.
- [x] Generic `job_start` also uses a fixed non-sensitive default label and
  redacts caller-supplied labels before the durable job snapshot/event.
- [x] Local filesystem host paths now resolve existing components through
  realpath and reject outside/dangling symlink escapes; read-before-write
  observation state is mutex-protected for concurrent Agent calls.
- [x] `grep`/`glob` now accept an injected session workspace root and reject
  explicit outside or symlinked search paths; legacy embedders without an
  addressed session retain the prior unconstrained constructor behavior.
- [x] Generic and minimal persistent terminal creation/close/reset paths now
  publish the existing durable `terminal/start` and `terminal/stop` facts;
  runtime-scoped sinks are used for Agent calls and the legacy log is only the
  direct-call fallback.
- [~] This does not make the legacy `/term` REPL or persistent terminal state
  restartable; process-owned terminal recovery and cross-platform sandbox
  enforcement remain open under T15/T17/T21.

#### 2026-08-29 ACP image admission delta

- [x] Image prompts now require a non-empty model selected by the addressed
  session and check that session's provider capability, rather than trusting
  only the global modality flag or the legacy current session.
- [x] Multi-image admission validates the complete batch before writing and
  uses attachment-store batch rollback when a later write fails; the durable
  transcript still contains references only, never inline image bytes.
- [~] Exact remote model-catalog resolution, provider credential lifetime and
  the complete external ACP/MCP lifecycle suite remain open.

#### 2026-08-29 MCP image result admission delta

- [x] MCP image results now require the addressed session's multimodal/model
  route before entering model-visible content; denied images remain explicit
  diagnostics while canonical MCP values are retained for programmatic users.
- [x] Multiple image blocks in one MCP result are validated before storage and
  are persisted through the attachment batch API with rollback on write failure.
- [~] Notification-driven tool-list generations, MCP task execution and full
  SDK reconnect/dispose contract coverage remain open.

#### 2026-08-29 request-route and shared-fixture delta

- [x] `RequestHook` 改写 provider/model 后，loop 现在在最终 request 边界重新解析
  实际 transport，并从同一个 resolved LLM 读取 retry policy；因此 durable route、
  实际 provider 调用和 retry 预算不再可能分叉。无 registry 名称的 standalone
  embedder 仍保留原有匿名 LLM 兼容路径。
- [x] DeepSeek、OpenAI-compatible、Anthropic、Google 和 OpenAI Responses adapter
  现在都以最终 `ChatRequest.Model` 为 wire model（空值才回退构造时默认值），并有
  provider-level request-wire regression tests。
- [x] 同一份 `core-turn-replay.json` 现在同时经过 session、native Web projection
  以及 JSONL/SQLite persistence 的 validate → append → load → restore → history
  replay，backend contract 不再只覆盖自造的简化事件。
- [~] 仍未完成：provider route 的远程 model-catalog/能力解析、跨进程 crash/fault
  矩阵，以及 ACP/MCP/SDK 的外部 client lifecycle contract。

#### 2026-08-29 telemetry resource identity delta

#### 2026-08-29 approval ownership and admission delta

- [x] Agent/ACP 运行时的 approval/question ask 现在要求已有 open `turn/start`
  边界；turn 外请求 fail-closed，不会制造无法被 replay 正确归属的审计对。
- [x] Approval Service 新增 `ListForSession` ownership boundary；Web answerer
  和 `interact_status` 优先通过 Service 做会话过滤，不再把 process-wide
  approval queue 暴露给 transport 后再依赖调用方手工过滤。
- [x] pending-limit 的 `List` + `Create` admission window 由 Engine 串行化，
  并发 Agent 请求不会共同观察同一个旧 pending count 后突破上限；补充并发
  regression 与跨 session list isolation contract。
- [~] 这仍不等于跨进程 approval decision 与 audit event 的单事务；expiry
  的 canonical audit、三类 answerer 的外部 replay matrix 仍待完成。

- [x] OTLP exporter 现在按批次发送 `service.name`、`service.version` 和 profile-local
  `user.id` resource attributes；主应用把 `cfg.DataDir` 传入 exporter，feedback 与
  telemetry 复用同一个 `.anonymous-user-id`。
- [x] FULL 模式 shutdown 会在异步 drain 中发送独立的 `.../ops` scope 与
  `telemetry.op=shutdown`，关闭故障仍被隔离，不改变 session/turn 结果。
- [~] 仍未完成：reference OTel SDK 的完整 exporter/processor passthrough、redaction
  waterfall、ops records、collector retry/queue 行为与 shutdown fault matrix。
#### 2026-08-29 scoped approval read delta

- [x] `ListForSession` now has a provider-side least-privilege seam. Memory
  and SQLite providers filter before returning rows; SQLite applies the
  `session_id` predicate in SQL. `interact_status` no longer reads the full
  process-wide queue before applying ownership filtering.
- [~] Compatibility providers without the optional scoped method still use a
  safe in-process fallback; cross-process decision plus audit transaction and
  the complete CLI/Web/ACP answerer replay matrix remain open.
- [x] Lazy request expiry now has an explicit audit callback seam; the app
  writes an idempotent `approval/decided {outcome: "expired"}` fact and cold
  restore preserves that terminal outcome. Provider transition and audit are
  still separate commits, so this does not close the cross-process atomicity
  item.

#### 2026-08-29 verified status corrections

- The live observer/durable sink split is implemented and covered by append,
  append-persisted, sink-failure and observer-panic tests; the T2 dual-layer
  item is complete.
- Observability is explicitly best-effort while durable event append is
  fail-stop; loop tests cover both sides of that contract, so the T19 proof
  item is complete.
- ACP now has an external-package wire contract test and rejects relative
  `session/new.cwd` values. Attachment admission verifies raster bytes,
  dimensions, batch limits and content-address digests; remaining ACP and
  attachment gaps are separate protocol/deployment features.

#### 2026-08-29 attachment durability and shutdown-order delta

- [x] Attachment limits are deployment-resolved and shared by CLI/ACP/MCP/Web;
  WebP admission parses RIFF/VP8/VP8L/VP8X framing, and content-addressed
  publication is atomic, synced and safe under concurrent reuse. Batch rollback
  removes only objects created by that batch.
- [x] Web and ACP entrypoints now register the shutdown admission gate after
  dependent cleanup handlers, making it the first deferred shutdown action.
- [x] An external-package MCP wire test now exercises the exported stdio
  client through initialize, tool metadata, structured call results and
  idempotent close/closed-operation behavior.
- [~] OS-level hostile-code enforcement, cross-process storage locking and
  crash-injection corruption matrices, legacy global compatibility removal, and
  the strict CGO race gate remain open; they are not represented as completed.
- [x] Workspace `read_image` now shares attachment byte, format, WebP
  dimension, pixel and raster-dimension admission with upload/storage paths;
  a focused regression proves configured dimension limits are enforced before
  an image block is returned.
- [x] `request/context` is now a first-class canonical event: loop emission is
  deduplicated by resolved provider/model/capacity, lifecycle validation keeps
  it inside a turn, and the evidence JSON Schema plus meter-facing contract
  recognize the route-context projection.
- [x] The canonical wire validator now includes feedback, todo and plan state
  facts with required-field/type checks; malformed product-state events no
  longer pass the core wire boundary as opaque required extensions.
- [x] Legacy scheduled-reminder failure now releases its per-session delivery
  lock, and shutdown drains scheduler/jobs before closing Agent handles so
  completion wakes are not rejected by premature Agent teardown.
- [~] Full background quiesce/await across every plugin, worker and external
  child path, including process-crash recovery, remains open.

#### 2026-08-29 MCP Streamable HTTP lifecycle delta

- [x] The Go MCP seam now selects stdio or Streamable HTTP per server while
  retaining the existing Factory seam for stdio-only embedders. HTTP clients
  perform initialize/initialized, propagate `Mcp-Session-Id`, drain paginated
  `tools/list`, call `tools/call`, consume SSE responses and list-changed
  notifications, and best-effort DELETE the session during Close.
- [x] REPL dynamic selectors, static bridged tools and ACP-owned MCP clients
  all use the same transport selector; HTTP reconnects create a new protocol
  session through the existing reconnect supervisor rather than replaying a
  failed tool call into an old session.
- [~] MCP Tasks remain intentionally unsupported because the current reference
  bridge rejects `taskSupport: required`; full external SDK replay, callback
  response, authentication rotation and cross-process MCP server ownership
  remain open.

#### 2026-08-29 bounded shell output and terminal process ownership delta

- [x] `run_code`, foreground `bash`, and `job_start` now drain stdout/stderr
  concurrently into bounded captures; background bash jobs drain into bounded
  pollable files without allowing a noisy process to grow output indefinitely
  on disk.
- [x] Persistent terminal sessions now start in an owned Unix process group or
  Windows Job Object, so terminal close attempts to terminate descendants as
  well as the interactive shell before the existing quiesce deadline.
- [~] This closes the local output/cleanup seam only. Hostile-code isolation,
  Windows ACL/network policy, resource ceilings, cross-process terminal/job
  ownership and the strict race/leak gate remain open.

#### 2026-08-29 hard-blocker list correction

The historical blocker list above must be read together with all later deltas.
In particular, JSONL/SQLite now share the persistence contract, Team and ACP
now have durable partial projections, and shell/job/terminal output and cleanup
have the local bounded/process-owned implementations recorded above. The
authoritative blockers still requiring closure are: removal or formal proof of
the native global compatibility bridge; an OS-enforcing hostile sandbox on all
supported hosts; cross-process approval/Team/MCP/job transaction and ownership
semantics; full external ACP/MCP/SDK lifecycle replay; provider credential
lifetime/rotation through a dedicated per-operation credential seam and remote
model catalog resolution; full worker resource
limits; and the strict race/leak/fault/security/performance CI gate.

#### 2026-08-29 Agent disposal and MCP error admission delta

- [x] Agent-owned job disposal now closes owner admission, cancels and awaits
  jobs/observers, and removes only the addressed owner. Persistent model
  terminals and approval policies are also disposed from Agent/ACP/Team scope.
- [x] Session title generation is provider/model scoped; Web search event
  logging fails closed when an Agent log is unavailable; disposed terminals are
  no longer addressable and emit durable stop facts.
- [x] MCP `isError` handling is consistent across dynamic, bridged and ACP
  surfaces: the result is `MCP_TOOL_ERROR`, raw protocol content remains in the
  programmatic value path, and rich images are not persisted on failure.
- [~] These are local lifecycle/admission corrections, not closure of provider
  leases, cross-session Team receipts, hostile sandbox enforcement, external
  protocol replay or the strict race/fault/security/performance gates.

#### 2026-08-29 workflow worker admission delta

- [x] Node workflow `agent()` increments its total counter before the first
  await, so concurrent queued calls cannot bypass `maxTotalAgents`.
- [x] The Go workflow host cancels in-flight agent callbacks at workflow
  settlement/cancellation, bounds callback drain, and rejects late JSONL
  writes; the app adapter cancels a child run when its Result wait is aborted.
- [~] Reference worker-death/grace `agent-end` synthesis, pending-start and
  child-disposal receipt replay, and hostile protocol fuzz coverage remain
  open.

#### 2026-08-29 approval disposal delta

- [x] Approval engines expose an optional session cancellation seam. Agent
  disposal cancels only its own pending requests; private ACP engines mark
  pending requests unavailable before close, and shared cleanup projects
  canonical cancelled decisions.
- [~] Cross-process approval/event atomicity and the complete three-answerer
  replay matrix remain open.

#### 2026-08-29 workflow lifecycle boundary delta

- [x] `workflow/start` and `workflow/end` are host-owned, including Node
  launch failure and worker-exit paths; host callback drain precedes terminal
  end publication.
- [x] Host-side workflow agent ledgers synthesize exactly one cancelled
  `workflow/agent-end` for each unpaired start on cancellation/exit.
- [~] Full worker-death admission protocol, pending-start receipts and
  cross-process lifecycle replay remain open.

#### 2026-08-29 Code Mode value/output boundary delta

- [x] Undefined completion is now a successful no-value result; host nil is
  transported as JSON null; non-lossless host binding resolutions are returned
  to the program as catchable binding errors.
- [x] Output-limit diagnostics reserve their serialized budget and trim the
  retained log prefix, rather than exceeding the configured envelope.
- [~] Worker resource enforcement, hostile-peer replay and the complete
  nested dispatch lifecycle are still required for T11.

#### 2026-08-29 Agent provider route pinning delta

- [x] Agent turn construction captures the selected provider instance and
  reuses it for same-route requests across a concurrent registry publication.
- [~] Provider-generation usage leases, safe retirement and credential wipe
  barriers remain required for T13.

- [x] Code Runtime hostile child frames are admitted once per call ID, so
  duplicate stdout calls cannot repeat host effects.
- [x] Node TypeScript strip/parser failures are classified as replayable
  program exceptions rather than worker-exit transport failures.

#### 2026-08-29 provider-generation lifetime delta

- [x] Add ref-counted publication generations and defer retired-provider close
  until all assembled Agent turns and active streams release their leases.
- [x] Make long-lived title/eval/compaction/subagent consumers resolve their
  route at operation start instead of retaining a concrete retired adapter.
- [x] Add a context-aware per-operation credential-provider seam to every
  production LLM adapter and wire built-in/custom routes to the locked
  settings/environment resolver.
- [~] Add cross-process credential leases and complete rotation behavior, and
  finish the Web authorization matrix before marking credential lifetime
  equivalent; provider-catalog and MCP projection redaction are covered by
  the later Web projection delta.

#### 2026-08-29 MCP/Web secret projection delta

- [x] Redact MCP configured header values in Web inventory and refresh
  responses while preserving non-sensitive header names.
- [x] Complete the MCP Web projection inventory for headers, credential-shaped
  command arguments, URL userinfo/query values, provider catalog values and
  refresh diagnostics; masked update values restore the existing secret
  instead of overwriting it.
- [~] Complete the Web mutation response and per-session authorization matrix.
- [x] Add a context-aware credential-provider seam to every production LLM
  adapter; each `Available`/`Stream` operation snapshots the current key and
  retired generations still wipe bootstrap material.
- [x] Wire the seam to the locked settings/environment resolver for all
  built-in and custom protocol routes, with rotation regression coverage.
- [~] Cross-process credential leases and the complete Web authorization matrix
  remain required before T13 can be closed; provider-catalog and MCP
  projection redaction are covered by the later Web projection delta.

#### 2026-08-29 runtime-context overlap and MCP projection delta

- [x] Keep Agent approval/compaction/plan/subagent projections on their runtime
  log when the Agent id overlaps the native `currentID` compatibility value.
- [x] Mask MCP Web argument, URL and header credentials in inventory and refresh
  diagnostics, and restore masked values on configuration update.
- [~] Complete Web per-user/session mutation authorization, cross-process
  lifecycle transactions, enforcing sandbox parity and the race/fault gates.

#### 2026-08-29 Agent runtime fail-closed overlap correction

- [x] Restrict runtime-context and native-goal log fallback to pure legacy
  construction; an active Agent Registry cannot route an unmaterialized Agent
  session into `currentID/a.log`.
- [x] Ensure static MCP bridge calls produce the same canonical `mcp/call`
  event as dynamic selector calls, using the addressed runtime sink.
- [~] Complete the remaining legacy-state migration, cross-process lifecycle,
  hostile sandbox and race/fault gates before claiming T1/T15 equivalence.

#### 2026-08-29 fork parent-boundary and durable-seed correction

- [x] Add an addressed parent-log resolver to the in-process fork provider;
  Agent-owned parents no longer fall through to the mutable legacy host log or
  an empty context.
- [x] Enforce a completed-turn seed boundary at the last `turn/end`, and keep
  child result derivation after that boundary, including cold resume.
- [x] Persist child lineage/header and fork seed atomically when the Store
  exposes `SessionCreateEventStore`, with a tested legacy fallback.
- [~] Full Agent publication/disposal transactionality, cross-process nested
  worker recovery and Team/ACP/Code Mode lifecycle parity remain open.

#### 2026-08-29 child publication rollback delta

- [x] Publish Spawn/fork child logs only after seed restoration, durable
  header/seed creation and the first lifecycle event succeed; injected store
  failure proves no ghost child is left in the runtime index.
- [x] When a parent Agent is cold, read its durable raw transcript before the
  completed-turn boundary is applied; a fork no longer silently loses context
  merely because the parent is not live in memory.
- [~] Cross-process nested-worker recovery, Team/ACP/Code Mode lifecycle and
  the strict fault/security/performance/race gates remain open.

#### 2026-08-29 child id cold-start correction

- [x] Synchronize Spawn/Fork child counters from durable session metadata before
  allocation; restart regression proves existing `spawn-N`/`fork-N` ids are not
  reused by a fresh provider instance.
- [~] Atomic id reservation against a simultaneous second process remains an
  open backend/storage requirement.

#### 2026-08-29 Agent memo disposal correction

- [x] Remove the app-side `sessionAgents` memo from Agent scope cleanup when
  Registry.Close disposes the handle; focused lifecycle coverage proves a
  closed handle is not reused by Web/Native materialization.
- [~] Cross-process Agent publication/disposal receipts and replay remain open.

#### 2026-08-29 release-gate rerun

- [x] Release gate passed diff check, full Go tests, vet/build, Web tests/build/
  manifest verification, and Linux/Windows cross-build.
- [ ] Strict `CGO_ENABLED=1 go test -race ./...` remains unverified because
  this Windows host has no `gcc`; a missing compiler is not an acceptable pass.

#### 2026-08-29 child start cancellation correction

- [x] Make synchronous Spawn/Fork durable header/seed initialization honor the
  caller context; cancellation regression proves no child is published after
  an interrupted create.
- [~] Legacy fallback stores still lack one cross-store transaction, and the
  cross-process lifecycle gate remains open.

#### 2026-08-29 reference replay verification

- [x] Run the core reference replay with
  `DSH_REFERENCE_ROOT=D:\\dev-projects\\Agent\\deepseek-harness`; the checked-in
  fixture test passes.
- [~] Extend reference-driven fixtures to tool, approval, Team, ACP/MCP and SDK
  lifecycle surfaces before closing T20.

## 2026-08-30 全面审计补齐清单（权威 backlog）

本节覆盖本轮“能力等价而非表面相似”审计发现。它是现阶段的收敛入口，
优先级高于历史增量记录；历史记录中的 `[x]` 仅表示局部实现或局部证据，
不得抵消本节的 `[ ]` 阻断项。除非 A0、A1、A2 及全部发布门禁关闭，项目状态
仍必须标记为 `FAIL / not capability-equivalent`。

### A0. 先冻结等价边界与证据基线

- [x] A0.1 固定参考版本：记录 `deepseek-harness` 的 commit/tag、profile、
  Node/Go 版本、操作系统和启动参数；本轮基线使用 `dsh-v0.1.0-rc.8`，
  但必须把实际选择写入可机器读取的 manifest。
  - 交付：`docs/equivalence-manifest.yaml`，包含 reference commit、profile、
    支持平台、禁用能力和兼容规则。
  - 验收：同一 manifest 在 CI、replay、工具目录生成和发布报告中被读取，
    不再依赖人工记忆“哪些是 optional”。
- [~] A0.2 明确三个 profile：`dsh-base/core`、当前产品完整 profile、
  平台能力矩阵。将 e2b、Python runtime、Cordis dynamic runner、Team、
  Codex/Claude/ACP provider 等标成 `required` 或 `profile-optional`，不能
  把 optional 当作已完成，也不能把未选 profile 的能力误报为核心缺口。
- [~] A0.3 生成单一能力目录：逐项记录 DSH seam、模型可见工具名、别名、
  schema、输入/输出 rich block、错误码、取消、超时、持久化事件、权限和
  所属 profile；Go 的额外能力也必须显式记录并决定“兼容、隐藏或删除”。
  - 特别处理：`get_time` 目前是 Go 默认模型工具，而 DSH 参考目录使用
    time context、没有同名模型工具；必须给出兼容规则。
- [x] A0.4 修正文档状态源：`docs/dsh-tool-capability-parity.md` 的 P4-P8
  仍未勾选，而对应阶段文档声称完成。所有阶段状态改由 manifest/测试生成，
  发布报告不得出现互相矛盾的“完成”。
- [~] A0.5 建立证据索引：每一个 `[x]` 都链接到源码、测试、fixture 和命令
  输出；只有“能在第二进程、冷恢复、取消和故障注入中复现”的证据才允许
  从 `[~]` 变成 `[x]`。

### A1. P0 运行时等价：先消除 bridge，再谈能力覆盖

- [x] A1.1 移除或正式隔离 `currentID`、`sessionStateMu`、`runTurnForLegacy`、
  `runtimeLogs` 等全局/兼容 turn 路径。所有 CLI、Web、ACP、SDK、subagent、
  Team、job、workflow 和 terminal 请求必须从 addressed Agent/Session
  解析 context，不得按进程当前 session 回退。
  - 相关现状：`cmd/pa/main.go` 的 compatibility fields、
    `cmd/pa/agent_runtime.go` 的 bridge、`runTurnForLegacy`。
  - 验收：两个 Agent 并发运行、切换 active session、关闭其中一个后，任何
    event、tool、approval、job、provider、projection 都不能串 session；
    删除 legacy 路径后全量测试仍通过。
- [x] A1.2 使所有 turn 都遵守同一个 canonical waterfall：
  `turn/start → step/start → user/message → request/header → assistant/* →
  tool/* → step/end → turn/end`。pre-step、turn-stopping、next-step、
  steering、quiet injection、question/approval 和内部 tool continuation
  必须逐事件对齐，而非只覆盖首个外部 step。
  - 验收：对 provider success、empty/max-token、transient error、fatal
    error、cancel、abort、approval、tool timeout、worker death 分别做
    reference replay；事件 seq、嵌套关系、history 和 retry 次数一致。
- [x] A1.3 将 retry/compaction 规则收敛为 provider-scoped canonical policy：
  明确 normal/always、Retry-After、max-token、abort、cancel、surface
  replacement、重试上限、provider fallback 和 disposal drain；每一次
  provider call（包括空响应和失败）都要生成可重放的 assistant/message 或
  normalized failure facts。
- [x] A1.4 固定“模型可见输入只能来自 durable log”的不变量。`DeriveHistory`
  是唯一 history source；raw stream chunk、request/context、tool result、
  steering 和 surface replacement 必须能从冷恢复 session 重建，不能依赖
  live memory 或 projection 私有缓存。
- [ ] A1.5 完成 Agent publication/disposal transaction：Agent handle、session
  header、首个 lifecycle event、registry index、provider lease、child/job
  owner 的提交/回滚必须有明确边界；任一步失败不得留下 ghost Agent、ghost
  child 或可继续使用的 disposed handle。

### A2. P0 持久化与跨进程一致性

- [~] A2.1 为 JSONL/SQLite 增加跨进程 writer lock、reader/writer 竞争、
  monotonic id reservation、migration、backup、repair 和 stale-lock recovery；
  仅有进程内 mutex 不算等价。
- [~] A2.2 建立事务边界：domain mutation + durable session event + projection/
  receipt 必须原子提交，覆盖 approval decision、Team membership/task/mailbox、
  MCP lifecycle、job completion、Agent publication、credential rotation、
  workflow/code receipt。
  - 验收：在每个 commit 点注入 crash/IO error，重启后只能得到 committed
    状态或可安全重试状态，不能得到半个 decision、重复 receipt、丢失 wakeup
    或悬空 claim。
- [~] A2.3 完成 corruption matrix：截断 header、坏 seq、重复 seq、半条 JSON、
  torn SQLite transaction、失配 fork seed、缺失 tool result、异常关闭和
  双进程 append；定义 fail-closed、repair、quarantine、backup restore 的
  明确结果。
- [~] A2.4 完成所有 owner/claim 的冷恢复：job、terminal、subagent、Team
  teammate、approval、MCP server、workflow child、provider generation；
  stale claim 必须可检测、可释放且不重复执行外部副作用。

### A3. P1 安全执行：sandbox 必须是实际 enforcement

- [x] A3.1 为每个支持平台提供可证明的 enforcing backend：workspace root、
  read-only/workspace-write/full-access、network、process tree、cwd/symlink
  traversal、credential exposure、CPU/wall/memory/output/file limits 均由
  backend 强制，而不是只写入 context 或 capability 字段。
  - 相关现状：Go 在 Windows 已明确承认 network denial 不在当前保证范围；
    `internal/config/config.go` 的 `SANDBOX_UNAVAILABLE` 规则不能替代 enforcement。
  - 验收：恶意 child process、越界路径、symlink、环境变量、网络连接、
    fork bomb、输出洪泛和超时 kill-tree 的 hostile fixture 在每个 profile/
    平台都 fail-closed。
- [ ] A3.2 若某平台无法提供上述 enforcement，就从该平台的支持矩阵移除
  `workspace-write`/`run_code`，或强制拒绝调用；不得把“controlled subprocess”
  宣称成 DSH sandbox 等价。
- [~] A3.3 完成 Code Mode worker 等价：在受控 Worker/等价隔离中执行，配置
  resource limits、compute budget、wall timeout、output budget、termination、
  quiescence、duplicate call-id admission 和 child cleanup；外部 Node
  `exec.CommandContext` 方案必须明确标为兼容降级，直到这些语义有同等证明。
- [ ] A3.4 完成 Workflow worker 等价：worker death、grace period、pending
  `agent-start`、唯一 `agent-end`、late JSONL、callback cancellation、
  result/receipt replay 和 child disposal 必须与 reference 对齐。

### A4. P1 工具注册、目录与模型可见性

- [~] A4.1 用生成目录替代散落的 whitelist：每个 ToolDefinition 必须带 owner、
  plugin、generation、profile、renderer、provenance、input/output schema、
  error taxonomy、cancel/timeout、durable event 和 allowed policy。
  `VisibleSpecs` 与实际 execute admission 必须消费同一份定义。
- [x] A4.2 修复已确认的 MCP 生产缺陷：默认 `mcpToolNames` 为空，导致
  `mcp_list`/`mcp_call` 在生产 registry 中被 policy 拒绝；补充完整配置到
  registry 的集成测试，不得只在测试手工 `Allow`。
- [x] A4.3 修复已确认的 `subagent_fork` 可见性缺口：实现虽存在，但当前
  注释和 root composition 明确把它排除在模型可见注册之外；按目标 profile
  注册 `subagent_fork`，并同步别名、schema、权限、seed boundary、错误和
  report 结果。
- [ ] A4.4 逐工具完成 reference contract：文件读写/edit、glob/grep、shell/
  persistent shell、terminal、jobs、schedule、goal、plan、todo、question、
  session query、subagent、Team、workflow、web、LSP、MCP、Code Mode、ralph、
  skill、report 和 interrupt/control。
  每个工具至少覆盖 valid/invalid、unknown、timeout、cancel、approval、
  rich result/image、spill、replay、disabled profile 和 disposed owner。
- [~] A4.5 对外部工具补齐 lifecycle：MCP 的 list-changed/reconnect/task/auth、
  terminal/job process ownership、LSP startup/shutdown、web fetch/search
  cancellation、plugin generation reload；未支持的 DSH capability 必须在
  manifest 中明确 `unsupported`，不能静默伪装成成功。

### A5. P1 Subagent、Team、Job、Terminal 所有权

- [~] A5.1 将 spawn/fork/resume/status/cancel/list/send/followup/wait/report
  统一到 Agent-owned scope；child log、lineage、seed、provider、credential、
  tool policy 和 disposal 都必须有 owner authorization。
- [~] A5.2 完成 Team roster/member/task/mailbox 的 append-only fold、CAS revision、
  blocker DAG、receipt、cross-session authorization 和跨进程恢复；
  `team_task_create/get/list/update/wait` 等模型可见名字与 reference profile
  对齐，或在 manifest 中标成 profile-optional。
- [~] A5.3 job/terminal 必须具备 durable owner、claim、completion wakeup、
  restart recovery、kill-tree、late result rejection 和跨进程 exactly-once/
  safely-retry 语义；关闭 Agent 后不得再接收其 job/terminal 写入。
- [~] A5.4 对 child id、Team member id、task id、job id 建立跨进程原子预留；
  SQLite 已新增 namespace-scoped durable reservation，并接入普通 session、
  subagent/fork child、Team task/message 与 Local job 的生成边界；Team member
  identity、旧 job/event identity 回填、以及 reservation 与 domain receipt 的
  同事务证明仍未完成。仅从 durable metadata 同步计数器仍不足以覆盖所有命名空间。

### A6. P1 Credential、Provider、Model Catalog

- [~] A6.1 建立独立 credentials seam：配置只保存 credential reference，
  provider/secret store 持有真实值；支持 per-operation resolution、lease、
  rotation、撤销、wipe、in-flight drain、owner disposal 和错误/日志脱敏。
  不能把 `llm.key.<provider>` 直接放入通用 settings 当作等价实现。
- [~] A6.2 将 provider generation lease 与 credential lease 绑定：新请求不可
  读旧 generation，旧 stream 结束前旧 secret 不得销毁，rotation/close 后又
  必须在 quiescence 时擦除；跨进程需要同等锁和恢复规则。
- [ ] A6.3 完成远程 model catalog/capability resolution：模型名、context/token
  capacity、reasoning、tool/vision/audio 支持和 provider route 必须由同一
  catalog 驱动 CLI/Web/ACP/SDK/agent loop，不允许各入口自己猜默认值。
- [~] A6.4 加入 secret hostile tests：进程环境、错误、spill、MCP headers/URL、
  provider catalog、Web inventory、crash dump 和 telemetry 均不得泄漏 credential。

### A7. P2 ACP、MCP、SDK、Web 外部协议

- [ ] A7.1 ACP 完成外部客户端矩阵：initialize/auth/session/new/resume/
  reconnect/prompt/cancel/update/permissions、cwd/additionalContexts、
  attachment、resource/image/audio/embedded、错误、断线和重连。对 reference
  明确拒绝的能力保留拒绝，但错误码、时序、ownership 和 durable replay 必须一致。
- [ ] A7.2 MCP 完成 stdio 与 Streamable HTTP 的外部 replay：initialize、headers/
  auth、session id、pagination、list-changed、SSE、call error、image/rich
  result、reconnect、close、taskSupport 和 server ownership；跨进程重复启动
  或断线时不得重复外部副作用。
- [ ] A7.3 SDK/client/provider 逐协议验证 request wire、stream chunks、empty
  response、usage、finish reason、retry、auth、tool calls、image/audio、close
  和 cancellation；补齐 remote catalog，而不是只验证本地 fake provider。
- [~] A7.4 Web mutation 全面加入 user/session/Agent authorization、CSRF/跨源
  边界、错误投影、masked secret update、reconnect replay 和 shutdown admission；
  当前 bearer middleware 之外已加入 Origin/Referer 同 Host 检查（无浏览器
  provenance 的 CLI/ACP/native 请求保持兼容），但 per-user/session/Agent
  authorization、reconnect replay 与 shutdown admission 的完整矩阵仍未闭合，
  不能只验证 inventory 的脱敏。

### A8. P2 Projection、Telemetry、Storage Domain

- [ ] A8.1 让 CLI/native/Web/ACP/SDK 使用同一 canonical projection；session
  query、trajectory、history、title、feedback、plan、todo、approval、Team、
  jobs 和 MCP inventory 不得各自维护不可回放的副本。
- [ ] A8.2 补齐 session projection cache 的 revision/invalidaton/rebuild 语义：
  cache 丢失、旧 revision、并发 reconnect、partial write 和 replay 后结果
  必须稳定。
- [~] A8.3 明确 storage/storage-domain、file reference、session reference、
  directory picker、e2b、Python code runtime、Cordis dynamic runner/inspect
  等 profile seam；选中的 profile 必须实现并验收，未选中的 profile 必须给出
  可验证的 disabled response，不得让客户端看到“有接口但永远失败”的假能力。
- [~] A8.4 Telemetry 只做 best-effort，不得改变 durable outcome；完成 provider/
  session/user resource identity、redaction、queue retry、collector failure、
  shutdown flush 和 telemetry fault isolation 的 reference 对比。

### A9. T20 最终门禁与可重复验证

- [ ] A9.1 生成跨入口 contract suite：相同 fixture 依次跑 native CLI、Web、ACP、
  SDK、Agent child、Team、JSONL、SQLite 和 reference；比较 event envelope、
  seq、history、tool schema/result/error、projection、side effect 和 cleanup。
- [ ] A9.2 生成 capability-negative suite：每项 disabled/unsupported、权限拒绝、
  sandbox unavailable、unknown tool/event、坏 schema、过期 approval、disposed
  owner、worker death、网络断开都必须返回稳定 fail-closed 结果。
- [ ] A9.3 完成 fault/security suite：跨进程竞争、kill -9、崩溃点注入、磁盘满、
  pipe 洪泛、子进程树、symlink、越界网络、credential rotation、generation
  reload、Team receipt 和 MCP reconnect。
- [ ] A9.4 完成 race/leak/process gate：在有 CGO 的 Linux/Windows CI 中运行
  `CGO_ENABLED=1 go test -race ./...`，并执行 goroutine、FD、child process、
  worker、temporary file、SQLite lock、provider/credential wipe 检查；本机无
  `gcc` 只能记录为未验证，不能判定通过。
- [ ] A9.5 只有以下条件全部满足才更新为 `capability-equivalent`：A0-A8 无
  `[ ]`/未解释 `[~]`，reference replay 无未解释差异，所有支持平台安全门禁
  通过，`go test ./...`、`go vet ./...`、`go build ./...` 和 strict race gate
  全绿，且发布报告引用同一 manifest 和 commit。

### 本轮确认的立即修复项（按顺序）

1. [x] MCP 默认 allowlist：修复 `mcpToolNames=[]` 与生产 registry admission，
   增加不依赖测试手工 policy 的回归测试。
2. [x] `subagent_fork` composition root：注册到目标 profile 的模型可见工具目录，
   完成 alias/schema/policy/seed/replay 测试。
3. [~] 文档/目录一致性：解决 P4-P8 checklist 与阶段文档的矛盾，并处理 Go
   `get_time` 相对 DSH time context 的额外表面。
4. [~] 运行时 bridge：冻结所有 legacy fallback 新增，列出迁移剩余调用点，
   将每个入口切到 Agent-owned context 后再删除兼容字段。
5. [ ] 安全与验证：先确定 Windows enforcing sandbox 的实现或支持边界，随后在
   有 CGO 的 CI 完成 race/fault/security gate；在这两项关闭前不发布等价声明。

#### 2026-08-30 model-visible composition root correction

- [x] Native composition root now registers `subagent_fork`, and the normalized
  subagent whitelist includes it; registry visibility is covered by a model-facing
  `VisibleSpecs` assertion.
- [x] ACP composition root now registers the same fork surface instead of only
  creating an internal fork provider; the ACP test proves provider and tool
  visibility together.
- [x] MCP default configuration now includes `mcp_list` and `mcp_call`, and a
  composition test executes `mcp_list` through `PolicyFromConfig` without a test-only
  manual allowlist.
- [x] A0.1 now has a machine-readable `docs/equivalence-manifest.yaml` fixing the
  reference commit/tag, subject baseline, profile scope, compatibility extensions,
  open blockers and evidence entry points; the release gate refuses a claim unless
  the manifest explicitly changes `claimAllowed` to `true`.
- [~] These fixes close only the immediate registration defects. They do not close
  generated catalog parity, cross-process MCP ownership/transactions, external
  lifecycle replay, or the remaining P0/P1 security and runtime gates.

#### 2026-08-30 persistence publication and diagnostic-redaction delta

- [x] SQLite append paths now reject gaps, duplicate positions and stale writer
  jumps while retaining exact-byte replay; the regression covers duplicate and
  non-contiguous batches.
- [x] Native session fork now publishes the closed seed, inherited metadata,
  runtime config and child lineage in one transaction. Invalid boundaries leave
  no child row; the native RPC no longer uses Create→Append→SetHeader cleanup.
- [x] The Web REST `/api/sessions/{id}/fork` path now uses the same atomic
  `SessionForkStore` capability on SQLite; its legacy multi-write implementation
  remains only for older lightweight Store adapters.
- [x] Atomic session creation is used by production native, ACP, Team and
  subagent publication paths where the SQLite capability is present. The first
  `subagent/start` fact is committed with the child row, while `SeedLength`
  remains the exact inherited-prefix boundary.
- [x] `internal/tools.Registry.CatalogJSON()` now exposes a versioned,
  deterministic machine-readable snapshot; prompt visibility and transport
  inventory continue to derive from the same catalog snapshot.
- [x] MCP startup and reconnect diagnostics now redact configured URL, argv,
  header and environment secrets before console emission; Web inventory and
  refresh already use the same redaction boundary.
- [x] Verification completed on this workspace: `go test ./... -count=1`,
  `go vet ./...`, `go build ./...`, Web tests/build/manifest verification,
  core reference replay, and targeted MCP/session/fork regressions.
- [~] These changes close concrete publication and logging windows only. Full
  cross-process domain transactions, hostile sandbox enforcement, generated
  catalog parity, external protocol replay, authorization, telemetry fault
  isolation, race/fault/security and performance gates remain open; manifest
  status must stay `fail` with `claimAllowed: false`.

### 推荐执行顺序与依赖

`A0 → A1/A2 → A3/A4 → A5/A6 → A7/A8 → A9`

其中 A3 的平台支持边界会影响 A0 manifest，A1/A2 是所有跨入口 replay 的
前置条件，A4 的生成目录是 A7 外部协议和 A9 contract suite 的输入；因此不应
通过先补齐工具名称或 UI 来提前关闭 P0。每完成一个任务，必须同时更新本节
checkbox、manifest、对应测试和证据索引，避免再次出现“阶段文档已完成、总门禁
仍未通过”的状态漂移。
### 2026-08-31 external crash-boundary task expansion

- [ ] A2.2 remains partial until MCP child kill/restart, job wake-budget cold
  semantics, and journal-failure retry are covered by executable negative tests.
- [ ] A2.4 remains partial until owner recovery proves no duplicated wake after
  restart and provider generations are reconstructed only from durable
  config/settings/credential state.
- [~] A2.5 is a new P0 blocker: every MCP, terminal, schedule, workflow-child,
  subagent and plugin side effect must declare its crash contract
  (at-most-once, durable retryable receipt, or audited unordered failure).
  Failed transports must not be silently replayed, and process-local lifecycle
  must not be disguised as durable domain state.
- [~] A3.4, A0.3, A4.1, A4.6 and A4.7 remain done only within their stated
  boundaries; they do not close A2.5 or the full external lifecycle matrix.

### 2026-08-31 MCP crash and job budget delta

- [x] A real MCP stdio regression now kills the first child during
  `tools/call`, proves replacement generation ownership, and asserts by a
  cross-process journal that the failed call is not replayed.
- [x] Job completion recovery and live settlement now use one wake-budget
  delivery boundary. This fixes a memoized-Agent recovery bypass.
- [x] Injected journal failure now proves no splice is committed on failure,
  exactly one receipt is committed on retry, and repeats do not duplicate.
- [~] A2.2/A2.4 remain partial: persisted-store cold materialization, MCP
  Start/ListTools/list-changed, HTTP generations and other external receipts
  still need their full kill-point matrices.

#### 2026-08-31 MCP initial failure correction

- [x] Initial MCP `Start` failures now enter the reference-shaped bounded
  supervisor retry budget. Spawn and initialize handshake failures are included.
- [x] Real child-process regressions cover kill points during `initialize`,
  `tools/list` and `tools/call`, with replacement generation ownership and no
  failed-call replay.
- [x] Optional MCP startup retains the recovering supervised client for shutdown
  and publishes only after replacement discovery succeeds. Explicit
  `fail_on_startup_error=true` still closes loudly.
- [~] A4.5 remains open for list-changed during recovery, HTTP generation
  retirement, provider cold recovery and other external crash boundaries.

#### 2026-08-31 MCP generation boundary closure

- [x] MCP `list_changed` is generation-aware: notifications during
  reconnect/backoff and from retired generations cannot trigger a stale tool
  resync; only the current generation can refresh.
- [x] HTTP generation replacement explicitly deletes the old MCP session and
  performs later discovery only through the replacement session.
- [x] Client close waits for in-flight `ListTools` before retiring the
  generation.
- [~] A4.5 remains open for MCP task/auth external matrices, provider cold
  recovery, and terminal/job/LSP/Web/plugin lifecycle contracts.

#### 2026-08-31 provider cold restart correction

- [x] Provider generation cold recovery now uses a real parent/child process
  boundary. The child rebuilds a durable custom provider route from SQLite and
  the dedicated credential backend, publishes a new generation, and completes a
  real streaming request.
- [x] The parent proves the old generation drains and retires exactly once, and
  its closed in-memory credential vault rejects later resolve attempts.
- [~] A2.4 remains partial for persisted owner/materialization recovery; A6.2
  remains partial for provider-policy, revocation and hostile-secret matrices.

#### 2026-08-31 persisted owner materialization closure

- [x] A2.4 cold owner materialization now has a SQLite end-to-end regression:
  one app writes only `job/done`, the next rebuilds the owner and writes exactly
  one durable inbox receipt, and an independent store read verifies the pair.
- [x] A2.4 is done in the machine-readable register. Its broad owner/claim
  acceptance command passes across internal packages and the composition root.
- [~] A2 remains partial overall because transactional external crash contracts
  remain under A2.2/A2.5; A6.2 policy/revocation work remains open.

#### 2026-08-31 provider credential revocation closure

- [x] A6.2 covers revocation across all provider protocol families, not only the
  shared DeepSeek/OpenAI client. Live streams retain their lease; later streams
  receive stable `CREDENTIAL_UNAVAILABLE`; terminal drain releases the lease and
  diagnostics do not disclose the secret.
- [x] A6.2 now has executable evidence for rotation, backend failure, in-flight
  drain/wipe, retired generation rejection and cross-process cold rebuild.
- [~] Secret-store migration and broad non-disclosure/cancellation matrices
  remain under A6.1/A6.4.

#### 2026-08-31 Web credential seam closure

- [x] Built-in/custom Web provider credential writes now use the dedicated
  Vault backend; generic `llm.key.*` settings are only a compatibility fallback
  for embedders without a Vault.
- [x] Added SQLite regressions for Web save, clear, rollback, custom save and
  custom delete. Dedicated records are created or removed exactly as requested,
  and no secret value appears in generic settings on the Vault path.
- [x] A6.1 is done. A6.4 still owns the broader hostile-secret, telemetry and
  cancellation non-disclosure matrix.

#### 2026-08-31 session Agent journal retry closure

- [x] Extended receipt-retry evidence from the direct journal helper to the
  production `sessionAgent` memo path with transient load and sink failures.
- [x] Fixed the stale runtime journal exposed by that test: memoized Agents now
  resolve the current session log for each inbox write, preventing conflicting
  durable sequences after reload.
- [~] A1.5 remains partial pending its A2.2/A5.1 dependency and release-boundary
  decisions, but this production retry omission is closed.

#### 2026-08-31 subagent ownership closure

- [x] Added a consolidated ownership matrix for the subagent control plane.
  Cross-owner status/resume/send/followup/interrupt/cancel/list attempts fail
  closed; wait and list discovery do not project foreign children; no foreign
  callback runs.
- [x] A5.1 is done with executable evidence for addressed runtime isolation,
  cold restore, lineage authorization and disposal-safe Agent ownership.
- [~] A5 remains partial for Team lifecycle, job/terminal receipts and broader
  identity reservation transaction work.

#### 2026-08-31 job/terminal owner closure

- [x] A5.3 is done. Job owner close rejects late admission, cancels live work,
  waits for the settlement observer, and preserves first-wins terminal facts.
- [x] Terminal owner close removes only owned sessions, tears down owned
  process trees, and records one durable stop edge. Cold restart appends one
  stop receipt for stale claims and skips live processes.
- [x] SQLite `job/done` cold materialization and failed inbox-splice retry are
  covered by production sessionAgent regressions.
- [~] A5 remains partial for Team lifecycle and reservation/receipt
  transactional work under A5.2/A5.4.

#### 2026-08-31 Team identity and lifecycle closure

- [x] A5.4 is done. Legacy job, Team member, task and message identities use
  durable reservations; restarts reject reuse; orphan job/task/message tokens
  are skipped without stalling future generation; and Team member atomic
  publication rolls back reservation plus receipt together.
- [x] A5.2 is done. Team state has executable negative-path evidence for
  append-only replay, CAS/stale revision, blockers/cycles/tombstones,
  authorization, member death/rebind, cold snapshots and duplicate delivery.
- [~] A5 is closed, but A2.2/A2.5 transaction and crash-boundary work remains a
  required blocker.

#### 2026-08-31 schedule crash replay closure

- [x] Durable schedule reminders now have crash-window replay evidence for
  owner inbox, schedule/fire and scheduler dispatch. One occurrence remains one
  owner receipt and one fire fact across restart.
- [x] Fixed claimed-schedule duplicate wakes and fixed terminal owner close to
  ignore unrelated audit-only session events.
- [~] A2.2 remains partial only for external protocol auth/task kill-point
  classification; A2.5 still requires the complete external-boundary matrix.

#### 2026-08-31 MCP external kill-point closure

- [x] HTTP auth failures now have no-replay evidence: the failed call is
  terminal to the caller, the MCP session remains usable, and one later explicit
  call creates exactly one external effect.
- [x] Required MCP task execution is rejected before real HTTP `tools/call`;
  the external server receives discovery but no side-effect request.
- [x] A2.2 is done. A2.5 remains open for the exhaustive external-boundary
  classification matrix.

#### 2026-08-31 storage corruption closure

- [x] A2.3 covers both SQLite and JSONL with interrupted/torn tails, malformed
  or conflicting replay, sequence gaps/duplicates, atomic seeds/forks,
  process-lock recovery, backup/integrity and bounded non-mutating reads.
- [x] Session contract evidence rejects unpaired tool results, so a missing or
  unpaired result cannot silently enter replay.
- [x] A2.3 is done. A2.5 remains open for external-boundary classification.

#### 2026-08-31 storage lifecycle closure

- [x] A2.1 covers SQLite and JSONL writer locks, child-process stale-lock
  recovery, contiguous append ordering, durable reservations, migration,
  backup, integrity and repair.
- [x] New SQLite evidence migrates a real v1-style database without losing
  rows, rejects newer schemas, and proves independent handles preserve one
  conflict-free sequence namespace.
- [x] A2.1 is done.

#### 2026-08-31 Agent publication closure

- [x] A1.5 separates durable session-header publication from process-local
  Agent handle creation/disposal, matching the reference model.
- [x] Failed publication leaves no ghost handle, disposed capability leak or
  owned job; cascade disposal, stale memo replacement and cross-process receipt
  replay are covered by executable regressions.
- [x] A1.5 is done.
