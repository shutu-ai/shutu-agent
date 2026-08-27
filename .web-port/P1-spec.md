# dsh Web 工作台 P1 页移植规格（github.com/jabing/shutu-agent · vanilla JS + Go）

> 目标：参照 dsh web（deepseek-harness `packages/client/*`）把 github.com/jabing/shutu-agent 的主工作台页面做到「像 dsh 一样」。
> 本规格只研究 dsh 源码，可照此用零依赖 vanilla JS + CSS 写出等价页面。**不修改 dsh 源码**。
>
> 源码基线：`D:\dev-projects\Agent\deepseek-harness\packages\client\`
> 覆盖包：`ui-layout`（三栏壳）、`ui-conversation`（会话页）、`ui-tool`（工具卡片）、`ui-theme`（token）。

---

## 0. 结论摘要（一句话版）

- **页面 = 三栏 CSS Grid 壳**（sidebar | 对话 | details），默认 `280px | 1fr | 0px(关)`，窄于 1024px 侧栏自动收成 56px 图标轨。
- **对话列是自滚动容器**：顶部面包屑+页签，中间消息流（748px 居中列、16px 行距），底部 sticky 输入胶囊卡（宽 780px 上限、r22、上渐变遮罩），输入卡上方可叠 Todo/队列等 dock 卡。
- **消息流每行是一个“节点”**：user 右对齐气泡、assistant 全宽 markdown、Think 思维链折叠行、工具行（可展开的 IN/OUT 或终端/diff/read/search/web 卡片）、错误行、运行中 shimmer 行。
- **主题**：`body[data-ds-dark-theme]` 切换深/浅，核心是 3 组 token 表（静态色、语义别名、字体/阴影）。
- **API 缺口很大**：现有个体 `{seq,type,time,summary,reasoning?,tool_name?,tool_output?}` 事件投影 + 5 个 SSE 帧类型只够画「用户气泡 + assistant 文本 + 一个简化工具行 + 错误行」；工具参数/运行态/callId、上下文计量、任务、队列、审批、分页、详情面板所需字段全部缺失（详见 §6）。

---

## 1. 整体布局（三栏壳 AppFrame）

源码：`ui-layout/src/client/{AppFrame.tsx, AppFrame.module.css, columns.ts, stores.ts}`。

### 1.1 几何常量（columns.ts，契约冻结值）

| 常量 | 值 | 含义 |
|---|---|---|
| `SIDEBAR_DEFAULT` | 280 px | 侧栏宽度偏好（未拖过） |
| `SIDEBAR_MIN` / `SIDEBAR_MAX` | 264 / 420 px | 侧栏拖拽钳制范围 |
| `SIDEBAR_COLLAPSED` | 56 px | 侧栏折叠后的图标轨宽度 |
| `SIDEBAR_AUTO_COLLAPSE` | 1024 px | 视口低于此值侧栏自动折叠 |
| `DETAILS_DEFAULT` | 360 px | 详情栏宽度偏好（默认关闭=0） |
| `DETAILS_MIN` / `DETAILS_MAX` | 300 / 520 px | 详情栏拖拽钳制范围 |
| `CENTER_MIN` | 640 px | 中栏地板值（仅最终兜底可低于） |

### 1.2 网格 CSS 结构（建议直接照抄）

```css
.frame {
  position: relative;            /* 拖柄绝对定位的锚 */
  display: grid;
  grid-template-rows: 100%;
  grid-template-columns: 280px minmax(0, 1fr) 0px;  /* JS 写侧栏px / 中栏 / 详情px */
  height: 100%;
  overflow: hidden;
  background: var(--dsw-alias-bg-base);
  transition: grid-template-columns 0.3s cubic-bezier(0.4, 0, 0.2, 1); /* --ds-transition-duration-slow */
}
.frame[data-dragging] { transition: none; }   /* 拖拽时禁用缓动，避免列边脱手 */
@media (prefers-reduced-motion: reduce) { .frame { transition: none; } }

.frame > .sidebarCol { min-width: 0; overflow: hidden; background: var(--dsw-specific-sidebar-fill);
  border-right: 1px solid var(--dsw-alias-border-l1); }
.frame > .centerCol  { min-width: 0; display: flex; flex-direction: column; overflow: hidden; }
.frame > .detailsCol { min-width: 0; overflow: hidden; border-left: 1px solid var(--dsw-alias-border-l2); }
.frame[data-details-collapsed] > .detailsCol { border-left: none; }  /* 0 宽时去掉 1px 接缝 */
```

- 详情栏即使“关闭”（0 宽）也**保持挂载**（`data-details-collapsed`），只是视觉隐藏；侧栏折叠则保留 56px 轨（有边框）。
- 列宽偏好存在 JS 状态：`{ sidebar:280, details:0, narrow:false, narrowExpanded:false }`。拖拽写入时钳制到各自 [MIN,MAX]，且**绝不跨过 开/关 线**；打开/关闭显式写 默认值/0。
- 每帧解析（`computeColumns`，纯函数，无滞回）：
  1. `sidebar + details + CENTER_MIN ≤ viewport` → 全按偏好；
  2. 否则把 details 压到 `max(DETAILS_MIN, viewport - sidebar - CENTER_MIN)`；
  3. 仍放不下 → details 自动归 0（偏好不被改写，拉宽窗口自动恢复），中栏吸收剩余。
  - **侧栏永不妥协**；中栏 `minmax(0,1fr)` 兜底可低于 640。

### 1.3 拖拽调整

- 拖柄：`position:absolute; top:0; bottom:0; width:8px; margin-left:-4px; cursor:col-resize; z-index:2; touch-action:none;` 内联 `left` 定位到列边界。
- 侧栏柄**无可见装饰**；详情柄在正中挂一个 12×32、r10 的浮钮 pill（`::after`，默认透明，悬停该列/柄或拖拽时显现，hover 用 `--dsw-alias-button-floating-hover` + `--dsw-alias-border-l3`）。
- 手势：`pointerdown → setPointerCapture` → `pointermove` 用 `requestAnimationFrame` 节流计算 `dx = clientX - 起点`，`pointerup` 释放。拖动基数是**拖起瞬间的渲染宽度**（拖拽过程中帧内钳制不跳回偏好）。
- 侧栏 `dx` 直接加；详情 `dx` **取负**（右边界）。拖拽期间给 `.frame[data-dragging]` 关过渡。

### 1.4 窄屏自动折叠

- `ResizeObserver` 观测 `.frame` 自身宽度（rAF 节流），`width < 1024` → `narrow=true`。
- narrow 下侧栏强制折叠（无论偏好），但 `.narrowExpanded` 覆盖可手动重新展开（此时中栏被挤压），宽度偏好不被改写；跨过断点任一侧 `narrowExpanded` 重置为 false。
- DOM 数据属性：`data-sidebar-collapsed`、`data-details-collapsed`、`data-dragging`（CSS 与后续逻辑靠它们）。

### 1.5 DOM 树（AppFrame 渲染结果）

```
div.frame[style="grid-template-columns: 280px minmax(0,1fr) 360px"][data-*]
  div.sidebarCol          → 侧栏槽位（本规格不实现，仅占位）
  div.centerCol           → 会话页（§2）
  div.detailsCol          → 详情面板（§2.5）
  div.overlayLayer[data-shell-overlay]   /* 绝对 inset:0, z-20, pointer-events:none，子级 auto */
  [未折叠] div.handle[data-side=sidebar]  (left=侧栏宽)
  [details>0] div.handle[data-side=details] (left=viewport-details宽)
```

---

## 2. 每个 UI 区域的 DOM 层级与外观

### 2.1 会话消息流（ChatView）

源码：`ui-conversation/src/client/chat/ChatView.tsx + .module.css`、`MessageItem.tsx`、`AssistantMarkdown.tsx`、`ReasoningRow.tsx`、`AssistantNodeView.tsx`、`ChatNodeSeat.tsx`。

```
div.root
  div.scroll                         /* 作为宿主滚容器的流内容（overflow visible）；独立时自己滚动 */
    div.column[data-chat-flow]       /* max-width:748(--dsh-chat-content-width); margin:0 auto; flex col; gap:16 */
      [加载中]  div.hint      "载入历史…"
      [加载失败] div.openError "历史加载失败：{message}（{code}）"
      [hasMore]  div.older > button "加载更早"（loading 时禁用）
      -- 每个节点一行 --
      div.flowItem[data-chat-anchor-key][data-chat-flow-kind]  → 按 kind 分发：
        user / steering    → UserMessageNodeView（§2.1.1）
        assistant-step     → AssistantMarkdown（§2.1.2 + §2.1.3）
        tool-call          → ToolCallTree → ToolRow（§2.1.4）
        turn-error         → 错误行（§2.1.5）
        turn-max-tokens    → 截断提示行
        compaction         → 压缩标记行
        unknown            → JSON 块兜底
      [running]  div.turnStatus role=status aria-live=polite  "Deep diving..."（源码硬编码英文）
                   + 运行 ≥15s 后 span.turnStatusClock "· 用时 45秒"（shimmer 蓝字动画）
      [pendingSteering]   用户预排队气泡（同 user 外观 + pending 标记）
    [非底部] div.toBottomSlot > button.toBottom（34px 圆钮，下箭头，aria-label 回到底部）
```

- 流列 16px 行距；`div.flowItem:empty{display:none}`（渲染器可弃行）。
- 外层 `.scroll` 内边距 `16px calc(16 + 16)`（clearance+16），列本身 max-width 居中。
- 运行中 turn 底部放一条 **TurnStatus**：26px 高、渐变蓝 shimmer 文字（`--dsw-static-deepseek-500→200→500`，`background-clip:text`，1.8s 循环，reduced-motion 停）。

#### 2.1.1 user 气泡（右对齐）

```
div.userRow[data-pending-steering?][data-time-hover-root]
  div.userStack            /* align:flex-end; gap:8; max-width:min(525px,82%) */
    (图片组，可选)
    div.bubble             /* bg:--dsw-specific-bubble; r22; padding:10px 16px; font:16/24; 色:label-primary */
      · 文本（支持 @/ 引用 chip 高亮：business-blue，带引用图标）
      · 非文本块 → JSON 块
    (引用会话摘要行，可选： "引用会话 · {labels}")
  div.actions → MessageIconActions（§2.1.6）
```

#### 2.1.2 assistant 气泡 / markdown（全宽）

```
div.root[data-streaming]     /* font:16/28; 色:label-primary */
  div.body                   /* flex col; gap:16 */
    MarkdownText            /* 文本块，markdown 渲染 */
    ReasoningRow            /* 思维链块，§2.1.3 */
    (连续 image 块合并为画廊)
    [interrupted] span.stopped "已停止"（小标签：11/18, r6, interactive-bg-hover 底）
  (若为已完成轮次的收尾 → 下方接 IconActions，§2.1.6，margin-top:16)
```

- 文本为**流式追加**：`data-streaming` 时边滚动边拼；中断轮次显示「已停止」标记。
- markdown 字号体系：正文 16/28、h1 24/34(700)、h2 22/32、h3 20/30、h4 16/28(600)、行内 code 14/22 等（token 见 §4）。

#### 2.1.3 思维链折叠块（ReasoningRow / “Think”）

```
div.root[data-variant=think][data-state=running|ok]
  [running] 隐藏的可读状态文本（aria）
  button/行（DisclosureRow，整行可点展开）
    span.leading  → Think 图标（IconThinkOutline14，14px）
    span.title    → "Think"（源码硬编码英文标题）
    chevron（右）
    [折叠时] span.sep(2x2 点) + span.summary   /* 14/24 tertiary，ellipsis；
        running 时显示“最新一行”且 scrollLeft 自动滚到末尾（跟随输出） */
  [展开时] div.thinkBody    /* padding:4 0 4 22; 14/24 tertiary; pre-wrap; break-word；全文 */
  [running] .row::after 300px 渐变扫光动画（2.6s ease-out 无限；reduced-motion 停）
```

- 折叠态一行放不下时省略；展开态缩进 22px 灰字显示完整推理。
- 无“打开/折叠”持久化，纯本地开关状态。

#### 2.1.4 工具卡片（ToolRow + 变体卡片）

源码：`ui-tool/src/client/tool/components/ToolRow.tsx + .module.css`、`ToolCallTree.tsx`、`models/*`。

外层工具树（ToolCallTree）：一个根调用 + 递归子调用（run_code 内嵌）：

```
div.callRow[data-chat-anchor-key="call:{callId}"][data-chat-call-id][data-selected?]
  → 工具行（按 toolName 分发的原子视图；未注册走 GenericToolCard）
  [有子调用] div.subCalls   /* margin:4 0 2 22; padding-left:8; border-left:1px border-l2; gap:4 */
    （递归工具行…）
```

工具行（GenericToolCard → ToolRow）：

```
div.root[data-variant=search|read|bash|write|edit|code|others][data-tool={name}][data-state=running|ok|error|stopped]
  [visuallyHidden] 状态文本（运行中/失败/已停止，aria）
  div.row   /* 24px 单行；整行点击切换展开 */
    div.leading 16px      /* 运行中/OK：变体图标；error→红点(StateDot)；stopped→琥珀点 */
    span.title            /* 14/24；变体标题（见下） */
    [折叠] span.sep(2x2点) + 摘要
      · 普通：span.summary 14/24 tertiary，ellipsis
      · 单文件工具(read/write/edit)：button.fileLink 下划线链接（hover 变实，点击打开文件）
      · error：摘要=失败首行，error 色
      · (可选) summarySuffix（如 todo 并行计数）
  [展开] div.bodyWrap      /* 与 .row 同级，点内部不触发折叠 */
    -- 至多一种卡片，替换文本区 --
    TerminalBlock 终端卡   /* 命令/工作目录/输出/退出码/信号；输出区内滚，上限224px */
    DiffBlock 差异卡       /* hunks{path,oldText,newText}；聊天内 CHAT_DIFF_MAX_LINES=8 */
    ReadBlock 读取卡       /* 行号窗口；CHAT_READ_MAX_LINES=8 */
    SearchBlock 搜索卡     /* matches(分组)或 paths；CHAT_SEARCH_MAX_LINES=8；截断时附 recovery 定位文本 */
    WebBlock 网页卡        /* search→answer+sources；fetch→url+statusCode */
    -- 通用 IN/OUT 卡（无卡片意图时）--
    div.ioCard r12 code-block 底：div.ioSection[IN] gutter + 文本（max-height:150 内滚）
                                      + ioDivider + div.ioSection[OUT]（error 时红字）
    -- code 变体：CodeBlock（max-height:260 内滚，shiki TS 高亮）--
    [有 inspect] button.inspectButton 胶囊 "Inspect"（悬停浮现，跳转详情/轨迹）
  [running] .row::after 300px 渐变扫光（同 Think）
```

- 变体标题（**英文设计字面量**，非翻译）：Search / Read / Bash / Write / Edit / Code / Tool call；额外：`pwsh→Pwsh`、`cordis_run→Run Cordis Plugin`、`cordis_stop→Stop Cordis Plugin`、`cordis_undefine→Remove Cordis Plugin`、`cordis_*_inspect→Inspect`。
- 工具名→变体分类（`TOOL_VARIANTS`）：`bash/pwsh→bash`、`read/web_fetch→read`、`web_search/grep/glob→search`、`write→write`、`edit→edit`、`run_code→code`、`cordis_run/stop/undefine→others`、未知→`others`。
- 摘要生成：`others` 且无自定义标题 → `"{toolName} · {摘要}"`；其余从参数里按变体偏好取 `description/command/path/query/url` 等首个字符串。
- `data-tool^=cordis_` 时 leading/title/sep 着 business-blue 强调。

#### 2.1.5 错误行 / 提示行

```
div.turnErrorRow role=status   /* grid: 10px 1fr auto; gap8; 13/20 */
  span.turnErrorDot（红点 StateDot error）
  div.turnErrorCopy
    span.turnErrorTitle "本轮运行失败"（error 色 600） + span.turnErrorMessage（secondary）
  [可选] code.turnErrorCode
（输出 token 上限提示同构，标题琥珀色 "已达到输出 token 上限" + 提示文案）
```

#### 2.1.6 图标动作（MessageIconActions）

```
div.actions   /* flex; gap:10; height:28 */
  [clock=start] span.timeStart（user：时间在前）→ 复制钮 → [branch 钮]
  [clock=end]  （assistant：复制钮在前 → 时间在后）
```

- 时间标签：同日 `HH:mm`；今年非同日 `{m}月{d}日 HH:mm`；往年 `{y}年{m}月{d}日 HH:mm`（`data-time-hover-root` 内 hover/focus 淡入，桌面默认隐藏）。
- 复制：写剪贴板成功 → 换对勾图标 1s。
- 分支钮：仅已完成轮次收尾可用，否则 `data-unavailable`（0.4 透明 + tooltip 解释）。
- 时间后可追加 `· 用时 {duration}`、`· 首 token {s}秒`、`· {tps} tok/s`。

### 2.2 输入栏（InputBar）

源码：`skeleton/InputBar.tsx + .module.css`。

```
div.root[hero?]                /* padding:0 16(clearance) 8px; hero 无底 pad */
  [info 通知] div.notice role=status（12/18，interactive-bg-hover 底，r8）
  div.card[data-composer-card] /* 位置相对；flex col; gap:12; padding-top:10;
                                  width:100%; max-width:780(--dsh-composer-card-max-width);
                                  border:1px solid --dsw-alias-border-l2-darkmode-thin; r22;
                                  bg:--dsw-specific-input-major; box-shadow:--dsw-shadow-lv2; font:16/24;
                                  --dsh-scrollbar-thumb:-l2 对（内滚区） */
    div.overlayAnchor（悬浮层锚，height:0）
    div.accessory（附件区）
    (附件 rail 槽位：拖放添加图片)
    div.scroll[data-input-scroll]  /* max-height:336(--dsh-composer-text-max-height); overflow-y:auto */
      div.grow  /* position:relative */
        div.backdrop[data-input-backdrop]  /* 绝对 inset:0；着色文字层（token/chip/提示） */
        textarea.input[data-phase=inert|...] /* 绝对；透明文字(caret business-blue)；padding:4 12 0 16；
                                                white-space:pre-wrap; rows=2 */
        div.mirror[data-input-mirror]       /* 隐藏层，渲染 draft+'\n' → 高度权威（自动增高核心） */
    div.row   /* flex wrap space-between; gap:12; padding:2 8 6; container-type:inline-size */
      div.tools(gap:16)
        button.add（28px 圆钮，+ 图标，命令菜单）
        div.modes → 权限/计划 chip
        leftItems
      div.trailing(margin-left:auto; gap:12)
        rightItems → 模型选择槽 → ContextMeter(§2.6) → [Stop 钮(仅可续子会话运行中)] → button.primary
    button.primary  /* 34px 圆钮；bg:--dsw-alias-button-info-fill(#679EFE 暗/#4176E6 亮)；白箭头；
                       空态/禁用 → opacity .4；运行中主会话→变成 Stop（方块图标） */
  footer → StatsLine(§2.10)
```

- **自动增高**：`rows=2` + 隐藏 mirror 层按实际换行决定高度；超过 336px（14 行）后 `.scroll` 内部滚动。
- **Enter 发送**：`Enter`（非组合态、非 Shift、非 machineBusy、非锁定）→ 提交；`Shift+Enter` 换行；IME 组合中 Enter 不发送（`composingRef` / `keyCode 229`）；按住 Enter 连发有 `e.repeat` 防抖。
- **快捷键**：`↑/↓`（机器仲裁）、`Escape`（关弹层）、`Ctrl/Cmd+Z/Y`（撤销/重做，走机器 undo 栈）、`Ctrl/Cmd+Enter`（空草稿时把排队消息全部插话发送）。
- **禁用态**：`disabled = removed || inert(无会话) || !live || blocked || parentOffline`。禁用时同一棵 DOM（textarea `disabled`），背景层文字变 tertiary；`.cardWorkspaceTrigger` 态整卡变成「选择工作区」虚线描边触发器（dash ring，hover 变蓝）。
- 占位文案优先级：workspace > parentOffline > unavailable > steerQueue > plan > default（见 §5）。
- 草稿在 hero ↔ docked 切换时**同一 textarea 存活**（不重建 DOM），仅变 variant。
- 轮播：`.scroll` 滚到头时把 `wheel` 转发给会话滚容器（`data-conversation-scroll`），长草稿不吞滚轮。

### 2.3 空态引导（Hero / EmptyHero + HeroShell）

无会话（cold start）或空会话（blank）时的居中画面：

```
div.composerStack.composerHero   /* 列内 flex 居中；padding-bottom:32；width:min(780+32, 100%) */
  svg.heroGlow   /* 绝对定位在输入卡上方 ~92px；1051x468 视窗；椭圆 rx425 ry134 填 #6187D8 8% + GaussianBlur 50；
                    z-index:-1；宽 = 100%*1051/776（贴卡缩放） */
  div.HeroShell.root
    div.headline   /* 26/32 500；grid 34px auto auto; gap10; 居中 */
      span.fishHitbox > FishLogo(34x25) + span.headlineText "智行未至之境" + span.previewBadge "预览版"
      （previewBadge：mono 12/18 500, r24, 1px 边, business-tertiary 底, bluish 文字）
    div.body（占位，输入卡在此之下）
  div.heroWorkspaceRow   /* padding-left:20; margin-top:4 */
    button.workspace     /* 文件夹图标(开/闭)+label+chevron；r16; 13/20 500；hover 淡底；
                           未选 → 占位 "选择工作区" + 闭文件夹图标 */
      (展开菜单由宿主弹层渲染，规格外)
```

- Hero 触发条件：`sessionId===undefined`，或 `composerPhase==='blank' && (openState==='open' || summaryBlank)`。
- 会话仍在回放（loading+blank，且列表未证明空）→ `settling` 相位：composer seat 保留但 `visibility:hidden`（不闪错画面）。

### 2.4 顶部 / 面包屑 / 页签（ConversationSessionHeader）

```
header.header   /* padding:12 28 0 20; 底部 1px --dsw-alias-border-l2 线 */
  div.titleRow  /* min-height:32 */
    div.titleCluster
      nav.crumbs[aria-label=会话层级]   /* gap:4; 面包屑链；祖先会话（subagent 链） */
        span.crumbSeg > span.crumbSep "/" + button.crumb{crumbCurrent?}
        /* 14/20；非当前 crumb 14/20 tertiary，r12 hover 底；当前 500 primary，disabled；
           祖先显示 displayTitle；无祖先 → 直接显示 sessionId */
      div.headerActions（右侧动作槽）
    div.headerUtilities（更多工具，可空隐藏）
  [多个视图] div.tabs role=tablist   /* gap:36; padding-left:8; margin-top:4 */
    button.tab role=tab  /* 13/16 500; 当前 → business-blue + 底部 2px 蓝色条(::after) */
```

- 空会话（blank + blank composerPhase）时整个 header 隐藏（`headerHidden`），不占列高。

### 2.5 详情面板（DetailsPanel，第三栏）

```
div.root   /* flex col; height:100%; border-left:1px border-l2; bg:bg-base */
  div.header  /* padding:14 12 12; border-bottom:1px border-l2 */
    div.title  "详情" 或 选中调用名（14/20 500，ellipsis）
    button.close 28px 圆钮 ✕（aria-label 关闭详情）
  div.body   /* flex:1; padding:12 16; overflow-y:auto */
    [未选中] div.empty "点击消息流中的工具行查看详情"
    [调用不在窗口] div.empty "该调用不在当前窗口内"
    [选中且找到] 
      section → div.sectionLabel "输入" + CodeBlock（pretty JSON args）
      section → div.sectionLabel "输出" + 工具视图（终端/read/diff/search/web 卡 或 <pre> 原文，error 红）
    [运行中无结果] div.empty "运行中…"
```

### 2.6 上下文计量（ContextMeter，输入卡右下、发送钮左侧）

```
span.root（position:relative; inline-flex）
  button.trigger 28px 圆钮（tooltip/aria "上下文已用 {percent}%"）
    svg 14x14 → circle.track（stroke border-l3, 2px）+ circle.fill（stroke label-tertiary, 2px, round cap,
                 strokeDasharray = 2π·5.5·percent/100，rotate(-90)）
  [开] div.panel role=dialog  /* absolute; bottom:100%+8; right:0; width:264; r12; 1px border-inverted;
                                 bg:--dsw-specific-menu; shadow-lv3; 12/20 */
    div.header → span.headline（前后缀） + span.percent("45%") + span.figures("~12.2K / 128K"，tabular-nums)
    div.bar   /* height:4; r999; gap1; 底 interactive-bg-hover */
      span.segment{colorSystem|colorTools|colorMessages} width:%（按 breakdown 比例分色）
        system=bluish-400、tools=rgb(167,139,250)、messages=blue-450(#4D93F8)
    dl.rows   /* 系统提示词 / 工具 / 对话消息 → swatch + 计数(tabular) */
```

### 2.7 审批面板（ApprovalPanel，输入槽位被接管）

```
div.root   /* padding:8 (clearance+16) 12; 与输入卡同列 */
  div.card  /* width:100%; max-width:748(--dsh-chat-content-width); r20; 1px --dsw-alias-state-warn-secondary;
               bg:--dsw-specific-input-major; shadow-lv2 */
    div.strip   /* 琥珀底（warn-tertiary）10px 16px；8px 圆点 + "等待审批"（warn-primary 13/18） */
    div.body[data-approval-scroll][tabIndex=0] role=group  /* max-height:336; overflow-y:auto; padding:12 16 0 */
      div.headline  /* 15/24 500 primary：审批理由（模型 justification） */
      [有命令] div.command  /* mono 13/20 tertiary；word-break:break-all */
    div.actionRow   /* justify-end; gap:8; padding:14 16 */
      button 描边 "拒绝"（hover 危险底红字）
      button 主色 "允许一次"
```

- 一次性：点击后按钮禁用，面板在批准结果帧落地后消失。

### 2.8 任务面板（TodoPanel，输入卡上方 dock）

```
section.root[aria-label=任务]  /* dock 卡：width = (卡宽-4·inset)；r12；1px border-l1；bg:--dsw-specific-tip */
  div.body  /* gap:8; padding:6 12 */
    button.header  /* 宽 100%；gap:10；aria-expanded */
      span.lead（清单图标，tertiary）+ span.title "任务"（13/24 500）+ span.progress
        （"3 已完成 · 2 进行中 · 1 待处理"，用  ·  连接；零计数段省略） + span.chevron（上/下）
    [展开] ul.list  /* max-height:180; overflow-y:auto; gap:8 */
      li.item[data-status]  /* 13/20 secondary */
        span.glyph 16px：
          completed → 绿色实心对勾环（success-primary）
          in_progress → business-blue 渐变环，CSS 1s 旋转
          pending → 灰虚线环（label-caption，dash 2.4 2.4）
        span.content（ellipsis）
```

### 2.9 队列 dock（QueueDock，输入卡上方 dock）

```
div.dock[data-queue-dock]  /* width=(卡宽-2·inset)；margin:0 auto calc(-gap-3px)（上贴输入卡方底）;
                              padding:0 8(inset) */
  div.panel  /* r12 仅上圆角(r12 12 0 0)；padding:2 0；bg:--dsw-specific-tip；::after 1px 边框但去下边 */
    [>1 条] button.header   /* height:36; gap:10; padding:4 12 */
      span.lead(队列图标) + span.count "{n} 条排队消息"（13/500） + span.chevron
    ul.list[hidden=折叠&&>1]  /* max-height:180; overflow-y:auto */
      li.row  /* height:36; gap:10; padding:4 5 4 12; 行间 inset 0 1px 0 border-l1 */
        [单条] span.lead(队列图标)
        [编辑中] input.editor（r6, 1px border-l2, 聚焦 business-blue） ↔ span.preview（13/20 dimmed ellipsis）
        div.actions（gap:10）
          编辑态：保存 ✓ / 取消 ✕
          普通态：编辑 ✎ / 删除 🗑 / 插话发送 ➤（仅 running 可用；非文本不可编辑）
```

### 2.10 统计行（StatsLine，输入卡下沿 footer）

```
div.root  /* display:block; text-align:center; max-width:748; padding:4 (clearance+16) 0;
             12/20 tertiary; nowrap ellipsis；超长 hover tooltip 全文 */
  span "N 轮 · M 步"  |  span "LLM 45.2s · 工具调用 1m2s"  |  span "首 token 平均 1.2s · 34.5 tok/s"  |  span "缓存命中 62%" · "输入 12.2K tok · 输出 517 tok"
```

- 组间用 ` | ` 分隔（`--dsw-alias-separator-primary`），无数据整组消失。

---

## 3. 关键交互

### 3.1 消息流滚动（底部吸附 + 历史加载）

- 宿主滚容器是 `[data-conversation-scroll]`（ConversationRoot 的 `.scrollBody`）；ChatView 在宿主内是普通流内容，自身 `.scroll` 不滚动。
- **底部吸附**：`FOLLOW_THRESHOLD=24px`。用户滚到距底 ≤24px → 视为“钉底”；新流内容（assistant 流、工具披露、新 user 消息、steering 气泡）到达且钉底 → 自动 `scrollTop=scrollHeight` 吸底。用户上滚离开底部 → 停止跟随，右下角浮现 `toBottom` 圆钮，点击回底。
- 跟随由 `ResizeObserver` 观测消息列 + composer seat 高度（`--dsh-composer-height`）驱动，只在钉底时写滚动。
- **历史加载**：`hasMore` 时顶部有「加载更早」钮；点击前先记录当前可视锚点行（`data-chat-anchor-key` + 相对滚动位），加载完成后把同一行平移回原位置（prepend 保位）。`loadingOlder` 期间按钮禁用。
- 视图切换保留滚动位：离开时存 `{anchorKey, anchorTop, scrollTop}`，回来恢复；钉底则存 null（回来继续吸底）。

### 3.2 工具卡片展开/折叠

- 整行（`.row`）点击/Enter/Space 切换；有 body/output/任一卡片才可展开（`expandable`）。
- 折叠态恒单行（摘要 FILL 省略）；展开体在 `.bodyWrap`（点击其内部不触发折叠）。
- 详情联动：选中某个调用（`selection.callId`）→ 该行 `data-selected`，第三栏详情显示其输入/输出；`.callRow` 无选中描边（与 Think 行一致）。
- Inspect 胶囊悬停浮现，点击跳到该调用的轨迹/详情（本规格简化为打开详情面板）。

### 3.3 思维链展开/折叠

- 整行点击/Enter/Space 切换（`DisclosureRow` 的 `expandOnRowClick`）；折叠态显示摘要（首行或运行最新行），展开显示全文（22px 缩进灰字）。
- 运行中新增内容只在**流末尾行**跟随（summary 自动滚到最右），不强制展开。

### 3.4 输入禁用与恢复

- 禁用原因：会话已删 / 无会话（inert，卡片变“选工作区”触发器）/ 无机器 / 有 block / 父会话离线（可续子会话）。
- 阻断（block）时禁用但不隐藏（保留模型选择以解除）；提交/仲裁中 textarea `readOnly`（草稿可见）。
- 恢复：解锁 effect 会把焦点还给 textarea（`preventScroll`），并自行 reveal 光标。
- 发送后草稿清空、焦点保留；失败（promptError）toast 提示，草稿保留可重发。

### 3.5 队列 dock

- 一条直接平铺；多条折叠成 `{n} 条排队消息` 头（点击展开/收起）；空队列整卡消失。
- 每行操作：编辑（进入行内 input，Enter 保存 / Esc 取消）、删除、插话发送（仅 running；空草稿时 `Ctrl/Cmd+Enter` 可全队列插话）。
- 操作失败 → 会话通知（error）并保持行内状态；busy 期间按钮禁用。

### 3.6 其他

- 主题切换：`html.style.colorScheme` + `body[data-ds-dark-theme]` 属性 + `meta[name=theme-color]` 跟随背景色。
- 窄屏：<1024px 侧栏自动收轨；输入卡/消息列始终同宽轴（消息列 = 卡宽 - 32）。
- 消息时间 hover 淡入、复制成功换勾、重试 shimmer、审批一次性等见各自小节。

---

## 4. CSS token 值表（暗色默认）+ 浅色关键差异

主题机制：默认 **暗色**。`body[data-ds-dark-theme]` 切换整套 token；`html { color-scheme: dark|light }` 控制原生 UI。下表为**暗色（默认）已解析值**（源码 token 链全部解开；注释给用途）。定义放 `:root`/`body` 即可（vanilla CSS 直接写值，不必保留引用链）。

### 4.1 语义别名 token（设计平台暗色）

| token | 暗色值 | 用途 |
|---|---|---|
| `--dsw-alias-bg-base` | `#151517` | 页面/列底色 |
| `--dsw-alias-bg-layer-1/2/3` | `#232324 / #2C2C2E / #353638` | 层级表面（上浮） |
| `--dsw-alias-bg-overlay` | `#61666B` | 遮罩/浮层底 |
| `--dsw-alias-border-l1` | `rgba(255,255,255,0.06)` | 最弱描边（卡片细线） |
| `--dsw-alias-border-l2-darkmode-thin` | `rgba(255,255,255,0.06)` | 输入卡描边（比按钮弱一档） |
| `--dsw-alias-border-l2` | `rgba(255,255,255,0.12)` | 常规分隔线 |
| `--dsw-alias-border-l3` | `rgba(255,255,255,0.16)` | 稍强描边 |
| `--dsw-alias-border-l4` | `rgba(255,255,255,0.2)` | 最强描边 |
| `--dsw-alias-border-inverted` | `rgba(255,255,255,0.06)` | 深色表面对比描边 |
| `--dsw-alias-label-primary` | `#F9FAFB` | 主文字/图标 |
| `--dsw-alias-label-secondary` | `#CFD3D6` | 次文字 |
| `--dsw-alias-label-tertiary` | `#ADB2B8` | 弱文字（摘要/副信息） |
| `--dsw-alias-label-caption` | `#81858C` | 说明/分隔/占位 |
| `--dsw-alias-label-dimmed` | `#43454A` | 禁用的弱化文字 |
| `--dsw-alias-label-primary-dimmed` | `#EBEEF2` | 主文字弱化（队列预览等） |
| `--dsw-alias-state-business-primary` | `#679EFE` | 业务蓝（选中/活动/焦点/发送钮） |
| `--dsw-alias-state-business-tertiary` | `#34415B` | 业务蓝弱底 |
| `--dsw-alias-state-error-primary` | `#F25A5A` | 错误文字/红点 |
| `--dsw-alias-state-error-secondary` | `#F25A5A` | 错误次 |
| `--dsw-alias-state-success-primary` | `#22C55E` | 成功/完成对勾 |
| `--dsw-alias-state-success-secondary` | `#4ED17E` | 成功次 |
| `--dsw-alias-state-success-tertiary` | `#233C2C` | 成功弱底 |
| `--dsw-alias-state-warn-primary` | `#F59E0B` | 警告文字/圆点 |
| `--dsw-alias-state-warn-secondary` | `#F7AD31` | 警告描边 |
| `--dsw-alias-state-warn-tertiary` | `#27241F` | 警告弱底（审批条带） |
| `--dsw-alias-state-warn-label` | `#DD8629` | 警告标签文字 |
| `--dsw-alias-interactive-bg-hover` | `rgba(255,255,255,0.08)` | 悬停淡底 |
| `--dsw-alias-interactive-bg-hover-solid` | `#353638` | 悬停实底（按钮） |
| `--dsw-alias-interactive-bg-active` | `rgba(255,255,255,0.14)` | 按下 |
| `--dsw-alias-interactive-bg-hover-danger` | `rgba(242,90,90,0.15)` | 危险悬停底 |
| `--dsw-alias-button-info-fill` | `#679EFE` | 发送主圆钮填充 |
| `--dsw-alias-button-info-hover` | `#4176E6` | 发送钮 hover |
| `--dsw-alias-button-floating-fill` | `#2C2C2E` | 浮动钮（回底）底 |
| `--dsw-alias-button-floating-hover` | `#353638` | 浮动钮 hover |
| `--dsw-alias-button-primary-fill` | `#F9FAFB` | 主按钮（品牌=墨色） |
| `--dsw-alias-brand-primary` | `#F9FAFB` | 品牌色（此主题=墨） |
| `--dsw-alias-markdown-code-block` | `#1B1B1C` | 代码块/IN-OUT 卡底 |
| `--dsw-alias-markdown-code-block-banner` | `#2C2C2E` | 代码块顶栏 |
| `--dsw-alias-markdown-inline-code` | `#2C2C2E` | 行内代码底 |
| `--dsw-alias-markdown-code-segment-selected` | `#353638` | 代码段选中 |
| `--dsw-alias-markdown-code-segment-unselected` | `#1B1B1C` | 代码段未选 |
| `--dsw-alias-scrollbar-bg-l1` | `#3C3C3D` | 滚动条拇指（基础面） |
| `--dsw-alias-scrollbar-bg-l2` | `#545557` | 滚动条拇指（浮层面） |
| `--dsw-alias-scrollbar-hover-l1` | `#545557` | 拇指 hover |
| `--dsw-alias-scrollbar-hover-l2` | `#65676B` | 拇指 hover（浮层） |
| `--dsw-specific-bubble` | `#2C2C2E` | 用户气泡底 |
| `--dsw-specific-bubble-highlight` | `#43454A` | 气泡高亮 |
| `--dsw-specific-input-major` | `#2C2C2E` | 输入卡/审批卡底 |
| `--dsw-specific-menu` | `#353638` | 菜单/浮层面 |
| `--dsw-specific-selector` | `#353638` | 选择器 chip 底 |
| `--dsw-specific-sidebar-fill` | `#1B1B1C` | 侧栏底 |
| `--dsw-specific-tip` | `#353638` | dock/tip 卡底 |
| `--dsw-alias-tooltip-bg` | `#43454A` | tooltip 底 |
| `--dsw-alias-toast-bg` | `#43454A` | toast 底 |
| `--dsw-shadow-lv1` | `0 2px 4px 0 rgba(0,0,0,0.05)` | 浅阴影 |
| `--dsw-shadow-lv2` | `0 4px 12px 0 rgba(0,0,0,0.02), 0 2px 8px 0 rgba(0,0,0,0.04)` | 输入卡/浮钮 |
| `--dsw-shadow-lv3` | `0 0 1px 0 rgba(0,0,0,0.2), 0 0 4px 0 rgba(0,0,0,0.02), 0 12px 32px 0 rgba(0,0,0,0.08)` | 弹层/菜单 |

### 4.2 字体 token（暗色同值，无深浅差异）

| token | 值 | 用途 |
|---|---|---|
| `--dsw-font-family` | `-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Helvetica Neue', Helvetica, Arial, sans-serif` | 正文字体栈 |
| `--ds-font-family-code` | `'SF Mono', 'JetBrains Mono', 'Fira Code', Consolas, 'Liberation Mono', Menlo, Courier, 'PingFang SC', 'Microsoft YaHei'` | 等宽栈 |
| `--dsw-font-markdown-base` | `400 16px/28px` | assistant 正文 |
| `--dsw-font-markdown-h1…h4` | `700 24/34` `700 22/32` `700 20/30` `600 16/28` | markdown 标题 |
| `--dsw-font-markdown-code` | `400 14px/22px` code 栈 | 行内代码 |
| `--dsw-font-markdown-code-block` | `400 13px/22px` code 栈 | 独立代码块 |
| `--dsw-font-markdown-code-block-small` | `400 12px/18px` code 栈 | 工具卡内 code |
| `--dsw-font-xl-24` | `600 24px/32px` | hero/大标题 |
| `--dsw-font-l-20` | `500 20px/28px` | 次级标题 |
| `--dsw-font-s-14` | `400 14px/22px` | 常规正文小 |
| `--dsw-font-s-strong-14` | `500 14px/22px` | 强调 14 |
| `--dsw-font-xs-13` | `400 13px/20px` | chip/工具行 |
| `--dsw-font-xs-strong-13` | `500 13px/20px` | 页签/强调 13 |
| `--dsw-font-xxs-12` | `400 12px/18px` | 说明/统计 |
| `--dsw-font-xxxs-11` | `400 11px/14px` | 微型标签 |

### 4.3 圆角 / 间距 / 尺寸要点（暗色）

- 圆角：用户气泡/输入卡 **22**、审批卡 **20**、卡片（IN-OUT/tool 卡/dock/菜单/详情代码块）**12**、crumb **12**、chip **8**、圆钮 **999**、tool 卡 r6、代码块 r12、拖柄 pill r10。
- 列宽/中栏：见 §1.1。
- 消息列：`--dsh-chat-content-width: 748px`；输入卡 `max-width: 780px`（748+2×16）；侧清 `--dsh-composer-side-clearance: 16px`；dock 内缩 `--dsh-composer-dock-inset: 8px`；composer 文本上限 `--dsh-composer-text-max-height: 336px`；composer 栈 gap `6px`。
- 消息流：列 gap 16；user 气泡 `max-width:min(525px,82%)`、`padding:10 16`；assistant 行距 16、字号 16/28。
- 工具行：单行 24px；leading 16px；标题 14/24；摘要 14/24；展开 IN/OUT 节 `max-height:150` 内滚、code 区 260、终端输出 224。
- 输入卡：pt 10、内部 gap 12、工具行 `padding:2 8 6`；发送钮 34px、+ 钮 28px。
- 滚动条：8px 宽，r4 拇指，透明轨道；Firefox `scrollbar-width:thin` + `scrollbar-color`。

### 4.4 浅色主题关键差异（`body:not([data-ds-dark-theme])`）

| token | 浅色值 | 差异说明 |
|---|---|---|
| `--dsw-alias-bg-base` | `#FFFFFF` | 底色变白 |
| `--dsw-alias-bg-overlay` | `#E9ECF2` | 浮层底变浅灰 |
| `--dsw-alias-label-primary` | `#0F1115` | 主文字变近黑 |
| `--dsw-alias-label-secondary` | `#61666B` | 次文字 |
| `--dsw-alias-label-tertiary` | `#81858C` | 弱文字 |
| `--dsw-alias-label-caption` | `#ADB2B8` | 说明 |
| `--dsw-alias-border-l1` | `rgba(0,0,0,0.04)` | 描边变黑半透明 |
| `--dsw-alias-border-l2` | `rgba(0,0,0,0.1)` | 常规线 |
| `--dsw-alias-border-l3` | `rgba(0,0,0,0.12)` | |
| `--dsw-alias-state-business-primary` | `#4176E6` | 业务蓝变深（deepseek-500） |
| `--dsw-alias-state-business-tertiary` | `#E4EDFD` | 蓝弱底变浅蓝 |
| `--dsw-alias-state-error-primary` | `#EC1313` | 红更深 |
| `--dsw-alias-state-warn-tertiary` | `#FEF5E7` | 警告弱底变浅琥珀 |
| `--dsw-alias-interactive-bg-hover` | `rgba(38,49,72,0.06)` | 悬停淡底（蓝黑调） |
| `--dsw-alias-interactive-bg-hover-solid` | `#F1F3F5` | 悬停实底 |
| `--dsw-alias-button-info-fill` | `#4176E6` | 发送钮蓝更深 |
| `--dsw-specific-bubble` | `#EDF3FE` | **用户气泡变浅蓝底**（deepseek-50） |
| `--dsw-specific-input-major` | `#FFFFFF` | 输入卡白底 |
| `--dsw-specific-menu` | `#FFFFFF` | 菜单白底 |
| `--dsw-specific-sidebar-fill` | `#F9FAFB` | 侧栏浅灰 |
| `--dsw-specific-tip` | `#F5F6F7` | dock 卡浅灰 |
| `--dsw-alias-markdown-code-block` | `#F9FAFB` | 代码块浅灰 |
| `--dsw-alias-scrollbar-bg-l1` | `#E5E5E5` | 滚动条浅 |
| `--dsw-alias-brand-primary` | `#0F1115` | 品牌=墨黑 |
| `--dsw-alias-button-primary-fill` | `#0F1115` | 主按钮墨底白字 |

> 浅色下唯一结构性差异是**用户气泡**从深灰变成浅蓝底；其余均为同一套别名 token 的换值。

---

## 5. 中文文案清单（源码 `locales.ts` + 字面量）

**输入栏**
- 占位：`给智能体发消息` / `描述你想要构建的内容`(hero) / `选择一个工作区开始` / `会话不可用` / `父会话已离线，无法继续发送；仍可停止当前运行` / `描述你的任务以生成计划`(plan) / `Cmd/Ctrl+Enter 插话发送全部排队消息`
- 按钮：`发送消息` / `停止生成` / `命令`
- 命令提示：`输入目标，智能体将持续执行` / `当前目标进行中。可输入 edit 修改 / pause 暂停 / resume 继续 / clear 清除`
- 访问模式：`访问模式，当前：{name}`；Full access 确认：`确认启用 Full access？` `启用 Full access 后…（长说明见源码）` `我已了解风险，并愿意继续` `取消` `启用 Full access`

**空态引导**
- 主标题：`智行未至之境`；角标：`预览版`；工作区：`选择工作区`

**顶部**
- 页签：`对话`；面包屑 aria：`会话层级`

**消息流**
- 加载：`载入历史…` / `历史加载失败：{message}（{code}）` / `加载更早` / `回到底部` / `载入历史…`
- 消息动作：`复制`→`已复制`；`在新对话中分支` / `仅可从已完成轮次的最后一条消息分支`；`用时 {duration}`；`首 token {seconds}秒`；`{tps} tok/s`；时间模板 `{m}月{d}日` / `{y}年{m}月{d}日`
- 轮次：`本轮运行失败` / `已达到输出 token 上限` + `回答被截断，已有输出保留在对话中。发送“继续”可让模型接着输出。` / `已停止`
- 重试：`正在重试模型请求` / `模型请求重试已取消` / `已重试模型请求` / `等待重试模型请求` / `{label}（{retry}/{maximum}） · {seconds}s` / `重试延迟：` / `失败原因：`
- 引用/注入：`引用会话 · {labels}`（分隔 `、`）/ `上下文注入` / `跨会话召回` / `上下文已压缩` / `已压缩 {items} 条历史记录（约 {tokens} tokens）` / `点击查看压缩摘要` / `附加内容块` / `未知 surface 事件：{type}`
- 图片：`最多 {count} 张，每张 {size}` / `一条消息最多添加 {count} 张图片` / `单张图片不能超过 {size}` / `图片总大小超过 {size}，请移除部分图片` / `仅支持 PNG、JPG、WebP、GIF 格式的图片` / `图片加载失败，点击重试` / `图片加载中…` / `原图预览` / `查看原图` 等（P1 可不做图片，文案备用）

**详情面板**
- `详情` / `关闭详情` / `点击消息流中的工具行查看详情` / `该调用不在当前窗口内` / `输入` / `输出` / `运行中…`

**任务面板**
- `任务` / `{done} 已完成` / `{active} 进行中` / `{pending} 待处理`

**队列 dock**
- `{n} 条排队消息` / `编辑排队消息` / `保存排队消息` / `取消编辑` / `删除排队消息` / `插话发送` / `仅运行中可插话发送` / `包含非文本内容，暂不支持编辑` / `编辑失败：这条消息可能已经开始发送。` / `删除失败：这条消息可能已经开始发送。` / `插话发送失败，请重试。`

**审批面板**
- `等待审批` / `审批详情` / `工具 {toolName} 请求越权执行` / `拒绝` / `允许一次`

**工具卡片/统计**
- 工具行 aria：`运行中` / `失败` / `已停止`
- 终端卡：`信号 {signal}` / `退出码 {code}` / `运行中` / `失败` / `已完成` / `无输出` / `收起输出` / `展开其余 {n} 行输出` / `… 其余 {n} 行`
- 统计行：`{turns} 轮 · {steps} 步` / `LLM {duration}` / `工具调用 {duration}` / `首 token 平均 {duration}` / `{throughput} tok/s` / `缓存命中 {percent}%` / `输入 {input} tok · 输出 {output} tok`
- 上下文：`上下文已用 {percent}` / `上下文已用` / `系统提示词` / `工具` / `对话消息`
- 时长：`{seconds}秒` / `{minutes}分{seconds}秒`；JSON 截断 `… 已截断，共 {total} 字符`

**源码硬编码英文（移植时应本地化或保留原样）**
- `TurnStatus` 标签：`Deep diving...`（未走词典）
- 思维链标题：`Think`
- 工具变体标题：`Search/Read/Bash/Write/Edit/Code/Tool call`（设计字面量）
- IN/OUT 卡 gutter：`IN` / `OUT`；Inspect 胶囊：`Inspect`

---

## 6. 数据需求映射 + API 缺口清单

github.com/jabing/shutu-agent 现有 API（节选自任务书）：
`GET /api/sessions`、`GET /api/sessions/{id}/events` → `[{seq,type,time,summary,reasoning?,tool_name?,tool_output?}]`、`POST /api/sessions`、`POST /api/sessions/{id}/resume`、`POST /api/sessions/{id}/message {text}`、`GET /api/sessions/{id}/events/stream`（SSE `data:{json}`，帧类型 user/message、assistant/chunk、assistant/message、tool/result、tool/error）、`GET /api/config`、`GET /api/subagents`、`GET /api/jobs`、`GET /api/health`。

### 6.1 UI 元素 → API 字段映射

| UI 元素 | 所需数据 | 来源 API | 状态 |
|---|---|---|---|
| 会话列表/标题 | id、标题(summary)、是否空会话、当前是否运行 | `GET /api/sessions` | ⚠️ 列表字段未在本规格 API 清单中给出 → **缺：需确认 sessions 响应含 id/title/blank/running** |
| 选择/新建会话 | 会话 id；新建 | `GET /api/sessions`、`POST /api/sessions` | ✅ 有 |
| 恢复会话 | — | `POST /api/sessions/{id}/resume` | ✅ 有 |
| user 气泡 | 用户消息全文文本、时间 | `user/message` 帧（或 events 里 type=user/message 的 text/ summary） | ⚠️ 若 `summary` 即全文则 ✅；否则 **缺：user 消息文本字段** |
| assistant 文本 | 流式分块文本（追加） | `assistant/chunk`；最终 `assistant/message` | ⚠️ 需分块文本字段；若帧只带 summary → **缺：assistant/chunk 的增量文本**；**缺：assistant/message 的完整文本**（若被 summary 截断） |
| 思维链折叠块 | 推理文本（流式+最终） | `reasoning?` 字段 + 推理流式帧 | ⚠️ 有静态 `reasoning`；**缺：推理流式帧类型（如 assistant/reasoning 或 reasoning/chunk）** |
| 工具行（工具卡片） | 工具名、参数 JSON（摘要/IN 卡/路径）、callId、运行态、输出、错误、退出码 | `tool/result`(tool_name/tool_output)、`tool/error` | ❌ **缺**：`tool/call`（或等价）工具调用开始事件（否则无运行态与行首）；**缺：工具参数 arguments JSON**；**缺：callId**（详情/选中/锚点）；**缺：结构化退出码/信号**；**缺：run_code 子调用嵌套** |
| 工具卡片变体识别 | 工具名 | tool_name | ⚠️ 有（按名分类） |
| 错误行 | 本轮失败消息+code | `tool/error` / assistant 错误帧 | ⚠️ tool/error 有；**缺：assistant/轮次级错误（turn-error）类型与 message/code** |
| 运行中状态行 "Deep diving..." | 当前是否 running、起始时间 | — | ⚠️ 可由最近 user 后无 assistant/message 推断；**缺：明确的 running/停止信号（如 turn/start、assistant/start、tool/error 即停止）** |
| 回到底部/历史加载 | hasMore 标记、分页 before=seq | `GET /api/sessions/{id}/events` | ❌ **缺：分页参数（before/offset/limit）与 hasMore/nextSeq 字段** |
| 上下文计量（ContextMeter） | 已用%、usedTokens、contextWindow、system/tools/messages 分解 | — | ❌ **缺：contextPressure/contextBreakdown 类字段（projectedTokens/contextWindow/systemTokens/toolsTokens/messageTokens）** |
| 统计行（StatsLine） | 轮数、步数、LLM/工具耗时、TTFT、tok/s、缓存命中、输入/输出 token | — | ❌ **缺：sessionStats/tokenUsage 类字段**；轮数/步数可由事件计数近似（user/message 数 + assistant/message 数），但耗时/token 无来源 |
| 任务面板（TodoPanel） | todos `[{content,status}]` | — | ❌ **缺：todos 数据源** |
| 队列 dock（QueueDock） | 排队消息列表 + edit/remove/steer 操作 | — | ❌ **缺：排队消息列表与操作端点**（任务书未提供） |
| 审批面板（ApprovalPanel） | 待审批工具调用（理由、命令）+ 允许一次/拒绝操作 | — | ❌ **缺：审批请求帧与 allow/reject 操作端点** |
| 详情面板（DetailsPanel） | 选中调用 callId → 参数 + 结果 | — | ❌ **缺：callId 关联 + 按调用取参数/结果**（见工具行） |
| 打开文件 | 宿主打开文件/文件夹能力 | — | ❌ **缺：文件打开 API**（P1 可降级为无操作/提示） |
| 分支/会话层级 | 父会话链、fork 操作 | `GET /api/subagents` | ❌ **缺：会话 parentId/层级与 fork 端点** |
| 消息复制 | 剪贴板 | 纯前端 | ✅ 无需 API |
| 空态引导 | 是否空会话（blank） | events 为空 → blank | ⚠️ 可推断（无事件=blank）；会话级 blank 标记可省 |
| 主题 | 深/浅设置 | `GET /api/config`（脱敏） | ⚠️ 若 config 含主题字段则用；否则前端本地偏好 |
| 模型/权限/计划座位 | 模型列表、权限、plan 状态 | — | ❌ **缺**（P1 可不渲染这些槽位） |

### 6.2 缺口清单（按优先级）

1. **工具生命周期**：`tool/call` 开始事件、`arguments`(JSON)、`callId`、`exit_code/signal`、子调用层级 —— 工具行/详情面板的地基。
2. **SSE 帧补充**：`assistant/reasoning`（思维链流式）、明确的 `turn/start` / `turn/end`（running 状态与轮次边界）、assistant 帧携带**完整文本**而非截断摘要。
3. **分页**：events 支持 `before=seq&limit=N` 并返回 `has_more` / `next_before`；首屏加载方向（最新在前 or 最早在前）需与 UI 流式追加约定一致。
4. **详情数据**：按 `callId` 取参数/结果（或 events 里携带）。
5. **上下文与统计**：contextPressure/contextBreakdown、sessionStats/tokenUsage（P1 可整体隐藏 ContextMeter 与 StatsLine 的无数据分组）。
6. **任务 / 队列 / 审批**：todos 数据源、排队消息列表与 edit/remove/steer、审批请求与 allow/reject。
7. **会话列表字段**：确认 `GET /api/sessions` 返回 `id/title/blank/running`（含标题供面包屑/侧栏）。
8. **文件打开 / 分支 / 会话层级**：P1 可先不做（对应 UI 隐藏）。

### 6.3 可实现的最小闭环（P1 建议首版）

- 三栏壳 + 侧栏占位 + 会话列表切换 + 空态 hero。
- 消息流渲染：user 气泡 / assistant markdown（SSE 分块） / 简化工具行（tool_name + 输出，可展开 IN/OUT）/ 错误行 / running 指示。
- 输入栏：自动增高、Enter 发送、发送中禁用、Stop（若 SSE 有取消则做）。
- 底部吸附 + 「加载更早」分页（配合 §6.2.3）。
- ContextMeter / StatsLine / Todo / Queue / Approval：数据结构未就绪前隐藏（组件可保留，无数据显示 null，与 dsh 行为一致）。

---

## 7. 附：源码文件清单（研究范围）

已读取（核心）：
- `ui-layout/src/client/AppFrame.tsx`、`AppFrame.module.css`、`columns.ts`、`stores.ts`、`theme-presenter.ts`
- `ui-conversation/src/client/skeleton/ConversationRoot.tsx`、`ConversationRoot.module.css`、`ConversationSession.tsx`、`EmptyHero.tsx`、`HeroShell.module.css`、`InputBar.tsx`、`InputBar.module.css`、`DetailsPanel.tsx`、`DetailsPanel.module.css`、`ContextMeter.tsx`、`ContextMeter.module.css`、`ApprovalPanel.tsx`、`ApprovalPanel.module.css`、`TodoPanel.tsx`、`TodoPanel.module.css`
- `ui-conversation/src/client/chat/ChatView.tsx`、`ChatView.module.css`、`MessageItem.tsx`、`MessageItem.module.css`、`ReasoningRow.tsx`、`ReasoningRow.module.css`、`AssistantMarkdown.tsx`、`AssistantMarkdown.module.css`、`StatsLine.tsx`、`StatsLine.module.css`、`MessageIconActions.tsx`、`MessageIconActions.module.css`、`tool-node-reader.ts`、`register-node-renderers.ts`、`turn-assistant.ts`、`AssistantNodeView.tsx`、`ChatNodeSeat.tsx`、`message-chrome.ts`、`turn-metrics.ts`、`locales.ts`、`conversation-nodes/tool.ts`
- `ui-conversation/src/client/queue/QueueDock.tsx`、`QueueDock.module.css`
- `ui-tool/src/client/apply.ts`、`locale.ts`、`tool/ToolCallTree.tsx`、`ToolCallTree.module.css`、`tool/ToolDetails.tsx`、`tool/ToolDetails.module.css`、`tool/components/ToolRow.tsx`、`ToolRow.module.css`、`tool/toolviews/GenericToolCard.tsx`、`tool/models/tool-call-model.ts`、`terminal-card-model.ts`、`read-card-model.ts`、`diff-card-model.ts`、`search-card-model.ts`、`web-card-model.ts`
- `ui-theme/src/styles/base.css`、`design-platform.css`、`gradient-shadow-text.css`、`scrollbar.css`

未读取（不影响 P1 实现，P1 仅做占位/隐藏）：
- `ui-layout` 的 `service.ts`/`index.ts`（注册细节）
- `ui-conversation` 的 `apply.ts`/`service.ts`/`stores.ts`/`contract/*`、`conversation-nodes/` 其余文件、`input/*`（机器状态机）、`settings/*`、`skeleton/PermissionSelect.tsx`/`safari.ts`、`chat/CommandNodeView.tsx`/`CompactionItem.tsx`/`ContextInjectionRow.tsx`/`ContextBody.tsx`/`TurnTailNodeView.tsx`/`use-*`、`reference/*`
- `ui-tool` 的 `tool/toolviews/*` 各注册行、`contract/slots.ts`、`index.ts`
- `ui-theme` 的 `shiki.css`、`ui-theme/src/client/*`（主题服务）
- 侧栏（ui-sidebar）、附件（ui-attachment）、命令（ui-commands）、模型选择（ui-model-selection）等非 P1 包

> 备注：`ui-tool` 的 `ui-tool/src/client` 只有 locale/apply/index —— 工具卡片渲染确实在 `tool/` 子目录（ToolRow/卡片/模型），已全部纳入。
