# Shutu React/Cordis Web

This is the new DSH-style web surface for `shutu-agent`. It is owned by
`shutu-agent`; `deepseek-harness` is only a read-only source for the Cordis and
React build dependencies.

Build from this directory:

```text
npm.cmd run typecheck
npm.cmd run build
```

The scripts resolve the DSH toolchain through `SHUTU_DSH_ROOT`. A sibling
`deepseek-harness` checkout is used only as the local default; CI or a deploy
workspace must set the variable explicitly to its read-only DSH source tree.

The Go server requires and serves the React/Cordis `web/dist` directory from
`web_server.dist_dir` in `config.yaml`; there is no embedded legacy frontend
fallback. The client consumes the cursor envelope from
`GET /api/sessions/{id}/events?limit=100`, reconnects through SSE with
`Last-Event-ID`, and de-duplicates events by `seq`.
