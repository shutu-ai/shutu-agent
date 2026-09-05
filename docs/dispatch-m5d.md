# M5d 实施派发消息（控制会话 → 实施会话）——技能 skill

> 状态：已派发 2026-08-19（M5 拆四段：M5a ✅ → M5b ✅ → M5c ✅ → M5d 技能；本文件为第四段，拆为 M5d-1/M5d-2 顺序派发）· 用法：把下文整段粘贴给新开的实施会话（M5d-1 见 dispatch-m5d-1.md）。

---

请阅读 `D:\dev-projects\Agent\shutu-agent\Agent.md`、`docs/design.md`（§4 pre-step 扩展点、§7 提示词分节、§10 D1/D3/D4/D10）**和 `docs/decisions/2026-08-18-m5-agent-core.md`（M5 主 ADR，本段对应"决策 ④"）**，并通读参考源码 `D:\dev-projects\Agent\deepseek-harness\packages\skill\`（重点：`skill/`（注册表 `src/index.ts`）、`skill-filesystem/`（本地发现）、`tool-skill/`（目录注入 + `skill` 加载工具））以及 `docs/subsystems/skills.md`（Provider 契约、发现优先级、技能身份、目录与工具契约），按设计基线实现 **M5d 技能**（M5 第四段；本段验收标准见下，M5 完整验收标准见 Agent.md 第 4 节）。

**前置依赖**：本段依赖 **M5b 的 pre-step 扩展点**（目录注入挂在 pre-step 注入器上）。若 M5b 验收后 `PreStep` 接口有调整，以**当前代码**为准。

**M5d 范围（只做技能，M5 收尾段）**：

1. **`internal/skill` 包——技能注册表（Service 定义，多 Provider）+ 文件系统 Provider（默认）+ 目录/加载工具（Consumer）**：
   ```go
   type Candidate struct {
       Name        string   // kebab-case（^[a-z0-9]+(-[a-z0-9]+)*$）
       Description string
       Source      string   // project-dsh | project-agents | user-dsh | custom | ...
       Rank        int      // 低 rank 优先（同名裁决）
       Path        string   // 绝对路径（文件系统 provider）
   }

   type Definition struct {
       Name, Description, Content string
       Source, Path string
       ModelInvocable, UserInvocable bool
   }

   type Provider interface {
       Name() string
       List(ctx context.Context) ([]Candidate, error)
       Get(ctx context.Context, c Candidate) (*Definition, error)
   }

   type Registry interface {
       RegisterProvider(p Provider) error
       List(ctx context.Context) ([]Candidate, error)          // 合并、按 rank 裁决、按 name 排序
       Get(ctx context.Context, name string) (*Definition, error)
       Close() error
   }
   ```
   - **文件系统 Provider（默认）**：扫描根按 rank（对照 `docs/subsystems/skills.md` 发现优先级）：
     - 100 `project-dsh`：`<projectRoot>/.dsh/skills`（projectRoot = 最近的含 `.git` 祖先，无则 cwd）
     - 200 `project-agents`：`<projectRoot>/.agents/skills`
     - 300 `custom`：`skill.dirs`（config 自定义目录）
     - 400 `user-dsh`：`<userHome>/.dsh/skills`
   - **技能身份**：kebab-case 名；目录束 `<name>/SKILL.md` 或平铺 `<name>.md`；**不递归发现**（对照 dsh）。frontmatter 支持 `disable-model-invocation` / `user-invocable`（缺省均 true）。
   - **同名裁决**：低 rank 优先，同 rank 按 provider 注册序、再本地序；`Get` 加载时若 name 与 candidate 不再匹配则拒绝。
   - **目录注入（D3）**：会话开始时（pre-step 注入器，M5b 统一机制）把完整技能**目录**（排序后的 `name + description`；每条 description 按 `description_max_chars` 截断，不塞正文/路径/来源）注入为上下文消息，并落 `skill/catalog` 事件。目录列表不整体截断。目录变更由组合根按需重读（下一次 pre-step 重取；不引入文件监视）。
   - **Consumer（工具）**：`skill_load(name)`——校验 kebab-case → 查目录 → 加载完整正文返回模型（`<skill_content>`，正文有长度上限防超长注入）。D7 校验；D10 默认关。

2. **事件（D3，`internal/session` 新增，log-only）**：`EventSkillCatalog = "skill/catalog"`、`EventSkillLoad = "skill/load"` + 载荷构造 `NewSkillCatalog/NewSkillLoad`（条目数/版本；技能名/来源/正文摘要）。`DeriveHistory` 视为不透明数据。`skill_load` 结果经 `tool/result` 落日志（D3 满足）。

3. **config（`internal/config` 扩展）**：
   ```yaml
   skill:
     enabled: false
     dirs: []
    description_max_chars: 500
   ```
   `skill.enabled` 单一开关（D10）：false ⇒ skill 工具不注册、不进白名单、目录注入器不注册、组合根不初始化注册表。

**决策记录（必交）**：M5 主 ADR `docs/decisions/2026-08-18-m5-agent-core.md` 决策 ④ 已写好（本段）。实施中若有偏离（如发现优先级、frontmatter 解析），**更新该 ADR 对应小节**并说明。

**约束**（严格遵守 design.md 第 10 节 D1–D10）：

- 不改 loop 的 turn/step 结构（D4）；目录注入走 pre-step 注入器（M5b）。
- **技能正文安全**：技能是本地可信文件，加载后作为模型指令输入，**不执行**；`skill_load` 返回正文有长度上限（`description_max_chars` 之外再设 `body_max_chars`，默认 8000，超出截断）。
- **明确不做（本段，dsh 裁剪）**：scope 分层（无插件系统）、文件监视自动失效、远程 Provider、打包 badge 技能（后续可用 `skill.dirs` 指向项目内目录实现）、子代理/压缩（已在前段验收）。
- `skill.enabled` 默认关闭（D10）。
- 保持 CGO-free；**不新增任何第三方依赖**；Go 沙箱绕行沿用项目内缓存。
- 原有测试必须保持绿色。

**参考源码**：`D:\dev-projects\Agent\deepseek-harness\packages\skill\`（注册表、文件系统发现、工具；**只借鉴思路与契约，不照搬 TS 代码**）。`docs/subsystems/skills.md` 的 Provider 契约、发现优先级、目录与工具契约。

**自测（全部通过后提交，提交信息含 M5d）**：`go vet ./...`、`go test ./...`、`go build ./...`。新增测试至少覆盖：文件系统发现（目录束 + 平铺 + 非递归 + frontmatter 解析）、同名 rank 裁决、`Get` 加载完整正文 + name 失配拒绝、目录完整注入且每条 description 有界（`description_max_chars` 截断）+ `skill/catalog` 事件、`skill_load` 工具（kebab-case 校验、正文长度上限、`tool/result`）、`skill/*` 事件类型可落日志、skill 默认关闭（enabled=false 不初始化）。

**完成报告**：改动文件清单、实现决策、测试结果、提交 hash、对 M5 主 ADR 的更新说明（如有）。提交后报告，不要等待控制会话确认——报告即交接。
