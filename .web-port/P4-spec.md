# P4 移植规格：子代理列表 + 后台任务列表面板

> 目标：把 dsh web 的「子代理目录」「后台任务」两类只读列表 UI 移植到 github.com/shutu-ai/shutu-agent（Go 单体、vanilla JS 无构建、零依赖、中文文案）。
> 数据源：`GET /api/subagents`、`GET /api/jobs`（只读快照，无 RPC 控制）。
> 本规格是「可照抄」的实现参考，包含数据映射、配色、文案与缺口清单。

---

## 1. 读过的 dsh 文件清单（相对 `deepseek-harness/`，全部只读）

子代理 UI（独立包 `ui-subagent`，挂载在会话头部 actions 槽位）：
- `packages/client/ui-subagent/src/client/SubagentCatalogAction.tsx`（目录树主组件：trigger + popover tree）
- `packages/client/ui-subagent/src/client/SubagentCatalogAction.module.css`
- `packages/client/ui-subagent/src/client/SubagentReadOnlyComposer.tsx`（只读子代理提示条）
- `packages/client/ui-subagent/src/client/locales.ts`

后台任务 UI（独立包 `ui-jobs`，同样挂会话头部 actions 槽位）：
- `packages/client/ui-jobs/src/client/JobListAction.tsx`（任务列表主组件：trigger + popover）
- `packages/client/ui-jobs/src/client/JobListAction.module.css`
- `packages/client/ui-jobs/src/client/locales.ts`

工作台会话行（子代理运行状态点在会话行上的呈现）：
- `packages/client/ui-workspace/src/client/rows/Rows.tsx`（`sessionStatuses` / `runningSubagentCount`）
- `packages/client/ui-workspace/src/client/locales.ts`

状态点原语与主题：
- `packages/client/ui-primitives/src/StateDot.tsx`、`packages/client/ui-primitives/src/StateDot.module.css`
- `packages/client/ui-theme/src/styles/design-platform.css`（全部 `--dsw-static-*` 与 `--dsw-alias-*`）
- `packages/client/ui-theme/src/styles/gradient-shadow-text.css`（`--dsw-shadow-lv3`）
- `packages/client/ui-theme/src/styles/base.css`（字体族）

对照阅读的 github.com/shutu-ai/shutu-agent 文件（非 dsh，仅用于映射约束）：
- `internal/webserver/static/index.html`、`app.js`、`style.css`
- `internal/webserver/webserver.go`（`handleSubagents` / `handleJobs` / `writeJSON`）
- `cmd/sta/webserver.go`（`webSubagents` / `webJobs` 洗白层）
- `internal/jobs/service.go`（`Status` 枚举：running/stopping/completed/killed/failed）

---

## 2. dsh 两类 UI 的一句话要点 + DOM 结构

### 2.1 子代理目录（ui-subagent）

**一句话**：会话头部的一个触发器按钮（运行中有蓝色追逐动画点 + `{n} 个子代理` 计数 + 下箭头），点击展开 336px 的 `role=tree` 弹层，每一行是「[展开箭头|占位] 状态点 + 名称 + 次级摘要(标题·模式·活动) + 指标(token·时长)」，可递归展开下级子代理，点行跳转到该子代理会话。

**DOM 树草图**（子树递归；`level` 每层 +1）：
```
div.root
├─ button.trigger [aria-haspopup=tree, aria-expanded]
│   ├─ span.activitySlot        → runningCount>0 时放 StateDot(ongoing)
│   ├─ span.count               → "{n} 个子代理"（有运行中则 "{n} 个子代理，正在运行"）
│   └─ IconChevronDownOutline14（展开时 .triggerOpen 旋转 180°）
└─ (open) div.menu [role=tree, aria-label=子代理会话, 336px, max-height 560px]
    └─ (每行) div.node > div.row [role=treeitem, aria-level]
        ├─ button.disclosure(▸ 展开/▾ 收起) | span.disclosureSpace（无下级时占位）
        └─ div.clickarea
            ├─ StateDot(ongoing | done)
            ├─ span.content > span.label（名称） + span.summary（"标题 · 一次性 · 正在运行"）
            └─ span.metrics > span.metricToken（"1.2K tok"） + span.metricDuration（"3分05秒"）
        └─ (展开时) div.children[role=group] → 递归 CatalogRows(level+1)
```
另有加载行（`.loadingRow` 灰态 `正在加载子代理…`）、错误行（`.error` + 「重试」按钮）、诊断行（损坏/版本不受支持/暂不可用）。

### 2.2 后台任务（ui-jobs）

**一句话**：会话头部的触发器按钮（有 live 任务时蓝色点 + `{n} 个后台任务运行中` + 下箭头），点击展开 336px 弹层，任务行 =「状态点 + kind 徽标 + label + 状态/详情 + 时长」，live 任务优先按开始时间升序、已结束按完成时间倒序；仅当存在 ≥1 个任务时才渲染触发器。

**DOM 树草图**：
```
div.root
├─ button.trigger [aria-expanded]
│   ├─ (liveCount>0) StateDot(ongoing)
│   ├─ span.count                 → "{n} 个后台任务运行中" | "{n} 个后台任务"
│   └─ IconChevronDownOutline14
└─ (open) ul.menu [aria-label=后台任务, 336px, max-height 420px]
    └─ (每任务) li.row[.rowSettled]
        ├─ StateDot(dotState(status))   ← running→ongoing, stopping→warning,
        │                                   completed→done, killed→warning, failed→error
        ├─ span.kind（kind 徽标，圆角浅底）←--dsw-alias-fill-l2 底 + label-secondary 字
        ├─ span.label[title]（等宽字体、单行省略）
        ├─ span.status[title=detail??status]（详情优先，无则状态词，max-width 40% 省略）
        └─ span.duration[title=已运行/耗时]（tabular-nums，live 每秒走表）
```

### 2.3 两类的容器说明

两者**不是独立页面**，都是「会话头部 actions 槽位（`conversation.session.header.actions`）」里的弹出触发器，视觉上是 336px 浮层菜单（`--dsw-specific-menu` 底 + `--dsw-shadow-lv3` 阴影），随会话存在与否出现/消失。github.com/shutu-ai/shutu-agent 没有按会话的头部 actions 槽位，且 `/api/subagents`、`/api/jobs` 是**全局快照**（当前 agent / 当前 owner），所以移植应做成**全局面板**（见 §7 位置建议），而不是每个会话一份。

---

## 3. 逐元素数据映射

### 3.1 子代理行（dsh catalog row ↔ `GET /api/subagents` 项 `{id,label,running}`）

| dsh 元素 | github.com/shutu-ai/shutu-agent 字段 | 状态 | 降级方案 |
|---|---|---|---|
| 行点击 → 打开子代理会话（`openChild(address)`） | 无（只读快照，无会话跳转） | **缺** | 行不做可点击跳转；仅展示。P4 隐藏整行交互 |
| 展开箭头（递归子树，`hasChildren` / 后代计数） | 无（扁平行，无 parentId/children） | **缺** | 去掉 disclosure，保留 14px 占位或无；扁平列表 |
| `StateDot`：`activity==='running' ? ongoing : done` | `running ? ongoing : done` | ✅ 直接映射 | — |
| label（`entry.label ?? entry.id`） | `label ?? id` | ✅ | label 为空字符串时回退 id |
| secondary 摘要：`标题 · 模式 · 活动` | title、mode 缺；activity = running ? '正在运行' : '当前未运行' | 部分缺 | 摘要只显示活动状态词；无 title/mode |
| 指标 token 计数（`{formatTokens} tok`） | 无 | **缺** | 隐藏（不渲染 metrics 列） |
| 指标时长（`formatDuration`，live 每秒刷新） | 无 | **缺** | 隐藏 |
| 触发器计数徽标（`descendants.count/runningCount`） | `total = list.length`、`running = Σ running` | ✅ 可算 | 直接前端计数 |
| 诊断行（corrupt/unsupported/unavailable + 重试） | 无对应后端概念 | **缺** | 不实现；错误统一走 `.error` + 「重试」 |

### 3.2 任务行（dsh job row ↔ `GET /api/jobs` 项 `{id,kind,label,status,detail,started_at,finished_at}`）

| dsh 元素 | github.com/shutu-ai/shutu-agent 字段 | 状态 | 降级方案 |
|---|---|---|---|
| `StateDot(dotState(status))` | `status`：running→ongoing / stopping→warning / completed→done / killed→warning / failed→error | ✅ **1:1**（两端 status 字符串完全一致，见 `internal/jobs/service.go`） | — |
| kind 徽标（`.kind`） | `kind` | ✅ | kind 空则隐藏徽标 |
| label（`.label`，等宽字体 + title） | `label ?? id` | ✅ | — |
| 状态/详情列（`detail ?? statusLabel`，title=detail） | `detail ?? 中文状态词` | ✅ | 详情为空时显示状态词（dsh 同款模式） |
| 时长（`.duration`，live=`now-startedAt`，settled=`finishedAt-startedAt`） | `started_at`/`finished_at` | ⚠️ 格式不同（见 §6-①） | 前端把 RFC3339 → epoch ms 再算，或后端改发 epoch ms |
| 排序（live 先按 startedAt 升序；settled 按 finishedAt 倒序） | 同字段可算 | ✅ | 前端排序，直接照抄 dsh `ordered()` 逻辑 |
| 触发器计数（liveCount / totalCount） | `Σ(running|stopping)` / `len` | ✅ | — |
| 触发器「无任务不渲染」（`jobs.length===0 → null`） | 空数组同理 | ✅ | 面板分区空态显示「暂无后台任务」 |

> 注：dsh 任务 UI **没有进度条**——状态呈现全靠 StateDot（+ duration 计时）。github.com/shutu-ai/shutu-agent 无需发明进度条；状态列已表达全部语义。

---

## 4. 状态点 / 徽标配色（自 `ui-theme/src/styles/design-platform.css`、`StateDot.module.css` 解析）

### 4.1 状态点（StateDot）四态色值（深浅主题）

| 语义 | dsh token | 浅色值 | 深色值 | 说明 |
|---|---|---|---|---|
| ongoing（进行中/运行中） | `--dsh-state-ongoing = --dsw-static-deepseek-450` | `rgb(86,134,254)` `#5686FE` | 同左（静态色不随主题） | 像素追逐动画（见下） |
| done（完成/空闲） | `--dsw-alias-state-success-primary = --dsw-static-green-500` | `rgb(34,197,94)` `#22C55E` | `#22C55E` | 绿 |
| warning（等待/停止中/已取消） | `--dsw-alias-state-warn-primary = --dsw-static-amber-500` | `rgb(245,158,11)` `#F59E0B` | `#F59E0B` | 琥珀 |
| error（失败） | `--dsw-alias-state-error-primary` | 浅 `--dsw-static-red-600` `rgb(236,19,19)` `#EC1313` | 深 `--dsw-static-red-400` `rgb(242,90,90)` `#F25A5A` | 红 |

**StateDot 造型**（github.com/shutu-ai/shutu-agent 可整段照抄为纯 CSS）：
- done/warning/error：10×10 圆，`::before` 同色 `opacity:.10` 光晕层（inset:0, radius:50%），`::after` 6×6 实心核（inset:20%）。
- ongoing：3×3 网格上 8 个 2×2 像素矩形，`@keyframes dsh-state-dot-chase 1s infinite`，亮度阶梯 1→0.6→0.35→0.15（各段 12.5%），每格 `animation-delay: index*-125ms` 形成顺时针追逐。
- github.com/shutu-ai/shutu-agent 现有会话行 `.si-dot` 是简化 6px 实心圆（`data-state=running/done/idle`），风格一致；P4 面板推荐用完整 10px halo 版（忠实 dsh），也允许复用简化版（§7 权衡）。

### 4.2 其余语义 token（面板所需，深浅主题取值）

| 用途 | dsh token | 浅色 | 深色 |
|---|---|---|---|
| 行主文本 | `--dsw-alias-label-primary` | `#0F1115` | `#F9FAFB` |
| 次级/摘要/状态/时长 | `--dsw-alias-label-tertiary` | `#ADB2B8` | `#ADB2B8` |
| kind 徽标底色 | `--dsw-alias-fill-l2`（上游 token，本仓库未定义） | → 用 `--dsw-alias-bg-layer-2`（浅 `#F1F3F5`/深 `#2C2C2E`）替代 | 同左 |
| kind 徽标文字 | `--dsw-alias-label-secondary` | `#61666B` | `#CFD3D6` |
| 弹层底色 | `--dsw-specific-menu` | `#FFFFFF` | `#353638` |
| 弹层边框 | `--dsw-alias-border-l2` | `rgba(0,0,0,.10)` | `rgba(255,255,255,.12)` |
| 弹层阴影 | `--dsw-shadow-lv3` | `0 0 1px 0 rgba(0,0,0,.2), 0 0 4px 0 rgba(0,0,0,.02), 0 12px 32px 0 rgba(0,0,0,.08)` | 同左 |
| 行 hover | `--dsw-alias-interactive-bg-hover` | `rgba(38,49,72,.06)` | `rgba(255,255,255,.08)` |
| settled 行文字 | `--dsw-alias-label-tertiary` | 同 tertiary | 同 tertiary |
| 错误文案 | `--dsw-alias-state-error-primary` | `#EC1313` | `#F25A5A` |
| 滚动条 | `--dsw-alias-scrollbar-bg-l2` / `-hover-l2` | `#D0D0D0` / `#E5E5E5` | `#545557` / `#3C3C3D` |

> github.com/shutu-ai/shutu-agent `style.css` 已按同一来源定义这些 `--dsw-alias-*`（深浅两套），面板直接用现有变量即可，仅需补 `--dsw-alias-fill-l2`（用 `bg-layer-2` 代替）与像素追逐动画 keyframes。

---

## 5. 完整中文文案清单

### 5.1 任务状态词（dsh `ui-jobs/locales.ts`，照抄）

| key | 中文 |
|---|---|
| status.running | 运行中 |
| status.stopping | 正在停止 |
| status.completed | 已完成 |
| status.killed | 已取消 |
| status.failed | 已失败 |
| duration.seconds | `{seconds}秒`（秒=0 时显示 `0秒`） |
| duration.minutes | `{minutes}分{seconds}秒` |
| duration.hours | `{hours}小时{minutes}分` |
| duration.title.live | 已运行 `{duration}` |
| duration.title.done | 耗时 `{duration}` |
| count.live.one/other | `{count} 个后台任务运行中` |
| count.idle.one/other | `{count} 个后台任务` |
| list.aria | 后台任务 |

### 5.2 子代理相关（dsh `ui-subagent/locales.ts` 中本面板用到的）

| key | 中文 |
|---|---|
| count.total.one/other | `{count} 个子代理` |
| count.running.one/other | `{count} 个子代理，正在运行` |
| tree.aria | 子代理会话 |
| loading.label | 正在加载子代理… |
| load.error | 无法加载子代理 |
| retry | 重试 |
| activity.running | 正在运行 |
| activity.inactive | 当前未运行 |
| mode.oneShot / mode.continuable | 一次性 / 可继续（**当前无数据源，P4 隐藏**） |
| diagnostic.* | 会话记录损坏 / 子代理记录版本不受支持 / 会话记录暂不可用（**无对应后端，P4 不实现**） |

### 5.3 github.com/shutu-ai/shutu-agent 新增/微调文案（dsh 没有现成词，需自拟）

| 用途 | 中文 |
|---|---|
| 侧栏底部 tab | 运行 |
| 面板标题（可选） | 运行状态 |
| 分区标题 | 子代理 / 后台任务 |
| 空态（子代理） | 暂无子代理 |
| 空态（后台任务） | 暂无后台任务 |
| 手动刷新按钮（title） | 刷新 |
| 刷新失败 | 刷新失败：`{msg}` |
| 加载失败 | 加载失败：`{msg}` |
| 面板关闭（title） | 关闭 |

> 会话行复用现有：`进行中` / `空闲` / `已完成` / `{n} 个子代理运行中`（`ui-workspace/locales.ts`，github.com/shutu-ai/shutu-agent 会话行已部分使用，保持词表一致）。

---

## 6. github.com/shutu-ai/shutu-agent API 缺口清单

### 后端需要补 / 改（可选，尽量不动后端）

1. **`started_at`/`finished_at` 线格式**：`writeJSON` 用 Go `json.NewEncoder`，`time.Time` 默认输出 **RFC3339 字符串**（如 `2025-01-01T12:00:00Z`）；dsh 的 `JobView.startedAt/finishedAt` 是 **epoch ms 数字**。二者任选其一：
   - 推荐 **不动后端**：前端 `new Date(j.started_at).getTime()` 转 epoch ms 再算时长（前后端各一行）。
   - 或后端 `webJobs` 输出 `j.StartedAt.UnixMilli()` / `FinishedAt.UnixMilli()`，与 dsh 字段语义完全对齐（需同步改 webserver 测试）。
2. **子代理层级**：`ListChildren` 只返回扁平 `{id,label,running}`，无 `parentId/children/mode/tokenUsage/duration`。P4 **不补后端**，前端降级为扁平列表、隐藏树与指标（§3.1 缺项）。

### 前端降级 / 隐藏（P4 不做的事）

3. **无 RPC 控制**：不能终止任务（`jobs` 无 kill 端点暴露给 web）、不能打开子代理会话、不能看中间日志 → 面板**纯只读**，无行内操作按钮、无行点击跳转。
4. **无推送**：无 polling 之外的实时通道 → 前端 10s 轮询 + 手动刷新；`document.visibilityState==='hidden'` 时暂停轮询（省资源）。
5. **501 未接线**：provider 未 wire 时 `GET /api/*` 返回 501 → 前端把 501 当「未启用」，显示空态（或面板隐藏），不报错轰炸。401 沿用现有 `api()` 统一处理（跳登录）。
6. **disabled 能力**：`jobs_enabled/subagent_enabled=false` 时后端返回 `[]`（`cmd/sta/webserver.go`），前端自然显示「暂无…」空态，无需额外判断。
7. **`detail` 只在 terminal 时通常有值**（"exit code: N" 等），running 时常为空 → 状态列用 `detail ?? 状态词`（dsh 同款），无需后端保证。
8. **长文本**：label/detail 单行省略（dsh `.label` 省略号、`.status` `max-width:40%` + ellipsis），避免撑破 336px 弹层。

---

## 7. P4 最小闭环建议

### 7.1 位置建议（推荐：侧栏底部 tab）

**推荐「侧栏底部 tab」**：在 `index.html` 的 `.sidebar-foot` 现有「⚙ 设置 / 📚 知识库」旁加一个「🧩 运行」tab。点击后在侧栏内展开一个 **锚定弹层**（绝对定位，对齐 `.si-pop` 视觉、但用 dsh `.menu` 的 336px / max-height / shadow-lv3 参数），内分「子代理」「后台任务」两区。理由：
- 数据是**全局快照**（非某会话），放会话无关的侧栏更符合语义；
- 详情列（`.col-details`，0 宽挂载中）已在 `index.html` 注释里**预留给工具调用详情面板**（每会话视角），塞全局列表会抢掉该预留位，不推荐；
- 侧栏 foot 已有同类导航入口，视觉与路由成本最低（无需新页面，`route()` 不必改——tab 只是面板开关，不切 hash）。

备选（不推荐首版）：顶部 topbar 右侧按钮触发弹层（空间窄，容纳不了两区）；独立 `#/runs` 页面（隐藏会话、成本高）。

### 7.2 首版做（闭环最小）

1. HTML：`.sidebar-foot` 加「🧩 运行」tab + 一个空容器 `<div id="runs-panel" class="runs-panel hidden">`（锚定在侧栏内部、foot 上方）。
2. CSS（style.css，全用现有 `--dsw-alias-*` 变量，仅新增）：
   - `.runs-panel`：336px、max-height、`--dsw-specific-menu` 底、`--dsw-alias-border-l2` 边、`--dsw-shadow-lv3` 阴影、圆角 12px、内部滚动条；
   - 分区头（「子代理」「后台任务」）+ 右侧「刷新」按钮；
   - 子代理行：`StateDot(10px halo) + label + 活动状态词`（次级），无 disclosure/metrics；
   - 任务行：`StateDot + kind 徽标 + label(等宽) + 状态/详情 + 时长(tabular-nums)`，live 行 `min-height 32px`、settled 行 tertiary 字色；
   - 像素追逐 keyframes（`dsh-state-dot-chase`）+ 光晕/实心核（§4.1），`prefers-reduced-motion` 关动画；
   - 空态 / 加载态 / 错误态（「重试」）三态样式。
3. JS（app.js，照抄 dsh 逻辑）：
   - `loadRuns()`：并行 `fetch /api/subagents`、`fetch /api/jobs`（走现有 `api()`，401 自动跳登录，501 记未启用）；渲染两区；失败显示错误行 + 重试；
   - 轮询 10s（`visibilityState==='hidden'` 暂停；面板关闭时停表），手动「刷新」按钮立即拉一次；
   - 任务排序：live（running/stopping）优先按 startedAt 升序，settled 按 finishedAt 倒序（照抄 dsh `ordered()`）；
   - 子代理排序：running 优先，其余保持后端顺序；
   - 时长：`new Date(started_at)` 换算；live 行用面板打开后的 1s 时钟刷新（照抄 dsh `setInterval 1000`，仅 live>0 时开）；
   - tab 点击切换 `.hidden`，面板外点击关闭（复用现有 `closeAnyMenu` 思路）。
4. 空态文案：无子代理/无任务时显示 §5.3 文案。

### 7.3 首版隐藏（明确不做）

- 子代理树/展开递归、token 计量、时长、mode（一次性/可继续）徽标、诊断行；
- 任务/子代理任何行内操作（kill/stop、打开子代理会话、看日志）——无后端支撑；
- 进度条（dsh 本就没有）；
- 详情列挂载方案、独立 `#/runs` 路由页；
- 主题跟随之外的新组件（复用现有 `data-ds-dark-theme` 变量体系）。

### 7.4 验收

- 有 token 时正常出数；401 → 登录；501（provider 未 wire）→ 空态不报错；
- 任务 running 行时长每秒递增、settled 行显示耗时、failed 行红点 + 详情；
- 子代理 running 行蓝色追逐点、其余绿色 done 点；
- 深浅主题切换色值正确（§4 数值）；
- 10s 轮询与手动刷新生效、面板关闭后停止计时。
