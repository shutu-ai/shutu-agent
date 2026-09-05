import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(fileURLToPath(new URL('..', import.meta.url)))
const uiRoot = resolve(webRoot, 'vendor/shutu-ui')
const outputPath = resolve(webRoot, 'dist/shutu-native-manifest.json')
const packageScope = '@shutu-ai'

const clientPackageDirs = [
  'connection', 'hmr', 'locale', 'runtime', 'ui-agent-preset', 'ui-attachment',
  'ui-brand-official', 'ui-commands', 'ui-conversation', 'ui-deliverables',
  'ui-directory-picker-browse', 'ui-goal', 'ui-input-trigger', 'ui-jobs',
  'ui-layout', 'ui-message-feedback', 'ui-model-selection', 'ui-permission-presets',
  'ui-plan', 'ui-reference', 'ui-renderer', 'ui-settings', 'ui-settings-general',
  'ui-settings-models', 'ui-settings-plugin-inventory', 'ui-settings-plugins',
  'ui-sidebar', 'ui-skill', 'ui-subagent', 'ui-theme', 'ui-tool', 'ui-trajectory',
  'ui-user-questions', 'ui-workflow-run', 'ui-workspace',
]

const extraPackages = [
  ['typert/registry', `${packageScope}/typert-registry`],
  ['extensions/cordis-client-runner', `${packageScope}/cordis-client-runner`],
  ['extensions/ui-cordis', `${packageScope}/ui-cordis`],
  ['session-query/session-log-export', `${packageScope}/session-log-export`],
]

function readJSON(path) {
  return JSON.parse(readFileSync(path, 'utf8'))
}

const rootManifestPath = resolve(uiRoot, 'package.json')
if (!existsSync(rootManifestPath)) throw new Error(`UI source manifest is missing: ${rootManifestPath}`)
const rootManifest = readJSON(rootManifestPath)

const plugins = clientPackageDirs.map(dir => ({
  id: `${packageScope}/client-${dir}`,
  source: `vendor/shutu-ui/packages/client/${dir}/src/client/index.ts`,
}))
for (const [relative, id] of extraPackages) {
  plugins.push({ id, source: `vendor/shutu-ui/packages/${relative}/src/client/index.ts` })
}
plugins.push({ id: `${packageScope}/api-remotes`, source: 'web/src/native-remote.ts' })

const manifest = {
  schemaVersion: 1,
  kind: 'shutu-native-ui',
  profile: 'official',
  buildRevision: 'shutu-vendored-ui-v1',
  ui: {
    version: rootManifest.version ?? null,
    sourceRoot: 'vendor/shutu-ui',
  },
  plugins,
}

writeFileSync(outputPath, `${JSON.stringify(manifest, null, 2)}\n`)
console.log(JSON.stringify({ manifest: 'shutu-native-manifest.json', plugins: plugins.length, uiVersion: manifest.ui.version }))
