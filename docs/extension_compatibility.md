# Extension Compatibility

## Compatibility promise

`sdk/extension` follows semantic versioning:

* patch: implementation fix;
* minor: backward-compatible DTO/method addition;
* major: incompatible change.

Protocol `shutu-extension/1` must match exactly. API compatibility is major-exact and minor-backward-compatible.

## Negotiation

At startup the host compares:

1. manifest `extension_api`;
2. manifest `required_agent_api`;
3. returned `extensionApiVersion`;
4. manifest capabilities with returned capabilities.

A mismatch produces an explicit startup error for that extension and never falls back to another protocol.

## Public surface

Stable public imports:

```text
github.com/shutu-ai/shutu-agent/sdk/extension
```

All Agent implementation packages under `internal/...` are unstable. No official extension may import them.

## Deployment compatibility

An extension can be added by changing only discovery configuration and restarting the Agent process; the Agent binary is not recompiled. Extension upgrades that keep Extension API v1 and keep tool names/schemas compatible do not require Agent source changes.

Agent upgrades within Extension API v1 do not require extension source changes. An extension compiled with a newer v1 SDK must keep its manifest limited to the fields understood by the oldest v1 host it targets; new negotiated behavior is exposed through capabilities and the initialize result rather than assumptions about unknown manifest fields.
