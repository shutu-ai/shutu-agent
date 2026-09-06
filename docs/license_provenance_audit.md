# License provenance audit

## Result

`PASS`. Shutu Agent may publish its original material under Apache-2.0. Direct
and modified DeepSeek Harness material, DSH-generated contracts, and nested
vendored MIT components remain attributed in
[THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md). Apache-2.0 is compatible
with the MIT-licensed sources; MIT notice and copyright terms are preserved.

The repository owner is `shutu-ai`. Original project material is Copyright (c)
2026 shutu-ai. DeepSeek-derived material retains its separate MIT notice.

## Source provenance

- Reference baseline: DeepSeek Harness
  `141eb6fef83422698aef7a981029e843e8161534` (`dsh-v0.1.0-rc.8`), whose root
  license is MIT with `Copyright (c) 2026 DeepSeek`.
- `.agents/` contains 2,198 tracked files: 2,196 are byte-identical to the
  reference and 2 are modified.
- `web/vendor/shutu-ui/` contains 1,638 tracked files: 502 are byte-identical
  by SHA-256 to some reference path; the rest are renamed/rescoped/adapted
  derivatives. Its nested vendor directories retain their own MIT licenses.
- `internal/sdkclient/testdata/protocol.schema.json` records seven DSH source
  files and source hashes in `x-generated-from`; the generator reads the local
  read-only reference checkout.
- No tracked Go, SDK, web integration, script, configuration, example, or
  project documentation file outside the inventory areas has a SHA-256 content
  match in the reference checkout.

The detailed inventory is in
[source_reuse_inventory.md](source_reuse_inventory.md).

## Questions and gates

1. Does the project have the right to publish as Apache-2.0? `PASS`. The
   repository owner is `shutu-ai`; independently authored material can be
   licensed Apache-2.0, and MIT-derived material permits redistribution and
   relicensing within derivative works when its notice is preserved.
2. Is third-party source directly reused? `PASS` with required attribution.
3. What are the source licenses? DeepSeek Harness and nested vendored
   Cordis/Cosmokit/Schemastery components are MIT.
4. Is Apache-2.0 compatible? `PASS`. MIT is compatible with Apache-2.0.
5. Is there a copyleft blocker? `PASS`. No GPL, AGPL, LGPL, SSPL, BUSL, Elastic
   License, Commons Clause, non-commercial, source-available, or other known
   restrictive license was found in redistributed source or production
   dependency closures.
6. Is `NOTICE` or `THIRD_PARTY_NOTICES` required? `THIRD_PARTY_NOTICES.md` is
   required and present. A separate root `NOTICE` is not required: no included
   Apache-2.0 source carries an additional NOTICE that must be propagated.
7. Is any source unattributable? `PASS`. The DSH-derived areas and generated
   SDK contract are identified; the independently implemented areas are covered
   by the root Apache-2.0 license.

## Go dependency production closure

Audited Go 1.26.7 with the module closure from `go list -deps ./...`. The
third-party production modules are:

| Module | License |
| --- | --- |
| `github.com/dustin/go-humanize` | MIT |
| `github.com/mattn/go-isatty` | MIT |
| `github.com/ncruces/go-strftime` | MIT |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `github.com/santhosh-tekuri/jsonschema/v5` | Apache-2.0 |
| `golang.org/x/sys` | BSD-3-Clause |
| `gopkg.in/yaml.v3` | MIT and Apache-2.0 |
| `modernc.org/libc` | BSD-3-Clause, with retained third-party notices |
| `modernc.org/mathutil` | BSD-3-Clause |
| `modernc.org/memory` | BSD-3-Clause, with retained third-party notices |
| `modernc.org/sqlite` | BSD-3-Clause |

Result: `PASS`. Module-provided license and third-party notice files remain in
the Go module distribution.

## Web production dependency closure

Audited `web/package-lock.json`. The production closure contains 110 locked
package instances (109 unique paths plus one nested duplicate of
`use-sync-external-store`). Locked licenses are MIT, ISC, and BSD-3-Clause only.
The `@types/*` packages are compile-time type metadata, not runtime output.
Dev/test/build-only tools are outside the shipped runtime closure.

Result: `PASS`.

## Assets, fonts, and generated code

- No font source files are tracked. The production web bundle emits the KaTeX
  fonts supplied by the MIT-licensed `katex@0.16.47` npm package; this bundled
  asset is recorded in the reuse inventory and third-party notices.
- Tracked images are project evidence screenshots, the Shutu web favicon/logo,
  and project-generated UI baselines; no third-party font or non-original
  redistributed asset was identified.
- Project-generated Go output-schema declarations and icon regeneration helpers
  are inventoried above. The DSH-derived SDK JSON Schema contract is attributed.
