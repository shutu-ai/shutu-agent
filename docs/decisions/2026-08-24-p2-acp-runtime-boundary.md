# P2 ACP runtime extension boundary

## Decision

The ACP session runtime does not add a dynamic plugin loader, bundle loader,
profile executor, or self-modification mechanism.

This is an intentional compile-time boundary, not an unimplemented implicit
fallback:

- Go runtime plugins are excluded, including because the target Windows build
  does not support the required `plugin` build mode;
- bundles are data/file concepts currently owned by the skill manager, not
  executable ACP extensions;
- profiles are configuration data for the host's LLM/provider settings, not a
  session tool-registration protocol;
- self-modification has no bounded, reviewable, session-owned execution
  contract and therefore is not exposed.

New capabilities continue to enter through the compile-time Go seam of
Service, Provider, and Tool, or through an explicitly isolated external MCP
server. ACP session creation does not copy `app.reg`, profile maps, plugin
directories, or other host extension state. The registry is assembled from
the explicit session-safe capability list and the opt-in runtimes documented in
the preceding ACP decisions.

## Regression boundary

The ACP registry test seeds the host app with a dynamic extension and provider
profile data, then verifies that none appear in the ACP session registry. This
keeps a future composition-root refactor from accidentally turning host
configuration into remote executable capability.

If a future runtime extension is required, it must first define a session-owned
service, an explicit ACP configuration switch, an owner/close contract, and a
metadata-only event policy before it can be added to the ACP registry.
