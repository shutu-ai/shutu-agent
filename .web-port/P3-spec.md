# dsh Web 工作台 P3 页移植规格（github.com/shutu-ai/shutu-agent · 设置 + 模型选择）

> 目标：参照 dsh web（deepseek-harness `packages/client/*`）把 github.com/shutu-ai/shutu-agent 的设置页 + 模型选择做成「像 dsh 一样」。
> 本规格只研究 dsh 源码，可照此用零依赖 vanilla JS + CSS 写出等价页面。**不修改 dsh 源码**。
>
> 源码基线：`D:\dev-projects\Agent\deepseek-harness\packages\client\`
> 覆盖包：`ui-settings`（设置域基座/槽契约）、`ui-settings-general`（设置壳 + 通用段）、`ui-settings-models`（模型段）、`ui-model-selection`（对话输入栏模型选择器）、`ui-theme`（token + 外观行）、`ui-agent-preset`（通用段行的行样式参考）。
>
> 架构红线（github.com/shutu-ai/shutu-agent）：config.yaml 只读、无运行时热改（ADR D-WEB2-D）；单 LLM provider（deepseek），模型来自 config.model，**无模型列表配置**；零依赖 vanilla JS、无构建；中文文案。所有「开关/输入」只读展示，附「修改 config.yaml 后重启生效」。

---

## 0. 结论摘要（一句话版）

- **设置 = 一个居中面板，左 188px 导航轨 + 右内容列**（dsh `SettingsRoot`）：导航行（图标+文字，选中底 `#43454A`/浅 `#EBEEF2`）、内容列顶部 54px 头（标题/动作 + 关闭钮）、下方滚动 options 区。
- **模型段 = provider 行卡**（dsh `ModelsSection`）：一行「名称 + 状态点/标签 + 右侧胶囊按钮」；展开时是 `bg-module-platform` 填充编辑器。github.com/shutu-ai/shutu-agent 只读 ⇒ 行卡只展示「模型 · provider · base_url · mode」，编辑钮禁用或隐藏。
- **模型选择器 = 输入栏的一个 28px 圆角 chip**（dsh `ModelSelect`），点开是二级下拉：根菜单（模型 / 推理等级）→ 按 provider 分组的模型列表，当前模型右侧打 ✓。github.com/shutu-ai/shutu-agent 单模型 ⇒ 下拉固定为「DeepSeek」单组、单模型、默认选中 + ✓，整卡 disabled。
- **主题**：沿用 P1/P2 已移植的 `body[data-ds-dark-theme]` token 体系；设置页需要额外解出的 token（nav 选中/hover、菜单面、输入面、阴影/遮罩）见 §5。
- **API 缺口很小**：`GET /api/config` 已覆盖全部要展示字段；只有「模型显示名」「config.yaml 路径」两个可选后端补充，其余全部前端静态映射/降级（§7）。

---

## 1. 读过的 dsh 源码清单（相对路径，只读）

**设置壳与通用段（ui-settings-general）**
- `ui-settings-general/src/client/SettingsRoot.tsx` — 面板壳（导航轨 + 内容列 + 遮罩 + Esc 关闭）
- `ui-settings-general/src/client/SettingsRoot.module.css` — 面板/导航/头/options 的全部几何与 token
- `ui-settings-general/src/client/GeneralSection.tsx` + `GeneralSection.module.css` — 通用段 = 单列叠「行」槽，末行去分隔线
- `ui-settings-general/src/client/chrome.tsx` — 触发行（⚙ 图标 + 「设置」）与面板标题/关闭文案
- `ui-settings-general/src/client/chrome.module.css`（触发标签样式）
- `ui-settings-general/src/client/locales.ts` — 壳文案（设置/关闭/打开配置文件/通用设置）
- `ui-settings-general/src/client/index.ts` — 段注册（general order 0）+ 壳 slots 装配
- `ui-settings-general/src/client/shell-contract.ts` — 导航行投影契约（id/order/label）
- `ui-settings-general/src/client/SettingsDocumentAction.tsx` + `.module.css` — 头部「打开配置文件」动作行（参考：动作放在内容列头部）

**设置域基座（ui-settings）**
- `ui-settings/src/client/contract/slots.ts` — 设置槽契约（trigger/header/action/close/section/general.item），说明「段」的 owner 只收到 `close`，数据全走自己的注入

**模型段（ui-settings-models）**
- `ui-settings-models/src/client/ModelsSection.tsx` — provider 行卡列表：rowCard / rowHead / rowIdentity（名称+自定义标签+凭证点）/ rowActions（编辑/删除）
- `ui-settings-models/src/client/ModelsSection.module.css` — 行卡、按钮、输入、select、模型目录的全部样式
- `ui-settings-models/src/client/locales.ts` — 模型段中文文案
- `ui-settings-models/src/client/index.ts` — 段注册（models order 10）

**模型选择器（ui-model-selection）**
- `ui-model-selection/src/client/ModelSelect.tsx` — 二级下拉的全部交互（打开/关闭/键盘/Esc/选中）
- `ui-model-selection/src/client/ModelSelect.module.css` — chip 触发器 + 菜单 + 分组 + 选中 ✓
- `ui-model-selection/src/client/locales.ts` — 模型选择器中文文案

**主题（ui-theme）**
- `ui-theme/src/styles/design-platform.css` — 全部 `--dsw-static-*` / `--dsw-alias-*` / `--dsw-specific-*` token（暗/浅）
- `ui-theme/src/styles/gradient-shadow-text.css` — `--dsw-shadow-lv1/2/3`、`--dsw-mask-blur`
- `ui-theme/src/styles/scrollbar.css` — 滚动条皮肤（thumb token 绑定约定）
- `ui-theme/src/styles/base.css` — 字体族（`--dsw-font-family` 等，P1 已移植）
- `ui-theme/src/client/AppearanceRow.tsx` + `.module.css` — 外观行（浅色/深色/跟随系统 三立方）
- `ui-theme/src/client/locales.ts` — 外观文案
- `ui-theme/src/boot-theme.ts`（主题切换机制，P1/P2 已研究）

**通用段「行」样式参考（ui-agent-preset）**
- `ui-agent-preset/src/client/AgentPresetRow.tsx` + `.module.css` — 标题+说明+右侧选择胶囊的「行」范式（P3 的只读行照它排）
- `ui-agent-preset/src/client/index.ts` — 段注册（agent-presets order 20，**github.com/shutu-ai/shutu-agent 排除**）

**对照的 github.com/shutu-ai/shutu-agent 现状（只读）**
- `github.com/shutu-ai/shutu-agent/internal/webserver/static/index.html`（#/settings 页骨架、topbar `model-label`/`mode-badge`）
- `github.com/shutu-ai/shutu-agent/internal/webserver/static/app.js`（`loadConfig()`、`renderSettings()` 分组表格、路由）
- `github.com/shutu-ai/shutu-agent/internal/webserver/static/style.css`（:root token 表 + settings 段样式）
- `github.com/shutu-ai/shutu-agent/cmd/pa/webserver.go`（`webConfig()` — GET /api/config 字段）
- `github.com/shutu-ai/shutu-agent/internal/webserver/webserver.go`（`handleConfig`、`requireAuth`）
- `github.com/shutu-ai/shutu-agent/.web-port/P1-spec.md`、`P2-spec.md`（已移植的布局/token/侧栏，P3 引用其主题机制与 token 值）

---

## 2. dsh 设置页一句话要点 + 页面结构（DOM 树草图）

### 2.1 一句话要点

dsh 设置是一个**全屏遮罩 + 居中模态面板**（800×min(800,100vh-48)，r24，`bg-layer-2` + `shadow-lv3`），面板内是「188px 导航轨（图标+文字行，选中实底高亮）+ 内容列（54px 头：标题/动作 + 关闭钮；下方滚动区叠当前段）」，每个「段」是插件注册进 `settings.section` 列表槽的一张页面，导航行就是段列表。

### 2.2 DOM 结构草图（dsh SettingsRoot → github.com/shutu-ai/shutu-agent 映射）

```
body[data-ds-dark-theme]                     ← 主题开关（已有 P1/P2）
└─ #settings.settings-page（github.com/shutu-ai/shutu-agent 沿用 #/settings 路由整页，见 §8）
   └─ .settings-panel                       ← dsh 面板：800px、r24、bg-layer-2、shadow-lv3
      ├─ nav.settings-nav（188px，列，gap18，pad 22/12/0）
      │  ├─ .navTitle「设置」（16/500/24，pad 0 12）
      │  └─ .navList（列 gap4）
      │     ├─ button.navCell[aria-current]  ⚙  通用设置     ← general order 0
      │     ├─ button.navCell.active        ◈  模型         ← models order 10（当前高亮）
      │     ├─ button.navCell               ⚡  能力开关     ← github.com/shutu-ai/shutu-agent 自定义段
      │     └─ button.navCell               🧰  工具         ← github.com/shutu-ai/shutu-agent 自定义段
      └─ .settings-content（列，flex1）
         ├─ .settings-header（54px，justify-between）
         │  ├─ .actions [「打开配置文件」动作 ← 可选，§7] 
         │  └─ button.close ✕（28px r28，hover interactive-bg-hover）
         └─ .settings-options（pad 0 24 24，overflow-y auto）
            └─ （当前段）section.settings-section（max-width 720px，列 gap12）
               ├─ h2.title（16/500/24）
               ├─ p.intro（14/22 label-tertiary）
               ├─ p.notice（12/18 warn；只读提示）
               └─ （段内容：行卡/列表/白名单徽标，见 §3）
```

**要点差异（github.com/shutu-ai/shutu-agent 与 dsh 的取舍）**：

1. **整页 vs 模态**：dsh 是从侧栏脚触发、遮罩盖在工作台上。github.com/shutu-ai/shutu-agent 已是 `#/settings` 路由整页（P2 已做侧栏脚「⚙ 设置」链接 + 顶部「← 返回聊天」）。**P3 建议保留整页路由**，把页面内容做成 dsh 面板观感（居中面板 + 内部两栏），不加遮罩——这保住现有路由/返回/顶部主题切换，改动最小。若追求「连遮罩都像」，可把面板包进 `.overlay`（fixed inset0 + mask + blur2px）并在 Esc/点遮罩关闭，但会绕开「返回」按钮，不推荐首版做。
2. **导航行图标**：dsh 用 16px outline 图标集（ui-primitives）。github.com/shutu-ai/shutu-agent 零依赖 ⇒ 用内联 SVG（1.5px stroke、currentColor）或沿用现有 emoji（⚙/◈/⚡/🧰）；规格只约束几何（16px、gap8、40px 行）。
3. **段列表**：dsh 的「插件」「智能体预设」段是架构排除项（ADR D-WEB2-I），P3 不显示；用「能力开关」「工具」两段承接 github.com/shutu-ai/shutu-agent 的 19 个 `*_enabled` 与工具白名单。

### 2.3 行/卡结构范式（dsh → P3）

**范式 A — 通用段「行」（GeneralSection.item，参考 AgentPresetRow/AppearanceRow）**：

```
.row（flex center gap8，pad 16 0，border-bottom l2；段末行去分隔线）
├─ .rowText（列 gap4，flex1）
│  ├─ .title「外观」（14/400/22 label-primary）
│  └─ .desc「…说明…」（12/400/18 label-tertiary）
└─ .control（右侧控件：选择胶囊/立方组/开关徽标）
```

**范式 B — 模型段 provider 行卡（ModelsSection.rowCard）**：

```
ul.rows（列 gap8，mt12）> li.rowCard（border l2，r12，pad 12 14，列 gap12）
├─ .rowHead（flex center gap10）
│  ├─ .rowIdentity（inline-flex gap6）
│  │  ├─ .rowName「DeepSeek」（14/500/22 label-primary）
│  │  ├─ .rowTag「只读」（11px，border l3，r4，label-secondary）   ← github.com/shutu-ai/shutu-agent 加，替代「自定义」标签
│  │  └─ .credentialDot（8px 圆点；configured=绿 success / missing=红 error）← github.com/shutu-ai/shutu-agent 无凭证概念，隐藏
│  └─ .rowActions（margin-left auto，gap4）
│     ├─ button.secondaryButton「编辑」（28px r14 12px，disabled）  ← 只读：禁用
│     └─ button.dangerButton「删除」（不存在 → 隐藏）
└─ （展开区 .editor：bg-module-platform r12 pad 14 16 —— github.com/shutu-ai/shutu-agent 不需要，隐藏）
```

**范式 C — 能力开关/工具只读徽标行（github.com/shutu-ai/shutu-agent 自定义，沿用范式 A 的行 + 状态徽标）**：

```
.row > .rowText(title+desc) + .badge（r14 胶囊：开=绿底绿字 / 关=透明+label-caption 字「关」）
```

---

## 3. 各分组面板的逐元素数据映射（字段名 ↔ github.com/shutu-ai/shutu-agent config 键）

数据源：`GET /api/config`（`cmd/pa/webserver.go` `webConfig()`），snake_case；前端 `config` 全局缓存（`app.js` `loadConfig()` 已填）。

### 3.1 段：通用设置（nav「通用设置」）

| 元素（dsh 对应） | dsh 字段/来源 | github.com/shutu-ai/shutu-agent 键 | 说明 |
|---|---|---|---|
| 外观行（AppearanceRow） | 主题偏好（前端 store，localStorage） | 前端 `toggleTheme()` + `localStorage`（P1 已有） | 照 dsh 三立方：浅色/深色/跟随系统；「跟随系统」github.com/shutu-ai/shutu-agent 现为两态切换，**首版可只做浅/深两立方 + 隐藏「跟随系统」**，或加 matchMedia 跟随（P1 已研究，可选） |
| 会话模式 | 无（dsh 是 cordis 预设，不在通用段） | `mode`（standard/minimal/code） | 展示为行：值徽标 + 说明「改 config.yaml 重启生效」；topbar `mode-badge` 已有同数据 |
| Web 服务地址 | 无 | `web_server_addr` | 行：值只读（`http://127.0.0.1:PORT`） |
| 打开配置文件动作 | `settings.action` 头部按钮（Host 打开文件） | **缺**（无后端；见 §7） | 首版隐藏；或降级为只读文案「配置文件：`config.yaml`（项目根，改后重启生效）」 |
| 只读提示 | `readOnly` 状态（writable=false） | 恒真（前端常量） | 段顶部 notice「当前部署的设置文档为只读。」 |

### 3.2 段：模型（nav「模型」）

dsh 是「多 provider 行卡 + 每行可展开编辑」；github.com/shutu-ai/shutu-agent 单 provider ⇒ 一行为「当前模型」。

| 元素（dsh 对应） | dsh 字段 | github.com/shutu-ai/shutu-agent 键 | 说明 |
|---|---|---|---|
| 段标题（h2.title） | `title`=模型 | 固定文案 | — |
| 段引言（p.intro） | `intro` | 固定文案（适配） | 见 §6 |
| 只读提示（p.notice） | `readOnly` | 恒真 | 「当前部署的设置文档为只读。」 |
| 行卡名称（rowName） | `displayName`（适配器目录） | `llm_provider`（deepseek）→ 显示「DeepSeek」 | 静态映射：`deepseek → DeepSeek`；未知 provider 显示原值 |
| 行卡当前模型 | 模型目录 current（provider+model id） | `model`（如 `deepseek-chat`） | 展示为行内次级文本或编辑展开区的一行「模型 ID：deepseek-chat」 |
| 行卡 provider 标签 | `rowTag`「自定义」 | **不用** | 改放「只读」tag（见 §2.3 范式 B） |
| 凭证点（credentialDot） | 密钥 configured/missing | **缺/隐藏** | github.com/shutu-ai/shutu-agent 密钥走 env、config 无密钥字段 ⇒ 隐藏 |
| 编辑/删除按钮 | edit/remove（可写） | — | 只读 ⇒ 「编辑」disabled（或隐藏）、「删除」不渲染 |
| 模型显示名/描述 | `model.name` / `model.description` | **缺**（见 §7） | 前端静态映射 `deepseek-chat→DeepSeek Chat` 等，兜底显示原始 id |
| base_url | ProviderEditor 字段 | `base_url` | 行或展开区只读展示；空则显示「（默认）」 |
| 模型目录编辑（ModelListEditor） | 目录 CRUD | **缺/无** | github.com/shutu-ai/shutu-agent 无模型列表配置 ⇒ 整块隐藏（§8） |
| 推理等级（effort） | ModelReasoningEffort | **缺/无** | 无 reasoning 元数据 ⇒ 隐藏（§4） |

### 3.3 段：能力开关（nav「能力开关」，github.com/shutu-ai/shutu-agent 自定义）

遍历 `Object.keys(config)` 中以 `_enabled` 结尾的键（`cmd/pa` 共 19 个），每键一行「中文名 + 开/关徽标」。

| config 键 | 中文标签（前端静态映射） | 默认 |
|---|---|---|
| `terminal_enabled` | 终端（terminal） | 关 |
| `fs_enabled` | 文件系统（fs） | 关 |
| `fs_search_enabled` | 全文检索（fs_search） | 关 |
| `ralph_enabled` | Ralph 循环（ralph） | 关 |
| `workflow_enabled` | 工作流（workflow） | 关 |
| `kb_enabled` | 知识库（kb） | 关 |
| `jobs_enabled` | 后台任务（jobs） | 关 |
| `subagent_enabled` | 子代理（subagent） | 关 |
| `web_enabled` | 联网（web） | 关 |
| `eval_enabled` | 评测（eval） | 关 |
| `code_enabled` | 代码执行（code） | 关 |
| `interact_enabled` | 交互确认（interact） | 关 |
| `mcp_enabled` | MCP（mcp） | 关 |
| `skill_enabled` | 技能（skill） | 关 |
| `schedule_enabled` | 定时（schedule） | 关 |
| `plan_enabled` | 计划（plan） | 关 |
| `spill_enabled` | 溢出（spill） | 关 |
| `compaction_enabled` | 压缩（compaction） | 关 |
| `multimodal_enabled` | 多模态（multimodal） | 关 |

> 键不固定：**建议按 `Object.keys(config)` 动态扫描 `_enabled` 后缀**（现 `renderSettings` 已这么做），未知键用 `key.replace(/_enabled$/,'')` 兜底显示，避免后端加能力开关时前端失配。

### 3.4 段：工具（nav「工具」，github.com/shutu-ai/shutu-agent 自定义）

| 元素 | github.com/shutu-ai/shutu-agent 键 | 说明 |
|---|---|---|
| 段标题 | 固定文案「工具白名单」 | — |
| 计数行 | `tools_enabled_count` | 「已启用 N 个工具」 |
| 工具列表 | `tools_enabled`（数组，后端截断至 30 项 + `…` 尾标） | 每工具一行/一徽标只读；尾部 `…` 表示已截断 |

### 3.5 段级缺失汇总（「缺」清单）

- `model.name` / `model.description`（模型显示名/描述）——**缺**
- 模型列表/目录（可切换的候选模型）——**缺（无模型列表配置）**
- 推理等级（reasoning effort 元数据）——**缺**
- 凭证状态（API key configured/missing）——**缺（密钥走 env，不展示）**
- 配置文件路径（`settings.action` 打开文件所需）——**缺**
- 插件段（ui-settings-plugins）、智能体预设段（ui-agent-preset）——架构排除，P3 不显示

---

## 4. 模型选择器交互细节 + 适配方案（github.com/shutu-ai/shutu-agent 只读时怎么呈现）

### 4.1 dsh 交互细节（ModelSelect）

- **触发器**：输入栏一个 28px 高、r24 圆角 chip：`[模型名] · [推理等级] [▼]`，13/20/500 `label-secondary`，hover `interactive-bg-hover`，max-width `min(360px,45cqw)`，chevron 旋转 180° 展开（120ms ease）；`disabled` 时 `label-dimmed`。
- **打开**：点 chip → 每次都 reload 目录；点外部 / Esc / blur 关闭；Esc 先退子页再关闭。
- **根菜单**（Pane=root）：两行 cell（40px，r10）——「模型」右显示当前模型名 + 右 chevron；「推理等级」右显示当前等级 + 右 chevron（无 reasoning 元数据则整行不渲染）。
- **模型列表**（Pane=model）：按 provider 分组（sticky 组头 12/500 三级色），每模型一行 `menuitemradio`（`aria-checked`），名称 14/500 + 可选描述 12px 三级色，**当前模型右侧 ✓**（`IconCheckOutline16`，选中不是底色而是尾勾），点选即提交（相同则直接关）。空态「没有可用的模型。」
- **加载/失败**：列表内 status 行「正在刷新模型列表…」；加载失败红色 strip +「重试」。
- **选中语义**：radio（`menuitemradio` + `aria-checked`），不是复选。

### 4.2 github.com/shutu-ai/shutu-agent 只读适配方案（推荐：单模型单组 + 全禁用）

github.com/shutu-ai/shutu-agent 无模型列表、无 provider 目录、无 reasoning ⇒ 不引入「真实切换」。**两种呈现，二选一或叠加**：

**方案 A（推荐，最贴 dsh 观感）——只读下拉「只看得见选中的那个」**：
- 触发器：展示当前 `model · provider`（如 `deepseek-chat · DeepSeek`），复用 P2 顶部 `model-label` 的拼法（`config.model + " · " + config.llm_provider`）；`disabled`（`label-dimmed` + 不触发打开），chevron 保留但整 chip 灰化。
- 若打开：菜单渲染为**单组「DeepSeek」+ 单模型行**（`aria-checked=true` + 尾 ✓），模型名用显示名映射，描述行放「当前模型来自 config.yaml，修改后重启生效」（12px 三级色）；全部 `disabled`，不响应点击。加载/失败/空态逻辑可省略（数据来自已缓存的 `/api/config`，不异步）。
- 语义：即便只读，保留 `aria-haspopup="menu"` + `menuitemradio` + `aria-checked`，方便无头测试/读屏。

**方案 B（更省）——模型段内「当前模型」行卡 + 禁用编辑**：
- 不画下拉；模型段 rowCard 直接展示：名称 `DeepSeek`、次级行 `模型 ID：deepseek-chat`、`API 地址：…`、`会话模式：standard`；右侧「编辑」disabled。
- 交互为 0（无点击路径），最诚实。

**建议**：首版做 **B（模型段静态行卡）**，把 **A 的下拉 chip 作为可选项**——因为 github.com/shutu-ai/shutu-agent 对话输入栏目前没有模型 chip（`#model-label` 只在 topbar 展示），为了 P3 最小闭环先不做输入栏芯片，只在设置模型段展示（§8）。若要同时在输入栏显示 chip，把方案 A 的只读触发放入 composer 即可（零后端）。

---

## 5. 暗/浅色主题 token（ui-theme/design-platform.css + gradient-shadow-text.css 解析）

主题机制沿用 P1/P2：默认暗色，`body[data-ds-dark-theme]` 切换；github.com/shutu-ai/shutu-agent `style.css :root` 已移植暗色表。下表为**设置页实际用到的 token 最终色值**（未在 P1/P2 列出或需核对的部分全列出）：

### 5.1 面板 / 遮罩 / 阴影

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-alias-bg-layer-2` | `#2C2C2E` (44,44,46) | `#FFFFFF` (255,255,255) | 面板底 |
| `--dsw-alias-bg-layer-1` | `#232324` (35,35,36) | `#FFFFFF` | 输入框底 |
| `--dsw-alias-bg-module-platform` | `#353638` (53,54,56) | `#F5F6F7` (245,246,247) | 编辑器面/选择胶囊底/选中立方底 |
| `--dsw-alias-bg-mask-1` | `rgba(0,0,0,0.5)` | `rgba(0,0,0,0.24)` | 遮罩底（若做模态） |
| `--dsw-mask-blur` | `blur(2px)` | 同左 | 遮罩模糊 |
| `--dsw-shadow-lv3` | `0 0 1px 0 rgba(0,0,0,0.2), 0 0 4px 0 rgba(0,0,0,0.02), 0 12px 32px 0 rgba(0,0,0,0.08)` | 同左 | 面板/菜单浮层阴影 |
| `--dsw-alias-scrollbar-bg-l2` | `#545557` | `#E5E5E5` | 面板滚动条拇指（l2 面） |
| `--dsw-alias-scrollbar-hover-l2` | `#65676B` (101,103,107) | `#D4D4D4` (212,212,212) | 拇指 hover |

### 5.2 导航（navCell）

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-specific-sidebar-nav-item-hover` | `#2C2C2E` (44,44,46) | `#F1F3F5` (241,243,245) | 导航行 hover |
| `--dsw-specific-sidebar-nav-item-active` | `#43454A` (67,69,74) | `#EBEEF2` (235,238,242) | 导航行选中底（P2 §4.1 已列，设置导航同用） |
| `--dsw-specific-sidebar-nav-item-active-accent` | `#353638` | `#E4EDFD` (228,237,253, deepseek-100) | 激活项强调（若做左色条/图标着色） |

### 5.3 文本 / 边框 / 交互

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-alias-label-primary` | `#F9FAFB` | `#0F1115` | 标题/值/选中立方字 |
| `--dsw-alias-label-secondary` | `#CFD3D6` | `#61666B` | 标签/次要按钮 |
| `--dsw-alias-label-tertiary` | `#ADB2B8` | `#81858C` | 说明/引言/占位 |
| `--dsw-alias-label-caption` | `#81858C` | `#ADB2B8` | chevron/徽标次级 |
| `--dsw-alias-label-dimmed` | `#43454A` | `#E1E5EE` | disabled 文本 |
| `--dsw-alias-label-primary-foreground` | `#0F1115` | `#FFFFFF` | 主按钮（primary fill 底）上的字 |
| `--dsw-alias-border-l1` | `rgba(255,255,255,0.06)` | `rgba(0,0,0,0.04)` | 最细分隔 |
| `--dsw-alias-border-l2` | `rgba(255,255,255,0.12)` | `rgba(0,0,0,0.1)` | 行卡描边/输入框 |
| `--dsw-alias-border-l3` | `rgba(255,255,255,0.16)` | `rgba(0,0,0,0.12)` | 标签描边/focus ring |
| `--dsw-alias-border-inverted` | `rgba(255,255,255,0.06)` | `rgba(0,0,0,0)` | 菜单/模态描边（浅色透明） |
| `--dsw-specific-menu` | `#353638` | `#FFFFFF`（bg-layer-3=bluish-00） | 下拉菜单面 |
| `--dsw-alias-interactive-bg-hover` | `rgba(255,255,255,0.08)` | `rgba(38,49,72,0.06)` | 行/钮/cell hover |
| `--dsw-alias-interactive-bg-hover-solid` | `#353638` | `#F1F3F5` | 次级按钮 hover（实底） |
| `--dsw-alias-interactive-bg-hover-danger` | `rgba(242,90,90,0.15)` | `rgba(236,19,19,0.05)` | 删除钮 hover |

### 5.4 状态（开/关/错误/成功）

| token | 暗色 | 浅色 | 用途 |
|---|---|---|---|
| `--dsw-alias-state-success-primary` | `#22C55E` | 同左 | 开/已配置 绿 |
| `--dsw-alias-state-warn-label` | `#DD8629` | 同左（amber-600） | 只读/提示 琥珀字 |
| `--dsw-alias-state-error-primary` | `#F25A5A` (red-400) | `#EC1313` (red-600) | 错误/缺失 红 |
| `--dsw-alias-state-business-primary` | `#679EFE` (deepseek-400) | `#4176E6` (deepseek-500) | 品牌/强调（可选） |

> 能力开关徽标建议：**开** = `border-l2` + 绿字 `state-success-primary`（或 success-tertiary 底 `#1B1B1C`? 不——dark 用 `#E6FAED`(green-100) 底太亮，**推荐边框+绿字**即可）；**关** = 透明底 + `label-caption` 字「关」。开/关徽标 14px r14 胶囊。

---

## 6. 完整中文文案清单（导航名 / 分组名 / 标签 / 说明）

### 6.1 设置壳（dsh `settings` ns，照抄）

| 键 | 中文 |
|---|---|
| trigger | 设置 |
| title | 设置 |
| close | 关闭 |
| openDocument | 打开配置文件 |
| openDocument.error | 无法打开配置文件 |
| general.nav | 通用设置 |

### 6.2 模型段（dsh `settings.models` ns，照抄 + 适配标注）

| 键 | dsh 中文 | github.com/shutu-ai/shutu-agent 采用 |
|---|---|---|
| nav / title | 模型 | ✓ 同 |
| intro | 填入各提供方的 API 密钥即可使用其模型。 | 改为「仅展示当前使用的模型与提供方。修改 `config.yaml` 后重启生效。」 |
| readOnly | 当前部署的设置文档为只读。 | ✓ 同 |
| edit | 编辑 | ✓（禁用） |
| editProvider | 编辑 {provider} | （不用，无多 provider） |
| remove / delete* | 删除… | 不用（不渲染） |
| add | 添加提供方 | 不用（无多 provider） |
| provider | 提供方 | ✓ |
| customTag | 自定义 | 改为「只读」tag |
| credentialConfigured / credentialMissing | API 密钥已配置 / 缺失 | 不用（隐藏） |
| models / modelsInherited / modelsCustomized | 模型目录 / 正在使用适配器默认模型 / 已自定义模型目录 | 不用（无目录编辑） |
| modelId / modelName / contextWindow / maxTokens / addModel / removeModel / modelsEmpty | 模型目录编辑相关 | 不用（隐藏整块） |
| baseUrl | API 地址 | ✓（只读展示） |
| baseUrlDefault | 提供方默认 | ✓（空值时） |

### 6.3 模型选择器（dsh `model` ns，照抄）

| 键 | 中文 |
|---|---|
| trigger.fallback | 选择模型 |
| trigger.aria | 选择模型，当前 {model} |
| menu.aria | 模型与推理等级 |
| menu.model | 模型 |
| menu.effort | 推理等级 |
| status.loading | 正在刷新模型列表… |
| error.action | 模型操作失败：{message} |
| action.reload | 重新加载 |
| empty.models | 没有可用的模型。 |
| empty.efforts | 当前模型未提供推理等级。 |
| command.description | 选择本会话使用的模型 |

### 6.4 外观（dsh `settings.theme` ns，照抄）

| 键 | 中文 |
|---|---|
| appearance.title | 外观 |
| appearance.light | 浅色 |
| appearance.dark | 深色 |
| appearance.system | 跟随系统 |

### 6.5 github.com/shutu-ai/shutu-agent 自定义（非 dsh，需自拟）

| 位置 | 文案 |
|---|---|
| 段导航 | 能力开关 / 工具 |
| 通用段·模式行 | 会话模式 · 值 `standard/minimal/code` · 说明「修改 config.yaml 后重启生效」 |
| 通用段·Web 行 | Web 服务地址 · 值 |
| 通用段·配置行 | 配置文件 · 值 `config.yaml`（项目根）· 说明「修改后重启生效，无运行时热改（ADR D-WEB2-D）」 |
| 能力段 | 标题「能力开关」· 引言「各能力默认关闭（D10），启用需在 config.yaml 打开对应开关」· 每行 中文名 + `开`/`关` 徽标 |
| 工具段 | 标题「工具白名单」· 计数「已启用 {N} 个工具」· 说明「白名单为空/为只读工具时提示」 |
| 模型段·模型行 | 「模型 ID」+ 值；「API 地址」+ 值（空显示「提供方默认」） |
| 只读通用提示（段顶） | 「修改 config.yaml 后重启生效（无运行时热改）。」 |

---

## 7. github.com/shutu-ai/shutu-agent API 缺口清单（后端补 vs 前端降级）

### 7.1 建议后端补（小、可选，非首版阻塞）

| 缺口 | 用途 | 建议 |
|---|---|---|
| `model_display_name`（+可选 `model_description`） | 模型段/选择器显示友好名 | `webConfig()` 加一个静态映射或新字段；**前端可降级**：静态映射 `deepseek-chat→DeepSeek Chat`、`deepseek-reasoner→DeepSeek Reasoner`，兜底显示原始 id |
| `config_file_path` | 「配置文件在哪」的真实路径 | `webConfig()` 加字段；**前端可降级**：固定文案「config.yaml（项目根）」 |

> 其余字段 `model`、`llm_provider`、`mode`、`base_url`、19 个 `*_enabled`、`tools_enabled_count`、`tools_enabled`、`web_server_addr` **全部已就绪**，P3 无需后端改动即可渲染。

### 7.2 纯前端降级 / 常量（不用后端）

| 项 | 处理 |
|---|---|
| 只读标记（writable） | 前端常量 `true`（dsh 由 `!writable` 驱动禁用；github.com/shutu-ai/shutu-agent 恒只读） |
| 主题（外观行） | 复用 `toggleTheme()` + localStorage（P1/P2 已有）；设置页加三立方行 |
| 能力开关中文名 | 前端静态映射表（§3.3），未知键 `key.replace(/_enabled$/,'')` 兜底 |
| 工具白名单渲染 | 前端遍历 `tools_enabled`（后端已截断至 30 + `…`） |
| 模型显示名 | 前端静态映射（§7.1 降级） |
| 模型选择器 | 不异步、不拉取目录——直接渲染已缓存 config 的单模型（§4.2 方案 A/B） |

### 7.3 架构排除（不补）

- 模型列表/切换、provider 目录、推理等级 → github.com/shutu-ai/shutu-agent 无此配置（config.model 单值），后端不造列表。
- 插件段、智能体预设段 → ADR D-WEB2-I 排除，P3 不显示。
- 配置热改/写接口 → ADR D-WEB2-D 明确只读；前端只给「重启生效」提示。

---

## 8. P3 最小闭环建议（首版做什么、什么隐藏）

### 首版做（一个提交，`#/settings` 重构）

1. **面板壳**：`#settings` 整页改为 dsh 两栏面板观感（800px 居中、r24、`bg-layer-2`、`shadow-lv3`、内部 `nav(188px) + content`），顶部保留「← 返回聊天」与主题切换；面板头标题「设置」+ 关闭（关闭 = 返回聊天）。
2. **四个导航段**：通用设置 / 模型 / 能力开关 / 工具；点击切换内容列（激活行 `nav-item-active` 实底高亮 + `aria-current`），Esc/返回可退（可选）。
3. **通用设置段**：外观三立方行（浅/深/跟随系统可选）、会话模式行、Web 地址行、配置文件说明行——全部只读 + 「修改 config.yaml 后重启生效」。
4. **模型段**：单 provider 行卡（范式 B）——`DeepSeek` + 「只读」tag + `模型 ID：deepseek-chat` + `API 地址：…` + `会话模式：…`，「编辑」disabled；段顶只读 notice。
5. **能力开关段**：动态扫描 `*_enabled` → 中文名 + 开/关徽标（§3.3 映射表）。
6. **工具段**：`tools_enabled_count` 计数 + `tools_enabled` 列表（含尾部 `…`）。
7. **文案**：全部中文（§6），静态 JSON/常量放 `app.js` 顶部便于维护。

### 首版隐藏 / 不做

- **输入栏模型 chip**（方案 A 下拉）：github.com/shutu-ai/shutu-agent 输入栏无模型座位，首版不做；若做，用 §4.2 方案 A 只读 chip（零后端）。
- 模态遮罩/浮层形态（保留整页路由）。
- 模型目录编辑、添加/删除 provider、fetch models、推理等级、凭证点。
- 「打开配置文件」动作（无后端）——降级为配置说明行。
- 「跟随系统」立方（如 P1 主题切换未支持）——首版只做浅/深。

### 验收建议（对齐 dsh 观感）

- 面板尺寸/圆角/阴影、导航轨 188px、行卡 12px 圆角描边、胶囊按钮 r14、图标 16px 对齐 P1/P2 已移植 token。
- 深/浅主题各看一遍（`body[data-ds-dark-theme]` 切换），确认无硬编码浅色字。
- `GET /api/config` 数据全映射（§3），无 token/key 泄露（沿用 D-WEB2-D）。
- `app.js` 路由 `#/settings` 与返回聊天、主题切换均正常。

---

## 附：实现落点（github.com/shutu-ai/shutu-agent 文件）

| 文件 | 改动 |
|---|---|
| `internal/webserver/static/index.html` | `#settings` 块重写为面板骨架（nav + content 容器） |
| `internal/webserver/static/style.css` | 新增 settings 面板/导航/行卡/徽标样式（token 见 §5，可追加在 settings 段） |
| `internal/webserver/static/app.js` | 重写 `renderSettings()`：静态段配置数组 + 渲染函数；复用 `loadConfig()` 缓存 |
| `cmd/pa/webserver.go` | （可选）`webConfig()` 加 `model_display_name`、`config_file_path` |
