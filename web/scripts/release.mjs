import { spawnSync } from 'node:child_process'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const isWindows = process.platform === 'win32'
const npm = isWindows ? 'npm.cmd' : 'npm'
const steps = [
  ['run', 'typecheck'],
  ['test', '--', '--run'],
  ['run', 'build'],
  ['run', 'verify'],
]

for (const args of steps) {
  console.log(`\n> npm ${args.join(' ')}`)
  const command = isWindows ? (process.env.ComSpec ?? 'cmd.exe') : npm
  const commandArgs = isWindows ? ['/d', '/s', '/c', npm, ...args] : args
  const result = spawnSync(command, commandArgs, { cwd: webRoot, stdio: 'inherit' })
  if (result.error) throw result.error
  if (result.status !== 0) process.exit(result.status ?? 1)
}
