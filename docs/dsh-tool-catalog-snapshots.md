# Shutu DSH tool catalog snapshots

These are the final model-facing projections for the non-Cordis, non-team-task migration. The standard catalog is configuration-dependent; entries below describe the complete set of registered capability families when their corresponding feature is enabled.

## Standard

Native schemas for every enabled registered tool, except `run_code`:

`get_time`, `read`, `write`, `edit`, `str_replace_editor`, platform Shell (`pwsh` on Windows), `terminal_*`, `glob`, `grep`, `lsp`, `read_image`, `skill`, `web_search`, `web_fetch`, `schedule_create`, `schedule_delete`, `schedule_list`, `create_goal`, `get_goal`, `update_goal`, `todo_write`, `exit_plan_mode`, `subagent`, `subagent_fork`, `spawn_teammate`, `list_agents`, `wait_agent`, `interrupt_agent`, `send_message`, `followup_task`, `report`, `job_output`, `job_kill`, `job_list`, `ralph`, `workflow`, `session_search`, `session_trace`, `session_event_read`, `session_event_search`, `session_event_trace`, and discovered `mcp__<server>__<tool>` names.

Disabled capabilities are absent from both the model wire catalog and the execution whitelist. The excluded `cordis_*` and `team_task_*` names are never added.

## Minimal

Exactly:

`pwsh` on Windows, or the platform shell name on the current OS; `str_replace_editor`.

The combined editor owns the DSH minimal read/replace workflow. No `run_code`, legacy `read`/`write`/`edit` model entries, workflow, MCP, team-task, or Cordis entries are exposed.

## PTC / Code Mode

The native model wire catalog contains exactly:

`run_code`

The system prompt additionally contains the generated TypeScript SDK for the standard-mode nested tools. Direct native calls to any nested tool are denied; calls made inside `run_code` use the same argument schemas, output values, policy, approval, timeout, and error pipeline. Each run is fresh, foreground, cancellable, and returns the DSH envelope `{logs, result?}`.

## Verification

- Mode projection and execution boundary: `cmd/pa/sessionruntime_test.go`.
- Versioned manifest integrity and reload revision observability:
  `internal/tools/tools_test.go`; Web manifest projection:
  `cmd/pa/webw1_test.go`.
- Release artifact export/verification and compatibility-host fail-closed
  assembly: `cmd/pa/catalog_test.go`, `cmd/pa/sessionruntime_test.go`.
- ACP/SDK wire inventory and fail-closed provider errors:
  `internal/acp/server_test.go`, `internal/sdkclient/protocol_schema_test.go`,
  `cmd/pa/sdk_test.go`.
- Concurrent addressed-runtime isolation: `cmd/pa/agent_runtime_recovery_test.go`.
- Plugin generation replacement and ownership provenance:
  `internal/plugin/registry_test.go`, `internal/tools/tools_test.go`.
- Persistent terminal cancellation and process-tree reset:
  `internal/terminal/terminal_test.go`, `cmd/pa/terminal_test.go`.
- PTC SDK and transport schema: `cmd/pa/codemode_prompt_test.go`, `cmd/pa/codes_test.go`.
- Historical regression evidence exists, but the current release gate remains
  closed until the manifest, hostile sandbox, cross-process and strict race
  requirements pass together.
