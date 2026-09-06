# Source reuse inventory

Audited against the local read-only DeepSeek Harness reference checkout at
commit `141eb6fef83422698aef7a981029e843e8161534` (`dsh-v0.1.0-rc.8`).

| Area/File | Source | Source License | Reuse Type | Attribution Needed | Status |
| --- | --- | --- | --- | --- | --- |
| `.agents/` (2,196 of 2,198 files) | DeepSeek Harness | MIT | Direct | Yes | PASS |
| `.agents/notes/archived/manifest.json` | DeepSeek Harness | MIT | Modified | Yes | PASS |
| `.agents/skills/record-browser-gif/scripts/encode_gif.py` | DeepSeek Harness | MIT | Modified | Yes | PASS |
| `web/vendor/shutu-ui/` | DeepSeek Harness | MIT | Modified | Yes | PASS |
| `web/vendor/shutu-ui/vendor/cordis/`, `cosmokit/`, `include/`, `loader/`, `schemastery/` | Cordis, Cosmokit, and Schemastery distributions vendored by DeepSeek Harness | MIT | Modified | Yes | PASS |
| `internal/sdkclient/testdata/protocol.schema.json` | DeepSeek Harness SDK TypeScript types | MIT | Generated | Yes | PASS |
| `tools/generate-sdk-protocol-schema.mjs` | Shutu Agent generator; reads DeepSeek Harness as developer-only input | MIT (input) | Generated | Yes for output | PASS |
| `tools/extract-ui-icons.mjs`, `tools/inject-theme-icons.mjs` | Shutu Agent extraction tooling; reads DSH UI-primitive SVG source as developer-only input | MIT (input) | Generated | Yes if checked-in icon output is redistributed | PASS |
| `cmd/`, `internal/` (except generated DSH-derived artifacts), `sdk/`, `web/src`, `web/scripts`, `scripts/`, `tools/`, `config/`, `examples/` | Shutu Agent; DSH used as an architecture/protocol behavior reference | Apache-2.0 | Behavioral Reference | No | PASS |
| `docs/`, `.web-port/`, `.smoke/` | Shutu Agent design, acceptance, evidence, and parity records | Apache-2.0 | Generated | No | PASS |
| `web/public/favicon.svg`, `logo-b.png`, `logo-w.png`, `new-logo-b.png`; repository evidence images | Shutu Agent project assets/screenshots | Apache-2.0 | Generated | No | PASS |
| KaTeX font files emitted into `web/dist/assets/` by `npm run build` | KaTeX v0.16.47 npm package | MIT | Generated | Yes | PASS |

The vendored runtime has 1,638 tracked files. 502 files have SHA-256 content
matches in the reference checkout; the remainder are path-renamed, rescope,
adaptation, integration, or project-specific changes. No exact content match
was found between the reference checkout and the tracked Go implementation,
SDK implementation, web integration source, scripts, configuration, examples,
or project documentation outside the areas listed above.
