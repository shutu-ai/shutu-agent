import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const webRoot = resolve(fileURLToPath(new URL('..', import.meta.url)))
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const outputPath = resolve(webRoot, 'dist/dsh-native-manifest.json')

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
  ['typert/registry', '@deepseek-ai/dsh-typert-registry'],
  ['extensions/cordis-client-runner', '@deepseek-ai/dsh-cordis-client-runner'],
  ['extensions/ui-cordis', '@deepseek-ai/dsh-client-ui-cordis'],
  ['session-query/session-log-export', '@deepseek-ai/dsh-session-log-export'],
]

function readJSON(path) {
  return JSON.parse(readFileSync(path, 'utf8'))
}

function gitRevision() {
  const result = spawnSync('git', ['-C', dshRoot, 'rev-parse', 'HEAD'], { encoding: 'utf8' })
  if (result.status !== 0) return null
  const value = result.stdout.trim()
  return value === '' ? null : value
}

const rootManifestPath = resolve(dshRoot, 'package.json')
if (!existsSync(rootManifestPath)) throw new Error(`DSH source manifest is missing: ${rootManifestPath}`)
const rootManifest = readJSON(rootManifestPath)

const plugins = clientPackageDirs.map((dir) => ({
  id: `@deepseek-ai/dsh-client-${dir}`,
  source: `deepseek-harness/packages/client/${dir}/src/client/index.ts`,
}))
for (const [relative, id] of extraPackages) {
  plugins.push({ id, source: `deepseek-harness/packages/${relative}/src/client/index.ts` })
}
plugins.push({ id: '@deepseek-ai/dsh-api-remotes', source: 'shutu-agent/web/src/dsh-native-remote.ts' })

const manifest = {
  schemaVersion: 1,
  kind: 'shutu-dsh-native',
  profile: 'official',
  buildRevision: 'shutu-native-p36-4',
  dsh: {
    version: rootManifest.version ?? null,
    packageManager: rootManifest.packageManager ?? null,
    sourceRoot: 'deepseek-harness',
    gitRevision: gitRevision(),
  },
  plugins,
}

writeFileSync(outputPath, `${JSON.stringify(manifest, null, 2)}\n`)
console.log(JSON.stringify({ manifest: 'dsh-native-manifest.json', plugins: plugins.length, dshVersion: manifest.dsh.version }))
