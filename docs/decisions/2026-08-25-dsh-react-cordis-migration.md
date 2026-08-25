# Shutu DSH React/Cordis migration

Status: implementing

## Scope

Only `shutu-agent` is changed. The Go runtime, SQLite event log, and auth
boundary remain in place; the browser surface moves to an independently built
React/Cordis application under `web/`. `deepseek-harness` is read-only
reference material and is not part of the migration output.

The new protocol is the acceptance target. Old data, old clients, and the
legacy unpaged response are not compatibility requirements for this refactor.

## New event contract

The client reads the cursor envelope:

```text
GET /api/sessions/{id}/events?limit=100
GET /api/sessions/{id}/events?before_seq=501&limit=100
GET /api/sessions/{id}/events?after_seq=900&limit=100
```

```json
{
  "events": [{"seq": 1, "type": "user/message", "version": 1, "time": "..."}],
  "has_more": true,
  "next_before_seq": 501,
  "first_seq": 502,
  "last_seq": 601
}
```

`seq` is the de-duplication key and SSE `id`. `before_seq` and `after_seq`
are exclusive cursors; SQLite reads `limit + 1` rows to calculate
`has_more`, with a maximum page size of 500.

The public event view is bounded. Request details are allow-listed to provider,
model, reasoning effort, status/error, retry metadata, and token usage; raw
request bodies, keys, system context, and unapproved fields do not cross the
web boundary.

## Frontend behavior

`web/src` provides:

- Conversation and Trajectory tabs.
- Search across event type, summary, reasoning, tool name, and tool output.
- Fixed-row virtual scrolling with overscan.
- Cursor-based history loading when reaching the top.
- Expand/collapse for tool input and request/token details.
- SSE reconnect with `Last-Event-ID` and sequence de-duplication.
- Loading, empty, disconnected, retry, send-error, and no-match states.

Build and serve it with:

```text
cd web
npm.cmd run typecheck
npm.cmd run build
```

`config.yaml` points `web_server.dist_dir` to `web/dist`, and the Go server
serves the SPA with history fallback.
