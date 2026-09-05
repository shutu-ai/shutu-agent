# Extension Test Report

Date: 2026-09-05

## Commands

```powershell
go test ./...
go test -race ./sdk/extension ./internal/extensionhost ./internal/webserver ./cmd/sta ./examples/extension
```

Both commands completed successfully.

## Coverage summary

### Public contract

* Manifest parsing and semantic fields
* unknown-field rejection
* safe extension/tool names
* credential-shaped environment rejection
* protocol/API major-minor negotiation
* initialize and tool/call protocol framing

### Host and lifecycle

* explicit discovery and startup
* managed stdio process launch
* health-ready startup
* startup failure for a missing executable
* protocol mismatch rejection
* context cancellation and timeout fail-soft behavior
* process crash fail-soft behavior and automatic replacement
* explicit restart and restart-budget exhaustion
* graceful shutdown

### Context pipeline

* context reaches a captured model request through the existing Loop pre-step seam
* context source provenance is preserved on the injected message
* priority, deduplication, per-contribution and global token/character budgets
* required-provider fail semantics remain declarative in the manifest

### Tool and approval integration

* extension tool registration as `ext__<extension>__<tool>`
* normal registry schema/whitelist execution
* write-risk tool is added to the sensitive-tool approval list
* composition-root test covers discovery, context injector, tool and approval policy together

### Web

* authenticated generic reverse proxy
* extension-relative path forwarding
* extension inventory API
* unavailable backend error behavior
* Agent bearer token and cookie stripping

### Regression and platform scans

* All existing Go package tests passed under `go test ./...`.
* Race detector passed for the public SDK, extension host, Web shell and composition-root packages.
* `git diff --check` passed.
* No public/example extension imports `internal/...`.
* No Knowledge/RAG/Embedding/Vector domain implementation exists in Agent code.
