# Shutu React/Cordis Web

This is the new DSH-style web surface for `shutu-agent`. It is owned by
`shutu-agent`; `deepseek-harness` is only a read-only source for the Cordis and
React build dependencies.

Build from this directory:

```text
npm.cmd run typecheck
npm.cmd test -- --run
npm.cmd run build
npm.cmd run verify
npm.cmd run e2e
```

For a release-ready frontend, run `npm.cmd run release`; it executes the type,
unit, build, dist, and Playwright smoke checks, and fails if
`dist/index.html` references source-only paths or missing bundles. The final
`dist/` directory is the only frontend artifact required by the Go server.

The scripts resolve the DSH toolchain through `SHUTU_DSH_ROOT`. A sibling
`deepseek-harness` checkout is used only as the local default; CI or a deploy
workspace must set the variable explicitly to its read-only DSH source tree.

The Go server requires and serves the React/Cordis `web/dist` directory from
`web_server.dist_dir` in `config.yaml`; there is no embedded legacy frontend
fallback. The native client uses DSH `client-request` / `server-response` RPC
envelopes and the downlink-only `events.mux` / `events.host` WebSockets. Session
history and live updates share the same projection cursor and event sequence.
