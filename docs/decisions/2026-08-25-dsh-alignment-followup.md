# dsh 对齐补充：后台作业与 shell 环境

本补充记录 2026-08-25 的二次核对结果。原 pwsh 决策文件保留作为历史决策记录。

## 当前契约

- 后台作业观察工具使用 `job_output`、`job_list`、`job_kill`；旧的 `job_read`、`job_status`、`job_cancel` 仅作为兼容别名。
- `job_output` 使用消费式游标读取 stdout/stderr 增量，空增量显示 `(no new output)`，并附加当前作业状态；等待默认 30 秒，上限 10 分钟。
- bash/pwsh 后台作业的流式输出只包含 stdout/stderr 正文，不把退出码或超时标记重复塞进增量；终态信息由作业状态渲染提供。
- shell 子进程使用 scrubbed 环境，并按 dsh 语义注入 `DSH_HOME`、`DSH_SHELL=1` 与当前会话 id；`NO_COLOR`、`PAGER`、`GIT_PAGER` 等输出稳定性变量会覆盖父环境中的同名值。

## 有意保留的边界

shutu 的 `Tool.Execute` 接口面向模型返回文本，因此保留 dsh 结构化结果的文本渲染边界；作业持久化仍使用 shutu 的 SQLite 服务，而不是 dsh 的 JSONL 后端。
