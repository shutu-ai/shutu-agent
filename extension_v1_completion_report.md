# Extension Platform v1 Completion Report

## Result

**PASS.** The three requested v1 gaps are closed: native extension navigation, observational event subscription, and the context-provider strategy integration matrix. The independent `demo-extension` runtime integration passes against a separately built external process.

## A. Native Web Navigation

**PASS.**

The complete flow works without Agent knowledge of any extension's business domain:

```text
extension manifest web contribution
  -> Extension Host inventory and readiness
  -> GET /api/extensions
  -> generic Agent Web navigation registry
  -> authenticated reverse proxy
  -> extension-owned web page
```

- Web contributions expose generic presentation and ordering metadata: `extensionId`, `title`, `route`, `icon`, `navigationEnabled`, `navigationGroup`, `order`, and `ready`.
- `navigationEnabled` is optional and defaults to enabled. Empty icons and groups receive generic fallbacks. Order defaults deterministically for omitted or invalid client values.
- The frontend hides disabled or malformed contributions, retains unhealthy contributions as unavailable and non-clickable, and sorts by group, order, title, and id.
- A failed inventory request clears extensions, records an error state, and leaves the conversation UI and session refresh working.
- The Agent frontend contains no extension-specific branch and does not import extension UI code.

Evidence:

- `internal/extensionhost/navigation_test.go` covers host inventory metadata, readiness, defaults, disabled contributions, and ordering.
- `web/src/extensions.test.ts` covers frontend normalization, fallbacks, sorting, disabled entries, malformed routes, and unhealthy entries.
- `internal/webserver/extension_proxy_test.go` covers authenticated reverse proxying, Agent credential stripping, and inventory serialization.
- `web/src/store.test.ts` covers a failed inventory request without breaking session loading or the conversation shell.
- `examples/extension/integration_test.go` obtains a real `demo` contribution, reads it through `/api/extensions`, and requests the proxied `/extensions/demo/` page.

## B. Extension Event Subscription

**PASS.**

Protocol v1 now exposes a small, stable, observational event vocabulary:

- Session: `session.started`
- Turns: `turn.started`, `turn.completed`
- Steps: `step.started`, `step.completed`
- Tools: `tool.started`, `tool.completed`, `tool.failed`
- Context: `context.requested`, `context.injected`
- Extension lifecycle: `extension.started`, `extension.restarted`, `extension.stopped`

`session.ended` is intentionally absent: durable sessions have no reliable ended state in this architecture. `AgentStopping` is intentionally represented by per-extension `extension.stopped`, which has an unambiguous transport/lifecycle boundary. These omissions are documented rather than filled with synthetic semantics.

Event frames are versioned JSON DTOs with event id, timestamp, session, turn, step, and minimal typed payload. They never contain prompts, model output, tool arguments, tool output, provider metadata, or internal Agent structs. Manifests must enable the `events` capability and declare an exact `events.subscribe` allow-list; there is no global fanout.

Delivery is per-extension, asynchronous, bounded, ordered by one delivery worker, non-blocking, at-most-once, and best-effort. Queue overflow is counted and dropped. Timeouts, errors, connection loss, malformed responses, and slow handlers are isolated and observed. Observability records extension id, event type, success, delivery, drop, timeout, error, duration, and queue depth. Agent shutdown and restart terminal lifecycle events are queued directly and given a bounded drain opportunity before transport close.

Evidence:

- `internal/extensionhost/events_test.go` covers filtering, independent ordered delivery to multiple subscribers, envelope stability, slow subscribers, explicit handler timeouts, connection loss, repeated handler failures, observed queue overflow, sensitive-payload exclusion, replacement resubscription, stable session-start envelopes, and delivery of `extension.stopped` during close.
- `sdk/extension/manifest_test.go` covers capability, unknown event, and duplicate subscription validation.
- `sdk/extension/server_test.go` covers extension-side event dispatch.
- `go test -race ./internal/extensionhost` passes.

## C. Context Provider Strategy Matrix

**PASS.**

The tests exercise the real Agent Turn -> Step -> Model Call -> Tool Result -> Next Step path through `loop`, not a direct provider function.

| Strategy | Status | Locked behavior |
| --- | --- | --- |
| `once_per_turn` | PASS | One provider call and one durable contribution in a multi-step turn; separate sessions are isolated. |
| `before_every_model_call` | PASS | One provider call before every model call, including post-tool continuation. |
| `on_user_input_change` | PASS | Unicode-field normalized whitespace identity; unchanged input does not re-run; changed input does. |
| `after_tool_result` | PASS | Contributes before the first post-tool model call. A one-tool turn yields one post-tool contribution; two tool calls yield two. |
| `manual` | PASS | Automatic Loop calls are zero; only the generic `RefreshContext` seam invokes the provider. |

The failure matrix covers optional timeout/crash fail-soft behavior, required provider failure stopping the step, empty contributions, oversized truncatable contributions, and per-contribution limits. A strategy-wide matrix repeats timeout, crash, empty, oversized, and cancellation behavior for all five strategies, including manual through its explicit refresh seam. Existing integration tests also lock global character/token budgets, provider character budgets, priority ordering, same-source/content deduplication, and one durable context row per successful once-per-turn injection. Context contributions are source-attributed durable rows; the documented deduplication boundary is concurrent context contributions, not comparison with prior tool output.

Evidence:

- `internal/extensionhost/context_strategy_test.go`
- `internal/extensionhost/extensionhost_test.go`
- `go test -race ./internal/extensionhost`

The strategy file also locks once-per-turn recurrence on the next durable turn, provider/global character and token truncation ordering, cross-provider deduplication, and post-tool behavior after successful, failed, and cancelled tool results.

## Independent Demo Acceptance

`examples/extension/integration_test.go` builds `examples/extension` as its own binary and runs it as a managed external process. Its manifest subscribes to session, turn, step, tool, context, and lifecycle events.

| Required check | Result |
| --- | --- |
| Agent starts the independent extension | PASS |
| Inventory exposes Demo navigation | PASS |
| Frontend logic renders the contribution | PASS |
| Authenticated proxy serves the extension-owned page | PASS |
| Extension receives subscribed session/turn/tool/context events | PASS |
| Slow handler does not block publisher | PASS |
| `once_per_turn` behavior | PASS |
| `before_every_model_call` behavior | PASS |
| Post-tool `after_tool_result` behavior | PASS |
| Extension failure/crash isolation is covered | PASS |
| Restart produces a healthy new generation, event, and context contribution | PASS |
| Close removes navigation, Context target, and delivers `extension.stopped` | PASS |
| Agent Loop continues to completion | PASS |

The demo uses only generic `demo`, `strategy`, and observer names. No knowledge/RAG/domain-specific code was added to the platform.

## Compatibility

No protocol v2 or second event/context/web mechanism was introduced. New manifest fields are optional v1 additions with defaults. Unsupported subscriptions and unknown fields fail explicitly rather than being silently downgraded. Existing MCP, tool registry, approval, context pipeline, lifecycle, reverse proxy, session, CLI, REPL, Web, ACP, and SDK boundaries remain authoritative.

## Regression Evidence

Commands completed successfully on the final state:

```text
go test ./...
go test -race -count=1 ./internal/extensionhost ./internal/webserver ./examples/extension ./cmd/sta
go test ./examples/extension
npm test
npm run typecheck
npm run build
npm run verify
git diff --check
```

The post-audit verification used `-count=1` for the race and independent Demo suites. Web tests: 12 files / 53 tests passed. There is no dedicated JavaScript lint script in `web/package.json`; the repository's available static check is `npm run typecheck`, and it passed. `npm run build` emits only normal Vite chunk-size warnings; the build and dist verifier pass.
