# DSH non-Cordis tool parity

This task list covers model-visible DSH tools. `deepseek-harness` is reference-only and is never modified.

Explicitly excluded from this migration:

- `cordis_define`, `cordis_run`, `cordis_stop`, `cordis_inspect_*`, `cordis_undefine`
- `team_task_create`, `team_task_get`, `team_task_list`, `team_task_update`

## Tasks

- [x] P-NC-1: Align agent-control names and boundaries: `subagent`, `spawn_teammate`, `list_agents`, `wait_agent`, `interrupt_agent`, `send_message`, `followup_task`, and child-scoped `report`.
- [x] P-NC-2: Align canonical job observers: `job_output`, `job_kill`, and `job_list`; keep job creation internal.
- [x] P-NC-3: Remove legacy model-facing Plan/Eval/Spill tools; retain only goal/todo model tools and internal services.
- [x] P-NC-4: Align `ask_user_question`, including question/answer schema and cancellation behavior.
- [x] P-NC-5: Align file, shell, and terminal tools: `read`, `write`, `edit`, `str_replace_editor`, platform Shell, and `terminal_*`; remove model-facing `run_command` and duplicate file-list tools.
- [x] P-NC-6: Align remaining independent tools: `exit_plan_mode`, `get_time`, `skill`, `web_*`, `schedule_*`, `lsp`, `read_image`, `ralph`, `workflow`, and dynamic MCP tools.
- [x] P-NC-7: Verify `run_code` PTC/standard projection, cancellation, timeout, streaming output, and error protocol.
- [x] P-NC-8: Add final catalog snapshots for standard/minimal/PTC and full regression coverage.

## Completion evidence

- `go test ./...` passes across all Shutu packages.
- The mode snapshots are recorded in [`dsh-tool-catalog-snapshots.md`](dsh-tool-catalog-snapshots.md).
- `deepseek-harness` remains reference-only; no source or generated artifact in that checkout was changed.

## Reference catalog (excluding the explicit exclusions)

`ask_user_question`, platform Shell (`pwsh` on Windows), `create_goal`, `edit`, `exit_plan_mode`, `followup_task`, `get_goal`, `glob`, `grep`, `interrupt_agent`, `job_kill`, `job_list`, `job_output`, `list_agents`, `lsp`, `ralph`, `read`, `read_image`, `report`, `run_code`, `schedule_create`, `schedule_delete`, `schedule_list`, `send_message`, `session_event_read`, `session_event_search`, `session_event_trace`, `session_search`, `session_trace`, `skill`, `spawn_teammate`, `str_replace_editor`, `subagent`, `todo_write`, `update_goal`, `wait_agent`, `web_fetch`, `web_search`, `workflow`, `write`, and `terminal_*`.

`get_time` remains as an existing Shutu-local tool by prior agreement.
