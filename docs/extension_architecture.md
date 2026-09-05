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

## Failure model

| Failure | Behavior |
| --- | --- |
| manifest invalid / command missing | source fails; required source aborts startup |
| protocol/API mismatch | explicit `ERR_INCOMPATIBLE`, no silent fallback |
| capability omitted | startup fails for that extension |
| provider timeout/cancel/panic/process death | optional provider contributes nothing; required provider stops the step |
| oversized contribution | truncates when allowed, drops non-truncatable contribution |
| tool unavailable | normal structured tool failure; Agent turn continues |
| crash | no host crash; replacement starts under restart policy on next observed failure |
| restart budget exhausted | extension absent/unavailable; host stays stable |
| Web service unavailable | reverse proxy returns 502/503 |
| shutdown | `shutdown` RPC, stdin close, bounded wait, process kill if needed |
