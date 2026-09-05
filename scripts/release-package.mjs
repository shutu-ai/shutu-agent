import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const version = git('rev-parse', '--short', 'HEAD') || 'local'
const defaultOutput = join(root, 'release', `shutu-agent-${version}`)
const outputArg = process.argv.indexOf('--output')
const output = resolve(outputArg >= 0 ? process.argv[outputArg + 1] || defaultOutput : defaultOutput)
const binaryName = process.platform === 'win32' ? 'sta.exe' : 'sta'

function git(...args) {
  const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
  return result.status === 0 ? result.stdout.trim() : ''
}

function run(command, args, cwd = root) {
  const windowsCmd = process.platform === 'win32' && command.endsWith('.cmd')
  const executable = windowsCmd ? (process.env.ComSpec ?? 'cmd.exe') : command
  const executableArgs = windowsCmd ? ['/d', '/s', '/c', command, ...args] : args
  const result = spawnSync(executable, executableArgs, { cwd, stdio: 'inherit' })
  if (result.error) throw result.error
  if (result.status !== 0) process.exit(result.status ?? 1)
}

const webRoot = join(root, 'web')
run(process.platform === 'win32' ? 'npm.cmd' : 'npm', ['run', 'build'], webRoot)
run(process.platform === 'win32' ? 'npm.cmd' : 'npm', ['run', 'verify'], webRoot)

await rm(output, { recursive: true, force: true })
await mkdir(join(output, 'bin'), { recursive: true })
await mkdir(join(output, 'web'), { recursive: true })
await mkdir(join(output, 'config'), { recursive: true })
run('go', ['build', '-o', join(output, 'bin', binaryName), './cmd/sta'])
await cp(join(webRoot, 'dist'), join(output, 'web', 'dist'), { recursive: true })
await cp(join(root, 'config.yaml'), join(output, 'config.yaml'))
await cp(join(root, 'config', 'prompts'), join(output, 'config', 'prompts'), { recursive: true })
await cp(join(root, 'docs', 'deployment.md'), join(output, 'deployment.md'))
const manifest = {
  name: 'shutu-agent',
  version,
  commit: git('rev-parse', 'HEAD') || null,
  target: `${process.platform}-${process.arch}`,
  binary: `bin/${binaryName}`,
  frontend: 'web/dist',
  config: 'config.yaml',
}
await writeFile(join(output, 'release.json'), `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
console.log(JSON.stringify({ output, ...manifest }))
