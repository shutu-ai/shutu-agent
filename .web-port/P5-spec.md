# P5 移植规格：消息反馈（👍/👎）+ 附件（图片）+ 主题跟随系统

> 目标：把 dsh web 的「消息条操作（复制/反馈）」「附件（图片）」「主题三态（light/dark/system）」三块 UI 移植到 github.com/shutu-ai/shutu-agent（Go 单体、vanilla JS 无构建、零依赖、中文文案）。
> 现实约束：消息无 id（只有 seq）、无反馈后端、`internal/attachment` 已存在但未接 web、config.yaml 只读。
> 本规格是「可照抄」的实现参考：文件清单 → 一句话要点 + DOM 草图 → 数据映射 → 配色/尺寸 → 文案 → API 缺口 → P5 最小闭环。

---

## 1. 读过的 dsh 文件清单（相对 `deepseek-harness/`，全部只读未改）

### 消息反馈（`ui-message-feedback`，独立包，贡献到 assistant 消息的 actions 槽位）
- `packages/client/ui-message-feedback/src/client/MessageFeedbackActions.tsx`（👍/👎 + 注记弹层主组件）
- `packages/client/ui-message-feedback/src/client/MessageFeedbackActions.module.css`（按钮/弹层全部样式）
- `packages/client/ui-message-feedback/src/client/controller.ts`（会话级反馈对象层：list/put/delete + 版本冲突回放）
- `packages/client/ui-message-feedback/src/client/slots.ts`（inject 面：ensure/rate/toggle/clearNote）
- `packages/client/ui-message-feedback/src/client/locales.ts`（zh/en 文案）

### 消息条操作（`ui-conversation` 的共享 IconActions 行）
- `packages/client/ui-conversation/src/client/chat/MessageIconActions.tsx`（复制/分支/时间，extraActions 槽位）
- `packages/client/ui-conversation/src/client/chat/MessageIconActions.module.css`（28px 行、hover 显现时间、`.action` 圆钮）
- `packages/client/ui-conversation/src/client/chat/message-chrome.ts`（`formatMessageClock`：当日 HH:mm / 年内 `{m}月{d}日` / 跨年 `{y}年{m}月{d}日`）
- `packages/client/ui-conversation/src/client/chat/TurnTailNodeView.tsx` + `.module.css`（assistant 消息条挂载点：`.actions` margin-top 16 / margin-left -6）
- `packages/client/ui-conversation/src/client/chat/MessageItem.tsx`（user 消息条：`.userRow` `data-time-hover-root`，actions 在气泡下方）
- `packages/client/ui-conversation/src/client/chat/AssistantMarkdown.module.css`（assistant 正文 16/28、块距 16）
- `packages/client/ui-conversation/src/client/contract/slots.ts`（`conversation.chat.assistant-actions` 槽位 + `ComposerAttachment` / `MessageImagesOwnerProps` 类型）
- `packages/client/ui-conversation/src/client/locales.ts`（`message.*` 与 `image.*` 全部 zh 文案）

### 附件（`ui-attachment`，纯展示包，数据来自 conversation 的 service/InputBar）
- `packages/client/ui-attachment/src/AttachmentRail.tsx` + `.module.css`（64px 缩略图 rail：edge 箭头翻页、hover 显现移除、单点开原图）
- `packages/client/ui-attachment/src/DropOverlay.tsx` + `.module.css`（全屏拖拽遮罩，纯装饰 pointer-events:none）
- `packages/client/ui-attachment/src/ImageLightbox.tsx` + `.module.css`（原图预览：Esc/点底/✕ 关闭、焦点还原）
- `packages/client/ui-attachment/src/MessageImage.tsx` + `.module.css`（历史图片：单图 240px 长边 / 多图 64px 平铺、惰性加载、失败重试）
- `packages/client/ui-attachment/src/client/ComposerAttachments.tsx` + `.module.css`（草稿 rail + drop 遮罩 + lightbox 的组装器）
- `packages/client/ui-attachment/src/client/MessageImages.tsx`、`src/client/labels.ts`、`src/client/index.ts`、`src/index.ts`
- `packages/client/ui-conversation/src/client/service.ts`（`browserDraftAttachment`：`crypto.randomUUID()` + `URL.createObjectURL(file)`）
- `packages/client/ui-conversation/src/client/skeleton/InputBar.tsx`（`intakeImages` 粘贴/拖拽准入校验 + toast 拒因）

### 主题（`ui-theme` + `ui-layout`）
- `packages/client/ui-theme/src/theme-settings.ts`（`THEME_PREFERENCES = ['light','dark','system']`，默认 system）
- `packages/client/ui-theme/src/boot-theme.ts`（首屏内联脚本：system 解析 + 写 `body[data-ds-dark-theme]`）
- `packages/client/ui-theme/src/client/index.ts`（ThemeRuntime：`matchMedia('(prefers-color-scheme: dark)')` + `change` 监听 + 三态解析）
- `packages/client/ui-theme/src/client/AppearanceRow.tsx` + `.module.css`（「外观」+ 三 cube 行）
- `packages/client/ui-theme/src/client/locales.ts`（外观 / 浅色 / 深色 / 跟随系统）
- `packages/client/ui-layout/src/client/theme-presenter.ts`（DOM 应用：`html color-scheme` + body 属性 + 内联 token + `meta[name=theme-color]`）
- `packages/client/ui-theme/src/styles/design-platform.css`、`gradient-shadow-text.css`（mask / contrast / shadow-lv3 / `--dsw-mask-blur` 取值）

### 对照阅读的 github.com/shutu-ai/shutu-agent（非 dsh，仅映射约束）
- `internal/webserver/static/index.html`（composer / 设置页结构）、`app.js`（renderEvent / addUserMsg / addAssistant / addToolEvent / 主题）、`style.css`（`--dsw-*` 全量 token，见 §4）
- `internal/webserver/webserver.go`（`eventView`、`handleMessage`、路由表、`writeJSON`）
- `internal/attachment/attachment.go`（`Store.SaveImage/Read`、`SupportedMediaTypes`、`MediaTypeForExtension`）
- `.web-port/P4-spec.md`（既有规格格式参照）

---

## 2. 三块的一句话要点 + DOM 结构草图

### 2.1 消息反馈（👍/👎）

**一句话**：dsh 把 👍/👎（+ 已评后出现的「补充说明」注记按钮）渲染进每条已完结 assistant 消息的 IconActions 行，位于「复制」与「分支」之间；按钮始终可见（非 hover 显现）、hover 有圆底，会话级反馈在**首次 hover/聚焦时懒加载**，点 👍/👎 是「toggle 换评/取消」（再点取消），失败态在行内右侧显示错误文案；注记是 portaled 到 `document.body` 的 320px 弹层（含 textarea + 保存/取消）；存储走 Host 端 per-session compare-and-set 侧车（带版本号、冲突回放）。

**DOM 结构草图**（assistant 消息条；P5 建议只做 复制+👍/👎，注记隐藏）：
```
div.msg.assistant[data-time-hover-root]        ← PA 现有 .msg assistant
├─ div.markdown                                ← 正文（现有）
└─ div.msg-actions                             ← 新增（替换现有 .copy-btn 位置，放正文下方）
    ├─ button.act.copy        (复制；点击后 1s 换 ✓"已复制")
    ├─ button.act.fb-like     (👍，aria-pressed=已赞，data-active 高亮)
    ├─ button.act.fb-dislike  (👎，同上)
    └─ span.msg-time          (assistant：图标后 clock="end"，hover 显现；P5 可选)
```
> PA 现状：assistant 消息 `.msg-time` 在顶部、`.copy-btn` 在底部。P5 把「复制」并入 actions 行并加 👍/👎，时间可保留原样（不强行做 hover 显现）。

### 2.2 附件（图片）

**一句话**：dsh **没有上传按钮**——只靠「文档级拖拽」和「文本域粘贴图片」两条路径进草稿；草稿以 64px 圆角缩略图 rail 呈现（hover 显示右上移除 ✕、溢出时边缘箭头翻页、单点开原图 lightbox）；发送时把浏览器端 objectURL 草稿交给 Host 持久化，历史消息图片用 session 授权 URL **惰性加载**（单图 240px 长边 / 多图 64px 平铺，失败可点重试）；全屏 drop 遮罩是纯装饰（`pointer-events:none`，毛玻璃 + 插画 + 「图片拖动到此处即可添加」）。

**DOM 结构草图**：
```
div.composer                                   ← PA 现有 composer card
├─ div.attachments-rail.hidden                 ← 新增，作为 .composer 第一个子元素（textarea 上方）
│   ├─ button.rail-arrow.left                  (有左溢出才渲染)
│   ├─ div.rail                                (overflow-x，滚动条隐藏)
│   │   └─ (每张) div.item > button.thumb(64×64, img object-fit:cover)
│   │                                + button.remove(右上 18px 圆 ✕，hover/focus 显现)
│   └─ button.rail-arrow.right
├─ (其余现有：grow-wrap textarea + send)

（文档级拖拽进行时，body portal）
div.drop-mask [fixed inset:0, z1000, pointer-events:none, 毛玻璃]
└─ svg 插画(115×84) + div「图片拖动到此处即可添加」 + div「最多 {n} 张，每张 {s}」

（点击缩略图，body portal）
div.lightbox [fixed inset:0, z1000]
├─ div.mask (rgba 黑 + blur) + img(最大1600 / 高 max(100vh-80px)) + button.close(右上)

（历史消息图片，user/assistant 消息内）
div.gallery[data-align=start|end]
└─ (单图) button.frame[data-variant=single] 240px 长边 img | (多图) button.frame[data-variant=tile] 64×64 × n
```

### 2.3 主题三态

**一句话**：dsh 是 light/dark/system 三态，`system` 用 `matchMedia('(prefers-color-scheme: dark)')` 解析、监听 `change` 事件实时重算；DOM 应用三件事——`document.documentElement.style.colorScheme`（原生控件/滚动条）、`body[data-ds-dark-theme]`（存在=深，token 调色板）、`meta[name=theme-color]`（浏览器 UI）；设置页 Appearance 行 = 标题「外观」+ 三个 cube（浅色/深色/跟随系统），选中态用模块底 + bluish-400 边框。

**DOM 结构草图**（设置页通用段，替换现有两 cube）：
```
div.settings-row.appearance
├─ div.row-title「外观」
└─ div.cube-row
    ├─ button.theme-cube(data-theme=light)  ☀️ 浅色
    ├─ button.theme-cube(data-theme=dark)   🌙 深色
    └─ button.theme-cube(data-theme=system) 🖥 跟随系统
```

---

## 3. 逐元素数据映射

### 3.1 反馈（dsh ↔ github.com/shutu-ai/shutu-agent，无后端 → 前端 localStorage）

| dsh 元素 | github.com/shutu-ai/shutu-agent 现状 | 状态 | 降级方案 |
|---|---|---|---|
| 消息定位 `messageId`（Host 持久消息 id） | **无消息 id**，只有事件 `seq` | **缺** | 反馈键 = `` `${sessionId}:${seq}` ``；`data-seq` 标注在 actions 行；渲染/点击都用 seq |
| 会话级反馈列表 `list()`（懒加载） | 无反馈后端 | **缺** | 渲染时同步读 `localStorage.pa_feedback`（无异步，无需懒加载） |
| `put`/`delete` + `ifVersion` 版本冲突回放 | 单用户本地、无并发 | **缺（不需要）** | 直接覆盖写，不做冲突回放 |
| 点 👍/👎 = toggle（已评同值 → 取消；换值 → 替换） | 可照抄 | ✅ | 前端 `pa_feedback[sessionId][seq]` 三元：undefined / positive / negative |
| `data-active` 高亮（已评色 = label-primary） | 可照抄 | ✅ | — |
| `disabled={pending}` 防连点 | 本地即时写 | ✅ | 写操作同步，无需 pending |
| 注记 note 弹层（320px portal + textarea + 保存/取消） | 无 | **缺** | **P5 隐藏**；文案键保留备用 |
| 行内失败态（`error.conflict/generic/load`） | 本地写失败概率极低 | 部分 | 仅保留「反馈保存失败」一条（try/catch + JSON.parse 容错） |
| 复制按钮（IconCopyOutline → 1s ✓） | 已有 `.copy-btn`（assistant） | ✅ | 并入 actions 行；user 消息可选加复制 |
| 分支按钮（`onBranch`，仅完成轮最后一条） | 无 fork API | **缺** | **P5 隐藏**（不做分支） |
| hover 显现时间（`data-time-hover-root`，`@media (hover:hover)` 才隐藏） | `.msg-time` 恒显 | 部分 | P5 保持恒显（不学 hover 显现），减少动画复杂度 |

### 3.2 附件（dsh ↔ github.com/shutu-ai/shutu-agent，UI 可照抄，后端三件套要补）

| dsh 元素 | github.com/shutu-ai/shutu-agent 现状 | 状态 | 降级方案 |
|---|---|---|---|
| 草稿 `ComposerAttachment{kind,id,file,previewUrl}` | 无 | ✅ 可直接照抄 | `id = crypto.randomUUID()`、`previewUrl = URL.createObjectURL(file)`，仅存内存 Map |
| 拖拽准入：document-level dragenter/over/leave/drop + `dragDepth` 计数 + `dropEffect=copy/none` | 无 | ✅ 照抄 | `canAcceptDrop` = 会话就绪且非 running 锁定时 |
| 粘贴准入：textarea `paste` → `clipboardData.items.getAsFile()` | 无 | ✅ 照抄 | 有文件时 `preventDefault`，走同一 `intakeImages` |
| 准入校验：格式→数量→单张→总量（先格式后限制，dsh filter order） | config.yaml 只读、无图片限制配置 | **缺** | 前端硬编码默认 `maxImages=10`、`maxImageBytes=10MB`、`total=20MB`、类型 = PNG/JPG/WebP/GIF（与 `attachment.SupportedMediaTypes` 对齐）；超限 toast（§5.2 文案） |
| rail 64px 缩略图 + hover 移除 + edge 箭头 + 单点 lightbox | 无 | ✅ 照抄 | 无滚动条；垂直 wheel 转横向步进（可选） |
| Host 持久化（落盘 + 会话只存 ImageRef） | `attachment.Store.SaveImage` 已存在，**未接 web** | **缺 API** | 新增 `POST /api/sessions/{id}/attachments`（§6-①） |
| 发送带图（submit 携带 image ids → 入 loop） | `handleMessage` 只收 `{text}` | **缺 API** | 新增 `images` 字段（§6-③）；后端未支持时返回 400 → 前端提示「当前版本暂不支持带图发送」并保留草稿 |
| 历史图片惰性加载（session 授权 URL + 失败重试） | `eventView` 无图片字段 | **缺 API** | 扩展 `eventView`（§6-④）+ `GET .../attachments/{id}`（§6-②）；未扩展前**隐藏历史图**（只做草稿 rail + lightbox） |
| 单图 240px 长边 / 多图 64px tile / aspect clamp [0.25,4] | 无 | ✅ 照抄 | 宽高未知（attachment 不解析 → 0）时按 1:1 方盒渲染 |
| 全屏 drop 遮罩（`--dsw-alias-bg-mask-drop` + blur(10px) + 插画） | 无 | ✅ 照抄 | **需新增 token**（§4.2） |
| 上传进度 | dsh **本就没有进度条**（小文件本地快） | ✅ 不实现 | 发送时整批 await；失败走 toast 恢复草稿 |

### 3.3 主题（dsh ↔ github.com/shutu-ai/shutu-agent）

| dsh 元素 | github.com/shutu-ai/shutu-agent 现状 | 状态 | 降级方案 |
|---|---|---|---|
| 三态 `light/dark/system`（默认 system） | `pa_theme` 只有 light/dark，默认 dark | 部分缺 | 新增 `"system"` 值；未设置时仍默认 dark（保持现状，不强制 system） |
| `system` 解析：`matchMedia('(prefers-color-scheme: dark)')` | 无 | **缺** | `applyTheme()` 内三态分支 + `change` 监听重算（§7） |
| `html { color-scheme }`（原生控件） | 无 | **缺** | 设置 `document.documentElement.style.colorScheme` |
| `body[data-ds-dark-theme]`（PA 语义 `="true"/"false"`） | ✅ 已有 | ✅ | **保持 PA 的 `"true"/"false"` 值语义**（style.css 有 3 处 `="false"` 选择器，不迁移为 presence，避免误伤） |
| `meta[name=theme-color]` | 无 | **缺**（可选） | 取 `getComputedStyle(body).backgroundColor` 写入；P5 可加 |
| 外观行三 cube（浅色/深色/跟随系统） | 现有两 cube（浅色/深色） | 部分 | 加第三个「跟随系统」cube |
| 内联 token 覆盖层（第三方主题 overrideTokens） | 无动态主题 | **缺** | P5 不做（github.com/shutu-ai/shutu-agent 只需内建两套 palette，无第三方主题） |
| 主题偏好持久化（user-settings 文档） | `localStorage.pa_theme` | ✅ | 沿用 localStorage（config.yaml 只读，不可写设置） |

---

## 4. 配色 / 尺寸（token 具体值，深浅两套）

> github.com/shutu-ai/shutu-agent `style.css` 已按同一 dsh 来源定义 `--dsw-*` 全量变量（`:root` 深色基座 + `body[data-ds-dark-theme="false"]` 浅色覆盖），P5 全部复用，仅需新增 3 个缺失 token（见 §4.2）。以下「浅/深」列即 PA 现有值（与 dsh 对齐）。

### 4.1 反馈按钮（照抄 `MessageIconActions.module.css` / `MessageFeedbackActions.module.css`）

| 项 | 值 |
|---|---|
| `.act`（复制/👍/👎 共用） | `width:28px; height:28px; padding:6px; border-radius:28px; background:transparent; color:var(--dsw-alias-label-tertiary)`（深 `#ADB2B8` / 浅 `#81858C`） |
| hover | `background:var(--dsw-alias-interactive-bg-hover)`（深 `rgba(255,255,255,.08)` / 浅 `rgba(38,49,72,.06)`）、`color:var(--dsw-alias-label-secondary)` |
| 已评 `[data-active]` | `color:var(--dsw-alias-label-primary)`（**不随 hover 消退**，保持可读） |
| disabled | `opacity:.4; cursor:default` |
| actions 行 | `display:flex; align-items:center; gap:10px; height:28px` |
| assistant 行偏移 | `margin-top:16px; margin-left:-6px`（光学对齐 28px 圆钮） |
| 复制成功 | 1s 内图标换 ✓、文案「已复制」 |
| 注记按钮（P5 隐藏，备查） | `max-width:220px; font-size:13px; line-height:28px; border-radius:14px; color:tertiary`，hover `interactive-bg-hover` |

### 4.2 附件（照抄 ui-attachment 各 module.css）

| 项 | 值 |
|---|---|
| rail 卡片 | `64×64px; border-radius:16px; gap:10px; border:1px solid var(--dsw-alias-border-l2-thin)`（dsh 用 `-darkmode-thin`，PA 同名替代；深 `rgba(255,255,255,.06)` / 浅 `rgba(0,0,0,.06)`，比 dsh 浅色 `.1` 略细，可接受）；`background:var(--dsw-alias-interactive-bg-hover)`；`cursor:zoom-in` |
| 移除 ✕ | `18×18px 圆; top:4px; right:4px; background:var(--dsw-alias-button-primary-fill); color:var(--dsw-alias-bg-base)`（dsh 用 `--dsw-alias-button-contrast-fill` + `-label-primary-inverted`，PA 无 → 用 primary-fill + bg-base 反色对，与 PA `.send-btn` 同系）；`opacity:0` → hover/focus `1`，`transition:opacity .2s`；`@media(pointer:coarse){opacity:1}`；`prefers-reduced-motion` 关过渡 |
| edge 箭头 | `24×24px; border-radius:999px; top:50%; translateY(-50%); left/right:4px; border:1px solid var(--dsw-alias-border-l2-thin); background:var(--dsw-specific-input-major); color:var(--dsw-alias-label-secondary); box-shadow:var(--dsw-shadow-lv2)` |
| rail 滚动条 | 隐藏（`scrollbar-width:none` + `::-webkit-scrollbar{display:none}`） |
| drop 遮罩 | `fixed inset:0; z-index:1000; pointer-events:none; background:var(--dsw-alias-bg-mask-drop); backdrop-filter:blur(10px); animation:fade-in 160ms`；插画 `115×84`；标题 `font:500 20px/28px`（`--dsw-font-l-20`）；desc `14px/22px`、`color:label-tertiary`、`white-space:pre-wrap`；`prefers-reduced-motion` 关动画 |
| lightbox | `fixed inset:0; z-index:1000; padding:40px;` mask=`var(--dsw-alias-bg-mask-1)` + `backdrop-filter:var(--dsw-mask-blur)`；图 `max-width:min(100%,1600px); max-height:calc(100vh-80px); object-fit:contain; border-radius:12px; box-shadow:var(--dsw-shadow-lv3)`；close `36×36px; top:20px; right:20px; border-radius:999px; background:var(--dsw-specific-input-major)` |
| 历史单图 | 长边 `240px`；`aspect = clamp(0.25, w/h, 4)`；`object-fit:cover`；`objectPosition`：超高 `center top`、超宽 `left center`、否则 `center`；**不放大超过自然尺寸** |
| 历史多图 | `64×64px` tile，`gap:10px`，`flex-wrap:wrap`；`data-align=start|end` 控制 `justify-content` |
| 加载/失败态 | `font-size:12px; line-height:18px; color:var(--dsw-alias-label-tertiary)`；失败按钮 `border:1px solid var(--dsw-alias-border-l2-thin); background:var(--dsw-alias-interactive-bg-hover-danger); border-radius:10px`（tile 失败态保持 64px 格，radius 16） |
| 历史图边框 | `border:1px solid var(--dsw-alias-border-l2-thin); border-radius:16px; background:var(--dsw-alias-interactive-bg-hover)` |

### 4.3 主题 cube（照抄 `AppearanceRow.module.css`）

| 项 | 值 |
|---|---|
| `.theme-cube` | `flex:1 1 180px; flex-direction:column; align-items:center; gap:4px; padding:20px 32px; border:1px solid var(--dsw-alias-border-l2); border-radius:16px; font-size:14px; color:var(--dsw-alias-label-primary)` |
| hover（非选中） | `background:var(--dsw-alias-interactive-bg-hover)` |
| 选中 `.active` | `background:var(--dsw-alias-bg-layer-3); border-color:var(--dsw-alias-border-l3)`（PA 现有 `.theme-cube.active` 已是此配方，与 dsh 的 `bg-module-platform` + `bluish-400` 近似，**无需改**） |

### 4.4 P5 需在 style.css 新增的 3 个缺失 token

| token | 深色值 | 浅色值 | 用途 |
|---|---|---|---|
| `--dsw-alias-bg-mask-drop` | `rgba(39,39,48,0.7)` | `rgba(255,255,255,0.7)` | drop 遮罩底 |
| `--dsw-alias-bg-mask-1` | `rgba(0,0,0,0.5)` | `rgba(0,0,0,0.24)` | lightbox 遮罩底 |
| `--dsw-mask-blur` | `blur(2px)` | `blur(2px)` | lightbox 遮罩模糊（drop 遮罩用 `blur(10px)` 是硬编码，勿混） |

> dsh 另有用到但 PA 无需新增的：`--dsw-alias-button-contrast-fill` / `--dsw-alias-label-primary-inverted` / `--dsw-alias-label-primary-foreground` / `--dsw-alias-border-inverted`（§4.2 用 primary-fill + bg-base 反色替代）；`--dsw-alias-bg-mask-*` 是 dsh 也依赖、PA 明确缺失的，必须补。

---

## 5. 完整中文文案清单

### 5.1 反馈（照抄 `ui-message-feedback/locales.ts` zh；P5 本地版仅用加粗项）

| key | 中文 |
|---|---|
| **action.like** | 好的回答 |
| **action.likeActive** | 取消标记 |
| **action.dislike** | 有问题的回答 |
| **action.dislikeActive** | 取消标记 |
| ~~note.open~~ | 补充说明（P5 隐藏） |
| ~~note.dialog~~ | 反馈（P5 隐藏） |
| ~~note.placeholder~~ | 这条回答哪里好，或哪里有问题？（可选）（P5 隐藏） |
| ~~note.save / note.cancel / note.aria~~ | 保存 / 取消 / 反馈说明（P5 隐藏） |
| ~~error.conflict~~ | 这条反馈已在别处改动，已显示最新状态（本地版不用，备查） |
| ~~error.load~~ | 反馈状态加载失败（本地版不用，备查） |
| **error.generic** | 反馈保存失败 |

### 5.2 附件（照抄 `ui-conversation/locales.ts` 的 `image.*`；加粗为 P5 必用）

| key | 中文 |
|---|---|
| **image.dropTitle** | 图片拖动到此处即可添加 |
| **image.dropDesc** | 最多 {count} 张，每张 {size} |
| **image.dropBlocked** | 当前无法添加图片 |
| **image.pending** | 待发送图片 |
| **image.openOriginal** | 查看原图 |
| **image.openOriginalLabel** | {label}，点击查看原图 |
| **image.remove** | 移除图片 {name} |
| image.scrollLeft / scrollRight | 向左滚动图片 / 向右滚动图片 |
| image.original | 原图 |
| image.label | 图片 |
| **image.loadFailed** | 图片加载失败，点击重试 |
| **image.loading** | 图片加载中… |
| **image.preview** | 原图预览 |
| **image.closePreview** | 关闭原图预览 |
| **image.unsupportedType** | 仅支持 PNG、JPG、WebP、GIF 格式的图片 |
| **image.tooMany** | 一条消息最多添加 {count} 张图片 |
| **image.fileTooLarge** | 单张图片不能超过 {size} |
| **image.totalTooLarge** | 图片总大小超过 {size}，请移除部分图片 |
| image.tooManyPixels / dimensionTooLarge | 图片分辨率过大，请压缩后重试 / 图片宽高不能超过 {size}px，请缩小后重试 |
| image.modelUnsupported / subagentUnsupported | 当前模型不支持图片，请切换支持图片的模型 / 子智能体会话暂不支持图片 |
| **image.sendFailed** | 图片发送失败（{reason}），请重新添加图片后再试 |

github.com/shutu-ai/shutu-agent 新增（dsh 无现成词）：

| 用途 | 中文 |
|---|---|
| 上传失败 toast | 图片上传失败：{msg} |
| 发送带图不被后端支持 | 当前版本暂不支持带图发送，请先移除图片 |
| 反馈行分组 aria（可选） | 消息操作 |
| 复制（actions 行按钮 title） | 复制 |
| 已复制（1s 提示） | 已复制 |

### 5.3 主题（照抄 `ui-theme/locales.ts` zh）

| key | 中文 |
|---|---|
| appearance.title | 外观 |
| appearance.light | 浅色 |
| appearance.dark | 深色 |
| appearance.system | 跟随系统 |

---

## 6. github.com/shutu-ai/shutu-agent API 缺口清单（后端要补什么）

> config.yaml 只读：所有新能力走「新端点 + 前端默认值」，不新增配置项（限额硬编码默认值，见 §3.2）。

### 附件（推荐做最小三件套，否则附件进不了模型 = 死胡同）

**① 上传：`POST /api/sessions/{id}/attachments`**
- Content-Type：`multipart/form-data`，字段名 `file`。
- 处理：`r.ParseMultipartForm` → 取 `file` → 用 `attachment.Store.SaveImage(mediaType, data, maxBytes)`（`maxBytes` 默认 10MB）校验/落盘（`<data>/attachments/<id><ext>`）；`attachment.SupportedMediaTypes` 已含 png/jpg/jpeg/webp/gif。
- 成功：`201 { "id": "<hex32>", "media_type": "image/png", "bytes": 1234, "width": 0, "height": 0 }`（宽高暂 0，M8 裁剪）。
- 失败：`400 { "error": "unsupported type" | "empty" | "too large" }`（fail-closed，映射 `ErrUnsupportedType/ErrEmptyData/ErrTooLarge`）。
- 归属：校验 session 存在；返回的 id 仅在其后发送到该会话时有效。

**② 回显：`GET /api/sessions/{id}/attachments/{id}`**
- 处理：attachment store 按 id 找回 `llm.ImageRef` → `Read` 字节 → `Content-Type` 按 `MediaType`、`Cache-Control` 建议 `private, max-age=3600`（或 no-cache，避免旧图）。
- 鉴权：走现有 `requireAuth`；404 当会话/附件不存在。
- 前端用途：历史消息 `<img src>`、lightbox 原图。

**③ 发送带图：`POST /api/sessions/{id}/message`**
- body 扩为 `{ "text": "...", "images": ["<attachmentId>", ...] }`（`images` 可选）。
- 处理：handler 用 attachment store 把 id → `llm.ImageRef`（缺失/不属于该会话 → 400）；随用户消息块交给 loop（**复用 `/attach` 命令已消费 ImageRef 的路径**，零新依赖）。
- 失败：`400 { "error": "image ... not found" }`；前端收到后 `image.sendFailed` toast + 保留草稿。
- **若 loop 接线在本版量大**：降级为后端先忽略 `images` 并返回 `501 { "error": "images not supported yet" }`，前端提示「当前版本暂不支持带图发送，请先移除图片」——但**不推荐**，因为这会做出一个图进不了模型的半成品。

**④ 历史回显：扩展 `eventView`**
- `assistant/message`、`user/message` 的 `extraFields` 增加 `images` 字段：`"images": [{ "id", "name", "media_type", "width", "height" }]`（有界，最多前 N 张）。
- 前端：事件带 `images` 时渲染 `gallery` + 惰性调 ② 加载字节；未带（旧后端）时隐藏历史图（不报错）。

### 反馈（**P5 建议不做后端**，见 §7；若后续要做，最小 API）

| method | path | body / query | response |
|---|---|---|---|
| PUT | `/api/sessions/{id}/feedback/{seq}` | `{ "rating": "positive"\|"negative", "note"? }` | `200 {"ok":true}` 或 `409 {"error":{"code":"version-conflict","current":{...}}}` |
| DELETE | `/api/sessions/{id}/feedback/{seq}` | — | `200 {"ok":true}` |
| GET | `/api/sessions/{id}/feedback` | — | `200 {"items":[{"seq":N,"rating":"positive","note"?}]}` |

- 存储：`internal/store/sqlite` 加一张 `message_feedback(session_id, seq, rating, note, version)` 表（`PRIMARY KEY(session_id, seq)`），`version` 供 compare-and-set。
- **为什么不推荐 P5 做**：单用户本地应用、无多端并发、seq 在事件流稳定；localStorage 半小时工作量 vs 后端新 schema + 测试 + webserver 接线的工作量不成比例。UI 层（§3.1）两端结构一致，后端后补不改前端。

### 前端缺口（纯前端即可补齐，无需后端）

- 消息定位：反馈/复制都用 `data-seq`；无 id 无影响。
- 限额默认值：前端常量 `IMAGE_MAX = 10`、`IMAGE_MAX_BYTES = 10MB`、`IMAGE_TOTAL_BYTES = 20MB`、`IMAGE_TYPES = ['image/png','image/jpeg','image/webp','image/gif']`。
- 主题 system：纯前端（§7.3）。

---

## 7. P5 最小闭环建议

### 7.1 反馈：**前端 localStorage**（推荐，零后端改动）

- 存储：`localStorage.pa_feedback` = `{ "<sessionId>": { "<seq>": "positive"|"negative" } }`；读用 try/catch 容错（损坏则重置空对象）。
- UI：assistant 消息 actions 行 = 复制 + 👍 + 👎（替换现有 `.copy-btn`）；点赞逻辑照抄 dsh toggle：`当前===target ? 取消 : 置 target`；`data-active` 高亮 + `aria-pressed`；渲染时同步查表、点击即写。
- 隐藏：注记弹层（补充说明）、后端 schema、版本冲突、多端同步。
- 消息无 id 的影响：键用 `sessionId:seq`，无降级成本。

### 7.2 附件：**前端完整 UI + 最小上传/回显三件套**（推荐）

- 首版做：
  1. HTML：`.composer` 内 textarea 上方插 `<div id="attachments-rail">`（空则隐藏）；消息流 user/assistant 消息渲染函数支持 `images` 回显（eventView 未带则跳过）。
  2. CSS：§4.2 全部（rail/移除/箭头/drop 遮罩/lightbox/历史图/加载失败态）+ §4.4 三个新 token。
  3. JS（照抄 dsh 逻辑）：
     - `intakeImages(files)`：格式→数量→单张→总量校验（§3.2 默认值），失败 toast（§5.2）；
     - 草稿 Map（`crypto.randomUUID` + `URL.createObjectURL`）+ `revokeObjectURL` 释放；
     - document-level 拖拽（dragDepth 计数、`canAcceptDrop` 门控、dropEffect copy/none）+ textarea `paste`（有文件 `preventDefault`）；
     - rail 渲染 + 移除 + 单点开 lightbox（Esc/点底/✕ 关闭，焦点还原）；
     - 发送：`POST /api/sessions/{id}/message` body `{text, images:[ids]}`；成功清草稿+textarea；失败 `image.sendFailed` toast + 保留草稿；
     - 历史回显：事件 `images` → `GET /api/sessions/{id}/attachments/{id}` 惰性加载，失败「点击重试」，单图 240px/多图 64px。
  4. 后端（§6 ①②③④ 四件，Go，零新依赖）：
     - `POST /api/sessions/{id}/attachments`（multipart → `attachment.Store.SaveImage`）；
     - `GET /api/sessions/{id}/attachments/{id}`（Read + Content-Type）；
     - `handleMessage` 支持 `images`（id → ImageRef → loop，复用 `/attach` 路径）；
     - `eventView` 加 `images` 字段。
- 隐藏：上传进度条（dsh 本就没有，小文件本地快）、断点续传/重试队列、分辨率解码（M8 裁剪，宽高记 0）、子代理会话带图（无数据源）。

### 7.3 主题：**加「跟随系统」第三态**（纯前端，成本最低、收益直观）

- `applyTheme()` 重构：读 `pa_theme`（`"light"|"dark"|"system"`，缺省 `"dark"` 保持现状）→ system 时 `matchMedia('(prefers-color-scheme: dark)').matches` → 写 `body[data-ds-dark-theme]="true|false"`（保持 PA 值语义）+ `documentElement.style.colorScheme` + 更新顶部/设置页图标 + 设置页 cube 高亮。
- 监听：`matchMedia('(prefers-color-scheme: dark)')` 的 `change` → 仅当 `pa_theme==="system"` 时重算（照抄 dsh ThemeRuntime 的 `preference!=='system'` 短路）。
- 设置页：`renderGeneral` 的 cube 行加第三个 `data-theme="system"`「跟随系统」，点击写 `pa_theme`。
- 可选（P5 顺手）：`meta[name=theme-color]` = `getComputedStyle(body).backgroundColor`，随主题切换。
- 不做：内联 token 覆盖层（第三方动态主题）、HTML 首屏内联 boot（PA 已有 JS 主题，无需 SSR 防闪）。

### 7.4 首版明确隐藏 / 不做

- 反馈注记「补充说明」弹层；反馈后端 schema 与版本冲突；多端同步。
- 消息分支（fork）、user 消息复制（已有 assistant 复制，user 可后补）。
- 附件：上传进度条、断点续传、分辨率解码、子代理会话图片。
- 主题：动态 token 覆盖层、多主题注册、首屏 SSR 防闪。
- 不新增任何 config.yaml 配置项（限额用前端常量默认值）。

### 7.5 验收

- 主题：切「跟随系统」后随 OS 深浅联动；切浅/深恢复手动；刷新持久；`html color-scheme` 与 `meta theme-color` 生效。
- 反馈：assistant 消息有 复制/👍/👎 行；点赞后高亮、再点取消；刷新后反馈保留；换会话互不串。
- 附件：拖入/粘贴图片出现 64px rail，hover 可移除，点缩略图开 lightbox；超限/格式错 toast 且不进 rail；发送带图成功，历史回显单图 240px / 多图 64px、失败可重试；上传 501/未支持时提示并保留草稿。
- 消息 seq 定位：反馈键 `sessionId:seq` 与事件流 seq 一致，replay 后不漂移。
