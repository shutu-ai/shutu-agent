# P2 ACP subagent session runtime

## Decision

ACP sessions receive subagent capability only after two explicit switches are
on:

- `subagent.enabled: true` enables the normal subagent capability;
- `subagent.acp_enabled: true` authorizes creation of a subagent runtime for
  ACP sessions.

`subagent.acp_enabled` defaults to false. The normal REPL runtime remains
unchanged and is not copied into ACP sessions.

An enabled ACP session creates its own local `spawn` provider, optional enabled
external providers, `subagent.Runtime`, and `SubagentTools` bundle. The local
provider receives the ACP session's LLM, prompt builder, independent registry,
and store. The tool bundle captures the ACP session ID in its owner callback,
so omitted `owner_session` and `subagent_list` filtering never read the REPL's
mutable `currentID`.

All eight subagent tools are registered in the ACP session registry only after
the runtime is built: `subagent_spawn`, `subagent_status`,
`subagent_cancel`, `subagent_list`, `subagent_send`, `subagent_interrupt`,
`subagent_report`, and `subagent_resume`. Child agents therefore inherit the
session-scoped registry rather than the global REPL registry. This keeps the
session capability profile explicit, including any terminal or MCP tools that
were separately opted into for that session.

## Lifecycle and events

`acpSession.Close` first closes the session-owned subagent runtime. The local
provider cancels and waits for all live children, preventing child goroutines
from using session-owned MCP or terminal resources after those resources have
been closed. Runtime close is idempotent and rejects later starts.

Subagent metadata events are appended to the ACP session log through the
session-bound tool sink. Child logs remain independent; background child
goroutines do not append parent events directly. External provider processes
remain opt-in through their existing provider configuration and are created
only inside the ACP-owned runtime.

The next deferred capability remains runtime plugin/bundle/profile loading,
which is outside the current Go compile-time extension boundary.
