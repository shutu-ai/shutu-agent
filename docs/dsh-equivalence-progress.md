# DSH 等价审计进度（2026-08-28）

本文记录已验证的实现边界；它不替代 `dsh-equivalence-tasks.md` 的完整任务清单，也不把“有接口”当作“能力等价”。

## 本轮已完成

- JSONL 与 SQLite 通过 `SessionPersistence` 共享契约：header/version、append、严格 seq、幂等 replay、冲突拒绝、revision、Inspect、Flush、closed-prefix fork、OpenLog sink。
- 动态 plugin runtime 返回真实调用值，带 plugin generation；运行中的调用阻止 reload/unmount 回收，factory 失败回滚，panic 与取消可分类。
- 本地 code backend 对未实现的 network access、readonly/full sandbox mode 和 workspace-root escape fail-closed；越界路径在创建目录前拒绝。
- Agent-backed approval 事件投影为 `approval/asked` 与 `approval/decided`，旧 REPL 的 `interact/*` 事件仍可恢复。
- Agent-backed Web 普通消息、富图片消息和 goal continuation 使用显式 session log，不再因 session 切换借用当前全局 log。
- compaction pressure 已接入 meter 的 model-visible、replacement-aware surface 计数；request 生成前无法使用 provider usage baseline，这一点保留为已知边界。

## 仍未达到完整 Harness 等价

- native legacy command/REPL 仍有 `currentID` 与 `turnMu` 兼容桥；Web mutation、后台 job、skill、schedule 等仍需逐项证明完全不依赖全局状态。
- local sandbox 不是 hostile-code enforcing backend，不能宣称网络、文件权限、进程树和资源限制具有强隔离。
- approval 尚未把 CLI、Web、ACP 统一成同一 answerer service，也未完成所有 unavailable/expiry/replay 与执行结果的端到端 durable contract。
- Team 尚未把 teammate runtime、roster、owner authorization 和 snapshot restart/fork/cold inspection 完整接入 Agent/SessionPersistence。
- ACP permission callback、resume metadata、MCP Streamable HTTP/session 基础
  lifecycle 已有契约；reference 当前明确拒绝 MCP required task execution，Go
  也在 transport call 前拒绝。仍缺 audio/embedded context、additional
  directories 与全量协议 fault/client 边缘矩阵。
- nested Code Mode 尚未完整继承 parent call、registry generation、approval/sandbox policy、递归深度和资源回收。
- storage migration/backup/repair、bounded read、多进程锁、correlation/trace、fault/security/performance CI gate 仍未闭环。

## Latest verified delta (2026-09-01, remote catalog fault matrix)

- OpenAI-compatible remote model discovery now has a bounded probe deadline
  while still honoring caller cancellation and deadlines. A hanging catalog
  endpoint cannot hold model discovery indefinitely.
- Added a remote catalog fault matrix covering probe timeout, caller
  cancellation, HTTP 429 without retry, non-object listings, and explicit empty
  listings. Existing checks continue to reject oversize and malformed replies.
- This closes the remote catalog fault subcase for A7.3. The remaining A7.3
  gaps are still the reference-runtime external matrix, complete first-party
  effort maps, audio, and stable unsupported responses across every provider.

## Latest verified delta (2026-09-01, MCP restart effect oracle)

- Upgraded the real two-process stdio reconnect test into an external
  side-effect oracle. The first MCP server commits and fsyncs an effect while
  handling `tools/call`, then exits before returning a response. The
  supervisor starts a replacement process without replaying the call.
- The replacement is exercised only through explicit discovery, and the
  separate effect journal must contain exactly one committed entry. A stale
  generation callback or automatic tools/call replay would produce a second
  entry and fail.
- With HTTP auth/session/fault, pagination, SSE/list-changed, structured rich
  results, task metadata, DELETE close receipts, generation ownership, and
  this cross-process effect oracle in place, A7.2 moves to done.

## Latest verified delta (2026-09-01, second ACP client and sequence authority)

- Added `internal/acpclient`, an independent newline JSON-RPC client with its
  own request IDs, notifications, typed updates, reverse permission handling,
  authenticate, initialize, session/new, prompt, cancel, and reconnect
  operations. It does not reuse the ACP server's wire structures or reader.
- A production child-process fixture drives this second client through
  authenticate/initialize, session/new, reverse permission approval, a real
  editor side effect, durable image admission, cancellation, reconnect, and a
  recovery prompt whose provider request contains the earlier tool result,
  resolved image, cancellation prompt, and reconnect prompt.
- Fixed a durable sequence-authority race exposed by this fixture: ACP session
  logs are now registered as the session's runtime log, so titles and other
  application writers share the prompt loop's append authority. On resume the
  restored log atomically replaces the prior runtime log.
- Hardened SQLite interrupted-tail repair when a legitimate writer advances
  the tail between read and append: repair reloads the latest durable prefix,
  recomputes closers, and retries a bounded number of times using a typed
  conflict sentinel.
- A7.1 is now done. The equivalence claim remains fail-closed for the other
  registered blockers.

## Latest verified delta (2026-09-01, ACP output-loss cross-process recovery)

- Added an external output-loss oracle using two real ACP child processes and
  one SQLite database. The first process admits a prompt and has its stdout
  broken before the controlled provider commits assistant output; production
  code persists the output, while the client cannot receive the update or a
  success response.
- After the first process exits, a second independent process opens the same
  database, performs `session/reconnect`, and sends a recovery prompt. The
  replacement runtime's provider request contains the pre-loss assistant
  output and recovery prompt, and the second turn completes normally.
- This closes the output-loss disconnect/reconnect subcase. A7.1 now has only
  the second-independent-client equivalence subcase open.

## Latest verified delta (2026-09-01, ACP provider-fault reconnect replay)

- Added a production external-client fault fixture. The provider emits
  `partial before failure` and then fails; ACP returns the real `-32603`
  failure rather than a successful turn.
- Independent SQLite inspection confirms the durable `assistant/chunk`,
  interrupted assistant anchor, failed step, and failed turn. After a real
  `session/reconnect`, the controlled provider records the recovery request and
  observes the original prompt, the interrupted assistant prefix, and the new
  prompt.
- This closes the provider-fault/prompt-interruption reconnect subcase. The
  remaining failure-path subcase is output loss across external-client
  disconnect/reconnect; multi-client equivalence also remains open.

## Latest verified delta (2026-09-01, ACP rich-output replay after reconnect)

- Added a production ACP rich-output replay fixture. The child stores a
  canonical assistant message containing text plus an attachment-backed image
  block through the production session sink, performs a real
  `session/reconnect`, and sends a new prompt through the replacement runtime.
- The controlled provider records the post-reconnect model request. The
  assistant turn must still expose both the text and an image with a resolved
  local attachment path. After child exit, an independent SQLite/attachment
  handle and shared projection verify the same rich output.
- This closes the assistant rich-output replay subcase.

## Latest verified delta (2026-09-01, ACP image replay after reconnect)

- Extended the production ACP attachment fixture with a real
  `session/reconnect` before process exit. The replacement runtime reloads the
  durable transcript and resolves the stored `ImageRef` through the shared
  projection image resolver.
- A controlled provider records the post-reconnect model request. It must
  contain the original user text with a resolvable image, the first assistant
  output, and the reconnect prompt. This proves durable user-image state
  re-enters provider history, beyond post-exit projection inspection.
- A7.1's remaining rich-content work is now limited to failure-path reconnect
  recovery and a second independent external client.

## Latest verified delta (2026-09-01, ACP resource replay after reconnect)

- Added a production external-client resource replay fixture. The first ACP
  prompt carries text plus a `resource_link`; the client then performs a real
  `session/reconnect` through `acpFactory.ResumeSession`, and sends a second
  prompt through the replacement runtime.
- The controlled provider records the post-reconnect model request. It contains
  the original user text, the canonical resource-link projection, the first
  assistant output, and the reconnect prompt. After child exit, an independent
  SQLite handle and shared projection still preserve the resource marker.
- This closes the resource-link/text replay subcase. Rich-output replay at the
  provider-history seam, failure-path reconnect recovery, and a second
  independent external client remain open.

## Latest verified delta (2026-09-01, durable ACP attachment input)

- Added a production external-client attachment fixture. The child owns the
  real vision-route admission path: ACP canonical image admission, attachment
  publication, durable `ImageRef` in the user event, Agent loop execution, and
  `acp.Server`. A controlled provider records the exact reference it receives.
- After the external client disconnects and the child exits, independent
  handles reopen SQLite and the attachment store. The fixture verifies the
  same attachment ID and PNG bytes, resumes the same durable session, and uses
  the shared projection image resolver to restore the user image to its local
  path.
- This closes A7.1's durable attachment-input subcase. Resource/rich replay
  after reconnect, failure-path reconnect recovery, and a second independent
  client implementation remain open.

## Latest verified delta (2026-09-01, ACP admission submatrix)

- Consolidated the external ACP admission matrix at the exported newline-JSON
  server seam. It proves stable `-32602` fail-closed behavior for relative CWD,
  `additionalDirectories`, `mcpServers`, audio, embedded context, unknown
  session, blank text, non-canonical image data, and unsupported image media.
  It also drives accepted text, resource-link, and canonical image prompts.
- This closes the admission subcase only. Durable attachment input, rich state
  replay after actual resume/reconnect, failure-path reconnect oracles, and a
  second independent external client remain open.

## Latest verified delta (2026-09-01, production ACP disconnect oracle)

- Added a production `cmd/sta` ACP regression in which the child owns SQLite,
  `acpFactory`, the minimal tool registry, the Agent loop, and `acp.Server`.
  The parent is an external line-protocol client only. Its prompt commits a
  real `str_replace_editor` file side effect, then the client disconnects by
  EOF and the child process exits.
- After process death, an independent SQLite handle validates the complete
  lifecycle, tool call/result, filesystem event, and assistant settlement. A
  second independent `acpFactory` resumes the same durable session id and the
  shared projection reconstructs the same user/assistant history with the
  canonical cursor.
- This closes A7.1's previous "no real external post-disconnect state/side
  effect oracle" gap for one successful tool path. The full pinned ACP matrix
  remains partial: additional directories, durable attachment input,
  resource-link replay, audio, repeated reconnect/failure paths, and
  alternative client implementations still need proof.

## Latest verified delta (2026-09-01, cross-process fault/security start)

- Added `internal/contractfixture/fault_security_matrix_test.go` as the first
  A9.3 release-gate leg. It starts a real child SQLite worker through the
  production persistence adapter, lets the worker exit abruptly with an open
  turn, and requires the first restart handle to recover only the committed
  prefix. A second independent handle must observe the same recovered
  five-event lifecycle and revision 5.
- The same matrix adds a symlink side-effect oracle. It is skipped on the
  current managed Windows host because creating the link requires privilege;
  that skip is recorded as unverified, not as a pass.
- A9.3 moves from open to partial. Disk-full, kill-at-every-write, process-tree
  exhaustion, hostile-worker, network-boundary, consolidated credential
  rotation, plugin reload, and MCP restart side-effect oracles still remain, as
  does execution on every claimed platform. Existing bounded stdout/stderr
  coverage is now part of the registered A9.3 evidence for the pipe flood
  subcase; true disk-full and repeated kill-point injectors remain outstanding.

## Latest verified delta (2026-09-01, MCP call and reconnect semantics)

## Latest verified delta (2026-09-01, Code Mode close quiescence)

- TypeScript Code Mode now owns the process-tree stop operation together with
  each active run. External cancellation, output-limit termination, and
  runtime disposal cancel host bindings before waiting for the worker, and
  terminate the owned tree independently of stdout/stderr scanning.
- Added a regression proving `Close()` cancels an in-flight binding and joins
  the worker without returning a transport error. This closes a real teardown
  deadlock window, but A3.3 remains partial: the full resource, hostile-worker,
  and cross-platform enforcement matrix is still required.

## Latest verified delta (2026-09-01, Code Mode rich extension preservation)

- Code Mode durable nested-dispatch projection now preserves the original JSON
  metadata for forward-compatible content block kinds, instead of reducing an
  unknown `audio` or `resource` block to only its type/text fields.
- Added a regression covering nested audio metadata. This improves rich-result
  fidelity but does not claim native audio support; unsupported transports must
  continue to reject or explicitly project those blocks.

## Latest verified delta (2026-09-01, shared plan projection for Web state)

- The canonical projection now separates durable goal records from plan
  records, reconstructs goal-to-plan links and todo steps, and applies status,
  update, delete, and round-state mutations from the event stream.
- Web session state now reads `plan_mode`, goals, and plans from
  `projection.Build` instead of instantiating a second disposable plan engine.
  This closes a real cold/live authority split; native/ACP/SDK control-state
  consumers and the full cross-entry byte-equivalence fixture remain partial.

## Latest verified delta (2026-09-01, canonical native goal/todo adapter)

- Native reconnect/history control values now derive the current goal and todo
  list from the shared projection snapshot. Goal round/status changes and plan
  deletion update the same durable projection, including goal-to-plan links and
  detached todo cleanup; the native adapter only converts that snapshot to the
  compact DSH wire shape.
- This removes another private native control-state authority. Feedback, jobs,
  MCP activity, and complete ACP/SDK/query/trajectory migration remain partial,
  so A8.1 and the release gate stay open.

## Latest verified delta (2026-09-01, durable job lifecycle merge)

- The shared `Jobs` projection now merges lean `job/status` and `job/done`
  facts onto the original `job/start` record. Cold replay therefore retains
  kind, label, owner, and creation time while advancing status/detail/output
  and update time, matching the live registry's job identity semantics.
- Added an explicit timestamped replay regression. This fixes projection
  fidelity but does not by itself migrate every Web/native/ACP/SDK job reader
  to the shared snapshot; A8.1 remains partial.

- MCP `tools/call` now has a single-call boundary. Connection errors and
  timeouts are returned without replaying the request; the supervisor may
  reconnect in the background, while a later model-level retry remains
  explicit.
- Reconnect defaults are aligned with the reference (500ms initial delay,
  30s maximum delay, 10 attempts). Focused MCP, config, and CLI tests pass.
- CLI, Web, and ACP plan-mode reads now use the shared projection snapshot, and
  malformed boolean control facts fail closed instead of becoming inactive.
- This is a concrete semantic correction, not closure of A7.2: external
  client/fault matrices and the remaining release gates are still required.

## Latest verified delta (2026-08-29, bounded execution output and terminal ownership)

- `run_code` and foreground `bash` continuously drain stdout/stderr through
  bounded captures; background bash keeps pollable files bounded.
- Persistent terminals now attach Unix process groups or Windows Job Objects
  and close descendants within the existing quiesce barrier.
- Full equivalence remains open for hostile sandbox enforcement, cross-process
  resource ownership, and the strict CGO race/leak gate.

因此当前结论仍是“部分能力已实现并有局部证据”，不能发布 `capability-equivalent` 声明。

## Latest verified delta (2026-08-28, sandbox network and approval creation)

## Latest verified delta (2026-08-29, child publication rollback)

- Spawn/fork child logs are now published to the Agent runtime index only after
  seed restoration, durable header/seed creation, and the first lifecycle event
  all succeed. Failed initialization cannot leave a resolvable ghost child.
- The focused rollback regression passes repeatedly. This closes one in-process
  publication window only; cross-process Team receipts, hostile sandboxing,
  protocol lifecycle, and the strict race gate remain open.

## Latest verified delta (2026-08-29, process-wide shutdown coordination)

## Latest verified delta (2026-08-29, rich additionalContexts)

- `ToolResult` and Code Mode binding results now carry rich deferred user
  messages with source metadata; the old string compatibility field remains.
- Nested Code Mode results are ordered by binding submission, survive an outer
  program failure, and appear in the durable nested-dispatch projection.
- The loop appends them after the corresponding tool result and replays them
  on the following request without runtime-only provenance fields.
- Tool results now support the successful `concludesTurn` terminal marker, and
  registries expose composable post-execute around hooks.
- Remaining Code Mode gaps are post-execute decision parity, full image/content
  forwarding, worker resource enforcement and complete producer coverage.

- Added `internal/lifecycle.Coordinator` and wired the native composition root
  to register store, telemetry, capability services, Jobs, Agents, Web and
  admission in dependency order.
- Teardown now closes admission first, then drains Jobs before Agents and
  invokes the resource-level close barriers. Concurrent Close callers share
  one completion result; late registration is rejected.
- This closes the application-level ordering gap only. Cross-process crash
  recovery, hostile sandbox enforcement, complete ACP/MCP lifecycle and the
  strict race gate remain open.

## Latest verified delta (2026-08-29, shared replay fixture consumers)

- Added a transport-neutral `contractfixture.CoreTurnEvents` decoder and moved
  the session, persistence and native Web fixture consumers onto it.
- The shared fixture now has one parsing/timestamp boundary, while each target
  package still performs its own canonical event validation or projection.
- T20 remains partial because the fixture matrix does not yet cover all tool,
  approval, Team, ACP/MCP and SDK contracts.

## Latest verified delta (2026-08-29, cross-process approval and Team provisioning)

- Two independently opened SQLite stores now race the same approval decision;
  the durable CAS yields exactly one winner and one `ErrAlreadyResolved`, with
  only one terminal audit event.
- SQLite Team provisioning can atomically publish the child Session/header/
  closed fork seed and the lead's `team/member` provisioning fact. Root
  sequence conflicts and invalid seeds roll back both sides.
- The remaining Team gap is deliberate: live Agent publication and the later
  active/failed member transition still cross separate runtime and event
  commits, so this is not yet a complete cross-process membership transaction.

## Latest verified delta (2026-08-29, structured post-execute decisions)

- Tool registries now expose explicit `accept|block` post-execute policy
  decisions. Accepted value/content replacements are mutually exclusive and
  cannot replace a failed result; accepted deferred contexts prepend existing
  tool contexts, while blocked results retain only decision feedback/contexts.
- The complete reference waterfall and every producer's output/replay matrix
  remain open, so T8 stays partial.

## Latest verified delta (2026-08-29, failed nested Code Mode context)

- Code Mode now forwards a nested tool's rich deferred contexts even when the
  nested tool rejects. The TypeScript promise remains rejected, while the
  outer durable `code/dispatch` and `tool/result` retain the context for the
  next model step.
- Worker resource enforcement, renderer-owned value replacement, and complete
  image/content forwarding remain open under T11.

## Latest verified delta (2026-08-29, ACP and goal durable failure propagation)

- ACP loops now bind the addressed session runtime and durable event sink, so
  context-aware filesystem and MCP tools no longer fall back to a void event
  callback. ACP MCP list/call and terminal start/stop return persistence
  failures; terminal-start failure closes the just-created process.
- ACP subagent settlement uses an explicit parent session event sink, and the
  goal round driver now propagates status/round lifecycle append failures to
  its caller. A goal runner cannot continue after its initial durable status
  event fails.
- ACP permission requests created by the application factory now use the same
  session-scoped approval transition as CLI and Web; the private engine is
  retained only for directly-constructed unit-test sessions.

These are fail-stop and routing fixes. They do not close ACP prompt admission
and connection teardown parity, atomic domain-plus-event transactions, or the
remaining reference/fault/security/performance gates.

## Latest verified delta (2026-08-29, durable pre-step failure propagation)

- Native and ACP compaction pre-step injectors now expose an error-returning
  entry point. Failure to persist compaction start, summary, end, or error-end
  is returned by the loop and prevents the next provider request.
- The legacy void injector remains available only as a compatibility wrapper;
  the production compaction registration uses the durable entry point.

This closes a fail-open pre-step path. It does not make domain mutation and
event append one transaction, and it does not close the remaining retry,
cross-process storage, sandbox, protocol, or CI gates.

## Latest verified delta (2026-08-29, job terminal projection serialization)

- `job/*` runtime events now pass through one application-level commit lock.
  The synchronous `job_start` observation and asynchronous completion observer
  therefore cannot both win the scan-and-append race for the same `job/done`.
- A concurrent regression calls the terminal projection from 32 observers and
  verifies exactly one durable `job/done` event. This closes an in-process
  duplicate-terminal seam; cross-process job event atomicity and full schedule/
  workflow wake-up parity remain open.

## Latest verified delta (2026-08-29, JSONL append rollback)

- The standalone JSONL persistence adapter now records the committed byte
  prefix before each append batch and truncates plus syncs that prefix when a
  record write or final sync fails. This prevents a failed batch from being
  mistaken for committed events after restart.
- A regression covers restoration of the exact committed prefix. OS-level
  crash injection and the complete cross-process corruption matrix remain open.

- Linux bubblewrap capability probing now separately verifies the network
  namespace. When available, read-only/workspace-write calls use
  `--unshare-net`; `RequireNetworkIsolation` is enforced by the provider itself
  as well as by the Engine capability gate. Hosts without that backend remain
  fail-closed, and full-access calls cannot claim strong/network isolation.
- OpenAI-compatible calls now carry the configured provider label into shared
  HTTP/SSE diagnostics, so a custom route is not reported as `deepseek`.
- `interact_ask` and `ask_user_question` now use the same atomic creation seam
  as sensitive-tool and ACP approval paths when SQLite is available: pending
  projection plus `approval/asked` are committed together, then projected into
  the session log captured at request creation. Compatibility providers retain
  append-failure rollback.

These are partial closures. Windows still has no enforcing network namespace,
Team/Code Mode/ACP-MCP lifecycle and telemetry remain incomplete, and the
reference/fault/security/performance/race gates are not all passing.

## Latest verified delta (2026-08-28, correlation propagation)

- Added structured runtime correlation fields for agent, session, turn, step,
  request, call and generation boundaries. Loop request hooks/provider calls
  and tool dispatch contexts now receive the same narrowed identity without
  changing the durable session event wire.
- Added concurrency-safe structured counters and a bounded in-process span
  recorder with typed failure-code classification; the application loop now
  records turns, steps, provider attempts, tool outcomes and usage totals.
- Added regression coverage for runtime-context narrowing, loop request-hook
  propagation, metrics concurrency and span idempotence. Export wiring and the
  proof that observer failures cannot affect execution remain open.

## Latest verified delta (2026-08-28)

The following gaps were materially reduced after the original audit snapshot:

- ACP permission now has a server-initiated `session/request_permission` JSON-RPC round trip, DSH-shaped `toolCall` parameters, response routing, cancellation cleanup, and fail-closed sensitive-tool execution.
- ACP prompt handling now accepts text, image, and resource-link blocks where the advertised capability is enabled; assistant updates are emitted from committed `assistant/message` events rather than raw provider chunks.
- Tool results now preserve rich content atomically, structured `meta`, deterministic spill previews, retrieval hints, and tool-call source linkage.
- Team teammates now use the application Agent Registry, durable child sessions, direct-child authorization, closed-prefix fork seeding, roster snapshots, and restart-time handle rebind.
- `go test -count=1 ./...`, `go vet ./...`, and `go build ./...` pass. `go test -race` remains unverified on this Windows host because `gcc` is not installed; it fails before compiling tests with `cgo: C compiler "gcc" not found`.

These changes do not close the remaining hard gates: hostile-code enforcing sandboxing, complete Agent-scoped migration of legacy native/background paths, unified CLI/Web/ACP approval replay, full ACP/MCP task and reconnect lifecycle, nested Code Mode resource limits, correlation/trace telemetry, storage backup/migration/multiprocess locking, reference-runtime replay fixtures, and the fault/security/performance CI gate. The project must therefore still not claim full DeepSeek Harness capability equivalence.
## Latest verified delta (2026-08-28)

- Added `internal/contractfixture/core-turn-replay.json` as a shared canonical
  turn fixture. `internal/session` validates and derives it; the native Web
  projection replays and revalidates the projected envelopes.
- Corrected canonical tool-result shape: `tool/result.message.content` now
  contains one `tool-result` block with nested content and `toolCallId`.
  Legacy top-level `content/output` fields remain readable, while native
  projection unwraps the outer block exactly once.
- Restored Team members are now visible to the subagent control plane after
  rebind: `list_agents`, `subagent_list`, `send_message`, `followup_task`,
  `subagent_status`, and `interrupt_agent` can use the durable roster.
- Added regression coverage for the shared fixture, nested tool-result replay,
  and restored teammate control callbacks.
- Tightened the checked-in session-event schema with canonical surface metadata,
  content-block variants, nested tool-result blocks, attachment references, and
  the actual `request/header`/turn-end reason vocabulary.
- ACP sensitive-tool approval now creates and resolves requests through the
  session-aware `interact.Engine`; the ACP client remains only the answerer.
  Request IDs are distinct from tool call IDs, and unavailable/rejected paths
  are durable canonical decisions.
- Added a direct ACP gate contract test covering allowed-once and rejected
  decisions, request/call correlation, and canonical durable event ordering.

Remaining hard gates are unchanged: reference-runtime double replay, complete
removal of native global-session compatibility state, enforcing hostile-code
sandbox, unified durable approval service, full ACP/MCP/SDK lifecycle, exact
meter/compaction contract, and fault/security/performance CI gates.

Verification after this delta: `go test -count=1 ./...`, `go vet ./...`, and
`go build ./...` pass. `go test -race` cannot start on this host: `CGO_ENABLED=0`
is rejected because race requires cgo, while `CGO_ENABLED=1` cannot find the
local `gcc` compiler.

## Latest verified delta (2026-08-28, continuation)

- DeepSeek Search now exposes `OnRequestContext(context.Context, SearchRequestEvent)`;
  the composition root routes the durable `web/search-request` event through the
  active Agent/session runtime log, with a legacy callback retained for callers
  that do not provide a context.
- Added a regression test proving caller context reaches the request-event sink.
- Updated the task ledger to distinguish completed local contracts from the
  remaining reference-runtime, enforcement, lifecycle, storage, telemetry and
  CI-gate work. Full capability equivalence is still not claimed.

Verification for this delta: `go test -count=1 ./internal/web ./cmd/sta` passes.

- Per-session model selection now fails closed when a persisted provider ID is
  unknown; it no longer silently falls back to the process-global provider.
- Verification remains: `go test -count=1 ./...`, `go vet ./...`, and
`go build ./...` pass. The race gate is blocked by this host toolchain:
  `CGO_ENABLED=1 go test -race ./...` cannot find `gcc` before compilation.

## Latest verified delta (2026-08-28, session-local schedules)

## Latest verified delta (2026-08-28, persistence and exact ACP capability)

- SQLite `LoadSession` now closes a structurally valid interrupted turn with
  durable `interrupted` terminal events. `InspectSession` remains strict and
  non-mutating, while native history/fork use an explicit raw live-tail read
  so in-progress work is not presented as a normal completed turn.
- Provider image capability is explicit and is propagated through the retry
  wrapper. ACP image advertisement and admission now check the exact selected
  provider route in addition to global multimodal configuration.
- The corresponding task ledger entries were corrected; these changes still
  do not close hostile sandbox, unified approval, full lifecycle, telemetry,
  reference replay, or fault/security/performance gates.
- Provider/profile edits now use compensating rollback when registry rebuild
  fails, so a rejected edit cannot leave durable settings ahead of the live
  provider registry.

- Durable schedule projections are now resolved from the runtime session and
  rebuilt from that session's log; concurrent sessions no longer share the
  schedule table or ID sequence.
- Reminder delivery uses the addressed session's Agent inbox when the Agent
  runtime is available, and pre-step delivery is serialized against the
  background scheduler. Legacy direct REPL delivery remains compatible.
- Added regression coverage proving schedule create/list state does not leak
  between two session contexts.

Verification for this delta: `go test -count=1 ./internal/schedule ./cmd/sta`
passes; full suite remains the required follow-up gate.

## Latest verified delta (2026-08-28, session-scoped plan projections)

- `plan_*` tools and goal continuation now resolve a disposable plan Engine
  from the addressed runtime session and restore it from that session's log;
  the no-runtime CLI path keeps the legacy current-session engine.
- Goal activation callbacks are context-aware for Agent runs, and stale
  session projections are closed when the active session is restored.
- Minimal persistent shell creation/reuse now uses the runtime owner and that
  owner's workspace instead of `currentID`.
- Added regression coverage for plan projection isolation across two Agent
  session contexts.

Verification for this delta: `go test -count=1 ./...`, `go vet ./...`, and
`go build ./...` pass. The race gate remains unavailable on this Windows host
because the required `gcc` compiler is not installed.

## Latest verified delta (2026-08-28, child runtime selection)

- In-process subagent children now resolve provider/model, tool registry, and
  prompt from the parent session when those resolvers are supplied by the
  composition root; stale global selection is no longer the only path.
- Each child loop now carries its own owner ID, runtime session context, and
  durable event sink, so child tool authorization and event writes are scoped
  to the child session.
- Added regression coverage for parent-session provider/model and
  tools/prompt resolver behavior.

Verification for this delta: targeted subagent and application tests pass.
The full suite and race gate remain part of the final verification gate.

## Latest verified delta (2026-08-28, nested child log binding)

- SpawnProvider now publishes each child log to the host runtime index when a
  composition root supplies `BindSessionLog`; nested session-aware tools can
  resolve the child ID and its durable log instead of treating it as unknown.
- Added coverage that verifies the child ID/log binding accompanies the
  provider/model resolver path.

## Latest verified delta (2026-08-28, enforcing sandbox and JSONL process lock)

- The local code provider now performs a bounded functional bubblewrap probe on
  Linux. Only a usable bwrap backend advertises strong isolation and the
  read-only mode; unavailable hosts fail closed for those requests.
- Linux bwrap execution applies the reference file-effect profile: the host
  tree is read-only, workspace-write remounts only the selected workspace, and
  `/tmp` is ephemeral. Network isolation remains unadvertised because the
  reference file-effect seam does not promise it.
- JSONL now takes a per-session OS file lock around create, append, load,
  inspect, and repair. This closes the gap where independent processes could
  concurrently mutate one transcript despite the existing in-process mutex.
- Added a regression test proving independent lock handles serialize.

Verification for this delta: `go test -count=1 ./internal/code
./internal/persistence ./internal/store` passes. The complete suite, vet,
build, and the host's unavailable cgo race gate remain required before any
equivalence claim.

## Latest verified delta (2026-08-28, session-local compaction)

- Compaction pressure and context-overflow recovery now resolve a dedicated
  BasicEngine per non-current runtime session, using that session's provider,
  model, log, and compaction counter. The legacy current-session engine remains
  the compatibility path for direct CLI calls.
- Re-registering compaction clears stale per-session projections, preventing a
  provider/model settings change from reusing an old summary engine.
- Added a regression test for per-session compaction projection identity and
  reuse.

Verification for this delta: `go test -count=1 ./cmd/sta` passes; the complete
suite and final gates remain required.

## Latest verified delta (2026-08-28, bounded JSONL cursor reads)

- `JSONL.ReadFrom` now scans records incrementally under the session process
  lock, validates the complete sequence/lifecycle state, and retains only the
  requested suffix. It no longer calls full `Inspect` and then copies a second
  full event slice.
- Cold cursor reads remain non-mutating: an incomplete final record is ignored
  and repair is still restricted to `Load`.
- Added/retained backend contract coverage for cursor suffix and revision
  behavior; Unix and Windows process-lock implementations both compile.

Verification for this delta: `go test -count=1 ./...`, `go vet ./...`, and
`go build ./...` pass. `go test -race ./...` remains unable to start because
the host has no `gcc` compiler.

## Latest verified delta (2026-08-28, lossless Web SSE cursor repair)

- The Web SSE handler now deduplicates events that race the initial snapshot
  and subscription registration.
- When a live notification jumps over a sequence, the handler reads the
  durable suffix through the bounded store page API and emits only contiguous
  events. A periodic tail reconciliation also repairs a dropped final hub
  notification when no later live event arrives.
- The in-memory event hub remains non-blocking; it is no longer treated as the
  source of truth for stream delivery.
- Added regression coverage for snapshot/subscription duplicate suppression
  and ordered durable gap repair.

Verification for this delta: `go test -count=1 ./...`, `go vet ./...`, and
`go build ./...` pass. This closes the Web stream delivery gap only; it does
not establish full Web authorization or cross-surface approval equivalence.

## Latest verified delta (2026-08-28, SQLite seek reads and storage hardening)

- SQLite now exposes an optional seek-capable `seq >= N` suffix primitive;
  `SQLiteAdapter.ReadFrom` uses bounded pages and returns the durable revision
  without materializing the prefix. Stores that do not implement the optional
  seam retain the validated full-read fallback.
- SQLite schema migration now runs as one transaction and records a schema
  version marker. Database files and materialized WAL sidecars are tightened
  to private `0600` permissions; append re-applies the policy after sidecar
  creation.
- Added coverage for private database permissions and retained the shared
  persistence contract suite.

Verification for this delta: targeted persistence/store/interact/application
tests pass. The broader persistence coordinator (write-behind ownership,
quiescent flush/dispose, live adoption and immutable inspection cache) remains
open and is not represented as complete by this change.

## Latest verified delta (2026-08-28, approval ownership collision guard)

- Session-aware approval requests still use the legacy process-wide `req-N`
  provider ID format, preserving existing clients, while the durable restore
  path detects a legacy ID reused by multiple sessions and refuses to restore
  the ambiguous item rather than allowing cross-session resolution.
- Added coverage that session-aware requests retain their owner and receive
  distinct IDs within one approval service.

Verification for this delta: `go test ./internal/interact ./cmd/sta ./internal/persistence ./internal/store` passes.

## Latest verified delta (2026-08-28, Code Mode dispatch gate)

- TypeScript Code Mode now supports a per-program ordered dispatch gate:
  consecutive safe host calls overlap up to `MaxParallelSubCalls` (default
  ten), while an unsafe/unknown/panicking classification is an exclusive
  ordering barrier.
- The host CodeTools wiring classifies calls through the existing Registry
  `IsConcurrencySafe` seam, preserving schema/policy ownership in the host.
- Added direct gate coverage for the exclusive barrier; standalone runtime
  callers without a classifier retain the previous compatibility behavior.

This closes only the concurrency scheduling portion of Code Mode. Lossless
argument snapshot rejection, deferred rich content/additional-context
propagation, and full run-settlement observability remain open.

## Latest verified delta (2026-08-28, sibling delegation scheduling)

- `subagent`, `subagent_fork`, and `spawn_teammate` now advertise the existing
  loop concurrency-safe classifier, so sibling delegations can overlap under
  the rolling tool pool while their parent results still commit in model order.
- The Team/subagent registries remain mutex-protected and child sessions keep
  independent logs; send/wait/cancel control calls remain exclusive by default.

## Latest verified delta (2026-08-28, configurable Code Mode overlap)

- Added `code.max_parallel_sub_calls` with validation/default `10` and wired it
  into the TypeScript host dispatch gate; `1` restores serial nested dispatch.
- Existing standalone `ProgramRequest` callers without a classifier retain
  compatibility behavior, while production CodeTools uses the Registry's
  fail-closed `IsConcurrencySafe` classification.

## Latest verified delta (2026-08-28, sandbox modes and provider snapshots)

- The local sandbox now distinguishes explicit `readonly`, `workspace-write`,
  and opt-in `danger-full-access` requests. Readonly is admitted only when a
  functional bubblewrap backend is available and never creates a working
  directory as a side effect.
- Provider registry assembly and Web/ACP capability discovery consume a single
  immutable configuration/key/profile snapshot, reducing live model-switch
  races and preventing partially edited provider state from being observed.
- SQLite migration now rejects a database schema newer than the running
  binary instead of silently downgrading its interpretation.

## Latest verified delta (2026-08-28, maintenance and Team lineage)

- SQLite and JSONL now expose explicit integrity-check, non-overwriting backup,
  and session-repair operations. JSONL copies locked artifacts through
  temporary files; SQLite uses `VACUUM INTO`; cold inspection remains
  non-mutating.
- Custom-provider deletion now compensates a failed registry rebuild by
  restoring settings, in-memory indexes, and the previously published
  registry.
- Team child sessions now persist parent lineage and resolve the shared task
  and mailbox board to the lead session, while retaining the child runtime
  identity for authorization.

These are concrete durability and ownership corrections. Reference replay,
complete approval replay, enforcing hostile-code isolation, external ACP/MCP
contracts, nested Code Mode settlement, and the final fault/security/
performance/race gates remain open.

## Latest verified delta (2026-08-28, MCP selectors and Code Mode boundaries)

- Native MCP composition now registers both dynamic selector tools (`mcp_list`
  and `mcp_call`) when MCP is enabled, in addition to startup-discovered
  namespaced bridge tools. The selector path uses the active runtime event
  sink and fails closed when its durable `mcp/*` event cannot be appended.
- Go Code Mode now rejects lossy JavaScript values before the subprocess
  boundary (including Date, negative zero, sparse/extra-property arrays,
  functions and cycles), accounts structured completions against its output
  quota, cancels unawaited child bindings after the program emits `done`, and
  waits for tracked subprocesses during `Close`.
- ACP's generic server admission now enforces the reference raster MIME
  allow-list and canonical RFC 4648 base64; session-update write failures are
  returned as protocol-internal errors instead of being silently discarded.

These are narrow contract corrections. They do not close the enforcing
sandbox, complete durable approval/Team persistence, implement the full
reference Code Mode worker resource model, or satisfy the final replay,
security, performance and CI gates.

## Latest verified delta (2026-08-28, Agent hierarchy disposal)

- `Registry.Create` now registers each published child Agent with its parent
  scope. Closing a parent therefore removes and closes descendant Agents,
  while an explicitly closed child remains safe to encounter during the later
  parent cleanup.
- A regression test verifies child status and registry publication after
  parent close.

The native legacy/global compatibility paths and the remaining lifecycle
integration gates are still intentionally open.

## Latest verified delta (2026-08-28, Web command and runtime lifecycle isolation)

- Web slash commands now install an addressed-session runtime before command
  dispatch. `/plan`, `/goal`, `/feedback`, `/compact`, `/permission`, and
  `/export` write to the target Agent log and plan projection instead of the
  legacy `currentID`/`a.log` fallback; a cross-session regression test covers
  the goal path.
- Code Mode nested binding no longer reads the legacy mutable policy field on
  Agent-owned calls. Configuration snapshots deep-copy mutable maps, slices,
  and pointer switches; ACP MCP/subagent construction uses its pinned
  creation-time provider/model/config snapshot.
- Web stop cancellation is keyed by session, and goal-scheduler shutdown now
  cancels and joins its worker before closing scheduler resources.

These corrections improve isolation and shutdown semantics but do not close
the reference-runtime replay, enforcing sandbox, unified approval, complete
ACP/MCP/Team lifecycle, nested Code Mode resource, observability,
fault/security/performance, or CGO race gates.

## Latest verified delta (2026-08-28, ACP identity and rich-content boundary)

- ACP `session/new` now publishes an optional durable `SessionID` supplied by
  the session implementation. The legacy generated id remains available for
  non-durable embedders, while the application factory's returned id is now
  directly usable by `session/resume`/`session/reconnect`.
- ACP image prompt admission validates the full ordered block batch, including
  MIME, canonical base64, non-empty data, and per-image size, before writing
  any attachment. This matches the reference's atomic malformed-batch seam.
- Committed ACP assistant output now preflights all text/image blocks and emits
  them in content order. Missing or unreadable image attachments fail the
  delivery before any update callback is invoked; legacy text-only payloads
  remain supported.
- The shared core fixture is now consumed by session restore/history and native
  projection tests. Reference ACP bridge/content/approval/turn/multi-session/
  disposal/edge/codec tests were run directly from the checked-in Harness
  workspace: 82 tests passed. This is evidence for the ACP subset, not a claim
  of full dual-runtime replay equivalence.
- SQLite first-open schema creation and migration now take the same OS-level
  lock as append/backup, closing the concurrent deployment initialization
  window.

These changes close specific ACP contract mismatches. Durable approval
authority, enforcing sandbox backends, full Team/MCP lifecycle, reference
event replay, and the final fault/security/performance/race gates remain open.
## Latest verified delta (2026-08-28, approval convergence and canonical native projection)

- Web approval resolution now uses one serialized application transition: the
  live request is resolved, the decision is appended to the owning session, and
  an append failure restores the exact pending request. CLI legacy resolution
  has the same rollback behavior.
- ACP sessions reuse the app approval Engine when the interaction capability is
  enabled and install a session-scoped policy; closing one ACP session no
  longer closes the shared approval service. ACP asked/decision append failures
  clean up or restore the in-memory request.
- Native WebSocket projection now accepts both legacy `interact/*` and
  canonical `approval/asked|approval/decided` events, including question
  resolution and cancellation semantics.
- Team task/message adapters now have regression coverage proving snapshot
  persistence failure rolls back create, update, and send mutations.

These changes reduce live/replay divergence but do not close the durable
approval-table/atomic-transaction requirement, the full three-surface answerer
contract, or the remaining T0/T6/T10/T11/T12/T14-T21 gates.

## Latest verified delta (2026-08-28, durable approvals and nested rich dispatch)

- SQLite-backed approval rows now preserve request identity, session/call
  ownership, structured answers and terminal status across restart; resolution
  is compare-and-set and duplicate answers are rejected.
- The app selects the durable approval provider when SQLite is available and
  falls back to the memory provider only for stores without that optional
  capability. Event replay remains the audit contract, with rollback on event
  append failure.
- Code Mode nested dispatch now retains ordered tool content blocks, metadata
  and additional-context handles in its durable envelope while returning only
  the JSON value to the TypeScript program.
- Durable schedule fire records carry occurrence identity and are idempotent;
  background delivery now records the fire before enqueueing the Agent
  follow-up, avoiding an unlogged live enqueue and allowing retry after a
  failed follow-up.

These are still partial closures: provider/event commit is not one SQLite
transaction, reference replay and the enforcing sandbox remain open, and the
full Team/ACP/MCP/lifecycle/fault/security/performance gates are not complete.

## Latest verified delta (2026-08-28, projection reconciliation and Code Mode namespaces)

- Durable approval startup now reconstructs the SQLite projection from the
  authoritative session event log and atomically replaces the table, removing
  orphan rows left by a crash between provider mutation and event append.
- TypeScript Code Mode now supports multiple validated portable binding globals
  and arbitrary member names over the bridge, while retaining the legacy
  default `tools` namespace.
- Token metering now folds replacement-aware positional nodes, includes image
  placeholders in heuristic pricing, fingerprints full tool schemas/content,
  and carries the provider usage baseline.
- Application shutdown now registers Agent quiescence before dependent service
  cleanup, preventing late child turns from entering closed services.

These are targeted closures; provider/event atomicity, enforcing sandbox
backends, complete Team/ACP/MCP lifecycle, reference double replay, and the
final fault/security/performance/race gates remain open.

## Latest verified delta (2026-08-28, fail-closed runtime authorization)

- Agent-owned turn assembly now treats non-`not found` session-config read
  failures and approval-policy injection failures as fatal instead of silently
  inheriting process-global provider, model or permission state. The legacy
  REPL compatibility bridge remains explicitly isolated because its historical
  signature cannot return an error.
- `subagent_resume` now requires a calling Agent and validates the target's
  direct/descendant lineage before accepting a durable session id; a supplied
  provider must also match the known live child provider.
- `subagent_status`, `subagent_cancel` and explicit-parent `subagent_list`
  now use the same caller-scope check, preventing ID probing, cross-Agent
  cancellation and parent-session enumeration.
- Session-provider/model lookup used by child and compaction runtimes now has
  the same fail-closed behavior: durable read errors select an unavailable
  adapter rather than silently reusing the process-global LLM.
- Subagent composition callbacks now reject all child tools and use an
  isolated unavailable-runtime prompt when the parent session cannot be
  reconstructed.

These close two authorization/failure-mode gaps, but do not make the legacy
bridge, subagent provider lookup, or the broader Team/ACP/Web mutation surface
fully equivalent.

## Latest verified delta (2026-08-28, executable reference double replay)

- Added `scripts/verify-reference-replay.mjs`, which imports the actual
  reference `@deepseek-ai/dsh-session` source under the reference checkout's
  TypeScript loader and emits the ordered surface/history projection.
- Added `TestCoreTurnReplayMatchesReference`; with
  `DSH_REFERENCE_ROOT=D:\\dev-projects\\Agent\\deepseek-harness` it passed,
  comparing surface positions, roles, content blocks and tool call identity
  against Go replay. The test skips when the external reference checkout is
  not provisioned, so an ordinary local Go run does not falsely claim this
  gate.

T0 now has executable double-replay evidence for the core fixture. It does not
cover the remaining replacement, protocol, ACP/MCP, Code Mode or persistence
fixture families.

## Latest verified delta (2026-08-28, loop failure-state alignment)

- Request-error recovery now checks the owning context after the recovery hook
  returns; a retry action that races cancellation cannot issue another provider
  request.
- Unsupported provider finish reasons, including `content_filter` and
  `refusal`, now become structured typed failures and may pass through the
  request-error recovery seam instead of silently completing the turn.
- Failed turn boundaries retain the normalized failure code, and pre-step
  refusal is persisted as the reference `blocked` reason.

The loop matrix is better aligned, but the full reference retry policy,
provider error taxonomy, live/durable event dual projection, and cross-surface
fault gates remain open.

The Agent Registry also now has a terminal close barrier: `CloseAll` is
idempotent and later publication is rejected, preventing close/create races
from resurrecting a runtime after dependent services begin shutting down.

## Latest verified delta (2026-08-28, Team task/restore hardening and Code Mode value split)

- Team roster admission now validates lower-kebab names, reserves failed
  identities, enforces the bounded roster, and rejects inactive targets.
- Team task tools now expose the reference transition set (`edit`, dependency
  reset, `reopen`, and `reassign`), start tasks unowned, enforce CAS transition
  rules, reject deleting live blockers, and validate workspace-relative write
  scopes and deployment limits.
- Team cold restore now fails closed on a corrupt newest snapshot and refuses
  to rebind an active teammate whose child Session or parent lineage is absent.
- Code Mode rich binding results now return only their JSON value to the
  TypeScript promise while retaining content/meta/context projection for the
  durable outer dispatch; completion values also consume the output ledger,
  and the subprocess backend now accepts a configured Node old-generation heap
  ceiling.

These are focused contract closures. Team remains snapshot-based rather than
the reference append-only member/task/mailbox event fold, and Code Mode still
lacks the reference worker compute/heap/resource and complete queued-abandon
semantics.

ACP approval requests now carry the originating tool call ID into the shared
approval service. ACP MCP list/call no longer hold the session mutex across
external server I/O, reducing close/call deadlock risk; the MCP client itself
is still not a complete reconnect/task lifecycle implementation.

Team messages now have a runtime dispatch seam: queued snapshots are delivered
through the target Agent when it is live, acknowledged durably on success, and
redelivered on board materialization when a target becomes available. The
queue/dispatch/ack sequence is still not one atomic transaction with the child
Session receipt, so it remains a partial rather than full mailbox equivalence.

## Latest verified delta (2026-08-28, approval transaction and Team event tail)

- SQLite approval resolution now commits the pending-row CAS and canonical
  `approval/decided` audit event in one transaction. A conflicting event rolls
  back the approval state; ordinary compatibility providers retain the explicit
  non-atomic fallback.
- The normal approval path and ACP approval path use the atomic seam when the
  durable backend supports it, then attach the already-committed event to the
  in-memory log without writing it twice.
- Team task revisions and mailbox queue/delivery transitions now have typed
  append-only `team/task` and `team/message/*` events and can be folded after a
  legacy snapshot checkpoint. New application mutations prefer this journal;
  snapshots remain for migration/roster recovery.

These deltas close concrete consistency gaps, but Team membership is still
snapshot-backed, mailbox delivery is not transactionally coupled to the child
Session receipt, and the remaining sandbox, Code Mode resource contract,
ACP/MCP lifecycle, telemetry and final fault/security/performance/race gates
remain open.

## Latest verified delta (2026-08-29, sandbox close quiescence)

- The local provider now publishes active child commands under the same lock
  used by `Close`; shutdown marks the provider closed, kills every active
  command, waits for completion, and only then returns. A start/close race is
  rejected before the child can escape tracking.
- A regression test covers close of an in-flight long-running command and
  verifies both the close call and the run settle.

This closes a concrete process-lifecycle leak. It does not provide the missing
Windows ACL or cross-platform hostile-code enforcement backend.

## Latest verified delta (2026-08-29, Code Runtime output ledger)

- The TypeScript host now accounts for the serialized log-array envelope,
  JSON-string escaping, separators, completion bytes, and failure diagnostics
  instead of charging only raw text lengths. Oversized log entries retain the
  largest UTF-8-safe fitting prefix before returning `output-limit`.
- The public failure constants are centralized in the Code Runtime contract,
  keeping persisted/tool-visible kinds aligned with the reference worker.

This improves output-budget parity but does not close worker-thread compute
accounting, native heap resource limits, or the remaining nested-dispatch
queued-abandon and cross-surface fixtures.

## Latest verified delta (2026-08-28, Team cold projection and observer isolation)

- Team boards now retain and replay durable member rows even without a live
  Agent Registry, so cold inspection does not silently discard roster identity.
  Registry-backed boards still delegate live authorization and handle binding
  to `Roster`.
- A provisioning teammate with a verified durable child Session is rebound on
  recovery and persisted to the active edge; a missing child is persisted as a
  failed identity rather than being fabricated as a live Agent.
- Team delivery commit ordering now writes `team/message/delivered` before
  acknowledging the in-memory queue. A failed delivery-event append therefore
  leaves the already-queued message retryable.
- Session observers are panic-contained for both normal and externally
  committed events; an SSE/telemetry projection failure cannot fail a durable
  append or a model step.

These close concrete recovery and fault-isolation cases. They do not close the
child-Session receipt transaction, full Team content-block mailbox contract,
external telemetry export, or the remaining sandbox/protocol/final-gate work.

## Latest verified delta (2026-08-28, Team dispatch ordering and provider truncation)

- Team live delivery now has a target-local FIFO fence and in-flight message-id
  coalescing. Concurrent sends and recovery attempts cannot reorder injections
  or wake the same message twice within one process; restart ordering remains
  reconstructed from the durable Lead queue.
- The Google streaming adapter now treats EOF before a provider finish reason
  as `STREAM_CLOSED` instead of manufacturing a successful `stop` result.

These are hardening closures with focused regression tests. Cross-process
mailbox transactionality, rich Team content, and the remaining final gates are
still open.

## Latest verified delta (2026-08-29, rich Team mailbox and atomic receipt)

- Team messages now preserve ordered text, reasoning, image, tool-call and
  nested tool-result blocks through the typed journal, `team_message` input,
  target Session receipt and Agent inbox. Legacy string input remains accepted.
- Agent inbox de-duplication now scopes local message ids by Team id, avoiding
  cross-Team collisions when each board starts at `msg-1`.
- SQLite now exposes a multi-session atomic append seam; Team delivery uses it
  to commit target receipt plus Lead delivery, with idempotent compatibility
  handling for the existing Team tool callback.
- Parent cancellation now stops the Agent driver before it claims another
  queued turn, so queued waiters settle as `ErrAgentClosed` deterministically.

The atomic Team path has focused SQLite/session evidence, but JSONL fallback,
cross-process crash injection and the remaining protocol/security/performance
gates are still open.

## Latest verified delta (2026-08-29, ACP/MCP rich result and meter breakdown)

- ACP advertised MCP tools and the dynamic `mcp_call` bridge now expose the
  same canonical `{content, structuredContent}` value shape as the native MCP
  tool. Text/image/resource projections retain ordered rich content without
  leaking image base64 into the transcript; task-required advertised tools
  fail closed because the current foreground bridge has no task lifecycle.
- Compaction now carries the complete meter breakdown (`logRevision`, estimated
  and usage baselines, surface delta, total/surface tokens and priced nodes)
  through the provider-neutral result, while `ShadowedTokens` is selected from
  the same measured nodes.

These are contract closures with focused tests. ACP reconnect/task metadata,
enforcing sandbox backends, cross-process persistence/approval atomicity and
the final fault/security/performance/race gates remain open.

The native dynamic `mcp_call` output schema was also corrected to match its
structured executor result; a registry-level test now verifies that the
canonical rich value survives output-schema validation instead of only passing
when the tool is invoked directly.

## Latest verified delta (2026-08-29, runtime session and storage barriers)

- Agent-backed Web `new`/`resume` now materializes the addressed session
  without changing the legacy REPL's `currentID` or shared log; concurrent
  Agent runs are tracked as a protected session set, so running/pending
  status cannot be overwritten by the last-started session.
- SQLite control-plane mutations (session header/config/title/workspace/CWD,
  archive/reorder/delete, feedback, workspace and settings writes) now share
  the same OS process lock as event and approval transactions. Concurrent
  workspace creation across two database handles has a deterministic unique
  sort-order regression test.
- SQLite session creation can now atomically publish its lineage header and
  seed events; persistence adapters and direct fork use this seam, with a
  rollback test proving that an invalid seed never leaves a half-created
  session.

These barriers close concrete concurrency/crash windows but do not change the
non-equivalence conclusion: legacy global REPL state, enforcing hostile-code
sandbox, complete approval/Team/Code Mode/ACP-MCP lifecycle contracts,
cross-surface fixtures, and final fault/security/performance/race gates remain
open.

## Latest verified delta (2026-08-29, workflow worker boundary)

- Workflow concurrency now follows the reference CPU-adaptive default rather
  than a fixed four-agent default.
- The external workflow runner now scrubs its environment, so vm escape does
  not expose host credentials or loader flags.
- Workflow combinators use a private fatal marker, preventing model-authored
  objects shaped like `{fatal: true}` from bypassing ordinary per-item null
  handling.
- Workflow return values and agent options now reject lossy/non-plain JSON
  values (including dates, cycles, sparse arrays and accessors); default agent
  labels use the first line with the reference 48-rune bound.
- Cancellation of the external runner now resolves a `cancelled` result and
  emits the terminal workflow event instead of leaking a Go context error.
- Code Mode namespace validation now uses the cross-backend ECMAScript/Python
  reserved-word set and reserves the Python exception protocol members.

These changes close concrete T11 protocol and containment gaps. Workflow
worker resource ceilings, true child quiescence/queued-abandon replay, complete
Code Mode compute/wall/output ledger parity, and the global final gates remain
open.

## Latest verified delta (2026-08-29, durable meter replay)

- Token meter now restores the latest provider usage anchor from durable
  `assistant/message.usage` plus the preceding canonical request header, so a
  fresh process does not silently fall back to surface-only pressure.
- Usage accounting uses disjoint input/cache/output buckets and does not count
  reasoning diagnostics twice; an unmetered successful call clears the prior
  provider anchor.
- Empty sessions now expose the reference `baseline: none` projection.
- Text and JSON envelope pricing now counts UTF-16 code units, matching the
  TypeScript estimator for astral characters instead of Go rune counts.

Compaction still lacks the reference's full incremental malformed-event fold,
exact source-chunk usage reconstruction for every legacy shape, and complete
replacement/prune/usage contract fixtures.

## Latest verified delta (2026-08-29, cancellable SQLite coordination)

- SQLite's Unix `flock` and Windows `LockFileEx` acquisition now use
  non-blocking retry loops with context cancellation. Control-plane, event,
  approval, backup and atomic append paths no longer wait indefinitely behind
  another process after their caller has aborted.
- A regression test holds the process lock and proves a cancelled write
  returns `context.Canceled` within the bounded wait.

This strengthens the multiprocess persistence barrier; true two-process crash
injection and the complete JSONL/SQLite corruption matrix remain open.

## Latest verified delta (2026-08-29, public Code Runtime failures and JSONL cancellation)

- Code Runtime now exposes only the reference failure kinds: `exception`,
  `timeout`, `abort`, `worker-exit`, `invalid-output`, and `output-limit`.
  Malformed/unknown worker frames are ignored as hostile extensions; an
  unsettled process is reported as `worker-exit` rather than leaking private
  `protocol`/`substrate` kinds.
- JSONL lock acquisition now uses the same context-aware non-blocking retry
  boundary as SQLite, with regression coverage for cancellation while another
  handle owns the lock.
- Seed headers are checked against the exact seed event count across both
  persistence backends, preventing a fork from publishing contradictory
  lineage metadata.
- The local shell provider now rejects default `workspace-write` execution
  when no enforcing backend is available. Explicit `danger-full-access` remains
  an opt-in escape hatch; this is fail-closed behavior, not sandbox parity.

These changes close public error-taxonomy and cancellation/safety admission
gaps. Cross-platform enforcing sandbox backends, true worker resource limits,
cross-process crash injection, and the final contract/fault/security gates
remain open.

## Latest verified delta (2026-08-29, retry lifecycle)

- Provider retry wrappers now publish a request-context retry observer before
  waiting and immediately before the next wire attempt.
- Session logs now persist `llm/retry` and `llm/retry-started` with a shared
  retry identity, turn/step coordinates, provider failure facts and the
  scheduled delay; cancellation during backoff leaves no false started event.
- The retry defaults now match the reference normal policy's five eligible
  retries, 500ms initial delay, 10s cap and symmetric 10% jitter, and
  `EMPTY_RESPONSE` is treated as retryable.
- Native/Web projections preserve the new lifecycle event and explicit retry
  identity while retaining compatibility with legacy retry payloads.

This closes the previously hidden retry-observability gap, but the Go wrapper
is still a compatibility bridge rather than the full provider-owned policy
model: per-provider retry-policy schema, retry-after handling, always mode,
and exhaustive retry invariant/replay tests remain open.

## Latest verified delta (2026-08-29, cold inspection and crash recovery)

- JSONL and SQLite now share the same session-level interrupted-tail closer
  algorithm, including `TOOL_NOT_STARTED` and `TOOL_OUTCOME_UNKNOWN` result
  synthesis before the boundary closers.
- `Inspect` returns an in-memory balanced logical view without changing the
  durable revision; `ReadFrom(0)` remains a raw, non-mutating physical suffix;
  only `Load` commits recovery closers.
- Durable JSONL revision is advanced after a successful repair, and retry
  lifecycle events are accepted by the native wire-envelope validator.

The full reference persistence normalization/migration matrix, crash injection
across independent processes, opaque revision semantics, and all corruption
cases remain open.

## Latest verified delta (2026-08-29, repeated request-error recovery)

- Loop request-error recovery now re-enters the waterfall after each failed
  finish, including a retry whose next stream-open fails; cancellation wins
  before another provider request.
- Failure facts are refreshed for the next recovery decision, so refusal,
  content-filter and transport failures do not collapse into the first error's
  code in the final durable request/turn record.
- A regression fixture covers two failed terminal finishes followed by a
  successful model response.

Provider-owned retry policy, per-provider retry-after/always behavior and the
full cancellation/abort/max-token matrix remain open.

## Latest verified delta (2026-08-29, canonical session relations)

- Canonical sessions with explicit turn/step coordinates now validate actual
  turn and step numbering, coordinate ownership, and `tool/call` → `tool/result`
  pairing; legacy uncoordinated logs remain readable.
- Synthetic `TOOL_NOT_STARTED` results are explicitly accepted as the one
  valid result without a preceding durable `tool/call`.

The web/native/session schema is still not one generated schema, and exact
unknown-event/migration behavior remains a cross-surface gate.

## Latest verified delta (2026-08-29, Agent-owned runtime log isolation)

- A context carrying an Agent session id now prefers that session's materialized
  runtime log even when the id equals the legacy REPL `currentID`.
- The compatibility fallback to `a.log` is restricted to the same session id;
  an addressed Agent session cannot silently borrow another session's log during
  bootstrap.

The legacy REPL still owns global `currentID`, `log`, tool policy swapping and
`turnMu`; removing that compatibility path remains a P0 blocker.

## Latest verified delta (2026-08-29, disjoint usage buckets and route retry policy)

- Provider-neutral usage now carries separate `cacheReadTokens` and
  `cacheWriteTokens` buckets; DeepSeek/OpenAI Responses/Gemini subtract cache
  hits from prompt input, while Anthropic preserves cache creation as writes.
  The legacy `cachedInputTokens` field remains a read-compatibility fallback.
- HTTP failures preserve status, provider `Retry-After`, and request-id facts;
  the retry wrapper exposes its captured provider policy and supports
  `normal`/`always`, explicit retryable codes, stable policy keys, and bounded
  provider delays.

This is a contract delta with targeted tests, not a parity claim. The retry
executor is still wired as a Go compatibility wrapper rather than the full
reference agent/request-error plugin, and provider-specific policy registration
plus complete retry invariant/replay coverage remain open.

## Latest verified delta (2026-08-29, fork boundaries and meter projections)

- Persistence fork now reads the physical durable transcript and rejects an
  open parent; recovery-generated `interrupted` closers can no longer become
  false child seed history. Team fork uses the same raw-read boundary before
  selecting its closed prefix.
- SQLite `ForkSession` treats an explicit sequence boundary of `0` as a real
  boundary instead of overloading it as “latest”.
- The Go meter now exposes pure replay folds for cumulative per-step usage,
  context pressure, and system/tools/message breakdown. Same-step streaming
  usage is replaced by final usage, prompt pressure excludes output, and the
  projected pressure follows surface growth.

These changes close specific contract gaps only. Opaque source-qualified
revisions, persisted projection checkpoints, full cross-surface schema
generation, and the remaining fault/security/performance gates are still open.

## Latest verified delta (2026-08-29, route retry capture and runtime cancel race)

- Retry configuration now supports provider-route overlays with pointer-valued
  fields, so omitted values inherit the global compatibility default while an
  explicit `max_retries: 0` remains zero. The selected wrapper exposes the
  effective captured route policy.
- Failed model finishes now pass the complete failure facts through the
  request-error waterfall; status, provider retry delay, and request id are no
  longer discarded before recovery policy evaluates them.
- Code Runtime cancellation during the process start admission race resolves
  to the same typed `abort` result as cancellation after process start.

The route overlay is still a Go configuration bridge rather than the full
provider adapter policy object plus `llm-retry` plugin lifecycle, and the race
detector remains unavailable in this Windows environment because CGO has no
compiler toolchain.

## Latest verified delta (2026-08-29, persistence revision and legacy boundaries)

- JSONL and SQLite persistence now expose a source-qualified opaque revision
  token in addition to the legacy numeric sequence. The token is stable for an
  unchanged log and changes after a durable append, so independent stores do
  not accidentally compare equal local sequence numbers.
- Session live append, atomic append, persisted replay, cold validation and the
  native wire boundary consistently reject obsolete `request/header-delta`,
  `mode/set`, and legacy `request/header` fallback encodings.
- The existing cursor path remains non-mutating and bounded: JSONL retains
  only the returned suffix while validating the stream, and SQLite reads
  `limit+1` rows per page to determine continuation.

This does not close the reference migration/normalization matrix: legacy
message/steering upgrades, prepare/commit crash injection, and exhaustive
corruption cases remain open. Full equivalence is still not claimed.

The steering portion is now covered at the runtime boundary: new Agent and
ACP steering deliveries persist as canonical `user/message`, while old
`steering/message` rows replay onto the same user surface. Durable in-place
message identity normalization and the remaining crash/corruption matrix are
still open.

## Latest verified delta (2026-08-29, sandbox path containment and fault evidence)

- Local Code Runtime policy checks now resolve the existing filesystem path
  before applying `Root`/`Cwd` containment, rejecting both an existing symlink
  escape and a missing child below an escaping symlink.
- Direct fault coverage now proves credential-shaped environment entries are
  scrubbed while ordinary entries survive; output quota, timeout, and process
  teardown remain covered by real subprocess tests.

This closes the path-validation evidence gap only. Windows and other hosts
still lack a complete hostile-code enforcing backend, and the reference worker
compute-budget/outer ledger contract remains only partially implemented.

## Latest verified delta (2026-08-29, Code Mode environment boundary)

- TypeScript Code Runtime now starts with an explicitly empty child-process
  environment, matching the reference worker's no-ambient-environment rule;
  Windows' mandatory `SYSTEMROOT` prerequisite is the only platform exception.
- A real subprocess test parses `process.env` and asserts that no host
  credentials, loader flags, proxy settings, or unrelated environment entries
  cross the Code Mode boundary.

Heap limiting, output accounting, cancellation, and nested-call bounds remain
covered only to the extent of the current subprocess implementation; the
reference worker's measured compute budget and complete outer ledger are open.
## 2026-08-29 Agent isolation and durable inbox delta

- Agent-backed Web turns now use a per-session command/turn coordination lock;
  different addressed sessions do not contend on the legacy REPL `turnMu`.
- Native `goal.*` mutations resolve the addressed session log and plan engine,
  with regression coverage proving the REPL selection is unchanged.
- Agent inbox insertion, claim, cancellation, and replay are now represented
  by canonical `agent/inbox/spliced` session events. Journal commit precedes
  live queue mutation, and replay advances message IDs to prevent collisions.
- ACP permission outcomes now route through the shared approval service;
  cancellation wins over late answers and invalid/rogue outcomes resolve as
  `unavailable`.
- Workflow Node cancellation during process admission now settles as a
  resolved `cancelled` result, including the start/cancel race.

Full capability equivalence remains unclaimed: global legacy compatibility
state, enforcing hostile-code sandbox backends, complete provider retry and
cross-process persistence matrices, and the remaining P1/P2 gates are open.

## 2026-08-29 Agent steering and ACP resume delta

- Agent-backed loop cancellation now has a steering-only continuation seam:
  an interrupted provider step can claim the durable steering batch and resume
  the same turn, while ordinary cancellation still closes the turn.
- ACP Agent construction now replays pending `agent/inbox/spliced` events and
  journals later mutations, so resumed ACP sessions retain queued work.
- Added a loop lifecycle regression proving one `turn/start`/`turn/end` pair
  across an interrupted step and its steering continuation.

The remaining lifecycle, sandbox, retry, persistence, Team, ACP/MCP, and
fault/security/performance gates are still open.
## Latest verified delta (2026-08-29, background schedule isolation and delivery dedupe)

- Durable schedule delivery now serializes only the owning session. The scheduler
  ticker and an addressed Agent pre-step cannot advance the same session
  concurrently, while unrelated sessions no longer contend on one process-wide
  run mutex.
- Scheduled Agent follow-ups carry a stable occurrence dedupe key, and the Agent
  inbox applies generic durable-queue deduplication in addition to Team-message
  deduplication. This prevents a retry after a dispatch failure from enqueueing
  the same live occurrence twice.
- Agent steering releases the Agent mutex before invoking the cancellation
  callback, preserving the atomic queue/steer flag boundary without making the
  runner cancellation path wait on Agent state locks.

This still does not make cross-component schedule fire, inbox enqueue, and
scheduler dispatch one crash-atomic transaction; restart recovery and the full
background quiesce matrix remain open.
## Latest verified delta (2026-08-29, Code Runtime budget contract)

- TypeScript Code Runtime now defaults to the reference worker's 60,000 ms
  compute budget, 600,000 ms wall ceiling, 64 MiB combined output ledger, and
  512 MiB Node old-generation heap cap. `maxWallMs` and `maxOutputBytes` are
  validated at the runtime boundary, including the Node timer upper bound and
  the minimum four-byte output ledger.
- Linux and Windows workers poll child CPU time and terminate a synchronous
  hot loop when the busy-time budget is exhausted; waiting on a host binding
  consumes wall time but not the CPU budget. The resolved result is the typed
  `timeout` failure, distinct from external `abort`.

This closes the primary Go Code Runtime budget mismatch, but worker-thread
protocol parity, all-platform CPU accounting, and the complete subprocess
resource/security matrix remain open.

## Latest verified delta (2026-08-29, lifecycle and observability gates)

- ACP now serializes `session/prompt` per session at the protocol boundary.
  Concurrent JSON-RPC requests and reconnect replacement therefore cannot enter
  one Session implementation concurrently; additional directories,
  `mcpServers`, unsupported content blocks, and late permission replies have
  executable rejection/discard tests.
- Code Runtime now fails closed if process CPU accounting is unavailable after
  worker start. A configured compute ceiling can no longer silently become an
  unenforced hint on a host without a supported accounting backend.
- `internal/observability` now exposes a replaceable exporter seam and a
  serialized JSONL exporter. Export errors are returned to diagnostics while
  leaving execution counters unchanged.
- `scripts/equivalence-gate.ps1` is the release gate for format, diff, full
  test, vet, build, CGO race, and reference replay. It intentionally fails when
  `DSH_REFERENCE_ROOT` or a C compiler is unavailable.

These changes add evidence and close narrow failure modes; they do not close
the global legacy bridge, hostile-code enforcing sandbox on every host, full
MCP/ACP task lifecycle, complete Team membership/receipt transaction, or the
remaining reference/fault/security/performance gates.

## Latest verified delta (2026-08-29, provider generation snapshot)

- Provider/config publication now uses one shared in-process barrier. Turn
  assembly reads the configuration and selected provider from the same registry
  generation, avoiding a transient new-config/old-adapter combination during
  live model switching.
- A concurrent snapshot/model-switch regression test covers the invariant;
  this does not replace the unavailable CGO race gate or the secret-redaction
  and credential-lifetime audit.

## Latest verified delta (2026-08-29, Team membership projection)

- `team/member` replay now validates and folds the durable Board projection and
  live Roster together under a deterministic lock order. Rejected immutable
  identity transitions leave both views unchanged.
- The new regression is limited to in-process projection consistency; the
  durable cross-session receipt/authorization transaction remains open.

## Latest verified delta (2026-08-29, subagent completion wake)

- A child settlement now publishes the parent-scoped `subagent/end` event
  before attempting a live parent-Agent follow-up. The follow-up carries a
  stable `subagent:end:<child>` dedupe key and a bounded result summary, so a
  live parent no longer needs to poll `subagent_status` to observe completion.
- Cold parents are not implicitly started; their durable event remains the
  recovery source.

This closes the live child-completion wake seam only. Atomic event-plus-inbox
commit, complete Team receipt binding, and the full background quiesce matrix
remain open.

## Latest verified delta (2026-08-29, job completion projection)

- The Local job completion observer now ensures the owning session has an
  idempotent `job/done` durable event even when no `job_wait` or status reader
  observed the terminal transition, then delivers the bounded completion wake
  to the owner Agent.
- Process-crash recovery between that event and inbox delivery, and the full
  background lifecycle/quiesce matrix, remain open.

## Latest verified delta (2026-08-29, native command parity)

- The native REPL now supports `/feedback <text>` as a log-only
  `feedback/record` operation and `/plan [message]` plus `/plan off` as the
  durable plan-mode controls shared by the Web surface.
- A non-empty `/plan` suffix becomes the ordinary turn text after mode entry;
  the command line itself is not added to model history. Blank slash input is
  rejected explicitly.
- Native and Web resolved commands now emit paired `command/run` and
  `command/done` facts, with unknown command admission remaining
  lifecycle-free; the Web projection also exposes the command name/result
  without raw payload leakage.
- Telemetry sharing disclosure, attachment-admission parity and the full
  cross-entry-point command suite remain open, so this is an incremental parity
  improvement rather than an equivalence claim.

## Latest verified delta (2026-08-29, durable event failure propagation)

- Agent-runtime event sinks for `plan/*`, `workflow/*`, `eval/run` and
  `ralph/run` now return persistence errors to the model-facing tool instead
  of reporting a successful result after the durable append failed.
- Contract tests cover the failure path for workflow, evaluation and Ralph;
  the plan tool uses the same runtime sink propagation path.

This prevents unrecorded success from propagating through these tool results.
It does not make the preceding domain mutation plus event append one atomic
transaction; that transaction boundary remains an explicit open gap.

## Latest verified delta (2026-08-29, Team mailbox crash boundary)

- [x] Team delivery now commits the target Agent inbox splice before the Lead
  `team/message/delivered` acknowledgement. Retries use the target-scoped Team
  message dedupe key, so a crash between those commits preserves recoverability
  without duplicate inbox entries.
- [x] A regression proves the durable order and rejects the former unsafe
  target-user-message-plus-Lead-delivery fast path, which could mark a message
  delivered before any inbox work existed.
- [~] Queue admission, target inbox persistence, Lead acknowledgement and Board
  snapshot are still separate commits; membership transitions and the complete
  cross-process Team transaction remain open.

## Latest verified delta (2026-08-29, feedback identity disclosure)

- [x] `/feedback` acknowledgements now include a lazy, stable per-data-directory
  anonymous identity and explicitly disclose that session sharing is not
  configured in this deployment.
- [x] Feedback `command/run` omits the user text, matching the reference
  `recordInput:false` contract; the text remains only in `feedback/record`.
- [x] Session telemetry now has the reference-compatible default-off,
  `FULL`/`FEEDBACK_ONLY` and `DSH_TELEMETRY_DISABLED` switches. Enabled modes
  batch canonical session observer events into an OTLP/HTTP JSON logs endpoint;
  native, Agent and ACP session sinks share the same exporter.
- [~] The exporter is intentionally bounded and observation-only, but SDK-level
  retry/queue semantics, feedback suffix replay and deployment collector
  contract coverage remain open. Credential lifetime and full redaction policy
  are also still deployment decisions.

## 2026-08-29：后台完成通知的冷恢复边界

- `subagent/end` 与 `job/done` 现在都先作为 authoritative durable fact 写入
  owner session，再通过带稳定 `dedupe_key` 的 Agent inbox splice 投递完成通知。
- 若进程在 terminal event 已提交、inbox splice 尚未提交的窗口退出，session
  materialization 会从 terminal event 补建一次 wake；若 splice 已提交但消息后来
  已被 claim，也不会因为重启而重复补投。
- 这闭合了后台完成通知的一个 at-least-once crash window，并增加了针对子 Agent
  与 job 的 recovery/idempotency tests。它不等于完整后台生命周期等价：跨进程
  owner authorization、telemetry SDK/feedback replay contract、job/subagent
  全量 replay matrix 和 scheduler persistence 仍是开放项。
### 2026-08-29 credential update rollback delta

- Native credential set/unset now treats the durable setting, in-memory
  credential overlay, and live provider registry as one serialized update.
- If the selected provider would become unavailable after a credential change,
  the setting and overlay are restored and the previous registry is rebuilt;
  the change is not reported as accepted. A regression covers selected-provider
  unset failure and verifies both durable and in-memory restoration.
- This does not close the broader credential-lifetime/secret-redaction audit:
  provider instances still hold credentials for their lifetime, and the
  deployment-level secret storage contract remains open.

## Latest verified delta (2026-08-29, generation controls and child inheritance)

- `ChatRequest` now carries optional `maxTokens`, explicit-zero-aware
  `temperature`, and ordered `stop` sequences. DeepSeek/OpenAI-compatible,
  Anthropic, Gemini, and OpenAI Responses adapters serialize the controls in
  their native wire shapes; request headers retain the effective values.
- Spawned children inherit the resolved parent output-token cap, while a
  per-child `agentOptions.maxTokens` override wins. Cold resume reconstructs
  the cap from the durable request header, and `agentOptions.model` is passed
  through to the child request. Provider wire tests and spawn/tool inheritance
  tests cover the behavior.
- Provider-specific remote model catalogs and the remaining full child
  descriptor/continuation matrix are still open.
### 2026-08-29 model terminal workspace/lifecycle delta

The model-owned persistent terminal adapter now treats the addressed session
workspace as its authority boundary: `terminal_open.cwd` is normalized,
symlink-resolved, and rejected when it escapes that workspace. Creation and
close/reset also persist the existing `terminal/start` and `terminal/stop`
facts through the Agent runtime sink, so a successful terminal is not invisible
to durable replay. This closes one T15 host-adapter seam; restartable terminal
state, `/term` compatibility isolation, and cross-platform hostile-code
enforcement remain intentionally open.

Fresh `bash` and `pwsh` calls now receive the same per-session workspace-root
constraint. Explicit `workdir` values are real-path checked before the process
starts, including background jobs; standalone package users can still omit the
optional root hook when they own their own policy.

The generic `job_start` path now avoids copying the command into its default
durable label and redacts explicit labels before they enter job snapshots or
`job/start`; this closes a concrete secret-bearing metadata seam, but does not
complete the repository-wide credential lifetime/redaction audit.

The local filesystem adapter now checks real paths for existing components and
rejects outside or dangling symlink escapes, while its read-before-write
observation map is mutex-protected for concurrent Agent calls. This closes the
ordinary workspace traversal seam; it is still not an OS-level hostile-code
isolation boundary.

`grep` and `glob` now receive the addressed session workspace root in the app
composition and reject explicit outside or symlinked search paths. The
standalone package constructors remain compatible when no session root is
provided.

### 2026-08-29 release-gate and telemetry retry delta

Session telemetry retries transient collector failures (network errors, HTTP
429, and HTTP 5xx) with a bounded three-attempt backoff. Non-retryable HTTP 4xx
responses stop immediately; telemetry remains observation-only and cannot
change the durable turn result. A regression covers two transient failures
followed by a successful export.

The equivalence gate now includes Web tests, production build, native manifest
verification, and CGO-disabled Linux/Windows cross-builds. The strict CGO race
gate remains mandatory; on the current Windows host it is blocked because no
C compiler (`gcc`) is installed, so this environment cannot claim the complete
release gate.

The Code Mode CPU monitor also treats loss of the OS accounting handle after a
worker has already settled or been cancelled as a normal teardown race, while
retaining fail-closed behavior for an accounting failure during live work.

ACP server prompt admission now also enforces its advertised image capability
at the protocol boundary. A rich image session implementation cannot receive
an image block after `initialize` reports `image: false`; the request fails as
invalid parameters before content reaches the session.

### 2026-08-29 lifecycle, meter and observability contract delta

Application shutdown now closes the admission gate before draining Agent,
session, scheduler and title-worker services. New work is rejected
deterministically after the gate opens, while already-admitted work follows the
existing close/drain order.

The meter usage-anchor fingerprint now includes `maxTokens`, `temperature`,
and ordered `stop` controls, and cold replay reads those fields from both the
current request/header shape and the legacy `header.config` shape. Different
generation controls can no longer reuse a provider usage anchor.

Native completion-ledger projection is verified against the Go meter on
committed assistant/tool-result boundaries. Structured `ToolResult` failures
are now counted as tool failures in both serial and parallel loop execution;
durable request-header append failure is covered by a fail-stop regression that
proves no provider request is issued.

### 2026-08-29 attachment admission and ACP contract delta

Attachment admission now verifies the declared raster format, records PNG/JPEG/GIF dimensions, rejects decompression-bomb bounds, enforces default batch count/bytes, and uses a SHA-256 content address with digest verification on reads. Legacy non-content-addressed attachment IDs remain readable for migration compatibility. ACP `session/new` now rejects relative CWDs on every host, including platform-independent Windows drive/UNC forms, and an external-package wire contract test covers protocol version, session creation, and invalid-parameter error codes.

The attachment policy is now deployment-resolved: configured image count,
aggregate bytes, pixel and dimension limits are applied by the shared store
used by CLI/ACP/MCP/Web. WebP admission parses RIFF framing and VP8/VP8L/VP8X
dimensions instead of accepting a signature-only payload. New objects are
written through a synced temporary file and non-overwriting hard-link publish,
so interruption cannot expose a partially-written content-addressed object;
batch rollback removes only files created by that batch and preserves reused
objects.

The process entrypoint now registers its shutdown admission gate after all
dependent cleanup handlers, making the gate the first deferred shutdown action
for both Web and ACP modes. This closes the previously misleading defer-order
claim; OS-level hostile-code enforcement, cross-process storage locking and
strict CGO race execution remain open and are not claimed as equivalent.

An external-package MCP wire contract test now exercises the exported stdio
client through initialize, tool metadata, structured call results and
idempotent close/closed-operation behavior. The remaining MCP task lifecycle
and full SDK reconnect semantics are still outside the implemented contract.

Workspace `read_image` now uses the same attachment admission policy as
stored/uploaded images: the configured byte cap, raster validation, WebP
dimension parsing, pixel and dimension bounds are applied before the image
block is exposed to the model. The parser is centralized in
`internal/attachment` so filesystem and attachment producers cannot silently
diverge.

The loop now emits the reference-compatible `request/context` projection for
each new provider/model/capacity route, deduplicated against the session's
last route context. The event is validated as a turn-scoped core event, folded
by the existing meter, and included in the evidence JSON Schema.

The canonical wire validator now also covers the native product-state facts
that were previously only accepted by internal projections: `feedback/record`,
`plan/create|update|delete|status|list|mode`, and the complete `todo/write`
shape. Invalid state payloads fail closed with the same malformed-wire error
used by core lifecycle events.

The application lock boundary now names its remaining compatibility scope
explicitly: `legacyTurnMu` protects only `currentID/log` session switching and
the legacy REPL turn, while `controlMu` protects provider/config/credential
publication. Agent-owned Web/ACP turns continue through per-session handles,
runtime logs and cloned registries without taking the legacy turn mutex. The
legacy fields and a few compatibility callbacks remain, so this is an
isolation step rather than completion of the global-state removal item.

Shutdown ordering now drains the scheduler and local jobs before closing the
Agent registry, so a late job settlement can still publish its durable
completion wake into the owner inbox. The legacy scheduled-reminder path also
releases its per-session delivery lock when the turn fails; a failed reminder
cannot strand later reminders for that session. Full background quiesce across
every plugin/worker path and a process-crash proof remain open.

The Web conversation and trajectory adapters now consume one shared event-fact
model for identity, timestamps, request/call relationships, event kind, status,
structural classification, text and error state. A cross-projection contract
test covers request context, context messages, failed tool results and opaque
extensions. This removes drift between the two TypeScript projections; the
native Go projection still uses its own typed implementation and therefore the
full generated-schema convergence item remains open.
### 2026-08-29 MCP reconnect supervisor and ACP external wire contract

- ACP now has external-package wire coverage for initialize/version, absolute
  cwd admission, unknown-method error codes, durable session resume/reconnect,
  and text/resource-link/image prompt plus update frames.
- MCP stdio clients expose connection-loss events. Long-lived bridged clients
  use a configurable, close-interruptible exponential-backoff supervisor with
  bounded attempts and a post-reconnect tool-schema resync callback. Failed
  tool calls are not replayed by the supervisor; the model retry policy still
  owns call replay.
- The full MCP task/session/HTTP lifecycle, generated external schema matrix,
  hostile-code sandbox enforcement and cross-process crash/fault evidence
  remain open, so this delta does not justify an equivalence declaration.

### 2026-08-29 Agent-owned context resolver delta

- Agent-owned jobs, plan, subagent and workflow registrations now receive
  explicit `context.Context` owner resolvers. Their model-facing calls resolve
  the addressed session from runtime context first, while the old zero-context
  constructors remain compatibility-only for legacy embedders and tests.
- This removes another direct `currentID` callback from the normal Agent path;
  terminal, session-query, interact and legacy scheduler compatibility paths
  still require the remaining global-state removal work.

### 2026-08-29 approval answerer contract

- CLI, Web and application-created ACP sessions now resolve through one
  application-owned `approvalAnswerer` seam. It performs session-scoped listing,
  ownership checks, serialized CAS, canonical durable decision projection and
  compatibility-only legacy event selection. The focused regression covers
  cross-session denial, Web/ACP canonical resolution and CLI compatibility.
- Directly constructed ACP unit-test sessions still use their private fallback
  engine; this is an intentional test/embedding compatibility path, not the
  production factory path.

### 2026-08-29 jobs close quiescence delta

- `internal/jobs.Local.Close` now waits for both the job terminal signal and
  the completion-observer signal. The latter closes only after the per-job and
  registry observers return, so application shutdown cannot close Agent
  handles while a `job/done` projection is still delivering its owner wake.
- A regression test holds the observer open and proves `Close` remains blocked
  until that observer is released. This closes one ordering edge only; the
  unified quiesce coordinator and equivalent evidence for child Agents,
  schedulers, plugins/terminal resources, session switch and process-crash
  recovery remain open.
### 2026-08-29 plan-mode boundary delta

- CLI and Web Agent-backed `/plan` now share a session-scoped transition: idle
  selections append `plan/mode` immediately; selections made during a running
  turn remain pending until the next accepted Agent boundary, and a message
  suffix steers the active Agent rather than creating a competing next-turn
  run.
- The boundary applies `plan/mode` before runtime/prompt assembly. Successful
  `command/run` + `command/done` records reconstruct an uncommitted selection
  after restart; failed or unfinished command records do not.
- Web Agent-backed `/plan` now accepts image references as rich content and
  uses the same content-preserving run/steer seam. ACP command parity,
  generated plan projection fixtures and full cross-entry priority/replay
  coverage remain open.

### 2026-08-29 Agent maintenance and protocol quiesce delta

- `Agent.RunMaintenance` now models the reference idle-maintenance phase:
  turn claiming is excluded by an explicit claim barrier, queued wakes remain
  durable until the phase settles, cancellation propagates through the task
  context, and concurrent `Close` calls wait for one disposal barrier.
- Native and Web manual `/compact` now execute through that Agent-owned
  maintenance seam when the Agent runtime is enabled. Legacy direct execution
  remains only for compatibility/test construction.
- The MCP reconnect wrapper now tracks in-flight `Start/ListTools/Call`
  operations, serializes starts, and waits for both active protocol calls and
  the reconnect supervisor before closing the underlying client. Reconnect
  never replays a failed model tool call.
- Job completion persistence now commits the owner-scoped `job/done` fact
  before attempting a live Agent wake, including during shutdown admission.

These changes close concrete maintenance and quiesce races; they do not close
the full process-wide background coordinator, cross-process event/receipt
transactions, MCP task/HTTP lifecycle, hostile-code sandbox enforcement, or
the unavailable strict CGO race gate.

### 2026-08-29 close-barrier completion delta

- Schedule, plan, spill and interaction engines now make concurrent `Close`
  callers await provider disposal instead of returning while the first close is
  still in flight. The subagent runtime and spawn/fork provider apply the same
  barrier and continue closing all providers after the first error.
- Plugin and skill registries now drain all mounted/provider cleanup before a
  concurrent close returns. The Web server similarly serializes shutdown and
  waits for the first `http.Server.Shutdown` attempt.
- MCP stdio close now drains its notification dispatcher and connection-loss
  callbacks. Persistent terminal close now reports a bounded timeout and keeps
  a retryable state rather than claiming successful cleanup.

These are resource-level barriers, not proof of a single process-wide
transaction: cross-process event/receipt atomicity, hostile-code enforcement,
MCP task/HTTP lifecycle and the full external fault/security/performance suite
remain open.
#### 2026-08-29 tool value/render separation delta

- [x] The Go tool registry now exposes optional `OutputRenderer` and
  `OutputMetadata` seams. Canonical values are schema-validated before the
  renderer runs; renderer panics, invalid blocks, and renderer errors become
  `INVALID_TOOL_OUTPUT` failures rather than empty success. A post-execute
  accepted value replacement forces a fresh content/metadata projection,
  while an accepted content replacement remains authoritative.
- [~] Tool-owned render-intent cards, presentation metadata persistence and
  migration of every built-in tool to typed output definitions are still open.
#### 2026-08-29 SQLite write-admission delta

- [x] SQLite session creation, Team child provisioning and ordinary/atomic
  append paths now validate event version/type vocabulary before opening the
  durable write. Unsupported `mode/set`, `request/header-delta` and malformed
  event identities therefore cannot become rows that fail only on restart.
- [~] Raw inspection remains intentionally permissive for forensics; true
  process-crash injection, filesystem fault simulation and the complete
  corruption/repair matrix remain open.
#### 2026-08-29 SQLite bounded-reader integrity delta

- [x] `LoadSessionFrom` and `LoadSessionPage` now validate event vocabulary and
  contiguous sequence numbers within the bounded window, including the
  cursor boundary. A deleted/corrupt middle row is rejected instead of being
  presented as a valid reconnect suffix.
- [~] Raw forensic reads, cross-process crash injection and the full storage
  corruption/repair matrix remain open.

#### 2026-08-29 provider retry waterfall delta

- [x] Always-mode provider recovery now gives application request-error hooks
  first refusal/ownership, then falls back to provider retry; terminal stream
  failures follow the same order. Always mode retries non-cancellation model
  failures beyond the normal transient-code allow-list, matching the reference
  policy's downstream recovery contract.
- [~] Full provider-policy schema generation, downstream recovery disposal
  drain and exhaustive retry invariant/replay matrices remain open.

#### 2026-08-29 background-job correlation delta

- [x] The jobs provider now snapshots runtime correlation at registration,
  carries it into the independent background context and exposes it on both
  started and terminal snapshots. Command, PowerShell, subagent, schedule and
  persistent-terminal producers pass Agent/Session/Turn/Step/Request/Call/
  Generation identity from their initiating context.
- [x] The application opens a bounded `job.<kind>` span before the producer
  goroutine starts and closes it from the single settlement observer, including
  failed and killed outcomes; completion delivery therefore cannot erase the
  async trace after the initiating tool returns.
- [~] This remains an in-process bounded recorder: external OTel SDK/span
  processor parity, cross-process job registry persistence and crash recovery
  are still open.

#### 2026-08-29 storage crash-boundary evidence delta

- [x] JSONL normal append and interrupted-tail repair now share one physical
  append transaction boundary. A write, sync or close failure closes the
  writer and restores the exact pre-append byte prefix before returning an
  error; recovery cannot create a second corrupt tail.
- [x] Child-process tests prove JSONL torn-tail recovery, interrupted-turn
  closure, cross-process lock ownership and deadline cancellation. A SQLite
  child-process test proves an uncommitted transaction is absent after reopen
  while committed data remains available and `integrity_check` passes.
- [~] These are process-exit and lock/transaction boundary proofs, not a full
  kill-at-every-write fault injector. Filesystem fault simulation, backup
  restore verification and the complete JSONL/SQLite corruption matrix remain
  open under T4/T17/T21.

#### 2026-08-29 tool partial-failure preservation delta

- [x] Registry execution no longer discards a structured result when a
  `ResultExecutor` or execute hook returns `(partialResult, error)`. Validated
  rich content, metadata and deferred context messages survive as a failed
  tool result, while the canonical value is withheld and the stable execution
  error code is applied.
- [~] This closes one output-loss seam; complete built-in producer migration,
  renderer-owned intent cards and cross-transport source/event fixtures remain
  open.

#### 2026-08-29 Team member transition transaction delta

- [x] SQLite Team provisioning now requires an explicit `provisioning` member
  event. A separate serialized transition seam validates the child session's
  immutable parent, root sequence, prior member phase and exact event identity;
  repeated submission of the same committed transition is idempotent.
- [~] The live Agent Registry publication cannot participate in the SQLite
  transaction, and the complete cross-process membership/receipt/
  authorization service remains open.

#### 2026-08-29 MCP Streamable HTTP lifecycle delta

- [x] The Go MCP seam now selects stdio or Streamable HTTP per server. HTTP
  performs initialize/initialized, propagates `Mcp-Session-Id`, drains paginated
  `tools/list`, calls `tools/call`, consumes JSON/SSE responses and dispatches
  `notifications/tools/list_changed`; Close best-effort deletes stateful MCP
  sessions.
- [x] REPL dynamic selectors, static bridged tools and ACP-owned MCP clients
  use the same selector. Reconnect starts a fresh protocol session and leaves
  failed tool-call replay to the outer tool retry contract.
- [~] Task-required tools remain intentionally rejected to match the current
  reference bridge. Full SDK replay, server-to-client callback responses,
  credential rotation and cross-process MCP ownership remain open.

#### 2026-08-29 Agent disposal and MCP error admission delta

- [x] `jobs.CloseOwner` now closes admission for the exact Agent owner,
  cancels its jobs, waits for job and observer settlement, and removes only
  that owner's records. Agent/ACP/Team scopes register this cleanup together
  with persistent model-terminal and approval-policy cleanup.
- [x] Persistent model terminals reject lookup after owner disposal, emit a
  durable `terminal/stop`, and have a regression proving a foreign owner's
  terminal remains running. Session title generation resolves the addressed
  session's provider/model; Web search logging fails closed without its log.
- [x] MCP `isError` results no longer persist image blocks. Dynamic, bridged
  and ACP MCP surfaces now return the same structured `MCP_TOOL_ERROR` result
  while retaining raw protocol content for programmatic callers.
- [~] Provider-generation leases, Team cross-session atomic receipt, hostile
  sandbox enforcement and the complete cross-surface contract gates remain
  open.

#### 2026-08-29 workflow worker admission delta

- [x] The external Node workflow runner increments `maxTotalAgents` before
  the first await, so concurrent queued calls cannot bypass the per-run total
  cap.
- [x] After a workflow result or host cancellation, the Go host cancels
  dispatched agent callbacks, drains them for a bounded grace period, and
  rejects late JSONL writes. The application adapter cancels a child run when
  its Result wait is aborted, preventing fire-and-forget children from
  outliving the workflow terminal boundary.
- [~] Reference worker-death/grace `agent-end` synthesis, explicit
  pending-start and child-disposal receipts, and hostile protocol replay
  still require dedicated cross-process contract tests.

#### 2026-08-29 approval disposal delta

- [x] Approval engines now expose an optional session cancellation seam. Agent
  disposal cancels only that session's pending requests, while private ACP
  approval engines mark pending requests unavailable before closing.
- [~] Shared application cleanup records the canonical cancelled decision, but
  cross-process approval/event atomicity and the complete answerer replay
  matrix remain open.

#### 2026-08-29 workflow lifecycle boundary delta

- [x] `workflow/start` and `workflow/end` are now host-owned rather than
  worker-script-owned. Start is observable even when Node cannot launch; end
  is emitted after host callback drain and stranded-agent pairing on terminal
  paths.
- [x] Worker cancellation/exit tracks workflow agent starts and emits one
  synthetic cancelled `workflow/agent-end` for each unpaired start.
- [~] Full worker-death admission protocol, pending-start receipts and
  cross-process lifecycle replay remain open.

#### 2026-08-29 Code Mode boundary correction delta

- [x] Code Runtime now treats an omitted return/undefined as a successful
  completion without a value, preserves host nil as JavaScript null, and
  converts a non-lossless host binding result into a catchable binding error
  instead of a worker-exit transport failure.
- [x] Output-limit failures reserve space for their diagnostic and trim the
  retained log prefix to the same serialized budget, including small-cap
  diagnostics.
- [~] This closes concrete value/diagnostic accounting mismatches only;
  worker-thread resource semantics, hostile-peer replay, and the complete
  nested dispatch lifecycle remain open.

#### 2026-08-29 Agent provider-route pinning delta

- [x] Agent turn assembly now captures the selected provider instance and
  passes it into the loop. Requests for that same route remain on the captured
  instance even if Web publishes a new registry generation during the turn.
- [~] Retiring old generations still needs an in-flight usage lease and close
  barrier before credential material can be wiped safely.

- [x] Code Runtime host admission now ignores duplicate child call frames, so
  forged/replayed stdout cannot repeat a host-side tool side effect.
- [x] Node strip/parser failures are classified as program `exception` results
  from stderr instead of being exposed as worker-exit transport failures.

#### 2026-08-29 provider-generation lifetime delta

- [x] Published provider registries now have ref-counted generations. Agent
  turns hold a lease until their assembled runtime is restored; stream-based
  consumers release after EOF/error, and retired generations close exactly
  once after the final lease.
- [x] Long-lived subagent/title/eval/compaction consumers resolve their route
  at operation start, so a model switch does not retain stale credential
  generations or strand later requests on a retired adapter.
- [x] The application now supplies a context-aware per-operation credential
  provider to every production LLM adapter; rotation is visible without
  rebuilding long-lived consumers.
- [~] Cross-process credential leases and the complete Web authorization matrix
  remain open; provider-catalog and MCP projection redaction are covered by
  the later Web projection delta.

#### 2026-08-29 MCP/Web secret projection delta

- [x] Web MCP inventory and refresh responses now redact configured header
  values while retaining header names and empty-value shape for diagnostics.
- [x] Web MCP inventory also masks credential-shaped command arguments and URL
  userinfo/query values; masked values are restored on update so redaction is
  non-destructive. Provider catalog views and MCP refresh diagnostics contain
  no configured credential values.
- [~] The full Web mutation authorization matrix still requires independent
  cross-surface tests.

#### 2026-08-29 per-operation credential resolution delta

- [x] LLM adapters now accept an optional context-aware credential provider;
  `Available` and every `Stream` snapshot the current credential at operation
  start, while generation retirement still wipes adapter-owned bootstrap
  material.
- [x] The application wires that seam to the locked settings/environment
  resolver for DeepSeek, OpenAI-compatible, Anthropic, Gemini and Responses
  routes; rotation is visible without rebuilding long-lived consumers.
- [~] Cross-process credential leases, complete rotation behavior and the full
  Web authorization matrix remain open; provider-catalog and MCP projection
  redaction are covered by the later Web projection delta.

#### 2026-08-29 runtime-context overlap and Web MCP projection delta

- [x] Agent-owned approval, compaction, plan and subagent projections now keep
  using the runtime log even when an Agent id overlaps the legacy `currentID`.
- [x] MCP Web responses mask credential-shaped arguments, URL userinfo/query
  values and configured headers; refresh diagnostics use the same configured
  secret projection, and masked updates restore the stored values.
- [~] Web per-user/session authorization, cross-process lifecycle semantics,
  enforcing sandbox parity and the strict race gate remain open.

#### 2026-08-29 Agent runtime fail-closed overlap correction

- [x] Runtime-context log fallback now requires the absence of an Agent
  Registry; an Agent-owned session whose durable log is not materialized no
  longer borrows the legacy `currentID/a.log`.
- [x] Native goal mutation applies the same rule, with a focused regression
  covering an overlapping Agent/native session identifier.
- [x] Static MCP bridge calls now emit the canonical `mcp/call` event through
  the Agent runtime sink, with legacy-log fallback only for legacy callers.
- [~] The broader migration of legacy REPL state and the remaining sandbox,
  cross-process lifecycle and strict race gates remain open.

#### 2026-08-29 fork parent-boundary and durable-seed correction

- [x] Fork resolves an addressed live parent log instead of silently treating
  a real Agent parent as fresh; only the explicit legacy compatibility path may
  use the host log.
- [x] Fork copies only the completed prefix through the last `turn/end`, so an
  in-flight parent turn cannot enter a child seed and invalidate replay.
- [x] SQLite child creation uses `CreateSessionWithEvents` for lineage/header
  plus seed, with a tested fallback for older Store implementations.
- [x] Child resume records the existing transcript watermark, preventing a
  no-content resumed turn from returning a stale prior answer.
- [~] Full Agent publication/disposal receipts, cross-process nested-worker
  recovery and the remaining Team/ACP/Code Mode lifecycle gates remain open.

#### 2026-08-29 child id cold-start correction

- [x] Spawn/fork providers now inspect durable session metadata before allocating
  a child id, so a new provider instance continues past existing `spawn-N` /
  `fork-N` sessions instead of replaying the first id. A restart regression is
  covered.
- [~] This is a cold-start collision fix; atomic reservation against a truly
  concurrent second process still requires a backend-level id allocation seam.

#### 2026-08-29 Agent memo disposal correction

- [x] The app-side `sessionAgents` memo is now removed by the Agent scope
  cleanup when Registry.Close disposes a handle. Web/Native materialization can
  therefore recreate a live runtime after parent-close or explicit disposal,
  rather than reusing a closed handle. A focused lifecycle regression passes.
- [~] Cross-process Agent publication and full disposal receipt replay remain
  open.

#### 2026-08-29 release-gate rerun

- [x] `scripts/equivalence-gate.ps1` passed diff check, full Go tests, vet,
  build, Web tests/build/manifest verification, and Linux/Windows cross-build.
- [ ] The final strict race stage still fails before compilation because this
  Windows host has no `gcc` for `CGO_ENABLED=1`; the gate correctly refuses to
  publish a capability-equivalence result.

#### 2026-08-29 child start cancellation correction

- [x] Synchronous Spawn/Fork durable creation now honors the caller context;
  cancellation during header/seed publication aborts before runtime-index
  publication. A blocked-store regression covers the boundary.
- [~] Legacy fallback stores still cannot make header/seed/domain mutation one
  cross-store transaction; the cross-process lifecycle gate remains open.

#### 2026-08-29 reference replay verification

- [x] With `DSH_REFERENCE_ROOT=D:\\dev-projects\\Agent\\deepseek-harness`,
  `go test ./internal/session -run TestCoreTurnReplayMatchesReference -count=1`
  passed. This proves the checked-in core fixture replay only; it does not
  close the full tool/approval/Team/ACP/MCP/SDK fixture matrix.

#### 2026-08-29 Web Agent command isolation correction

- [x] Agent-backed Web `/goal` now clones the global registration only as a
  definition source, applies the addressed session's strict runtime policy and
  owner, and executes `create_goal` through that scoped view. A regression
  proves a globally denied registry cannot make the addressed Agent command
  fail or write into the legacy session.
- [x] A stale closed `sessionAgents` memo is rejected and replaced from the
  durable session, in addition to the normal scope-cleanup path.
- [~] Web capability inventory, mutation authorization and the complete
  cross-surface command contract remain open.

#### 2026-08-29 usage-anchor provenance correction

- [x] Final loop assistant boundaries now persist `sourceEventSeqs` for the
  streamed text/reasoning events that produced them; known empty streams use
  an explicit `[]`, while legacy/unknown provenance remains omitted.
- [x] Meter replay and live usage anchors use the cited provider chunks rather
  than blindly pricing a rewritten durable assistant surface; request-header
  fingerprinting canonicalizes nil versus empty wire arrays.
- [x] Regression coverage proves a durable assistant rewrite produces a signed
  surface delta instead of silently becoming the provider anchor; session,
  meter, loop and application tests plus vet pass for the affected packages.
- [~] Full request-context capacity, retry/provider usage matrix, compaction
  and all Web/ACP/SDK projection contracts remain open.
- [x] Compaction replacement markers now persist every shadowed surface seq in
  `sourceEventSeqs`; a regression checks the complete range is cited.
- [x] Approval resolution uses `ListForSession` when the provider supports it,
  keeping the candidate lookup scoped before the durable decision transaction.
- [x] Session live/atomic/persisted/replay paths validate present provenance
  references and compaction shadow coverage, with malformed-source regressions.

#### 2026-08-29 command lifecycle contract correction

- [x] Session command rows now enforce unique `command/run` IDs, one matching
  `command/done`, `success|error` result kinds, and valid earlier non-command
  `sourceEventSeq` provenance across append, atomic, persisted and replay
  paths. Open runs remain replayable as crash tails.
- [~] Complete command registry parity across CLI/Web/ACP/SDK, authorization,
  external-client lifecycle and full replay fixtures remain open.
- [x] Native `commands/execute` now derives its response from the committed Web
  `command/run`/`web/command-result`/`command/done` rows, preserving the real
  ID, result text, kind and optional source sequence.
- [x] Native command discovery no longer advertises Web model-turn skills, and
  `/plan` is the only command advertising image input; Web rejects undeclared
  command images before entering model history.
- [x] TypeScript Code Mode bounds stderr and cancels/drains oversized raw
  stdout frames as a typed worker-exit result, with a regression proving the
  worker cannot strand the host on a full pipe.
- [x] Successful Web command lifecycle rows now link `command/done` to the
  committed `web/command-result` sequence through `sourceEventSeq`.
- [x] Production Web queue callbacks now enforce durable addressed-session
  existence for list/enqueue/edit before touching queue state.

#### 2026-08-29 ACP admission and Team receipt retry correction

- [x] ACP rich-prompt admission now owns the prompt slot and cancellation
  context before decode/attachment persistence; cancelled admission returns
  `cancelled` without queuing a late Agent message.
- [x] Team delivery retry recognizes matching durable `agent/inbox/spliced`
  insertions, including already-claimed receipts, so a missing root delivery
  acknowledgement cannot duplicate the target message.
- [~] These are in-process retry/cancellation corrections; cross-process
  receipt races, complete Team transaction tests, and the remaining lifecycle
  gates are still open.

#### 2026-08-29 native rich-prompt batch correction

- [x] Native `session.prompt` now checks the addressed session before image
  persistence and sends all inline images through `attachment.SaveImages`, so
  invalid later content cannot leave an earlier attachment object behind.
- [~] Multimodal capability routing and the complete Web/native authorization
  matrix remain open.

- [x] Native rich prompts now consult a composition-root resolver for the exact
  session provider/model image capability before calling `SaveImages`.

#### 2026-08-29 Team cold-directory correction

- [x] Team control-plane directory discovery now includes persisted session
  roots carrying Team facts, so a restarted process can find lazy boards that
  are not present in its in-memory cache.
- [~] Cross-process member authorization and atomic receipt transactions still
  require backend-level proof.

#### 2026-08-29 approval reconnect-owner correction

- [x] Session-scoped approval resolution no longer rejects a durable request
  merely because its correlation map was created in another process; the
  provider listing proves the owner before resolution.
- [x] Native question cancellation can recover that owner after reconnect via
  a session-id-only resolver, without exposing approval contents.
- [~] Cross-process approval/event transactionality and the full answerer
  replay matrix remain open.

#### 2026-08-29 MCP reconnect-policy validation correction

- [x] Config-file loading now validates explicitly supplied MCP reconnect
  delays and attempt budgets before applying omission defaults. Zero/negative,
  non-integer, and reversed delay values fail closed instead of silently
  changing the reconnect policy.
- [~] Per-server MCP timeout/startup policy, fresh-generation disposal
  barriers, task lifecycle, and the complete external SDK replay suite remain
  open.

#### 2026-08-29 MCP subprocess credential-boundary correction

- [x] MCP stdio children now receive the scrubbed ambient environment before
  explicit embedding/test variables are appended; credential-shaped names are
  not inherited merely because a server is launched by the Agent.
- [~] Explicit MCP env/cwd policy, per-server timeout/startup semantics,
  fresh-generation disposal barriers, task lifecycle, and external SDK replay
  remain open.

#### 2026-08-29 MCP optional-startup correction

- [x] MCP server entries now expose `fail_on_startup_error`; the default
  startup path logs an unavailable optional server and keeps dynamic recovery
  tools alive, while explicit `true` preserves strict startup failure.
- [~] Per-server env/cwd and call-timeout policy, fresh client-generation
  disposal, task lifecycle, and external SDK replay remain open.

#### 2026-08-29 MCP reconnect disposal cancellation

- [x] Reconnect supervisors now own a cancellation context for background
  generation startup; application close cancels an in-flight factory/start
  operation before waiting for the supervisor and active operations to drain.
- [~] A generic client that ignores context during `Start`/`Close` still needs
  the full bounded generation-close barrier and task lifecycle contract.

#### 2026-08-29 MCP fresh-generation reconnect correction

- [x] Long-lived stdio bridges can now supply a generation factory. On
  reconnect the supervisor starts a fresh MCP client, swaps it only after a
  successful handshake, retires the old client, and the regression proves the
  old generation is closed before recovery is reported.
- [~] The complete SDK close-timeout barrier, per-server env/cwd and timeout
  schema, MCP tasks/callbacks, and external replay suite remain open.

#### 2026-08-29 MCP per-server process policy correction

- [x] MCP configuration now preserves per-server `env`, `cwd` and
  `tool_call_timeout_ms`; the default stdio factory applies the scrubbed
  ambient environment plus explicit entries, launches in the configured
  directory, and bounds `tools/call` independently of handshake/discovery.
  Streamable HTTP applies the same call-timeout contract. ACP, dynamic MCP and
  long-lived bridge construction all use the same configured server shape.
- [~] Web editing/redaction for these new fields, MCP task lifecycle,
  generation close timeout barriers and the external SDK replay suite remain
  open.

MCP client construction now validates the server namespace and transport before
invoking either the legacy or configured factory, so programmatic callers
cannot bypass the same fail-closed server contract used by the application.

The subsequent release-gate rerun passed the full Go suite, vet/build, Web
tests/build/manifest verification and Linux/Windows cross-build. The strict
race stage remains blocked by the host toolchain (`gcc` is unavailable).

The fresh-generation lifecycle regression now reads test-double counters under
their mutex, so that proof remains race-safe when the host compiler/toolchain
is available.

#### 2026-08-29 Code Mode worker-exit classification correction

- [x] TypeScript worker CPU-accounting loss now cancels the worker without
  overwriting a process's already-observable syntax/exit classification; the
  regression remains an exception instead of becoming a flaky
  `compute-accounting-unavailable` result under suite load.
- [~] True worker resource enforcement, queued-abandon semantics, and the
  complete cross-platform process matrix remain open.

The local shell provider now also treats the Windows `StdoutPipe` close
observed after a successfully reaped process as normal shutdown, preventing a
completed full-access command from becoming a spurious transport error. Other
pipe faults remain surfaced.

#### 2026-08-29 MCP identity-stable status correction

- [x] Web MCP inventory/refresh now resolve live clients by server name rather
  than successful-connection array position; an optional failed server no
  longer shifts every later server's `connected` status.
- [~] Per-server env/cwd and call-timeout policy, fresh client-generation
  disposal, task lifecycle, and external SDK replay remain open.
## Latest implementation correction: MCP timeout configuration validation (2026-08-29)

The YAML loader now rejects an explicitly configured MCP
`tool_call_timeout_ms` that is zero, negative, non-integer, or out of range,
while preserving the legacy programmatic zero-as-omitted default. This keeps
the durable per-server call deadline fail-closed and replay-stable. The full
MCP lifecycle, external SDK replay, and final equivalence gates remain open.

#### 2026-08-29 SDK and runtime-owner hardening

- [x] Added the independent `--sdk` newline JSON-RPC runtime with durable
  prompt receipts, initialized route/cwd/maxTokens, session event envelopes,
  session idle status, subagent lifecycle notifications and idempotent
  shutdown. SDK content admission validates the whole batch before session
  creation and checks image capability against the durable session route;
  SDK maxTokens is inherited by host-owned local children unless explicitly
  overridden.
- [x] The event hub now supports a process-wide SDK subscription carrying the
  originating session id, so child-session events are not lost after a prompt
  creates a new session tree node.
- [x] Agent-owned Team and shell context owners no longer use `currentID` as
  an implicit fallback when the Agent registry is mounted; missing runtime
  identity fails closed.
- [~] The Go typed client now covers request cancellation/timeout, structured
  response errors, notification lineage scoping, receipt-to-idle collection and
  the full shutdown/reap ladder. Reference-driven protocol fuzz/replay, reverse
  request/callback matrix beyond the basic callback surfaces, hostile child
  exit matrices and complete shared ACP/MCP/SDK lifecycle fixtures remain open.

#### 2026-08-29 Go external SDK client lifecycle delta

- [x] Added `internal/sdkclient`: a transport-neutral newline JSON-RPC
  transport plus a subprocess-owning typed client. Requests preserve response
  error code/data, abandon timed-out pending entries, honor context
  cancellation, and leave the server-side work running exactly like the
  reference SDK contract.
- [x] Notification subscriptions queue delivered frames, fail closed on
  runtime death, and support client-side session-tree filtering from
  `subagent.started` lineage edges. The high-level `Harness.Run` subscribes
  before prompting, waits for its durable `agent/inbox/spliced` receipt, and
  collects root events plus descendant notifications through the next root
  idle state.
- [x] Runtime teardown requests bounded `shutdown`, closes stdin, waits for
  cooperative EOF exit, escalates on POSIX through graceful termination, then
  force-kills and reaps. Real-child tests cover cooperative shutdown, EOF
  fallback and forced reaping; an in-process `--sdk` contract test drives the
  actual server through initialize/shutdown over the shared client transport.
- [x] Verification: `go test ./internal/sdkclient`, targeted SDK/client tests,
  `go test -count=10 ./internal/sdkclient`, `go vet ./internal/sdkclient
  ./cmd/sta`, and full `go test ./...` across all packages. The full run's
  three Code Mode failures were sandbox denials on the pre-existing user-level
  workspace; an approved rerun of exactly those tests outside the workspace
  sandbox passes. `go test -race` remains unavailable on this host because CGO
  is enabled but no Windows gcc is installed.

#### 2026-08-29 SDK callback and hostile-frame delta

- [x] The shared line transport now answers server-to-client requests. With no
  installed handler it returns JSON-RPC `-32601`; an installed handler receives
  object-normalized params, can return raw JSON, and structured
  `ResponseError` code/data round-trips to the peer. Handler panics map to
  `-32603` instead of killing the reader.
- [x] Added deterministic hostile-frame and mutation coverage: malformed JSON,
  blank/null frames, response-only frames, unknown response ids, valid
  notifications, reverse requests, cancellation without frame emission, and
  300 seeded mutated response frames followed by the real response. A native
  `FuzzLineTransportFrame` seed corpus also exercises reader termination on
  arbitrary bytes without panics.

#### 2026-08-29 SDK reference replay and launch-boundary delta

- [x] Replayed all four checked-in reference notification fixtures through the
  Go subprocess client: text turn, bash tool, persistent tools and in-process
  subagent. The comparison covers methods, params, receipt-to-idle ordering,
  root final response and descendant lineage restoration from the sanitized
  snapshot's subagent boundaries.
- [x] The replay caught a real wire mismatch: reference `SessionEvent.time`
  is numeric epoch milliseconds, while the Go runtime emitted RFC3339 text and
  the Go client decoded `time.Time`. Both SDK boundaries now use `int64`
  milliseconds, with the server wire test enforcing the numeric representation.
- [x] Added hostile process/frame gates: a child that exits 9 after writing
  stderr fails subscriptions with a typed closed error carrying exit code and
  bounded stderr; a line over the 4 MiB scanner cap fails pending requests and
  ends the reader. Launch binding now proves exact child cwd, replacement env
  semantics (an inherited marker is absent), configured request timeout, and
  subsequent force/reap cleanup.
- [x] Added a shared SDK protocol JSON Schema used by Go tests. It accepts all
  reference notification fixtures and every client request method, validates
  typed result shapes, and rejects invalid event timestamps, lifecycle
  vocabulary, lineage fields, prompt blocks, token bounds and shutdown results.
  The schema is generated from the reference TS types and includes source
  hashes; setting `DSH_REFERENCE_ROOT` also regenerates it in a Go test and
  compares the checked-in artifact byte-for-byte.

#### 2026-08-29 SDK server prompt settlement delta

- [x] The actual Go `--sdk` server is now driven through an external client
  transport for a full prompt, not only initialize/shutdown. The contract uses
  SQLite, the process-wide event hub, an Agent runner and a scripted provider,
  then verifies the durable inbox receipt, streamed root events, committed
  assistant message, terminal idle status, persisted event count and shutdown.
- [x] The integration exposed an Agent state-machine race: an inbox item could
  be claimed while public status was still idle and before `running` was
  published, letting `WhenIdle` return in that gap. Agent now publishes a
  claim barrier atomically with durable inbox claim under the same state lock;
  a gated journal regression holds the claim boundary open while `WhenIdle`
  contends for the lock.
- [x] The integration also exposed notification reordering: asynchronous event
  forwarding could let `session.status` overtake its causal durable events. SDK
  records now track the last forwarded event sequence per session; running and
  idle statuses wait for that barrier and share a global notification ordering
  lock, so wire order preserves receipt → running → events → idle.
- [x] The reference replay also removed the non-reference event-level
  `version` field from SDK `session.event` envelopes while preserving numeric
  epoch-millisecond `time`; the wire contract test rejects the leaked field.

#### 2026-08-30 SDK generated-schema and ContentBlock delta

- [x] Added `tools/generate-sdk-protocol-schema.mjs`, a TypeScript AST-driven
  generator that follows the reference protocol imports into LLM, session,
  subagent and attachment types. The generated artifact records every source
  hash, and the Go schema test regenerates it into a temporary file to prevent
  silent checked-in drift. JSON Pointer escaping covers the `session/prompt`
  method definition.
- [x] The generated schema exposed that the Go SDK had invented a base64 image
  prompt shape instead of the reference durable `ImageAttachmentRef`. The Go
  client and server now model the full known ContentBlock union: text,
  reasoning, image attachment ref, tool-call and tool-result. Reference image
  refs resolve through the local attachment store and retain a filesystem path
  only inside the runtime; direct base64 admission remains a compatibility
  extension.
- [x] The reference ContentBlockMap merge-extension gap was subsequently closed
  with an opaque raw carrier through SDK admission, Agent inbox, session wire
  replay and LLM runtime content.

#### 2026-08-30 SDK callback and hostile-runtime delta

- [x] The subprocess-owning Go Client now exposes `OnRequest` before runtime
  start. A real child runtime issues a reverse request during initialize; the
  parent receives the normalized params, returns JSON, rejects handler
  replacement after start, and still shuts down cleanly.
- [x] Reverse-request failure semantics now cover the no-handler `-32601`
  default, structured error code/data preservation, and handler panic mapped
  to `-32603` without terminating the reader goroutine.
- [x] Added a live-child transport-loss case: the runtime closes stdout while
  sleeping, the parent request fails closed, and Close still performs bounded
  shutdown/EOF escalation and force-reap rather than leaking the process.

#### 2026-08-30 ContentBlock extension delta

- [x] `llm.ContentBlock` now has an immutable raw extension carrier and default
  reference-compatible JSON: lower-case `type`, `attachmentId` image metadata,
  tool-call/result vocabulary, and no runtime-only path/Go-field leakage.
  Unknown ContentBlockMap extensions decode into `Raw` instead of being erased.
- [x] SDK ContentBlock unmarshalling preserves unknown entries verbatim. Server
  admission forwards them into the Agent inbox; session wire conversion and
  restore return the same bytes after replay. The generated schema admits an
  unknown tagged block while rejecting malformed known text/image/tool blocks.
- [x] Coverage proves the chain at both boundaries: SDK JSON → server decode →
  LLM raw block, and user/message durable JSON → restore → derived history.

#### 2026-08-30 shared protocol lifecycle and hostile-child delta

- [x] Added `contractfixture.ProtocolLifecycleFixture`, a checked-in
  transport-neutral rich-session-tool-turn scenario. External ACP, MCP stdio
  and SDK tests now consume the same workspace, session, message id, prompt,
  tool arguments/output, assistant settlement and stage list instead of
  maintaining three unrelated hardcoded lifecycle samples.
- [x] The ACP consumer uses a session publication barrier before prompting;
  MCP tool listing/call and result metadata come from the shared fixture; SDK
  validates and decodes the same prompt through its generated schema and
  server admission boundary.
- [x] Hostile-child coverage now includes malformed/noise frames before a valid
  response and a live child emitting an over-4MiB frame. The first recovers the
  response; the second fails pending requests closed and is force-reaped by
  Close. These complement stdout loss, intentional exit, EOF fallback and
  callback panic/error paths.

#### 2026-08-30 MCP task-support audit correction

- [x] Audited the reference MCP bridge: `execution.taskSupport: "required"` is
  deliberately rejected before `callTool`; the reference does not expose a task
  lifecycle API for that mode. The earlier task-list wording treated this as a
  missing task implementation and has been corrected.
- [x] Added a Go regression proving the bridge fails required-task tools before
  any MCP transport call. Streamable HTTP session lifecycle coverage already
  proves initialize, initialized notification, session/protocol headers, list,
  call, SSE notification, DELETE close and fresh-session reconnect.
- [x] Extended Streamable HTTP fault coverage: SSE multi-line `data:` assembly,
  list-changed notification handling, unmatched request IDs, malformed SSE
  rejection, and a malformed 200 JSON body that leaves the existing MCP session
  usable without an implicit reconnect.

#### 2026-08-30 ACP resume metadata and replacement barrier

- [x] ACP `session/resume` and `session/reconnect` now expose an optional
  durable metadata extension. The production session returns workspace,
  provider/model/effort/mode composition plus the restored event cursor and
  next-turn boundary, while unknown metadata remains optional for embedders.
- [x] Unknown resume targets are now classified as invalid request parameters
  through `acp.ErrSessionNotFound` instead of opaque `-32603`; a replacement
  runtime is closed if publication fails, and the old live runtime is retained.
- [x] Runtime replacement is serialized and refuses to swap a session while
  its prompt is in flight (`-32000` `ACP prompt is in flight`). Regression
  coverage spans server dispatch, external wire metadata, and the real
  SQLite-backed ACP factory.

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
  reference replay fixture. The equivalence claim remains disabled; hostile
  sandboxing, cross-process domain receipts, full external replay,
  authorization, telemetry isolation and strict race/fault/security gates
  remain open.

## Latest implementation correction (2026-08-30, durable identity reservation)

- SQLite schema version 2 adds a durable `(namespace, id)` reservation table;
  duplicate claims are rejected under the existing cross-process SQLite lock
  and survive process restart.
- Session/fork, subagent child, Team task/message and application-wired Local
  job generation now claim IDs before publication with bounded retries.
- This is a generated-ID seam only. Team member identity reconciliation,
  domain receipt atomicity and full cross-process Agent/Team/job/terminal
  recovery remain open; the manifest stays `fail` / `claimAllowed: false`.

## 2026-08-30 executable audit register

- Added `docs/equivalence-task-register.yaml` as the machine-readable companion
  to this narrative checklist. It decomposes A0-A9 into implementation,
  acceptance, command, evidence, dependency, and release-blocker fields.
- The register records the current honest state: only the already repaired
  catalog/MCP/fork items are `done`; the cross-process, sandbox, owner,
  credential, external protocol, projection, telemetry, and strict race gates
  remain `partial` or `open`.
- `scripts/equivalence-gate.ps1` now validates that register before running a
  capability-equivalence claim. If `claimAllowed` is changed to `true` while a
  required register item is not `done`, the gate rejects the claim.

## 2026-08-30 credential boundary correction

- Added `internal/credential.Vault` with reference-only lookup, rotation,
  revocation, in-flight lease draining, process-side wipe, and close behavior.
- SQLite schema version 3 adds a dedicated `credentials` table and
  `CredentialRecordStore`; startup migrates legacy `llm.key.*` rows out of the
  generic settings table and provider set/unset now uses the vault.
- The focused credential/SQLite/provider regression passes, including restart
  recovery and the invariant that secrets do not appear in generic settings.
- A6 remains `partial`: the four production stream adapter families now accept
  an optional release-aware lease and release it at terminal/error/body-close,
  while the legacy string callback remains for embedders. SQLite is not an OS
  keyring/KMS, abandoned readers cannot be force-closed through the current
  reader interface, and hostile dump coverage is not yet proven.

## 2026-08-30 Team identity and cross-process credential refresh

- Team member provisioning now claims `team-member:<team-id>` before publishing
  the child session, member provisioning event, or Agent Registry handle. The
  Board and direct Roster paths share the reservation seam, and failed names
  remain non-reusable.
- Vault `Acquire` now refreshes one reference from the durable backend before
  each operation, so rotation and deletion in another process become visible;
  local writes serialize generation allocation and backend publication. Focused
  Team and credential tests cover pre-publication reservation and two-process
  refresh/revocation behavior.
- A5.4 and A6 remain partial: reservation-to-domain receipt atomicity,
  reservation recovery/garbage collection, OS-protected secret storage,
  abandoned-stream disposal, and the full hostile/fault matrix remain open.

#### 2026-08-30 Team reservation-to-receipt transaction correction

- SQLite now has `CreateTeamMemberSessionWithReservation`. The
  `team-member:<team-id>` reservation, child Session, fork seed, and Lead
  `team/member` provisioning receipt commit in one transaction; any later
  publication failure rolls the identity claim back with the receipt.
- Production Team provisioning uses this stronger path when available. Legacy
  stores retain the pre-claim plus compensating cleanup compatibility path.
- New regressions prove: committed receipt retains the reservation, a second
  store handle cannot claim the same member identity after publication, and a
  receipt conflict does not strand the reservation. Store, Team and `cmd/sta`
  focused/full regressions pass.
- This closes only the Team member transaction edge. Generated-ID orphan
  recovery/GC, job/terminal/subagent/MCP/workflow receipt atomicity, and the
  full crash matrix keep A2.2, A2.4, A5.3 and A5.4 partial.

#### 2026-08-30 terminal stale-claim receipt correction

- Cold Agent materialization now folds durable terminal lifecycle edges. A
  prior process's `terminal/start` with no matching stop receives exactly one
  `terminal/stop reason=process_restart`; repeat recovery is idempotent and a
  live terminal in the current process is never marked stale.
- Agent-scope terminal disposal now serializes retries, keeps first-wins
  receipt semantics, removes the stopped record from the process registry, and
  its regression rejects duplicate stop events or a retained addressable
  record.
- This is a durable claim/release correction, not full terminal recovery: the
  old PTY cannot be resurrected, crash-time kill-tree by process identity, PID
  reuse protection, and cross-host process cleanup remain open.

#### 2026-08-30 workflow durable receipt correction

- The production Workflow tool now installs an addressed, failure-reporting
  event sink. Runtime-owned calls still use the addressed runtime emit; legacy
  calls resolve the current process log through the composition root. Missing
  or unavailable sinks now fail the tool instead of allowing a successful
  result after a lost `workflow/*` receipt.
- Regression coverage proves the context sink is used without runtimectx, the
  first persistence failure returns `persist event`, and no later success
  receipt is emitted. Existing worker cancellation, late-callback rejection,
  stranded-agent synthesis, and one-terminal-end tests still pass.
- Workflow still remains open overall: restart replay that never duplicates an
  external child action and the complete death/JSONL/fault matrix are not yet
  proven.

#### 2026-08-30 workflow receipt-failure admission correction

- Workflow execution now runs under a receipt-owned child context. The first
  failed durable lifecycle event cancels that run context before the host
  reads another external `agent()` admission. The caller's context remains
  untouched, and the durable sink error is returned as the tool failure.
- A failure-injection regression proves that a failed `workflow/start` closes
  admission before an external child can start. Existing Node cancellation,
  stranded-agent, late-callback, one-terminal-end, and `cmd/sta` regressions
  still pass.
- A3.4 remains open: durable workflow intents and restart replay still need to
  prove that an interrupted run cannot duplicate an already-started external
  child action.

#### 2026-08-30 workflow worker protocol fail-closed correction

- The Node workflow host now validates every worker JSONL frame. Empty frames,
  invalid JSON, malformed event/agent/result payloads, and unknown frame types
  terminate the process, synthesize exactly one failed `workflow/end`, emit
  stranded agent ends, and fail the tool; they are no longer silently skipped.
- Protocol-boundary regressions cover malformed JSON, missing/unknown frame
  types, non-object event data, and missing agent/result required fields, plus
  the three valid frame families. Existing dynamic workflow, cancellation,
  late-callback, and `cmd/sta` suites pass.
- A3.4 still remains open for durable restart intents and the complete
  kill/crash/fault matrix.

#### 2026-08-30 workflow identity audit correction

- A candidate owner/`meta.name` reservation was audited against the pinned
  reference and removed. Reference records use an opaque per-run ID; `name`
  identifies the durable record payload, while the record invariant rejects a
  repeated `runId`, not a repeated name. Multiple and concurrent workflows are
  valid.
- The over-strict SQLite guard and its test were removed before release. This
  restores valid new runs while leaving restart replay/reconciliation open; do
  not interpret the removed reservation as completed A3.4 evidence.

#### 2026-08-30 workflow member record publication correction

- The external Node worker no longer records `workflow/agent-start` before the
  host starts a child. The member start is now emitted only after the provider
  returns a non-empty child ID, immediately followed by its paired end; a call
  that never publishes a child emits neither event.
- This aligns the worker record with the reference invariant that a member
  start carries a published child Session ID and that start/end pairs are
  complete. Regression coverage proves cancellation before publication emits no
  member pair.
- A3.4 remains open: the parent-side `tool-workflow/*` recorder projection,
  durable restart intent/reconciliation, and the complete worker death matrix
  are still not equivalent.

#### 2026-08-30 parent workflow recorder projection

- Added the reference-shaped parent session records:
  `tool-workflow/run-start`, `tool-workflow/agent-start`,
  `tool-workflow/agent-end`, and `tool-workflow/run-end`. Member starts carry
  the published child Session ID, and run-end is written only after the runtime
  observed a valid terminal state.
- The recorder folds only the active opaque run ID, validates member sequence
  and outcomes, and disables that run's projection after a malformed or
  mismatched runtime event. Matching the reference, a recorder append failure
  disables subsequent records but does not turn the successful workflow result
  into a tool error.
- Added bounded native Web projection fields for the four record types.
  Regression coverage proves the complete run/member pair, child identity, stop
  reason, and recorder-failure isolation. A3.4 still remains open for durable
  restart intent/reconciliation and the full worker death matrix.

#### 2026-08-30 workflow record replay invariant

- Session restore, live append, persisted-event adoption, and
  `ValidateLifecycle` now fold `tool-workflow/*` records with the same
  ownership rules as the reference: run IDs are unique, member starts carry a
  published child ID, member endings match open starts, and run endings reject
  open members or later record events.
- Consistent with the pinned invariant, an unfinished run at the end of a log
  remains valid as a crash prefix; recovery neither fabricates a run ending nor
  automatically re-executes the workflow.
- Interleaved complete runs and an open crash prefix have positive replay
  tests; duplicate run/member IDs, missing child IDs, unpaired endings, open
  members at run end, and post-run events have negative tests. A3.4 remains
  open for external-action reconciliation and the full worker death matrix.

## 2026-08-30 provider route availability admission

- Added `llm.ErrProviderUnavailable` as the stable negative class for a
  registered route that fails its local availability check.
- Non-pinned snapshots, pinned turn snapshots, and routed LLM adapters now use
  one registry-backed admission boundary. A stale session route to a dormant
  provider fails before acquiring a generation lease or reaching the provider
  stream; ACP surfaces the same typed route error before building a turn.
- Regression coverage proves an unavailable registered provider is rejected by
  pinned/non-pinned snapshots and routed streams without invoking
  `Provider.Stream` or acquiring its generation. Opaque standalone `llm.LLM`
  embedders retain their existing opt-out from registry-backed admission.
- A6.3 moves from open to partial. One catalog still needs to own
  context/token/reasoning/tool/vision/audio capability and defaults across
  CLI, Web, ACP and SDK; model-level negative results beyond provider
  availability are not yet equivalent.

## 2026-08-30 canonical tool contract projection

- The registry catalog now generates, for every registered tool, detached
  input/output schemas plus owner/provenance, active profile, the canonical
  error taxonomy, effective timeout, explicit cooperative-cancellation state,
  lifecycle events, and execution/approval/concurrency policy projection.
- `Registry.ValidateProjection` rejects unknown tools, repeated names, schema
  drift, and entries outside the active profile. CLI standard, PTC, and minimal
  mode tests now compare their model-facing projection against the registry
  catalog; production runtime and ACP policies carry the mode as catalog
  profile.
- A4.1 remains partial, and the register now splits the remaining work into
  A4.6 and A4.7. Registry registration now carries explicit source defaults,
  while plugin-owned production registration and tool-family cancellation
  classification remain incomplete. Therefore this is contract visibility and
  fail-closed projection, not completed tool-catalog equivalence.
- Follow-up registration hardening moved provenance to the registry boundary:
  ordinary registrations now store builtin owner/plugin/provenance, while MCP
  selector and per-server bridge tools store `mcp` provenance with the owning
  server. Those MCP tools also explicitly declare cooperative cancellation and
  catalog regressions verify both facts. Plugin-owned production registration
  and cancellation declarations for the remaining tool families remain open.
- A first cancellation audit now covers shell, run_code, grep/glob,
  web_search/web_fetch, MCP selector/bridge, and structured user questions.
  Each declaration is backed by its existing context-forwarding execution
  semantics plus a focused catalog or classifier regression.
- The second audit extends explicit classification to fresh pwsh, job
  wait/output, subagent wait, workflow run, and LSP. Negative regressions also
  preserve the deliberate false contracts: persistent terminal operations do
  not abort on the caller context, and starting a background job or foreground
  subagent creates work that outlives the tool call rather than claiming
  caller-context cancellation.

## 2026-08-30 versioned canonical catalog manifest

- Registry snapshots now expose a `CatalogManifest` with schema version, the
  highest registration generation, and a SHA-256 digest over the deterministic
  canonical tool payload. Validation rejects unsupported versions, payload
  drift, revision drift, and digest drift.
- The Web legacy inventory now derives names, count, and full canonical
  metadata from one manifest rather than separate registry reads. A regression
  verifies that replacing a tool generation advances both revision and digest.
- Agent-owned native/Web/SDK turn assembly validates the mode projection before
  loop construction, and ACP session creation validates its registry projection
  before exposing a session. A4.7 remains partial: legacy CLI fail-open
  assembly, first-class ACP/SDK inventory fields, and end-to-end release
  artifact digest pinning remain open.

## 2026-08-30 legacy runtime and catalog artifact fail-closed correction

- The compatibility-host CLI turn path now uses strict runtime assembly and
  returns durable configuration/projection failures instead of silently
  falling back to global runtime state. The old `applySessionRuntime` wrapper
  remains only for focused tests and no longer backs production turn paths.
- Added `pa --catalog-manifest <path|- >` to export the deterministic
  `CatalogManifest` and `pa --verify-catalog-manifest <path>` to validate an
  artifact against the exact live registry before service startup. Verification
  rejects internal payload/revision/digest tampering and runtime generation
  drift.
- ACP now exposes a first-class `toolCatalog` on initialize, session/new,
  resume, and reconnect. SDK initialize carries an optional, schema-compatible
  `toolCatalog` extension. Both reject registry/manifest failures before
  publishing a session or completing the handshake. The release gate now
  exports and verifies one CLI catalog artifact.
- A4.7 remains dependent on A4.6 for complete plugin/terminal registration
  semantics; the transport inventory and release verification boundary itself
  now has executable negative coverage.

## 2026-08-30 generation-guarded plugin tool publication

- Added the production plugin-to-tool composition seam. Plugin `Spec.Tools`
  publishes through a mount-owned `ToolPublisher`; ordinary tool registration
  bypasses are no longer needed for plugin ownership.
- First publication stores owner/plugin/provenance and the plugin mount
  generation. Reload uses generation-advancing replacement; old prepared calls
  reject through the existing stale-generation fence. Scope cleanups are
  generation-guarded, so closing the old plugin scope cannot delete a newer
  tool generation.
- The production app now owns a plugin registry bound to its canonical tool
  registry and registers plugin cleanup ahead of Agent consumers. A4.6 remains
  partial for persistent terminal context-abort semantics only.

## 2026-08-30 persistent terminal context abort

- `terminal.Session.WriteContext` observes the caller context while stdin and
  foreground command output settle. Cancellation sends SIGINT/CTRL_BREAK to the
  owned process group, then fail-safe closes the terminal process tree so an
  interrupt-ignoring command cannot continue orphaned.
- Native persistent shell and model `terminal_send` use this seam and classify
  only those foreground writes as cancellable. Terminal lifecycle receipts and
  non-blocking operations intentionally remain non-cancellable. ACP terminal
  write receives the same caller context.
- With plugin publication and persistent terminal cancellation closed, A4.6 and
  A4.7 are now done. The overall capability claim remains blocked by other
  runtime, persistence, protocol, security, and release-gate tasks.

## 2026-08-30 provider lease and durable-history audit

- Provider retry policy is declared by the published adapter route, captured at
  runtime assembly, and executed at the loop's durable request-error boundary.
  The shared policy owns retry classification, bounded backoff, Retry-After
  normalization, and policy identity; provider adapters do not independently
  define production retry behavior.
- Added a retirement regression proving that a retired provider generation
  rejects new streams without invoking the provider, while an already-acquired
  stream keeps the old provider alive until its terminal boundary and closes it
  exactly once.
- Added a model-history regression proving that a caller-declared persisted
  message without a committed durable event is omitted from provider requests;
  only the committed `user/message` row reaches the model. A1.3/A1.4 move from
  open to partial, not done: both remain gated by the full turn waterfall,
  external-wire policy fixtures, persistence recovery, and corruption gates.

## 2026-08-30 addressed-runtime isolation evidence

- Added a two-session concurrent production-runtime regression on SQLite. Both
  addressed Agents materialize separate durable logs and cloned registries while
  sharing the process default adapter; each model request and assistant answer
  contains only that session's history, and neither log observes the other
  session's user or answer text.
- This evidence also upgrades the completed canonical-catalog work: A0.3 and
  A4.1 now cover deterministic manifests plus native/Web/ACP/SDK fail-closed
  projections. A1.1 remains partial because command/tool adapters outside the
  turn runtime still expose explicit compatibility seams.

## 2026-08-30 Agent publication rollback deadlock correction

- Failure-injection testing exposed a deadlock in the session Agent rollback
  path: `sessionAgent` held the app Agent memo lock while closing an unpublished
  handle, and the memo cleanup waited for that same lock. A canceled Start could
  therefore wedge the caller instead of rolling back publication.
- Memo removal is now rollback-safe: it takes the memo lock only after
  publication has released it. During rollback it recognizes the unpublished
  state; after normal external disposal it removes the exact memo.
- A new regression injects canceled Start, proves the owned background job is
  cancelled and drained, the registry/memo have no ghost Agent, and a later
  healthy request materializes a fresh live Agent. A1.5 moves to partial; a
  dedicated durable Agent publication/disposal receipt and cross-process fold
  remain open.

## 2026-08-30 child owner publication rollback audit

- Audited the pinned reference ownership-transfer contract and added the
  production child-publication failure matrix. `SpawnProvider.Start` commits the
  child header, inherited fork seed, and `subagent/start` receipt in one atomic
  SQLite transaction; the live child is bound to the provider/runtime map only
  after that transaction succeeds.
- A new injection regression proves that atomic publication failure leaves no
  bound child log, no provider child entry, and no durable child row that a
  later restart could mistake for a published child. The existing durable
  replay tests cover cold resumption from the committed child log.
- Reference review found no durable `agent/created`/`agent/disposed` event:
  those are process-local lifecycle notifications, while durable ownership is
  represented by the session header/domain receipt and recreated from storage.

## 2026-08-30 sticky max-tokens waterfall correction

- Aligned the Go turn waterfall with the reference's sticky `max-tokens`
  semantics. A tool-carrying step whose provider finish is `length` or
  `max_tokens` now latches the turn outcome before tool execution, and a later
  turn-stopping-driven step that finishes normally can no longer downgrade the
  durable `turn/end` to `completed`.
- Added a two-step regression with a tool call on the capped step and a normal
  model tail on the next step; the final durable reason remains
  `max-tokens`. A1.2 moves to partial, not done: approval, timeout, worker
  death, and crash-injected retry/compaction branches still need a single
  cross-entry waterfall matrix.

## 2026-08-30 approval and timeout waterfall replay evidence

- Added the reference-shaped pre-execute denial matrix through the full loop.
  A denied sensitive call preserves the durable `tool/call`, emits a
  model-visible `TOOL_DENIED` result linked to that call, keeps `step/end`
  before the recovery step, and never executes the tool body. Cold replay
  produces the same derived model history.
- Strengthened the existing timeout waterfall test to assert the complete
  turn/step/tool lifecycle, structured `TOOL_TIMEOUT` source linkage, and
  cold-replay history equality. A1.2 now has executable approval and timeout
  branch evidence; worker death and crash-injected retry/compaction remain
  open.

#### 2026-08-30 model catalog route capability seam

- Added `modelCapabilityFor(sessionID)` as the runtime lookup for the effective
  provider/model route. It resolves the durable session override, the configured
  built-in/custom model directory entry, the built-in model fallback, and
  protocol/config capability fallbacks in one place.
- Model-directory rows now carry explicit optional `reasoning`, `tools`,
  `vision`, and `audio` declarations in addition to context/output capacity.
  Web provider edits and native provider rows preserve these declarations.
- CLI/native, Web, ACP and SDK turn assembly now consume this seam for context
  capacity and output limits; ACP applies a disclosed catalog output limit as a
  fallback rather than hard-coding only the global budget. Vision admission is
  the catalog declaration combined with the existing provider/modality checks.
- Regression coverage proves a catalog entry drives loop context/output limits
  and explicit reasoning/tool/vision/audio values. A6.3 remains partial: the
  remaining built-in candidates still need complete first-class capacity and
  modality metadata, and unavailable model-level routes still need the same
  stable negative contract across all transports.

#### 2026-08-30 built-in catalog and native selection admission

- The scattered `modelCandidates`, DeepSeek context-window map and DeepSeek
  reasoning special case now derive from one built-in model catalog. DeepSeek
  V4 entries carry context capacity and explicit reasoning; providers without
  locally provable metadata intentionally omit values instead of inventing them.
- Native `session.selectModel` now applies a shared runtime route-admission
  seam before persisting a session override. Unknown or dormant providers use
  `provider-unavailable` / `llm.ErrProviderUnavailable`; invalid efforts use a
  stable bad request result. Native `session.models` reports the effective route
  as non-routable when that same admission rejects it.
- Regression coverage rejects an unavailable native selection without changing
  durable session config, validates available DeepSeek and dormant routes through
  the composition-root seam, and proves catalog capacity drives loop limits.

#### 2026-08-30 model tool/reasoning admission

- Catalog capability is now runtime policy, not display-only. A route that
  explicitly declares `tools: false` projects no model-facing tool schemas;
  native, SDK and ACP loop assembly share this projection. A route that
  explicitly declares `reasoning: false` clears a stale effort at runtime.
- Selecting an explicit reasoning effort on a route that declares no reasoning
  fails with `llm.ErrCapabilityUnavailable`. Undeclared/free-form routes keep
  pass-through behavior until the catalog explicitly classifies them, so
  gateways that accept arbitrary deployment names are not falsely rejected.
- SDK-created sessions clear an inherited unsupported effort before durable
  config publication. Regression coverage covers empty tool surfaces, effort
  clearing, stable selection rejection, and valid effort pass-through.

## 2026-08-30 workflow terminal-response ownership correction

- Workflow `result` now claims terminal transport ownership immediately. The
  host closes worker request admission before draining in-flight callbacks, so
  an `agent_result` that arrives after the terminal result is rejected as late
  output rather than accepted during the drain grace.
- Protocol decode failures also claim transport ownership before killing the
  worker. A real-worker regression proves an in-flight child callback can drain
  after `workflow/result`, while its late response increments the host's
  fail-closed rejection counter.
- A3.4 moves to partial. Worker protocol, cancellation, unpublished-member,
  malformed-frame, and late-response paths now have executable evidence; the
  remaining explicit crash-open restart/replay fixture still needs to prove a
  new run cannot re-invoke an old run's external child.

## 2026-08-30 workflow crash-open restart identity correction

- Node workflow run IDs are now opaque 128-bit identities instead of per-process
  counters. A fresh Runner after process restart can no longer mint
  `workflow-node-1` and collide with an older crash-open durable run prefix.
- The restart regression runs two independent Runner instances and proves their
  IDs differ. A separate crash-open fixture restores a run/member prefix, then
  executes an explicit new workflow: restart materialization and the new run do
  not invoke the old child, the old prefix remains readable, and the combined
  lifecycle folds successfully.
- Worker death after member publication is covered by a real Node process test.
  Killing the worker after `workflow/agent-start`/`agent-end` yields exactly one
  published pair, no repeated provider admission, and one error
  `workflow/end`.
- A3.4 is now done: worker death, terminal ownership, late responses,
  cancellation, child disposal, crash-open replay, and restart identity have
  executable negative/positive evidence.

## 2026-08-30 credential backend failure generation fence

- Added a persistence-failure regression for dedicated credential rotation.
  When the credential backend rejects `Put`, the proposed value is never
  published, the old in-flight lease remains valid, and the failed generation
  cannot be reused. Clearing the backend fault allows the next monotonic
  generation to commit, and a restarted Vault resolves only the committed
  value.
- This supplements the existing SQLite dedicated-table durability, cross-process
  refresh, provider-generation retirement, and in-flight lease drain evidence.
  A2.2/A6.1/A6.2 remain partial because the broader MCP/job/Agent transaction
  and hostile storage matrices are still open.

## 2026-08-31 external crash-boundary audit

- Re-ran the focused static gates after the workflow and credential changes:
  `go vet`, `go build ./...`, and `git diff --check` pass with the repository
  audit caches.
- Audit confirmed that `job/done` is committed before owner wake and that cold
  materialization rebuilds a deduplicated inbox receipt. This is real evidence,
  but it does not prove the process-local wake budget across restart or behavior
  when the inbox journal fails after memoization.
- Audit confirmed that MCP `mcp/list` and `mcp/call` are session audit facts,
  while child/server lifetime is process/config-owned. The existing reconnect
  tests use protocol stubs; they do not yet prove a real stdio child kill during
  Start, discovery, or call leaves one generation and never replays the failed
  call.
- Added A2.5 to the task register so every externally side-effecting boundary
  must state whether process death is at-most-once, retryable through a durable
  receipt, or an audited unordered failure. Provider-generation cold recovery
  and Agent recovery retry are explicitly retained under A2.4/A1.5/A6.2.
- The overall equivalence verdict remains fail / `claimAllowed: false`.

## 2026-08-31 real MCP crash and job receipt deltas

- Added a real two-process MCP stdio regression. The first child is killed in
  the middle of `tools/call`; the reconnect supervisor creates a fresh child,
  the failed call is not replayed, and the cross-process request journal proves
  later discovery and calls belong only to the replacement generation. The
  regression passed five times.
- Audited the job wake boundary and fixed a fail-open recovery seam: a
  memoized Agent's `job/done` receipt recovery previously reopened an idle turn
  through `recoverJobCompletionWakes` without spending the configured
  consecutive-wake budget. Live settlement and cold receipt recovery now share
  one delivery boundary.
- Added negative regressions for a failed inbox receipt (no durable splice on
  failure, exactly one committed receipt on retry, no duplicate on repeat) and
  for wake-budget spend/restore. The focused `cmd/sta` regressions passed five
  times.
- `go test ./internal/mcp ./cmd/sta`, `go vet ./internal/mcp ./cmd/sta`,
  `go build ./...`, and `git diff --check` pass. A2.2/A2.4 remain partial;
  full equivalence remains `fail`.

## 2026-08-31 MCP initial failure supervision alignment

- Aligned the Go supervisor with the reference connection lifecycle: the first
  `Start` failure now enters the same bounded retry budget as a lost
  established connection. Start failures include spawn and initialize
  handshake failures.
- Added real two-process kill regressions for child death during `initialize`
  and `tools/list`. Each test observes the synchronous first error, waits for a
  fresh replacement generation, and validates the cross-process request
  journal. Together with the existing call-crash regression, Start, ListTools
  and Call kill points now have executable evidence.
- Fixed the composition layer to preserve that semantic: an optional MCP
  server's initial failure retains the supervised wrapper for background retry
  and shutdown, while `fail_on_startup_error=true` remains loud and closes the
  generation. A new integration regression proves the replacement generation
  publishes one tool after initial-start recovery.
- `go test ./internal/mcp ./cmd/sta`, `go vet`, `go build ./...`, and
  `git diff --check` pass. MCP list-changed, HTTP retirement, provider cold
  recovery and the remaining external crash-boundary contracts remain open.

## 2026-08-31 MCP generation list-changed and HTTP retirement

- Made MCP list-changed dispatch generation-aware. A notification arriving
  while the supervisor owns the outage is dropped; a notification from an
  already-retired generation cannot trigger a stale resync; only the current
  replacement generation invokes the registry refresh.
- Added negative regressions for a list-changed signal during reconnect and a
  stale signal after replacement. The test proves zero reads from the old
  generation, one discovery from the current generation, and one surviving
  replacement.
- Added `Close` quiescence evidence for in-flight `ListTools`: teardown waits
  for discovery to settle before generation close.
- Added HTTP generation-retirement evidence. Replacing an established HTTP
  generation explicitly DELETEs the old MCP session, installs the replacement
  under a fresh session, and performs later discovery only on that replacement.
- Full focused package tests, `go vet`, `go build ./...`, and
  `git diff --check` pass. A4.5 remains open for external task/auth matrices
  and the broader terminal/job/LSP/Web/plugin lifecycle contracts.

## 2026-08-31 cross-process provider generation cold restart

- Added a real parent/child process regression for provider cold recovery. The
  parent persists a custom provider route and its dedicated credential in one
  SQLite database, drains and retires the parent's provider generation exactly
  once, then closes the in-memory vault and store.
- The child process independently opens the same durable store, reconstructs
  the custom route and credential lease, publishes a new provider generation,
  and streams through the real OpenAI-compatible HTTP adapter. The parent's
  test server verifies the fresh process used the durable secret, persisted
  base URL and model. The regression passed three times.
- Provider, credential, store, loop retry and composition package tests pass,
  along with vet, build and diff checks. A2.4 retains the broader persisted
  wake-materialization gap; A6.2 still requires complete provider-scoped
  policy and hostile-secret matrices.

## 2026-08-31 persisted job owner cold materialization

- Added an end-to-end SQLite cold-materialization regression. The first app
  commits only `job/done`; a second app independently opens the store,
  reconstructs the owner Agent, and materializes one quiet inbox receipt
  through the durable sink; a third store reader verifies exactly one
  `job/done` and one `agent/inbox/spliced` fact.
- This closes the final remaining A2.4 execution gap in the task register. The
  owner state is rebuilt from the durable receipt, and repeated restarts do not
  duplicate the wake because the inbox insertion carries the stable job dedupe
  key.
- The A2.4 acceptance command passes across `./internal/... ./cmd/sta`, along
  with focused vet, build and diff checks.

## 2026-08-31 provider credential revocation matrix

- Added a protocol-family revocation matrix for DeepSeek/OpenAI completions,
  Anthropic Messages, Google Generative AI and OpenAI Responses. Every adapter
  transfers the vault lease to its live stream reader.
- While one stream remains live, revoking the credential fails every later
  stream with the stable `CREDENTIAL_UNAVAILABLE` code and no secret in the
  diagnostic. Draining the old reader releases the last lease reference.
- This upgrades A6.2 to done: rotation, failed backend persistence, in-flight
  drain/wipe, retired-generation rejection, cross-process cold rebuild and
  provider-family revocation now have executable negative-path evidence.

## 2026-08-31 Web credential boundary correction

- Fixed a production bypass in Web provider management: built-in and custom
  provider API keys were still written to generic `llm.key.*` settings even
  when the dedicated Vault was installed.
- Built-in save/clear, custom provider save/delete, and failed-edit rollback now
  use one dedicated credential seam. Generic settings retain only provider
  profiles/custom declarations; secret values live in the dedicated credential
  table. Lightweight embedders without a Vault retain an explicit compatibility
  fallback.
- Added SQLite regressions proving generic settings contain no `llm.key.*`
  secret while dedicated records are created, rolled back, cleared and deleted.
  A6.1 is now done; its acceptance command, full composition package, vet,
  build and diff checks pass.

## 2026-08-31 session Agent stale journal correction

- Fixed a production stale-journal defect exposed by fault injection. A
  memoized session Agent previously bound its inbox journal to one captured
  in-memory log. After a runtime-log reload, recovery could write the new
  inbox receipt to the stale sequence space and conflict with durable facts
  appended by another process.
- `sessionInboxJournal` now resolves the current runtime log on every write.
- Added a production `sessionAgent` regression covering a transient log-load
  failure, an externally appended `job/done`, a transient sink failure, retry,
  and idempotent recovery. Durable counts prove 1/1 after the first receipt and
  2/2 after the second, with no duplicate on repeat lookup.
- The A1.5 focused acceptance command and full agent/store/jobs/composition
  package tests pass, along with vet, build and diff checks.

## 2026-08-31 subagent ownership matrix closure

- Added a consolidated A5.1 ownership matrix for the subagent control plane.
  A child owned by one session is rejected for status, resume, send, followup,
  interrupt, cancel and explicit parent-scoped list from another caller.
- Caller-scoped discovery and wait return no foreign projection, and the matrix
  proves zero child callbacks run for any cross-owner control attempt.
- Existing resume/lineage, disposal, Agent scope and spawn lineage tests remain
  part of the evidence. A5.1 is now done; its acceptance command, complete
  agent/subagent/composition package tests, vet, build and diff checks pass.

## 2026-08-31 job and terminal owner receipt closure

- Closed A5.3 with the full owner/receipt acceptance matrix. Job owner close now
  has executable evidence for cancelling live work, removing only the owner's
  addressable records, closing admission, rejecting late starts, and waiting for
  the completion observer. Terminal owner close proves process-tree teardown,
  owner-scoped removal and exactly one durable stop receipt.
- Cold restart coverage proves stale terminal claims receive one stop receipt,
  live processes are skipped, SQLite `job/done` materializes exactly one quiet
  owner receipt, and failed inbox splices retry without duplication.
- A5.3 is now done. Its acceptance command passes across jobs, terminal and the
  composition root; remaining A5 work is Team lifecycle and identity
  reservation transactional evidence.

## 2026-08-31 Team identity and lifecycle closure

- Added orphan-reservation regressions for legacy job and Team task/message
  namespaces. A reservation committed before a crash remains non-reusable after
  restart, while generation counters recover by skipping the orphan and minting
  the next stable identity. Team member atomic publication already proves that
  receipt failure rolls back the reservation, and failed provisioning retains
  the failed identity rather than reusing it.
- A5.4 is now done. Its acceptance command covers durable namespace scoping,
  restart uniqueness, Team member atomicity, legacy job generation and the new
  Team task/message orphan recovery.
- A5.2 is also done. Existing executable evidence covers reference wire shapes,
  append-only task/mailbox folds, CAS revisions, blocker DAG/tombstones,
  snapshot restore, cold member rebind/lifecycle, roster authorization, failed
  provisioning, atomic delivery receipts and claimed-inbox retry.

## 2026-08-31 schedule crash-replay boundary

- Added a production durable-schedule regression for the two crash windows:
  owner inbox committed but fire/dispatch missing, and durable fire committed
  but owner delivery/dispatch missing. Both replay to one owner receipt, one
  `schedule/fire`, and successful dispatch without duplicates.
- The test exposed a real duplicate-wake defect. A claimed schedule inbox splice
  is itself the durable receipt; restart now checks that splice before issuing
  another `Followup`, instead of re-adding a reminder merely because it was
  already consumed.
- Also fixed terminal owner close to scan only `terminal/start` and
  `terminal/stop` records. Unrelated id-less audit events such as
  `schedule/change` can no longer wedge Agent disposal.
- Schedule, terminal and composition package tests pass, along with vet, build
  and diff checks. A2.2 remains partial only for external protocol auth/task
  kill-point classification.

## 2026-08-31 MCP auth/task kill-point closure

- Added an HTTP auth-failure regression for a side-effecting `tools/call`. An
  unauthorized request fails terminally for that caller, does not automatically
  replay, leaves the negotiated MCP session usable, and a later explicit call
  produces exactly one external effect.
- Added a real Streamable HTTP required-task regression. The bridge discovers
  `execution.taskSupport: required`, rejects it before dispatch, and the test
  proves no `tools/call` reaches the external server.
- With approval, Team, MCP, job, Agent, credential, terminal, schedule and
  workflow/code boundaries covered, A2.2 is now done. A4.5 remains open for the
  broader external client schema and lifecycle matrix.

## 2026-08-31 storage corruption matrix audit

- Audited A2.3 across both durable backends instead of only SQLite. SQLite and
  JSONL now have one shared corruption/recovery acceptance boundary covering
  interrupted and torn tails, bounded non-repairing reads, replay conflicts,
  duplicate/gapped sequence rejection, atomic seed/fork publication, cross-process
  locks, abnormal child-process exit, backup and integrity repair.
- Session contract evidence also rejects unpaired tool/result records, while
  SQLite atomic tests prove conflicting rows roll back without partial durable
  state. A2.3 is now done; its expanded acceptance command passes across
  `internal/store` and `internal/persistence`.

## 2026-08-31 storage lifecycle matrix closure

- Added SQLite schema-migration evidence for a real v1-style database: legacy
  rows and titles survive, title provenance and event counters are reconciled,
  reservation storage becomes available, the version advances, and a newer
  database is rejected without downgrade.
- Added an independent SQLite-handle append regression proving contiguous
  sequences, no duplicate or lost commits, and no cross-handle identity reuse.
- Combined with JSONL child-process lock recovery and the shared backend
  fixture, A2.1 now covers both backends across locks, reservations, migration,
  backup, repair and reader/writer ordering. A2.1 is done.

## 2026-08-31 Agent publication lifecycle closure

- Closed A1.5 after re-auditing the reference boundary: the durable session
  header/domain receipt owns cross-process publication, while live Agent
  creation/disposal remains Cordis-like process-local state.
- Executable evidence now covers failed publication rollback, stale memo
  replacement, registry cascade disposal, child/job owner cleanup, provider
  generation retirement, concurrent addressed-Agent isolation and transient
  journal failure replay. A failed Start leaves no ghost memo or owned job, and
  a later healthy request materializes a fresh Agent.
- A1.5's acceptance command passes across `internal/agent`, `internal/store`
  and `cmd/sta`.

## 2026-08-31 canonical worker-death recovery closure

- Added one canonical session fixture that crosses provider retry facts,
  log-only compaction facts and worker death after tool dispatch. Recovery
  closes only the open tail with a linked `TOOL_OUTCOME_UNKNOWN` result, then
  one interrupted `step/end` and one interrupted `turn/end`.
- The fixture proves wire replay, idempotent second recovery, preserved terminal
  turn one, compaction observations, and a model-visible explicit unknown-outcome
  tool message after cold restore.
- Combined with the loop waterfall/retry/approval/timeout suites and the JSONL
  and SQLite interrupted-tail repair regressions, A1.2 is now done. The overall
  equivalence claim remains forbidden by A1.1/A1.3/A1.4, external lifecycle
  matrices, projections and final release gates.

## 2026-08-31 provider retry taxonomy closure

- Added a real-wire matrix across DeepSeek/OpenAI completions, Anthropic
  Messages, Google Generative AI and OpenAI Responses. Every family normalizes
  the same HTTP 429 response to structured `RATE_LIMIT` facts while preserving
  provider Retry-After, request correlation and bounded secret redaction.
- The composition policy fixture proves each route exposes one provider-scoped
  normal policy, preserves its registry identity, makes exactly one wire request
  when private retries are disabled, and decides retry eligibility from typed
  failure codes rather than provider text.
- Existing loop evidence covers durable retry boundaries, always-mode fallback,
  cancellation-over-retry, unsupported finish and max-token stickiness; A6.2
  covers credential lease drain and retirement. A1.3 is now done. A1.1/A1.4 and
  later external/release gates still forbid an equivalence claim.

## 2026-08-31 durable history write-boundary closure

- Added a JSONL/SQLite surface-replacement contract. A provenance-complete
  compaction replacement produces identical model history live, after cold
  replay, and after fork; under-cited replacements are rejected at the write
  boundary instead of becoming corruption discovered only during restore.
- Exposed session provenance validation to persistence and wired it into JSONL
  create/append and SQLite adapter create/append. Failed appends leave the
  committed prefix unchanged, and invalid cold seeds leave no loadable session.
- With existing durable-history, raw-chunk folding, steering, quiet-injection,
  fork, compaction and corruption evidence, A1.4 is now done. A1.1 and the
  external/release gates still prevent any equivalence claim.

## 2026-08-31 model route capacity boundary correction

- Moved the unknown/free-form model context fallback into the composition-root
  catalog seam. Web metering and loop assembly now use the same 1,000,000-token
  default instead of Web owning a second hidden fallback.
- Fixed ACP capability resolution to use its pinned provider/model route and its
  captured configuration snapshot. ACP turn logs now carry the same durable
  request/context capacity as native and SDK loop assembly.
- Added route-aware image admission for SDK prompts initialized with an explicit
  provider/model pair, combining catalog vision capability with provider support.
- A6.3 remains partial only for verified first-party metadata and evolving
  remote catalog negative coverage; no unverified provider facts were invented.

## 2026-08-31 hostile-secret egress hardening

- Added real egress tests for credential-shaped hostile fixtures in telemetry,
  durable spill files, and recovered tool-panic diagnostics. Telemetry now
  redacts decoded canonical payloads at the exporter boundary; spill redacts at
  the single engine boundary before manual writes and AutoSpill can persist a
  memory; tool panics remain classified and redacted.
- Strengthened the shared diagnostic redactor for compact JSON members and added
  a byte-level credential-lease drain test proving revocation plus release
  zeroes the process-local buffer.
- The broad A6.4 acceptance command passes across all internal packages and the
  composition root. A6.4 remains partial only for the OS-level crash-image
  boundary: disabling core dumps, protected scrubbing, or an explicit
  unsupported deployment profile still needs a durable product decision.

## 2026-08-31 crash-dump policy closure

- Added `security.crash_dump_policy` with safe default `disabled` and explicit
  non-equivalent `external`. Unknown policies fail startup validation.
- On Unix, the disabled profile sets `RLIMIT_CORE=0` before credential loading.
  On Windows, it fail-closes when WER LocalDumps is enabled globally or for the
  executable, and cannot silently inherit a machine dump policy.
- Registry-seam tests cover global, per-application and missing keys; policy
  tests cover the explicit external profile and invalid values. A6.4 is done.
  A6.3 and final protocol/security/release gates still keep overall equivalence
  forbidden.

## 2026-08-31 reference catalog metadata correction

- Pinned the DeepSeek reference facts in Go: both V4 routes own 1,000,000-token
  context, 256,000-token output, reasoning, tools and text-only input.
- Unknown/free-form routes now remain text-only until a catalog entry explicitly
  declares a modality. ACP image admission uses the pinned provider/model route,
  and provider adapter support remains a second admission gate.
- Updated an SDK catalog-override fixture that previously relied on global
  modality to invent vision capability. A6.3 remains partial because non-DeepSeek
  facts live in the upstream pi-ai installed catalog, whose model data is not
  vendored in the pinned checkout.

## 2026-08-31 tool-family runtime isolation closure

- Added a common negative contract to fs, skill, MCP, job, plan, schedule,
  spill, workflow and subagent event sinks: when an addressed runtime context is
  present, its session-owned `Emit` wins and the legacy process-log callback is
  not invoked.
- Completed the outside-turn runtime audit. The remaining `currentID`/`a.log`
  paths are explicit compatibility adapters behind `runtimeSessionID`,
  `runtimeLog`, `terminalOwner` or context-aware tool emitters; production
  composition sets `strictAgentRuntime`, so absent runtime identity cannot fall
  back to the legacy turn path.
- A1.1 is done with concurrent addressed-Agent, strict production startup and
  per-family runtime sink evidence. The A1 legacy-global blocker is removed;
  A2/A6.3/A7/A8/A9 still forbid an equivalence claim.

## 2026-08-31 manifest status source closure

- Added `scripts/equivalence-report.ps1`, which emits a verification report with
  the subject commit, pinned reference commit, profile list, exact open blocker
  IDs and UTC verification timestamp.
- The release gate now validates status/claim agreement, exact register-derived
  blockers, disclosure in task/status/parity documents, report identity and
  timestamp freshness before considering a claim path. It performs these checks
  even while `claimAllowed` is false.
- Replaced area-level blocker labels with exact required task IDs. A0.4 is done;
  the manifest remains `fail` / `claimAllowed: false` because the other listed
  release blockers are still open.

## 2026-08-31 task register evidence closure

- Added `scripts/equivalence-register-lint.ps1` and wired it into the release
  gate. The lint verifies unique task IDs, status vocabulary, required fields,
  acceptance criteria, implementation/evidence paths, executable evidence and
  replayable commands for all done tasks.
- Fixed the audit's real findings: stale owner/MCP implementation paths,
  documentation-only evidence for A4.2/A4.3 and an A4.2 acceptance command that
  did not select its claimed regressions. A0.5 is now done.

## 2026-08-31 runtime profile authority delta

- Added `internal/profile` as one fail-closed authority for selected and optional
  runtime profiles. SQLite storage, local file, session reference, local
  JavaScript and local shell profiles carry enforcing implementation and replay
  descriptions; e2b, Python, Cordis dynamic runner and Cordis inspect are
  explicitly unsupported.
- Native Web now exposes `runtime.profiles` and `runtime.profile`. The former
  empty-success Cordis handlers were replaced with stable
  `profile-unsupported` failures, eliminating the “empty implementation dressed
  as capability” behavior called out by the equivalence contract.
- A8.3 moves from open to partial. Its broad command passes, but A0.2 profile
  classification and A3.2 enforcing sandbox backend remain dependencies before
  selected optional sandbox profiles can be called equivalent.

## 2026-08-31 external crash-boundary registry delta

- Added `internal/crashboundary` as one machine-readable authority for external
  side-effect contracts. MCP calls, foreground terminal writes, workflow children
  and plugin calls are at-most-once with no automatic transport replay; schedule
  fires and subagent receipts are durable-retryable; plugin generation reload is
  an audited unordered failure; terminal/process lifecycle is runtime state.
- Native Web now exposes `runtime.crash-contracts` and `runtime.crash-contract`
  with stable unknown-boundary failures.
- A2.5 moves from open to partial. Its broad acceptance command and new registry
  wire tests pass; A4.5 external client matrices and complete plugin-owned call
  evidence remain before done.

## 2026-08-31 lifecycle coverage correction

- Re-audited A4.5 instead of leaving it open despite existing coverage. MCP,
  terminal/job, LSP, Web fetch/search cancellation, plugin generation reload and
  unsupported-profile regressions now all have executable evidence.
- Fixed an MCP timeout test design issue exposed by the broad suite: process
  startup/initialize used the same 300ms request bound as the deliberately hung
  `tools/list`, making the test load-sensitive. Handshake now uses the normal
  timeout and the 300ms bound applies only to the target request.
- A4.5 moves from open to partial. Full external client fault/schema matrices
  for MCP task/auth, remote Web providers and plugin reload combinations remain.

## 2026-08-31 A7.4 Web authorization and admission closure

- A7.4 is now done. The consolidated browser matrix covers bearer auth, cross-origin
  `POST`/`PATCH` mutation rejection, local non-browser mutation admission, addressed
  queue callbacks, unknown interaction stability, foreign subagent control-plane
  rejection and secret non-disclosure for credential updates and provider catalogs.
- SSE malformed cursors fail closed, and `Last-Event-ID` replay emits only the
  durable suffix plus correctly deduplicated live events. Native replay/live
  projection shape remains covered by the existing native projection tests.
- A real shutdown admission gap was closed: Web queue enqueue, update and edit now
  call the application admission gate before touching process-local state. The new
  owner/shutdown matrix proves foreign and unknown items cannot mutate another
  session and that late shutdown mutations preserve the existing queue.
- The exact acceptance command passes across `internal/webserver` and `cmd/sta`.
  Overall equivalence remains blocked by A0.2, A2.5, A3.x, A4.4/A4.5, A6.3,
  A7.1-A7.3, A8.x and the A9 release gates; `claimAllowed` remains false.

## 2026-08-31 A8.4 telemetry closure

- Fixed an actual shutdown-lifecycle defect in session telemetry. The export
  worker used a detached context for collector HTTP calls, so a hung collector
  could remain active for the client timeout even after `Shutdown` returned.
  Shutdown now cancels the exporter context and joins the worker before
  reporting the bounded deadline.
- Added regressions for hung-collector shutdown bounds, nonblocking behavior when
  the telemetry queue overflows, and preservation of route, session, service,
  version and stable user identity on OTLP records.
- A8.4 is done independently of A8.1: telemetry consumes already-committed
  canonical events, while A8.1 still owns cross-entry projection authority.
  Overall equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 A0.2 capability baseline closure

- Replaced prose profile classification with an executable authority. The fixed
  inventory covers all 58 capability seams in the pinned reference's
  `docs/capability-seams.md`, including base, Web, headless and explicitly
  optional E2B/Team/Terminal/LSP boundaries.
- Each row carries exactly one required/optional classification and either an
  enforcing implementation plus replay evidence or an explicit unsupported
  reason. Native Web exposes the full inventory and stable negative responses;
  E2B/Cordis classifications agree with the existing profile registry.
- A0.2 is done and no longer circularly blocks A8.3. A8.3 now remains partial
  only for enforcing sandbox/backend composition evidence. Overall equivalence
  remains `fail` / `claimAllowed: false`.

## 2026-08-31 A4.5 external lifecycle closure

- Added consolidated contract matrices for Streamable HTTP MCP, remote Web
  provider/client behavior and plugin generation reload. MCP coverage proves
  auth/malformed-protocol faults retain the session, do not replay automatically,
  preserve required-task metadata and preserve rich output schemas.
- Web coverage proves provider fault classification, redirect policy, unsupported
  content, non-2xx result handling and closed input/output schemas.
- Plugin coverage found and fixed a real reload transaction defect. When tool
  publication succeeded but a later mount/runtime step failed, cleanup could
  unregister the live tool. Reload now snapshots the prior tool and restores its
  exact tool, schema and ownership metadata; failed attempts cannot make the
  capability disappear.
- A4.5 is done; unsupported profile disclosure is covered by A0.2. Overall
  equivalence remains `fail` / `claimAllowed: false`.

## 2026-08-31 tool argument and pre-cancellation boundary

- `Prepare` now crosses a lossless JSON argument snapshot before policy or
  dispatch. Already-decoded maps/slices are detached, so a caller cannot alter
  a prepared request between authorization and body start.
- After successful materialization, an already-cancelled call returns
  `ABORTED_BEFORE_DISPATCH` before whitelist, schema, approval, or pre-hook
  execution. Materialization failures retain precedence, matching DSH's
  argument-boundary rule.
- `TestPreCancelledCallSkipsPolicySchemaAndHooks`,
  `TestPrepareSnapshotsArgumentsBeforeDispatch`, and
  `TestPreCancelledArgumentMaterializationFailureWins` provide executable
  evidence. This advances A4.4 only to partial; the complete per-tool
  rich-result, disposed-owner, and replay matrix remains open.

## 2026-08-31 native session-list checkpoint boundary

- Auditing the cross-entry projection path found that native `session.list`
  intentionally reads only a bounded event tail for sidebar metadata, but was
  also folding that tail into a checkpoint labelled with the complete session
  revision. A cold list request could therefore overwrite a correct-looking
  cache with missing goal/plan/permission/history state from older events.
- `session.list` now uses the bounded tail only for list metadata, reuses an
  exact durable checkpoint when present, and otherwise replays the complete
  raw session before writing a checkpoint. The cache schema version was bumped
  so semantically incomplete version-1 rows cannot survive an upgrade.
- `TestNativeSessionListDoesNotCheckpointTailOnlyProjection` covers state
  outside the tail window and verifies the saved checkpoint contains it. This
  advances A8.2's revision/invalidation evidence; A8.1 remains open because
  the CLI/native/Web/ACP/SDK projections are not yet one shared canonical
  implementation.

## 2026-08-31 preparation-failure result notification

- DSH's `tools/result` observer is terminal for every model tool call,
  including unknown names, argument materialization/schema failures, policy
  denial, and pre-dispatch cancellation. Shutu previously invoked its
  `ResultHook` only from the successful `ExecutePrepared` finish path, so a
  direct registry caller could observe no result at all for those failures.
- `ExecuteWithCallID` now publishes one normalized error result after
  `Prepare` fails; stale-generation and pre-dispatch cancellation failures in
  `ExecutePrepared` use the same boundary. Observer panics remain isolated and
  the existing Go error return is preserved for compatibility.
- `TestResultHookObservesPreparationFailuresExactlyOnce` covers unknown,
  denied, invalid-args, and pre-cancelled calls. This is additional A4.4
  evidence; the loop's durable per-tool and cross-transport matrix remains
  open.

## 2026-08-31 canonical session title event

- Native `session.rename` now rejects empty normalized titles with
  `title-invalid`, appends a durable `session/title` event through the
  addressed session log, and returns the real event sequence. The composition
  root owns this callback; production Web PATCH rename uses the same callback,
  while the generic webserver keeps its metadata-only path only for embedders
  without a live runtime.
- SQLite folds title/source from that event in the same transaction that
  inserts it. Automatic fallback/provider title revisions use the same event
  path, and their final write is serialized with user rename to preserve the
  user pin under an in-flight model result.
- This is concrete A8.1 evidence for title convergence across list/history/
  mux/restart, but it does not close A8.1: the remaining CLI/native/Web/ACP/SDK
  projection implementation is still not one shared canonical module.

## 2026-08-31 model selection image-capability boundary

- Reference `session.selectModel` resolves the target route before publishing
  it and refuses a text-only route when durable or pending model-visible
  content contains images. Shutu's shared selection validator previously
  checked provider availability and reasoning effort but allowed this invalid
  durable combination.
- The composition-root validator now replays the addressed session's derived
  history before admission and returns `ErrCapabilityUnavailable` for a
  non-vision target. Native RPC maps that stable class to `model-unavailable`
  and does not write the session override.
- `cmd/sta/modelcatalog_test.go:TestSessionModelSelectionRejectsTextRouteForDurableImages`
  and the native selection tests cover the route-level negative path. A6.3
  remains partial: pending inbox image admission and a provider-owned
  `listModels/resolveModelInfo` seam are still missing, and non-DeepSeek facts
  cannot be safely fabricated from the pinned reference checkout.

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

## 2026-08-31 DeepSeek wire and SDK failure-boundary repair

- The DeepSeek request now always sends `stream_options.include_usage=true`,
  matching the reference wire contract. Its reasoning effort path now emits
  the explicit `thinking` mode, rejects unsupported effort values, and rejects
  enabling effort when the deployment is locked to disabled thinking.
- The SSE decoder now treats an unterminated final event as `STREAM_CLOSED`,
  accepts a UTF-8 BOM on the first field, preserves every logical delta in one
  payload instead of dropping reasoning/text or later choices, and accepts both
  DeepSeek cache-hit usage spellings while keeping input/cache-read counts
  disjoint.
- SDK transport requests with no timeout no longer dereference a nil timer.
  Low-level runtime spawn failures are reusable on a subsequent `Start`, and
  transport loss waits briefly for process/stderr settlement so the returned
  `ClientClosedError` carries exit code and bounded stderr context like dsh.
- Regression coverage is in `internal/llm/deepseek/deepseek_test.go` and
  `internal/sdkclient/client_test.go`; targeted provider and SDK suites pass.
  A7.3 remains open because ACP/SDK reference-runtime matrices, all production
  provider catalogs, audio, and the full negative/fault release matrix are not
  yet complete.
## Latest verified delta (2026-08-31, shared session-list metadata fold)

- Web `/api/sessions` and native `session.list` now consume the same
  `internal/session.SessionListMetadata` event fold. Only `turn/start` marks a
  session engaged (`blank=false`); a legacy user-only log remains blank, and
  only human `user/message` provenance updates prompt recency.
- Added a cross-surface regression through the Web session-list test and a
  direct session projection test. This removes the concrete `event_count > 0`
  divergence but does not close A8.1: ACP/SDK/CLI history, title, feedback,
  plan/todo, approval, and inventory still lack one complete canonical
  projection boundary.
## 2026-08-31 Web session-list activity projection repair

Web `GET /api/sessions` now consumes the shared `session.SessionListMetadata`
fold for both blankness and activity time. Its `updated_at` no longer blindly
uses SQLite's last physical append timestamp when a later lifecycle/transport
event was committed after the most recent eligible human prompt; it promotes
the Unix-millisecond prompt timestamp exactly as native session.list does.
`TestSessionListUsesLatestHumanPromptForUpdatedAt` covers the ordering-sensitive
case. This is a partial A8.1 repair; the full cross-entry projection and release
gate remain open.

## 2026-08-31 Native model catalog metadata projection

The composition root now publishes an explicit `catalog_models` projection in
the sanitized provider view for owned model facts. Native `llm.models` consumes
that projection and preserves `contextWindow`, `maxTokens`, `reasoning`,
`tools`, `vision`, and `audio` instead of dropping them while ACP/loop already
used the same route facts. ID-only provider suggestions remain outside this
authoritative projection. `TestNativeLLMCatalogPreservesOwnedModelMetadata`
covers the wire result. A6.3 remains partial because provider-owned dynamic
`listModels/resolveModelInfo` and non-DeepSeek first-party metadata are still
not proven.

## 2026-08-31 Provider-owned model catalog contract seam

`internal/llm` now exposes provider-owned `ListModels` and exact
`ResolveModelInfo` operations, preserving explicit false capability metadata,
rejecting duplicate/cross-provider rows, and retaining one catalog failure per
provider. The retry wrapper forwards the seam without inventing retry/config
facts. This closes a missing registry contract only; A6.3 remains partial
until production adapters implement dynamic catalog resolution and the
remote failure/cancellation matrix.

The DeepSeek adapter now accepts the composition-root's owned catalog for
`ListModels` and exact `ResolveModelInfo`; known routes return explicit
capacity/modality facts while unlisted routes remain pass-through with unknown
capacity. This is intentionally narrower than claiming all installed
providers are equivalent.

## 2026-08-31 Cross-entry core-turn contract probe

Added `internal/contractfixture/cross_entry_test.go` as the first real
cross-entry suite rather than another package-local fixture check. The same
canonical turn is written and cold-read through SQLite and JSONL, queried
through authenticated Web events and native `session.history`, and compared
against the SDK event envelope type. The comparison keeps `seq`, event type, time,
and durable data strict, with an explicit native normalization only for
generated message IDs, internal turn/step coordinates, and tool-result lookup
fields.

The suite exposed and repaired two actual native projection omissions: nested
`data.chunk.text` was being erased from assistant chunks, and nested
`data.message` content/source was ignored for assistant messages. A9.1 remains
partial: SDK runtime execution, ACP execution, native CLI/child Agent execution,
reference replay, and side-effect/cleanup oracles still need to be joined to
this same fixture and comparison path.

## 2026-08-31 DeepSeek wire and SDK liveness boundary

The DeepSeek request now sends `stream_options.include_usage=true`, validates
thinking/reasoning effort, rejects truncated SSE tails, preserves every delta
from one payload, accepts both cache-hit usage spellings, and applies a
five-minute idle watchdog. A silent interval is `TIMEOUT`; caller cancellation
is `ABORTED`. The SDK transport now handles no-timeout requests safely, permits
low-level spawn retry, and enriches runtime transport loss with settled exit
and bounded stderr context. Targeted provider/SDK tests and the full Go suite
pass. A7.3 remains partial: ACP/SDK external matrices, telemetry/session
headers, image-bearing tool-result wire expansion, all claimed provider
catalogs, audio, and full fault/negative oracles remain outstanding.

## 2026-08-31 DeepSeek image budget and tool-result wire repair

- `internal/llm.OffloadRequestImages` now follows dsh's request-image
  semantics: it budgets base64 payload length including padding, removes the
  oldest images until the whole request fits, uses the exact dsh placeholder,
  preserves nested block positions, and never mutates durable message history.
  Exact-boundary, oldest-first, nested, and immutability regressions pass.
- DeepSeek tool-result images no longer serialize as image parts on a `tool`
  message. The tool result stays textual (`(see attached image)` when needed),
  and consecutive image-bearing tool results are grouped into a following user
  multimodal message with the reference marker.
- Provider-owned DeepSeek catalog negatives now override a permissive global
  image flag for known models before offload or file reads. Dynamic unlisted
  routes retain the standalone adapter fallback and remain explicitly
  catalog-unverified.
- This closes the concrete image/tool-result wire gap but does not close A7.3:
  ACP/SDK external matrices, telemetry/session headers, all claimed provider
  catalogs, audio, and the full fault/negative release oracles remain open.

## 2026-08-31 Provider catalog runtime forwarding

- OpenAI-compatible, Anthropic Messages, Gemini, and OpenAI Responses adapters
  now accept the composition-root catalog snapshot and implement detached
  `ListModels`/`ResolveModelInfo` forwarding. The retry wrapper preserves the
  same seam, so registry catalog queries no longer return `unavailable` merely
  because the selected adapter is not DeepSeek.
- Runtime catalog projection retains ID-only rows as unknown-fact entries;
  it does not turn settings-page suggestions into invented context, reasoning,
  vision, audio, or tool facts. Exact rows still preserve explicit false
  capability values and model resolution keeps dynamic unlisted routes
  ID-only.
- All four adapters apply an exact catalog `Vision=false` before image offload
  or serialization; a global image flag remains only the fallback for dynamic
  routes whose catalog has no explicit modality fact.
- This closes the adapter/runtime seam gap but not A6.3: the pinned checkout
  does not vendor the upstream pi-ai model facts for every provider, and remote
  refresh/cancellation/failure oracles plus complete first-party metadata are
still required. Overall claim remains fail-closed.

## 2026-08-31 Node permission enforcement probe

`TypeScriptRuntimeStatus` previously treated the presence of `--permission` in
`node --help` as proof that Code Mode was enforceable. That was only an API-shape
check: a Node binary could advertise the flag while failing to enforce the
boundary at runtime. The capability probe now launches a short-lived Node
process with an empty environment and requires an ungranted read of its own
executable to fail with `ERR_ACCESS_DENIED`. `run_code` is therefore not
registered, and the local JavaScript profile is marked unsupported, when the
host cannot prove enforcement. `internal/code/capabilities_test.go` locks this
negative capability check to the actual installed runtime.

This repairs a concrete A3.2 advertisement gap. A3.1/A3.2/A3.3 remain release
blockers: the broader cross-platform sandbox, hostile-child, network, resource
limit, and native/Web/ACP/SDK unsupported-response matrix is not yet complete.

## 2026-08-31 ID-only catalog uncertainty boundary

Runtime provider catalog snapshots continue to expose ID-only rows so adapters
can list owned candidates, but model-selection capability state now keeps
reasoning unknown unless the selected row or its inherited first-party entry
explicitly owns a reasoning fact. An ID-only suggestion is no longer silently
treated as a known non-reasoning model. The regression is covered by
`TestIDOnlyCatalogEntryKeepsReasoningUnknown`.

This removes a concrete A6.3 fail-closed semantic inversion. A6.3 remains
partial because complete pinned pi-ai facts, protocol-specific reasoning wire
semantics, and remote catalog refresh/failure/cancellation oracles are still
missing.

## 2026-08-31 Generic reasoning fail-closed boundary

The Anthropic Messages, Gemini, and OpenAI Responses adapters now reject a
non-empty `low`/`high`/`max` reasoning selection with the stable
`UNSUPPORTED_REASONING_EFFORT` failure before credentials or network I/O. They
previously accepted the request at the provider-neutral loop boundary and then
silently omitted the protocol-specific thinking field. Empty effort and
`off` preserve the ordinary no-parameter request; DeepSeek keeps its own
thinking wire implementation.

This closes a concrete capability-to-wire mismatch. Full catalog effort maps
are still missing; low/high/max budget policy is now carried from
`llm.thinking_budgets` through the canonical loop request, so A6.3/A7.3 remain
partial for the wider dsh effort vocabulary and remote catalog evidence.

## 2026-08-31 First-party capacity and modality facts

The built-in catalog now carries the pinned dsh/pi-ai facts for the currently
advertised OpenAI (`gpt-4o*`, `gpt-4.1*`), Anthropic (`claude-sonnet-4-5`,
`claude-opus-4-1`, `claude-haiku-4-5`), and Gemini (`gemini-2.5-*`,
`gemini-2.0-flash`) routes: names, context windows, output limits, tool
support, image input, and audio negatives. OpenAI's known non-reasoning facts
are also authoritative. The selector and runtime catalog expose these values
through the same detached snapshots.

The pinned catalog marks the Anthropic and Gemini 2.5 models as reasoning
models, and the Go adapters now serialize their protocol-specific thinking
fields. The remaining gap is the full dsh catalog effort map (including
per-model wire maps and levels outside the selected low/high/max vocabulary),
plus the remote catalog fault/refresh matrix. A6.3 remains partial until those
are implemented or explicitly excluded from the selected profile.

## 2026-08-31 Reasoning request wire completion

The previous fail-closed boundary has been replaced by real request
serialization for the three non-DeepSeek protocol families: Anthropic emits
`thinking.type` with a bounded `budget_tokens` (or `disabled`), Gemini emits
`generationConfig.thinkingConfig` with `includeThoughts` and
`thinkingBudget`, and OpenAI Responses emits `reasoning.effort` plus
`reasoning.encrypted_content`. Exact catalog false facts reject unsupported
efforts, while unknown dynamic routes remain pass-through. Regression servers
assert the request JSON, temperature handling, and encrypted-content include.

This removes the core reasoning wire omission. Remaining A6.3/A7.3 gaps are
complete dsh effort maps/per-model wire values and remote catalog
failure/refresh/cancellation evidence.

## 2026-08-31 Durable unknown-event and negative-matrix closure

Durable session writes now use a closed event vocabulary. Live append, atomic
append, persisted incorporation, replay, and lifecycle validation reject an
unknown durable event with `ErrUnknownRequiredEvent` before sequence advance;
the native wire-level `ignorable` extension exception remains separate from
the durable log. `TestUnknownDurableEventRejectedAcrossAppendPaths` covers the
write and replay paths.

`internal/contractfixture/negative_matrix_test.go` now exercises representative
tool, sandbox, optional-profile, event, approval-owner, and remote-provider
negative paths and checks stable failures, no tool body execution, and no
partial durable state. A9.2 is therefore partial rather than open. It still
needs exhaustive coverage for every required tool/provider and cross-process
worker-death/network-loss post-rejection oracles.

The latest local validation passed `go vet ./...`, `go build ./...`, the task
register evidence lint, and the focused negative matrix. Full `go test ./...`
passed every package except two `internal/skill` fixtures that intentionally
walk past their temporary no-`.git` root into the host workspace's
`.agents/skills`; this is recorded as an environment-sensitive verification
failure, not treated as a release pass.

## 2026-08-31 Cross-entry ACP/MCP fixture expansion

The shared protocol lifecycle fixture is now exercised by one external-package
test through the public ACP server, a real MCP stdio child process, and the
public SDK client against a child JSON-RPC runtime. These paths preserve the
fixture's session identity, tool identity, tool output, and terminal assistant
fact at their respective wire boundaries. This closes another A9.1 evidence
gap without pretending ACP, MCP, and SDK have identical wire shapes. The
shutu SDK runtime, native CLI/child Agent, reference replay, and cross-entry
side-effect/cleanup oracle remain outstanding.

## 2026-08-31 SDK shutdown creation fence

The SDK server now closes session admission before waiting for an in-flight
`session/prompt` Agent creation. A creation that crosses the first admission
check is rechecked before publication and is disposed when shutdown has
already sealed the server. Concurrent and repeated shutdown calls share one
completion result, so callers cannot observe teardown success while a raced
Agent remains owned. `TestSDKServerShutdownIsConcurrentAndIdempotent` covers
the public lifecycle boundary; the full cross-process creation race still
needs an injected worker/slow-store oracle.

## 2026-08-31 Startup approval projection isolation

The process composition root no longer uses strict session replay while
restoring the approval ownership table. When SQLite exposes
`SessionRawStore`, `restoreInteractions` reads committed rows exactly as
stored and extracts only interaction facts; unrelated legacy lifecycle damage
therefore cannot prevent SDK/Web/ACP startup. Opening the affected session
still uses the strict recovery-aware path and reports corrupt history at the
session boundary. `TestRegisterInteractsDoesNotReplayUnrelatedDamagedSession`
covers a transcript with repeated open turn anchors. A real rebuilt `pa
--sdk` executable also completed initialize → shutdown against the existing
default SQLite data directory; the isolated smoke used an explicit external
crash-dump policy and placeholder credential, and did not send a model request.

## 2026-08-31 Durable admission parity across stores

The durable event vocabulary is now enforced at every append boundary, not only
inside the in-memory `session.Log`: SQLite seed/append/atomic/team/approval
paths and JSONL load/recovery paths use `ValidateDurableEvent`, while native wire
events retain their separate `ignorable:true` extension rule. An unknown event
therefore cannot be inserted and deferred until restart, and a failed SQLite
append leaves neither a session row nor an event row. This closes a concrete
storage-level negative-path omission; broader corruption/fault and cross-process
oracles remain release blockers.

## 2026-08-31 Native projected-history admission parity

Native `session.history` still reads the raw store so an in-flight open-tail
remains visible, but it now validates the durable event vocabulary before
deriving a client-facing projection. An unknown persisted event can no longer
be relabeled as a harmless `ignorable` wire extension on the native path while
the Web path rejects it. `TestNativeHistoryRejectsUnknownDurableEventsFromRawStore`
covers this negative boundary; startup approval restoration and fork inspection
retain their explicitly raw control-plane seams.

## 2026-08-31 Windows process-tree cancellation hardening

The Windows `run_command`, `pwsh`, and background-job cancellation seams now
use a private Job Object attached immediately after process start, with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, to terminate the command tree, matching
Code Mode's existing native process-tree boundary.
If an incompatible external job prevents assignment, the implementation keeps
the direct-child kill fallback. The Windows path has passed cross-compilation;
a Windows runtime child-process oracle remains part of A9.3/A9.4; the local
Windows descendant tests now exercise the runtime boundary directly.

## 2026-08-31 Model-level input catalog projection

Custom and built-in model-directory rows now preserve a DSH-compatible
model-level `input` declaration for the currently supported `text` and
`image` modalities. The declaration is projected into the provider-owned
`ModelInfo`, Web `ProviderModel`, and native catalog rows; an explicit
`input:["text"]` therefore cannot be overridden by the deployment-wide
`text,image` setting. Unsupported `audio`, duplicate modalities, duplicate
model IDs, negative capacities, and persisted invalid rows fail closed before
provider registration or provider-edit mutation. Custom providers whose
legacy `model` field is empty now resolve their first declared `models[]` row
as the effective model consistently in registration and Web inventory.

This closes a concrete catalog/projection inconsistency, but A6.3 remains
partial: per-model `reasoningEfforts`/wire maps, full upstream metadata, and
remote catalog refresh/failure/cancellation coverage are still outstanding.

## 2026-08-31 Model-level reasoning effort wire maps

The provider-owned catalog now supports the DSH canonical effort vocabulary
(`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`) with per-model wire
mapping. An authoritative map rejects an effort that is not declared, preserves
`off:null`, and translates mapped values before DeepSeek or OpenAI Responses
serialization. Anthropic and Gemini use the same canonical admission and
thinking path. Catalog snapshots deep-copy the map, and Web/native model rows
preserve it for selection surfaces.

This is intentionally partial: model default-effort selection, budget maps,
complete upstream model metadata, and remote refresh/failure behavior remain
open A6.3/A7.3 work.

## 2026-08-31 Code preset capability advertisement

The native agent-preset catalog and selection path now consume the composition
root's live `code_available` capability. When `run_code` was not registered,
the `code` preset is omitted from `agentPreset.list` and selecting it returns
the stable `agent-preset-invalid` response. This closes a concrete Web/native
advertisement mismatch; the broader A3.1 enforcing-backend and ACP/SDK
negative matrix remains open.

## 2026-08-31 MCP server ping response

The stdio and Streamable HTTP MCP clients now answer the protocol-level server
`ping` request with an empty JSON-RPC result instead of returning `-32601` or
silently dropping the request. A real helper-process round trip and a real SSE
`httptest` cover the request before/inside `initialize`; HTTP replies use a
separate POST because the original response stream is already being consumed.
Servers using the MCP liveness handshake no longer get a false
incompatibility response.
Unsupported server-to-client methods remain explicitly rejected, and tool-call
reconnect still never replays an unconfirmed external side effect.

## 2026-08-31 Built-in model input modality projection

The owned built-in catalog rows now carry the same explicit `input` modality
facts as the pinned reference rows: OpenAI, Anthropic, and Gemini entries are
`[text,image]`, while DeepSeek V4 entries are `[text]`. The values are preserved
through native/Web catalog rows and provider `ModelInfo`, so a vision boolean
cannot silently drift away from the provider-owned modality list. ID-only
provider suggestions remain intentionally unclassified. This is a concrete
A6.3 metadata repair; remote refresh, complete upstream metadata, and the
remaining provider catalog matrix are still open.

## 2026-08-31 Provider request attribution and session headers

All four provider HTTP adapters now apply one stable shutu User-Agent at the
wire boundary. Normal loop requests carry the runtime session ID, while ACP
compaction requests carry the same session ID and an explicit `compaction`
purpose. The official DeepSeek route lazily resolves the existing stable
anonymous data-directory identity and sends the reference-compatible user,
session, and compaction headers; OpenAI-compatible custom routes are excluded
from those DeepSeek-specific headers. `TestOfficialRequestCarriesAttributionIdentity`
covers the complete local header contract.

This closes the locally reproducible attribution omission, but A7.3 remains
partial until the external SDK/provider disconnect, reconnect, cleanup, and
remote catalog fault matrix is replayed against the pinned reference.

## 2026-08-31 SDK close-race pending cleanup

`internal/sdkclient.LineTransport.Request` now removes its pending entry when
transport closure wins after request registration but before the serialized
write can begin. The race is covered by
`TestLineTransportClosedBeforeWriteRemovesPending`; timeout, late-response,
process-death, and shutdown tests continue to pass. This prevents repeated
disconnect/shutdown races from retaining abandoned request state, but does not
substitute for the missing external SDK runtime replay matrix.

## 2026-08-31 ACP client-disconnect cleanup

The ACP server now has an executable disconnect boundary test that waits until
`session/new` has been committed and then closes the client input. The server
cancels the established session before closing it, and the session is released
exactly once. `TestServerClientEOFCancelsAndClosesEstablishedSessions` covers
the same ownership direction as the pinned reference bridge's client-disconnect
tests. A7.1 is now partial rather than open; the full external client,
reconnect, rich-content, permission, and post-disconnect side-effect matrix
remains required.
## Latest implementation correction (2026-08-31, MCP terminal reconnect failure)

When an MCP reconnect outage exhausts its retry budget, the reconnecting client
now emits a one-shot terminal-failure signal. The composition root withdraws
all tools owned by that server, so an unavailable generation is no longer left
callable after retries stop. This is covered by
`internal/mcp/reconnect_test.go:TestReconnectingClientNotifiesExhaustionOnce`
and
`cmd/sta/mcps_test.go:TestRegisterMcpsWithdrawsToolsAfterReconnectBudgetExhaustion`.

This repairs the terminal MCP tool-lifecycle boundary; A7.2 remains open for
the complete external stdio/Streamable HTTP reconnect, close, and side-effect
matrix.

## Latest implementation correction (2026-08-31, provider catalog ownership)

Provider model metadata is now deep-copied at construction, listing, and exact
resolution boundaries. This prevents callers from mutating input modalities,
reasoning maps, or capability pointers and silently changing later request
admission. The regression coverage is in
`internal/llm/model_catalog_test.go:TestCopyModelInfoDetachesNestedCatalogFacts`
and
`internal/llm/deepseek/model_catalog_test.go:TestModelCatalogSnapshotDoesNotAliasConfig`.

This is a bounded A6.3 repair; remote catalog refresh/fault behavior and the
remaining upstream model facts are still open.

## Latest implementation correction (2026-08-31, SDK subscription failure ownership)

SDK subscription close now drops queued notifications while preserving the
first terminal runtime failure. A caller that closes a subscription after an
unexpected runtime exit therefore does not lose the original disposition and
diagnostic identity. Covered by
`internal/sdkclient/subscription_test.go:TestSubscriptionCloseDropsQueueButPreservesFirstFailure`.

This is a bounded A7.3 cleanup; the full reference SDK external process matrix
and strict race gate remain open.

## Latest implementation correction (2026-08-31, canonical durable-event projection)

`internal/projection` now provides a strict cold-rebuild boundary over the
ordered durable session events. It reconstructs model history, session-list
metadata and title fallback, plan/todo state, approval settlement, feedback,
job state, and MCP activity from the same event stream. Web session listing and
CLI title selection now consume this snapshot instead of independently folding
those fields. Invalid or non-contiguous durable streams fail the projection
explicitly, and nested state is detached so a caller cannot mutate replay
authority through a returned map.

This moves A8.1 from open to partial. Native rich protocol projection and the
ACP/SDK/query/trajectory consumers still need migration to the shared snapshot;
the model-selection image-capability gate now also consumes the canonical
history projection. A live-vs-cold equivalence fixture is still required.

## Latest implementation correction (2026-08-31, ACP external process disconnect)

ACP now has a real child-process wire test for the established-session EOF
boundary. The parent drives `initialize` and `session/new` over pipes, closes
stdin, and verifies the child cancels every owned session before closing it and
exiting cleanly. This is stronger than the earlier in-process reader tests and
is covered by
`internal/acp/contract_external_test.go:TestExternalACPProcessDisconnectCleansEstablishedSession`.

A7.1 remains partial: the pinned reference client matrix, reconnect replacement,
rich attachment/resource combinations, permission settlement, and
post-disconnect durable side-effect oracle are still open.

## Latest implementation correction (2026-08-31, SDK pre-start subscription settlement)

`Client.Close` now fails subscriptions even when no runtime process was ever
started, including after a spawn failure. Previously a subscription created
before `Start` had no producer left but could wait forever after `Close`. The
regression in
`internal/sdkclient/client_test.go:TestClientCloseBeforeStartFailsSubscriptions`
covers both states.

This closes a local SDK lifecycle leak; A7.3 still requires the pinned runtime
matrix and cross-process cleanup oracle.

## Latest implementation correction (2026-08-31, live projection cursor)

`internal/projection.Cursor` now provides the live counterpart to the durable
cold rebuild. It validates each committed event through the session log,
folds it once, and returns detached snapshots. The new
`internal/projection/projection_test.go:TestLiveCursorMatchesColdProjectionAfterEveryCommittedEvent`
compares live and cold state after every event in a shared lifecycle fixture;
`TestLiveCursorSnapshotIsDetached` protects the ownership boundary.

A8.1 remains partial because rich native state and ACP/SDK/query/trajectory
consumers still require migration to this canonical snapshot.

## Latest verification correction (2026-08-31, reference replay environment)

The pinned `TestCoreTurnReplayMatchesReference` was retried with the checked-in
reference root and an isolated Go cache. It is currently `unverified`, not
`passed`: Node 24.19.0 fails before loading the reference module with
`uv_os_get_passwd returned ENOMEM`; a standalone `node -e` calling
`os.userInfo()` reproduces the same host error. The Go projection and full
local test suites remain independently green.

## Latest implementation correction (2026-08-31, SDK status ordering)

SDK server status notifications are now reserved per session before prompt and
idle transitions and flushed only after their prerequisite durable event has
been forwarded. This prevents later `session.event` notifications from
overtaking `session.status(running)` for an external client. The real external
client regression
`cmd/sta/sdk_test.go:TestSDKServerExternalClientPromptRunsAgentThroughIdle`
passed ten consecutive runs, and the full Go suite passed after the change.

This is a bounded A7.3 repair; the pinned SDK/reference-provider replay matrix
and broader disconnect/reconnect, catalog-fault, audio, and side-effect
oracles remain outstanding.

## Latest implementation correction (2026-08-31, SDK real-child cross-entry)

The cross-entry evidence now launches the production shutu SDK server in a
real test child process. The public SDK client drives initialization, prompt,
durable receipt, assistant events, running/idle status ordering, and shutdown
over exec/stdio; the regression passed five consecutive runs. A9.1's missing
production SDK child leg is now covered. Native CLI/child-Agent execution,
reference replay, and side-effect/cleanup comparison remain outstanding.

## Latest implementation correction (2026-08-31, MCP close interruption)

MCP stdio and Streamable HTTP clients now signal close before waiting for the
serialized request lock. An in-flight stdio read tears down the child process,
and an in-flight HTTP request is canceled by the client close context. The
cleanup DELETE keeps its own bounded context so session retirement is still
attempted after cancellation. Covered by
`internal/mcp/mcp_test.go:TestClientCloseInterruptsInFlightRequest` and
`internal/mcp/http_test.go:TestStreamableHTTPClientCloseInterruptsInFlightRequest`;
the focused MCP/CLI/contractfixture suites and the full `go test ./... -count=1`
run pass with a workspace-external writable TEMP/TMP root. The complete
external reconnect/auth/side-effect matrix remains open.

## Latest implementation correction (2026-08-31, projection cache monotonicity)

SQLite projection checkpoints now reject late writes whose revision is older
than the committed cache row, so a delayed reconnect cannot roll derived state
backward. Regression coverage also runs concurrent writers and corrupt-cache
rebuild through the native history path. A8.2 is now done; A8.1 remains
partial because the remaining query/trajectory/control consumers still need to
migrate to the shared projection.

## Latest implementation correction (2026-08-31, Web context projection)

The Web context-token fallback now reads `projection.Snapshot.History` rather
than rebuilding a private `session.Log` history. This also handles a valid
live open-turn tail through the shared projection; only an explicitly damaged
durable stream uses the conservative raw-event fallback. The regression is
covered by `internal/webserver/webserver_test.go:TestEstimateContextTokensUsesSharedProjectionForLiveOpenTail`.
A8.1 remains partial because native/ACP/SDK/query/trajectory consumers still
need migration.

## Latest implementation correction (2026-08-31, code capability fail-closed)

When the Code Mode runtime is unavailable, Web now removes `code` from the
settings preset list, rejects attempts to persist it without changing stored
settings, and reports `code_enabled` from the actual registered runtime. This
extends the existing native preset and CLI registration gates. The focused
native/Web regression passes; ACP/SDK negative catalog coverage and a real
enforcing sandbox backend remain open under A3.2/A3.1.

## Latest implementation correction (2026-08-31, shared query surface projection)

The session-query tools no longer classify `current`, `shadowed`, and
`log-only` events by scanning replacement payloads in the query package. The
classification now runs through `internal/projection.ClassifyEventSurfaces`,
which validates the same durable stream and applies replacement ranges beside
the canonical history fold. ACP compaction token estimation also consumes
`projection.Snapshot.History`. Native/SDK history and trajectory/control-state
consumers still need the remaining migration, so A8.1 remains partial.

## Latest implementation correction (2026-08-31, capability-probe contention)

The Node permission-model capability probe no longer permanently caches a
negative result when the host is temporarily contended: successful and
permanently unsupported outcomes remain memoized, while probe timeouts can be
retried. The bounded probe window was also widened for full-repository
parallelism. The targeted capability suite and the complete `go test ./...
-count=1` suite now pass with isolated writable Go/TEMP roots. This fixes a
stability defect in the capability gate; it does not close the missing
enforcing sandbox backend or the ACP/SDK negative matrices.

## Latest implementation correction (2026-08-31, loop history projection)

The main loop now obtains model-visible history through
`internal/projection.Build` both before assembling every request and after
context-overflow compaction before retrying. This brings the primary model
execution path onto the same validated cold projection used by Web, ACP, and
session-query, with projection errors stopping the step instead of silently
falling back to a private history interpretation. The focused loop/projection
tests and the full Go suite pass. Native rich trajectory/control-state
projection and the remaining SDK migration still keep A8.1 partial.

Compaction pressure detection now uses the same projection boundary as the
loop rather than `SessionLike.DeriveHistory`; an invalid durable stream fails
closed before a summary request is made. `TestCompactIfNeededFailsClosedOnInvalidProjection`
covers this negative path. This removes another duplicate history authority,
but does not close native rich trajectory/control-state migration.

## Latest implementation correction (2026-08-31, non-DeepSeek catalog facts)

The built-in candidates for OpenRouter, Together, Groq, Mistral, NVIDIA, and
Hugging Face now carry the pinned pi-ai capacity, modality, and reasoning facts
where the installed reference catalog provides them. The focused catalog tests
pass. This is a bounded A6.3 repair; remote catalog refresh/failure behavior,
complete upstream metadata, and per-model effort/budget semantics remain open.

## Latest implementation correction (2026-08-31, meter surface projection)

The shared projection now exposes replacement-aware model-visible surface
entries with their owning durable sequence. Token metering consumes that
projection for positional nodes and usage-anchor surface pricing instead of
maintaining a second replacement fold. The projection replay path also accepts
the historical zero-based open-tail stream used by native paging. Focused
projection, meter, session, and native-history regressions pass; native rich
trajectory/control-state and SDK consumers remain open under A8.1.

The model-request path retains its runtime-only attachment boundary through
`projection.BuildWithImageResolver` and `session.Log.ImageResolver`: durable
snapshots remain path-free, while the loop explicitly resolves local image
paths immediately before provider dispatch.

## Latest implementation correction (2026-08-31, SDK session-event envelope)

The SDK notification adapter now uses `session.WireEvent`. Surface events lift
`surfaceOp` and optional `sourceEventSeqs` to the DSH event envelope, remove the
internal storage copies from `data`, and preserve opaque payload fields. This
closes a concrete SDK schema/replay mismatch; SDK history/state consumers still
remain outside the shared rich projection, so A8.1 and A7.3 stay partial.
## Latest implementation correction (2026-08-31, cross-process negative oracles)

`internal/contractfixture/negative_matrix_test.go:TestNegativeCrossProcessOracles`
now runs the worker-death and network-loss cases in independent child
processes. A worker that exits after committing a durable turn/step/tool-call
prefix is recovered on reload into the single `interrupted` step/end and
turn/end terminal closure; no synthetic `tool/result` is admitted. A child
provider request against a closed HTTP server settles as a stable provider
error while its durable log remains empty. This closes the cross-process
fixture gap for representative cases. A9.2 remains partial because the
negative matrix still needs every required tool/provider and external
side-effect oracle.

## Latest implementation correction (2026-09-01, live history authority)

The live `internal/projection.Cursor` now folds history through
`session.DeriveHistoryEvents` from its own committed event prefix. Its
validation `session.Log` remains only the append-admission boundary and is no
longer used as a second history authority. The native reconnect surface
snapshot and standard plan/todo control values also consume a canonical
projection cursor; truncated pages and legacy assistant replacement markers
use an explicit adapter fallback. The
live/cold cursor equivalence suite and the dependent loop, compaction, meter,
Web, native, and SDK tests pass. Native rich trajectory/control-state
projection and the remaining ACP/SDK projection migration are still partial
under A8.1.

## Latest implementation correction (2026-09-01, ACP cancellation idempotency)

The ACP server now treats `session/cancel` for an unknown session as an
idempotent notification, matching the pinned bridge contract. A stale cancel
racing session disposal/reconnect no longer produces a misleading `-32602`
response. `internal/acp/contract_external_test.go:TestExternalACPCancelUnknownSessionIsIdempotent`
covers the public wire behavior. A7.1 remains partial: the full reference
client, permission/settlement, reconnect, and post-disconnect side-effect
matrix is still required.

## Latest implementation correction (2026-09-01, ACP transport-error teardown)

The ACP scanner-error path now cancels server-initiated permission requests
before waiting for prompt workers. This closes a real abnormal-transport hang:
an approval request using a longer-lived context can no longer keep `Run`
blocked after the input pipe fails. The regression is
`internal/acp/server_test.go:TestServerScannerErrorCancelsPendingPermissionBeforeWaiting`.
A7.1 remains partial because the complete external reference matrix is still
not executed.

## Latest implementation correction (2026-09-01, SDK malformed-frame behavior)

The SDK runtime now ignores malformed JSON lines, matching the pinned DSH line
protocol. It no longer emits an uncorrelatable null-id parse-error response.
`cmd/sta/sdk_test.go:TestSDKServerIgnoresMalformedJSONLines` covers the wire
behavior. A7.3 remains partial because the reference-runtime and provider
fault/reconnect matrix is still not complete.

### Latest implementation correction (2026-08-31, optional sandbox startup downgrade)

When `code.enabled` is on but the Node permission model cannot be verified, the
composition root now keeps the core Agent running, removes `run_code` from both
the base and live registry whitelists, and records the reason. Native/Web/ACP
and SDK surfaces therefore cannot advertise or execute an unverified sandbox;
persisted code-mode sessions fail closed at runtime admission. This is a
stability and fail-closed correction, not evidence that Code Mode has reached
the dsh enforcing-backend requirement; A3.1/A3.2/A3.3 remain open or partial.

## Latest implementation correction (2026-09-01, explicit model-route catalog)

Loop assembly now resolves the final provider/model route before deriving
catalog-dependent output limits and context capacity. An explicit route can no
longer inherit the global model's output budget during assembly. The regression
is `cmd/sta/modelcatalog_test.go:TestBuildLoopUsesExplicitRouteCatalog`.
A6.3 remains partial: complete upstream model facts, remote catalog
refresh/failure/cancellation behavior, and transport-wide negative matrices are
still required.

## Latest implementation correction (2026-09-01, dynamic MCP call at-most-once)

The dynamic `internal/mcp` `mcp_call` path now sends exactly one `tools/call`
request. Connection errors and timeouts are returned without creating a new
client or replaying the request, because the server may have committed an
external side effect before the transport failure became visible. This now
matches the reference tool bridge's at-most-once boundary; the regression is
`internal/mcp/tools_test.go:TestMcpDynamicCallDoesNotReplayUnknownCommit`.
A7.2 remains partial because the complete external stdio/Streamable HTTP
reconnect, auth, task-support, and side-effect matrix is still required.

## Latest implementation correction (2026-09-01, dynamic MCP task admission)

Dynamic `mcp_call` now discovers the named tool's current `tools/list` metadata
before sending `tools/call`. If the tool declares `execution.taskSupport` as
`required`, the call fails before the side-effecting request; ordinary unknown
tool names remain owned by the server's normal `tools/call` diagnostic. The
regression is
`internal/mcp/tools_test.go:TestMcpDynamicCallRejectsRequiredTaskBeforeCall`.
A7.2 remains partial pending the full external task/auth/reconnect matrix.
## Latest verification correction (2026-09-01, Code Mode catalog fail-closed matrix)

When the TypeScript permission-model probe fails, the composition root now has
direct factory-level coverage that the unavailable `run_code` capability is
absent from both the ACP and SDK tool catalogs, in addition to the existing
Native/Web catalog and execution checks. The registry execution gate remains
covered as well, so an unavailable capability cannot be reached through an
alternate public transport. `cmd/sta/codes_test.go:TestRegisterCodeUnavailableRemovesRunCodeFromACPAndSDKCatalogs`
passes with the focused suite. A3.2 remains partial because its dependency on
an enforcing sandbox backend (A3.1) is still unresolved; this closes evidence
coverage, not the backend requirement itself.

## Latest implementation correction (2026-09-01, macOS Seatbelt sandbox backend)

The local shell sandbox now detects and uses macOS `sandbox-exec` when its
deny-write profile passes a bounded functional probe. Read-only and
workspace-write calls are translated to the same file-effect vocabulary as
the DSH Seatbelt backend, including `/dev/null`, workspace, and temporary
roots; network isolation remains explicitly unavailable on this backend.
`TestSeatbeltProfileMatchesFileEffectPolicy` and the real
`TestSeatbeltEnforcesReadOnlyWhenAvailable` fixture cover the profile and
effect boundary. Linux bubblewrap remains preferred when available, and
Windows still fails closed for controlled workspace modes. A3.1 is now
accurately classified as partial, not complete: the resource-limit,
hostile-descendant, and every-claimed-platform enforcement matrix is not yet
proven.

## Latest verified delta (2026-09-01, Windows Code Mode CPU backstop)

- TypeScript Code Mode now passes its `computeMs` budget into the Windows Job
  Object that owns the worker process tree.
- Windows therefore has a kernel-enforced cumulative CPU ceiling for the Code
  Mode worker, while the existing process-time monitor remains responsible for
  DSH-compatible compute-expiry classification and cancellation behavior.
- This is a narrow resource-enforcement improvement. The DSH Windows
  `WRITE_RESTRICTED` token/ACL backend is not implemented: controlled shell
  workspace modes remain unavailable on Windows, and memory, file-size,
  fork-bomb, network, and hostile-descendant fixtures remain open in A3.1.
- `go test -p 1 ./internal/code -count=1` and `go vet ./...` pass. A full
  `go test ./...` run reaches the two existing `internal/skill` temporary-root
  fixtures but fails when the managed temporary root is inside the repository;
  rerun with an external writable temp root for the release gate.
- `TestProcessTreeEnforcesPerProcessCPU` passes on the managed Windows host,
  using a real busy child and a 250 ms Job Object CPU ceiling.
- `TestTypeScriptRuntimeContainsHeapExhaustion` passes with a 16 MiB
  `--max-old-space-size` worker ceiling and observes a contained
  `worker-exit`, leaving the Go host alive.

## Latest verified delta (2026-09-01, Windows ACL restricted-token backend)

- Added the DSH-shaped Windows ACL path for controlled shell modes: deterministic
  workspace/temp capability SIDs, inherited write grants, cross-process DACL
  locking, `WRITE_RESTRICTED` token creation, restricted `os/exec` startup,
  default-DACL extension for child stdio, private `TMP`/`TEMP`, and revocable
  temp cleanup.
- The capability advertisement is guarded by a real workspace-write probe and
  never falls back to an unrestricted child when a Win32 step fails.
- The managed Windows host currently returns `ERROR_INVALID_PARAMETER` from
  `CreateRestrictedToken`; therefore the new backend is compiled and wired but
  remains unavailable on this host, and its real boundary test is skipped. This
  is recorded as an environmental backend limitation, not as successful ACL
  equivalence.

## Latest verified delta (2026-09-01, Native model capability projection)

The Native `llm.models` and `llm.discoverModels` adapters now preserve the
catalog's model-level `input` modalities and `reasoningEfforts`, in addition to
capacity, reasoning, tools, vision, and audio facts. These fields were already
owned by the composition-root catalog but were silently dropped at this
transport boundary. `internal/webserver/native_rpc_test.go:TestNativeLLMCatalogPreservesOwnedModelMetadata`
covers the round trip. This closes one A6.3/A8.1 projection omission; remote
catalog refresh/failure and the remaining rich trajectory/control-state
migrations are unaffected.

## Latest verified delta (2026-09-01, shared ACP assistant surface)

ACP committed assistant delivery now rebuilds the durable model-visible
surface through `internal/projection.Snapshot.Surface` instead of walking raw
assistant event payloads as an ACP-only history authority. The session decoder
now preserves assistant text, image attachment references, and unknown
merge-extensible content blocks, so the shared surface does not silently lose
rich output before ACP adaptation. Single plain-text assistant output retains
the existing ACP text update shape; mixed rich output keeps typed block order
and still preflights every attachment before emitting any update.

Evidence: `internal/session/session_test.go:TestDeriveHistoryAssistantPreservesRichContentBlocks`,
`cmd/sta/acp_test.go:TestACPCommittedRichOutputPreservesOrderAndPreflightsAttachments`,
and `cmd/sta/acp_test.go:TestACPEmitsOnlyCommittedAssistantMessage`.

## Latest protocol correction (2026-09-01, ACP output delivery failure)

ACP `session/update` delivery is now part of the prompt's settlement boundary.
The server records the first notification write failure and returns an internal
prompt error instead of falsely reporting `end_turn` after committed output was
lost. Protocol writes also reject short writes with `io.ErrShortWrite`, so a
partially written JSON-RPC frame cannot be treated as delivered.

Evidence: `internal/acp/server_test.go:TestServerContainsSessionUpdateWriteFailure`
and `internal/acp/server_test.go:TestServerRejectsShortProtocolWrites`.

## Latest model catalog correction (2026-09-01, default reasoning effort)

The shared `llm.ModelInfo` catalog now carries the model-owned default
reasoning effort (`DefaultReasoningEffort`), matching DSH's
`reasoning.defaultEffort`. Custom model persistence and Web `ProviderModel`
round trips preserve it; Native model catalog/discovery emits it; and the
Anthropic, Gemini, OpenAI Responses, and loop request seams consume the same
catalog value when the caller omits an effort. Legacy boolean/map catalog
facts receive the deterministic selector-compatible default, while invalid or
map-inconsistent defaults fail closed.

Evidence: `internal/llm/model_catalog_test.go:TestModelDefaultReasoningEffortUsesOwnedMetadataAndLegacyFacts`,
`cmd/sta/model_catalog_data_test.go:TestReasoningEffortCatalogIsProjectedToProviderAndWebRows`,
`internal/webserver/native_rpc_test.go:TestNativeLLMCatalogPreservesOwnedModelMetadata`.

### Latest model-catalog correction (2026-09-01, default output budget)

The model catalog now distinguishes DSH's resolved request default
`defaultMaxTokens` from the model's advertised `maxTokens` capacity. DeepSeek
uses the reference connection default `256000` for exact and unlisted route
resolution; an explicit runtime/session budget remains authoritative. The
value is carried through the custom-model persistence shape, composition
capability, CLI/ACP loop construction, Native catalog/discovery, and Web model
rows. A model without an explicit default uses the reference route fallback
32768 for the non-DeepSeek adapters; capacity remains metadata only.

Evidence: `internal/llm/deepseek/model_catalog_test.go:TestStreamMaterializesProviderDefaultMaxTokens`,
`cmd/sta/model_catalog_data_test.go:TestDeepSeekReferenceCatalogMetadata`, and
the targeted provider/Native/Web regression suite.

### Latest projection correction (2026-09-01, SDK session snapshot)

The optional SDK `session/snapshot` method now returns a durable session
projection rebuilt by `projection.Build`, covering history, the current
trajectory surface, title/session-list metadata, plan/todo, approvals,
feedback, jobs, and MCP activity. This makes SDK query state use the same
cold-restart authority as Native/Web while preserving the reference SDK
request map unchanged. The method is included in the local generated schema
as a shutu extension; the complete cross-entry equivalence fixture and ACP
permission/reconnect projection remain open.

Evidence: `cmd/sta/sdk.go`, `internal/sdkclient/client.go`,
`internal/sdkclient/types.go`, `cmd/sta/sdk_test.go:TestSDKServerExternalClientPromptRunsAgentThroughIdle`,
`tools/generate-sdk-protocol-schema.mjs`.

## Latest sandbox correction (2026-09-01, controlled shell resource ceilings)

`RunRequest` now carries optional hard memory, file-size, and process-count
ceilings for controlled shell modes. Zero uses finite provider defaults of
512 MiB, 64 MiB, and 256 processes; explicit `danger-full-access` remains
unrestricted by these fields. On POSIX, the provider installs both soft and
hard `ulimit` values before executing the user command, so fork/exec
descendants inherit limits that cannot be raised by the command itself. The
real file-size negative fixture is
`internal/code/code_test.go:TestControlledShellEnforcesHardFileLimit`; it is
skipped on the current Windows host because the restricted-token backend is
unavailable and the POSIX mechanism is not applicable there.

This is a backend-enforcement improvement for A3.1, not completion: runnable
memory/fork-bomb/hostile-descendant fixtures on every claimed POSIX backend,
and a Windows Job Object/ACL resource validation on a host where
`CreateRestrictedToken` succeeds, remain open.

## Latest model-catalog correction (2026-09-01, request-default boundary)

The LLM route now keeps the DSH distinction between model capacity and a
deployment-chosen request default. A catalog row that only declares
`maxTokens` no longer causes CLI/ACP/loop assembly to send that capacity as a
per-request cap; an explicit `defaultMaxTokens` is the only catalog value
materialized at that seam. Anthropic, Gemini, OpenAI Responses, and the
OpenAI-compatible adapter also consume that explicit model default at the wire
boundary. When neither the model nor the request declares a default, all four
non-DeepSeek wire families now use the reference pi-ai route fallback of
32,768. The OpenAI-compatible adapter no longer inherits DeepSeek's special
256K fallback; it uses its own generic route fallback.
Persisted builtin/custom provider profiles may also set a route-level
`default_max_tokens`; registration passes it to the selected protocol adapter,
and the provider cold-restart fixture verifies that it survives restart and
reaches the wire.

Focused model/catalog and provider wire tests pass, including unlisted-model
route-default cases. A6.3 remains partial because the full upstream catalog,
remote refresh/failure/cancellation behavior, and complete unsupported-
capability matrix are still not proven.

## Latest ACP correction (2026-09-01, permission wire round trip)

ACP permission approval is now covered at the exported external wire seam, not
only by package-internal response routing. The regression drives a real
`session/new` and `session/prompt`, observes the server-initiated
`session/request_permission`, returns the matching JSON-RPC response on the
same input stream, and verifies that the prompt resumes and emits
`stopReason=end_turn`.

Evidence: `internal/acp/contract_external_test.go:TestExternalACPPermissionWireContract`.
This closes an evidence gap for permission settlement only. A7.1 remains
partial until the full pinned ACP client matrix and post-disconnect durable
state/side-effect oracle are executed.

## Latest SDK correction (2026-09-01, external snapshot query)

The production SDK child-process regression now queries `session/snapshot` over
the real exec/stdio transport after the prompt reaches idle. It verifies the
returned durable session id plus non-empty history, trajectory surface, and
projection cursor, strengthening A8.1/A9.1 beyond the in-process transport
test. This remains projection evidence only; the complete cross-entry byte
equivalence and restart oracle are still required.

Evidence: `cmd/sta/sdk_test.go:TestSDKClientDrivesRealRuntimeChildThroughIdle`.

Current gate status (2026-09-01): the register contains 47 tasks — 34 done,
10 partial, and 3 open — so 13 remain non-done. Twelve are required release
blockers; A8.3 is optional per the task scope. The generated report remains
`fail`/`claimAllowed=false`. The full Go regression, `go vet ./...`, register
lint, report generation, and diff check pass; the release gate remains closed
because the remaining items are capability-equivalence gaps, not test failures.

## Latest MCP correction (2026-09-01, reconnect generation retirement)

MCP reconnect now tracks in-flight operations by connection generation. A
replacement generation may start after transport loss, but the retired
generation is not passed to its context-free `Close` until all calls owned by
that generation have returned. Requests admitted after the swap are tracked
separately, so active traffic on the replacement cannot delay retirement of
the old transport. `TestReconnectingClientRetiresOldGenerationAfterInFlightCall`
covers the asynchronous-loss race.

This closes a concrete reconnect lifecycle race in A7.2. The full pinned MCP
stdio/Streamable HTTP external matrix, side-effect oracle, and release-gate
fault/restart suites remain required.

## Latest host-boundary correction (2026-09-01)

ACP CWD, FS persistence, Terminal workspace/lifecycle, code sandbox path
admission, fssearch, pwsh, and Web workspace canonicalization now share
`internal/pathsecure`. On managed Windows, a denied `EvalSymlinks` is accepted
only after every existing path component is checked for a reparse point;
reparse points and unresolved access still fail closed. PowerShell starts from
the agent directory and performs a literal in-process `Set-Location` because
the same host rejects an otherwise accessible temp directory when supplied as
the native process cwd. The change restores stable host behavior without
claiming that the local code backend provides hostile-code isolation.

## Latest ACP resume correction (2026-09-01)

Production ACP resume metadata now reports the last durable event sequence as
`eventCursor`, rather than the count of restored events. This matters after
non-contiguous durable sequences such as compaction/replacement. The SQLite
factory regression `cmd/sta/acp_test.go:TestACPFactoryResumeRestoresDurableIdentityCWDHistoryAndCursor`
drives `session/new → append → close → session/resume` and verifies identity,
CWD, restored history, durable cursor, and cleanup. A7.1 remains partial because
the complete external ACP/reference client matrix and post-disconnect
side-effect oracle are still required.

## Latest MCP correction (2026-09-01, transactional tool resync)

Reconnect-time MCP tool discovery now validates duplicate public names before
mutation and publishes the replacement generation transactionally. If a later
tool conflicts with another registry owner, newly registered tools are removed
and same-name replacements are restored, so the previous live generation stays
callable and the published-name index does not become stale.

Evidence: `cmd/sta/mcps_test.go:TestMCPReconnectResyncReplacesPublishedGeneration`
and `cmd/sta/mcps_test.go:TestMCPReconnectResyncKeepsPreviousGenerationOnConflict`.
This fixes a concrete availability gap in A7.2; the full external MCP
stdio/Streamable HTTP matrix and side-effect/restart oracle remain required.

## Latest MCP correction (2026-09-01, request cancellation scope)

Streamable HTTP caller cancellation is now request-scoped: it returns the
caller cancellation without retiring the negotiated MCP session. A later
request reuses the same session, and the reconnect supervisor does not replace
a healthy generation solely because a caller aborted one request.

Evidence: `internal/mcp/http_test.go:TestStreamableHTTPClientCallerCancellationKeepsSession`.
Transport failures and client-owned bounded timeouts remain the recovery
triggers; the full external MCP fault/side-effect matrix remains required.

## 2026-09-01 A4.4 complete tool contract and rich replay

- Added `cmd/sta/tool_contract_matrix_test.go`. It assembles every required
  model-facing production tool from its owning package, then drives each one
  through the same public Registry. The matrix proves disabled admission,
  schema-invalid admission, and approval/pre-execute denial boundaries without
  entering a tool body. It also requires exactly one terminal `ResultHook`
  observation per rejection.
- Each approval-boundary rejection is normalized into a canonical
  `turn/start -> step/start -> tool/call -> tool/error result -> step/end ->
  turn/end` sequence with source linkage. A fresh session restores those events
  and verifies the stable call ID, output, and DSH error code.
- The new matrix exposed a real Registry bug: an unknown merge-extensible rich
  block such as `audio` or `resource` was rejected even when `ContentBlock.Raw`
  contained the lossless wire representation. The registry now accepts unknown
  kinds only with `Raw`; a block whose raw bytes were stripped remains invalid.
- The rich replay test proves ordered image, audio-with-duration metadata, and
  resource-with-version metadata survive Registry normalization, canonical
  tool/result append, restore, and derived provider history. Existing ACP, MCP,
  Code Mode, and session suites remain the transport-specific guards.

Evidence: `cmd/sta/tool_contract_matrix_test.go`,
`internal/tools/tools.go:validateContentBlocks`, plus the focused ownership,
cancellation, rich-content, and per-domain tests already referenced by A4.4.
A4.4 moves to done. Overall equivalence remains fail-closed for A3.1-A3.3,
A6.3, A7.3, A8.1, and A9.1-A9.5.

## 2026-09-01 A3.2 unavailable Code Mode cross-entry closure

- Added `TestCodeUnavailableFailClosedAcrossEntries` as one production
  composition regression for an unproven Node permission runtime. It proves
  that `registerCode` keeps the engine nil, removes `run_code` from the
  registry and whitelist, and preserves the probe failure reason.
- The same unavailable state is checked at each claimed entry: Native standard
  projection omits the tool, Web reports `code_available=false`, omits
  `run_code` from its enabled tools, ACP omits it from its catalog, SDK omits
  it from its catalog, and a direct registry call returns stable
  `UNKNOWN_TOOL`.
- Existing Native preset, Web settings, capability, and workspace-mode tests
  continue to guard the corresponding selection/admission negative responses.

This closes A3.2 as a fail-closed contract. It does not claim Code Mode is
equivalent when unavailable, nor does it provide an enforcing sandbox backend;
A3.1 remains separately open for that enforcement work.

## 2026-09-01 A6.3 complete pinned model catalog inventory

- Replaced the hand-picked non-DeepSeek route subset with an embedded catalog
  generated from the reference-pinned `@earendil-works/pi-ai@0.82.1` provider
  data. The baseline contains 38 upstream providers and 1,234 model rows with
  owned id/name/context/output/input/reasoning facts, non-null effort wire
  maps, and deterministic first efforts.
- Kept the checked-in curated rows as an overlay, not a second authority. The
  merge key is provider/model ID; Shutu-owned protocol tool facts, DeepSeek
  request defaults, and route fallbacks augment the upstream row without
  dropping upstream-only routes. Model IDs absent from the pinned upstream
  inventory are no longer advertised merely because they occurred in a legacy
  subset.
- Model discovery for a built-in route now returns these owned rows instead of
  ID-only suggestions. New tests pin the package source, complete coverage,
  DeepSeek defaults, curated tool facts, upstream GPT-5 effort facts, and
  Groq discovery capacities.

Evidence: `cmd/sta/model_catalog_generated.json`,
`cmd/sta/model_catalog_data.go`,
`cmd/sta/model_catalog_data_test.go:TestGeneratedModelCatalogCoversPinnedUpstream`,
and `cmd/sta/discover_test.go:TestDiscoverCatalogRoute`. A6.3 moves to done.
This is static installed-catalog parity; A7.3 still owns the broader external
wire/reference matrix and audio behavior.

## 2026-09-01 A7.3 stable unsupported request-content boundary

- Added `ValidateRequestBlocks` to the provider-neutral message seam. Core
  request blocks are text, reasoning, image, tool-call, and tool-result.
  Audio, resource, vendor, and other merge-extensible blocks may remain durable
  for projection, but are rejected before credential resolution, attachment
  reads, image offloading, or network I/O.
- Every claimed production provider now returns the stable typed code
  `UNSUPPORTED_INPUT_CONTENT` for such input. The shared cross-provider test
  covers DeepSeek, OpenAI-compatible, Anthropic, Gemini, and OpenAI Responses
  adapters, including nested unsupported blocks in tool-result content.
- This separates durable rich-block preservation from provider admission: a
  registry can preserve audio/resource metadata for replay without a provider
  silently dropping it during request serialization.

Evidence: `internal/llm/message.go`,
`internal/llm/provider_unsupported_input_test.go:TestEveryProviderRejectsUnsupportedRequestInput`,
plus the existing ACP audio admission and image negative suites. A7.3 remains
partial only for the broader external reference/provider-header replay matrix.

## 2026-09-01 A9.2 SQLite-backed all-tool negative oracle

- Upgraded the A4.4 required-tool negative matrix from an in-memory replay
  oracle to production SQLite. For every required model-facing tool, the
  approval-boundary rejection is committed through `Store.AppendEvents` as the
  canonical `turn/start -> step/start -> tool/call -> tool/error result ->
  step/end -> turn/end` sequence.
- The test reloads that session with `Store.LoadSession`, verifies lifecycle
  validation and event count, byte-compares each durable event, and confirms
  the stable tool call ID, terminal output, and structured error code survive
  cold replay. Because the rejection is settled before the tool body enters,
  the absence of any tool-side durable fact is part of the expected oracle.

This advances A9.2 from representative durable-state coverage to every required
model-facing tool. It does not close A9.2: cross-process external-side-effect
oracles still cover representative worker/network cases rather than every
production dependency.

## 2026-09-01 A8.1 permission and reconnect projection migration

- Extended the shared `projection.Snapshot` with the current durable
  `Permission` and `SandboxMode` tiers. `permission/preset` and
  `sandbox/mode` now fold through the same live Cursor and cold Build path;
  malformed permission/sandbox control facts fail closed instead of falling
  back to a safe-looking default.
- Native projection changed values and the history baseline now source the
  permission tier from `projection.Snapshot` when a durable fact exists. The
  session-config permission remains only a fallback for sessions with no
  durable permission event.
- ACP `session/resume` now validates the restored event stream through
  `projection.Build` before composing the runtime. `ResumeMetadata` reports
  `eventCursor` from `projection.Snapshot.AsOfSeq` rather than scanning raw
  events privately, so a projection-invalid durable transcript cannot resume
  with an invented cursor.

Evidence:
`internal/projection/projection_test.go:TestBuildFoldsPermissionPresetAndSandboxMode`,
`internal/webserver/native_rpc_test.go:TestNativeHistoryPermissionUsesSharedProjectionOverConfigFallback`,
and
`cmd/sta/acp_test.go:TestACPFactoryResumeRejectsProjectionInvalidDurableEvent`.
This advances A8.1; the complete native/SDK/ACP trajectory/control-state
equivalence fixture remains outstanding.

## 2026-09-01 A9.3 JSONL disk-full and process-death restart matrix

- Added a bounded write-fault seam to JSONL's physical append. It changes no
  bytes and bypasses no validation; production leaves it disabled. The release
  matrix uses it to model ENOSPC and abrupt process death at a record boundary.
- The new cross-process oracle starts a real helper process over a committed
  five-event transcript. For ENOSPC at the second physical record, the helper
  returns through the normal rollback path and a reopened handle observes the
  exact five-event file.
- For process death after the first complete record, the reopened handle
  observes the sixth `turn/start` record in the external file; `Load` then
  deterministically appends the interrupted-turn closer. In both cases the
  post-restart load preserves the committed prefix and never resurrects the
  second failed event.

Evidence: `internal/persistence/jsonl_test.go:TestJSONLWriteFaultSettlesAcrossProcessRestart`.
This closes the disk-full and kill-at-every-write JSONL subcases; process-tree
exhaustion, network boundary, hostile-worker, and claimed-platform matrix work
remain for A9.3.

## 2026-09-01 A9.3 HTTP cross-origin network boundary oracle

- Added a network-boundary leg to the A9.3 fault matrix. The parent runs real
  HTTP source/target endpoints, while the fetch itself runs in a real second
  test process through the production `web.NewHttpFetchProvider`.
- The source redirects cross-origin to a different loopback target. The child
  settles with the stable `redirect-blocked` boundary, and the target endpoint
  never writes its contact sentinel.
- After the child settles, an independent JSONL storage handle reloads the
  session and proves the three-event committed prefix remains byte-stable; the
  network fault does not create a partial tool result or lifecycle record.

Evidence:
`internal/contractfixture/fault_security_matrix_test.go:TestFaultSecurityRestartMatrix/network_cross-origin_boundary_preserves_durable_prefix`.
This closes the A9.3 HTTP cross-origin network boundary subcase. Process-tree
exhaustion, hostile-worker, and claimed-platform fault work remain.

## 2026-09-01 A9.3/A3.3 active-process exhaustion enforcement

- Windows Job Objects for Code Mode and controlled shell runs now enforce a
  concurrent active-process ceiling in addition to CPU and kill-on-close. The
  TypeScript worker uses a 16-process backstop even though the Node permission
  model denies child-process APIs; controlled shell runs forward the configured
  `MaxProcesses`, while explicit full-access runs receive no resource ceiling.
- Added a real Windows process-tree exhaustion oracle. The parent attaches a
  two-process Job Object to a real child, validates the kernel limit, and
  releases the child only after the job is attached. The child attempts four
  concurrent descendants and reports that exactly one was admitted; the job
  then terminates the survivor and the direct child.

Evidence:
`internal/code/process_tree_windows_test.go:TestProcessTreeEnforcesActiveProcessLimit`.
This closes the Windows Job Object process-tree exhaustion subcase. Runnable
POSIX fork-bomb fixtures, hostile-worker restart oracles, and claimed-platform
matrix work remain.

## 2026-09-01 A8.1 shared session-query relation projection

- Added `projection.EventRelations` as the validated authority for replacement
  chains, shadowed events, and assistant/tool-call edges. It validates the
  complete durable stream through `projection.Build` before deriving any
  relation and uses the same `session.SurfaceReplacement` vocabulary as the
  canonical history/surface fold.
- Migrated `session_event_trace` to this shared API and removed its private
  `surfaceOp`, replacement-chain, and tool-call relation parsers. The model
  tool continues to present the same bounded text shape, but relation semantics
  can no longer diverge between query, Native history, SDK, and cold replay.

Evidence: `internal/projection/surface_test.go:TestEventRelationsUsesValidatedSharedStream`,
`TestEventRelationsDerivesToolCallEdges`,
`TestEventRelationsRejectsInvalidDurableStream`, and the existing session-query
tool relation regression. A8.1 remains partial pending the full native/SDK/ACP
trajectory/control-state cross-entry fixture.

## 2026-09-01 A8.1 Team projection folded into shared Snapshot

- `projection.Snapshot` now owns optional Team board state. A durable
  `team/snapshot` checkpoint and later `team/member`, `team/task`,
  `team/message/queued`, and `team/message/delivered` events fold through the
  same cold `Build` and live Cursor path.
- Malformed or non-contiguous Team state now fails projection rebuild instead
  of silently exposing an empty board. Live Cursor append rolls back its Team
  fold if a prospective event cannot be admitted.
- Web session state now reads the optional Team projection from the shared
  Snapshot, so plan/goal state and Team board state have the same durable
  replay authority.

Evidence:
`internal/projection/projection_test.go:TestBuildFoldsTeamSnapshotAndIncrementalEvents`.
This advances A8.1; the remaining cross-entry trajectory/control-state
equivalence fixture is still required.

## 2026-09-01 A9.1 native CLI production cross-entry leg

- Added `TestNativeCLICrossEntryFixture`. The test builds the production
  `pa` binary, points it at a local OpenAI-compatible provider, and sends the
  shared cross-entry fixture prompt through native CLI stdin.
- The provider's first turn now invokes the production `write` tool. The
  oracle verifies the committed workspace file and durable `tool/result`, then
  requires a second model step for the fixture assistant response on stdout.
- The oracle requires the fixture assistant response on stdout. After the CLI
  exits, it opens the production SQLite database with an independent handle,
  loads the one native session, rebuilds the canonical projection, and requires
  the fixture assistant text as the final history entry plus non-empty `AsOfSeq`
  and surface.
- The first run exposed a malformed test-provider SSE body: its data lines were
  not separated by blank-line event terminators. That fixture was corrected
  before production passed; this is not a runtime provider change.

Evidence: `cmd/sta/native_cli_cross_entry_test.go:TestNativeCLICrossEntryFixture`.
This closes the native CLI execution and external-file-effect subcases.

## 2026-09-01 A9.1 child Agent production cross-entry leg

- Added `TestCrossEntryChildAgentFixture`. The shared lifecycle fixture now
  drives the production child runner: the child receives the fixture text,
  observes the fixture tool schema, invokes `fixture_echo`, receives the fixture
  tool output, and closes with the fixture assistant text.
- The child tool now commits one observable external effect with `fsync`, while
  a bounded temporary lock represents the in-flight operation. The oracle
  compares the exact committed effect, requires the lock to be removed after
  execution, and keeps the effect after provider close.
- The child owns an independent loop and SQLite session. The test verifies the
  durable header records the fixture parent, `origin=subagent`, and delegation
  depth one, then closes the provider before opening a separate SQLite handle.
- The independent handle reloads the committed child events, finds the fixture
  tool result, and rebuilds the canonical projection. The cold snapshot must
  have a non-empty cursor/surface, begin with the fixture user text, and end
  with the fixture assistant text.
- The child delegation API accepts text rather than rich content blocks, so the
  fixture image block is explicitly out of scope for this delegation leg; rich
  input remains covered by its transport/projection suites.

Evidence:
`internal/contractfixture/child_agent_entry_test.go:TestCrossEntryChildAgentFixture`.
This closes the child Agent execution and child-side effect/cleanup subcases.

## 2026-09-01 A9.1 reference replay recheck

- Re-enabled the opt-in reference gate with `DSH_REFERENCE_ROOT` and reran
  `TestCoreTurnReplayMatchesReference` against the read-only reference
  checkout.
- The real reference Session imported through its TypeScript source produced
  the same ordered surface and model-facing history as the Go fixture replay.
- The previously recorded Node 24.19.0 `uv_os_get_passwd` ENOMEM condition did
  not recur. The gate passed five consecutive runs; this replaces the stale
  environment blocker with current evidence rather than a general skip.

Evidence:
`internal/session/reference_replay_test.go:TestCoreTurnReplayMatchesReference`
and `scripts/verify-reference-replay.mjs`. At that checkpoint A9.1 remained
partial for the same-fixture side-effect/cleanup comparison; the closure entry
below supersedes it.

## 2026-09-01 A9.1 Web/ACP/SDK effect cleanup and closure

- Added Agent-backed Web and real SDK child legs using the shared fixture. In
  each, the fixture tool commits and fsyncs an external effect, removes its
  bounded lock, fails with the stable `TOOL_EXECUTION_ERROR` result, and the
  loop continues to the fixture assistant. Independent SQLite handles rebuild
  the same projection after settlement/child close.
- Bound the production ACP external-disconnect child to the same fixture. It
  commits the fixture filesystem effect, attempts an outside-workspace write
  that must settle as `tool/error`, survives real client disconnect and child
  exit, preserves the committed effect, and passes independent durable and
  resume/projection oracles.
- With native CLI, child Agent, SQLite/JSONL, protocol wires, and the
  five-run reference replay already green, A9.1 moves to done. This does not
  close the release: A3.1, A3.3, A7.3, A8.1, A9.2-A9.5 remain required
  blockers.

## 2026-09-01 A8.1 canonical projection closure

- Re-audited the remaining “native/SDK/ACP trajectory/control-state” item
  against current code. Native rich state is now a wire adapter: its durable
  plan/todo/goal/permission/session-list values and reconnect surface come from
  `projection.Snapshot`. SDK `session/snapshot` and notification surface
  metadata use the canonical rebuild/wire seam; ACP resume, assistant delivery,
  and compaction estimation consume the same projection.
- Session query replacement/tool relations use `projection.EventRelations`;
  history and live Cursor folds share `session.DeriveHistoryEvents`; Team,
  jobs, MCP activity, feedback, approvals, title/list metadata, permission, and
  sandbox state are rebuilt by the shared Snapshot.
- The previously outstanding cross-entry trajectory/control-state runtime
  evidence is supplied by the completed A9.1 suite, including native CLI,
  Agent-backed Web, real SDK child, production ACP disconnect/resume, child
  Agent, SQLite/JSONL, and independent cold-projection oracles.

Evidence: `internal/projection`, `internal/sessionquery`,
`internal/webserver/native_projection.go`, `cmd/sta/sdk.go`, `cmd/sta/acp.go`,
and the A9.1 cross-entry suite. A8.1 moves to done. The release remains
fail-closed for A3.1, A3.3, A7.3, and A9.2-A9.5.

## 2026-09-01 A9.3 consolidated generation fault matrix

- Added `TestGenerationRotationReloadAndReconnectMatrix` to consolidate three
  generation-boundary families into one release-gate oracle.
- Credential rotation now runs against two independent SQLite-backed Vault
  handles. The oracle proves an in-flight lease retains the old value, a new
  lease observes generation two, revocation rejects later acquisition, and the
  durable backend settles as deleted/revoked.
- Plugin reload exercises the real disposer boundary: the old generation is
  disposed, old and new tool effects are fsynced in order, and the replacement
  executes without retaining the previous generation.
- MCP reconnect starts two real child-process generations. The first child
  commits and fsyncs an external effect before dying; the replacement serves a
  new explicit call. The request/effect journals prove the failed operation is
  settled exactly once and is not replayed.

Evidence:
`internal/contractfixture/generation_release_matrix_test.go`. This closes the
A9.3 credential/plugin/MCP consolidation subcase. Hostile-worker injection,
runnable POSIX fork-bomb coverage, provider-wipe, and claimed-platform matrix
execution remain.

## 2026-09-02 A9.2 full negative boundary closure

- Instrumented the required-tool matrix at the production body boundary. All
  49 assembled model-facing tools now prove that disabled admission, bad schema,
  and policy denial return stable registry codes without executing the tool.
  Each normalized denial is still committed to SQLite, cold-replayed
  byte-for-byte, and checked for stable call ID/output/error code.
- Upgraded the provider unsupported-input matrix from a dead-address assertion
  to a real local HTTP endpoint with request/credential counters. All five
  claimed adapters (DeepSeek, OpenAI, Anthropic, Gemini, OpenAI Responses)
  return `UNSUPPORTED_INPUT_CONTENT` with zero provider requests and no
  credential header crossing the boundary.
- Re-ran the A9.2 full filtered command. Dedicated dependency evidence covers
  durable unknown-event rejection, sandbox unavailability, expired/cross-owner
  approval, disposed plugin generations, worker death, network loss and
  cross-origin blocking, MCP schema/auth faults and no-replay reconnect,
  credential lease/rotation/revocation, bounded process termination, and
  platform process-tree cleanup.

Evidence: `cmd/sta/tool_contract_matrix_test.go`,
`internal/llm/provider_unsupported_input_test.go`, and the registered
dependency matrices. A9.2 moves to done. Required blockers are now A3.1,
A3.3, A7.3, A9.3-A9.5 plus optional A8.3.

## 2026-09-02 A9.3 hostile Code Mode worker oracle

- Added `TestHostileCodeWorkerFaultMatrix` for the reference's hostile-peer
  worker contract. A real Node Code Mode worker first invokes the production
  host binding; that binding commits and fsyncs one external effect, then the
  worker emits a 20MiB oversized raw protocol frame.
- The production TypeScript runtime settles the run with one bounded
  `worker-exit` result, does not crash on the hostile frame, and invokes the
  host binding exactly once.
- The test records the canonical interrupted tool failure in SQLite, closes
  the first handle, and rebuilds through an independent handle. The cold
  lifecycle/projection contains exactly one stable `TOOL_EXECUTION_ERROR`
  receipt; the hostile frame cannot manufacture success or a second result.

Evidence:
`internal/contractfixture/hostile_worker_matrix_test.go`. This closes the
hostile-worker fault-injection subcase. A9.3 remains partial for runnable
POSIX fork-bomb/hostile-descendant coverage and claimed-platform matrix
execution.

## 2026-09-02 A3.3 Code Mode duplicate/cleanup lifecycle matrix

- Added the missing A3.3 worker-lifecycle oracles. A timeout and a hostile
  oversized raw-protocol frame each settle as exactly one typed terminal
  receipt, and neither leaves a generated `.shutu-ptc-*` program file in the
  sandbox.
- Added a forged duplicate host-call probe. The worker writes the same
  correlation frame twice through raw stdout; the production host admits the
  first call exactly once, rejects/ignores the duplicate, and still settles the
  program deterministically.
- The profile inventory now explicitly classifies the external Node
  permission-model subprocess as compatibility degradation until worker-thread
  compute/wall/heap/isolation parity is complete.

Evidence: `internal/code/typescript_lifecycle_matrix_test.go` and
`internal/profile/classification.go`. A3.3 remains partial for the
cross-platform worker isolation/resource-budget matrix and worker-thread parity.

## 2026-09-02 A7.3 pinned reference SDK runtime/client matrix

- Added a read-only reference source loader for Node. It indexes pinned
  workspace packages, maps missing `lib/index.js` seams to TypeScript source,
  and uses the reference checkout's own TypeScript compiler to support
  decorators and parameter properties without launching tsx/esbuild or writing
  build artifacts into the read-only reference.
- Added `TestPinnedReferenceSDKRuntimeExternalClient`. The real pinned
  reference SDK runtime runs in a Node subprocess while Shutu's external Go SDK
  client owns the protocol. The test proves initialize, durable prompt receipt,
  assistant output, running-to-idle settlement, and shutdown.
- The reference runtime calls a real local OpenAI-compatible provider. The
  oracle verifies exactly one provider request and pins model, max-token,
  system/user boundaries, and authorization. This closes the A7.3 reference SDK
  runtime/client leg.

Evidence: `cmd/sta/reference_sdk_runtime_test.go`,
`cmd/sta/testdata/reference_source_loader.mjs`, and the local provider endpoint.
A7.3 remains partial only for the reference ACP runtime/client matrix.

## 2026-09-02 A3.1 Windows ACL blocker correction

- Re-ran the real Windows ACL probe on the current managed host. It now fails
  at `grant Windows ACL workspace: Access is denied`, before
  `CreateRestrictedToken`; the stale registration that attributed the blocker
  to an invalid-token parameter did not match current host behavior.
- Inspecting user TEMP shows a `Modify`-only DACL for the account, without
  `WRITE_DAC`. `SetNamedSecurityInfo` therefore cannot add the deterministic
  workspace capability ACE. This is an enforcing fail-closed boundary: the
  backend correctly leaves controlled workspace/read-only modes unadvertised
  rather than falling back to full access.

Evidence: `internal/code/windows_acl.go:windowsACLProbe` and
`internal/code/windows_acl_test.go:TestWindowsACLWorkspaceWriteAndReadOnlyBoundaries`.
A3.1 remains partial; Windows controlled-mode validation requires a host that
permits DACL grants and permits `CreateRestrictedToken`.

## 2026-09-02 A9.4 cleanup-gate inventory and race-gate status

- Re classified A9.4 from open to partial after auditing the existing cleanup
  oracles. Provider generation retirement closes only after the final stream
  lease; credential leases drain and wipe revoked bytes; MCP client teardown
  does not leak goroutines; Code Mode close cancels host bindings and joins the
  worker; local provider close stops active processes; background-job and
  Windows process-tree cancellation have real child-process oracles; and SQLite
  abandoned job reservations recover across restart.
- Attempted the strict race boundary on the managed Windows host with
  `CGO_ENABLED=1`. Go cannot build the cgo/race toolchain because the required
  C compiler (`gcc`) is absent. This is recorded as unverified, not converted
  into a pass.
- A9.4 remains partial for a real Linux/Windows CGO race matrix and a
  consolidated cross-platform cleanup gate covering goroutines, file
  descriptors, child processes, workers, temporary files, SQLite locks,
  providers, and credentials.

## 2026-09-02 A7.3 all-provider header replay

- Added `TestEveryProviderHeaderReplay` as one shared real-HTTP boundary for
  DeepSeek official, OpenAI-compatible, Anthropic, Gemini, and OpenAI
  Responses.
- Each provider receives the same request fixture and streams a terminal
  `"header replay"` result through its native SSE shape. The oracle fixes POST
  endpoint/protocol query, auth scheme, stable User-Agent, streaming
  content-type, and finish mapping.
- The official DeepSeek route must carry the stable anonymous user, durable
  session, and compaction headers. Every non-DeepSeek route must omit all
  DeepSeek-specific identity headers. This closes the provider/header replay
  subcase at the real local HTTP boundary.

Evidence: `internal/llm/provider_header_replay_test.go`. A7.3 remains partial
for the pinned reference SDK/ACP runtime/client external matrix.

## 2026-09-02 A7.3 pinned reference ACP runtime/client matrix

- Added `TestPinnedReferenceACPRuntimeExternalClient`. The pinned reference
  ACP plugin and agent-loop test dependencies run in a real Node subprocess
  through the existing read-only TypeScript source loader. The reference
  checkout remains unmodified and receives no generated build artifacts.
- Shutu drives that subprocess with a raw JSON-RPC ndjson client, not an
  internal adapter. The oracle pins initialize identity
  (`deepseek-harness-acp` / `0.0.1`), `session/new`, the assistant
  `session/update` chunk with its exact text, `stopReason=end_turn`, client
  stdin EOF, runtime quiescence, and a zero-exit child.
- Reconnect is not claimed by this leg: the pinned reference ACP runtime owns
  one connection lifecycle. Production disconnect, reconnect, and resume
  behavior remain covered separately by the completed A7.1 external matrix.

Evidence: `cmd/sta/reference_acp_runtime_test.go` and
`cmd/sta/testdata/reference_source_loader.mjs`. A7.3 is done. Required blockers
are now A3.1, A3.3, A9.3-A9.5 plus optional A8.3.

## 2026-09-02 A3.3 queued-call abandonment semantics

- Code Mode settlement now drains bounded host replies before OS shutdown. An
  in-flight host call observes cancellation; a queued-unstarted call receives
  `run_code run is over (run_code settled); <tool> tool call abandoned` without
  entering the host binding. This removes the prior race where the worker died
  before its queued promise could settle.
- The terminal-result owner remains single. Post-terminal `done` and `call`
  frames are ignored; only bounded post-settlement log output is drained, and
  exceeding that budget forces process shutdown.
- `TestTypeScriptRuntimeAbandonsQueuedCallAtSettlement` proves one exclusive
  in-flight effect, zero bindings for the queued effect, explicit worker-side
  abandonment, one program exception, and no generated program file.

Evidence: `internal/code/typescript_lifecycle_matrix_test.go` and the A3.3
focused suite (`internal/code`, `internal/tools`, `internal/profile`, and
`cmd/sta`). A3.3 remains partial for the cross-platform worker
isolation/resource-budget matrix and worker-thread parity.

## 2026-09-02 A3.3 Linux/Windows worker matrix closure

- Added `TestProcessTreeKillsBoundedForkBombGroup`. On Linux, a bounded
  four-level fork creates descendants in the worker's owned process group; the
  runtime's kill-tree leaves the group unaddressable. This uses the ownership
  semantics of a fork bomb without risking an unbounded host explosion.
- Added `TestTypeScriptRuntimeDeniesRealAmbientProcessAndWorkerEffects`.
  Model-authored code invokes the real child-process and worker-thread APIs;
  the Node permission model rejects both before an ambient effect starts. The
  same test passes on Windows and Linux.
- Cross-compiled `internal/code` and executed its full test binary on
  AlmaLinux 8.10 with user-level Node 24.19.0. The same suite passes on
  Windows with Node 24.19.0. Coverage includes timeout, hostile oversized
  frame, worker death, heap/output quotas, CPU/wall budgets, forged duplicate
  call admission, queued-unstarted abandonment, cancellation, close join,
  empty environment, and cleanup.
- Registered DSH's worker-thread transport as an internal architecture
  exclusion. `ctx.codeRuntime` now explicitly describes Shutu's external Node
  permission-model process as an architecture substitute: same observable
  budgets, explicit ambient child/worker denial, and stronger process-level
  ownership. A profile descriptor regression rejects the old
  "compatibility degradation" wording.

Evidence: `internal/code/process_tree_unix_test.go`,
`internal/code/typescript_lifecycle_matrix_test.go`,
`internal/profile/classification.go`, and
`internal/profile/profile_test.go`. A3.3 is done. Required blockers are now
A3.1 and A9.3-A9.5 plus optional A8.3.

## 2026-09-03 A3.1 containment and resource closure

- Windows ACL closure was retested on the replacement development host.
  `CreateRestrictedToken` and its production read-only restricting-SID set pass,
  the token has three zero-attribute restricting SIDs, native `DeleteFileW` /
  `RemoveDirectoryW` pass in workspace, readonly/outside deletes return
  `ERROR_ACCESS_DENIED`, timeout/cancel cleanup is idempotent, crash recovery
  restores the exact raw security descriptor, and the three fault phases recover
  idempotently. The restoration now persists the semantic DACL control word and
  uses `SetFileSecurityW` only when `SetNamedSecurityInfoW` normalizes
  `SE_DACL_AUTO_INHERITED`.
- Linux bwrap hard resource limits now use `prlimit --as --fsize --nproc`
  before exec instead of a second shell `ulimit` inside the namespace. A host
  without `prlimit` no longer advertises controlled bwrap modes. Real Linux
  fixtures cover read-only denial, network-interface hiding, file-size, memory,
  bounded fork bomb, owned process-group teardown, credential-shaped
  environment scrubbing, and the hostile oversized Node frame.
- Windows Job Objects now enforce per-process memory in addition to CPU and
  active-process ceilings. `TestProcessTreeEnforcesProcessMemory` starts a real
  child, configures a 64 MiB ceiling, releases it to allocate/write 128 MiB, and
  proves the kernel terminates it. A controlled-shell oracle proves a
  credential-shaped parent variable does not enter either the Windows or Linux
  child.
- Startup recovery now treats a crash journal whose workspace path no longer
  exists as a safe no-op and finalizes/removes the journal. This prevents a
  deleted test or temporary fixture from blocking every later startup. The
  regression is `TestWindowsACLRecoveryFinalizesMissingWorkspaceJournal`.
- The Windows A3.1 registered command passes across `internal/...`; the Linux
  cross-compiled `internal/code` A3.1 matrix passes in Ubuntu 22.04 WSL with
  bubblewrap 0.6.1 and Node 24.19.0. The Windows backend remains explicitly
  containment-only: strong/network isolation stays fail-closed rather than
  being claimed. Overall equivalence remains fail-closed for A9.3-A9.5 plus
  optional A8.3.

Evidence: `internal/code/local.go`, `internal/code/windows_acl.go`,
`internal/code/windows_acl_test.go`,
`internal/code/windows_acl_delete_diagnostic_test.go`,
`internal/code/windows_acl_restricted_token_diagnostic_test.go`,
`internal/code/process_tree_windows_test.go`,
`internal/code/process_tree_unix_test.go`, and
`internal/code/code_test.go`.

## 2026-09-03 A8.3 selected/optional profile contract closure

- The profile registry now has explicit enforcing and replay descriptors for the
  selected SQLite storage, local filesystem, and durable session-reference
  profiles. The same authority marks e2b, Python, Cordis dynamic runner, and
  Cordis inspect unsupported with concrete reasons; `Registry.Use` rejects each
  one with `ErrProfileUnsupported`.
- Native Web RPC regression covers `runtime.profiles`, selected
  `runtime.profile`, unsupported `runtime.profile`, and both Cordis methods.
  Optional capability inventory remains aligned to the same unsupported
  descriptors and returns `capability-unsupported`, while unknown capabilities
  remain `capability-unknown`.
- The dependency issue in the old A8.3 note is stale in a useful way only
  historically: A3.2 is now done. The registered A8.3 command passes across all
  `internal/...` packages and `cmd/sta`.

Evidence: `internal/profile/profile.go`,
`internal/profile/profile_test.go`,
`internal/profile/classification_test.go`, and
`internal/webserver/runtime_profile_test.go`. A8.3 is done. Required blockers
are now only A9.3-A9.5.

## 2026-09-03 A9.3 fault/security matrix closure

- The registered A9.3 command passes on Windows and Linux. The Linux run uses
  cross-compiled `internal/contractfixture`, `internal/persistence`,
  `internal/store`, and `internal/code` test binaries in Ubuntu 22.04 WSL with
  bubblewrap and Node 24.19.0.
- Verbose Windows and Linux runs confirm the parent matrices execute SQLite
  death recovery, workspace symlink escape, HTTP cross-origin denial, JSONL
  disk-full/process-death recovery, credential rotation, plugin generation
  reload, MCP reconnect, and the hostile Code Mode oversized-frame oracle. The
  only SKIP rows are direct invocations of helper-process test entry points; the
  parent tests launch those helpers.
- Combined with A3.1, real process oracles now include bounded POSIX fork bombs,
  owned process-group teardown, and Windows Job Object CPU/active-process/memory
  termination.

Evidence: `internal/contractfixture/fault_security_matrix_test.go`,
`internal/contractfixture/generation_release_matrix_test.go`,
`internal/contractfixture/hostile_worker_matrix_test.go`,
`internal/persistence/jsonl_test.go`,
`internal/store/sqlite_test.go`,
`internal/process_tree_unix_test.go`, and
`internal/process_tree_windows_test.go`. A9.3 is done. Required blockers are
now only A9.4-A9.5.

## 2026-09-03 A9.4 strict race and cleanup-gate closure

- The registered strict race command now passes on Windows and Linux with
  `CGO_ENABLED=1`. Windows uses the local CGO toolchain; Linux uses Ubuntu
  22.04 WSL with Go 1.26.7, gcc 11.4, bubblewrap, and Node 24.19.0. Both runs
  execute every package with `-count=1`.
- The race matrix exposed four real defects. `programCallGate` now wakes the
  next waiting call after safe admission, not only after release; without that
  wakeup, independently safe Code Mode bindings could serialize depending on
  goroutine scheduling. `scriptedLLM` in the subagent tests now serializes call
  and step mutation. The Unix persistent terminal uses non-interactive Bash
  with process-group ownership and platform-correct command submission. ACP/SDK
  transports ignore only SIGPIPE so a closed peer stdout returns EPIPE through
  normal shutdown instead of killing the process.
- Additional closures from the cross-platform run: `RunCommand` drains stdout
  and stderr before `cmd.Wait`, preventing `Wait` from closing `StdoutPipe`
  while capture is still reading; the native host WebSocket server now tracks
  handler completion and waits during `Close`; skill project-root discovery
  accepts an explicit test boundary; and the SQLite dependency is upgraded to
  modernc.org/sqlite v1.50.0 to eliminate intermittent Windows close/handle
  retention.
- Existing owning-package oracles continue to prove bounded goroutines, file
  descriptors, child processes, Code Mode workers, temporary files, SQLite
  locks, provider generations, and credential drain/wipe. A9.4 is done; A9.5 is
  the only remaining required register blocker.

## 2026-09-03 A9.5 final capability-equivalence release gate

- The final release prerequisites passed on the current worktree: full Go
  tests, `go vet ./...`, `go build ./...`, Web tests/build/manifest
  verification, Windows/Linux strict race, and the pinned reference replay.
- The audit reference root points to a repository-local ignored checkout at
  `.gocache/reference/deepseek-harness`, pinned to
  `141eb6fef83422698aef7a981029e843e8161534` (`dsh-v0.1.0-rc.8`). This avoids
  mutating the user-owned rc.7 checkout while supplying the exact Web tool
  dependencies required by the production-style replay/build harness.
- The final gate reaches catalog export but is blocked by the host machine-level
  Windows Error Reporting LocalDumps policy before the user/administrator
  directly removed that empty local machine policy key; its export is retained
  outside version control at `.gocache/WER-LocalDumps-backup.reg`. The default
  `crash_dump_policy: disabled` then started normally, without using the
  non-equivalent `external` profile to bypass the gate.
- With that external state corrected, the complete equivalence gate passed:
  register lint/report agreement, diff/format, full Go tests, vet/build,
  production catalog export/verify, Web tests/build/manifest, Linux/Windows
  cross-builds, strict CGO race, and pinned reference replay. The manifest is
  now `status: pass`, `claimAllowed: true`, with no required open blockers.
