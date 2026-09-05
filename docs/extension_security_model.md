# Extension Security Model

## Trust boundary

An extension is an external local program or service, not an in-process library. It must never receive Agent credentials, private runtime objects, or unrestricted history.

## Data minimization

Context requests expose only fields whose permission name was granted:

* `session.id`
* `session.turn`
* `session.step`
* `workspace.path`
* `user.input`

A required manifest permission that is not granted fails startup. Optional ungranted permissions are omitted. The extension never receives the session event log, internal message slice, provider credentials or environment secrets.

## Process environment

Managed stdio children receive a small system/path/temp environment plus explicitly declared non-secret values. Names containing token, secret, password, credential, API key or key are rejected.

## Tool authority

Extensions declare risk metadata only. Agent policy owns:

* whitelist admission;
* approval;
* schema validation;
* timeout and cancellation;
* output cap and spill;
* durable tool/result events.

Non-read extension tools require explicit whitelist admission and are classified sensitive for approval.

## Context integrity

Providers contribute strings. They cannot rewrite history, replace the system prompt, mutate the turn, or address the filesystem. The host enforces priority, deduplication and conservative token plus hard character budgets before Loop persistence.

## Web boundary

The Web shell authenticates the user and proxies only the declared extension route. The Agent bearer token and browser cookies are stripped before proxying; the extension owns business authentication and authorization underneath its route. The backend URL is not published by `/api/extensions`.

## Persistence

Agent state and extension state are separate. The Agent does not open an extension database or index. Extensions do not open the session database or private Agent runtime files.

## Limits

V1 does not claim OS-level filesystem/network confinement. Operators choose executables and endpoints explicitly. Deployments needing stronger isolation should run an external service in their own OS container/service account and connect through the HTTP transport.
