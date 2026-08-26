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
process.exit(result.status ?? 1)
