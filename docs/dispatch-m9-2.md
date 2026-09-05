# M9-2 派发：terminal config + D3 事件 + 模型工具五件套 + /term REPL 命令 + 接线

> 里程碑 M9 持久终端（ADR `docs/decisions/2026-08-20-m9-terminal.md`）。本文件是 **M9-2（收尾）** 契约：config `terminal:` 段、D3 事件、模型工具 `terminal_*` 五件套、`/term` REPL 命令组、组合根接线、冒烟测试。前置：**M9-1 已交付** `internal/terminal`（`NewSession`/`Session`：`Write/Read/Consume/Signal/Close/Status/ID`，`WaitReason` 等；只读 import）。

## 0. 纪律

- **不改 `internal/loop/loop.go` 的 turn/step 结构**（D4）；主循环串行（D5）；**零新第三方依赖**；CGO-free；原有测试全绿。
- **默认关（D10）**：`terminal.enabled=false` 时无工具、无命令、无副作用。
- 凭证 env-only 纪律延伸：会话子进程环境 scrubbed（M9-1 已实现）。
- 每模块阶段提交（commit message 前缀 `M9-2`）。

## 1. 范围

**做**：
1. config `terminal:` 段（enabled 默认 false / shell / scrollback / idle / timeout / max_concurrent_sessions）+ config.yaml 注释。
2. D3 事件：`terminal/start`、`terminal/stop`（只记元数据，**不落输出正文**，照 job/* 模式）。
3. 模型工具五件套：`terminal_start` / `terminal_write` / `terminal_read` / `terminal_signal` / `terminal_stop`（照 job_* 同款）。
4. `/term` REPL 命令组（start / write / read / signal / stop）。
5. 组合根 `registerTerminal()`（enabled 时注册）+ printHelp 增行。
6. 冒烟测试。

**不做（本段）**：多会话并发（单活跃会话，owner 校验）；terminal 输出落会话日志正文（只记元数据）。

## 2. config 契约（internal/config）

```go
// 新增：
type TerminalConfig struct {
    Enabled              bool   `yaml:"enabled"`    // 默认 false（D10）
    Shell                string `yaml:"shell"`      // 默认 ""（→ cmd.exe）
    Args                 []string `yaml:"args"`
    Workdir              string `yaml:"workdir"`    // 默认 ""（继承）
    ScrollbackMaxBytes   int    `yaml:"scrollback_max_bytes"` // 默认 65536
    ScrollbackLines      int    `yaml:"scrollback_lines"`     // 默认 2000
    ReadIdleMS           int    `yaml:"read_idle_ms"`         // 默认 500
    ReadTimeoutMS        int    `yaml:"read_timeout_ms"`      // 默认 30000
    MaxConcurrentSessions int   `yaml:"max_concurrent_sessions"` // 默认 1（本段恒 1）
}
// Config 增加 Terminal TerminalConfig `yaml:"terminal"`
```
- applyDefaults：enabled 默认 false、其余默认如上。
- config.yaml 增 `terminal:` 段注释（含"默认关 D10 / 会话环境 scrubbed（凭证不继承）"）。

## 3. 组合根接线契约（cmd/sta）

- `app` 增加 `termSess *terminal.Session`（单活跃会话）+ `termOwner string`（拥有它的 session id）。
- `registerTerminal()`（新文件 `cmd/sta/terminal.go`，enabled 时）：
  1. 构造 `terminalTools`（见 §4）→ 注册 5 个工具到 `a.reg`（照 registerJobs 同款，循环 Register）。
  2. D3 事件回调：`terminal/start` / `terminal/stop` append 到 `a.log`（照 job/* onEvent 同款；a.log 读取在调用时）。
- `command` switch 增 `/term` 子命令（§5）。
- 生命周期：应用关闭时若有活跃会话 → `termSess.Close()`（照 deferred Close 心智）。
- printHelp 增 `/term <start|write|read|signal|stop>` 行。

## 4. 模型工具五件套契约（internal/terminal/tools.go 或 cmd/sta）

> 工具放 `internal/terminal`（照 jobs.NewJobTools 同款：`NewTerminalTools(accessor, onEvent)`）。accessor 提供会话访问（cmd/sta 闭包：取当前活跃会话 + 校验 owner）。

```go
// tools.go（internal/terminal）
// NewTerminalTools 返回 5 个工具的构造体（照 jobs.NewJobTools 同款风格）。
// accessor 由组合根提供：TerminalAccess 接口或函数闭包——
//   GetActive() (*Session, error)        // 无会话或 owner 不符 → 错误
//   Start(opts) (*Session, error)        // 已活跃 → 错误"已有活跃会话"
//   Stop() error                          // 关停活跃会话
//   Owner() string
type TerminalTools struct{ ... }
func NewTerminalTools(acc TerminalAccess, onEvent func(typ string, data any)) *TerminalTools
func (t *TerminalTools) Start() tools.Tool   // terminal_start
func (t *TerminalTools) Write() tools.Tool   // terminal_write
func (t *TerminalTools) Read() tools.Tool    // terminal_read
func (t *TerminalTools) Signal() tools.Tool   // terminal_signal
func (t *TerminalTools) Stop() tools.Tool    // terminal_stop
```

**工具语义（schema + 返回）**：
- `terminal_start`：参数 `{command?: string}`（非必填；传入则在会话里执行该命令，否则起裸会话）。返回：会话已启动（id、status: running）+ 首屏输出（启动 motd，有界截断）。
  - 实现：`Start(opts)` → NewSession；若 command 非空 → `Write(command, true)` 取 Viewport；事件 `terminal/start`（{session_id, owner}）。
- `terminal_write`：参数 `{text: string, submit?: bool}`（submit 默认 true）。返回：`{wait: "<stdin_read|timeout|session_exit>", viewport: <有界>, truncated: bool}`。事件不落正文（输出经工具返回，模型可见）。
- `terminal_read`：参数 `{offset?: int, count?: int}`（默认 0/500）。返回：`{text: <有界>, truncated: bool}`。
- `terminal_signal`：参数 `{kind: "stop"|"interrupt"}`。返回：`{delivered: true}` 或错误。
- `terminal_stop`：参数无。返回：`{status: "stopped"}`；事件 `terminal/stop`（{session_id, reason}）。

**容量**：所有返回文本经 `truncateChars(text, 8000)`（照 job_read 同款，UTF-8 安全，文末加 `…[truncated]`）。
**owner 校验**：write/read/signal/stop 前 `acc.GetActive()` 校验会话存在且属于当前 session（照 job owner 心智；本段单活跃会话，owner 不符 → 错误）。
**错误**：无会话 → 错误提示"no active terminal session (start one with terminal_start)"；会话已退出 → 错误。

## 5. /term REPL 命令契约（cmd/sta/terminal.go）

`/term <start [command]|write <text>|read [offset count]|signal <stop|interrupt>|stop>`
- `start`：起会话（enabled 才可用）；`write`：Write 后打印 wait + viewport（有界）；`read`：打印窗口；`signal`/`stop`：执行并打印结果。
- 复用 §4 的 accessor/容量逻辑；输出带 `term:` 前缀（照其他命令风格）。

## 6. 测试要求

`cmd/sta/terminal_test.go` + `internal/terminal/tools_test.go`：
- 工具层（tools_test.go）：start 无 owner/owner 不符 fail；start 后 write/read/signal/stop 走通；已活跃再 start → 错误；会话退出后 write → 错误；容量截断（大输出 → truncated 标记 + `[truncated]` 尾）。
- 接线层（cmd/sta）：`registerTerminal` enabled=false → 零工具零注册（D10）；enabled=true → 5 工具注册；/term start 后 write echo 返回；事件 `terminal/start`/`terminal/stop` append 到日志（断言事件类型出现）。
- 真实终端冒烟：短 idle（100ms）短 timeout（2s），断言 start + write("echo hi") 返回含 hi。

## 7. 提交与报告

- 每模块阶段提交（`M9-2: ...`）：config → tools → cmd/sta 接线 → 测试。
- 完成后 `go vet ./...` / `go test -count=1 ./...` / `go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
