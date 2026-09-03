# dsh Web 工作台 P2 页移植规格（github.com/shutu-ai/shutu-agent · 侧栏 + 会话管理）

> 目标：参照 dsh web 的**侧栏（`ui-sidebar`）+ 会话浏览区（`ui-workspace`）**，把 github.com/shutu-ai/shutu-agent 的左侧栏做到「像 dsh 一样」，可用零依赖 vanilla JS + CSS 照此写出等价实现。
> 本规格只研究 dsh 源码，**不修改 dsh 源码**，不写 github.com/shutu-ai/shutu-agent 产品代码。
>
> 源码基线：`D:\dev-projects\Agent\deepseek-harness\packages\client\`
> 覆盖包：`ui-sidebar`（侧栏壳）、`ui-workspace`（会话浏览区：节头/搜索/列表/行/菜单/弹窗）、`ui-layout`（折叠状态，P1 已实现，P2 只核对）、`ui-settings-general`（侧栏脚部「设置」触发行，仅外观）、`session-title`（标题生成规则，供对照 github.com/shutu-ai/shutu-agent 的 title 语义）。
> 前置：P1 规格（`.web-port/P1-spec.md`）已实现三栏壳（侧栏默认 280px、可拖 264–420、<1024px 收 56px 轨）。P2 只做**侧栏内部内容 + 折叠交互**，不重做壳。

---

## 0. 结论摘要（一句话版）

- **侧栏 = 一条竖向 Flex 栈**：logo 行（品牌 + 折叠钮）→「新会话」38px 胶囊钮 → 会话浏览区（flex:1，节头 + 可滚会话列表）→ 脚部「设置」行。折叠态（56px 轨）下品牌行只留 36px 折叠钮、新会话变 36px 圆钮、浏览区只留搜索/添加两个 36px 图标、设置变 36px 圆钮；其余内容整体淡出后卸载。
- **会话行 = 32px 行**：状态点(16px 槽) + 标题(14/20 ellipsis) + 相对时间(12px)；当前会话高亮、运行中蓝点跑马灯、完成未读绿点、悬停把时间换成「…」菜单（重命名/分叉/归档）。每会话组折叠到前 5 条 + 「展开其余 {n} 个会话」。
- **折叠是「滑入 + 交叉淡入」**：展开内容冻结在展开宽度上 150ms 淡出 → 四个上部控件（折叠钮/新会话/添加/搜索）平移 49px + 淡入 150ms → 网格列 300ms 滑到 56px；脚部设置只淡入不位移；冷启动折叠态静态渲染。
- **API 映射**：列表字段 `id/updated_at/blank/title` 齐全；**缺**：`running/completed` 状态字段、`rename/delete/fork/archive/search` 端点、workspace 分组概念、列表变更推送（详见 §6）。
- 功能一句话：**dsh 侧栏 = 品牌行 + 新会话 + 可分组/可搜索/带状态点的会话列表 + 折叠轨 + 脚部设置行**。

---

## 1. 侧栏整体（壳 SidebarRoot）

源码：`ui-sidebar/src/client/{SidebarRoot.tsx, SidebarRoot.module.css, contract/slots.ts, locales.ts}`。

### 1.1 几何与折叠态（与 P1 三栏壳的配合）

P1 已交付（本规格只核对，不改动）：
- 布局 store：`{ sidebar:280, details:0, narrow:false, narrowExpanded:false }`；`toggleSidebar()` = narrow 时翻 `narrowExpanded`，否则 `sidebar = sidebar===0 ? 280 : 0`。
- AppFrame 解析：`sidebarCollapsed = narrow ? !narrowExpanded : sidebar===0`；`computeColumns` 把 0 偏好解析为固定 **56px 轨**（`SIDEBAR_COLLAPSED`），折叠时列宽 = 56、有右描边、**无拖柄**（`!sidebarCollapsed && <DragHandle …>`）。
- 壳收到两个参数：`collapsed: boolean`、`width: number`（展开 = 264–420 偏好，折叠 = 56）。
- DOM 数据属性 `data-sidebar-collapsed` 已在 P1。

**P2 职责**：壳接收 `collapsed`/`width`，渲染品牌行 + 新会话 + 浏览区 + 脚部，并实现折叠动画与滚动条亲和。折叠按钮 → `toggleSidebar()`（复用 P1 的 store action 与 300ms 网格过渡）。

### 1.2 DOM 树

```
div.sidebarRoot            /* .root: flex col; height:100%; padding:6px 12px; box-sizing:border-box;
                              bg:var(--dsw-specific-sidebar-fill); color:label-primary; font-size:14px;
                              --dsh-scrollbar-thumb/…hover: 用 l2 对（侧栏高于对话面） */
  div.logoRow              /* 展开: flex row, justify-end, gap8, height60, padding 8px 0 8px 4px, mb8;
                              折叠: height36, padding0, mb12, justify-start */
    button.brand[.wide]    /* 仅展开; flex:1; 透明底; cursor:pointer; aria-label=新建会话; onClick=startSession() */
      span.brandIdentity   /* inline-flex, gap8, height24 */
        span.brandMark     /* 品牌标 24px（本项目: 自定义 logo 或占位） */
        span.brandName     /* 18/600/24, letter-spacing 0.04em; 名称「Personal Agent」（可换） */
                           /* 可选: span.buildRevision 版本徽标: mono 8px, 白字/墨底 r3 */
    button.iconButton.toggle /* 28px 圆钮; 面板图标 IconPanelLeftOutline16; aria/tooltip 收起侧边栏;
                                折叠时 36px 圆钮, 常驻鲸鱼标, hover 换面板图标 */
  button.newSession        /* 38px 胶囊(实为 r12 方角); 见 §2.1 壳级; aria-label=新建会话 */
    IconNewChatOutline16(14/18) + span.newSessionLabel(仅展开) "新会话"
  div.regionArea           /* flex:1; min-height:0; margin:-4 / -12(抵消壳内边距); padding-left:4; overflow:hidden */
    → 会话浏览区（§2，本规格核心）
  div.footArea             /* flex col; 折叠时居中 */
    div.footerActions      /* 可选操作槽（本项目可空） */
    div.settingsArea       /* → 脚部「设置」触发行（§2.7） */
```

折叠态类：`.root.collapsed`（padding `18px 10px 6px`）、`.root.railIn`（仅热折叠）、`.root.fading`（淡出中）、`.root.quietBars`（指针不在列内）。

### 1.3 折叠动画（slide + crossfade，3 阶段）

1. **淡出**：折叠触发时，整个展开内容**冻结在其展开宽度**（内联 `width = lastWideWidth`），`opacity→0` 过渡 **150ms**（`--ds-ease-in-out`）；此时网格列还在滑动裁剪它，**不回流**。
2. **落位**：150ms 后（`COLLAPSE_SETTLE_MS=150`）宽内容卸载；四个上部控件——折叠钮、新会话、（浏览区里）添加、搜索——从「原轨右缘」平移 `translateX(49px)` + 淡入 **150ms**（`.railIn`），与 AppFrame 剩余 150ms 的列滑动同时结束；每个 36px 控件盒落到轨内 10px 左内边距。
3. **脚部**：`sidebar.settings` 只参与淡入时间轴，**无水平位移**。
- **冷启动折叠**（刷新即折叠）直接静态渲染轨（无 `.railIn` 动画）。
- 展开是 200ms `wide-in` 淡入重挂载（`.wide`）。
- `prefers-reduced-motion: reduce` → 全部 transition/animation 关闭。

vanilla 实现要点：用 `setTimeout(150)` 换轨类并卸载宽内容；宽内容冻结宽度用内联 style；列滑动由 P1 的网格 `transition: grid-template-columns 300ms` 承担。

### 1.4 滚动条指针亲和（quietBars）

- 侧栏内所有滚动区的滚动条是「指针亲和」的：指针不在列内时把 `--dsh-scrollbar-thumb / --dsh-scrollbar-thumb-hover` 重绑为 `transparent`（列的滚动条不变，只是拇指隐藏）。
- 判定用**列的几何包围盒**（document `pointermove` + `getBoundingClientRect`），不是 DOM containment（设置全屏面板是列的 fixed 后代，不能靠 leave 事件）；离开后延迟 **2000ms**（`SCROLLBAR_LINGER_MS`）再隐藏，2s 内返回则取消。
- 列表用 `scrollbar-gutter: stable` 预留槽位 → 拇指显现/隐藏**不回流行**。
- 简化移植：可先做「指针离开列 2s 隐藏拇指」，骨架一致即可。

### 1.5 折叠交互语义（P2 的「折叠交互」部分）

- 折叠钮：宽态点击 → `toggleSidebar()`（写 store，`sidebar→0`，壳收 56px 轨）；窄态（<1024）点击 → 翻 `narrowExpanded`（在挤压中栏上临时展开，偏好不被改写）。
- 工具提示：展开 `收起侧边栏`、折叠 `打开侧边栏`，延迟 500ms。
- 浏览区「rail 搜索」手势：轨上点搜索图标 → 先 `expandSidebar()`，等 300ms 列滑动结束再把焦点放进搜索框（`EXPAND_SLIDE_MS=300`，避免拖慢滑动）。
- 折叠不影响草稿/搜索 query（query 状态活在浏览区组件外，折叠只是卸载宽 DOM，query 保留）。

---

## 2. 会话浏览区（ui-workspace = 会话列表）

源码：`ui-workspace/src/client/{WorkspaceBrowser.tsx, WorkspaceBrowser.module.css, rows/Rows.tsx, rows/Rows.module.css, tree.ts, stores.ts, locales.ts}`。

### 2.1 节头（sectionHeader，36px）

```
div.sectionHeader   /* flex row, justify-end, gap4, height36, r12, padding-left4, mb4,
                       color:label-tertiary; 宽态另 margin-top2 / margin-right -4 */
  span.sectionLabel[.wide]    /* 14px, max-width45%, ellipsis, line-height20;
                                  分组模式=「工作区」, 单列表=「会话」; 搜索展开时 max-width→0 淡出 */
  div.searchSlot               /* 28px → 100% 过渡 (180ms) */
    div.search                 /* 28px 圆(收起) → 30px 高 r10 + 1px border-l2(展开) */
      button.searchButton      /* 搜索图标; aria-label=搜索会话 */
      input.searchInput        /* 13px; 展开才可见; placeholder=搜索会话…; maxLength=500; Esc 清空+收起 */
      button.clearButton       /* 展开时 X 清空钮 24px */
  div.headerActions            /* 视图选项 + 添加; 搜索展开时隐藏 */
    button.iconButton          /* 视图选项 IconPersonalizationOutline16 → 分组/排序菜单 */
    button.iconButton          /* 添加工作区 IconProjectAddOutline16（无目录流时隐藏） */
```

- **分组/排序菜单**（ViewOptionsMenu，点击视图选项弹出，portal）：
  - 分组方式：`按工作区` / `单列表`；排序方式：`手动排序` / `最近更新`。
  - 偏好持久化（dsh 用 localStorage key `dsh.workspace.view.v5`）：`{groupBy, orderBy, groupExpansion, sessionOrderByAccount, sessionUpdatedAtByAccount}`。**本项目可只持久化 groupBy/orderBy**。
- **搜索**：输入防抖 **250ms** 后调宿主内容搜索（dsh `session.search`，上限 20 条）；本地同时做标题/工作区子串匹配并置顶、后端内容匹配补充；结果页 `< 20` 时提示「仅显示前 {n} 条结果，请缩小搜索范围。」。外部点击关闭（query 非空时只失焦不收起）。详见 §3 缺口。

### 2.2 会话列表（唯一滚动区）

```
div.treeBody            /* flex:1; min-height:0; position:relative */
  div.list[role=tree][aria-label=会话]   /* overflow-y:auto; scrollbar-gutter:stable;
                                             padding-left4, padding-right=(12-8-2)=2(计算见下),
                                             padding-bottom16; margin-right:2px */
    [组模式] 每组: div.groupSection → ProjectRow + 会话行… + 溢出钮
    [单列表] 每会话一个顶层行
    [空]     div.empty "暂无会话"
  span.fade              /* 底部渐隐: absolute bottom0 height24,
                            linear-gradient(transparent, sidebar-fill); pointer-events:none */
```

- 列表滚动条：**8px 宽、r4 拇指、`scrollbar-gutter: stable`**；右侧 2px 边距偏移 + 壳内边距 12px 抵消，让拇指可贴列缘而**不移动行**。
- 行距：组内/单列表/搜索树 `> * + * { margin-top:2px }`；组间 `margin-top:4px`。
- 底部 24px 渐隐遮罩跟踪 `--dsw-specific-sidebar-fill`（浅色下自动变浅）。

### 2.3 会话行（SessionNodeItem，32px）

```
div.sessionRow[role=treeitem][aria-selected][.selected][.menuOpen]  /* 32px, r8, padding 0 8, gap0,
        cursor:pointer, user-select:none, color:label-primary; 挂载淡入 150ms(row-in) */
  span.slot            /* 16x20 居中; 状态点 */
    StateDot(10px)      /* 运行中=蓝像素跑马灯; 完成未读=绿点; 等待交互=琥珀点; 错误=红点 */
    + visuallyHidden 朗读状态文本
  span.title           /* flex:1; 14/20; ellipsis; margin 0 6px 0 4px */
  span.time            /* flex:none; 12px; label-tertiary; 相对时间（刚刚/5分钟/3小时/2天/4个月/1年） */
  span.rowActions      /* 悬停浮现; “…” 16px 圆钮 → 菜单 */
```

- **选中（当前）高亮**：`.sessionRow.selected { background: var(--dsw-alias-interactive-bg-hover) }` —— 与 hover 同色（当前版本没有更强的高亮底）。
- **hover**：行 `interactive-bg-hover` 底；时间隐藏、`…` 菜单钮浮现（CSS 换显，`rowActions{display:none}` → hover/menuOpen 时 `inline-flex`）。
- **状态点规则**（`showStatus = primary.state !== 'done' || completed`）：
  - 优先级：pending 交互（琥珀，等待审批/计划待审/等待回答）> 自身/子代理运行中（蓝）> 子代理运行中 > 完成未读（绿 done）> 空闲（绿 done，无点）。
  - 完成未读（`completed`）：运行结束且未被打开 → 绿点；打开后清除。
  - **空会话（blank）行**：不显示状态点、不显示时间、不显示菜单 —— 它是「新会话」占位行（只对当前选中的 blank 会话可见，标题显示「新会话」）。
- **相对时间桶**（`relativeTime`，纯函数）：`<1min→刚刚`、`<1h→{n}分钟`、`<24h→{n}小时`、`<30d→{n}天`、`<365d→{n}个月`、`else→{n}年`；diff 取 `max(0, now-updatedAt)`。
- **行点击** → `open(id)`（切换会话，§3.2）。
- **可拖拽**（组内重排）：HTML5 drag，`dataTransfer text/plain=sessionId`，插入标记是行间 2px 蓝虚折线（before/after 半行判定）。**本项目无排序端点，P2 可不做拖拽**（见 §6）。

### 2.4 工作区/分组行（ProjectRowItem，34px）

```
div.projectRow[role=treeitem][aria-expanded]   /* 34px, r8; 文件夹图标 + 标题 */
  span.slot.folder          /* 文件夹图标(开/闭); hover 换右向小三角 */
  span.title                /* 分组名（本项目固定「未分组」或省略分组） */
  span.rowActions           /* hover: “…”（重命名/删除工作区）+ “+” 在该组新建会话 */
```

- 组内会话超过 **5 条**（`COLLAPSED_SESSION_LIMIT`）时显示溢出钮：
  `button.sessionOverflowButton "展开其余 {n} 个会话" / "收起"`（28px 高、r8、12px、tertiary 文字，`aria-expanded`）。

### 2.5 分组 / 单列表 / 排序

- **组模式（默认 groupBy='workspace'）**：每工作区一组；dsh 来自 Host 的 workspace 顺序。本项目**无工作区** → P2 可退化为单个「未分组」组，或直接用单列表。
- **单列表（flat）**：所有会话一个顶层行，严格**最新更新优先**（`updatedAt` 降序，id 做 tiebreak）。
- **排序**：`orderBy='updated'`（默认）——新活动会一次性把会话提升到顶部，之后固定；`orderBy='manual'` —— 完全按用户拖拽顺序。store 持久化。本项目无排序端点 → P2 默认且仅实现 `updated`（客户端按 `updated_at` 降序）。

### 2.6 空态

- `div.empty "暂无会话"`：padding 16px 12px、13px、label-tertiary。
- 列表在首拉未完成（phase!=ready）时不渲染独立 loading 行（直接空）；P2 可加一个轻量 loading 占位或直接空态。
- **会话列表加载/变更**：dsh 靠宿主实时推送。本项目**无推送** → P2 用「动作后刷新 + 轮询」（如每 30s `GET /api/sessions` 重拉），并在新会话/切换/发消息后立即刷新。

### 2.7 脚部设置触发行（ui-settings-general，仅外观）

```
button.trigger          /* 42px 行, r12, gap8, padding 0 10px 0 8px, margin 4px -2px,
                           bg:transparent, hover:interactive-bg-hover, font 14/22 */
  IconSettingsOutline16(16) + span.triggerLabel "设置"
.trigger.rail           /* 折叠: 36x36 圆钮, margin 8px 0 10px, 图标 18, 无文字 */
```

- P2 只做触发行外观 + 点击（可先打开一个占位弹层或空操作）；设置面板本体不在 P2 范围。

---

## 3. 会话管理交互（用户流程 + 数据来源）

### 3.1 新建会话

- **壳级**：品牌行点击 / 「新会话」按钮 → `startSession()`。
  - dsh 语义：优先当前会话所在工作区的 blank 会话（复用或新建），否则最近工作区，无工作区则清入纯「新会话」视图。
  - **本项目**：`POST /api/sessions` → 返回新会话 → 置为 current（写 localStorage）→ 列表刷新 → 右侧进入空会话 hero（P1 已有）。
- **行级**：组行「+」→ 在该组新建（本项目无组 → 与壳级相同）。

### 3.2 切换 / 恢复

- 点击会话行 → `open(id)`：
  - dsh：设为 current + 打开事件窗口 + 拉历史。
  - **本项目**：`POST /api/sessions/{id}/resume` → 置 current（localStorage 持久化，刷新恢复）→ `GET /api/sessions/{id}/events` + 订阅 `events/stream`（P1 已实现聊天页数据流）。
- **current 持久化**：localStorage key（对应 dsh `dsh.sessions.current`）；刷新后校验仍存在于列表，否则清空回「无会话」空态。

### 3.3 重命名（行菜单 → 弹窗）

- 行 `…` → `重命名` → Modal：单行 input（44px 高、r22、border-l2、autoFocus、全选），Enter（非 IME 组合态）提交 / Esc 取消。
- 校验：trim 非空；dsh 允许「未修改也提交」（该手势=固定当前自动标题）；冲突校验仅限工作区重命名（会话无冲突规则）。
- 提交中禁用、失败行内红字提示。
- **本项目：缺 rename 端点**（见 §6.2-①）。

### 3.4 删除 / 归档

- dsh 会话行只有 **归档**（无删除）：归档=从所有分组/搜索隐藏，日志与计数槽保留，不确认弹窗、非破坏性；归档当前会话 → 清空选择回新会话视图。
- 工作区行有**删除**（确认弹窗，仅移除工作区注册，文件夹/会话保留 → 会话落入「未分组」）。
- **本项目：缺 delete/archive 端点；无工作区删除语义**。P2 可先隐藏菜单里的删除/归档项，或标记为待端点（见 §6.2）。

### 3.5 标题生成（dsh 语义，供对照）

- dsh 宿主：首个可标题化 user 消息文本 → 确定回退标题（`fallbackSessionTitle`：清控制字符 → 合并空白 → 取前 `fallbackMaxWords` 词 → 按 `fallbackMaxBytes` UTF-8 截断）或 LLM 标题（`session/title` 事件，`source: fallback|provider|user`）；用户重命名即「钉住」。
- 客户端展示（`displayTitleOf`）：**持久 title → cwd 目录 basename → session id** 三级回退；blank 会话显示本地化「新会话」。
- **本项目对照**：`GET /api/sessions` 已返回 `title`（=首条 user 消息）→ 直接映射；无 cwd → 无 basename 层。标题本体**无缺口**（§6.1）。

### 3.6 排序与持久化

- 默认按 `updated_at` 降序（dsh `orderBy='updated'` 首拉即按 recency 全排）。
- dsh 的「一次性提升 + 手动固定」与拖拽重排需要 order 持久化端点 → **缺**，P2 只做静态 recency 排序。

---

## 4. CSS token 值表（暗色默认）+ 浅色差异

主题机制沿用 P1：默认暗色，`body[data-ds-dark-theme]` 切换。下表为**已解开的最终值**（vanilla CSS 直接写值即可）。

### 4.1 侧栏专用 token（ui-theme design-platform.css）

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-specific-sidebar-fill` | `#1B1B1C` (rgb 27,27,28) | `#F9FAFB` (249,250,251) | 侧栏列底（P1 已列） |
| `--dsw-specific-sidebar-nav-item-hover` | `#2C2C2E` (44,44,46) | `#F1F3F5` (241,243,245) | 侧栏项 hover（设置导航单元） |
| `--dsw-specific-sidebar-nav-item-active` | `#43454A` (67,69,74) | `#EBEEF2` (235,238,242) | 侧栏项激活 |
| `--dsw-specific-sidebar-nav-item-active-accent` | `#353638` (53,54,56) | `#E4EDFD` (228,237,253) | 激活项强调（蓝调） |
| `--dsw-alias-button-elevated-fill` | `#43454A` | `#FFFFFF` | 「新会话」按钮底 |
| `--dsw-alias-button-floating-hover` | `#353638` | `#F1F3F5` | 「新会话」hover 底 |

> 注意：**会话行的 hover 与选中同用一个 `--dsw-alias-interactive-bg-hover`**（暗 `rgba(255,255,255,0.08)` / 浅 `rgba(38,49,72,0.06)`），并不用上面的 nav-item 系列（那是设置面板/导航单元用的）。若希望当前会话高亮更明显，P2 可自选 nav-item-active，但与 dsh 现行渲染不同——**按 dsh 现状抄**即可。

### 4.2 侧栏复用的通用 token（暗色 / 浅色）

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-alias-label-primary` | `#F9FAFB` | `#0F1115` | 行标题/品牌 |
| `--dsw-alias-label-secondary` | `#CFD3D6` | `#61666B` | 图标/次要 |
| `--dsw-alias-label-tertiary` | `#ADB2B8` | `#81858C` | 时间/摘要/节头 |
| `--dsw-alias-label-caption` | `#81858C` | `#ADB2B8` | chevron |
| `--dsw-alias-border-l2` | `rgba(255,255,255,0.12)` | `rgba(0,0,0,0.1)` | 新会话描边/搜索展开描边/弹窗输入 |
| `--dsw-alias-interactive-bg-hover` | `rgba(255,255,255,0.08)` | `rgba(38,49,72,0.06)` | 行 hover/选中、圆钮 hover |
| `--dsw-alias-state-business-primary` | `#679EFE` | `#4176E6` | 拖拽插入线（P2 可不做） |
| `--dsw-static-deepseek-450` | `#5686FE`（两主题同） | 同左 | 运行中状态点（像素跑马灯） |
| `--dsw-alias-state-success-primary` | `#22C55E` | 同左 | 完成未读绿点 |
| `--dsw-alias-state-warn-primary` | `#F59E0B` | 同左 | 等待交互琥珀点 |
| `--dsw-alias-state-error-primary` | `#F25A5A` | `#EC1313` | 错误点/错误文案 |
| `--dsw-alias-scrollbar-bg-l2` | `#545557` | `#E5E5E5` | 侧栏滚动条拇指（高于对话面） |
| `--dsw-alias-scrollbar-hover-l2` | `#65676B` | 浅灰 | 拇指 hover |
| `--ds-ease-in-out` | — | — | 缓动曲线（折叠/淡入/展开） |
| `--ds-transition-duration-slow` | 300ms | 同 | 网格列滑动 |

### 4.3 行内尺寸速查（暗/浅无关）

- 壳：padding `6px 12px`；折叠 `18px 10px 6px`；`--dsh-sidebar-inline-padding:12px`。
- 品牌行：展开 60px（pb8 mb8）；折叠 36px（mb12）。品牌名 18/600/24，字距 0.04em；回退名 17px 无字距。
- 圆钮：宽 28px、轨 36px、r50%、透明底、hover `interactive-bg-hover`；轨上图标 18px、展开 16px。
- 新会话：展开 38px 高、r12、padding 8 16、gap6、margin `0 2px 8px`、图标 14；轨 36px 圆、margin `0 0 12px`、图标 18。
- 节头：36px、r12、padding-left4、mb4；标签 14px line20。
- 搜索：收起 28px 圆；展开 30px 高、r10、`width:calc(100%+4px)`、`margin-inline:-2px`、`padding 0 4px 0 0`、1px border-l2；输入 13px；清除 24px。
- 会话行：32px、r8、padding `0 8`；状态槽 16×20；标题 14/20；时间 12px；行间距 2px；组间距 4px。
- 工作区行：34px。
- 溢出钮：28px 高、r8、12px、padding `0 12px 0 28px`。
- 空态：padding `16px 12px`、13px。
- 设置触发行：42px 高、r12、gap8、padding `0 10px 0 8px`、margin `4px -2px`；轨 36px 圆、margin `8px 0 10px`。
- 状态点：10px（done/warning/error=10px 光晕 10% + 6px 实芯；ongoing=10px 3×3 像素跑马灯，1s 循环、每格负延迟 -125ms）。
- 弹窗输入：44px 高、r22、1px border-l2、14px；错误红字 12px。
- 底部渐隐：24px；滚动条：8px 宽 r4、`scrollbar-gutter:stable`、Firefox `scrollbar-width:thin`。

---

## 5. 中文文案清单（源码 `locales.ts` + 字面量）

**侧栏壳（sidebar ns）**
- `新会话`（按钮文字） / `新建会话`（aria） / `打开侧边栏` / `收起侧边栏`

**会话浏览区（workspace ns）**
- 分组：`会话`（单列表节头） / `工作区`（分组节头） / `未分组`
- 视图：`视图选项` / `分组方式` / `按工作区` / `单列表` / `排序方式` / `手动排序` / `最近更新`
- 溢出：`展开其余 {n} 个会话` / `收起`
- 空态：`暂无会话`
- 搜索：`搜索会话`（aria） / `搜索会话…`（占位） / `清除搜索` / `搜索结果`（aria） / `正在搜索会话历史…` / `内容搜索暂不可用，仅显示名称匹配。` / `无匹配会话` / `仅显示前 {n} 条结果，请缩小搜索范围。`
- 添加：`添加工作区` / `添加工作区…`
- 行操作：`重命名` / `重命名会话` / `会话名称` / `分叉会话` / `归档会话` / `会话“{name}”的操作`（aria）/ `工作区“{name}”的操作` / `在“{name}”中新建会话` / `删除工作区` / `将把“{name}”从工作区列表中移除。文件夹与会话记录会保留，其会话将显示在“未分组”下。` / `正在删除工作区…` / `重命名工作区` / `工作区名称` / `已存在名为“{name}”的工作区。` / `关闭`
- 状态：`进行中` / `空闲` / `已完成` / `{n} 个子代理运行中` / `等待审批` / `计划待审` / `等待回答`
- 时间：`刚刚` / `{n}分钟` / `{n}小时` / `{n}天` / `{n}个月` / `{n}年` / `{t}前` / `创建于 {time}` / `已复制`
- hover 卡：标题 + 相对时间 + 状态列表 + 点击复制标题

**脚部设置（settings ns，仅触发行）**
- `设置` / `关闭`

---

## 6. 数据映射 + API 缺口

### 6.1 UI 元素 → github.com/shutu-ai/shutu-agent API 字段映射

| UI 元素 | 所需数据 | 来源（github.com/shutu-ai/shutu-agent） | 状态 |
|---|---|---|---|
| 会话行 id / 标题 | `id`、`title`（首条 user 消息，dsh 语义一致） | `GET /api/sessions` | ✅ |
| 空会话占位行（标题「新会话」） | `blank` | `GET /api/sessions` | ✅ |
| 相对时间 | `updated_at` | `GET /api/sessions` | ✅ |
| 排序（最新优先） | `updated_at` 降序 | 客户端排序 | ✅ |
| 当前高亮 + 刷新恢复 | 选中 id | localStorage 持久化 + `POST /api/sessions/{id}/resume` | ✅ |
| 新建会话 | — | `POST /api/sessions` → 置 current | ✅ |
| 切换会话 | — | `POST /api/sessions/{id}/resume` + `GET events` + SSE（P1 已接） | ✅ |
| 运行中状态点（蓝） | `running` | — | ❌ **缺：列表 running 字段**（可尝试用 `GET /api/jobs` 关联 session 推断，见 §6.2-②） |
| 完成未读绿点 | `completed`（运行结束未打开） | — | ❌ **缺：completed 字段**（P2 可不做该点） |
| 等待交互琥珀点（审批/计划/回答） | `pendingInteraction` | — | ❌ **缺**（github.com/shutu-ai/shutu-agent 无审批/计划模式 → P2 不做） |
| 子代理运行计数 | `runningSubagentCount` | `GET /api/subagents` | ⚠️ 可关联推断，P2 可不做 |
| 行菜单·重命名 | — | — | ❌ **缺：rename 端点** |
| 行菜单·归档/删除 | — | — | ❌ **缺：delete/archive 端点** |
| 行菜单·分叉 | — | — | ❌ **缺：fork 端点**（P2 可不做） |
| 拖拽重排（manual 顺序持久化） | — | — | ❌ **缺：排序端点**（P2 不做拖拽） |
| 内容搜索 | 会话标题/内容匹配 | — | ⚠️ **缺：搜索端点**；P2 可先做**前端标题子串过滤**（对齐 dsh 的本地匹配层） |
| 分组（按工作区） | workspace 列表/成员 | — | ❌ **缺：workspaces 端点**；P2 固定「单列表」或单一「未分组」 |
| 列表实时变更（新会话/完成/标题更新） | — | — | ❌ **缺：列表变更推送**；P2 用动作后刷新 + 轮询（如 30s） |
| hover 卡 / 复制标题 | 全为已有字段 | 纯前端 | ✅ |
| 折叠轨 / 宽度 | — | P1 布局 store | ✅ |

### 6.2 缺口清单（按 P2 优先级）

1. **会话级操作端点**（P2 若要做行菜单）：
   - `PATCH /api/sessions/{id}/title`（或 `POST …/rename`）→ 重命名（空会话固定标题的「钉住」语义可省略）。
   - `DELETE /api/sessions/{id}`（或 `POST …/archive`）→ 删除/归档。
   - `POST /api/sessions/{id}/fork` → 分叉（P2 可不做）。
2. **列表状态字段**：`GET /api/sessions` 增加 `running`、`completed`（可选 `pending_interaction`）；或提供全局会话事件推送（SSE/WS）让列表免轮询。
3. **搜索**：`GET /api/sessions?q=…`（标题/内容匹配，分页）。P2 无该端点时可降级为**前端标题子串过滤**，并隐藏「内容搜索不可用」提示之外的离线痕迹。
4. **Workspace 概念**：github.com/shutu-ai/shutu-agent 无工作区；「按工作区分组」「添加工作区」在 P2 隐藏，固定单列表。
5. **排序持久化**：无拖拽重排端点 → P2 仅 `updated_at` 降序。
6. **拖拽**：不做（依赖排序端点）。
7. **列表变更推送**：无 → 轮询刷新（动作后立即 + 定时兜底）。

### 6.3 P2 可实现的最小闭环（建议首版）

- 侧栏壳（品牌行 + 折叠钮 + 新会话 + 浏览区 + 设置触发行）与折叠轨动画、滚动条亲和。
- 会话单列表：`GET /api/sessions` → 按 `updated_at` 降序渲染 32px 行（标题/相对时间/当前高亮/blank 显示「新会话」/运行中蓝点若能由 jobs 推断）；点击行 → resume + 置 current + 刷新列表。
- 新建会话按钮 → `POST /api/sessions` → 置 current + 刷新。
- 空态「暂无会话」、溢出折叠（>5 条「展开其余 {n} 个会话」）、底部渐隐、8px 滚动条。
- 搜索：前端标题子串过滤（无内容搜索端点时隐藏「仅名称匹配」提示）。
- 行菜单：可先只保留 `重命名`（若补端点）或整组隐藏；删除/归档/分叉/拖拽标注待端点。
- 状态点：运行中蓝点（若可由 `GET /api/jobs` 推断 running）+ 其余状态点暂不渲染（与 dsh「无数据不画」一致）。

---

## 7. 附：源码文件清单（研究范围）

已读取（核心）：
- `ui-sidebar/src/index.ts`、`src/client/index.ts`、`src/client/SidebarRoot.tsx`、`src/client/SidebarRoot.module.css`、`src/client/contract/slots.ts`、`src/client/locales.ts`、`README.md`
- `ui-workspace/src/client/index.ts`、`src/client/WorkspaceBrowser.tsx`（含 ProjectRow/SessionTree/FlatList/SearchResults/两个重命名 Modal/删除 Modal）、`src/client/WorkspaceBrowser.module.css`、`src/client/rows/Rows.tsx`、`src/client/rows/Rows.module.css`、`src/client/tree.ts`（deriveGroups/deriveFlat/deriveSearchResults/relativeTime）、`src/client/stores.ts`（groupBy/orderBy 持久化）、`src/client/locales.ts`、`src/client/contract/slots.ts`
- `ui-layout/src/client/AppFrame.tsx`、`columns.ts`、`stores.ts`（折叠语义与 P1 核对）
- `ui-settings-general/src/client/chrome.tsx`、`chrome.module.css`、`SettingsRoot.module.css`（脚部设置触发行外观）、`locales.ts`
- `ui-theme/src/styles/design-platform.css`（sidebar 专用 token 与静态色解析）
- `ui-primitives/src/StateDot.tsx`、`StateDot.module.css`（状态点渲染）
- `packages/client/runtime/src/client/sessions/service.ts`（SessionSummary/displayTitleOf 三级回退）、`manager.ts`（title 投影关键行）
- `packages/session/session-title/src/index.ts`、`normalize.ts`（标题生成规则，供对照）

未读取（不影响 P2 实现）：
- `ui-sidebar/tests/*`、`README.i18n.yaml`、`README.zh.md`、`invariant.ts`
- `ui-workspace/src/client/WorkspacePicker.tsx`（hero 空态选择器，P1 范围）、`src/client/invariant.ts`
- `ui-settings-general/src/client/SettingsRoot.tsx`、`GeneralSection.tsx`、`settings-document-store.ts` 等（设置面板本体，不在 P2）
- `runtime/src/client/sessions/manager.ts` 全文、`lineage.ts`、`projection-store.ts`（数据层细节，P2 按 API 字段映射即可）
- 内容搜索宿主实现（`packages/host/apiproxy/.../session-search.ts`，P2 无对应端点，仅引用其 `SESSION_SEARCH_RESULT_LIMIT=20` 与 hasMore 语义）
