# Extension Architecture Audit

## Scope and method

This audit covers the Agent loop, pre-step context pipeline, tool execution and approval, MCP bridge, SDK/ACP surfaces, Web shell, configuration, lifecycle, observability, and persistence boundaries. It was performed against the current source tree before the Extension v1 implementation and rechecked after integration.

## Existing reusable seams

| Area | Current seam | Audit conclusion |
| --- | --- | --- |
| Context | `internal/loop.PreStepInjector`, `PreStepHook`, `RequestHook`, `TurnStoppingHook` | Mature per-step pipeline with ordering, durability, cadence, aggregate/per-injector limits, cancellation and panic isolation. Best foundation for external providers; no second context pipeline should be created. |
| Tools | `internal/tools.Registry` | Single schema compilation, whitelist, timeout, output cap, result persistence, cancellation, parallel policy and middleware gate. Extension tools must register here. |
| Approval | `tools.AddPreExecuteHook` plus the interact policy | Existing authority boundary. Extension risk is input only; it cannot grant itself approval. |
| Process/tool integration | `internal/mcp` and `cmd/sta/mcps.go` | Good reconnect and process bridge, but MCP only models tool RPC. It has no native manifest negotiation, context contribution, health-ready capability surface, or generic Web route. |
| Plugin generations | `internal/plugin.Registry` | Strong transactional tool publication/replacement pattern, but it is an in-process/plugin ownership registry and is not a language-neutral external process contract. |
| SDK/ACP | `cmd/sta/sdk.go`, `internal/acp` | Stable host-facing transport, but not an extension ABI. Both must continue using the same Loop/tool seams. |
| Web | `internal/webserver` | Bearer-authenticated shell and deterministic route table already exist; extension UI should be a generic authenticated reverse proxy, not compiled into the React bundle. |
| Observability | `internal/observability`, structured session events | Existing metrics/tracer can aggregate extension calls; extension-specific structured JSONL events were missing. |
| Configuration | `internal/config` | Existing policy is fail-closed and clone-safe. Extension business settings must remain outside this file. |

## Gaps before this change

1. All native seams were under `internal/...`; an external project had to import internal packages or modify Agent source.
2. No versioned manifest, protocol identity, capability negotiation, or compatibility decision.
3. Context providers could not run in an independent process, and there was no cross-language context DTO.
4. Extension tools had no generic ownership/risk bridge into the canonical registry.
5. No managed-process supervisor with startup, health, restart budget and graceful shutdown.
6. Web UI contribution required source/build coupling.
7. Permission was implicit. A provider had no least-privilege context contract.
8. External failures lacked a uniform fail-soft/fail-required model and extension-specific telemetry.

## Why MCP alone is insufficient

MCP is retained for tool RPC and existing servers remain unchanged. It does not provide an Agent-wide deployment unit that can simultaneously negotiate context, tools, lifecycle, health, permissions, risk, Web routing and protocol compatibility. Putting every one of those concerns into MCP would overload tool transport and make non-Go deployment and independent upgrade harder.

## Design decision

Extension v1 is a thin language-neutral JSON-RPC contract plus a public Go DTO/server package. `internal/extensionhost` is the only place that adapts the contract to internal seams. This preserves the one-way dependency:

```text
external extension -> sdk/extension -> stable protocol
shutu-agent host -> internal adapters -> existing Loop/Tool/Web services
```

The Agent never learns a domain such as Knowledge, RAG, Data or Telecom.

## Residual risks

* Managed processes are local executables selected by explicit configuration. V1 scrubs credential-shaped environment input and minimizes context, but does not claim OS-level network/filesystem sandboxing.
* `after_tool_result` is represented as a per-step strategy at the existing pre-model-call boundary; it does not add a new Loop event path.
* Broad session-event fanout is protocol-reserved but not a v1.0 host subscription, because no current extension requires mutable state from it.
