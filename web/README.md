# Shutu React/Cordis Web

This is the new DSH-style web surface for `shutu-agent`. It is owned by
`shutu-agent`; `deepseek-harness` is only a read-only source for the Cordis and
React build dependencies.

Build from this directory:

```text
npm.cmd run typecheck
npm.cmd run build
```

The Go server serves `web/dist` when `web_server.dist_dir` is set in
`config.yaml`. The client consumes the cursor envelope from
`GET /api/sessions/{id}/events?limit=100`, reconnects through SSE with
`Last-Event-ID`, and de-duplicates events by `seq`.
