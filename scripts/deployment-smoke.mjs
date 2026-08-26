import { cp, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { spawn } from 'node:child_process'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const packageRoot = resolve(process.env.SHUTU_RELEASE_PACKAGE ?? join(root, 'release', 'p35-shutu-agent'))
const binaryName = process.platform === 'win32' ? 'shutu-agent.exe' : 'shutu-agent'
const token = 'p35-deployment-smoke-token'
const ports = [18131, 18132]

if (!existsSync(join(packageRoot, 'bin', binaryName))) {
  throw new Error(`release package is missing ${join(packageRoot, 'bin', binaryName)}`)
}

function delay(milliseconds) {
  return new Promise(resolvePromise => setTimeout(resolvePromise, milliseconds))
}

async function configurePackage(directory, port, dataDirectory) {
  const configPath = join(directory, 'config.yaml')
  const source = await readFile(configPath, 'utf8')
  const dataPath = dataDirectory.replaceAll('\\', '/')
  const configured = source
    .replace(/^  addr:.*$/m, `  addr: 127.0.0.1:${port}`)
    .replace(/^  token:.*$/m, `  token: "${token}"`)
    .replace(/^data_dir:.*$/m, `data_dir: ${dataPath}`)
  await writeFile(configPath, configured, 'utf8')
}

function start(directory) {
  const child = spawn(join(directory, 'bin', binaryName), ['--web-only', '--config', 'config.yaml'], {
    cwd: directory,
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  })
  let output = ''
  const capture = chunk => {
    output += chunk.toString()
    if (output.length > 8_000) output = output.slice(-8_000)
  }
  child.stdout?.on('data', capture)
  child.stderr?.on('data', capture)
  return { child, getOutput: () => output }
}

async function stop(processHandle) {
  if (processHandle === undefined || processHandle.child.exitCode !== null) return
  processHandle.child.kill()
  await new Promise(resolvePromise => {
    const timer = setTimeout(() => {
      processHandle.child.kill('SIGKILL')
      resolvePromise()
    }, 5_000)
    processHandle.child.once('exit', () => {
      clearTimeout(timer)
      resolvePromise()
    })
  })
}

async function removeTemporaryRoot(directory) {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      await rm(directory, { recursive: true, force: true })
      return
    } catch (error) {
      if (error?.code !== 'EBUSY' && error?.code !== 'EPERM') throw error
      await delay(250)
    }
  }
  throw new Error(`timed out cleaning temporary deployment directory: ${directory}`)
}

async function waitForHealthy(processHandle, port) {
  const url = `http://127.0.0.1:${port}`
  const headers = { Authorization: `Bearer ${token}` }
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    if (processHandle.child.exitCode !== null) {
      throw new Error(`web-only exited before health check: ${processHandle.getOutput()}`)
    }
    try {
      const health = await fetch(`${url}/api/health`, { headers })
      const sessions = await fetch(`${url}/api/sessions`, { headers })
      if (health.status === 200 && sessions.status === 200) {
        return { health: health.status, sessions: sessions.status }
      }
    } catch {
      // The process may still be binding its listener.
    }
    await delay(100)
  }
  throw new Error(`timed out waiting for ${url}; output: ${processHandle.getOutput()}`)
}

const temporaryRoot = await mkdtemp(join(root, 'release', '.deployment-smoke-'))
let active
try {
  const sharedData = join(temporaryRoot, 'shared-data')
  const versions = await Promise.all(ports.map(async (port, index) => {
    const directory = join(temporaryRoot, `version-${index + 1}`)
    await cp(packageRoot, directory, { recursive: true })
    await configurePackage(directory, port, sharedData)
    return directory
  }))

  active = start(versions[0])
  const initial = await waitForHealthy(active, ports[0])
  await stop(active)
  active = undefined

  active = start(versions[1])
  const upgrade = await waitForHealthy(active, ports[1])
  await stop(active)
  active = undefined

  active = start(versions[0])
  const rollback = await waitForHealthy(active, ports[0])
  await stop(active)
  active = undefined

  console.log(JSON.stringify({ package: packageRoot, initial, upgrade, rollback, sharedData: true }))
} finally {
  await stop(active)
  await removeTemporaryRoot(temporaryRoot)
}
