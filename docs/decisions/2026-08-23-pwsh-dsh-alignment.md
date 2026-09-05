# 2026-08-23 pwsh 工具对齐 dsh（tool-pwsh 移植�?
状态：实施完成
关联：M9（持久终�?seam�?026-08-20-m9-terminal.md）、M5a（jobs 心智）、M3（run_command 执行类工具）、dsh 对齐（工具改�?bash/pwsh/...�?
## 背景

- M9 落地�?`pwsh` 工具�?*持久 shell 会话**：首用自动起 cmd.exe（可配置 Pwsh/Git Bash/WSL/Cmd），每次调用�?stdin、靠 idle 静默判定就绪、返回视口输出�?- dsh 的默�?`pwsh` 工具（`packages/shell/tool-pwsh` + `pwsh-local`）语义完全不同：**每次调用起全新进�?* `pwsh -NoLogo -NoProfile -NonInteractive -Command <�?`，无跨调用状态；参数�?command/description/timeoutMs/workdir/run_in_background；输�?stdout + `[stderr]` �?+ `[exit code: N]` / `[timed out after Nms]` 标记；非零退出是**正常结果**（带标记），只有基础设施故障才是错误�?- 项目�?"dsh 对齐"（工具改�?bash/pwsh/read/write/edit/grep/glob/run_code），�?`pwsh` 的行为仍�?M9 持久会话，与 dsh �?pwsh 契约不一致�?- 用户拍板�?026-08-21）：**照抄 dsh 默认 pwsh**——每次调用全�?pwsh 进程；M9 持久 Session 保留�?`/term` REPL �?Web 终端设置，不删除�?
## 决策

### D-PWSH-1 pwsh 工具改为全新进程（dsh tool-pwsh 移植�?
- 新实现位�?`internal/tools/pwsh.go`（与 run_command 同包，复�?scrubEnv/monitorCtx/killTree/prepareProcessGroup 基础设施），模型工具名仍�?`pwsh`�?- 每次调用：`pwsh -NoLogo -NoProfile -NonInteractive -Command <编码 preamble + 命令>`；命令串�?*单个 argv 元素**（无中间 shell、无引号转义层，dsh 同款）�?- 编码 preamble（dsh ENCODING_PREAMBLE）：`[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = ...; ` —�?Windows PowerShell 5.1（pwsh 不可用时的回退可执行文件）默认�?OEM 代码页，preamble 固定 UTF-8 输出�?- 环境：scrubbed 父环境（凭证形状变量删除，照 run_command�? 覆盖 `NO_COLOR=1` / `PAGER=cat` / `GIT_PAGER=cat`（父环境已有同名值时保留父值，dsh 顺序）�?- 可执行文件解析：`pwsh` →（Windows 回退）`powershell.exe`；都找不到时调用报错（fail-closed）�?
### D-PWSH-2 参数面与结果渲染照抄 dsh

- Schema：`command`（必填）、`description`（必填，UI 摘要用）、`timeoutMs`（可选正数）、`workdir`（可选；默认 terminal.workdir，相对路径按它解析）、`run_in_background`（可选布尔；仅当 jobs.enabled 时出现在 schema，jobs 未接线时 D7 直接拒绝）�?- 渲染（dsh renderPwshResult）：stdout 正文；stderr 非空时追�?`[stderr]\n` 段；全空�?`(no output)`；标�?`[timed out after Nms]`（先）、`[killed by signal: X]`（POSIX�? `[exit code: N]`（非零退出；Windows 强杀�?`[exit code: 1]` 无信号标记）�?- 超时：工具默�?120s、上�?600s（dsh 默认/上限），`timeoutMs` 只允许更短；注册表外�?`tools.timeout` 是硬边界，两者先到者杀进程树并�?`[timed out after Nms]` **正常结果**上报——只有上游取消（用户 stop）才是错误（`pwsh: interrupted`）�?
### D-PWSH-3 run_in_background �?jobs.Registry

- `run_in_background: true` �?`jobs.Registry.Start`（kind `pwsh`，label=命令，owner=当前会话 id），返回 `started background job pwsh-N; observe with job_status or job_read, await with job_wait, stop with job_cancel`�?- 作业输出上限 64KiB（dsh maxOutputBytes 默认）；job_cancel/Close 通过 Cancel 钩子杀进程树；作业结果�?dsh processOutcome：完成（detail `exit code: N`，非零也算完成）/ 被杀（killed）�?- 后台作业�?job/start 事实不单独发（D3）：pwsh 调用�?tool/start + tool/result 已携带完整模型可见文本；job/status、job/done 由观察工具（job_status/job_wait/job_cancel/job_read）照常发�?
### D-PWSH-4 M9 持久 Session 保留�?/term 专用

- `internal/terminal` �?Session/缓冲/就绪判定全部保留；`TerminalAccess`（单活跃会话 + owner 围栏）只�?`/term` REPL 命令使用�?- 删除 `internal/terminal/tools.go` 的模型工具面（TerminalTools/PwshTool/视口截断）；`ErrNoActive` 移入 session.go，GetActive 提示改为 "start one with /term start"�?- 模型工具不再触发 terminal/start、terminal/stop 事件；`terminal_shell` Web 设置只配�?`/term` 会话�?shell�?- 接线不变：`terminal.enabled`（默认开，dsh 对齐 opt-out）仍�?pwsh 工具 + `/term` 的唯一总开关，白名�?`terminalToolNames = ["pwsh"]` 照旧�?
## 影响

- 模型可见变化：pwsh 不再有跨调用状态（cwd/环境/REPL 上下文），需要状态的交互式会话走 `/term`；调用须�?`description`�?- minimal 预设 persona 同步更新�?persistent shell: pwsh" �?"each call runs in a fresh pwsh process"）�?- Web UI 无需改动：pwsh 行已�?bash 家族终端卡（summary 优先 description、退 command）�?- 测试面：`internal/tools/pwsh_test.go`（无 pwsh 环境自动 skip，照 dsh 惯例）；`cmd/sta/terminal_test.go` 重写为组合测试（注册门、全新进程无状态、退出码标记、后台作业�?term 冒烟）�?
## 验收标准（全部通过�?
1. `go build ./...`、`go vet ./...`、`go test ./...` 全绿（含新增 pwsh 测试）�?2. pwsh 两次调用无状态传递；workdir 解析（绝�?相对/缺省）正确；凭证环境 scrubbed；UTF-8 输出完好�?3. 非零退�?�?`[exit code: N]` 正常结果；timeoutMs �?tools.timeout 到期 �?`[timed out after Nms]` 正常结果并杀进程树；用户取消 �?错误�?4. run_in_background：jobs 接线�?schema 可见且返�?`pwsh-N` 作业 id，job_wait/job_read/job_cancel 语义正确；jobs 未接线时不宣传且拒绝�?5. terminal.enabled=false �?pwsh 工具、无 /term�?term start/write/stop 冒烟通过，M9 会话与模型工具互不干扰�?
