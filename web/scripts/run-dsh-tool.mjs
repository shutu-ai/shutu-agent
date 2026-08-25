import { existsSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const tools = {
  vite: resolve(dshRoot, 'apps/web/node_modules/vite/bin/vite.js'),
  typescript: resolve(dshRoot, 'apps/web/node_modules/typescript/bin/tsc'),
  vitest: resolve(dshRoot, 'apps/web/node_modules/vitest/vitest.mjs'),
}
const name = process.argv[2]
const tool = name === undefined ? undefined : tools[name]

if (tool === undefined || !existsSync(tool)) {
  throw new Error([
    'shutu web build tool is unavailable.',
    `Set SHUTU_DSH_ROOT to a deepseek-harness checkout (current: ${dshRoot}).`,
    `Expected tool: ${tool ?? 'vite | typescript | vitest'}.`,
  ].join('\n'))
}

const result = spawnSync(process.execPath, [tool, ...process.argv.slice(3)], {
  cwd: webRoot,
  env: process.env,
  stdio: 'inherit',
})
if (result.error) throw result.error
process.exit(result.status ?? 1)
