# Extension v1 After-Tool-Result Hardening Report

## Result

**AFTER_TOOL_RESULT HARDENING: PASS**

## A. Tool Boundary Identity

**PASS.** The boundary is the durable session-event sequence of the last `tool/result` committed for the batch. `internal/loop` computes it only after the batch's tool calls have committed and only when another model request will run. The identity is stable across model/pre-step re-entry and is not generated from pointers, timestamps, random values, slice indexes or step-number changes.

Multiple tools dispatched by one model step and completed before that next model call form one boundary. A failed tool result forms the same kind of boundary when the Loop continues to the next model call.

## B. At-most-once

**PASS.** Automatic scheduling claims a boundary before dispatching a provider RPC using the key:

```text
session ID + turn ID + provider/extension ID
```

A repeated `ProvideContext` at the same boundary does not invoke the provider again. This includes the model/pre-step retry scenario. A different durable boundary re-enables the provider.

## C. Multi-provider

**PASS.** Consumption is provider-local. Provider A consuming boundary #17 does not prevent provider B from consuming it. Current manifests expose one context provider per extension, but the cache key already includes a provider/extension component so the evolution path does not require changing the idempotency model.

## D. Concurrent Re-entry

**PASS.** A test issues the same boundary to 32 concurrent callers for the same session, turn and provider. The lock-protected atomic claim admits one dispatch; the other callers receive no contribution. The cache lock is released before the extension RPC runs.

## E. Retry

**PASS.** A real Agent Loop test commits a tool result, runs `after_tool_result` through the pre-step path, and then replays the same durable boundary into `ProvideContext`. The provider is invoked exactly once.

## F. New Boundary

**PASS.** Boundary #17 does not suppress boundary #18. A later committed tool-result batch triggers the provider again.

## Failure Semantics

The host atomically claims a boundary immediately before scheduling the provider. The claim is the consumption point; the cache lock never covers the RPC. These cases all consume the claimed boundary:

| Case | Boundary consumed | Automatic retry at same boundary |
| --- | --- | --- |
| success | yes | no |
| empty contribution | yes | no |
| optional timeout | yes | no |
| optional crash / RPC error | yes | no |
| optional cancellation | yes | no |
| required failure | yes | no; the existing required-provider failure still fails the step |

This follows the current fail-soft optional provider model and avoids adding a new retry framework. The boundary identity stays inside the Agent runtime; Extension Protocol v1 requests and schemas are unchanged.

## Verification

Commands run on the final state:

```text
go test -count=1 ./...
go vet ./...
go build ./...
CGO_ENABLED=1 go test -race -count=1 ./...
go test -race -count=20 ./internal/extensionhost
cd web
npm test
npm run typecheck
npm run build
npm run verify
npm run e2e
```
