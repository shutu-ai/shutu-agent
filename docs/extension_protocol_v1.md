# Extension Protocol v1

## Versioning

* Wire protocol: `shutu-extension/1`
* SDK/API version: `1.0`
* v1 minor changes are backward compatible.
* Major versions are incompatible and must fail startup explicitly.

A manifest may set `required_agent_api`. The host accepts it only when it is the same major version and its minor is not newer than the host.

## Transports

### Managed stdio

The Agent launches `transport.command` with `transport.args`. Frames are newline-delimited JSON-RPC 2.0 on stdin/stdout. stderr is not part of the protocol.

The child receives a minimized environment: common path/temp/system variables plus explicit non-secret `NAME=value` entries. Credential-shaped variables are rejected.

### External HTTP

`transport.endpoint` accepts one JSON-RPC 2.0 request with `POST application/json` and returns one JSON-RPC response. This mode is for an independently started local service.

## Methods

### `initialize`

Request:

```json
{
  "protocolVersion": "shutu-extension/1",
  "agentApiVersion": "1.0",
  "agentName": "shutu-agent",
  "grantedPermissions": ["session.id", "user.input"],
  "supportedCapabilities": {
    "tools": true,
    "contextProvider": true,
    "lifecycle": true,
    "web": true,
    "health": true,
    "events": true
  }
}
```

Result:

```json
{
  "protocolVersion": "shutu-extension/1",
  "extensionApiVersion": "1.0",
  "capabilities": {"tools": true, "contextProvider": true},
  "tools": [],
  "webBaseUrl": "http://127.0.0.1:0/"
}
```

Returned tools are the complete advertised tool set. The manifest is used when no tools are returned.

### `health`

Returns `{"ready":true,"status":"ready"}`. Startup fails if a declared health capability is not ready.

### `context/provide`

Request is the minimal permitted session/turn/step/workspace/user context. Result:

```json
{
  "contributions": [
    {
      "source": "demo",
      "content": "evidence",
      "priority": 10,
      "estimatedTokens": 8,
      "truncatable": true
    }
  ]
}
```

### `tool/call`

Request contains the extension-local tool name, parsed JSON object arguments, optional durable call id, and session id only when granted. A tool error is represented as `{"error":"message"}` inside a successful RPC result.

### `event`

Observational host-to-extension notification. It cannot mutate Agent state. V1.0 defines the frame and Go helper; broad Agent event subscription is reserved for a backward-compatible v1 minor update.

### `shutdown`

The extension returns `{}` and stops accepting new work. For stdio, Agent then closes stdin and applies a bounded kill deadline if needed.

## Errors

JSON-RPC codes:

* `-32001`: wire protocol mismatch
* `-32002`: Agent/extension API mismatch
* `-32003`: health failure
* `-32004`: context provider failure
* `-32005`: tool callback failure
* `-32006`: event observer failure

Transport-level errors are fail-soft for optional capabilities. A required context provider returns its error to the Loop and stops the model request.
