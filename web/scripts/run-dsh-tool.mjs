import { existsSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs'
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

function resolveTypeScriptProject() {
  if (name !== 'typescript') return undefined
  const sourcePath = resolve(webRoot, 'tsconfig.json')
  const generatedPath = resolve(webRoot, '.tsconfig.shutu.generated.json')
  const source = JSON.parse(readFileSync(sourcePath, 'utf8'))
  const replaceRoot = (value) => {
    if (typeof value === 'string') return value.replaceAll('../../deepseek-harness', dshRoot.replaceAll('\\', '/'))
    if (Array.isArray(value)) return value.map(replaceRoot)
    if (value !== null && typeof value === 'object') {
      return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, replaceRoot(child)]))
    }
    return value
  }
  writeFileSync(generatedPath, `${JSON.stringify(replaceRoot(source), null, 2)}\n`)
  return generatedPath
}

const project = resolveTypeScriptProject()
const args = project === undefined
  ? process.argv.slice(3)
  : ['--project', project, ...process.argv.slice(3)]
let result
try {
  result = spawnSync(process.execPath, [tool, ...args], {
    cwd: webRoot,
    env: process.env,
    stdio: 'inherit',
  })
} finally {
  if (project !== undefined) {
    try { unlinkSync(project) } catch { /* best-effort cleanup */ }
  }
}
if (result.error) throw result.error
process.exit(result.status ?? 1)
