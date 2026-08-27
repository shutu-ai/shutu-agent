# P36 native DSH visual baseline

Generated on 2026-08-27 from the native DSH build with:

```powershell
$env:SHUTU_E2E_ARTIFACT_DIR = 'docs/p36-visual-baseline'
npm.cmd run e2e
```

The baseline is produced by the repository's Playwright fallback because the
Browser plugin is not available in the current environment. It covers the
native DSH shell in light/dark desktop, mobile, loading/error states, and the
workspace, jobs, subagents, settings, and geometry surfaces.

The PNGs are evidence of the Shutu native build's rendered state. A pixel-diff
against an independently launched DSH reference instance is intentionally not
claimed here and remains an open P36-6 task.

## Files

- `shutu-native-desktop.png` and `shutu-native-mobile.png`
- `shutu-native-dark-desktop.png`
- `shutu-native-loading-desktop.png`
- `shutu-native-error-desktop.png` and `shutu-native-error-mobile.png`
- `shutu-native-core-*.png`
- `shutu-native-geometry-*.png` and the matching JSON measurements
