# Extension Implementation Report

## Delivered

* Public Extension Contract v1 in `sdk/extension`: manifest DTO/parser, protocol DTOs, method names and a Go server helper.
* Generic host in `internal/extensionhost`: explicit/directory discovery, stdio and HTTP transports, version/capability negotiation, permission grants, context budgeting, tool publication, health, restart and structured telemetry.
* Composition-root wiring in `cmd/sta`: canonical tool registry, sensitive-tool approval list, Loop pre-step injector, ACP Loop path and shutdown barrier.
* Web shell contribution in `internal/webserver`: `/api/extensions`, authenticated `/extensions/{id}/...` reverse proxy and credential stripping.
* Existing MCP configuration and reconnecting bridges remain unchanged; Extension v1 uses native `tool/call` and converges on the same registry/approval gate.
* Generic runtime example in `examples/extension`; it is an independent executable and has no Agent internal imports.
* Configuration and observability integration, including JSONL extension events without message/document bodies.
* Architecture, protocol, development, security and compatibility documentation.

## Requirement conclusions

### A. Can independent `shutu-knowledge` implement native behavior without modifying Agent source?

**Yes.** Using only `sdk/extension` and Extension Protocol v1, it can contribute model-call context, tools, lifecycle/health, a Web UI and telemetry. Tool execution and approval use the Agent's existing registry and pre-execute gate. Broad Agent-event fanout is a reserved v1 protocol method rather than a v1.0 host subscription; no Knowledge-specific work is required in Agent.

### B. Does any extension capability require importing `internal/...`?

**No / PASS.** The official public surface is `github.com/shutu-ai/shutu-agent/sdk/extension`. A repository scan found no import of `shutu-ai/shutu-agent/internal` in `sdk/extension` or `examples/extension`.

### C. Has Knowledge-specific code entered Agent Core?

**No / PASS.** A code scan for `Knowledge`, `RAG`, `Embedding`, `Vector` and `shutu-knowledge` found no implementation matches under `sdk`, `internal`, `cmd` or `examples`.

### D. Is the dependency one-way?

**Yes / PASS.** The dependency is:

```text
shutu-knowledge -> sdk/extension -> Extension Protocol v1
```

Agent has no `shutu-knowledge` module, package, API or manifest dependency.

### E. Can a new external extension be added without modifying or recompiling Agent source?

**Yes / PASS.** Build the independent extension, point `extensions.sources[].manifest` at its manifest (or place a child directory under `extensions.directory`), grant required permissions, and restart the Agent process. The Agent binary is not rebuilt.

## Verification boundaries

* Character and conservative token budgets are host-owned. Provider estimates are telemetry hints and cannot enlarge the hard host budget.
* Managed processes use scrubbed environments and least-privilege context, but V1 does not provide an OS-level filesystem/network sandbox. External HTTP mode exists for service-manager/container isolation.
* Event delivery is observational by contract; host event subscription is reserved for a backward-compatible v1 minor release.
