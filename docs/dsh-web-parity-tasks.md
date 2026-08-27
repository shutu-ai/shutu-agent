# DSH Web 功能对齐任务清单

## P36 本轮进展（2026-08-26）

- [x] P36-3：历史分页先按 DSH 的消息边界确定窗口，再对完整有序日志执行一次 native projection；跨页的 turn、step、surface replacement、compaction 与工具关联不会因页面截断而丢失上下文。
- [x] P36-3：`session.history` 返回 DSH Session header（version、id、createdAt、cwd；可用时带 agentPreset），列表同步暴露持久化 title。
- [x] P36-3：完成首批 DSH projection baseline：`asOfSeq`、`title`、`todos`、`plan`、`tokenUsage` 与 `contextPressure`；历史 tail 与 live mux 共用同一投影游标。
- [x] P36-3：按 DSH session-stats 规则折叠 `sessionStats`（turn/step、LLM/tool 时延、TTFT、decode），覆盖取消步骤、空输出和工具结果乱序场景。
- [x] P36-3：从 plan/goal 生命周期与会话配置折叠 DSH `goal`、`permissions` 投影；目标状态、轮次、阻塞原因和权限选择可在 history tail 恢复。
- [x] P36-3：折叠 DSH `subagent` 与 `subagentTiming`；支持 canonical `subagent/descriptor`，并将当前 Shutu `subagent/start` 映射为可消费的 legacy identity。
- [x] P36-3：折叠 DSH `contextBreakdown`；按 request header、surface 消息与 compaction shadow price 计算 system/tools/message token 分项，并复用于 history/live。
- [x] P36-3：为 `session.list` 行补齐 DSH `sessionListMetadata` projection；附件服务启用时同步下发 `imageLimits`，history/list 共用同一能力声明。
- [x] P36-3：补齐 Session header 的 subagent lineage（parent/origin/delegationDepth），并在 history tail 返回可恢复 replacement 状态的 `surface` snapshot。
- [x] P36-3：保真传输 DSH content blocks；统一 Markdown/code text、reasoning、image attachment、tool-result 与未知扩展 block，assistant/tool history/live 使用同一归一化结构。
- [x] P36-3：分页先完成全量 native projection，再按 append-origin message 与 `sourceEventSeqs` 计算 DSH message boundary；replacement 不消耗配额且不会切断消息组。
- [x] P36-2：补齐标准 `session.attachment`（会话引用授权、Base64 数据与附件元数据）及 `session.models` 响应入口。
- [x] P36-2：接入标准 `host.listDirectory`、`host.createDirectory`、`host.pickDirectory`，返回 DSH 目录条目、面包屑、截断标志及结构化能力错误。
- [x] P36-2：接入 `agentPreset.list/select`，将 `minimal/standard/code` 投影为系统 preset；选择仅允许空白会话，并与 `session.create.agentPreset` 共用会话配置存储。
- [x] P36-2：接入 `session.selectModel`，持久化会话级 provider/model/reasoning effort，并让 `session.models.current` 返回会话覆盖值。
- [x] P36-2：接入完整 `workspace.*` 原生写操作：创建/幂等解析、重命名冲突、删除、工作区/会话排序及归档快照；`workspace.list` 返回标准时间字段和 `archivedSessionIds`。
- [x] P36-2：接入 `session.fork` 原生方法：按已完成 turn 边界复制会话前缀，并继承标题、工作区、CWD 与会话配置；无可复制 turn、缺失会话和超出日志边界均返回结构化错误或回退到最近完成 turn。
- [x] P36-2：接入 `session.updateQueue` 原生方法：对齐 DSH `edit/remove/steer` action union，文本编辑写回队列，删除和 steer 复用队列执行器，不支持的富文本块显式返回结构化错误。
- [x] P36-2：接入 `settings.update/replace` 与 `llm.discoverModels` 原生方法；设置更新/替换共享 CAS revision 和脱敏视图，模型探测复用既有 Provider 探测器且不回传 API key。
- [x] P36-2：接入 `skill.list` 原生方法；按 DSH 只返回 user-invocable 技能，统一转换描述、whenToUse、modelInvocable 字段并按名称稳定排序。
- [x] P36-2：接入 `subagent.list` 原生方法；校验父会话存在，投影 child 的 mode/activity/label/hasChildren，并为运行时子代理摘要保留 continuable 标志。
- [x] P36-2：接入 `subagent.history` 原生方法；校验 parent/child lineage 与 mode，复用 session history 的消息边界分页、surface 和 projection baseline。
- [ ] P36-3：完整 projection baseline（所有已挂载 projection key）与生产数据规模验收仍待后续任务补齐。

状态基线：P0–P23 已完成。P24–P35 已完成首轮实现；P36-1–P36-8 为“DSH 原生 UI 接入/视觉替换”新目标。未勾选项表示仍需补齐或在真实环境验收，不将未验证内容标记为完成。

约束：`deepseek-harness` 仅作为只读参考，不修改其任何文件；新功能可以使用干净的新接口，不保留旧数据或旧接口兼容层。

## P36 执行任务列表（动态）

- [x] P36-1.1：生成 DSH 版本、Git revision、只读来源根目录和原生插件 roster manifest。
- [x] P36-2.1：实现会话、工作区、设置、模型、权限、文件、附件、命令、技能、队列、审批和导出接口。
- [x] P36-2.1a：接入 DSH `goal.create/edit/pause/resume/complete/clear` 原生 RPC，复用 Shutu plan/goal 引擎、事件持久化和 revision CAS。
- [x] P36-2.1b：接入 `host.openPath` 与 `credentials.set/unset`，补齐主机 opener、凭据引用校验、环境变量只读保护和 provider key 持久化。
- [x] P36-2.1c：接入 `settings.openDocument`，由服务端绑定配置文档并复用主机 opener，不向浏览器暴露任意路径。
- [x] P36-2.1d：接入 `agentPreset.read/copy/openDocument/remove`，以数据目录安全存储用户 preset，并让 `session.create/select` 识别用户 preset。
- [x] P36-2.2：补齐 host downlink、连接状态、断线重连、续传及剩余原生接口。
- [x] P36-2.2a：host downlink 首次连接下发活动会话、session status、Workspace/archive 快照，并在 turn start/end 时推送状态变化。
- [x] P36-2.2b：接入 `subagent.prompt/interrupt`，校验 parent/child lineage 与 continuable mode，并连接 live child inbox/cancel seam。
- [x] P36-2.2c：host downlink 增加新建/移除 session、workspace 增删改序、归档变化和 agent-error 实时 reconciliation；断线重连重新发送完整基线。
- [ ] P36-3.1：补齐全部 projection key，并完成生产数据规模验收。
- [x] P36-3.1a：对齐 DSH 当前声明的 sessionStats/title/todos/plan/goal/tokenUsage/contextPressure/contextBreakdown/permissions/subagent/subagentTiming/sessionListMetadata 初始值与 baseline 传输。
- [x] P36-4.1：接入 layout、theme、brand、sidebar、workspace 和 conversation 插件。
- [x] P36-4.2：接入 tool、trajectory、composer、command、input trigger、reference 和 skill 插件。
- [x] P36-4.3：接入 subagent、jobs、model、permission、plan、goal、settings、attachment 和 question 插件。
- [x] P36-4.4：移除 Shutu 自定义主布局、颜色 token、组件层级和页面导航最终渲染路径。
- [ ] P36-5.1：完成新建、切换、归档、删除、Fork 会话和 Workspace 管理。
- [ ] P36-5.2：完成发送、重试、取消、队列、steer、审批、问题、计划和目标操作。
- [x] P36-5.2a：补齐 DSH `goals/*` Remote 命名空间到 Shutu goal 引擎的参数解包与操作映射，覆盖 create/edit/pause/resume/complete/clear。
- [x] P36-5.2b：接入 DSH `approval/requested`/`question/requested` mux 下行、稳定 rpcId 重放和 `POST /api/respond`，回答结果回写同一 interact 引擎并广播 resolved。
- [x] P36-5.2c：在 native mux 订阅基线下发 DSH `session/queue` 快照，并将队列消息转换为 user content/source/placement 结构。
- [x] P36-5.2d：在 native mux 订阅基线下发已有后台任务的 DSH `session/jobs` 快照，统一 camelCase 字段与毫秒时间戳。
- [x] P36-5.2e：Playwright 原生 loaded-session fixture 覆盖反馈懒加载、评分/备注保存/撤回及 Composer `session.prompt` 发送，并断言对应 DSH Remote 请求实际发出。
- [ ] P36-5.3：完成 Tool 折叠、请求详情、Trajectory、搜索、历史分页和滚动定位。
- [x] P36-5.3c：Playwright 密集事件 fixture 验证搜索命中、请求详情、Trajectory、`beforeSeq` 历史分页、Turn 折叠/展开及虚拟行渲染。
- [ ] P36-5.4：完成子代理、后台任务、技能、文件引用、附件、模型、权限、Provider 和设置。
- [x] P36-5.4a：原生 Remote 接入 `fileReferences/list` 与 `sessionReferenceResolver/candidates`，返回受会话 CWD 限制的文件候选和 canonical `dsh-session:` 引用。
- [x] P36-5.4b：原生 Remote 接入 `pluginInventory/list`，返回与 native manifest 对齐的插件条目和生命周期状态。
- [x] P36-5.4c：Playwright 原生冷启动验证 Host/session/workspace/settings/preset/credentials 与 Cordis inventory 能力握手、双 WebSocket 建立及零控制台错误。
- [ ] P36-5.5：完成 export、feedback、错误恢复、重连和 session 状态提示。
- [x] P36-5.5a：原生 Remote 接入 `messageFeedback/list|put|delete` 的 messageId/version CAS；斜杠命令支持图片附件。
- [x] P36-5.5b：`session.cancel` 对无活动 turn 幂等返回 accepted，对未知会话返回结构化 `session-not-found`。
- [x] P36-5.5c：native mux 重连重新下发 session、pending interaction、queue 和 active jobs 基线；队列 mutation 后向现有订阅推送全量快照。
- [x] P36-5.5d：补齐 DSH `HEAD /api/session.export` 预检，返回与 ZIP 下载一致的类型、文件名和长度且不发送响应体。
- [x] P36-5.5e：`session.export` 支持 `includeDescendants`，按持久化 subagent lineage 将根会话及后代稳定写入 DSH ZIP 路径，并拒绝非法布尔参数。
- [x] P36-5.5f：`session.export` 递归收集根会话及后代的 image block，按附件 ID 去重并写入 DSH `media/<attachmentId>.<ext>` 路径。
- [x] P36-5.5g：Playwright 注入首条 host/mux WebSocket 断线，验证 DSH 原生运行时自动重连、UI 保持可用且无非预期错误。
- [ ] P36-6.1：建立桌面/移动端、深色/浅色、空数据/加载/错误状态截图基线。
- [x] P36-6.1a：通过 Playwright 保存原生 DSH 空数据桌面/移动基线截图；截图目录支持 `SHUTU_E2E_ARTIFACT_DIR`，本轮命令与结果记录在 `docs/dsh-web-parity-acceptance.md`。
- [x] P36-6.1b：通过 Playwright Chromium `colorScheme: dark` 保存原生 DSH 空数据桌面基线，并复用溢出、双 WebSocket、无障碍名称和控制台检查。
- [x] P36-6.1c：通过 Playwright 延迟 `session.list` 请求保存原生 DSH 桌面加载态截图，并验证加载完成后恢复正常 shell、双 WebSocket 和零控制台错误。
- [ ] P36-6.2：对比布局尺寸、字体、间距、颜色、边框、滚动条、焦点和弹层行为。
- [ ] P36-6.3：验证 Tab、快捷键、Escape、读屏语义、ARIA、焦点恢复和触控目标。
- [x] P36-6.3a：Playwright 验证原生设置 Escape 关闭、侧边栏 Enter 键切换、桌面/移动按钮可访问名称及移动端可见按钮不低于 24px；完整 Tab/读屏/焦点矩阵仍待补齐。
- [x] P36-6.3b：Playwright 验证 Settings 弹层由键盘 Escape 关闭后焦点恢复到触发按钮。
- [x] P36-6.3c：host-owned accessibility bridge 为 DSH 原生 `role=dialog` 增加 Tab/Shift+Tab 循环，jsdom 单测与 Playwright 桌面/移动 smoke 均通过。
- [ ] P36-6.4：为每个 DSH 核心页面保留截图和交互证据。
- [ ] P36-7.1：用真实“网页版超级玛丽”长任务验证 5 万、10 万级历史记录。
- [x] P36-7.1a：在当前真实服务的超级玛丽会话上完成原生 DSH 75,950 条历史加载基线；100,000 条与持续增长窗口仍待目标环境。
- [x] P36-7.1b：native DSH Chromium fixture 完成 100,000 条密集历史基线，加载约 2.35s、逻辑行 33、实际挂载 32 行、DOM 821、heap 峰值 56MiB；合成数据不替代真实 100k 任务。
- [ ] P36-7.2：验证 reasoning/token/tool 持续流、长文本、代码块和密集工具调用。
- [x] P36-7.2a：100,000 条 fixture 覆盖 reasoning、token usage、tool call/result、长文本和 TypeScript code block，并通过原生 Trajectory 展示与分页验证。
- [ ] P36-7.3：记录 FPS、JS heap、DOM、Long Task、事件到 UI 延迟和重连恢复时间。
- [x] P36-7.3c：真实任务基准脚本支持在 native session attach 后后台触发受控 `session.prompt`，并记录 mux 事件帧、事件序号和 MutationObserver 事件到 UI 延迟；服务端同步返回也不会阻塞采样。
- [x] P36-7.3b：100,000 条 fixture 记录 native DSH FPS/heap/DOM/Long Task 与控制台错误：61 帧窗口、heap 56→44MiB、DOM 821、Long Task 1,374ms、错误 0；持续增长与重连恢复仍待补齐。
- [x] P36-7.3a：Playwright Chromium 对真实 75,950 条会话完成约 32.2 秒原生观测：heap 36→43MiB、最大 DOM 554、Trajectory 16 行、Long Task 1,788ms、控制台错误 0；持续增长和重连恢复仍待补齐。
- [ ] P36-7.4：在原生 DSH UI 替换完成后重新设定并通过性能阈值。
- [ ] P36-8.1：生成包含 DSH 原生 dist、Go 服务、协议适配层和版本元数据的自包含交付包。
- [x] P36-8.1a：提交 `28b824c` 生成 Windows 自包含包，包含 native dist、Go 二进制、配置/提示词和 `release.json`，并通过 dist/manifest 校验。
- [ ] P36-8.2：验证本机及目标 Windows/Linux 环境的启动、健康检查、API、WebSocket 和静态资源。
- [x] P36-8.2a：本机 Windows 交付包启动 smoke 覆盖 `/api/health`、会话 API、静态首页和 native `host.describe` RPC；Linux/目标环境及 WebSocket 仍待补齐。
- [ ] P36-8.3：验证升级、数据目录复用、回滚和失败恢复。
- [x] P36-8.3a：本机 Windows 部署 smoke 使用共享 `data_dir` 完成初始包、升级副本和回滚副本启动检查；目标环境失败恢复仍待补齐。
- [ ] P36-8.4：完成目标环境部署记录、验收报告和回滚操作手册。

## 总览

| 编号 | 任务 | 优先级 | 状态 | 依赖 |
|---|---|---:|---|---|
| P24 | 轨迹数据模型对齐 | P0 | 首轮完成 | P23 |
| P25 | 实时流式轨迹 | P0 | 首轮完成 | P24 |
| P26 | 完整请求详情 Inspector | P1 | 已完成 | P24 |
| P27 | 历史分页与边界状态 | P1 | 已完成 | P24 |
| P28 | TrajectoryToolbar 与状态管理 | P1 | 已完成 | P24、P26 |
| P29 | 时间线完整能力 | P1 | 已完成 | P24、P28 |
| P30 | 搜索索引与结果呈现增强 | P1 | 已完成 | P24 |
| P31 | Conversation 展示模型对齐 | P1 | 已完成 | P24 |
| P32 | 异常场景与恢复机制 | P0 | 已完成 | P25、P27、P30 |
| P33 | 可访问性与键盘交互 | P2 | 已完成 | P28、P29、P31 |
| P34 | 性能、真实后端联调与验收 | P0 | 部分完成（保留真实大数据基线） | P25、P27、P32、P33 |
| P35 | 生产部署与交付验证 | P2 | 部分完成（待目标环境） | P34 |
| P36-1 | DSH 原生前端构建入口 | P0 | 部分完成（待 P36-2 bridge） | P34、P35 |
| P36-2 | DSH Web API/RPC/WebSocket 适配层 | P0 | 部分完成（核心 RPC/downlink） | P36-1 |
| P36-3 | DSH Session/Conversation 数据模型适配 | P0 | 部分完成（native history/live projection） | P36-2 |
| P36-4 | DSH 原生 UI 插件与视觉替换 | P0 | 已完成首轮实现 | P36-1、P36-3 |
| P36-5 | DSH 全量交互与状态能力 | P0 | 未开始 | P36-2、P36-4 |
| P36-6 | 视觉、响应式、键盘与无障碍验收 | P1 | 未开始 | P36-4、P36-5 |
| P36-7 | 真实任务性能与持续流式验收 | P1 | 未开始 | P36-5、P36-6 |
| P36-8 | 原生 UI 生产交付与目标环境验证 | P1 | 未开始 | P36-7 |

## 详细任务

### P24：轨迹数据模型对齐（首轮实现）

- [x] 建立统一的 trajectory record 模型，区分 turn、request、assistant、tool、reasoning、compaction 和错误记录。
- [x] 补齐 request/turn/assistant/tool 的父子关系、请求边界、稳定 ID 和请求编号。
- [x] 让时间线、轨迹列表、Inspector 和搜索共用同一份投影模型。
- [x] 支持 request-only、tool-only 和不完整事件，不依赖前端猜测事件类型。

验收标准：同一事件在列表、时间线、Inspector 和搜索中的身份一致；父子关系、请求编号和边界与 DSH 参考实现一致。

### P25：实时流式轨迹（首轮实现）

- [x] 支持流式事件对应记录的原地更新，而不是重复追加新卡片。
- [x] 展示进行中的 request、assistant 输出、tool 调用和结束状态。
- [x] 支持增量文本、reasoning、Token 使用量和最终结果更新。
- [x] 处理事件乱序、重复事件和 SSE 重连后的续传。

验收标准：流式请求过程中行身份稳定，内容持续更新；重连或重复事件不会造成重复记录或错误结束状态。

### P26：完整请求详情 Inspector（已完成）

- [x] 对齐 DSH 的请求概览、输入消息、输出消息、原始事件和耗时分区。
- [x] 展示模型、参数、请求选项、消息来源、工具 schema 和父级记录。
- [x] 区分单次请求 Token、累计 Token、缓存 Token 和费用/统计字段（字段存在时）。
- [x] 增加重试、取消、失败原因和服务端错误详情展示。

验收标准：选中任一 request 或其子记录，都能查看完整上下文，并能从 Inspector 定位对应轨迹记录。

### P27：历史分页与边界状态（已完成）

- [x] 增加“更早历史”边界行和明确的加载中状态。
- [x] 支持加载失败、重试、无更多历史和会话切换时的状态清理。
- [x] 保持加载旧记录前后的滚动锚点和当前选中记录。
- [x] 将 `has_more`、游标和历史起始序号纳入新的前端接口。

验收标准：向上滚动可以稳定分页；重复加载、失败重试和切换会话不会造成跳动、重复或丢失记录。

### P28：TrajectoryToolbar 与状态管理（已完成）

- [x] 补齐 DSH 工具栏中的实际耗时、实际时间、全部折叠和搜索状态控制。
- [x] 统一轨迹搜索、时间线筛选、折叠状态和选区状态的状态来源。
- [x] 支持当前会话内的状态恢复，以及切换会话时的正确重置。
- [x] 让工具栏在长列表滚动时保持可见，并提供清晰的当前状态反馈。

验收标准：工具栏操作会同步影响轨迹列表和时间线；切换会话后不会泄漏上一会话的筛选或折叠状态。

### P29：时间线完整能力（已完成）

- [x] 展示 turn 边界、时间刻度、持续时间和空闲间隔。
- [x] 支持点击、Shift 范围选择、拖拽范围选择、键盘扩展和选区清除。
- [x] 时间线选区与轨迹列表、Inspector 双向同步。
- [x] 覆盖无时间戳、时间倒序、超长间隔和单点事件。

验收标准：时间线能够表达完整请求时序；任何选区操作都能定位正确记录，并在异常时间数据下保持可用。

### P30：搜索索引与结果呈现增强（已完成）

- [x] 从同步全量过滤升级为可增量更新的搜索索引。
- [x] 增加输入防抖、大小写/匹配策略和大历史记录下的响应控制。
- [x] 展示匹配字段、摘要片段和匹配高亮。
- [x] 搜索结果保持 turn/request 结构，不破坏父子关系和时间线完整性。

验收标准：搜索能快速定位结构化字段和正文内容；结果能说明命中位置，同时仍可查看其完整上下文。

### P31：Conversation 展示模型对齐（已完成）

- [x] 对齐 DSH conversation node 的稳定 ID、消息层级和 assistant/tool 关系。
- [x] 补齐 Markdown、代码块、附件、图片、reasoning 和工具调用展示。
- [x] 支持消息节点折叠、展开、复制和定位到对应轨迹记录。
- [x] 处理空消息、部分消息、失败消息和超长消息。

验收标准：Conversation 与 Trajectory 对同一请求使用一致的身份和上下文；常见消息类型的展示与 DSH 行为一致。

### P32：异常场景与恢复机制（已完成）

- [x] 覆盖 API 的 401、404、409、429、5xx 和未知错误。
- [x] 覆盖 SSE 断开、重连失败、Last-Event-ID 续传和重复事件。
- [x] 覆盖空会话、无匹配结果、坏时间戳、缺失详情和 malformed JSON。
- [x] 覆盖超长输出、超大 tool 参数、图片加载失败和浏览器存储不可用。
- [x] 为加载失败、局部失败和不可恢复失败提供重试或明确提示。

验收标准：错误不会导致整个页面白屏或静默失败；用户能知道影响范围，并能在可恢复场景继续操作。

### P33：可访问性与键盘交互（已完成）

- [x] 完成 tab、button、list、tree/timeline 和 Inspector 的语义标注。
- [x] 支持键盘切换 Tab、选择记录、扩展范围、折叠、打开和关闭 Inspector。
- [x] 完善焦点恢复、Escape 关闭、选中状态和虚拟列表中的可见性提示。
- [x] 检查颜色对比度、错误提示、动态更新播报和移动端触控目标。

验收标准：不使用鼠标也能完成主要浏览、筛选和检查操作；无明显焦点丢失或不可访问控件。

### P34：性能、真实后端联调与验收（部分完成）

- [x] 使用真实 Go Web API 和 SSE 完成端到端联调。
- [ ] 在真实生产规模和持续流式输出下验证大历史记录、长文本、密集 tool 调用的内存与帧率（已用真实“网页版超级玛丽”长任务补充本机 55k–75k 历史基线；仍待目标生产环境当前版本的持续增长窗口）。
- [x] 通过 100,000 行虚拟列表和 10,000 条事件的合成基线验证虚拟列表、动态高度、搜索索引和 Inspector 联动的边界行为。
- [x] 建立桌面、移动端、空数据、异常数据和实时数据的自动化验收矩阵。
- [x] 完成单测、类型检查、构建、E2E、`go test`、`go vet` 和 `go build`。

验收标准：真实后端和模拟数据均通过验收；大数据量和流式场景无功能回退，发布构建可重复生成。

### P35：生产部署与交付验证（部分完成）

- [x] 生成包含前端 dist、Go 服务、配置/提示词资源和版本元数据的生产交付包。
- [x] 验证 `--web-only` 启动、静态资源加载、API/SSE 路由和健康检查。
- [ ] 完成目标环境部署、升级和回滚验证。
- [x] 补齐部署说明、配置项、故障排查和验收记录。

验收标准：交付包可在目标环境独立启动，核心页面、API、SSE 和错误处理均可用，并有可重复的部署步骤。

### P36-1：DSH 原生前端构建入口（部分完成）

- [x] 在 `shutu-agent` 内建立独立的 React/Cordis/Vite 原生入口；`SHUTU_DSH_NATIVE=1` 时不再以当前单体 `App` 作为 UI 根节点。
- [x] 以只读方式接入 DSH client package 的 boot、module loader、uiRenderer、slot 和 plugin manifest 入口约定；运行时 boot manifest/插件下发由 P36-2 补齐。
- [x] 解决当前 bundle 的依赖、dist 和 source map 闭包；`npm.cmd run build:native` 可生成 native dist，运行时不读取 `deepseek-harness` 源目录。
- [x] 建立 DSH 版本/来源清单，确保后续 DSH 参考变化可追踪；native build 生成 `dist/dsh-native-manifest.json`。

验收标准：构建入口和 DSH boot 调用链已具备；待 P36-2 提供 `window.__ModuleLoader__`、`window.__DSH_BOOT__` 和插件 bundle 后，才能在浏览器中完成原生 UI 启动验收。

### P36-2：DSH Web API/RPC/WebSocket 适配层

- [x] 实现 DSH client connection 所需的 RPC 请求/响应和错误协议；新增 `client-request` / `server-response` envelope、`rpcId` 回显和结构化错误。
- [x] 接入核心会话/工作区方法：`host.describe`、`session.list/search/create/history/rename/prompt/cancel`、`workspace.list`。
- [x] 实现 `/api/events.mux`、`/api/events.host` 的 downlink-only WebSocket 升级和 session subscription/event frame。
- [x] native bundle 自带 `client-modules` bootstrap，并在入口安装最小 `__ModuleLoader__` / `__DSH_BOOT__` graph。
- [x] 实现 `session.attachment` 的会话引用校验、Base64 读取和 DSH `ImageAttachmentRef` 映射；实现 `session.models` 标准方法入口。
- [ ] 实现会话、工作区、设置、模型、权限、文件、附件、命令、技能、队列、审批和导出接口。
- [ ] 补齐 host downlink 事件、连接状态、断线重连、续传，以及剩余设置/模型/权限/文件/附件/命令/技能/队列/审批/导出接口。
- [x] 已建立 Go handler、核心契约测试和 WebSocket subscription baseline 测试；replay fixture 与完整协议覆盖留待后续补齐。

验收标准：核心 RPC/downlink 已具备；待 P36-3/P36-4 补齐原生插件依赖的方法和插件事件后，DSH 原生 client 才能完成完整启动、打开、发送和恢复验收。

### P36-3：DSH Session/Conversation 数据模型适配

- [x] 将 Go 事件映射为 DSH Session header、surface、Conversation node 和 request/tool/turn 关系。
- [x] 对齐 stable ID、seq、父子关系、分页游标、实时 frame 和 compaction 语义。
- [x] 对齐 Markdown、代码块、reasoning、图片、附件、Token、错误和 produced files 数据中的消息 content block 结构；保留未知扩展 block。
- [x] replay、live stream 和 reconnect 使用同一份投影结果；分页边界也在完整投影游标上计算。

验收标准：DSH 原生 Conversation、Tool、Trajectory 和 Inspector 组件可以直接消费 Shutu 适配后的 DSH 数据。

### P36-4：DSH 原生 UI 插件与视觉替换

- [ ] 接入 DSH layout、theme、brand、sidebar、workspace 和 conversation 插件。
- [ ] 接入 DSH tool、trajectory、composer、command、input trigger、reference 和 skill 插件。
- [ ] 接入 DSH subagent、jobs、model、permission、plan、goal、settings、attachment 和 question 插件。
- [ ] 移除当前 Shutu 自定义主布局、颜色 token、组件层级和页面导航作为最终渲染路径。

验收标准：页面 DOM 结构、布局、主题 token、组件文案和视觉行为与 DSH 原生 Web 一致。

### P36-5：DSH 全量交互与状态能力

- [ ] 完成新建、切换、归档、删除、Fork 会话和 Workspace 管理。
- [ ] 完成发送、重试、取消、队列、steer、审批、问题、计划和目标操作。
- [ ] 完成 Tool 展开/折叠、请求详情、Trajectory、搜索、历史分页和滚动定位。
- [ ] 完成子代理、后台任务、技能、文件引用、附件、模型、权限、Provider 和设置。
- [ ] 完成 export、feedback、错误恢复、重连和 session 状态提示。

验收标准：DSH Web E2E 场景在 Shutu 后端上逐项通过，不保留当前自定义 UI 的替代交互。

### P36-6：视觉、响应式、键盘与无障碍验收

- [ ] 建立 DSH 桌面/移动端、深色/浅色、空数据/加载/错误状态截图基线。
- [ ] 对比布局尺寸、字体、间距、颜色、边框、滚动条、焦点和弹层行为。
- [ ] 验证 Tab、快捷键、Escape、读屏语义、ARIA、焦点恢复和触控目标。
- [ ] 为每个 DSH 核心页面保留截图和交互证据。

验收标准：视觉差异在约定阈值内，主要交互和无障碍检查与 DSH 结果一致。

### P36-7：真实任务性能与持续流式验收

- [ ] 用真实“网页版超级玛丽”长任务验证 5 万、10 万级历史记录。
- [ ] 验证 reasoning/token/tool 持续流、长文本、代码块和密集工具调用。
- [ ] 记录 FPS、JS heap、DOM、Long Task、事件到 UI 延迟和重连恢复时间。
- [ ] 在原生 DSH UI 替换完成后重新设定并通过性能阈值。

验收标准：当前版本在目标数据规模和持续流式输出下无明显卡顿、泄漏、重复事件或功能回退。

### P36-8：原生 UI 生产交付与目标环境验证

- [ ] 生成包含 DSH 原生 dist、Go 服务、协议适配层和版本元数据的自包含交付包。
- [ ] 验证本机、目标 Windows/Linux 环境的启动、健康检查、API、WebSocket 和静态资源。
- [ ] 验证升级、数据目录复用、回滚和失败恢复。
- [ ] 完成目标环境部署记录、验收报告和回滚操作手册。

验收标准：交付包在目标环境呈现与 DSH 一致的 UI 和功能，并有可重复的部署、升级和回滚步骤。

## 推荐执行顺序

```text
P24 数据模型
 ├─ P25 实时流式轨迹
 ├─ P26 请求详情 Inspector
 ├─ P27 历史分页
 ├─ P28 工具栏与状态
 │   └─ P29 时间线完整能力
 ├─ P30 搜索增强
 └─ P31 Conversation 对齐

P25 + P27 + P30 ──> P32 异常与恢复
P28 + P29 + P31 + P32 ──> P33 可访问性
P25 + P27 + P32 + P33 ──> P34 联调与验收
P34 ──> P35 部署交付
```

P24–P35 已完成首轮实现；P34/P35 的真实环境事项与 P36-1–P36-8 的 DSH 原生 UI 接入并行推进，所有未勾选项需在对应验收证据完成后再标记完成。

## P36 当前实现进度（2026-08-26）

本轮完成 P36-3 的首轮原生事件投影：`session.history` 与 `events.mux` 共用 DSH `SessionEvent` 转换器，统一消息 ID、turn/step、surfaceOp/sourceEventSeqs、tool-result、retry、Markdown/code text、reasoning、图片 attachment、未知 content block、subagent identity/timing、contextBreakdown、sessionListMetadata、Session header lineage、surface snapshot、DSH message-boundary 分页与未知事件的 ignorable 标记，并补充 replay/live、compaction、retry、subagent、contextBreakdown、session-list、imageLimits、header/surface、content block、分页 replacement/source-group 测试。P36-3 仍保留未勾选项：真实原生 UI 端到端验收。

## P36 native loaded-session verification slices

- [x] P36-5.3a: Playwright Chromium native fixture verifies session search opens a loaded session and the DSH Trajectory tab renders a bounded virtual table over dense tool, reasoning, code, and long-text history.
- [x] P36-5.3b: The native fixture serves tail-paged `session.history`; `Load earlier history` sends `beforeSeq`, prepends logical rows, and preserves the virtualized rendering/error-free browser state.
