# P2 ACP session-owned terminal

日期：2026-08-24

## 决策

ACP terminal 不复用 REPL 的全局 `termSess/termOwner`。当且仅当同时满足：

- `terminal.enabled` 开启；
- `terminal.acp_enabled: true` 显式开启；

每个 `session/new` 创建自己的 `internal/terminal.Session`，工作目录绑定到 ACP session 的 `cwd`。ACP registry 注册五个 session-bound 工具：

- `terminal_start`
- `terminal_write`
- `terminal_read`
- `terminal_signal`
- `terminal_stop`

工具输出只作为当前模型的 tool result 返回；session 日志只记录 `terminal/start` 和 `terminal/stop` 元数据，不记录 shell 输出正文。session close 会终止对应进程并释放缓冲区。

## 安全边界

持久 shell 的 write/signal 具备任意命令执行能力，所以不能把全局 terminal 开关直接等同于 ACP 暴露许可。`acp_enabled` 是第二道显式 opt-in；缺省为 false。环境仍由 `internal/terminal` 的 scrubbed env 规则处理，cwd 不跨 session 复用。

## 后续

MCP 与 subagent 仍需分别隔离外部进程和子代理树生命周期，不能由 terminal 的 session service 代替。
