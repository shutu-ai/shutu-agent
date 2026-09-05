# Extension Development Guide

## Build the demo

From an independent checkout or copied project:

```powershell
go build -o demo-extension.exe ./examples/extension
```

Edit `extension.yaml` so `transport.command` is the absolute built executable path, or place the executable on the Agent process `PATH`. The extension owns its business configuration; the Agent only stores discovery and generic policy.

## Enable discovery

Explicit source:

```yaml
extensions:
  enabled: true
  startup_timeout_ms: 10000
  context_timeout_ms: 5000
  global_context_chars: 4000
  max_contribution_chars: 2000
  global_context_tokens: 1000
  max_contribution_tokens: 500
  sources:
    - manifest: C:/extensions/demo/extension.yaml
      required: false
      grants:
        - session.id
        - session.turn
        - session.step
        - workspace.path
        - user.input
```

Directory discovery:

```yaml
extensions:
  enabled: true
  directory: C:/extensions
```

Each immediate child directory may contain `extension.yaml`. Permissions can be granted by extension id:

```yaml
extensions:
  enabled: true
  directory: C:/extensions
  grants:
    demo:
      - session.id
      - user.input
```

## Implement a Go extension

Use only `github.com/shutu-ai/shutu-agent/sdk/extension`. Never import `internal/...`.

```go
server := extension.NewServer(extension.ServerCallbacks{
    Manifest: manifest,
    ProvideContext: func(ctx context.Context, req extension.ContextRequest) (extension.ContextResult, error) {
        return extension.ContextResult{Contributions: []extension.ContextContribution{{
            Source: "my-extension", Content: "evidence", Truncatable: true,
        }}}, nil
    },
    CallTool: func(ctx context.Context, req extension.ToolCallRequest) (extension.ToolCallResult, error) {
        return extension.ToolCallResult{Value: req.Arguments["text"]}, nil
    },
})
err := server.Run(ctx, os.Stdin, os.Stdout)
```

Other languages implement the same JSON-RPC methods over stdio or HTTP.

## Context scheduling semantics

`after_tool_result` is scheduled at most once per durable tool-result batch per context provider. A timeout, empty result, cancellation or failure consumes that scheduling opportunity; it does not retry the same boundary. `manual` providers run only when the host explicitly calls `RefreshContext`; the generic seam does not make a `manual` provider run automatically.

## Subscribe to events

Declare an exact allow-list; the host never performs an unfiltered event fanout:

```yaml
capabilities:
  events: true
events:
  subscribe:
    - turn.started
    - turn.completed
    - tool.completed
```

Event handlers are observational. They must not attempt to mutate Agent state, change a model request, replace a tool result, or implement approval policy. Use a context provider for retrieval, a tool for action, and the existing approval path for authority.

## Tool approval

Read-only tools can be dynamically enabled. Any other risk requires an explicit Agent whitelist entry:

```yaml
tools:
  enabled:
    - ext__demo__record
```

The host also adds such names to `interact.sensitive_tools`. The extension cannot bypass this policy.

## Verify

1. Start the Agent after enabling `extensions`.
2. Inspect the tool catalog for `ext__demo__echo`.
3. Submit a user message and inspect durable context messages for the extension source.
4. Open `/extensions/demo/`.
5. Confirm `/api/extensions` contains the navigation contribution and the sidebar shows it.
6. Inspect `extension_events.jsonl` for delivered event observations, queue depth and drops.
7. Stop or kill the extension; verify the Agent remains responsive and restarts according to policy.
