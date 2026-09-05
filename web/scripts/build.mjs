import { spawnSync } from 'node:child_process'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(fileURLToPath(new URL('..', import.meta.url)))
const vite = resolve(webRoot, 'node_modules/vite/bin/vite.js')
const result = spawnSync(process.execPath, [vite, 'build'], {
  cwd: webRoot,
  env: { ...process.env, SHUTU_UI_NATIVE: '1' },
  stdio: 'inherit',
})

if (result.error) throw result.error
if ((result.status ?? 1) !== 0) process.exit(result.status ?? 1)

const manifest = spawnSync(process.execPath, [resolve(webRoot, 'scripts/native-manifest.mjs')], {
  cwd: webRoot,
  env: { ...process.env, SHUTU_UI_NATIVE: '1' },
  stdio: 'inherit',
})
if (manifest.error) throw manifest.error
process.exit(manifest.status ?? 1)
