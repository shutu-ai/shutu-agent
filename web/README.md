# Shutu Agent Web

The production web UI is the DSH-aligned React/Cordis shell, vendored under
`vendor/shutu-ui` and exposed through Shutu-owned `@shutu-ai/*` package names.
The build has no dependency on a sibling `deepseek-harness` checkout.

```text
npm install
npm run typecheck
npm test
npm run build
npm run verify
```

`npm run build` emits the production SPA and native plugin manifest to `dist/`.
The Go server serves that directory using `web_server.dist_dir`.
