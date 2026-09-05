# Extension Architecture

## Principle

Shutu Agent owns orchestration, context policy, security, approval, sessions, lifecycle framework and tool execution. An extension owns its domain logic, data, UI, models and configuration.

```text
Agent Loop
  -> loop.PreStepInjector
  -> extensionhost.ContextInjector
  -> Extension Protocol v1
  -> independent extension process/service
```

## Layers

* `sdk/extension`: public, versioned DTOs, manifest parser and Go protocol-server helper. It does not import `internal/...`.
* `internal/extensionhost`: discovery, negotiation, managed stdio or external HTTP transport, context budget, tool adapter, lifecycle, health, restart and telemetry.
* `internal/tools.Registry`: remains the only tool registry and execution gate.
* `internal/loop`: remains the only model request and durable context assembly path.
* `internal/webserver`: owns generic authenticated `/extensions/{id}/` reverse-proxy routes.

## Context ownership

Extensions return `ContextContribution` values only. They cannot mutate the prompt, session database, message slice or filesystem. The host:

1. derives a minimal `ContextRequest`;
2. enforces per-provider timeout and cancellation;
3. validates and trims contributions;
4. sorts by priority;
5. removes duplicates;
6. applies per-contribution and global token/character budgets;
7. emits an `llm.Message` through the normal pre-step injector.

The Loop persists and orders the message before the provider request. Strategies are `once_per_turn`, `before_every_model_call`, `on_user_input_change`, `after_tool_result` and `manual`.

Host-owned cadence facts are lock-protected and scoped by session, turn and extension, so ACP turns that construct a new Loop retain strategy semantics without sharing mutable turn state. Input identity is Unicode-field normalized (whitespace is collapsed); `once_per_turn` applies to each durable turn; `after_tool_result` is edge-triggered by the Loop's explicit post-tool/pre-next-model signal—not by comparing step IDs—and is at-most-once per durable tool-result boundary per context provider during automatic scheduling; `manual` is never called automatically and is available through the generic Host `RefreshContext` seam.

The post-tool signal carries the durable sequence of the last committed `tool/result` in the batch. Multiple tools executed before the next model call form one boundary. The host atomically claims that boundary per provider before dispatching the RPC; success, empty evidence, timeout, crash, cancellation and required failure all consume the claimed boundary without retrying the same boundary.

Context contributions are durable context rows by design. A strategy that calls once will not copy the same evidence again within the turn. A later strategy invocation intentionally creates a new, source-attributed contribution. The context pipeline deduplicates concurrent contributions by source and content; it does not compare evidence with prior tool output.

## Tool ownership

The extension advertises tool definitions during initialization. The host registers each as `ext__<extension>__<tool>` using the normal registry metadata. Schema validation, whitelist policy, timeout, cancellation, output cap, audit and result persistence remain in `tools.Registry`.

Read-only tools can be dynamically whitelisted. Write, destructive, external-side-effect, privileged, or explicitly approval-requesting tools require the Agent policy to enable them and are added to the sensitive-tool approval list.

## Relationship to MCP

Existing `mcp.servers` configuration and its reconnecting bridge remain unchanged. Extension v1 uses its native `tool/call` transport so one manifest can also negotiate context, health, Web and lifecycle capabilities. Both paths converge on the same registry and approval authority; neither an MCP tool nor an extension tool can bypass whitelist or sensitive-tool policy. A future backward-compatible v1 minor release may add a manifest-declared MCP endpoint, but the Agent will still publish it through this same registry boundary.

## Lifecycle

The manifest supports managed stdio processes and external HTTP services. Startup performs:

1. manifest parse and identity validation;
2. required Agent API compatibility check;
3. permission grant resolution;
4. `initialize`;
5. exact protocol and major-version checks;
6. capability intersection;
7. `health`;
8. transactional tool publication;
9. Web contribution registration.

Optional extension failures do not abort the host. Required extensions fail startup explicitly. Terminal call failures are never replayed. `on-failure` and `always` restart policies launch a replacement generation under a configured restart cap.

## Web

The shell exposes:

* `GET /api/extensions`: inventory and readiness;
* `/extensions/{id}/...`: authenticated reverse proxy to the extension-owned service.

The extension UI owns all markup and business routes. It should use relative asset paths so requests remain below its shell prefix. The service URL is not exposed by the inventory API.

The Web shell builds navigation from the inventory. Contributions are ordered by navigation group, optional `order`, title and id. Ready contributions are links; unhealthy contributions remain visible as unavailable but cannot be clicked. An inventory API failure hides extensions without breaking the conversation shell.

## Events

Extension Protocol v1 supports observational `event` delivery to subscribers declared by the manifest. The host converts the durable session vocabulary into a small stable vocabulary and gives each extension a bounded queue and worker. Publishing is non-blocking; overflow is dropped and counted. A slow or failed subscriber can never block a turn, tool call or model request.

Event payloads carry correlation facts, names, status and counters. They do not carry prompts, model output, tool arguments or tool results.

## Failure model

| Failure | Behavior |
| --- | --- |
| manifest invalid / command missing | source fails; required source aborts startup |
| protocol/API mismatch | explicit `ERR_INCOMPATIBLE`, no silent fallback |
| capability omitted | startup fails for that extension |
| provider timeout/cancel/panic/process death | optional provider contributes nothing; required provider stops the step |
| oversized contribution | truncates when allowed, drops non-truncatable contribution |
| tool unavailable | normal structured tool failure; Agent turn continues |
| slow event subscriber | bounded queue fills; later events are dropped; Agent continues |
| event handler failure | observed and isolated; Agent continues |
| crash | no host crash; replacement starts under restart policy on next observed failure |
| restart budget exhausted | extension absent/unavailable; host stays stable |
| Web service unavailable | reverse proxy returns 502/503 |
| shutdown | `shutdown` RPC, stdin close, bounded wait, process kill if needed |
