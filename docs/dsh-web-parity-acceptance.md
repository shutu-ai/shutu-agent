# DSH Web 对齐验收记录

## P36 原生 DSH UI 基线（2026-08-27）

`npm.cmd run e2e` 已通过 Playwright 原生入口验证：桌面 `1280x900`、移动 `390x844`，`/api/events.mux` 与 `/api/events.host` 均建立，控制台无 error/warning，页面横向溢出为 `0`。使用 `SHUTU_E2E_ARTIFACT_DIR=docs/evidence/dsh-native-2026-08-27` 可保存本轮桌面/移动空数据、深色、加载、错误态及几何矩阵截图；逐页视觉/交互证据仍待 P36-6 后续任务。

本记录对应 P34。`deepseek-harness` 仅作为只读参考，验收对象为 `shutu-agent`。

## 自动化检查

| 检查 | 结果 | 说明 |
|---|---|---|
| `go test ./...` | 通过 | Go 后端全部包通过 |
| `go vet ./...` | 通过 | 无 vet 问题 |
| `go build ./...` | 通过 | 后端可构建 |
| `npm.cmd run typecheck` | 通过 | 前端 TypeScript 无错误 |
| `npm.cmd run test` | 通过 | 8 个测试文件、42 个测试，含合成性能基线 |
| `npm.cmd run build` | 通过 | Vite 生产构建成功 |
| `npm.cmd run verify` | 通过 | dist、入口和资源校验成功 |
| `npm.cmd run e2e` | 通过 | Playwright 桌面/移动端通过，控制台无错误 |
| `npm.cmd run performance` | 通过 | Chromium 10,000 条事件基线：挂载 13 行、加载约 1.9s、搜索约 484ms、滚动约 32.7 FPS、JS heap 约 252MiB |

## 已覆盖交互

- Conversation / Trajectory / Running Tab 切换
- 会话加载、SSE 实时事件、重复事件去重
- 轨迹动态虚拟滚动和动态高度
- Turn、assistant tool call 折叠与状态恢复
- 时间线点击、Shift 范围选择、拖拽选择和选区清除
- 轨迹搜索、结构化详情命中、结果高亮和摘要片段
- 请求 Inspector、Token、输入/输出、错误、重试和父子关系
- 历史分页边界、加载状态、失败提示和重试入口
- 空数据、坏时间戳、未知事件、坏 SSE 帧和图片加载失败
- 桌面与移动端首屏布局、主要控件响应和控制台健康

## P34 性能与联调基线

- 使用 `cmd/pa` 的真实组合根测试覆盖健康检查、会话 API、事件发布到 SSE 的链路。
- 使用 10,000 条事件和 100,000 行虚拟列表合成数据验证投影、搜索索引和虚拟区间计算的正确性与边界。
- 使用真实 Chromium 页面运行 10,000 条含长文本/密集 tool 记录，验证虚拟挂载数量、加载时间、搜索响应、滚动帧率和 JS heap 基线。
- 已使用真实 `deepseek-v4-flash` 后端执行“生成网页版超级玛丽游戏”的长任务，并通过 `web/scripts/real-task-performance.mjs` 记录大历史与流式输出基线；结果见下方“真实任务基线”。
- 已建立桌面、移动端、空数据、异常数据和实时数据的自动化验收矩阵。
- 使用发布包完成本地双版本启动、共享 `data_dir`、升级切换和回滚切换演练。

## 真实任务基线（2026-08-26）

测试环境是本机 `cmd/pa --web-only`，模型配置为 `deepseek-official/deepseek-v4-flash`；任务工作区为 `real-task-super-mario`，任务内容是生成并持续维护一个原创网页版横版平台游戏，包含代码生成、测试、修复和 README 更新。该数据来自真实模型后端与真实 SSE 事件，不是合成事件；但它仍不是客户生产环境，不能替代目标环境验收。

采样命令：

```text
npm.cmd run real-performance -- s-f15a7e57 180
```

采样器使用 Chromium 1440×960，`performance.memory` 仅在 Chromium 中可用；FPS 是每个约 1 秒窗口的 `requestAnimationFrame` 数，DOM 是页面节点总数，`longTaskMs` 是累计长任务时间。

| 场景 | 事件范围 | 采样时长 | min/avg FPS | 最大 DOM | JS heap | 长任务 | 控制台错误 |
|---|---:|---:|---:|---:|---:|---:|---|
| 旧时间线实现，真实持续流式输出 | 28,828 → 36,218 | 121.1s | 4 / 25.7 | 8,625 | 5 → 174 MiB | 484 / 86,276ms | 无 |
| Canvas 时间线改造后的真实大历史对照 | 47,053 → 52,028 | 60.7s | 7 / 23.0 | 382 | 25 → 193 MiB | 330 / 23,761ms | 无 |
| 当前实现，55,394 条历史空闲观察 | 55,394 → 55,394 | 60.7s | 60 / 62.8 | 386 | 5 → 5 MiB | 1 / 106ms | 无 |
| 当前实现，75,942 条历史、compaction 阶段观察 | 75,942 → 75,945 | 180.7s | 60 / 68.2 | 390 | 6 → 6 MiB | 2 / 171ms | 无 |

结论：Canvas 已消除时间线 DOM 随历史线性增长；当前实现的大历史空闲状态稳定在 14 个虚拟行、约 386–390 个 DOM 节点和 60 FPS 以上。旧实现的持续流式数据已真实暴露出渲染阻塞（最低 4 FPS、长任务累计约 86 秒）；本轮最后一次真实任务在 compaction 阶段未进入足够长的新增 token 流，因此没有把稀疏的 75,942 → 75,945 观察误报为“持续流式通过”。P34 仍保留未勾选，待目标生产数据规模下完成一次“任务运行中、事件持续增长”的当前版本验收并设定可接受阈值。

## 剩余风险

- 已在真实后端长任务和 55k–75k 历史规模下记录本机基线；尚未完成目标生产环境中当前版本的长时间持续流式验收。
- 真实任务基线使用 Chromium 和本机模型配置；仍需在目标浏览器矩阵、目标机器和真实生产数据规模下复测并确认阈值。
- 尚未在多个真实浏览器内核上运行完整 E2E 矩阵。
- P35 已在本机生成并验证交付包；仍需在目标环境完成部署、升级和回滚验证。

当前剩余项：真实生产大数据内存/帧率基线、多个真实浏览器内核的完整 E2E 矩阵，以及目标环境部署/升级/回滚。

## P36-5.3 Native loaded-session evidence (2026-08-27)

The same loaded-session fixture clicks the last completed assistant's `Branch into a new conversation` action, observes the native `session.fork` request, and re-selects the source through the DSH session-search path before continuing the Trajectory checks. This is partial P36-5.1 evidence; new-session creation, archive/delete, and full Workspace UI lifecycle still require a dedicated host-backed run.

The native lifecycle fixture now uses the DSH search path to select a seeded session, then exercises the New session action, Workspace rename dialog, Workspace delete confirmation, and session row Archive action. It observed `session.create`, `workspace.rename`, `workspace.delete`, and `workspace.archiveSession`; the renamed Workspace and archived session rows changed as expected, with zero browser errors. Full host-backed lifecycle durability and the remaining Workspace ordering matrix remain open.

The loaded native fixture now also exercises the DSH message-feedback controller and composer: focusing an assistant action lazily requests `messageFeedback/list`, rating and saving a note issues `messageFeedback/put`, retracting it issues `messageFeedback/delete`, and submitting a composer prompt issues `session.prompt`. The fixture accepts both English and Simplified Chinese labels and passed with zero browser errors. This is browser interaction evidence for P36-5.2; queue/steer, approval/question, and production host lifecycle acceptance remain open.

The native mux interaction fixture injects an `approval/requested` frame and a `question/requested` frame for the selected session. Playwright clicked `Allow once`, selected `Safe`, submitted the question, observed two authenticated `/api/respond` calls, and received the matching approval/question resolved frames; the Composer takeover was released and the browser stayed error-free. Queue/steer, retry/cancel and production host interaction acceptance remain open.

The native queue fixture injects a DSH-shaped `session/queue` snapshot with two pending user messages. Playwright expanded the queue dock, edited one message, steered it while the session was running, removed the second message, and observed the final empty snapshot. The run recorded `session.updateQueue` actions in `edit`, `steer`, `remove` order with zero browser errors; retry/cancel and production host interaction acceptance remain open.

The native cancel/Plan/Goal fixture selected a running loaded session, clicked the DSH Composer stop control and observed `session.cancel`, injected active `plan` and `goal` projections, exited Plan mode through `commands/execute`, then edited, paused, resumed, and cleared the Goal through `goals/edit`, `goals/pause`, `goals/resume`, and `goals/clear`. The complete run passed with zero browser errors.

The native retry fixture replayed a DSH `llm/retry` event with a finite normal-mode maximum, opened its disclosure to verify retry count, delay, and failure details, then delivered the matching `llm/retry-started` event over the mux and observed the rendered `started` state. The complete E2E run passed with `{ scheduled: true, started: true, details: true }` and zero browser errors; target-production interaction acceptance remains open.

`SHUTU_PERF_EVENTS=1000 npm.cmd run performance` passed against the native dist with a dense loaded fixture. The fixture returned a tail page first, then a preceding page after the DSH `Load earlier history` action. The run observed a `session.history` request with `beforeSeq`, increased logical trajectory rows, a non-zero preserved scroll position, 31 mounted rows, zero console errors, and both native mux/host WebSockets connected. This is repeatable fixture evidence; it does not replace target-production data-scale acceptance.

The fixture now includes assistant text alongside reasoning, tool calls/results, long text, and TypeScript code. The browser path clicked a search result, opened the request details panel, toggled the native Turns collapse control and expanded it again, then loaded an earlier history page. `npm.cmd run performance` passed with `eventCount: 10000`, `trajectoryRows: 33`, logical row count `34`, `domNodes: 887`, `scrollHeight: 1141`, `61` sampled frames after settling, and zero browser errors. The 1000-event boundary run additionally verified the post-prepend scroll anchor remained non-zero. This is repeatable P36-5.3 interaction evidence; production scroll-anchor and continuous-growth acceptance remain open.

With `SHUTU_PERF_ARTIFACT_DIR=../docs/evidence/dsh-native-2026-08-27`, the same dense Chromium path saved `shutu-native-core-search.png`, `shutu-native-core-chat.png`, `shutu-native-core-trajectory.png`, and `shutu-native-core-inspector.png`. The screenshots correspond to the actual Search, loaded Conversation, Trajectory, and request Inspector states; the path also exercised feedback, prompt, Fork, collapse, and history pagination. Settings, Workspace, subagent/jobs and target-environment page evidence remain open.

## P36-5.5 Native export descendant evidence (2026-08-27)

`go test ./internal/webserver -run 'TestSessionExport(HeadMatchesDownloadHeadersWithoutBody|IncludesDescendantLineage)$' -count=1` passed. The native export endpoint now accepts DSH's boolean `includeDescendants` query, validates the parameter, preserves the DSH `dsh-session-*.zip` filename, and exports root/child/grandchild session logs under `session/events.jsonl` and `subagents/<sessionId>/events.jsonl` in deterministic lineage order.

## P36-5.4 Native capability cold-boot evidence (2026-08-27)

`npm.cmd run e2e` passed against the native dist. The desktop browser recorded `host.describe`, `session.list`, `workspace.list`, `settings.describe`, `agentPreset.list`, `credentials.describe`, `dynamicCordisRunner/inventory`, and `dynamicCordisRunner/syncInspectManifest`; `/api/events.host` and `/api/events.mux` both connected, with no console errors or warnings. The fixture is intentionally empty-data, so loaded capability content and target-production provider/skill catalogs remain open work.

The loaded capability fixture returned a DSH-shaped two-model catalog. Playwright opened the native model menu, drilled into the provider group, selected the alternate model, and observed both `session.models` and `session.selectModel` requests with zero browser errors. This closes fixture-level model catalog/selection evidence; subagent, jobs, skills, file references, attachments, permissions, providers and settings content still require dedicated loaded/host-backed coverage.

The loaded capability fixture now also injects a DSH `session/jobs` whole-set snapshot and serves a `subagent.list` catalog. Playwright opened the jobs list and verified running/failed rows, opened the subagent tree and verified the `Renderer worker` entry, and observed the native `subagent.list` request with zero browser errors. Skills, file references, attachments, permissions, Provider and settings content remain open for dedicated loaded/host-backed coverage.

The loaded capability fixture now also serves the native DSH slash skill and `@` reference providers. Playwright typed `/fixture` and verified the `fixture-skill` option after observing `skill.list`, then typed `@src` and verified the `src/main.ts` option after observing both `fileReferences/list` and `sessionReferenceResolver/candidates`; the complete E2E run remained free of console errors. Attachments, permissions, Provider and settings content remain open for dedicated loaded/host-backed coverage.

The loaded capability fixture now covers the remaining P36-5.4 UI slices. Playwright selected the native `/permission` command and its Read-only option, opened Settings → Models and rendered `Fixture Provider` after observing `commands/list`, `commands/execute`, and `llm.providers`, then loaded a historical image message and rendered `fixture.png` through the session-authorized `session.attachment` RPC. The complete `npm.cmd run e2e` run passed with zero console errors and warnings.

## P36-5.5 Native export media evidence (2026-08-27)

`go test ./internal/webserver -run 'TestSessionExport(HeadMatchesDownloadHeadersWithoutBody|IncludesDescendantLineage|IncludesDeduplicatedMedia)$' -count=1` passed. Root and descendant logs are exported together with one deduplicated image payload under `media/<attachmentId>.png`; missing or unavailable attachment storage fails the export before the response is sent.

## P36-5.5 Native WebSocket reconnect evidence (2026-08-27)

`npm.cmd run e2e` passed with a Playwright `WebSocketRoute.close(1011, ...)` fault injected into the first `/api/events.mux` and `/api/events.host` connections. Both native channels reconnected (`connections: {"/api/events.mux":2,"/api/events.host":2}`), the DSH shell remained available, and the only console warning was the expected `[web-runtime] connection lost, retry #1`; no page error or unexpected HTTP/console error occurred.

## P36-6.1 Native dark baseline evidence (2026-08-27)

`SHUTU_E2E_ARTIFACT_DIR=../docs/evidence/dsh-native-2026-08-27 npm.cmd run e2e` passed with a separate Chromium `colorScheme: dark` page. The dark desktop screenshot is `docs/evidence/dsh-native-2026-08-27/shutu-native-dark-desktop.png`; it has zero horizontal overflow, both native WebSockets, no unnamed-button regression, and no console errors or warnings. The error-state matrix now also covers desktop and mobile terminal turn failures; the complete light/dark page matrix remains open.

## P36-6.1 Native loading evidence (2026-08-27)

The same Playwright run delayed the native `session.list` response for 1 second and captured `docs/evidence/dsh-native-2026-08-27/shutu-native-loading-desktop.png` during the low-opacity loading state. The request then settled into the normal DSH shell with no page errors. A separate desktop/mobile run injects a DSH `turn/end` error and captures `shutu-native-error-desktop.png` and `shutu-native-error-mobile.png`, asserting the visible `This turn failed` / `本轮运行失败` status, failure detail, zero horizontal overflow, and zero console errors.

## P36-6.2 Native geometry evidence (2026-08-27)

The native Playwright geometry matrix passed for desktop `1280x900` and mobile `390x844`. Body widths were `1280` and `390` with no horizontal overflow; all sampled scroll containers had valid viewport/content heights. The settings overlay stayed inside the viewport at desktop `{x:240,y:50,w:800,h:800}` and mobile `{x:24,y:24,w:342,h:796}`, and Escape restored focus to its trigger. JSON and screenshots are stored as `docs/evidence/dsh-native-2026-08-27/shutu-native-geometry-{desktop,mobile}.{json,png}`. This is native geometry evidence; pixel-level comparison against separately captured reference builds remains open.

## P36-6.3 Native focus recovery evidence (2026-08-27)

The native accessibility bridge now moves focus into the first dialog control when a `role=dialog` opens, traps Tab/Shift+Tab within its focusable controls, and restores focus to the trigger after removal. `npm.cmd run test -- dsh-native-entry.test.ts` passed three jsdom tests and the full `npm.cmd run e2e` smoke passed; the desktop Settings flow closes with Escape and returns focus to the Settings trigger. Screen-reader announcement coverage and the full multi-browser matrix remain open.

The loaded-session accessibility matrix passed for desktop `1280x900` and mobile `390x844`: each exposed four named short navigation controls, ten settings-dialog focusable controls, and tabpanel labels were present where panels were exposed. Shift+Tab from the first control wrapped to the last, Tab wrapped back, Escape closed the dialog, and both viewports remained free of browser errors. DSH's compact mobile icon controls remain recorded in the geometry evidence rather than being resized by the host.

The same native smoke now injects one rejected `session.search` response, observes the DSH "content search unavailable" status, clears it with Escape, and confirms the next search succeeds with no browser errors. This is fixture-level error recovery evidence; host-backed failure recovery and broader session lifecycle acceptance remain open.

## P36-7.1 / P36-7.3 Native real-session baseline (2026-08-27)

`npm.cmd run real-performance -- s-f15a7e57 30` passed against the running service at `http://127.0.0.1:18099` after the harness fixed its selection branch and masked search-button click. This was the real Super Mario session, not a generated browser fixture. The native DSH page loaded `75,950` historical events and ran for `32.2s`: minimum sampled FPS `62`, average sampled frames `164.7`, JS heap `36→43MiB`, maximum DOM `554`, six Long Tasks totaling `1,788ms`, 16 mounted trajectory rows, logical row count `16`, and zero console errors. The event tail stayed at `75,950` throughout, so this is a static 75k baseline only; 100k history, continuous event growth, event-to-UI latency and reconnect recovery remain open.

The real-task harness now accepts `SHUTU_REAL_TASK_PROMPT` and dispatches `session.prompt` in the background after the native session is attached. It observes `/api/events.mux` frames and page mutations without waiting for a potentially synchronous prompt response, and reports `streamEventFrames`, stream sequence bounds, trigger status, and `eventToUiMs`. A fresh production-scale continuous-stream run is still required before P36-7.1/P36-7.3 can be marked complete.

## P36-8.1 / P36-8.2 / P36-8.3 Local package evidence (2026-08-27)

`node scripts/release-package.mjs --output release/shutu-agent-p36-current` passed from functional code revision `eb8680f` (`eb8680f095b830833a9afffb0842131c7d16f5f9`). The package manifest reports `win32-x64`, binary `bin/shutu-agent.exe`, frontend `web/dist`, and matching revision metadata; native dist verification reported `111` assets and a valid native manifest. `SHUTU_RELEASE_PACKAGE=release/shutu-agent-p36-current node scripts/deployment-smoke.mjs` passed initial, upgrade and rollback copies sharing one data directory, with `/api/health`, `/api/sessions`, `/`, native `host.describe`, and authenticated `/api/events.mux` plus `/api/events.host` WebSocket upgrades all returning valid responses (`101` for both sockets), with shared data preserved across package replacement. This proves local Windows packaging and lifecycle smoke only; Linux, target-environment startup and failure recovery remain open.

## P36-7.1 / P36-7.2 / P36-7.3 Native 100k fixture evidence (2026-08-27)

`SHUTU_PERF_CONTINUOUS_SECONDS=5 npm.cmd run performance` passed with a controlled native mux stream injected after connection: `continuousFrames=46`, the settled sample kept `61` frames, `DOM=887`, and zero browser errors. The injected events are a transport/rendering fixture, not a production Super Mario stream, so real continuous history growth, event-to-UI latency and reconnect recovery remain open.

`SHUTU_PERF_EVENTS=100000 npm.cmd run performance` passed against the native dist. The dense fixture includes reasoning deltas, token usage, two tool call/result pairs, long text and TypeScript code blocks. The native Trajectory loaded a tail page in `2,353ms`, issued a `beforeSeq` request for earlier history, exposed logical row count `33` with `32` mounted rows, maximum DOM `821`, heap `56MiB` at first sample and `44MiB` after settling, `61` frames in the sample window, `1,374ms` cumulative Long Tasks and zero console errors. This is synthetic fixture evidence; real 100k history, continuous growth, event-to-UI latency and reconnect recovery remain open.
