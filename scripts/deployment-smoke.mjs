import { cp, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { connect } from 'node:net'
import { spawn } from 'node:child_process'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const packageRoot = resolve(process.env.SHUTU_RELEASE_PACKAGE ?? join(root, 'release', 'p35-shutu-agent'))
const binaryName = process.platform === 'win32' ? 'shutu-agent.exe' : 'shutu-agent'
const token = 'p35-deployment-smoke-token'
const ports = [18131, 18132]
const currentCommit = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: root, encoding: 'utf8' }).trim()

async function verifyPackageLayout() {
  for (const relativePath of [
    join('bin', binaryName),
    join('web', 'dist', 'index.html'),
    join('config', 'prompts'),
    'release.json',
  ]) {
    if (!existsSync(join(packageRoot, relativePath))) {
      throw new Error(`release package is missing ${join(packageRoot, relativePath)}`)
    }
  }
  const manifest = JSON.parse(await readFile(join(packageRoot, 'release.json'), 'utf8'))
  if (manifest.commit !== currentCommit || manifest.target !== `${process.platform}-${process.arch}`) {
    throw new Error(`release manifest does not identify the current Windows build: ${JSON.stringify(manifest)}`)
  }
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

async function authorizedJSON(port, path, init = {}) {
  const response = await fetch(`http://127.0.0.1:${port}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(init.headers ?? {}),
    },
  })
  const text = await response.text()
  let body = null
  try {
    body = text ? JSON.parse(text) : null
  } catch {
    // Keep the raw response for the assertion below.
  }
  return { response, body, text }
}

async function createDurableSession(port) {
  const { response, body, text } = await authorizedJSON(port, '/api/session.create', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      type: 'client-request',
      rpcId: `deployment-create-${port}`,
      method: 'session.create',
      payload: {},
    }),
  })
  const sessionId = body?.result?.value?.sessionId
  if (response.status !== 200 || body?.result?.ok !== true || typeof sessionId !== 'string' || sessionId.length === 0) {
    throw new Error(`session.create failed: status=${response.status} body=${text}`)
  }
  return sessionId
}

async function verifyDurableSession(port, sessionId) {
  const sessions = await authorizedJSON(port, '/api/sessions')
  const events = await authorizedJSON(port, `/api/sessions/${encodeURIComponent(sessionId)}/events`)
  if (sessions.response.status !== 200 || !Array.isArray(sessions.body) || !sessions.body.some(item => item.id === sessionId)) {
    throw new Error(`persisted session missing after restart: ${sessionId}; status=${sessions.response.status} body=${sessions.text}`)
  }
  if (events.response.status !== 200) {
    throw new Error(`persisted session events unavailable after restart: ${sessionId}; status=${events.response.status} body=${events.text}`)
  }
  return { sessions: sessions.response.status, events: events.response.status }
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

async function stop(processHandle, force = false) {
  if (processHandle === undefined || processHandle.child.exitCode !== null) return
  if (force) {
    processHandle.child.kill('SIGKILL')
    await new Promise(resolvePromise => {
      processHandle.child.once('exit', resolvePromise)
    })
    return
  }
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

async function websocketUpgrade(port, path) {
  return await new Promise((resolvePromise, reject) => {
    const socket = connect(port, '127.0.0.1')
    let response = ''
    let settled = false
    const timer = setTimeout(() => {
      socket.destroy()
      reject(new Error(`timed out upgrading WebSocket ${path}`))
    }, 5_000)
    const finish = (error, result) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      socket.destroy()
      if (error) reject(error)
      else resolvePromise(result)
    }
    socket.once('error', error => finish(error))
    socket.on('data', chunk => {
      response += chunk.toString('latin1')
      if (!response.includes('\r\n\r\n')) return
      const status = response.split('\r\n', 1)[0]
      finish(null, status === 'HTTP/1.1 101 Switching Protocols')
    })
    socket.once('connect', () => {
      socket.write([
        `GET ${path} HTTP/1.1`,
        'Host: 127.0.0.1',
        'Connection: Upgrade',
        'Upgrade: websocket',
        'Sec-WebSocket-Version: 13',
        'Sec-WebSocket-Key: c2h1dHUtcGFjay13cw==',
        `Authorization: Bearer ${token}`,
        '',
        '',
      ].join('\r\n'))
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
      const unauthorizedHealth = await fetch(`${url}/api/health`)
      const health = await fetch(`${url}/api/health`, { headers })
      const sessions = await fetch(`${url}/api/sessions`, { headers })
      const staticShell = await fetch(`${url}/`, { headers })
      const native = await fetch(`${url}/api/host.describe`, {
        method: 'POST',
        headers: { ...headers, 'content-type': 'application/json' },
        body: JSON.stringify({ type: 'client-request', rpcId: `deployment-${port}`, method: 'host.describe', payload: {} }),
      })
      if (unauthorizedHealth.status === 401 && health.status === 200 && sessions.status === 200 && staticShell.status === 200 && native.status === 200) {
        const envelope = await native.json()
        if (envelope.type !== 'server-response' || envelope.rpcId !== `deployment-${port}` || envelope.result?.ok !== true) {
          throw new Error(`native host.describe returned an invalid response: ${JSON.stringify(envelope)}`)
        }
        const mux = await websocketUpgrade(port, '/api/events.mux')
        const host = await websocketUpgrade(port, '/api/events.host')
        if (!mux || !host) throw new Error('native WebSocket upgrade did not return 101')
        return { unauthorizedHealth: unauthorizedHealth.status, health: health.status, sessions: sessions.status, staticShell: staticShell.status, hostDescribe: native.status, webSockets: { mux: 101, host: 101 } }
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
  await verifyPackageLayout()
  const sharedData = join(temporaryRoot, 'shared-data')
  const versions = await Promise.all(ports.map(async (port, index) => {
    const directory = join(temporaryRoot, `version-${index + 1}`)
    await cp(packageRoot, directory, { recursive: true })
    await configurePackage(directory, port, sharedData)
    return directory
  }))

  active = start(versions[0])
  const initial = await waitForHealthy(active, ports[0])
  const sessionId = await createDurableSession(ports[0])
  const initialPersistence = await verifyDurableSession(ports[0], sessionId)
  await stop(active)
  active = undefined

  active = start(versions[1])
  const upgrade = await waitForHealthy(active, ports[1])
  const upgradePersistence = await verifyDurableSession(ports[1], sessionId)
  await stop(active)
  active = undefined

  active = start(versions[0])
  const rollback = await waitForHealthy(active, ports[0])
  const rollbackPersistence = await verifyDurableSession(ports[0], sessionId)
  await stop(active, true)
  active = undefined

  active = start(versions[0])
  const recovery = await waitForHealthy(active, ports[0])
  const recoveryPersistence = await verifyDurableSession(ports[0], sessionId)
  await stop(active)
  active = undefined

  console.log(JSON.stringify({ package: packageRoot, initial, upgrade, rollback, recovery, sessionId, persistence: { initial: initialPersistence, upgrade: upgradePersistence, rollback: rollbackPersistence, recovery: recoveryPersistence }, forcedStopRecovery: true, sharedData: true }))
} finally {
  await stop(active)
  await removeTemporaryRoot(temporaryRoot)
}
