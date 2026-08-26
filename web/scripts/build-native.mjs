import { spawnSync } from 'node:child_process'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const runner = resolve(webRoot, 'scripts/run-dsh-tool.mjs')
const result = spawnSync(process.execPath, [runner, 'vite', 'build'], {
  cwd: webRoot,
  env: { ...process.env, SHUTU_DSH_NATIVE: '1' },
  stdio: 'inherit',
})

if (result.error) throw result.error
if ((result.status ?? 1) !== 0) process.exit(result.status ?? 1)

const manifest = spawnSync(process.execPath, [resolve(webRoot, 'scripts/native-manifest.mjs')], {
  cwd: webRoot,
  env: { ...process.env, SHUTU_DSH_NATIVE: '1' },
  stdio: 'inherit',
})
if (manifest.error) throw manifest.error
process.exit(manifest.status ?? 1)
