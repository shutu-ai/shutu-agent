# Agent 工作指南

本文维护 `shutu-agent` 的稳定开发约束、支持范围和 DeepSeek Harness 等价审计口径。
历史进度以机器可读的 manifest/task register 为准，不在本文重复维护完成数量。

## 目标范围

- 项目遵循 DSH 的核心行为原则：薄核心、事件日志为事实、能力通过明确 Service/Provider/Tool 接缝接入。
- 等价判断以外部可观察行为为准：事件顺序、历史派生、工具 schema/result/error、权限、取消、重启恢复、owner 隔离和协议响应必须一致或有明确声明的归一化。
- 允许因 Go 与 Cordis/Node 架构不同而采用不同内部实现；内部架构不同本身不是缺陷，但不能因此掩盖安全、生命周期、失败语义或跨入口行为差异。
- Team/`ctx.agentTeams`、`team_task_*` 是可选能力，不属于当前 release blocker。未选定的 E2B、Python、LSP、持久终端、Codex/Claude provider，以及动态 Cordis/client module/HMR 专属能力，必须在 profile/manifest 中明确标为 optional 或 out-of-scope。
- 即使排除上述架构专属能力，核心 Agent loop、会话历史与 replay、工具策略、approval、sandbox fail-closed、subagent owner、持久化恢复以及已声明的 ACP/MCP/SDK/Web 协议仍属于验收范围。
- `deepseek-harness` 是只读参考目录，严禁修改其中任何文件；参考版本以 [`docs/equivalence-manifest.yaml`](docs/equivalence-manifest.yaml) 固定的 commit 为准。
- P36 当前 Windows 目标范围已完成；Linux/WSL 只有在被 manifest 声明为 claimed platform 时才属于发布验收门槛。

## 等价审计规则

- 工具名、能力 ID、catalog 或路由存在，只能证明表面库存对齐，不能证明能力等价。
- 架构差异可以从目标 profile 中排除，但必须同步更新 manifest、task register、status 和验证命令，不能仅在报告文字中忽略。
- 支持的能力必须覆盖成功、拒绝、未知、超时、取消、权限不可用、owner 已释放、进程死亡和重启恢复等负路径；无法证明的能力必须返回稳定的 unsupported/unavailable/fail-closed 结果。
- 默认 sandbox 和高风险工具必须在实际 backend 上执行策略；上下文 metadata 或 capability 声明不能替代隔离、网络、进程树、资源和输出限制。
- 所有外部副作用必须有明确的 at-most-once、retryable receipt 或审计失败语义，进程重启不能无条件重放未确认的副作用。
- Team 可不实现，但若启用，仍必须遵守 owner、roster、mailbox、task DAG、CAS 和生命周期规则；“可选”不等于可以伪装成已支持。
- `claimAllowed` 只有在保留范围内没有 partial/open/blocked blocker，且全量测试、reference replay、平台安全验证和 race/资源门禁都通过时才能设为 `true`。

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
2. 阅读 [`docs/design.md`](docs/design.md)、相关 ADR 和 [`docs/dsh-equivalence-contract.md`](docs/dsh-equivalence-contract.md)；需要对齐 DSH 行为时，以 `../deepseek-harness` 的源码和文档作为只读参考。
3. 涉及核心模型、事件、循环、依赖方向、安全边界或支持 profile 时，先更新设计基线/ADR，并同步 manifest 与 task register 的范围。
4. 先补测试，再实现；按 Service、Provider、Tool 拆分能力，并覆盖取消、重启、失败、权限、owner 和 unsupported 边界。
5. 完成后运行相关测试和全量验证；文档变更至少校验链接、JSON/YAML、profile 范围、任务状态和证据路径一致性。
6. 提交前检查 `git diff --check`、工作区状态、敏感信息、参考仓库未修改，以及没有把环境失败误记为测试通过。

## 常用验证命令

```powershell
go test -count=1 ./...
go vet ./...
go build ./...

cd web
pnpm.cmd typecheck
pnpm.cmd test
pnpm.cmd verify
pnpm.cmd e2e
```

等价审计命令：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/equivalence-register-lint.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/equivalence-gate.ps1
```

严格 race、reference replay 或 claimed platform 缺少工具链时必须记录为 `unverified`，不能记录为 `passed`。`claimAllowed: false` 时 release gate 主动拒绝等价声明是预期行为。

API key 示例：

```powershell
$env:DEEPSEEK_API_KEY = "..."
go run ./cmd/pa
```

Windows 目标环境的发布、部署和回滚验证按
[`docs/deployment.md`](docs/deployment.md) 与
[`docs/p36-deployment-runbook.md`](docs/p36-deployment-runbook.md) 执行。

## 文档入口

- 等价范围与状态：[`docs/equivalence-manifest.yaml`](docs/equivalence-manifest.yaml)
- 等价任务登记：[`docs/equivalence-task-register.yaml`](docs/equivalence-task-register.yaml)
- 等价契约：[`docs/dsh-equivalence-contract.md`](docs/dsh-equivalence-contract.md)
- 等价状态说明：[`docs/dsh-equivalence-status.md`](docs/dsh-equivalence-status.md)
- 原有 Web 任务/验收记录：[`docs/dsh-web-parity-tasks.md`](docs/dsh-web-parity-tasks.md)、[`docs/dsh-web-parity-acceptance.md`](docs/dsh-web-parity-acceptance.md)
- 设计基线：[`docs/design.md`](docs/design.md)
- ADR：[`docs/decisions/`](docs/decisions/)
- 部署说明：[`docs/deployment.md`](docs/deployment.md)
- DSH 参考：`../deepseek-harness/docs/`
## Current audit state (2026-09-01)

The capability-equivalence claim remains fail-closed. Runtime profile and capability advertisements must be derived from the same host probes used by execution: the TypeScript Code Mode profile requires Node's permission model, and the controlled shell/sandbox profile requires a proven workspace-write backend. An explicit full-access subprocess is not evidence that the default sandbox profile is available. The native/Web agent-preset catalog must also hide `code` and reject selecting it whenever `run_code` was not actually registered; configuration preference alone is not capability evidence.

On the current managed Windows host, the controlled-shell blocker is more precise: user TEMP carries only `Modify` (not `WRITE_DAC`), so `SetNamedSecurityInfo` cannot add the workspace capability ACE and returns Access Denied before `CreateRestrictedToken` runs. Controlled workspace modes therefore remain unadvertised and fail closed; do not treat full-access execution as sandbox evidence.

The SQLite projection cache is durable, versioned, committed-revision checkpointing. It is performance and reconnect hardening; canonical projection completeness is proven separately by A8.1's strict rebuild/live Cursor and cross-entry suite.

The release gate must continue to require evidence for A9.5. Team and architecture-specific exclusions remain optional only when they are represented as such in the manifest, profile, task register, and status documents.

A9.3 now has a bounded cross-process starting matrix: SQLite process death must
leave only the committed prefix, the production restart path may append only
deterministic interrupted-turn closers, and a second independent handle must
observe the same durable result. Treat this as progress toward A9.3, not a
completed fault/security suite. JSONL disk-full and kill-at-every-write now have
real second-process restart oracles. The HTTP cross-origin boundary also runs a
real second-process fetch, proves the target is not contacted, and reloads
JSONL through an independent handle. Windows Job Objects also enforce an
active-process ceiling for Code Mode and controlled-shell trees; a real child
process proves a two-process job admits only one descendant. Credential
rotation, plugin reload, and MCP reconnect now have one consolidated generation
matrix with effect/restart settlement and no automatic replay. A real Code Mode
hostile-worker oracle also proves an oversized protocol frame settles once,
keeps exactly one external host effect, and cold-replays one failure receipt.
Pipe, provider-wipe, and the complete claimed-platform fault/security
matrix remain required; a bounded POSIX fork-bomb process-group oracle is now
available.

A3.3 is closed as an explicit architecture substitution rather than a
degradation. Shutu replaces DSH's worker-thread transport with a Node
permission-model subprocess that enforces the same model-facing CPU, wall,
heap, and output budgets plus process-level ownership. On Windows, a Job Object
proves per-process CPU and active-process limits; on AlmaLinux, a bounded
four-level fork bomb proves process-group kill-tree. Windows and Linux suites
prove timeout, hostile oversized frame, worker death, heap/output, forged
duplicate admission, queued-unstarted abandonment, cancellation, close join,
empty environment, and cleanup. Real ambient probes prove child-process and
worker-thread APIs are denied. Post-terminal done/call frames cannot create a
second settlement.

A9.4 is done. The strict `CGO_ENABLED=1 go test -race ./... -count=1` gate
passes on Windows and Linux; the Linux run uses Go 1.26.7, gcc 11.4, and Node
24.19.0 in Ubuntu 22.04 WSL. The cleanup oracles cover provider-generation
retirement, credential drain/wipe, MCP goroutine cleanup, Code Mode worker
joins, child-process kills, job reservation recovery, and SQLite cross-process
locks. The race run surfaced and fixed real bounded-call admission, Unix
persistent-terminal, transport-pipe, and MCP child-reap lifecycle defects.

ACP now has a production external-disconnect oracle: the child process owns the
real SQLite/`acpFactory`/loop/server composition root, the external client
disconnects after a committed filesystem side effect, and independent handles
must observe the same durable lifecycle and shared projection after resume.
This is completed A7.1 evidence. The durable attachment subcase is separately
proven across real client disconnect, child exit, independent
attachment/SQLite handles, resume, and shared projection image resolution. A
real `session/reconnect` also proves that both the canonical resource
projection and a resolved durable user image re-enter provider history. A
canonical text+image assistant output seeded through the production session
sink is also proven to survive reconnect at the provider-history and
cold-projection seams.

ACP provider faults also have a production reconnect oracle: a partial stream
followed by failure produces the real wire error, durable interrupted assistant
anchor, failed step/turn, and then replays that prefix into the recovery
request after reconnect. Output loss also has a cross-process oracle: one child
persists assistant output after its stdout is broken, and a second independent
child reopens SQLite, reconnects, and replays that output into recovery
history.

The second independent external client (`internal/acpclient`) drives the
production composition through authenticate, initialize, session/new,
prompt/update, reverse permission approval and tool execution, durable image
input, cancellation, reconnect, and recovery provider-history replay. A7.1 is
therefore done. A separate real MCP stdio oracle proves that a crashing
generation can commit one external effect without replay after replacement;
A7.2 and A7.3 are also done.

The fixture exposed a real sequence-authority race: ACP session logs are now
registered as the session runtime log, so titles and later prompts share one
append authority, and resume atomically replaces that runtime log. SQLite
interrupted-tail repair also reloads and recomputes when a legitimate writer
advances the tail during recovery, using a typed conflict sentinel and a
bounded retry.

ACP `session/cancel` for an unknown session is an idempotent notification, as
in the pinned reference; stale cancellation during disposal/reconnect must not
produce `-32602`. Keep this behavior covered by the external wire test and
record any remaining ACP gaps as matrix/evidence gaps rather than implying
that the protocol is complete.

ACP transport teardown must cancel server-initiated permission requests on both
clean EOF and scanner/read error before joining prompt workers. A permission
request may deliberately outlive the prompt context; otherwise an abnormal
input-pipe failure can leave the server blocked forever.

The SDK runtime transport ignores malformed JSON lines, matching the reference
line protocol. It must not emit an uncorrelatable null-id parse-error response;
keep this behavior covered separately from typed JSON-RPC request errors.

MCP `tools/call` is one request boundary: a connection error or timeout may
arrive after the server committed the side effect, so the bridge returns the
original failure and never synchronously replays the same call. Recovery is
owned by the connection supervisor; the next model-level attempt is explicit.
The same at-most-once rule applies to the dynamic `mcp_call` tool; keep
`internal/mcp/tools_test.go:TestMcpDynamicCallDoesNotReplayUnknownCommit` in the
regression set whenever MCP recovery changes.
Dynamic `mcp_call` must discover the named tool's current execution metadata
before `tools/call` and reject `taskSupport=required` without crossing the
side-effect boundary; keep
`internal/mcp/tools_test.go:TestMcpDynamicCallRejectsRequiredTaskBeforeCall`
alongside the static bridge test.
Keep MCP reconnect defaults aligned with the pinned reference (500ms initial
delay, 30s maximum delay, 10 attempts) and test the defaults directly.

Plan-mode reads in CLI, Web, ACP, and model/runtime admission must go through
the shared projection snapshot. A malformed durable `plan/mode` value must
fail closed; never coerce a non-boolean `active` or `pending` field to false.
The same fail-closed rule now covers `permission/preset` and `sandbox/mode`.

Native goal/plan/todo/permission/session-list control values derive from the
canonical projection snapshot. The wire adapter may retain DSH-specific shape
and runtime activation fields, but durable goal lifecycle, goal-to-plan links,
todo steps, and plan-delete cascades come from replayable events. Native
permission values, ACP resume cursor/admission, SDK snapshot/query surfaces,
history/trajectory consumers, and the completed A9.1 cross-entry suite now use
or verify that same Snapshot authority.
Optional Team board state now folds into the same Snapshot from
`team/snapshot` plus member/task/mailbox events; malformed Team state fails
cold rebuild rather than exposing an empty board.

The shared job projection merges lean `job/status` and `job/done` facts onto
`job/start`, preserving job identity metadata and creation time across cold
replay. This is projection-fidelity evidence only; runtime job ownership and
the remaining transport readers must still converge on the same Snapshot.

Dynamic plugin calls now expose a generation-bound start/terminal audit seam;
the seam is intentionally not a durable store and never authorizes automatic
replay. Keep A2.5 closed only while the registry receipt tests and the
machine-readable crash-boundary contract remain in sync.

The tool pipeline also re-checks cancellation at the `ExecutePrepared` dispatch
boundary. A call cancelled after preparation cannot enter the tool body and
returns `ABORTED_BEFORE_DISPATCH`; this closes one real side-effect race and is
part of the completed A4.4 regression set.

Late cancellation is also classified by whether the body actually started:
started successful calls become `ABORTED` while retaining deferred contexts;
wrapper short-circuits remain `ABORTED_BEFORE_DISPATCH`. The corresponding
tests are part of the A4.4 partial evidence.

Specific wrapper/tool failures retain precedence over a concurrent caller
cancellation; only plain context cancellation is converted to the abort codes.
This distinction is covered by the cancellation failure-precedence tests and
is required for durable error replay.

Pre-cancelled calls are materialized before the cancellation check, then stop
before whitelist, schema, approval, and pre-hook execution. They return
`ABORTED_BEFORE_DISPATCH`; this prevents an already-impossible call from
triggering policy or user-interaction side effects. Argument materialization
errors still retain precedence because the call must first cross the same
lossless argument boundary as DSH.

`Prepare` also detaches already-decoded map/slice arguments through a lossless
JSON snapshot before storing them in `PreparedExecution`. The caller may not
mutate the prepared request between authorization and dispatch.

Process-backed cancellation must be classified at the tool boundary as an
explicit `AbortError/ABORTED`; registry code must not infer it from platform
specific exit-status text.

A4.4 closure rule: every required model-facing tool must be exercised through
the common registry with disabled, schema-invalid, and pre-execute denial
evidence; each rejection must have one terminal result observation and a
canonical replayable rejection. Unknown rich block kinds are valid only when
their lossless `Raw` wire representation is retained; image, audio, and resource
metadata must survive canonical tool/result replay. Do not substitute catalog
name presence for these execution/replay contracts.

A3.2 closure rule: configured Code Mode preference is not availability. A
failed permission-model probe must remove `run_code` from Native, Web, ACP, SDK
and registry execution, retain the reason, and keep startup healthy. Do not
represent this fail-closed behavior as sandbox enforcement; A3.1 owns that
proof.

Native `session.list` may use a bounded event tail only for sidebar metadata.
It must reuse an exact-revision projection checkpoint or replay the complete
raw session before creating one; never label a tail-only fold with the full
session revision. Version changes must invalidate older semantically unsafe
checkpoints. This remains A8.2 hardening, not evidence that A8.1's shared
CLI/native/Web/ACP/SDK canonical projection is complete.

The Web and native session-list surfaces now share
`internal/session.SessionListMetadata`: only `turn/start` makes a session
non-blank, and only human `user/message` events advance prompt recency. A
legacy user-only log therefore remains `blank=true` on both surfaces. Keep
this event fold at the session boundary; a transport-local `event_count > 0`
shortcut is a projection divergence.

Native `llm.models` may expose model metadata only through the composition-root
catalog projection. Explicit DeepSeek facts and user/provider profile rows now
carry context window, max output, reasoning, tools, vision, audio, and model
input-modality fields into that selector. ID-only suggestions for providers
without owned metadata remain non-authoritative; do not turn them into
selectable capability facts.

The LLM registry now has the provider-owned `ListModels`/
`ResolveModelInfo` contract with per-provider failure retention and duplicate /
cross-provider metadata validation; retry wrappers forward it. This is a
contract seam, not proof that every production adapter implements dynamic
catalog resolution. Keep A6.3 partial until real adapters and their remote
catalog/error/cancellation behavior are wired and tested. Loop assembly now
resolves the final provider/model route before deriving capability-dependent
limits, so an explicit route cannot inherit the global model's output budget;
`TestBuildLoopUsesExplicitRouteCatalog` guards this cross-entry invariant.

Web `GET /api/sessions` must also use the shared prompt recency when returning
`updated_at`. SQLite's last physical append may be a later lifecycle or
transport event; it is not a substitute for the latest eligible human prompt.
`TestSessionListUsesLatestHumanPromptForUpdatedAt` is the regression guard.

Every direct registry call must have one terminal result-observer delivery,
including unknown-tool, argument, policy, stale-generation, and pre-dispatch
cancellation failures. Preserve the existing Go error return if compatibility
requires it, but do not let `Prepare` errors bypass the normalized result
observer boundary or invoke an observer more than once.

Native `session.rename` is a canonical event operation: reject a title that
normalizes to empty with `title-invalid`, append `session/title` through the
addressed session log, and return that event's real sequence. SQLite must
project the same title/source in the append transaction so `session.list`,
native history, mux projection, and cold restart cannot observe separate title
authorities. Automatic fallback/provider titles use the same event path, and
the final provider write is serialized with user rename so an in-flight model
result can never overwrite a user pin.

The authenticated Web `PATCH /api/sessions/{id}/title` route is part of the same
boundary in production: it rejects empty normalized titles and uses the wired
rename callback, returning the real event sequence. Its direct metadata write
is retained only for embedders that do not own a live Agent runtime; that path
is compatibility-only and is not evidence of canonical projection parity.

Fallback title extraction must use `session.FirstEligibleUserText`, which folds
both legacy top-level text and durable rich `content` blocks through the session
derivation boundary. Do not add a Web/CLI-specific JSON decoder for title text;
that would reintroduce divergent projections.

Model selection is also an admission boundary. If durable model-visible
history contains image blocks, a text-only target route must be rejected with
the stable `model-unavailable` result before session configuration is written.
The same check must replay durable pending inbox mutations: an unclaimed image
prompt is already model-visible input and cannot be hidden from route admission.
Keep this check in the shared composition-root validator so native/Web/ACP/SDK
selection paths cannot silently diverge. Do not invent model capacity,
reasoning, tool, vision, or audio facts for providers whose adapter does not
expose an owned first-party catalog; keep A6.3 partial and fail closed until
every claimed adapter is covered. The provider-owned catalog seam is wired
through all four claimed wire-protocol adapter families, and pinned
capacity/modality facts are present for the currently advertised OpenAI,
Anthropic, and Gemini routes. ID-only rows remain unknown. Anthropic/Gemini
reasoning selections are serialized through their protocol thinking fields;
exact known non-reasoning models reject unsupported effort values. Complete
upstream facts and remote failure behavior are still incomplete.

A9.1 is done. Its suite now covers SQLite/JSONL envelopes, Web/native/SDK
projection, production native CLI and child Agent execution, ACP/MCP/SDK wire
legs, side-effect/cleanup and tool-error settlement across Web/ACP/SDK, and the
real reference replay. Declared wire-shape and rich-content normalizations remain
explicit; they are not silent equivalence substitutions.

The DeepSeek provider wire now requests usage reporting explicitly, validates
thinking/reasoning effort, rejects truncated SSE tails, preserves all deltas
from one payload, normalizes both cache-hit usage field spellings, and applies
a five-minute idle watchdog that distinguishes `TIMEOUT` from caller
`ABORTED`. Its image budget matches dsh's base64 accounting and oldest-first,
immutable offload; known catalog negatives cannot be overridden by a global
image flag across all four wire-protocol adapters; tool-result images remain textual on `tool` and are projected into
a following user multimodal message. The SDK transport handles no-timeout
requests safely, allows a low-level spawn retry, and enriches runtime transport
loss with settled exit/stderr context. These are real A7.3 repairs, but A7.3
remains partial until the complete ACP/SDK reference-runtime matrix,
external provider/header replay, and the release negative/fault suites are
evidenced. First-party model facts, effort/default-output wires, and stable
audio admission are covered separately. All four
claimed wire-protocol adapters now expose detached runtime catalog snapshots;
ID-only rows remain explicitly unknown and do not become invented capability
facts. Anthropic, Gemini, and OpenAI Responses serialize non-empty
reasoning selections using their protocol thinking wire, and exact catalog
non-reasoning models must fail with `UNSUPPORTED_REASONING_EFFORT`; silently
dropping `low`/`high`/`max` is not allowed. `TypeScriptRuntimeStatus` must also perform a real Node permission-model
probe (an ungranted executable read must fail with `ERR_ACCESS_DENIED`) before
`run_code` is registered; `node --help` alone is not evidence of enforcement.

Reasoning budgets configured under `llm.thinking_budgets` must travel through
the canonical Loop/ChatRequest seam to Anthropic and Gemini; zero means the
provider's reference default. Do not add a transport-local budget default that
overrides this composition-level setting. The current built-in vocabulary is
`low|high|max`; custom catalog rows may now carry canonical per-model effort
maps, provider wire spellings, and model-owned defaults. Configured output
budgets and model-level `defaultMaxTokens` resolution now reach every claimed
provider wire adapter, including the reference 32768 route fallback. The pinned
upstream catalog and bounded remote discovery/failure behavior are now covered.
All five production provider adapters have one real-HTTP header/stream replay
matrix. A read-only source loader drives the pinned reference SDK runtime under
Shutu's external Go SDK client and the pinned reference ACP runtime under a
Shutu raw JSON-RPC ndjson client; both reference subprocesses prove protocol
identity, turn/output settlement, and clean external-client teardown. A7.3 is
done.

Provider request admission must distinguish durable rich blocks from wire
support. Audio/resource/vendor blocks may be preserved in the canonical log, but
DeepSeek, OpenAI-compatible, Anthropic, Gemini, and OpenAI Responses adapters
must reject them before credentials, attachments, image offloading, or network
I/O with `UNSUPPORTED_INPUT_CONTENT`.

The durable session log now has a closed event vocabulary: live append,
atomic append, persisted incorporation, and replay reject unknown event types
with `ErrUnknownRequiredEvent` before advancing the sequence. Native wire
extensions may still be accepted only when explicitly marked `ignorable`; that
wire exception must not become a durable-log escape hatch. The cross-module
negative matrix is at `internal/contractfixture/negative_matrix_test.go`.
`cmd/pa/tool_contract_matrix_test.go` additionally proves SQLite durable
rejection replay for every required model-facing tool. A9.2 is done: all 49
denial paths prove the production body is not reached; every claimed provider
adapter rejects unsupported input before any request or credential header
crosses the network; and dedicated matrices cover persistence, sandbox,
approval, plugin generation, worker death, network boundaries, MCP faults/no
replay, credentials, process cleanup, and Code Mode unavailable/fail-closed
paths.

The shared protocol lifecycle fixture now has a cross-entry protocol leg at
`internal/contractfixture/protocol_entry_test.go`: the public ACP server, a
real MCP stdio child, and the public SDK client consume the same
session/tool/terminal facts. ACP, MCP, and SDK wire formats remain distinct by
design. The production native CLI now also drives the same fixture through a
real child process at
`cmd/pa/native_cli_cross_entry_test.go:TestNativeCLICrossEntryFixture`; it
requires a production write-tool file effect and durable tool result, then the
assistant response on stdout, and opens production SQLite with an independent
handle to require the same projected history and surface after exit. Child
Agent execution also drives the same fixture through the
production child runner at
`internal/contractfixture/child_agent_entry_test.go:TestCrossEntryChildAgentFixture`:
it verifies the fixture text prompt, tool schema/call/result, final assistant,
one fsynced external effect and temporary-lock cleanup, durable subagent
lineage, and an independent SQLite projection after provider close. The real
reference double-replay gate is also stable again on Node 24.19.0: five
consecutive runs passed with no diff. Web and real SDK child legs add fsynced
effects, lock cleanup, stable `TOOL_EXECUTION_ERROR`, and cold projection. The
production ACP child adds fixture side effect, outside-workspace tool/error,
real client disconnect, process cleanup, and independent resume/projection.

The full Go regression is green when its temporary root is outside the subject
repository. A temp root inside the repository intentionally discovers the
subject's `.dsh/.agents/skills` and makes two `internal/skill` fixtures fail;
the default Windows `%TEMP%` also cannot service Go's `EvalSymlinks` under the
managed sandbox. Keep those as verification-environment conditions, not as a
capability-equivalence pass or a reason to weaken path/symlink fail-closed
checks.

The SDK server now seals new session admission before shutdown waits for an
in-flight session creation. A late-created Agent is rejected and disposed
instead of being inserted into a closed server; concurrent shutdown callers
share one completion result. Keep this fence in the SDK owner boundary.

Approval restoration is a control-plane projection and must read SQLite's raw
committed event seam. It must not replay or validate unrelated session
lifecycles during process startup: an old damaged transcript can remain a
session-local error without taking down the SDK/Web/ACP composition root.
Actual session opening keeps the strict lifecycle and interrupted-tail
recovery boundary.

A rebuilt `pa --sdk` executable has also completed initialize -> shutdown
against the existing default SQLite data directory. This proves only startup
and lifecycle wiring, not model streaming or full cross-entry parity.

All durable event writers must use the closed durable vocabulary shared with the
session replay boundary. SQLite seed/append/atomic/team/approval paths and JSONL
load/recovery must reject unknown event types before inserting or advancing a
durable cursor; the native wire `ignorable:true` extension is not permission to
persist an unknown event.

Windows command, PowerShell, background-job, and Code Mode process trees must
be owned by a Job Object from process start through cancellation and quiescence.
Cancellation must wait for descendant termination; if an external Job Object
rejects assignment, use the bounded descendant-kill fallback and retain an
explicit platform runtime test. Direct-child termination alone is not a stable
process-tree boundary. TypeScript Code Mode runtime disposal must also cancel
host bindings before joining the worker; killing Node alone can leave the host
side call ledger waiting indefinitely.

Provider request attribution now has one shared User-Agent helper. Normal
loop requests carry the durable session ID, and ACP compaction requests carry
the session ID plus `Purpose: compaction`; the official DeepSeek route adds
the stable anonymous user ID and `x-deepseek-harness-*` headers lazily. The
OpenAI-compatible route is deliberately excluded from DeepSeek-specific
identity headers. A shared replay now proves those boundaries across all five
claimed production adapters over real HTTP. The pinned reference SDK runtime
client and pinned reference ACP runtime/client legs are also complete.

The SDK line transport also removes a request from its pending set when a
close wins after registration but before the write lock is acquired. Keep this
invariant alongside timeout and late-response cleanup: every abandoned request
must settle and no pending entry may survive a transport close.

ACP client EOF is also an ownership boundary: once `session/new` has committed,
disconnect must cancel the owned session and close it exactly once. Keep the
local regression in `internal/acp/server_test.go`, while treating the full
external reconnect/rich-content/permission matrix as still unverified.

Model catalog rules: an explicit model-level `input` declaration is
authoritative over deployment-wide modality settings. The current runtime may
advertise only `text` and `image`; audio must remain an explicit unsupported
negative until every request/content/transport path can encode it. Custom
provider `models[]` entries must have unique IDs and the first entry is the
effective model when the legacy `model` field is empty. Invalid catalog rows
must fail closed before provider registration or settings mutation. Do not
invent a second catalog. The non-DeepSeek inventory is generated from
reference-pinned `@earendil-works/pi-ai@0.82.1`; curated Shutu facts merge by
provider/model ID and upstream-only routes remain visible. Built-in discovery
returns owned catalog facts, not ID-only suggestions. Model-level reasoning
effort maps and defaults are carried through the shared catalog and direct
provider request seams.

The SDK `session/event` adapter must emit the reference envelope, not the
internal storage shape: surface events carry top-level `surfaceOp` and, when
present, `sourceEventSeqs`; opaque event data remains intact. Keep
`session.WireEvent`, `sdkclient.SessionEvent`, and the generated protocol schema
in sync, including the replacement-object form of `surfaceOp`.

Audit counting rule (2026-09-03): the task register is authoritative. It has
47 tasks and all 47 are done. The required-profile blocker A9.5 is closed after
the user/administrator removed the local machine WER LocalDumps policy key that
had blocked the required disabled-crash-dump profile.

Verification note (2026-09-01): on the managed Windows host, the targeted
package suite, `go vet ./...`, and a full `go test -p 1 ./...` pass when Go's
cache plus `TEMP`/`TMP` use an external writable temporary root
(`D:\dev-projects\Agent\.audit-external-temp-next`). A temp root inside the
subject repository intentionally discovers the subject's `.git` ancestor and
`.dsh/.agents/skills`, while the default user Temp path can separately deny
`EvalSymlinks` and child-process access. Those are verification-environment
conditions, not capability evidence; the external-root command is the
reproducible full-regression baseline for this host.

Code Mode negative-entry verification (2026-09-01): when the Node permission
model probe is unavailable, `run_code` is removed from the registry and from
the ACP/SDK catalogs, while Native/Web preset selection is hidden/rejected as
well. `cmd/pa/codes_test.go:TestRegisterCodeUnavailableRemovesRunCodeFromACPAndSDKCatalogs`
and the existing Native/Web tests cover the public entry points. This closes
the advertisement/dispatch evidence gap for A3.2; it does not close A3.1's
requirement for a real enforcing sandbox backend.

Sandbox backend correction (2026-09-01): the local controlled-shell profile
now selects a functionally probed Linux bubblewrap backend, macOS Seatbelt
file-effect backend, or the Windows ACL restricted-token backend. Seatbelt and
Windows ACL are not advertised as network-isolated; Windows ACL also does not
isolate reads or process visibility. The Windows implementation fails closed
when the host's `CreateRestrictedToken` probe returns `ERROR_INVALID_PARAMETER`,
as on the current managed host. Do not treat the new platform paths,
process-tree cleanup, or capability metadata as proof of the still-missing
resource-limit matrix.

Windows ACL backend correction (2026-09-01): `internal/code/windows_acl.go`
implements the DSH-shaped workspace SID/temp SID grants, cross-process
`LockFileEx` DACL transaction, `WRITE_RESTRICTED`/LUA token, default-DACL
grant, private `TMP`/`TEMP`, restricted `os/exec` token startup, and revocable
temp cleanup. `windows_acl_test.go` covers the real child behavior when the
host supports the API. Capability advertisement is gated by the same real
probe, so an API failure cannot silently widen a controlled run to full access.

Windows Code Mode resource correction (2026-09-01): TypeScript worker runs now
pass their `computeMs` ceiling into the Windows Job Object. The Job Object
enforces cumulative CPU time at the kernel boundary for the owned worker tree;
the existing process-time polling remains for DSH-compatible failure
classification. `--max-old-space-size` is also covered by a real contained OOM
test. Code Mode and controlled shell now also enforce a Job Object
active-process ceiling, with a real four-attempt child oracle proving only the
allowed descendant starts. These close Code Mode CPU/heap/process-count
subcases; they do not provide the complete shell memory/file-size matrix,
Windows `WRITE_RESTRICTED` validation on the managed host, or network
enforcement, so A3.1/A3.3 remain partial.

Shared ACP surface correction (2026-09-01): committed ACP assistant delivery
now reads `projection.Snapshot.Surface`, and the session decoder preserves
assistant text, image attachment references, and unknown rich blocks before
transport adaptation. Plain text keeps the existing ACP update shape; mixed
rich output remains ordered and attachment-preflighted. This advances A4.4/A8.1
but does not prove the remaining external ACP reconnect matrix or native/SDK
trajectory/control-state migration.

ACP delivery-failure correction (2026-09-01): `session/update` write failures
are now retained through the prompt boundary and returned as an internal ACP
failure, with explicit cancellation still taking precedence. Protocol short
writes are rejected as `io.ErrShortWrite`; a committed response can no longer
be reported as `end_turn` after its output was only partially delivered.
Evidence: `internal/acp/server_test.go:TestServerContainsSessionUpdateWriteFailure`,
`TestServerPrioritizesOutputDeliveryFailureOverPromptFailure`, and
`TestServerRejectsShortProtocolWrites`.

Model catalog default correction (2026-09-01): `llm.ModelInfo` now carries the
model-owned `DefaultReasoningEffort`, equivalent to DSH's
`reasoning.defaultEffort`. Custom model persistence, Web `ProviderModel`, the
Native catalog/discovery projection, and all catalog-backed Anthropic,
Gemini, OpenAI Responses, and loop request seams use the same value. Legacy
reasoning maps and boolean facts receive the deterministic compatibility
default used by the existing selector. Invalid defaults and defaults missing
from an authoritative effort map fail closed.
Evidence: `internal/llm/model_catalog_test.go:TestModelDefaultReasoningEffortUsesOwnedMetadataAndLegacyFacts`,
`cmd/pa/model_catalog_data_test.go:TestReasoningEffortCatalogIsProjectedToProviderAndWebRows`,
`internal/webserver/native_rpc_test.go:TestNativeLLMCatalogPreservesOwnedModelMetadata`.

Model catalog default-output correction (2026-09-01): `DefaultMaxTokens` is
distinct from the model capacity field `MaxTokens`. DeepSeek exact and
unlisted route resolution materializes the DSH connection default `256000`,
explicit request/session budgets still win, and CLI/ACP/Native/Web projections
preserve the distinction. A capacity-only catalog row is no longer serialized
as a request cap; all four non-DeepSeek wire adapter families consume an
explicit model default when one is declared, and use the reference pi-ai route
fallback `32768` when neither model nor request declares one. Evidence:
`internal/llm/deepseek/model_catalog_test.go:TestStreamMaterializesProviderDefaultMaxTokens`,
`internal/llm/model_catalog_test.go:TestModelDefaultMaxTokensDoesNotUseCapacity`,
`internal/llm/anthropic/anthropic_test.go:TestStreamUsesExplicitModelDefaultMaxTokens`,
`internal/llm/anthropic/anthropic_test.go:TestStreamUsesReferenceRouteDefaultMaxTokens`,
`internal/llm/google/google_test.go:TestStreamUsesExplicitModelDefaultMaxOutputTokens`,
`internal/llm/google/google_test.go:TestStreamUsesReferenceRouteDefaultMaxOutputTokens`,
`internal/llm/openairesponses/openairesponses_test.go:TestStreamUsesExplicitModelDefaultMaxOutputTokens`,
`internal/llm/openairesponses/openairesponses_test.go:TestStreamUsesReferenceRouteDefaultMaxOutputTokens`,
`internal/llm/openai/openai_test.go:TestStreamUsesExplicitModelDefaultMaxTokens`, and
`cmd/pa/model_catalog_data_test.go:TestDeepSeekReferenceCatalogMetadata`.
Persisted builtin/custom provider profiles may also set a route-level
`default_max_tokens`; registration passes it to the selected protocol adapter,
and `cmd/pa/llm_test.go` verifies it survives a provider cold restart and
reaches the wire.

SDK projection correction (2026-09-01): the SDK now exposes the optional
`session/snapshot` query. The runtime rebuilds that response with
`projection.Build` from the addressed session's durable events, so SDK
history/trajectory/control state shares the same cold-restart authority as
Native and Web. The extension is included in the local generated SDK schema;
reference request methods remain unchanged and reference clients may ignore
the extension. The full A8.1 cross-entry byte-equivalence fixture and ACP
permission/reconnect projection remain open.
Evidence: `cmd/pa/sdk.go`, `internal/sdkclient/client.go`,
`internal/sdkclient/types.go`, `cmd/pa/sdk_test.go:TestSDKServerExternalClientPromptRunsAgentThroughIdle`,
`tools/generate-sdk-protocol-schema.mjs`, and
`internal/sdkclient/testdata/protocol.schema.json`.

Controlled shell resource-boundary correction (2026-09-01): `RunRequest` now
supports hard memory, file-size, and process-count ceilings for controlled
shell modes, with finite defaults of 512 MiB, 64 MiB, and 256 processes.
POSIX execution installs both soft and hard `ulimit` values before `exec` so
the original command and its fork/exec descendants inherit limits that cannot
be raised by model-authored shell code. Explicit `danger-full-access` does not
receive these controls. `TestControlledShellEnforcesHardFileLimit` covers the
real write boundary when the host has a usable controlled POSIX backend; the
current Windows host skips it because its restricted-token probe fails closed.
This advances A3.1, but memory/fork-bomb/hostile-descendant fixtures on every
claimed backend and a successful Windows restricted-token validation remain
required before the sandbox blocker can close.

ACP permission wire correction (2026-09-01): the exported external ACP
contract now drives `session/new` and `session/prompt`, observes the
server-initiated `session/request_permission`, sends the matching JSON-RPC
response on the same stream, and verifies that the prompt resumes to
`stopReason=end_turn`. Evidence:
`internal/acp/contract_external_test.go:TestExternalACPPermissionWireContract`.
This closes permission-settlement evidence; later production two-client,
disconnect, reconnect, and fault/oracle work completed the remaining A7.1
matrix.

The production SDK child-process regression also queries `session/snapshot` over
real exec/stdio after the prompt reaches idle and verifies the durable session
id, history, trajectory surface, and projection cursor. Evidence:
`cmd/pa/sdk_test.go:TestSDKClientDrivesRealRuntimeChildThroughIdle`. This
strengthens A8.1 external evidence. A9.1's completed cross-entry suite supplies
the broader runtime/restart oracle.

Latest audit status (2026-09-03): `docs/equivalence-task-register.yaml` has
47 tasks: 45 done, 1 partial, and 1 open. A3.1 and A8.3 are done. A3.1 has
Windows ACL and Linux bubblewrap containment/resource evidence; A8.3 has
selected-profile enforcing/replay descriptors and optional fail-closed RPC
coverage. A7.3, A3.3, A9.3, A9.4 and A9.5 are done.

MCP reconnect generation correction (2026-09-01): reconnect now tracks
in-flight operations per connection generation. A replacement generation may
start after transport loss, but the old generation is closed only after its
own calls return; new-generation traffic is tracked independently. Evidence:
`internal/mcp/reconnect.go`,
`internal/mcp/reconnect_test.go:TestReconnectingClientRetiresOldGenerationAfterInFlightCall`.
This fixes a concrete lifecycle race, while the full pinned MCP external
matrix and side-effect/restart oracle remain required.

Workspace-bound ACP, FS, Terminal, code, fssearch, pwsh, and Web paths now use
the shared `internal/pathsecure` resolver. On Windows it accepts a normal
existing directory when the host denies Go's `EvalSymlinks`, but first checks
every component for reparse points; known links and unresolved access remain
fail-closed. PowerShell establishes the validated workdir with an in-process
`Set-Location -LiteralPath` so managed hosts that reject `cmd.Dir` during
startup still receive the requested cwd. This fixes host-specific startup
failures without turning the local code backend into an enforcing sandbox.

ACP production resume now reports the last durable event sequence as
`ResumeMetadata.eventCursor` instead of the number of events. The SQLite-backed
factory regression `cmd/pa/acp_test.go:TestACPFactoryResumeRestoresDurableIdentityCWDHistoryAndCursor`
covers `session/new → append → close → session/resume`, including stable id,
CWD, restored history, and cursor. This closes a concrete resume-cursor bug;
the full external ACP/reference matrix remains a release blocker.

MCP resync publication correction (2026-09-01): reconnect-time tool discovery
is now transactional. Duplicate public names are rejected before mutation, and
a registry conflict in any later tool rolls back replacements/new registrations,
leaving the prior live MCP generation and name index intact. Evidence:
`cmd/pa/mcps_test.go:TestMCPReconnectResyncKeepsPreviousGenerationOnConflict`.
This closes a concrete availability gap in A7.2; the full external MCP matrix,
side-effect oracle, and release-gate fault/restart suites remain open.

Streamable HTTP cancellation correction (2026-09-01): caller cancellation is
now scoped to the individual request and no longer retires a healthy MCP
session or triggers an unnecessary reconnect. Evidence:
`internal/mcp/http_test.go:TestStreamableHTTPClientCallerCancellationKeepsSession`.
