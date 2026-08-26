# DSH Web 功能对齐任务清单

状态基线：P0–P23 已完成。P24–P35 已完成首轮实现；P36-1–P36-8 为“DSH 原生 UI 接入/视觉替换”新目标。未勾选项表示仍需补齐或在真实环境验收，不将未验证内容标记为完成。

约束：`deepseek-harness` 仅作为只读参考，不修改其任何文件；新功能可以使用干净的新接口，不保留旧数据或旧接口兼容层。

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
| P36-3 | DSH Session/Conversation 数据模型适配 | P0 | 未开始 | P36-2 |
| P36-4 | DSH 原生 UI 插件与视觉替换 | P0 | 未开始 | P36-1、P36-3 |
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
- [ ] 建立 DSH 版本/来源清单，确保后续 DSH 参考变化可追踪。

验收标准：构建入口和 DSH boot 调用链已具备；待 P36-2 提供 `window.__ModuleLoader__`、`window.__DSH_BOOT__` 和插件 bundle 后，才能在浏览器中完成原生 UI 启动验收。

### P36-2：DSH Web API/RPC/WebSocket 适配层

- [x] 实现 DSH client connection 所需的 RPC 请求/响应和错误协议；新增 `client-request` / `server-response` envelope、`rpcId` 回显和结构化错误。
- [x] 接入核心会话/工作区方法：`host.describe`、`session.list/search/create/history/rename/prompt/cancel`、`workspace.list`。
- [x] 实现 `/api/events.mux`、`/api/events.host` 的 downlink-only WebSocket 升级和 session subscription/event frame。
- [x] native bundle 自带 `client-modules` bootstrap，并在入口安装最小 `__ModuleLoader__` / `__DSH_BOOT__` graph。
- [ ] 实现会话、工作区、设置、模型、权限、文件、附件、命令、技能、队列、审批和导出接口。
- [ ] 补齐 host downlink 事件、连接状态、断线重连、续传，以及剩余设置/模型/权限/文件/附件/命令/技能/队列/审批/导出接口。
- [x] 已建立 Go handler、核心契约测试和 WebSocket subscription baseline 测试；replay fixture 与完整协议覆盖留待后续补齐。

验收标准：核心 RPC/downlink 已具备；待 P36-3/P36-4 补齐原生插件依赖的方法和插件事件后，DSH 原生 client 才能完成完整启动、打开、发送和恢复验收。

### P36-3：DSH Session/Conversation 数据模型适配

- [ ] 将 Go 事件映射为 DSH Session header、surface、Conversation node 和 request/tool/turn 关系。
- [ ] 对齐 stable ID、seq、父子关系、分页游标、实时 frame 和 compaction 语义。
- [ ] 对齐 Markdown、代码块、reasoning、图片、附件、Token、错误和 produced files 数据。
- [ ] 确保 replay、live stream 和 reconnect 使用同一份投影结果。

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
