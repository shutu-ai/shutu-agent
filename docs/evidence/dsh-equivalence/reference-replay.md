# Core reference replay evidence

Date: 2026-08-28

Command:

```powershell
$env:DSH_REFERENCE_ROOT = 'D:\dev-projects\Agent\deepseek-harness'
go test -count=1 ./internal/session -run TestCoreTurnReplayMatchesReference -v
```

Observed result:

```text
=== RUN   TestCoreTurnReplayMatchesReference
--- PASS: TestCoreTurnReplayMatchesReference (0.80s)
PASS
```

The test invokes `scripts/verify-reference-replay.mjs` with the reference
checkout's TypeScript loader. It compares the ordered surface positions and
the derived message role/content/tool-call identity. Without
`DSH_REFERENCE_ROOT`, the test skips and is not evidence of a reference run.
