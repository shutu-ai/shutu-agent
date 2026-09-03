# DeepSeek Harness 等价审计：实现状态校正

更新时间：2026-08-28

`docs/dsh-equivalence-tasks.md` 是完整任务清单；本文件记录后续实现对其中状态的校正，避免把“已有接口”误报成“已达到能力等价”。

## Latest verified delta (2026-09-01, shared Team projection)

- `projection.Snapshot` now folds optional Team board state from a durable
  `team/snapshot` plus incremental member/task/mailbox events. Non-contiguous
  Team state fails cold rebuild, and live Cursor append rolls back rejected
  Team folds before the durable cursor advances.
- Web session state now reads the optional Team projection from the same
  Snapshot. This advances A8.1; the complete trajectory/control-state
  cross-entry fixture remains outstanding.

## Latest verified delta (2026-09-01, active-process exhaustion enforcement)

- Windows Job Objects now enforce Code Mode and controlled-shell active-process
  ceilings at the kernel boundary. TypeScript uses a 16-process backstop;
  controlled shell forwards its configured `MaxProcesses`.
- A real Windows child process attempts four concurrent descendants under a
  two-process Job Object. The kernel admits exactly one, proving a fork-bomb
  fan-out cannot exceed the owned-tree ceiling. Runnable POSIX fork-bomb
  fixtures and claimed-platform validation remain open.

## Latest verified delta (2026-09-01, HTTP network boundary oracle)

- A9.3 now has a real second-process HTTP cross-origin boundary oracle. The
  production fetch provider blocks the redirect before contacting the target;
  the child publishes a bounded settlement status, and an independent JSONL
  handle verifies the committed durable prefix did not gain a partial result.
- This removes HTTP cross-origin network loss from A9.3's remaining fault
  list. Process-tree exhaustion, hostile-worker, and claimed-platform matrix
  work remain open.

## Latest verified delta (2026-09-01, shared session-query relations)

- `projection.EventRelations` now owns replacement chains, shadowed-event
  ranges, and assistant/tool-call edges for session query. It validates the
  complete durable stream before deriving relations.
- `session_event_trace` no longer privately parses `surfaceOp` or tool-call
  relationships. Query relations now share the same cold-replay authority as
  Native history and SDK projection. A8.1 remains partial for the full
  trajectory/control-state cross-entry fixture.

## Latest verified delta (2026-09-01, JSONL fault restart oracles)

- A9.3 now includes real second-process JSONL disk-full and kill-at-every-write
  oracles. ENOSPC rolls back the complete physical batch; abrupt process death
  after the first complete record settles only that event externally, and a
  reopened handle deterministically closes the interrupted turn.
- The oracle reads the physical file before recovery, reopens storage for
  replay, and preserves the committed prefix. Process-tree exhaustion, network
  boundary, hostile-worker, and claimed-platform fault work remain open.

## Latest verified delta (2026-09-01, permission/reconnect shared projection)

- Shared projection now owns the durable permission/sandbox tier. Native live
  changes and history baseline read it, with session config only as a no-event
  fallback; malformed permission/sandbox control facts fail cold rebuild.
- ACP resume now rejects projection-invalid transcripts before composing a
  runtime and derives `ResumeMetadata.eventCursor` from
  `projection.Snapshot.AsOfSeq` rather than a private raw-event scan. This
  advances A8.1; the complete native/SDK/ACP trajectory/control-state
  equivalence fixture remains outstanding.

## Latest verified delta (2026-09-01, SQLite-backed all-tool negatives)

- A9.2 now has SQLite durable-replay evidence for every required model-facing
  tool. Each approval-boundary rejection is committed as a canonical
  turn/step/tool-call/tool-error lifecycle, reloaded with `Store.LoadSession`,
  byte-compared, and checked for stable call ID, output, and error code.
- The tool body never enters for these rejections, so no tool-side durable fact
  is expected. Cross-process external-effect oracles remain representative
  rather than every-dependency; A9.2 therefore remains partial.

## Latest verified delta (2026-09-01, stable unsupported provider input)

- The provider seam now rejects audio/resource/vendor request blocks before
  credentials, attachments, image offloading, or network I/O with the stable
  code `UNSUPPORTED_INPUT_CONTENT`. DeepSeek, OpenAI-compatible, Anthropic,
  Gemini, and OpenAI Responses share this admission boundary.
- Durable rich-block preservation remains unchanged: audio/resource metadata can
  survive replay, but it cannot be silently serialized to a provider that does
  not honor it. This closes the audio-admission subcase while the external
  reference/provider-header replay matrix keeps A7.3 partial.

## Latest verified delta (2026-09-01, pinned complete model catalog)

- A6.3 is now done. The model catalog is generated from the reference-pinned
  `pi-ai@0.82.1` provider inventory (38 providers, 1,234 models), not a smaller
  hand-picked subset. It preserves upstream input/capacity/reasoning facts and
  effort wires while merging Shutu-owned defaults and tool facts by model ID.
- Built-in discovery now returns owned catalog rows rather than ID-only
  suggestions. Unsupported/unavailable model selection remains fail-closed.
- This closes static catalog parity. A7.3 still owns the broader external
  wire/reference matrix.

## Latest verified delta (2026-09-01, unavailable Code Mode fail-closed)

- A3.2 is now done as a fail-closed contract. A single unavailable Node
  permission probe is followed through the production composition: Native
  projection, Web capability/tool inventory, ACP catalog, SDK catalog, and the
  registry all remove or reject `run_code`; startup remains healthy and reports
  the reason.
- This does not provide Code Mode enforcement or make the sandbox equivalent.
  A3.1 remains separately blocked on real enforcement/resource proof.

## Latest verified delta (2026-09-01, complete tool contract matrix)

- A4.4 is now done. A production-object matrix instantiates every required
  model-facing tool from its owning package and proves disabled, schema-invalid,
  and approval/pre-execute denial paths at the common Registry boundary. Every
  rejection emits exactly one terminal result observer, and the approval denial
  is replayed through a canonical tool/call plus source-linked tool/error event.
- The matrix exposed a real rich-output boundary bug: the Registry rejected
  merge-extensible `audio/resource` blocks even when their lossless wire bytes
  were retained. Unknown rich blocks now require `Raw`; the matrix proves image,
  audio metadata, and resource metadata survive Registry normalization, canonical
  tool/result append, restore, and derived provider history.
- Transport-specific rich adaptations remain covered by ACP, MCP, Code Mode, and
  session suites. The remaining required blockers are A3.1, A3.3, A7.3,
  A8.1, and A9.1-A9.5.

## Latest verified delta (2026-09-01, second ACP client and sequence authority)

- A second independent external ACP client now drives the production
  SQLite/`acpFactory`/loop/server composition through authenticate,
  initialize, session/new, prompt/update, reverse permission approval and tool
  execution, durable image input, cancellation, reconnect, and recovery
  provider-history replay.
- The fixture exposed and fixed a real sequence-authority race: ACP session
  logs are now registered as the session runtime log, so asynchronous titles
  and later prompts share one append authority. Resume atomically replaces
  that runtime log.
- SQLite interrupted-tail repair now handles a legitimate tail advance during
  recovery by reloading the durable prefix, recomputing closers, and retrying
  with a typed conflict sentinel.
- A7.1 moves to done; A7.2 is closed by the later restart-effect oracle.

## Latest verified delta (2026-09-01, MCP restart effect oracle)

- The real two-process stdio reconnect fixture now writes and fsyncs an
  external effect in the crashing generation before it exits. The replacement
  generation must not receive a replayed `tools/call`; discovery-only evidence
  and an exactly-one effect journal close the at-most-once restart contract.
- Combined with HTTP auth/session/fault, pagination, SSE/list-changed,
  structured rich results, task metadata, DELETE close receipts, generation
  ownership, and reconnect tests, A7.2 moves to done.

## Latest verified delta (2026-09-01, MCP call and reconnect semantics)

- MCP `tools/call` no longer performs a foreground retry after a connection
  error or timeout. The original failure is returned because the remote side
  may already have committed an external side effect; background supervision
  remains responsible for reconnecting the generation.
- MCP reconnect defaults now match the pinned reference: 500ms initial delay,
  30s maximum delay, and 10 attempts. Focused MCP/config/CLI regression tests
  pass; the A7.2 external client and fault matrices remain release blockers.
- CLI, Web, and ACP plan-mode reads now use the shared projection snapshot.
  The projection rejects malformed boolean control facts instead of silently
  treating them as inactive; remaining native/ACP/SDK projection migration is
  still tracked under A8.1.

## Authoritative equivalence blocker index

`docs/equivalence-manifest.yaml` 是唯一 release-status source。当前 required
blocker IDs 必须与下列列表一致：

- A9.3
- A9.4
- A9.5

## 已有局部实现与证据

- T4：JSONL 与 SQLite 已共享 `SessionPersistence`，包括 `ReadFrom`、`ListSnapshots`、revision、header、fork、flush、inspect 及共同 backend contract suite。
- T4：SQLite 恢复性 `LoadSession` 会关闭结构合法的 interrupted tail；严格 `InspectSession` 与 native history/fork 的 raw live-tail 读取保持非变更边界。
- T5：plugin manifest、依赖校验、profile/bundle/patch inventory、generation reload 排空及 disposer 已有实现和回归测试。
- T7/T9：session-scoped approval 已能投影 `approval/asked|approval/decided`；CLI 兼容路径仍保留，三类 answerer 的完整 replay contract 尚未闭环。
- T8：工具 registry 已提供稳定错误码、canonical rich content、spill locator/source linkage、
  merge-extensible rich-block 保留与 `VisibleSpecs`；每个 required model-facing 工具已有
  统一负路径、terminal result 观测和规范 rejection replay 证据。
- T10：teammate 已接入 Agent Registry、durable child session、roster rebind 与 subagent control plane；完整 snapshot、cold inspection、authorization contract 仍未完成。
- T14：ACP resume/reconnect 现在有 durable metadata、unknown-session cancel 幂等语义、in-flight prompt replacement barrier 和外部 wire contract。reference 当前拒绝 MCP required task execution，Go 在 transport 前拒绝；Streamable HTTP lifecycle 有测试。全量协议 client/fault matrix 仍未完成。
- T14：ACP image capability 现在要求 exact selected provider route 的显式 `SupportsImages`，retry wrapper 会透传该能力；audio/embedded/additional dirs/MCP 仍按 reference 当前行为拒绝。
- T15：DeepSeek Search 的 request event 已支持 caller-context 路由，Agent 并发 session 不再必须共享全局 Web 日志；其余 Web mutation、authorization、reconnect repair 仍未完成。
- T18：SSE 已按 durable sequence 做 snapshot/live 去重、缺口回补和尾部周期校准；全量 mutation authorization 仍未完成。
- T13：session 选择了未注册 provider 时已改为 fail-closed，不能静默回退到全局 provider；provider 切换期间的完整运行中 Agent 原子性与凭证生命周期仍未完成。
- T13：provider/profile 编辑失败现在会补偿恢复持久化 settings、内存索引和旧 registry；并发运行 Agent 的原子快照与 secret-redaction 门禁仍未完成。
- T6/T13：sandbox 已区分显式 `readonly`、`workspace-write`、`danger-full-access`；readonly 仅在可用 bubblewrap 后端下开放且不创建目录，provider registry 与 Web/ACP 能力读取使用不可变配置快照。
- T16：durable schedule projection 已按 runtime session 隔离，提醒触发会按 session 的 Agent inbox 投递，pre-step 与后台触发共享排他边界；jobs/workflow/skill 等其余后台路径仍需完成同等级生命周期迁移。

## 仍然阻断完整等价的任务

1. reference Harness 双端 replay 与跨运行时 history/trajectory/native projection 对比。
2. native legacy REPL、后台 job/schedule/workflow/skill 等路径彻底迁移到 Agent-scoped runtime context。
3. enforcing hostile-code sandbox，以及 read-only、workspace-write、full-access 的真实 per-call 隔离证明。
4. CLI/Web/ACP 统一 approval service、answerer ownership、expiry/unavailable、cold restore 与 replay 防重放。
5. Team roster、mailbox、snapshot/fork/restart/cold inspection 和 owner authorization 的持久化闭环。
6. ACP/MCP/SDK 的完整 content、permission、task、disconnect、resume/reconnect 外部 client contract。
7. nested Code Mode 的 parent call、generation、审批/沙箱继承、递归深度与资源回收契约。
8. storage migration、backup/repair、bounded read、multiprocess locking、deploy-safe permissions。
9. correlation/trace/metrics、fault/security/performance suite，以及可运行的 race-detector CI gate。

只有上述 P0/P1/P2 与门禁全部通过，才可以对外宣称 `capability-equivalent`。

## 本轮新增校正（2026-08-28，sandbox 与 approval 创建事务）

- Linux bubblewrap 现在独立探测并执行 network namespace；能力可用时非
  full-access 调用使用 `--unshare-net`，否则 `RequireNetworkIsolation`
  fail-closed。Windows 等平台不会伪称具备该能力。
- OpenAI-compatible 适配器的共享 HTTP/SSE 错误现在保留实际 route label，
  不再把错误诊断误标为 DeepSeek。
- `interact_ask` 与 `ask_user_question` 已接入与敏感工具、ACP 相同的
  atomic asked-event 创建 seam；SQLite 下 pending projection 与
  `approval/asked` 同事务提交，再投影到请求创建时捕获的 session log。

这些改动不改变非等价结论：approval 三 answerer 完整 replay、Team/Code
Mode/ACP-MCP 生命周期、cross-surface replay、telemetry 及最终故障/安全/
性能/race 门禁仍未全部完成。

## 本轮新增校正（2026-08-28，correlation propagation）

- runtime context 新增 agent/session/turn/step/request/call/generation 的
  结构化 correlation；loop request hook、provider 调用和 tool dispatch
  会继承同一组逐层收窄的身份，且没有改动 durable event wire。
- 新增并接入 concurrency-safe structured metrics 与 bounded span recorder，
  记录 turn/step/provider/tool/usage 及 typed failure code；已补并发与
  idempotence 回归测试。metrics/trace export 和 observer fault isolation
  仍未完成。
## 本轮新增校正（2026-08-28）

- T16：最小持久 shell 已按 runtime session 解析 owner 与 workspace，避免 Agent/Web 并发调用复用当前 REPL 会话的 shell。
- T6/T8：`plan_*` 工具与 goal continuation 已按 runtime session 懒加载独立 plan Engine，并从对应 session log 恢复；goal activation 回调也已 context-aware。
- 新增跨两个 Agent session 的 plan projection 隔离回归测试。
- 全量 `go test -count=1 ./...`、`go vet ./...`、`go build ./...` 通过；`go test -race ./...` 仍因 Windows 主机缺少 `gcc` 无法启动。

这些改动仍不足以宣称完整 DeepSeek Harness 能力等价；此前列出的 reference replay、hostile sandbox、统一 approval replay、完整 ACP/MCP/SDK 生命周期、nested Code Mode 限额、storage/telemetry 及 fault/security/performance gate 仍是硬缺口。
## 子 Agent 运行时补充（2026-08-28）

- SpawnProvider 已支持按父 session 解析 provider/model、tools 和 prompt；child loop 自带 child owner、runtime session context 与 child durable event sink。
- 这只消除了“固定使用全局运行时”的一组缺口；child 自身 plan/approval/team projection、完整 session persistence 与跨进程恢复仍需单独通过契约测试。

## Latest implementation correction (2026-08-28, sandbox and process locking)

- Linux local code execution now functionally probes bubblewrap before
  advertising strong isolation or read-only mode. The bwrap profile enforces
  read-only host files, workspace-only writes, and an ephemeral `/tmp`.
- JSONL persistence now uses a per-session OS file lock for create, append,
  load, inspect, and repair, covering independent service processes in
  addition to the existing in-process mutex.
- Other hosts and network isolation remain fail-closed where no enforcing
  backend is available; this is a capability reduction, not an equivalence
  claim.

- Compaction pressure and context-overflow recovery now use a per-runtime-session
  BasicEngine with that session's provider/model and log; stale projections are
  discarded when compaction is re-registered after a settings change.

- `JSONL.ReadFrom` now performs an incremental full-file validation while
  retaining only the requested cursor suffix; cold reads remain non-mutating,
  while `Load` remains the repair path. Both Unix and Windows process-lock
  branches compile.

## Latest implementation correction (2026-08-28, lossless Web SSE cursors)

- Web SSE suppresses snapshot/subscription duplicates, repairs durable sequence
  gaps through bounded suffix reads, and periodically reconciles the durable
  tail after non-blocking hub delivery.
- This removes event loss caused by a slow in-memory subscriber from the Web
  stream contract, but the broader Web authorization and cross-surface
  approval/replay gaps remain open.

## Latest implementation correction (2026-08-28, persistence and Code Mode)

- SQLite cursor reads now use bounded `seq >= N` pages when the backend exposes
  the seek seam; migrations are transactional and database/WAL files are
  private by default.
- Approval restore refuses ambiguous legacy request IDs shared by multiple
  sessions instead of silently assigning ownership to the last session read.
- Code Mode has ordered safe/exclusive sub-call scheduling with a default
  overlap cap of ten.

These are narrower hardening deltas. They do not close the persistence
coordinator, full approval service, Code Mode rich-content settlement, or the
remaining ACP/team/fault/performance gates.

## Latest implementation correction (2026-08-28, nested concurrency)

- Sibling subagent/fork/teammate calls are now classified as concurrency-safe
  for the loop's rolling pool; control operations remain exclusive.
- TypeScript Code Mode accepts a validated `code.max_parallel_sub_calls`
  setting and enforces ordered safe/exclusive nested dispatch.

The nested Code Mode content/argument/resource lifecycle and the broader
subagent/team persistence contract are still incomplete.

## Latest implementation correction (2026-08-28, maintenance and Team lineage)

Both transcript backends now have executable integrity checks, non-overwriting
backups, and explicit session repair, with regression coverage. Provider
deletion restores durable settings and the live registry when rebuild fails.
Team child sessions persist parent lineage and use the lead-owned shared board
for task/mailbox operations, while authorization still uses the child Agent
identity.

The equivalence claim remains blocked by the hard gaps listed above and by the
reference replay, approval replay, enforcing sandbox, ACP/MCP, Code Mode
settlement, observability, fault, security, performance, and race gates.

## Latest verified delta (2026-08-28, runtime isolation and lifecycle)

Web slash commands are now runtime-session aware, including their durable
event and plan projection writes. Configuration snapshots are deep and ACP
MCP/subagent runtimes use their pinned creation snapshot. Code Mode avoids a
legacy policy read on Agent-owned calls; Web stop cancellation is per session;
the goal scheduler cancels and joins its worker before resource closure.

Full capability equivalence remains unclaimed: the hard gates are still the
reference double replay, native global-state migration, enforcing hostile-code
sandbox, unified approval replay, full ACP/MCP/Team lifecycle, nested Code Mode
settlement, correlation/trace telemetry, storage coordinator/multiprocess
tests, and fault/security/performance/race CI.

## Latest implementation correction (2026-08-28, background ownership and approval correlation)

- Background `job_start` commands now receive the addressed session's captured
  workspace, and a failed `job/start` append cancels the just-created job so a
  failed tool call cannot leave an unlogged executable job behind.
- The goal scheduler no longer gates all reminders on the legacy global
  scheduler pointer; session-local schedulers are enumerated from the guarded
  map, including after restore.
- SQLite event append and backup operations now share an OS-level lock with
  independent processes. This serializes the transaction boundary in addition
  to SQLite's page locking; stale per-process sequence writers still fail with
  an explicit replay conflict rather than silently reordering events.
- Approval requests preserve the originating tool `callId` through the
  session-aware approval service and cold restore path.
- Subagent start-event persistence failures now cancel the newly-created child
  in foreground, background, and teammate paths.

These corrections harden commit-point and ownership behavior but do not close
the reference replay, enforcing sandbox, unified approval persistence,
complete ACP/MCP/Team lifecycle, nested Code Mode resource contract,
observability, or fault/security/performance/race gates.

## Latest implementation correction (2026-08-28, ACP identity and rich output)

ACP now exposes the durable session identity returned by the application
factory when one is available, so the ID from `session/new` is also valid for
resume/reconnect. Image prompt admission validates the entire batch before
writing attachments. Committed assistant text/image blocks are preflighted and
delivered in order; missing image attachments fail before partial updates.
SQLite schema creation and migration now share the append/backup OS lock for
concurrent first-open/deployment safety.
The shared core fixture is exercised by session restore/history and native
projection tests, and the checked-in reference ACP subset passed 82 tests.

This narrows ACP mismatches but does not satisfy the equivalence gate: the
reference double replay, durable unified approval service, enforcing sandbox,
full Team/MCP lifecycle, nested Code Mode resource contract, telemetry, and
fault/security/performance/race CI remain incomplete.
## Latest implementation correction (2026-08-28, approval and native replay)

The current build now serializes Web approval decisions and restores the
pending request if its durable event cannot be appended; the CLI gate follows
the same failure rule. ACP reuses the app approval service when interactions
are enabled and keeps policies session-scoped. Native live projection handles
canonical approval events as well as legacy interaction events, and Team
mutation adapters roll back when snapshot persistence fails.

This is still not a capability-equivalent claim: approval storage now has a
durable SQLite projection reconciled from the event log, but provider/event
commit is not one atomic transaction and the three answerer replay contract is
not complete. ACP/MCP/task/reconnect coverage is incomplete, sandbox
enforcement is incomplete, and the remaining contract/fault/security/
performance/race gates are open.

## Latest implementation correction (2026-08-28, MCP and Code Mode lifecycle)

The native composition root now actually mounts the existing dynamic MCP
selector implementations (`mcp_list` and `mcp_call`), rather than only
mounting static advertised-server bridges. Code Mode rejects lossy values at
the JavaScript boundary, accounts structured completions against its output
quota, cancels unawaited bindings after program settlement, and waits for
tracked processes on close. Generic ACP image admission and notification
write-error propagation were tightened to match the reference contract.

These changes are verified by targeted Go tests and the full selected ACP
reference suite (82/82), but the repository still must not claim full
DeepSeek Harness capability equivalence. Hostile-code enforcing sandboxing,
durable approval replay, complete Team persistence/authorization, full
Code Mode worker resource semantics, cross-surface replay, telemetry and the
fault/security/performance CI gates remain open.

## Latest implementation correction (2026-08-28, Agent hierarchy disposal)

Published child Agents are now registered with their parent scope, so parent
close cascades through the Agent registry and removes descendant publication;
explicit child close remains idempotent. This is covered by a focused Agent
regression test. It does not remove the native legacy/global compatibility
paths or close the remaining equivalence gates.

## Latest implementation correction (2026-08-28, durable approvals and nested rich dispatch)

SQLite now has an optional durable approval projection with restart-safe
request IDs, structured answers and compare-and-set terminal decisions. The
application uses it when available; event append failures still roll the live
approval back. Code Mode nested dispatch preserves ordered rich tool content,
metadata and additional context handles, and schedule fire occurrences are
deduplicated before retryable background delivery.

The build remains explicitly non-equivalent until provider/event atomicity,
reference replay, enforcing sandboxing, complete Team/ACP/MCP lifecycle,
observability and the fault/security/performance/race CI gates are closed.

## Latest implementation correction (2026-08-28, fail-closed runtime authorization)

Agent-owned turns now fail closed when durable session configuration or
session approval-policy injection fails; they no longer silently fall back to
global runtime state. `subagent_resume` also requires caller identity and
descendant-lineage authorization, including after a cold restart. Status,
cancel and explicit-parent list operations now apply the same caller scope.
Child/compaction provider lookup also fails closed on durable configuration
errors instead of reusing the global LLM.
Subagent child tool/prompt composition applies the same rejection behavior.
The native
legacy REPL retains an explicitly bounded compatibility fallback, so these
changes reduce but do not eliminate the global-path equivalence gap.

## Latest verified delta (2026-08-28, executable reference double replay)

The shared core-turn fixture now has an executable Go/reference replay gate.
`TestCoreTurnReplayMatchesReference` invokes the real reference Session source
when `DSH_REFERENCE_ROOT` is configured and compares surface order, history
roles/content and tool-call identity; the configured Windows checkout passed.
The gate remains explicitly skipped when the reference checkout is absent, and
additional replacement/protocol/ACP/MCP/Code Mode fixture families are still
required.

## Latest verified delta (2026-08-29, transaction and tool-policy hardening)

- SQLite approval resolution has an independent cross-process CAS regression:
  two service instances race one request and exactly one terminal decision and
  audit event commit.
- Team provisioning can atomically publish the child Session/header/closed
  fork seed with the lead `team/member` provisioning event; root sequence
  conflicts roll back the child as well.
- Tool registries now expose structured post-execute `accept|block` decisions,
  including value/content exclusivity, failed-result replacement rejection,
  context ordering and block-only feedback/context behavior.
- A rejected nested Code Mode tool preserves rich deferred contexts while its
  TypeScript promise remains rejected.

These are verified partial closures. The repository still must not claim full
DeepSeek Harness capability equivalence: the hostile sandbox, full Agent-scoped
migration, complete ACP/MCP/SDK lifecycle, Team active-transition transaction,
storage corruption/crash matrix, external telemetry and race gate remain open.

## Latest implementation correction (2026-08-28, projection reconciliation and namespace boundary)

Approval recovery now replaces, rather than merely upserts, the SQLite
projection from session-event facts, eliminating stale/orphan rows on cold
start. Code Mode exposes multiple validated portable namespaces and arbitrary
member names, meter nodes follow the current replacement-aware surface, and
process cleanup closes Agent runtimes before dependent services. The repository
remains non-equivalent until atomic provider/audit commit, hostile-code
sandboxing, full Team/ACP/MCP lifecycle, dual replay and final gates are closed.

## Latest verified delta (2026-08-28, loop failure-state alignment)

The loop now gives cancellation precedence over a stale request-error retry,
normalizes unsupported model finish reasons into typed failures, preserves
their codes in `turn/end`, and records pre-step refusal as `blocked`. These
changes close concrete state-matrix gaps, but do not complete the reference
retry policy/error taxonomy or the remaining live/durable replay gates.

The Agent Registry now rejects publication after an idempotent `CloseAll`,
closing one additional shutdown race without changing the repository's
non-equivalent status.

## Latest implementation correction (2026-08-28, Team task and Code Mode boundaries)

Team roster and task admission now enforce reference-style member naming,
bounded identities, active-target authorization, unowned task creation, CAS
transitions, owner/lead rules, dependency/reopen/reassign operations, delete
guards, workspace-relative scopes, and deployment limits. Cold restore fails
closed on corrupt snapshots or missing child lineage. Code Mode separates rich
binding projection from the JavaScript return value and charges completion
payloads against its output budget.

The implementation is still not capability-equivalent: Team state is still a
snapshot projection instead of the reference append-only fold, Code Mode does
not provide the reference worker compute/heap/resource contract, and the
approval, sandbox, ACP/MCP, telemetry, fault/security/performance/race gates
remain open.

Team mailbox delivery now has an Agent-scoped dispatch/ack seam and cold-start
redelivery for live targets. ACP approval requests preserve tool call IDs, and
ACP MCP external calls release the session mutex while awaiting server I/O.
The mailbox delivery receipt is still not atomically committed with the queue
event and child Session event, so this remains a hardening delta only.

SQLite approval decisions now have an atomic provider-CAS plus audit-event
transaction, with both normal and ACP consumers using the committed-event
projection. Team task and mailbox mutations also emit typed append-only events
and replay their tail after a legacy snapshot. Membership remains snapshot
backed, and full Team mailbox receipt atomicity is still open.

## Latest verified delta (2026-08-28, Team cold projection and observer isolation)

Team member facts are now retained by the Board during no-Registry cold
inspection and restored alongside tasks/mailbox state. Recovery reconciles a
durable provisioning row to active only when the child Session and lineage are
present; missing children become durable failed rows. Immediate mailbox
delivery commits the delivered journal edge before mutating the live queue, so
delivery-journal failure leaves the message retryable. Live session observers
are panic-contained after durable commit. The repository remains explicitly
non-equivalent: child-session receipt atomicity, full rich mailbox semantics,
external telemetry, sandbox enforcement, protocol lifecycle, and final fault/
security/performance/race gates remain open.

## Latest implementation correction (2026-08-29, telemetry and workspace boundaries)

- Session telemetry now follows the reference deployment switches: disabled by
  default, explicit `FULL`/`FEEDBACK_ONLY`, and authoritative
  `DSH_TELEMETRY_DISABLED`. Enabled modes use a bounded asynchronous OTLP/HTTP
  JSON log exporter connected to native, Agent and ACP session observers;
  `FEEDBACK_ONLY` replays the canonical log prefix through committed feedback.
- Filesystem, `grep` and `glob` host adapters now apply real-path workspace
  containment for addressed sessions, and the filesystem read-before-write map
  is concurrency protected. Generic job labels no longer default to raw
  command text and explicit labels are redacted before durable projection.

These are scoped hardening and integration deltas. SDK-level telemetry retry /
collector contracts, hostile-code enforcing sandboxing, legacy global-state
migration, cross-process transactionality, full ACP/MCP/Team/Code lifecycle and
the final fault/security/performance/race gates remain blocking items.
## 本轮已验证增量（2026-08-29）

- T1：Agent-owned jobs/plan/subagent/workflow 注册已改用显式 runtime-context owner resolver；legacy zero-context 构造器仍仅为兼容保留。
- T7：CLI/Web/ACP 生产审批入口共用 application-owned `approvalAnswerer`，统一 session ownership、CAS、durable decision projection 和失败回滚。
- T14：ACP 外部 wire contract 已覆盖 initialize/version、cwd/method errors、resume/reconnect、rich content；MCP stdio bridged client 已接入可关闭的指数退避监督器和 reconnect 后工具 schema resync。

上述增量均有针对性测试和全量普通 Go/Web 回归，但严格 CGO race 门禁在当前 Windows 环境因缺少 `gcc` 无法启动；这仍是发布阻断项。
## 本轮实现增量（2026-08-29，plan 与 quiesce）

- Agent-backed `/plan` 已统一 CLI/Web 的 idle commit、in-turn pending、steer、下一边界提交与 command lifecycle 恢复；Web 图片输入走 rich content。
- `jobs.Local.Close` 已等待 terminal observer 完成，避免 completion wake 在 Agent 关闭之后继续投递。
- Go 全量测试、vet、build 通过；严格 race 仍因当前 Windows 环境缺少 `gcc` 无法执行，因此不能宣称门禁全部通过或 capability-equivalent。
## Latest implementation delta: Agent maintenance and MCP close barrier

- `Agent.RunMaintenance` now enforces an Agent-owned idle maintenance phase:
  turn claiming is excluded at the claim boundary, inbox wakes remain queued,
  cancellation propagates through the task context, and concurrent `Close`
  calls wait for one disposal barrier.
- Agent-backed Native/Web `/compact` executes through this maintenance seam;
  legacy direct execution remains only for compatibility/test construction.
- The MCP reconnect supervisor now waits for in-flight `Start/ListTools/Call`
  operations before closing the underlying client, serializes starts, and has
  a reconnect-close regression test.
- Job settlement persists the owner-session `job/done` fact before attempting
  a live wake, including after shutdown admission has closed.

Still open: a full process-wide background quiesce coordinator, cross-process
receipt/event atomicity, the complete MCP task/HTTP/session lifecycle, hostile-
code enforcing sandboxing, and the strict CGO race gate unavailable on this
Windows host because no C compiler is installed.

## Latest implementation delta: rich additionalContexts (2026-08-29)

- Tool results now retain rich, source-attributed `llm.Message` contexts while
  keeping the legacy string form for compatibility.
- Nested Code Mode contexts are aggregated in submission order, included even
  when the outer program settles as a structured failure, and recorded on the
  nested dispatch projection.
- The loop commits deferred contexts after `tool/result`, so the next model
  step receives the same FIFO user-context surface as the reference loop.
- Successful tool results now support `concludesTurn`, and a composable
  post-execute around-hook waterfall is available to policy layers.
- This closes only the additional-context transport seam; full post-execute
  decision parity, worker resource enforcement and all producer migrations
  remain open.

## Latest implementation delta: child publication rollback (2026-08-29)

Spawn/fork child logs are now published to the Agent runtime index only after
seed restoration, durable header/seed creation, and the first lifecycle event
all succeed. Failed initialization cannot leave a resolvable ghost child;
this is an in-process publication fix, not closure of cross-process Team
receipts or the remaining lifecycle gates.

## Latest implementation delta: process-wide shutdown coordination (2026-08-29)

- The native composition root now uses `internal/lifecycle.Coordinator` for
  process-owned teardown instead of independent cleanup defers.
- Admission is registered last and therefore closes first; Jobs drain before
  Agents, and resource closers still provide their own in-flight barriers.
  Concurrent shutdown callers wait for the same terminal result, and
  post-shutdown registration fails closed.
- T16 is improved at the application boundary, but full capability equivalence
  remains blocked by the explicitly listed hostile-code, cross-process,
  protocol-lifecycle, storage-fault, and race-detector gates.

## Latest implementation delta: shared replay fixture consumers (2026-08-29)

- Session, JSONL/SQLite persistence and native Web projection contract tests
  now consume the same transport-neutral decoded core fixture records.
- This reduces cross-package contract drift but is not the complete T20 fixture
  matrix; tool, approval, Team, ACP/MCP and SDK fixtures remain outstanding.

## Latest implementation delta: resource close barriers (2026-08-29)

Schedule/plan/spill/interact engines, subagent runtime/providers, plugin and
skill registries, and the Web server now make concurrent close calls wait for
the active disposal. MCP stdio callbacks are drained before close completes;
persistent terminal cleanup returns a retryable timeout error instead of a
false success. These changes harden individual resource lifetimes but do not
establish full process-wide quiescence or change the remaining equivalence
blockers.
## Latest implementation correction (2026-08-29, tool value/render separation)

The Go tool registry now has optional tool-owned output rendering and
presentation metadata seams. Canonical output is validated before rendering;
renderer failures are classified as invalid tool output, and accepted
post-execute value replacements regenerate the model-facing projection.
Typed render-intent cards, durable presentation metadata and migration of all
built-in tools remain open, so the capability-equivalence claim stays blocked.

## Latest implementation delta (2026-08-29, background-job correlation)

Background jobs now preserve runtime correlation through registration, the
independent execution context and terminal snapshot. The application records
and closes one bounded `job.<kind>` span per job, while the command, PowerShell,
subagent, schedule and terminal producers forward the initiating identity.
This improves T19's async coverage but does not close external OTel parity,
cross-process job persistence/crash recovery or the remaining equivalence gates.

## Latest implementation delta: Team member transition transaction (2026-08-29)

SQLite Team provisioning now rejects non-provisioning initial member events and
offers a serialized active/failed transition with child-lineage, root-sequence,
prior-state and idempotent-replay checks. Live Agent publication and the full
cross-process membership/receipt/authorization matrix remain open.

## Latest implementation delta: storage crash-boundary evidence (2026-08-29)

JSONL append and interrupted-tail repair now use the same rollback-safe
physical append boundary. Child-process tests cover torn-tail recovery,
cross-process lock ownership and deadline cancellation; a SQLite child-process
test covers rollback of an uncommitted transaction while preserving committed
data and integrity. T4/T17 remain partial because arbitrary kill-point fault
injection, filesystem faults, backup restore verification and the full
corruption matrix are still not proven.

## Latest implementation delta: tool partial-failure preservation (2026-08-29)

The registry now preserves validated rich content, metadata and deferred
context messages when a structured tool or execution hook returns a partial
result together with an error. The failed result withholds the canonical value
and retains stable error classification, preventing context loss on the next
model step. T8 remains partial pending full producer migration and fixtures.

## Latest implementation delta: MCP Streamable HTTP lifecycle (2026-08-29)

The Go MCP seam now supports per-server stdio or Streamable HTTP selection.
HTTP performs initialize/initialized, carries `Mcp-Session-Id`, handles
paginated `tools/list`, `tools/call`, JSON/SSE responses, list-changed
notifications and best-effort session DELETE on close. REPL dynamic selectors,
static bridge tools and ACP MCP services share the selector. Reconnect starts a
fresh HTTP protocol session and does not replay a failed tool call
automatically. Task-required tools remain intentionally fail-closed because
the current reference bridge rejects them; SDK replay, callback response,
credential rotation and cross-process MCP ownership remain open.

## Latest implementation delta: bounded shell output and terminal process ownership (2026-08-29)

`run_code`, foreground `bash`, and `job_start` now continuously drain
subprocess output into bounded captures; background bash jobs use bounded
pollable files. Persistent
terminal sessions now own a Unix process group or Windows Job Object during
close. These are verified local hardening deltas, not full hostile-sandbox or
cross-process lifecycle equivalence; the strict race/leak gate remains blocked
until an environment with a C compiler is available.

## Latest implementation delta: Agent disposal and MCP error admission (2026-08-29)

Agent-owned jobs now have an owner admission barrier: disposal cancels and
waits for that owner's jobs/observers before removing them. ACP, Team and
native Agent scopes also dispose persistent model terminals and session
approval policy; terminal lookup after disposal is closed and its stop event is
durable. Session title generation uses the addressed session's provider/model,
and Web search logging fails closed instead of borrowing another Agent's log.
MCP `isError` results are structured `MCP_TOOL_ERROR` results across dynamic,
bridged and ACP surfaces and cannot persist image attachments.

These are in-process correctness fixes. Provider-generation lease barriers,
Team cross-session atomic receipt, hostile-code sandbox enforcement, complete
ACP/MCP/SDK lifecycle coverage and the strict race gate remain open.

## Latest implementation delta: workflow lifecycle boundary (2026-08-29)

`workflow/start` and `workflow/end` are now host-owned, including Node launch
failure and worker-exit paths. Host callback drain precedes terminal end
publication, and the host tracks workflow agent starts to synthesize exactly
one cancelled `workflow/agent-end` for each unpaired start on cancellation or
exit. Full worker-death admission, pending-start receipts and cross-process
replay remain open.

## Latest implementation delta: workflow worker admission (2026-08-29)

The Node workflow runner now counts `agent()` calls before queueing, so
concurrent `parallel()` calls cannot bypass `maxTotalAgents`. Workflow result
or cancellation closes host callback admission, drains callbacks for a bounded
grace period, rejects late JSONL writes, and the application adapter cancels a
child run when its Result wait is aborted.

Reference worker-death/grace agent-end synthesis, child disposal receipts,
pending-start replay, and hostile protocol replay remain open.

## Latest implementation delta: Agent provider route pinning (2026-08-29)

An Agent turn now retains the provider instance captured at turn assembly;
same-route request middleware no longer follows a concurrent Web-published
registry generation. Safe retirement of the old generation still requires a
ref-counted usage lease and close barrier, so credential-lifetime equivalence
remains open.

The Code Runtime host also records admitted child call IDs and ignores
duplicates, closing a replayed stdout frame's duplicate-side-effect path.
Node TypeScript strip/parser failures are likewise projected as program
exceptions when the wrapper cannot reach its catch block.

## Latest implementation delta: Code Mode value and output boundary (2026-08-29)

The TypeScript runner now matches the reference completion convention for
undefined (successful, no value), encodes host nil as JSON null, and turns
lossy host binding results into catchable typed binding failures. Output-limit
diagnostics reserve their serialized bytes and trim retained logs to stay
within the configured envelope. These are local protocol corrections; worker
resource enforcement, hostile-peer replay and the full nested-dispatch
lifecycle remain blockers.

## Latest implementation delta: approval disposal (2026-08-29)

Approval engines now expose an optional session cancellation seam. Agent
disposal cancels only that session's pending requests; private ACP approval
engines mark pending requests unavailable before closing. Shared application
cleanup projects the cancellation as a canonical decision event. Cross-process
approval/event atomicity and the complete answerer replay matrix remain open.

## Latest implementation delta: provider-generation lifetime (2026-08-29)

Provider registry publication now tracks generation references. Retired
generations close only after Agent turn leases and stream leases settle;
long-lived consumers resolve the current route at operation start. The
application now also resolves credentials through a context-aware per-operation
seam in every production LLM adapter. Cross-process credential leases, complete
rotation behavior and the full Web authorization matrix remain required for
full equivalence; provider-catalog and MCP projection redaction are covered by
the later Web projection delta.

## Latest implementation delta: MCP/Web secret projection (2026-08-29)

MCP server headers are now redacted in Web inventory and refresh responses;
header names remain visible but values never cross the projection boundary.
MCP command arguments, URL credential components, provider catalog values and
refresh diagnostics are now covered by the projection boundary; the full Web mutation
authorization matrix remains an open equivalence item.

## Latest implementation delta: per-operation credential resolution (2026-08-29)

Every production LLM adapter accepts a context-aware credential provider, and
`Available`/`Stream` snapshot the credential at operation start. Built-in and
custom routes use the locked settings/environment resolver, so rotation does
not require rebuilding long-lived consumers. Cross-process credential leases,
full Web authorization and cross-process lifecycle tests remain required for
full equivalence; provider-catalog and MCP projection redaction are covered by
the later Web projection delta.

## Latest implementation delta: runtime-context overlap and MCP projection (2026-08-29)

Agent-owned approval, compaction, plan and subagent projections now remain on
their runtime log even if an Agent id overlaps the native compatibility
`currentID`. MCP Web inventory and refresh diagnostics mask credential-shaped
arguments, URL userinfo/query values and configured headers; masked values are
restored from the stored configuration during updates. Web per-user/session
authorization, cross-process lifecycle semantics, enforcing sandbox parity and
the strict race gate remain open.

## Latest implementation delta: fail-closed runtime overlap (2026-08-29)

Runtime-context log fallback and native goal mutation now refuse to borrow the
legacy `currentID/a.log` whenever an Agent Registry is active, including when
the Agent id equals the legacy selection. This closes a concrete bootstrap
cross-session leakage path. Full legacy-state migration and the remaining
sandbox, cross-process lifecycle and strict race gates remain open.

Static MCP bridge calls also now emit `mcp/call` through the addressed Agent
runtime event sink; only legacy callers use the compatibility log callback.

## Latest implementation correction: fork boundary and durable seed (2026-08-29)

Fork now resolves the addressed parent runtime and copies only its completed
turn prefix through the last `turn/end`; an Agent parent is no longer silently
treated as a fresh child. SQLite child creation persists lineage/header and the
fork seed through `CreateSessionWithEvents`, while resume derives results after
the existing transcript watermark. This closes a concrete fork/restart gap,
but full nested-worker publication, Team receipts, protocol lifecycle, sandbox,
telemetry and final fault/security/performance/race gates remain open.

## Latest implementation correction: child publication rollback (2026-08-29)

Spawn/fork child logs are now published to the Agent runtime index only after
seed restoration, durable header/seed creation, and the first lifecycle event
all succeed. Failed initialization cannot leave a resolvable ghost child. A
cold parent can also supply its durable raw transcript as the fork source;
remaining cross-process nested-worker, Team, protocol, sandbox and final gate
items are still open.

## Latest implementation correction: child id cold start (2026-08-29)

Spawn/fork providers inspect durable session metadata before allocating their
next child id, preventing a provider recreated after restart from reusing an
existing `spawn-N` or `fork-N`. Backend-level atomic reservation for a truly
concurrent second process remains part of the storage gate.

## Latest implementation correction: Agent memo disposal (2026-08-29)

The application now removes a `sessionAgents` memo when the corresponding
Agent scope is closed. A later Web/Native request can rematerialize a live
handle instead of retrieving a closed Agent. Cross-process publication and
disposal receipt replay remain open.

## Latest gate evidence (2026-08-29)

The release gate passed diff check, full Go tests, vet/build, Web tests/build/
manifest verification, and Linux/Windows cross-build. It stopped at the strict
race stage because the Windows host has no `gcc` for `CGO_ENABLED=1`; this is
an unverified gate, not a pass.

## Latest implementation correction: child start cancellation (2026-08-29)

Spawn/Fork durable header/seed creation now uses the synchronous caller
context, so cancellation during initialization stops before child runtime
publication. Legacy fallback stores and cross-process lifecycle atomicity remain
open.

## Latest implementation correction (2026-08-31, live projection cursor)

The shared projection now has a validated incremental `Cursor` for live event
delivery, with detached snapshots and a per-event live-vs-cold equivalence
fixture. This closes the missing live cursor seam, while native rich state and
ACP/SDK/query/trajectory migration remain open under A8.1.

## Latest verification correction (2026-08-31, reference replay environment)

The pinned reference replay was retried with an isolated Go cache and remains
unverified because Node 24.19.0 fails before importing the reference source:
`uv_os_get_passwd returned ENOMEM`. The same failure occurs in a standalone
Node `os.userInfo()` call, so this is recorded as an environment/toolchain
blocker rather than a passing replay result.

The checked-in reference replay also passed with
`DSH_REFERENCE_ROOT=D:\\dev-projects\\Agent\\deepseek-harness`; this covers the
core fixture only, not the full cross-subsystem contract matrix.

## Latest implementation correction: Web Agent command registry isolation (2026-08-29)

Agent-backed Web `/goal` now executes through a cloned, session-owned registry
with the addressed session's owner, mode and permission policy. It no longer
uses the process-global registry directly, which could otherwise bypass the
Agent runtime policy even while writing the addressed session log. A regression
sets the global registry to deny the tool and proves the scoped Web command
still follows the session policy. The broader Web inventory/mutation
authorization and full command parity matrix remain open.

The application also discards a closed stale `sessionAgents` memo before
materializing a replacement handle. This complements scope cleanup and covers
legacy/racing disposal paths; it is not cross-process publication or receipt
replay.

## Latest implementation correction: usage-anchor provenance (2026-08-29)

Final assistant/message events now carry the exact assistant chunk sequence
numbers that produced the provider response, including an explicit empty array
for a known empty stream. Replay-aware metering reconstructs the provider
assistant content from those cited chunks (including reasoning), anchors before
the closing surface node, and treats nil/empty request arrays equivalently when
matching a durable request header. This closes a concrete usage-anchor rewrite
gap; full request-capacity/provider-usage/compaction/Web/ACP coverage and the
remaining release gates are still open.

Compaction replacement markers now also carry the complete shadowed surface
sequence list, allowing provenance-aware replay to validate the replacement
instead of relying only on its numeric range. Legacy manually constructed
replacement payloads remain readable for compatibility.

Session-scoped approval resolution now uses the provider's scoped listing seam
before loading the candidate request, reducing cross-session prompt disclosure
for durable providers; compatibility providers still use the filtered fallback.
The full cross-process answerer/replay matrix remains open.

Session append, atomic append, persisted adoption and cold restore now reject
present-but-invalid provenance (duplicate/future/missing/wrong-kind sources and
replacement ranges missing cited surface nodes). Omitted provenance remains a
readable legacy form.

## Latest implementation correction: command lifecycle contract (2026-08-29)

`command/run` and `command/done` are now validated on live append, atomic
append, persisted adoption, restore and explicit lifecycle replay. IDs cannot
repeat, every done must match an earlier run, result kind is limited to
`success|error`, and `sourceEventSeq` is allowed only on success and must point
to an earlier existing non-command event. The optional source field preserves
the distinction between omitted and explicit zero. Open runs remain valid for
crash recovery. Full cross-surface command, protocol and authorization
coverage remains open.

The native `commands/execute` adapter now returns the command ID, settled kind,
source sequence and UI result from the exact Web durable lifecycle it invoked;
it no longer fabricates a second ID or drops the result payload. This closes
one adapter projection mismatch, not the complete command matrix.

The native command catalog now excludes model-turn skills and advertises image
support only for `/plan`; Web rejects image-bearing invocations of commands
that do not declare image input before any model or durable history mutation.

Native `commands/execute` now returns the committed Web command ID, kind, text
and optional source sequence instead of fabricating a separate ID; the adapter
also filters skill model turns out of the command registry.

Successful Web command completions now cite the durable `web/command-result`
event from `command/done.sourceEventSeq`; error completions intentionally omit
that success-only provenance. This makes the command result and lifecycle
projection replay-linkable rather than merely ordered.

Application-owned Web queue list/enqueue/edit callbacks now require the
addressed durable session before reading or mutating the in-memory queue. A
guessed session id can no longer create work that later fails during dispatch;
generic transport callbacks remain injectable for isolated tests.

Code Mode now bounds Node stderr capture and treats an oversized raw stdout
protocol frame as a cancelled `worker-exit` outcome after draining the pipe;
this prevents a hostile program from deadlocking shutdown through an OS pipe.

ACP prompt admission now reserves the session prompt slot and cancellation
context before decoding or persisting rich content. Cancellation during image
admission therefore settles as `cancelled` and cannot enqueue a late user
message, matching the reference admission-to-followup boundary.

Team delivery retry now treats durable `agent/inbox/spliced` insertions with
the matching `team_id`/`team_message_id` as target receipts, including entries
whose messages have already been claimed. A retry after a missing root
acknowledgement consequently completes only the missing delivery edge instead
of injecting a duplicate message. Cross-process storage races and full
receipt transaction coverage remain open.

The native `session.prompt` adapter now validates and persists rich image input
through the attachment store's whole-batch admission API. It rejects an
invalid later image before writing an earlier image, and requires the addressed
session to exist before attachment persistence. This closes an adapter-level
orphan/unknown-session boundary; capability routing, multimodal policy and the
complete cross-surface authorization matrix remain open.

The composition root now supplies native `session.prompt` with the addressed
session's provider/model image capability. Native image admission fails before
attachment writes when that route is unavailable, matching the ACP capability
gate. Full route catalog parity and Web/native authorization remain open.

Team's subagent directory now discovers durable Team roots from persisted
session facts in addition to the process-local board cache. After restart,
`subagent/list`/direct control can therefore rematerialize a board before any
Web/native request has touched it. Cross-process membership authorization and
atomic receipt transactions remain open.

Approval resolution now distinguishes an unknown local correlation from a
known wrong-session correlation. For a durable provider, a request created by
another process can be looked up by its session-scoped provider listing; a
known owner mismatch still fails closed. Native question cancellation also
uses a durable owner resolver when reconnecting without a `sessionId`.
Cross-process approval/event atomicity and the complete answerer replay matrix
remain open.

MCP reconnect configuration now fails closed for explicitly invalid delay and
attempt values before omission defaults are applied. This preserves policy
identity across restart/replay; it does not close the remaining per-server
timeout/startup contract, generation disposal barrier, task lifecycle, or
external SDK replay gaps.

MCP stdio startup now scrubs credential-shaped ambient environment variables
before adding explicit caller-provided entries, matching the reference's
external-process boundary. This is a security correction only; per-server
env/cwd and timeout policy, generation disposal barriers, task lifecycle, and
SDK replay gaps remain open.

Code Mode now preserves a worker's concrete syntax/exit classification when
the OS CPU handle disappears as the process is already exiting; CPU-accounting
loss still cancels active execution. This removes a suite-load flake but does
not close the hostile worker resource/process matrix.

The local shell provider also ignores the platform-specific `StdoutPipe`
"file already closed" result after a normal process reap; genuine pipe faults
remain errors, so completed explicit full-access commands are not misreported
as transport failures.

Web MCP status now uses a name-keyed live-client index, so optional startup
failures cannot misreport the connection state of later servers. This closes a
projection correctness bug only; MCP process policy, generation disposal,
tasks, and SDK replay remain open.

MCP startup now supports the reference-style optional-server policy: an
initial connect/list failure is recoverable by default, while
`fail_on_startup_error: true` makes it fatal. The environment boundary is also
credential-scrubbed. Per-server env/cwd and timeout configuration, fresh
generation disposal, task lifecycle, and SDK replay are still open.

MCP runtime construction now carries per-server `env`, `cwd` and
`tool_call_timeout_ms` through stdio, HTTP, ACP and long-lived bridge paths;
explicit environment injection remains after credential-shaped ambient
scrubbing, and call timeout is separate from startup/discovery timeout. Web
settings projection/redaction, task lifecycle, generation close barriers and
complete SDK replay are still not equivalent.

Programmatic MCP client construction also applies the server name/transport
validation before factory dispatch; invalid custom-factory inputs now fail
closed consistently with application startup.

The release gate then passed all non-race stages; strict race detection remains
unverified because this Windows host has no `gcc`/C compiler.

The fresh-generation lifecycle assertion is synchronized under the test-double
lock; the actual race gate remains unverified on this host because no C
compiler is installed.
## Latest implementation correction: MCP timeout configuration validation (2026-08-29)

The YAML loader now rejects explicit MCP per-server `tool_call_timeout_ms`
values that are zero, negative, non-integer, or out of range; omitted values
retain the reference default. This is a fail-closed configuration boundary,
not closure of the remaining MCP lifecycle, SDK replay, or release gates.

Reconnect supervisors also cancel their private context when closing, so a
factory generation blocked in `Start` can quiesce instead of keeping process
shutdown waiting forever. The bounded close barrier for clients that ignore
context, MCP tasks, and external SDK replay remain open.

The independent Go `--sdk` runtime now has durable prompt receipts and
process-wide session-event delivery, but it remains a partial protocol bridge;
the external client/replay contract, cancellation/timeout semantics and full
SDK lifecycle are not closed. Agent-owned Team and shell runtime owners now
fail closed instead of falling back to the legacy `currentID` when identity is
missing.

## Latest implementation correction (2026-08-30, atomic publication and diagnostics)

- SQLite append validation rejects gaps, duplicate positions and stale writer
  jumps; exact-byte replay remains available for recovery.
- Native fork atomically publishes the closed seed, lineage, inherited metadata
  and runtime configuration. Native, ACP, Team and subagent creation use the
  atomic session seam on SQLite; `subagent/start` is committed with the child
  without changing the inherited `SeedLength` boundary.
- MCP startup/reconnect diagnostics redact configured URL, argv, headers and
  environment secrets before console output.
- Final local verification passed: `go test ./... -count=1`, `go vet ./...`,
  `go build ./...`, Web tests/build/manifest verification, and the core
  reference replay fixture. This is not a full equivalence result: the manifest
  intentionally remains `fail` / `claimAllowed: false` pending the remaining
  sandbox, cross-process, protocol, authorization, telemetry and race/fault
  gates.

## Latest implementation correction (2026-08-30, durable identity reservation)

- SQLite schema version 2 adds a durable `(namespace, id)` reservation table;
  duplicate claims are rejected under the existing cross-process SQLite lock
  and survive process restart.
- Session/fork, subagent child, Team task/message and application-wired Local
  job generation now claim IDs before publication with bounded retries.
- This closes only the generated-ID seam. Team member identity reconciliation,
  domain receipt atomicity and full cross-process Agent/Team/job/terminal
  recovery remain open; the manifest intentionally remains `fail` /
  `claimAllowed: false`.

## 2026-08-30 executable audit register

The detailed execution backlog is now available in
`docs/equivalence-task-register.yaml`. It is the machine-readable authority
referenced by `docs/equivalence-manifest.yaml`; each A0-A9 item carries an
implementation seam, acceptance oracle, reproducible command, evidence link,
dependencies, and release-blocker flag. The release gate checks the register
and refuses a capability-equivalence claim if any required item is not `done`.

## Latest implementation correction (2026-08-30, Team identity and credential refresh)

Team member provisioning now reserves `team-member:<team-id>` before child
session/member publication and Agent Registry creation; direct Roster calls use
the same durable seam. The credential Vault refreshes a reference from the
backend at each acquire boundary, serializes local generation writes, and the
production streaming adapters release leases at terminal/error/body-close.
These reduce, but do not close, A5.4/A6: reservation receipts and recovery,
OS-protected storage, abandoned-stream disposal, and the full hostile/fault
matrix remain open. The manifest remains `fail` / `claimAllowed: false`.

## Latest implementation correction (2026-08-30, Team reservation receipt transaction)

SQLite now commits the Team member identity reservation, child Session, fork
seed and Lead provisioning receipt in one transaction. Production provisioning
uses the atomic path without a separate pre-claim; regression coverage rejects
duplicate identities and proves failed publication rolls back the reservation.
Team, store and `cmd/pa` tests pass. A2.2/A5.4 remain partial because other
domain receipts, orphan-reservation recovery/GC and the full crash matrix are
still unproven.

## Latest implementation correction (2026-08-30, terminal claim cold recovery)

Cold Agent materialization now closes stale durable `terminal/start` claims
with exactly one `terminal/stop reason=process_restart`, while live terminals
in the current process are preserved. Agent-scope disposal now removes the
record after a successful stop receipt and regression coverage rejects
duplicate stops. A2.4/A5.3 remain partial: old PTY kill-tree/recovery by a
stable process identity, cross-host cleanup, and crash/fault injection are not
yet equivalent.

Workflow lifecycle events now use an addressed, failure-reporting sink for
runtime and legacy calls; a missing `workflow/*` receipt returns a tool error
rather than a false success. A3.4 remains open because external worker restart
replay and the full death/fault matrix remain unproven.

The first failed Workflow receipt now also cancels that run's admission
context, so receipt loss cannot be followed by another external `agent()` call.
This hardens pending-start admission; automatic restart replay idempotency and
the full worker death matrix remain open under A3.4.

Worker-to-host JSONL is now fail-closed: malformed or unknown frames terminate
the run with one failed `workflow/end` instead of being ignored. Protocol and
lifecycle regressions pass, while A3.4 remains open for restart intent
idempotency and the complete crash/fault matrix.

An audited candidate that permanently reserved owner/`meta.name` workflow runs
was removed: the pinned reference uses per-run opaque IDs and permits multiple
runs; name is record metadata, not a cross-run uniqueness key. A3.4 remains
open pending a reference-compatible durable intent/replay design.

The Node worker now records a workflow member only after its child Session ID
is published, and emits paired start/end facts. Unpublished calls emit neither
record. A3.4 still lacks reference-compatible restart intent/reconciliation and
the full fault matrix.

The parent session now also projects the four reference
`tool-workflow/*` durable records, with bounded native Web fields and
regressions for child identity, run closure, and recorder-failure isolation.
A3.4 remains open for durable restart intent/reconciliation and the full
worker death/fault matrix.

Those records are now validated on append and replay with the reference fold
rules. Crash-open workflow prefixes remain readable, but malformed duplicate or
unpaired records fail closed. A3.4 still requires external-action
reconciliation and the full worker death/fault matrix.

Provider runtime snapshots and routed adapters now reject registered but
unavailable routes with `llm.ErrProviderUnavailable` before stream dispatch or
generation lease acquisition. A6.3 is partial, not equivalent: one catalog
still does not own all model capacity and modality defaults across transports.

The new model catalog route seam now carries explicit reasoning/tool/vision/
audio declarations and capacity, and CLI/native, Web, ACP and SDK loop assembly
consume the same lookup for context/output limits. A6.3 still needs complete
built-in model metadata and stable unavailable-model negatives everywhere.

Built-in candidate IDs, DeepSeek capacity and reasoning now come from one
catalog, and native session model selection uses the same provider route
admission as turn assembly. A6.3 remains partial until complete model metadata
and model-level unsupported negatives cover all transports.

Explicit tool and reasoning catalog declarations now constrain the model
surface and effort route. Undeclared free-form routes retain pass-through
behavior; therefore complete first-party metadata is still required before
A6.3 can close.
## 2026-08-31 external crash-boundary audit

- A2.2 and A2.4 remain partial. Job completion has a durable terminal event and
  a cold receipt-replay path, but the bounded wake budget, journal-failure retry,
  and provider-generation cold reconstruction still require explicit negative
  evidence.
- MCP reconnect has protocol lifecycle tests, but not yet a real child-process
  kill matrix. `mcp/list` and `mcp/call` must not be mistaken for durable server
  lifecycle receipts.
- A new A2.5 release blocker requires an explicit crash-boundary classification
  for every external side effect. This prevents isolated normal-path receipts
  from being reported as cross-process recovery equivalence.
- Focused `go vet`, `go build ./...`, and `git diff --check` pass. The manifest
  remains `status: fail` and `claimAllowed: false`.

## 2026-08-31 real MCP and job receipt delta

- MCP now has real child-process evidence for crash during `tools/call`: one
  replacement generation is created, the failed call is not replayed, and the
  request journal shows no cross-generation duplicate. Start, list, list-changed
  and HTTP kill points remain open.
- Job completion recovery now shares the wake-budget delivery boundary with
  live settlement. This closes a memoized-Agent bypass that could turn every
  recovered receipt into a wake after the configured budget was exhausted.
- Added executable evidence for journal-failure retry and budget restore.
  Focused package tests, vet, build and diff checks pass; A2.2/A2.4 remain
  partial and the equivalence claim remains disallowed.

## 2026-08-31 MCP initial-failure alignment

- MCP initial failures now use the reference supervisor retry budget rather
  than stopping after one failed `Start`. Optional servers retain the supervised
  client for shutdown and publish their first tool generation only after a
  complete replacement discovery; strict `fail_on_startup_error` deployments
  remain fail-closed.
- Real stdio child kill coverage now includes `initialize`, `tools/list` and
  `tools/call`. Cross-process journals prove replacement-generation ownership
  and no failed-call replay.
- Focused MCP/package tests, vet, build and diff checks pass. A4.5 remains open
  for list-changed, HTTP generation retirement and the remaining external
  lifecycle matrix; the overall equivalence claim remains forbidden.

## 2026-08-31 MCP list-changed and HTTP retirement delta

- MCP list-changed is now generation-aware. During reconnect/backoff it cannot
  resync the old tool set; after retirement it cannot resurrect stale schemas;
  the current replacement generation still refreshes normally.
- HTTP generation replacement now has explicit session-retirement evidence: the
  old `Mcp-Session-Id` is DELETEd, the replacement uses a new session, and
  subsequent discovery never touches the old session.
- `Close` now has a regression proving in-flight `ListTools` quiesces before
  generation retirement.
- Focused package tests, vet, build and diff checks pass. A4.5 remains partial
  in scope because external task/auth and broader tool lifecycle matrices remain
  open; `claimAllowed` remains false.

## 2026-08-31 provider cold restart delta

- Provider generation recovery now has cross-process evidence, not merely a
  same-process registry replacement test. A child process rebuilds a durable
  custom route from SQLite settings and the dedicated credential backend, then
  streams with the reconstructed model, base URL and secret.
- The parent process proves the pre-restart generation drains, retires and
  closes exactly once before the child starts. A new child PID confirms this is
  an actual process boundary.
- Focused provider, credential, store and composition tests pass with vet,
  build and diff checks. A2.4/A6.2 remain partial for the remaining
  persisted-materialization and provider-policy/hostile-secret matrices.

## 2026-08-31 A2.4 cold materialization delta

- Job owner cold materialization now has persisted-store evidence across two app
  instances and a third independent SQLite reader. Exactly one terminal job
  receipt and one owner inbox receipt survive restart.
- A2.4 is now done in the task register: owner claims are rebuilt from durable
  receipts and the acceptance command passes across all internal packages plus
  `cmd/pa`.
- A2 remains blocked by A2.2/A2.5 external-crash and transaction work, so the
  overall equivalence claim remains disallowed.

## 2026-08-31 A6.2 credential revocation delta

- A6.2 is now done. Every provider protocol family has a revocation regression
  proving lease ownership across an in-flight stream, stable fail-closed rejection
  of later streams, and release on terminal drain without secret disclosure.
- The provider cold-restart, credential backend failure, rotation, wipe and
  retired-generation tests remain part of its replayable evidence.
- A6.1/A6.4 may still retain secret-store and non-disclosure matrix gaps; A6.2 no
  longer blocks the credential-lease objective.

## 2026-08-31 A6.1 Web credential delta

- Closed the Web provider-key bypass: production built-in/custom provider edits
  now use the dedicated credential backend for save, clear, delete and rollback.
  Generic settings contain provider/profile metadata but no credential values.
- Added executable evidence for dedicated-record creation, rollback, clearing,
  custom-provider deletion and absence of `llm.key.*` from generic settings.
- A6.1 is now done. A6.4 remains open for the broader hostile-secret/
  telemetry/cancellation matrix; the overall equivalence claim remains false.

## 2026-08-31 A1.5 production retry delta

- Production `sessionAgent` now has executable evidence for retry after
  transient log-load and inbox-sink failures. Stable dedupe keys ensure one
  owner receipt per durable terminal event.
- The fault test exposed and closed a real stale-journal defect: memoized
  Agents now resolve their inbox journal through the current runtime log, so a
  reloaded log cannot write against a stale durable sequence.
- A1.5 remains partial only for its dependency and release-boundary work with
  A2.2/A5.1; the manifest remains fail / `claimAllowed: false`.

## 2026-08-31 A5.1 ownership closure

- A5.1 is now done. The subagent control plane has a consolidated ownership
  matrix proving cross-lineage status/resume/send/followup/interrupt/cancel/list
  rejection, caller-scoped discovery/wait isolation, and no foreign callback
  execution.
- Cold restore and addressed-runtime tests remain part of the replayable
  evidence. A5.2 and A5.4 still cover broader Team and identity reservation
  matrices; the overall equivalence claim remains false.

## 2026-08-31 A5.3 job/terminal closure

- A5.3 is now done. Jobs and persistent terminals have executable evidence for
  owner admission close, late-work rejection, owner-scoped teardown, kill-tree,
  first-wins terminal settlement, cold restart reconciliation and retryable
  owner wake receipts.
- The A5.3 acceptance command passes across `internal/jobs`,
  `internal/terminal` and `cmd/pa`. A5 remains partial only for Team lifecycle
  and identity reservation transaction evidence.

## 2026-08-31 A5.2/A5.4 Team closure

- A5.4 is now done. Durable reservations survive restart and namespace scope,
  abandoned job/task/message tokens are skipped without identity reuse, Team
  member atomic publication rolls back on receipt failure, and failed member
  provisioning retains its identity.
- A5.2 is now done. Team roster/task/mailbox state has executable evidence for
  append-only replay, CAS revisions, blocker/cycle rules, authorization,
  member lifecycle/rebind, cold snapshots, duplicate-delivery rejection and
  atomic inbox/delivery receipts.
- All A5 ownership items are now closed. Higher-priority A2.2/A2.5 and final
  protocol/security/race gates still forbid an equivalence claim.

## 2026-08-31 schedule crash-replay delta

- Durable schedules now have production crash-window evidence for the
  owner-inbox, schedule/fire and scheduler-dispatch recoverable protocol. The
  new regression proves one occurrence produces one owner receipt and one fire
  fact across replay.
- Fixed two defects found by that test: claimed schedule receipts no longer
  cause duplicate owner wakes, and terminal owner close ignores unrelated
  audit-only session events.
- Focused schedule/terminal/composition tests, vet, build and diff checks pass.
  A2.2 remains partial only for external protocol auth/task classification.

## 2026-08-31 A2.2 closure delta

- A2.2 is now done. MCP auth and required-task boundaries have executable
  evidence: unauthorized calls do not auto-replay and preserve the session;
  required-task metadata is rejected before a real HTTP side-effect request.
- Together with approval, Team, job, Agent publication, credential, terminal,
  schedule and workflow/code crash evidence, every A2.2 acceptance item has a
  committed-or-retryable outcome. A2.5 remains open for the complete
  external-boundary classification matrix.

## 2026-08-31 A2.3 corruption closure

- A2.3 is now done. SQLite and JSONL share executable corruption/recovery
  evidence for interrupted tails, torn/partial records, sequence conflicts,
  atomic transactions, missing/invalid seeds, process-lock recovery, backup,
  integrity repair and non-mutating bounded reads.
- The expanded acceptance command passes across `internal/store`,
  `internal/persistence` and session contract fixtures. A2.5 remains the P0
  external-boundary classification blocker.

## 2026-08-31 A2.1 storage lifecycle closure

- A2.1 is now done. SQLite and JSONL evidence covers cross-process writer
  locking and recovery, contiguous append ordering, durable reservation reuse
  prevention, schema migration/downgrade rejection, backup, integrity repair
  and bounded reader/writer ordering.
- New SQLite regressions prove a real v1 database migrates without data loss
  and independently opened handles maintain one conflict-free event namespace.

## 2026-08-31 A1.5 publication closure

- A1.5 is now done. Durable publication is represented by the session
  header/domain receipt, while live Agent handle creation and disposal remain
  process-local runtime state, matching the pinned reference model.
- Negative evidence covers failed Start rollback, ghost-memo prevention, owned
  job cleanup, cascade disposal, stale memo replacement, concurrent addressed
  Agents, provider-generation retirement and replayable receipt retry.

## 2026-08-31 A1.2 worker-death closure

- A1.2 is now done. One canonical replay fixture covers provider retry facts,
  compaction facts, worker death after tool dispatch, linked
  `TOOL_OUTCOME_UNKNOWN` recovery, deterministic step/turn closure, wire replay,
  idempotent recovery and model-visible derived history.
- Existing loop waterfall evidence plus JSONL/SQLite interrupted-tail repair
  regressions complete the cross-backend recovery boundary. Overall equivalence
  remains `fail` / `claimAllowed: false`.

## 2026-08-31 A1.3 provider policy closure

- A1.3 is now done. All five wire adapter families share the structured HTTP
  failure taxonomy, Retry-After and request-id metadata, diagnostic redaction,
  provider-scoped retry policy, private-retry disablement and typed-code retry
  eligibility decision.
- Provider generation replacement, in-flight credential lease draining,
  retired-route rejection and durable retry/cancel waterfall behavior remain
  covered by A6.2 and A1.2 evidence. Overall equivalence remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A1.4 durable history closure

- A1.4 is now done. Model-visible history replays from committed events across
  cold restore, fork, compaction, steering, quiet injection and surface
  replacement; raw stream chunks, projections and process memory are not
  history authorities.
- New cross-backend write-boundary evidence rejects under-cited replacement
  cold seeds and appends for both JSONL and SQLite without mutating the durable
  prefix. Overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 A6.3 route fallback correction

- Unknown model context fallback is now owned by the model catalog seam, not
  independently by Web. ACP uses its pinned provider/model route, and SDK
  image admission uses the full provider/model catalog route.
- A6.3 remains partial: locally unverifiable built-in metadata is intentionally
  omitted rather than invented, and remote catalog negative coverage must grow
  with supported profiles. Overall equivalence remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A6.4 secret egress delta

- A6.4 now has executable hostile-fixture evidence for telemetry, spill files,
  recovered panic diagnostics, compact JSON diagnostics, MCP redaction, Web
  inventory and provider errors. Credential-lease drain now has a direct
  byte-zeroing regression.
- A6.4 remains partial only for OS-level crash dumps/core images. The
  application cannot prove that an OS-produced process image excludes secrets;
  it must disable dumps, use a protected scrubbing path, or declare that profile
  unsupported. Overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 A6.4 crash-dump closure

- A6.4 is done. The default profile now disables OS crash images before
  credential loading; Windows WER LocalDumps policy is fail-closed rather than
  silently inherited, and `external` is an explicit non-equivalent profile.
- Hostile-secret diagnostics, spill files, telemetry, recovered panic values and
  credential-lease drain have executable negative evidence. A6.3 and final
  release gates remain open, so equivalence remains `fail` /
  `claimAllowed: false`.

## 2026-08-31 A6.3 DeepSeek metadata delta

- DeepSeek V4 Flash/Pro now carry reference-verified capacity, output, reasoning,
  tools and text-only metadata from one catalog. Unknown routes do not inherit
  vision/audio from global settings, and ACP/SDK admission uses the exact route.
- A6.3 remains partial for non-DeepSeek provider metadata because the pinned
  reference source imports the upstream pi-ai installed catalog but does not
  vendor its model facts. Overall equivalence remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A1.1 runtime isolation closure

- A1.1 is done. Production startup sets strict Agent runtime admission, and
  fs/skill/MCP/job/plan/schedule/spill/workflow/subagent sinks now have tests
  proving that an addressed runtime `Emit` wins over the legacy log callback.
- The remaining process-global fields are isolated compatibility adapters for
  embedders and tests, not production turn fallbacks. The A1 manifest blocker is
  removed; overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 A0.4 manifest authority closure

- A0.4 is done. The release gate now derives open blockers from the task
  register, requires task/status/parity documents to disclose the exact IDs, and
  generates/validates a report with subject and reference commits, profiles and
  UTC verification time.
- The manifest uses precise task IDs instead of broad area labels and remains
  the sole release-status authority. Overall equivalence remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A0.5 evidence-linkage closure

- A0.5 is done. A dedicated register lint now validates status vocabulary, task
  ID uniqueness, required fields, acceptance criteria, implementation/evidence
  paths, executable test/gate evidence and replayable acceptance commands for
  every done item.
- The audit exposed and fixed stale implementation paths and non-executable
  evidence on A0.1/A2.4/A4.2/A4.3. A4.2 now has explicit MCP default-policy and
  production selector-policy regressions.

## 2026-08-31 A8.3 profile authority delta

- A8.3 is now partial rather than open. A single profile registry describes
  enforced local storage/file/session/code profiles and explicitly marks e2b,
  Python, Cordis runner and inspect unsupported. Native Web profile queries and
  Cordis RPCs now fail closed instead of returning empty successes.
- A8.3 remains blocked by A0.2 exact profile classification and A3.2 enforcing
  sandbox implementation. Overall equivalence remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A2.5 crash-boundary delta

- A2.5 is now partial rather than open. A machine-readable registry classifies
  MCP, terminal, schedule, workflow-child, subagent and plugin boundaries as
  at-most-once, durable-retryable, audited-unordered or runtime-state, and Web
  exposes those contracts for transport and release checks.
- Failed MCP/network transports retain no-replay semantics. A4.5 external
  matrices and complete plugin-owned call evidence remain before A2.5 can close.

## 2026-08-31 A4.5 lifecycle coverage delta

- A4.5 is now partial rather than open. Existing executable evidence covers MCP
  reconnect/list-changed/session retirement, terminal cancellation and ownership,
  job close/ownership, LSP cancellation, Web fetch/search cancellation and plugin
  generation reload, plus fail-closed unsupported profiles.
- An MCP timeout test was also corrected so its 300ms bound targets the hung
  request, not child startup/initialize. A4.5 remains partial for complete
  external MCP/Web/plugin fault and schema matrices.

## 2026-08-31 A7.4 Web authorization and admission closure

- A7.4 is done. A consolidated Web matrix now proves bearer rejection, cross-origin
  mutation rejection on representative REST routes, unknown interaction stability,
  foreign subagent history/prompt/interrupt rejection, credential/provider secret
  non-disclosure, malformed SSE cursor rejection and deterministic suffix replay.
- Production Web queue enqueue/update/edit now rejects after `beginShutdown()`,
  and a composition matrix proves addressed-session ownership, foreign-item and
  nonexistent-item rejection, plus post-shutdown state preservation.
- Existing native projection, reconnect-pending, lifecycle and shutdown evidence
  remains replayable. The overall equivalence claim remains
  `fail` / `claimAllowed: false`.

## 2026-08-31 A8.4 telemetry closure

- A8.4 is done. Fixed a real bounded-shutdown defect: an in-flight collector
  request previously used a detached context, so shutdown could return while the
  telemetry worker stayed blocked for the HTTP client timeout. Shutdown now
  cancels the exporter lifecycle and waits for worker exit.
- New executable evidence proves a hung collector is bounded by shutdown context,
  queue overflow never blocks durable-event observers, and OTLP records carry
  route, session, service, version and profile-local user identity. Existing
  retry, collector-failure, redaction and non-fatal exporter tests remain.
- A8.1 remains the projection canonicalization blocker. Overall equivalence
  remains `fail` / `claimAllowed: false`.

## 2026-08-31 A0.2 capability baseline closure

- A0.2 is done. `internal/profile` now owns one machine-readable inventory of all
  58 capability seams in the pinned reference composition. Every row has exactly
  one required or optional classification plus an explicit available/unsupported
  subject state, enforcement/implementation/replay evidence, or stable reason.
- Native Web now exposes `runtime.capabilities` and `runtime.capability`.
  Unsupported capabilities return stable `capability-unsupported`; unknown
  references return `capability-unknown`. Inventory agrees with the runtime
  profile registry for E2B and Cordis boundaries.
- A8.3 remains partial only for enforcing sandbox/backend composition evidence.
Overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 built-in model input facts

Owned OpenAI, Anthropic, Gemini, and DeepSeek built-in rows now explicitly
project their reference `input` modalities through native/Web/provider catalog
surfaces (`[text,image]` for the image-capable rows and `[text]` for DeepSeek
V4). This closes a metadata-only drift where `vision` was present but the
canonical modality array was omitted. A6.3 remains partial for the complete
upstream catalog, remote refresh/failure behavior, and broader metadata.

## 2026-08-31 A4.5 external lifecycle closure

- A4.5 is done. Consolidated Streamable HTTP MCP evidence now covers auth failures,
  malformed structured content, retained sessions, explicit-only retry, required
  task metadata and rich output-schema discovery. Existing real child-process and
  HTTP generation-retirement suites remain authoritative for reconnect.
- A new Web provider/client matrix covers malformed search payloads, auth errors,
  blocked redirects, unsupported content, same-origin redirects, non-2xx results
  and closed tool input/output schemas.
- Plugin reload now has transactional fault/schema evidence for manifest,
  dependency, mount, tool-publication and runtime-factory failures. The matrix
  exposed a real rollback defect: a failed reload could remove the live tool
  instead of restoring its previous definition. Failed attempts now restore the
  exact previous tool/schema/registration, while successful reloads retain
  stale-generation fencing.
- Overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 runtime capability and projection checkpoint correction

- `runtime.profiles`, `runtime.capabilities`, Code Mode registration, and actual
  execution now share host probes. Node Code Mode requires the permission model;
  controlled shell/workspace mode requires a proven bubblewrap backend. An
  explicit full-access subprocess does not make workspace isolation available.
- SQLite projection checkpoints are durable, versioned, and bound to the
  committed event revision. Native history/list reads ignore stale checkpoints;
  native mux initial replay and key committed events write back checkpoints.
- This remains partial: history still rebuilds the full event projection before
  consuming a checkpoint, and CLI/native/Web/ACP/SDK do not yet share one
  canonical projection implementation. The manifest remains `fail` /
  `claimAllowed: false`.
## 2026-08-31 plugin call crash-boundary closure

- Dynamic plugin calls now expose an optional host audit seam with a monotonic
  call id, exact plugin generation, a `started` receipt before the body, and one
  terminal outcome (`completed`, `cancelled`, `failed`, or `panicked`). The
  registry never replays an interrupted call; the observer is explicitly not a
  durable store, so deployments that need crash evidence must persist the
  receipts at the composition boundary.
- Reload tests prove old prepared tool calls remain stale and new calls carry
  the new generation. Observer panics are isolated and cannot change the call
  outcome. A2.5 is done; A3.x, A4.4, A6.3, A7.1-A7.3, A8.1 and A9.x still keep
  the overall claim fail-closed.

## 2026-08-31 prepared dispatch cancellation boundary

- Closed a concrete tool-pipeline hole without overstating A4.4: a call that
  passed `Prepare` could be cancelled while waiting for a parallel dispatch
  slot, and `ExecutePrepared` did not re-check the context before invoking the
  body. It now returns `ABORTED_BEFORE_DISPATCH` and never enters the tool.
- `internal/tools/tools_test.go:TestExecutePreparedRechecksCancellationBeforeDispatch`
  proves the body is not called after that boundary. A4.4 is therefore marked
  partial; the full per-tool rich-result, disposed-owner, and replay matrix is
  still open, as are the release blockers listed in the manifest.
- The same boundary now distinguishes a started body from a wrapper
  short-circuit: late cancellation of a successful started call returns
  `ABORTED` and retains deferred contexts, while a never-started body returns
  `ABORTED_BEFORE_DISPATCH`. Both cases are covered by the two adjacent
  cancellation tests in `internal/tools/tools_test.go`.

## 2026-08-31 cancellation failure precedence

- DSH cancellation is not a blanket replacement for a more specific failure.
  The registry now preserves typed wrapper/tool failures that settle while the
  caller is cancelling, while plain context cancellation still maps to
  `ABORTED` or `ABORTED_BEFORE_DISPATCH` according to whether the body started.
- `TestCancellationDoesNotEraseWrapperFailure` and
  `TestCancellationPreservesToolOwnedFailure` cover this precedence. A4.4
  remains partial because the complete per-tool rich-result, disposed-owner,
  and replay matrix is still outstanding.
- Process-backed cancellable tools (`run_command` and `pwsh`) now publish an
  explicit `AbortError/ABORTED` when process termination returns a platform
  exit-status wrapper, so the registry does not depend on `errors.Is` or
  diagnostic-string parsing to preserve cancellation semantics.

## 2026-08-31 pre-cancelled tool pipeline short-circuit

- `Prepare` now materializes arguments before checking cancellation, then
  returns `ABORTED_BEFORE_DISPATCH` before whitelist, schema, approval, or
  pre-hook execution. This matches DSH's rule that a pre-aborted call cannot
  trigger policy or interaction side effects.
- `TestPreCancelledCallSkipsPolicySchemaAndHooks` proves the negative path;
  lossless argument materialization errors remain the earlier boundary and are
  not hidden by cancellation.
- `Prepare` now also detaches already-decoded map/slice arguments through a
  lossless JSON snapshot. `TestPrepareSnapshotsArgumentsBeforeDispatch` proves
  a caller mutation after preparation cannot alter the dispatched arguments;
  `TestPreCancelledArgumentMaterializationFailureWins` covers the earlier
  serialization boundary.
- This is additional A4.4 evidence only. The complete per-tool rich-result,
  disposed-owner, and replay matrix remains open.
## 2026-08-31 native session-list checkpoint boundary

- Native `session.list` now treats the bounded event tail as sidebar metadata
  only. Exact-revision cached projections are reused; cache misses replay the
  complete raw session before checkpointing, and version-1 semantically
  incomplete rows are invalidated by the cache version bump.
- `TestNativeSessionListDoesNotCheckpointTailOnlyProjection` proves state
  outside the tail window survives list projection and durable checkpoint
  writeback. This strengthens A8.2 but does not close A8.1: the claimed
  CLI/native/Web/ACP/SDK surfaces still do not share one canonical projection.

## 2026-08-31 preparation-failure result notification

- Direct registry executions now emit one normalized `ResultHook` result for
  preparation failures, stale generations, and pre-dispatch cancellation;
  unknown/denied/invalid calls no longer disappear before the terminal
  observer boundary. The compatibility error return remains unchanged.
- The new matrix test also proves failure content is present and observer
  delivery is exactly once. A4.4 remains partial because the complete
  per-tool durable replay and external transport matrix is still open.

## 2026-08-31 canonical session title event

- Native `session.rename` previously updated only the SQLite sidebar title and
  always returned `seq: 0`; an empty normalized title was also reported as a
  successful clear. DSH instead rejects that input with `title-invalid`,
  appends a `session/title` event, and returns its durable event sequence.
- Production Web wiring now routes native rename through the addressed
  session log. SQLite projects the event's title/source in the same append
  transaction, so list metadata, native history, mux projection, and cold
  restart see one title authority. Automatic fallback/provider title writes
  use the same event path, with the final provider check serialized against a
  user rename so an in-flight result cannot overwrite a pin.
- `TestNativeSessionRenameUsesCanonicalEventCallbackAndRejectsEmptyTitle`,
  `TestNativeRenameSessionAppendsCanonicalTitleEvent`, and
  `TestSQLiteSessionTitleEventProjectsMetadataAtomically` cover the production,
  transport, and persistence boundaries. This closes a concrete title inconsistency but
  A8.1 remains open because CLI/native/Web/ACP/SDK still do not share one
  complete canonical projection implementation.

## 2026-08-31 model selection image-capability boundary

- The shared model-selection validator now replays durable model-visible
  history and rejects a text-only target when image blocks remain visible.
  Native maps `ErrCapabilityUnavailable` to the stable `model-unavailable`
  response before persisting the session override.
- This closes a concrete A6.3 negative-path inconsistency, but A6.3 remains
  partial because pending inbox images and the provider-owned
  `listModels/resolveModelInfo` catalog seam are not yet implemented. Overall
  equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 Web title mutation canonical boundary

- The authenticated Web `PATCH /api/sessions/{id}/title` route previously
  bypassed the live session log and wrote SQLite title metadata directly.
  Production wiring now sends it through the same rename callback as native
  `session.rename`, returns the real `session/title` sequence, and rejects an
  empty normalized title with `title-invalid`.
- `TestSessionTitlePatchUsesCanonicalRenameCallback` proves the Web route
  appends and projects the canonical event and does not invoke the callback for
  invalid input. The storage-only route remains compatibility-only for
  embedders without a live Agent runtime; A8.1 remains open for the broader
  shared CLI/native/Web/ACP/SDK projection module.

## 2026-08-31 shared rich title projection

- Web and CLI fallback title extraction previously decoded only the legacy
  top-level `text` field. Durable DSH user messages may instead carry rich
  `content` blocks, so the two entry points could display different titles for
  the same log.
- Added `session.FirstEligibleUserText`, backed by `DeriveEventMessage`, and
  routed both consumers through it. The rich-content regression proves text
  blocks are folded consistently while image blocks remain non-title content.
  This is additional A8.1 evidence; the full cross-entry projection module is
  still not complete.

## 2026-08-31 pending inbox image model-admission boundary

- Model selection now replays the durable Agent inbox splice journal in
  addition to durable model-visible history. A pending image prompt is already
  committed model input even before the Agent claims it, so a text-only route
  is rejected with `ErrCapabilityUnavailable` before session configuration can
  change.
- `TestSessionModelSelectionRejectsTextRouteForPendingImages` covers the
  reconstructed pending queue. A6.3 remains partial only for the provider-owned
  `listModels/resolveModelInfo` catalog seam and associated first-party facts.

## 2026-08-31 A7.3 wire and SDK liveness repair

DeepSeek now emits `stream_options.include_usage`, validates thinking effort,
handles BOM/strict SSE framing, preserves all deltas in one payload, maps both
cache-hit usage spellings, and enforces a configurable five-minute idle
watchdog with distinct `TIMEOUT` versus `ABORTED` outcomes. The SDK transport
now supports no-timeout requests, reusable low-level spawn after failure, and
settled exit/stderr context on runtime transport loss. Targeted and full Go
verification pass. A7.3 is partial, not equivalent: ACP/SDK external reference
matrices, telemetry/session headers, image-bearing tool-result wire expansion,
all claimed provider catalogs, audio, and complete fault/negative oracles are
still missing.

## Latest implementation correction (2026-08-31, DeepSeek image wire semantics)

The request image budget now matches dsh's base64-length calculation and
oldest-first eviction, including the exact offload placeholder, nested blocks,
and durable-message immutability. DeepSeek keeps image-bearing tool results as
textual `tool` messages and emits their images in a following user multimodal
message, grouping consecutive tool-result images. Known provider catalog
modality negatives across all four wire adapters also override a permissive
global image setting before serialization. This repairs the concrete
image/tool-result gap; A7.3 remains
partial for ACP/SDK external matrices, telemetry/session headers, all claimed
provider catalogs, audio, and the complete fault/negative oracles.

## Latest implementation correction (2026-08-31, provider catalog forwarding)

The four claimed wire-protocol adapter families now carry detached catalog
snapshots and implement `ListModels`/`ResolveModelInfo`; retry wrapping preserves
those operations. ID-only rows are exposed as unknown facts, while explicit
false capability values remain authoritative. This removes the prior
non-DeepSeek runtime `unavailable` seam, but A6.3/A7.3 remain partial because
complete upstream first-party metadata and remote refresh/cancellation/failure
oracles are not present in the pinned checkout. Audio remains unsupported.

## Latest implementation correction (2026-08-31, sandbox capability probe)

Node Code Mode capability advertisement now performs a real permission-model
check instead of relying on `node --help`: an ungranted read of the Node
executable must return `ERR_ACCESS_DENIED` under an empty environment. Hosts
that cannot prove this are marked unsupported before `run_code` registration.
This repairs the concrete A3.2 false-advertisement path; the full
cross-platform sandbox and all external unsupported-response matrices remain
open.

## Latest implementation correction (2026-08-31, ID-only model uncertainty)

An ID-only provider catalog row no longer marks reasoning as known-false during
model selection. Reasoning is authoritative only when the selected row or an
inherited first-party entry explicitly declares it. This preserves unknown
catalog state while keeping exact false facts authoritative; A6.3 remains
partial pending complete model facts, reasoning wire support, and remote
catalog fault/refresh coverage.

## Latest implementation correction (2026-08-31, generic reasoning fail-closed)

Anthropic Messages, Gemini, and OpenAI Responses now return
`UNSUPPORTED_REASONING_EFFORT` before credentials or network I/O when a
non-empty reasoning effort is selected. This prevents the provider-neutral
`ReasoningEffort` field from being silently discarded by adapters without
protocol-specific thinking serialization. The corresponding reasoning facts
remain unadvertised until those wires are implemented; A6.3/A7.3 stay partial.

## Latest implementation correction (2026-08-31, first-party model facts)

Pinned capacity, output-limit, tool, vision, and audio facts are now present
for the currently advertised OpenAI, Anthropic, and Gemini built-in routes;
OpenAI's known non-reasoning facts are also authoritative. Anthropic and Gemini
reasoning is now serialized using the pinned protocol shapes; the remaining gap
is full effort-map/profile-budget fidelity and remote catalog fault/refresh
coverage. A6.3 remains partial.

## Latest implementation correction (2026-08-31, reasoning request wire)

Anthropic, Gemini, and OpenAI Responses now serialize non-empty reasoning
efforts using their pinned dsh wire shapes, including Anthropic/Gemini thinking
budgets and Responses encrypted-reasoning inclusion. Exact catalog
non-reasoning models reject the effort before network I/O; unknown dynamic
routes remain pass-through. Focused local protocol-server regressions pass.
A6.3/A7.3 remain partial for complete effort maps/per-model wire values and
remote catalog fault/refresh/cancellation evidence.

## Latest implementation correction (2026-08-31, durable unknown-event rejection)

The durable session log now applies a closed event vocabulary to live append,
atomic append, persisted incorporation, replay, and lifecycle validation.
Unknown event types return `ErrUnknownRequiredEvent` without advancing the
sequence or invoking the durable sink. The native wire validator retains its
separate `ignorable:true` extension rule; that rule cannot introduce an
unknown durable event. A9.2 is now partial with a cross-module negative matrix;
the remaining work is the full required-tool/provider matrix and post-rejection
durable side-effect oracles.

## Latest implementation correction (2026-08-31, startup projection isolation)

Approval restoration now consumes SQLite's raw committed event seam instead of
strictly replaying every session lifecycle during process startup. Historical
or unrelated damaged transcripts no longer block the composition root; strict
session loading and interrupted-tail recovery remain enforced when a session is
actually opened. The regression is covered by
`TestRegisterInteractsDoesNotReplayUnrelatedDamagedSession`.

## Latest implementation correction (2026-08-31, durable admission parity)

SQLite and JSONL now share the closed durable event vocabulary with the live
session log. Unknown event types are rejected before SQLite creates a session or
updates an approval transaction, and JSONL recovery does not admit a row that
would fail replay later. The native wire-level ignorable extension remains
allowed only at the wire boundary. This removes one concrete deferred-corruption
path but does not close the broader fault, race, or cross-entry gates.

## Latest implementation correction (2026-08-31, native projected-history admission)

Native `session.history` now keeps raw open-tail visibility but rejects an
unknown durable event before projection, matching the Web durable boundary.
This prevents an unknown persisted record from being downgraded to a native
`ignorable` extension. Startup approval restoration and fork inspection remain
raw by design and are not being treated as client-facing projection authority.

## Latest implementation correction (2026-08-31, Windows process-tree cancellation)

Windows command, PowerShell, and job cancellation now use a private Job Object
to terminate descendants instead of killing only the direct shell child. The
direct kill remains a fallback when process assignment is rejected. This is
cross-compiled and covered by Windows descendant cancellation tests; the full
cross-process fault/security and race gates still require platform CI.

## Latest implementation correction (2026-08-31, model input catalog)

Model-directory rows now carry explicit `input` modalities through Web,
native, and provider-owned catalog projections. Text-only model rows are no
longer widened by the global image setting; unsupported audio, duplicate
modality/model declarations, negative capacities, and invalid persisted rows
are rejected before publication. Custom providers also use the first
`models[]` row when their legacy `model` field is empty. This is a partial
A6.3 repair; reasoning-effort maps, full metadata, and remote catalog fault
coverage remain open.

## Latest implementation correction (2026-08-31, reasoning effort maps)

Provider-owned model rows can now carry the DSH canonical reasoning effort
vocabulary and per-model wire mappings. Missing levels are rejected before
network I/O; mapped values reach DeepSeek/OpenAI Responses, while Anthropic
and Gemini share the same canonical admission. Snapshots and Web/native rows
retain the map. Defaults, budget maps, full upstream metadata, and remote
catalog refresh faults remain open, so A6.3/A7.3 stay partial.

## Latest implementation correction (2026-08-31, code preset advertisement)

Native/Web preset inventory and selection now require the live `code_available`
capability emitted by the composition root. If `run_code` is not registered,
the `code` preset is not advertised and selection returns
`agent-preset-invalid`. This is a bounded A3.2 repair; the real enforcing
sandbox backend and complete ACP/SDK negative coverage remain release blockers.

## Latest implementation correction (2026-08-31, MCP server ping)

The stdio and Streamable HTTP MCP transports now answer a server-initiated
protocol `ping` with an empty JSON-RPC result. The behavior is covered by a
real helper-process handshake and an SSE `httptest`; unsupported server
requests remain fail-closed and no tool call is automatically replayed after
transport loss. A7.2 remains open for the complete external
stdio/Streamable HTTP lifecycle matrix.

## Latest implementation correction (2026-08-31, provider request attribution)

Provider HTTP requests now expose one stable shutu User-Agent. Loop requests
carry the durable runtime session ID, and compaction requests add an explicit
purpose marker. The official DeepSeek route lazily reuses the existing stable
anonymous identity and sends the user/session/compaction harness headers;
OpenAI-compatible routes do not receive DeepSeek-specific identity headers.
The local contract is covered by
`internal/llm/deepseek/deepseek_test.go:TestOfficialRequestCarriesAttributionIdentity`.

This is a bounded A7.3 repair. The equivalence claim remains blocked by the
external SDK/provider replay matrix, remote catalog fault behavior, and the
other manifest blockers; local header coverage is not an external replay.

## Latest implementation correction (2026-08-31, SDK close race)

The SDK line transport now deletes a pending request when shutdown closes the
transport while that request is waiting for the write lock. The regression
`TestLineTransportClosedBeforeWriteRemovesPending` proves the request settles
with `ClosedError` and leaves no pending entry. This is a concrete cleanup
boundary repair; the external SDK disconnect/reconnect and process-tree matrix
remains open.

## Latest implementation correction (2026-08-31, ACP disconnect cleanup)

The ACP server's client-EOF path is now covered after a real session has been
established: it cancels the owned session and closes it exactly once before
returning. This moves A7.1 from open to partial, but does not prove the full
external ACP client matrix or post-disconnect durable/side-effect replay.

## Latest implementation correction (2026-08-31, MCP terminal reconnect failure)

An MCP reconnect supervisor now emits a one-shot terminal-failure signal when
its outage budget is exhausted or the generation close barrier fails. The
composition root uses that signal to withdraw every tool owned by the failed
server, preventing stale tools from remaining callable after automatic
recovery has stopped. Local coverage is provided by
`internal/mcp/reconnect_test.go:TestReconnectingClientNotifiesExhaustionOnce`
and
`cmd/pa/mcps_test.go:TestRegisterMcpsWithdrawsToolsAfterReconnectBudgetExhaustion`.

This is a bounded A7.2 repair. Full external stdio/Streamable HTTP reconnect,
close, and side-effect replay remains open.

## Latest implementation correction (2026-08-31, provider catalog ownership)

Provider model metadata is now deep-copied at construction, listing, and exact
resolution boundaries. Input modalities, reasoning maps, and capability
pointers can no longer be mutated through a catalog result to alter later
request admission. Local coverage is provided by
`internal/llm/model_catalog_test.go:TestCopyModelInfoDetachesNestedCatalogFacts`
and
`internal/llm/deepseek/model_catalog_test.go:TestModelCatalogSnapshotDoesNotAliasConfig`.

This is a bounded A6.3 repair. Remote catalog refresh/fault behavior and the
remaining upstream model facts are still open.

## Latest implementation correction (2026-08-31, SDK subscription failure ownership)

SDK subscription close now drops queued notifications while preserving the
first terminal runtime failure. Closing a subscription after an unexpected
runtime exit therefore retains the original failure identity instead of
replacing it with a generic subscription-closed error. Local coverage is
provided by
`internal/sdkclient/subscription_test.go:TestSubscriptionCloseDropsQueueButPreservesFirstFailure`.

This is a bounded A7.3 cleanup; the full reference SDK external process matrix
and strict race gate remain open.

## Latest implementation correction (2026-08-31, SDK status ordering)

SDK server status notifications are now reserved per session before prompt and
idle transitions, then flushed only after the prerequisite durable event has
been forwarded. This closes a real external-client ordering race where later
`session.event` notifications could overtake `session.status(running)`. The
external-client regression
`cmd/pa/sdk_test.go:TestSDKServerExternalClientPromptRunsAgentThroughIdle`
passes repeatedly, including a ten-run stress check.

This is a bounded A7.3 repair. The pinned SDK/reference-provider matrix,
remote catalog faults, audio, and broader disconnect/reconnect side-effect
oracles remain open.

## Latest implementation correction (2026-08-31, SDK real-child cross-entry)

The cross-entry evidence now launches the production shutu SDK server in a
real test child process. The public SDK client drives initialization, prompt,
durable receipt, assistant events, running/idle status ordering, and shutdown
over exec/stdio; the regression passes five consecutive runs. A9.1 no longer
has a missing production-SDK-child leg, but native CLI/child-Agent execution,
reference replay, and side-effect/cleanup comparison remain open.

## Latest implementation correction (2026-08-31, canonical durable-event projection)

The new `internal/projection` package is the shared strict cold-rebuild seam
for history, session list/title, plan/todo, approvals, feedback, jobs, and MCP
activity. Web session listing and CLI title selection now read that projection;
invalid durable streams are rejected and returned nested state is detached.
Native rich projection remains a wire adapter for now, so A8.1 is partial:
the model-selection image gate now consumes the same snapshot, while
ACP/SDK/query/trajectory migration and live/cold equivalence coverage remain.

## Latest implementation correction (2026-08-31, ACP external process disconnect)

The ACP contract now crosses a real child-process boundary: after a parent
client completes `initialize` and `session/new`, EOF is sent and the child
proves cancellation precedes session close through stderr cleanup markers.
This strengthens A7.1 evidence, but the pinned reference-client and
post-disconnect durable/side-effect matrix remains outstanding.

## Latest implementation correction (2026-08-31, SDK pre-start subscription settlement)

SDK client shutdown now settles subscriptions created before process start and
subscriptions retained after a failed spawn. This prevents `Next()` from
waiting forever after the client has become terminal; the local regression
covers both cases. The pinned reference runtime and broader A7.3 matrix remain
open.

## Latest implementation correction (2026-08-31, MCP close interruption)

MCP stdio and Streamable HTTP clients now signal close before waiting for the
serialized request lock. In-flight requests settle promptly, child processes
are torn down, and HTTP session DELETE uses a separate bounded cleanup context.
The targeted close-interruption tests pass; A7.2 is now partial, while the
focused MCP/CLI/contractfixture suites and full Go suite also pass with an
isolated writable TEMP/TMP root. The complete external reconnect/auth/side-effect
matrix remains a release blocker.

## Latest implementation correction (2026-08-31, projection cache monotonicity)

Projection cache writes are now revision-monotonic: a delayed or concurrent
older writer cannot overwrite a newer checkpoint. Cache deletion, corrupted
payload fallback, durable rebuild, and concurrent writer convergence are
covered by SQLite/native-history tests. A8.2 is complete; A8.1 remains
partial.

## Latest implementation correction (2026-08-31, Web context projection)

Web context-token fallback now consumes the shared projection snapshot, so a
valid open-turn live tail follows the same model-visible history as other
projection consumers. Damaged logs still receive the documented conservative
raw-event estimate. The focused regression passes; A8.1 remains partial until
native/ACP/SDK/query/trajectory migration is complete.

## Latest implementation correction (2026-08-31, code capability fail-closed)

Web settings and config no longer present Code Mode as enabled when the
runtime was not registered: the preset is omitted, persistence is rejected,
and the capability flag reflects executable reality. Native and Web focused
tests pass; ACP/SDK negative catalog coverage and an enforcing sandbox backend
remain release blockers.

## Latest implementation correction (2026-08-31, shared query projection)

Session-query surface classification is now owned by the shared projection
package and rejects invalid durable streams before returning search results.
ACP compaction token estimation uses the same `Snapshot.History`; this removes
two independent projection paths. Native/SDK history, trajectory, and control
state still require migration, so A8.1 remains partial and the overall release
claim remains blocked.

## Latest verification correction (2026-08-31, probe contention)

Node permission capability detection now retries transient host-contention
timeouts instead of caching them as permanent unsupported. The full Go suite
passes after this change; the release verdict remains `fail` because A3.1,
A3.2/A3.3, the protocol/reference matrices, shared-projection migration, and
the A9 release gates are still incomplete or unverified.

## Latest implementation correction (2026-08-31, loop history projection)

The main loop now obtains model-visible history through the shared projection
before normal request assembly and again after context-overflow compaction
before retry. Projection failures stop the step, so the request path cannot
silently diverge from Web, ACP, or session-query on replacement and durable
event semantics. Native rich trajectory/control-state projection and the
remaining SDK migration keep A8.1 partial.

Compaction pressure detection also now rebuilds `Snapshot.History` from the
same durable projection and fails closed on projection errors before invoking
the summarizer. This removes the last core compaction-side `DeriveHistory`
shortcut; native rich trajectory/control-state migration remains open.

## Latest implementation correction (2026-08-31, non-DeepSeek catalog facts)

Built-in candidates for OpenRouter, Together, Groq, Mistral, NVIDIA, and
Hugging Face now expose the pinned pi-ai capacity, modality, and reasoning
facts available in the installed reference catalog. Focused catalog tests
pass. A6.3 remains partial because remote catalog refresh/failure behavior,
complete upstream metadata, and per-model effort/budget semantics remain open.

## Latest implementation correction (2026-08-31, meter surface projection)

The shared projection now owns replacement-aware model-visible surface entries
and their durable sequence. Token metering uses these entries for positional
nodes and usage-anchor surface pricing instead of its own replacement fold.
Projection replay also accepts the historical zero-based open-tail stream used
by native paging. Focused projection/meter/session/native-history tests pass;
native rich trajectory/control-state and SDK consumers still keep A8.1 partial.

The model-request path preserves the runtime-only attachment boundary through
`projection.BuildWithImageResolver` and `session.Log.ImageResolver`: durable
snapshots stay path-free, while the loop explicitly resolves local image paths
only immediately before provider dispatch.

## Latest implementation correction (2026-08-31, SDK session-event envelope)

SDK notifications now use `session.WireEvent`, lifting surface metadata to the
top-level DSH event envelope and preserving opaque data. The concrete SDK wire
schema mismatch is repaired, but shared rich projection consumption by SDK and
trajectory/control-state surfaces remains incomplete under A8.1/A7.3.
2026-08-31：补齐可选 Code Mode 的启动降级边界：Node 权限模型无法验证时不再退出整个 Agent，而是撤销 `run_code` 白名单、禁止 code preset runtime admission，并让 native/Web/ACP/SDK 继续提供基础能力。该修复只证明 fail-closed，不等于已有真实强制沙箱；A3.1/A3.2/A3.3 仍未完成。

## Latest verification correction (2026-08-31, cross-process negative oracles)

`TestNegativeCrossProcessOracles` now covers independent worker-death and
network-loss processes. JSONL reload closes a committed crash-open prefix into
the unique `interrupted` lifecycle terminal state without fabricating a
`tool/result`; a closed provider endpoint settles as provider error without a
durable mutation. A9.2 remains partial until this oracle pattern covers every
required tool/provider and external side effect.

## Latest implementation correction (2026-09-01, live history authority)

The live `projection.Cursor` now derives history through
`session.DeriveHistoryEvents` from its committed event prefix. Its validation
log is retained only for append admission, removing the previous implicit
second history authority. Native reconnect surface snapshots and standard
plan/todo control values also consume a canonical projection cursor, with
explicit fallback only for truncated pages and legacy assistant replacement
markers. Cursor/cold equivalence and
dependent loop, compaction, meter, Web, native, and SDK tests pass; native
rich trajectory/control-state and remaining ACP/SDK projection migration
remain partial under A8.1.

## Latest implementation correction (2026-09-01, ACP cancellation idempotency)

Unknown-session `session/cancel` is now ignored as an idempotent notification,
matching the pinned reference behavior during stale cancellation and
disconnect races. The external wire regression is
`internal/acp/contract_external_test.go:TestExternalACPCancelUnknownSessionIsIdempotent`.
A7.1 remains partial because the complete reference ACP client and
post-disconnect side-effect matrix has not been executed.

## Latest implementation correction (2026-09-01, ACP transport-error teardown)

The ACP abnormal read-error path now cancels pending server-initiated
permission requests before joining prompt workers. This prevents an approval
request using a longer-lived context from hanging connection teardown. The
regression is
`internal/acp/server_test.go:TestServerScannerErrorCancelsPendingPermissionBeforeWaiting`.
A7.1 remains partial because the complete external reference matrix is still
not executed.

## Latest implementation correction (2026-09-01, SDK malformed-frame behavior)

Malformed JSON lines in the SDK runtime are now ignored, matching the pinned
DSH line protocol; no uncorrelatable null-id parse-error response is emitted.
The regression is `cmd/pa/sdk_test.go:TestSDKServerIgnoresMalformedJSONLines`.
A7.3 remains partial because the reference-runtime and provider
fault/reconnect matrix is still incomplete.
