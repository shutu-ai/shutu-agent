# DSH 非 Cordis 工具对齐任务清单

本清单只覆盖模型可见工具。以下能力明确排除，不在本轮实现：

- `cordis_define`
- `cordis_run`
- `cordis_stop`
- `cordis_inspect_*`
- `cordis_undefine`
- `team_task_create`
- `team_task_get`
- `team_task_list`
- `team_task_update`

`deepseek-harness` 只作为只读参考目录，Shutu 是唯一修改目标。

## 任务顺序

- [ ] P-NC-1：将 Agent 控制工具对齐为 DSH 官方名称和边界：`subagent`、`spawn_teammate`、`list_agents`、`wait_agent`、`interrupt_agent`、`send_message`、`followup_task`、`report`。不实现 `team_task_*`。
- [x] P-NC-2：对齐 Job 工具 `job_output`、`job_kill`、`job_list` 的 schema、输出、取消和 owner 隔离；旧 `job_start/status/cancel/wait/read` 不进入模型工具面。
- [ ] P-NC-3：清理旧 Plan/Eval/Spill 工具模型面，仅保留 DSH 的 `create_goal`、`get_goal`、`update_goal`、`todo_write`；内部实现可以继续作为存储/运行时复用。
- [ ] P-NC-4：对齐交互工具，仅保留 `ask_user_question` 的 DSH 模型面，并核对问题结构、回答结构、取消/超时行为。
- [ ] P-NC-5：对齐文件、Shell 和终端工具：`read`、`write`、`edit`、`str_replace_editor`、平台 Shell、`terminal_*`；移除模型面旧 `run_command` 和重复文件列表工具。
- [ ] P-NC-6：对齐剩余独立工具 `exit_plan_mode`、`get_time`、`skill`、`web_*`、`schedule_*`、`lsp`、`read_image`、`ralph`、`workflow`、MCP 动态工具的模式边界和返回结果。
- [ ] P-NC-7：核对 `run_code` 的 PTC/标准投影、取消、超时、流式输出和错误协议；PTC 外层只暴露 DSH 规定的 `run_code`。
- [ ] P-NC-8：建立最终工具目录快照测试，验证标准、极简、PTC 以及配置开关下不存在排除项之外的旧别名，并运行全量回归。

## DSH 参考目录（排除项已移除）

`ask_user_question`、平台 Shell（Windows 为 `pwsh`）、`create_goal`、`edit`、
`exit_plan_mode`、`followup_task`、`get_goal`、`glob`、`grep`、`interrupt_agent`、
`job_kill`、`job_list`、`job_output`、`list_agents`、`lsp`、`ralph`、`read`、
`read_image`、`report`、`run_code`、`schedule_create`、`schedule_delete`、
`schedule_list`、`send_message`、`session_event_read`、`session_event_search`、
`session_event_trace`、`session_search`、`session_trace`、`skill`、
`spawn_teammate`、`str_replace_editor`、`subagent`、`todo_write`、`update_goal`、
`wait_agent`、`web_fetch`、`web_search`、`workflow`、`write`、以及
`terminal_open/list/read/send/signal/close`。

`get_time` 是 Shutu 保留的本地工具，不属于 DSH 官方目录，但按既有约定保留。
