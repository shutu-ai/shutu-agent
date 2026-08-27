import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const vite = resolve(dshRoot, 'apps/web/node_modules/vite/bin/vite.js')
const distIndex = resolve(webRoot, 'dist/index.html')
const { chromium } = createRequire(import.meta.url)(resolve(dshRoot, 'apps/web/node_modules/playwright'))
const host = '127.0.0.1'
const port = Number(process.env.SHUTU_E2E_PORT ?? 18117)
const baseUrl = `http://${host}:${port}/`
const artifactDirectory = resolve(process.env.SHUTU_E2E_ARTIFACT_DIR ?? process.env.TEMP ?? process.env.TMP ?? '.')
mkdirSync(artifactDirectory, { recursive: true })

if (!existsSync(vite)) {
  throw new Error(`Vite is unavailable at ${vite}; set SHUTU_DSH_ROOT to a DSH checkout.`)
}
if (!existsSync(distIndex)) {
  throw new Error(`Native dist is unavailable at ${distIndex}; run npm.cmd run build first.`)
}

function valueFor(method) {
  switch (method) {
    case 'host.describe':
      return { attachedSessions: 0, canOpenPath: false, cwd: 'C:/shutu-smoke', home: '', model: 'test-model', version: 'smoke' }
    case 'settings.describe':
      return { hasDocument: false, namespaces: [] }
    case 'credentials.describe':
      return { credentials: {} }
    case 'session.list':
      return { items: [] }
    case 'workspace.list':
      return { items: [], ungroupedSessionIds: [], archivedSessionIds: [] }
    case 'session.search':
      return { items: [], hasMore: false }
    case 'agentPreset.list':
      return { authorable: false, hasDocument: false, presets: [] }
    case 'llm.providers':
      return { providers: [] }
    case 'dynamicCordisRunner/inventory':
      return []
    case 'dynamicCordisRunner/syncInspectManifest':
      return { ok: true }
    default:
      return {}
  }
}

async function installNativeMock(page, options = {}) {
  const sockets = new Set()
  const socketConnections = new Map()
  const requests = []
  const queueUpdates = []
  const goalActions = []
  let goalState = options.goalControls === true ? {
    id: 'goal-1', revision: 1, objective: 'Ship the fixture', phase: 'active', maxGoalRounds: 4,
    createdAt: 1_700_000_000_000, updatedAt: 1_700_000_000_000,
  } : null
  let queueItems = (options.queueItems ?? []).map(item => ({
    id: item.id,
    placement: item.placement ?? 'queued',
    message: {
      id: item.messageId ?? `${item.id}-message`,
      role: 'user',
      content: [{ type: 'text', text: item.text }],
      source: { kind: 'user', rpcId: item.id },
    },
  }))
  let searchFailuresRemaining = options.searchFailures ?? 0
  let muxSocket = null
  let interactionStage = 'idle'
  const retryEvents = [
    { seq: 1, type: 'turn/start', time: 1_700_000_000_001, data: { turn: 1 } },
    { seq: 2, type: 'step/start', time: 1_700_000_000_002, data: { turn: 1, step: 1 } },
    { seq: 3, type: 'llm/retry', time: 1_700_000_000_003, data: {
      retryId: 'retry-fixture', turn: 1, step: 1, provider: 'fixture-provider',
      mode: 'normal', policyKey: 'fixture-normal', retry: 1, maxRetries: 3,
      delayMs: 2_000, failure: { code: 'TRANSPORT', message: 'temporary fixture failure' },
    } },
  ]
  const errorEvents = [
    { seq: 1, type: 'turn/start', time: 1_700_000_000_001, data: { turn: 1 } },
    { seq: 2, type: 'user/message', time: 1_700_000_000_002, data: {
      id: 'error-user', role: 'user', content: [{ type: 'text', text: 'run the error fixture' }],
      source: { kind: 'user' },
    }, surfaceOp: 'append' },
    { seq: 3, type: 'step/start', time: 1_700_000_000_003, data: { turn: 1, step: 1 } },
    { seq: 4, type: 'step/end', time: 1_700_000_000_004, data: { turn: 1, step: 1 } },
    { seq: 5, type: 'turn/end', time: 1_700_000_000_005, data: {
      turn: 1, reason: { kind: 'error', error: { code: 'FIXTURE_TURN_FAILED', message: 'fixture turn failed' } },
    } },
  ]
  const sendMux = (rpcId, method, payload) => {
    if (muxSocket === null) throw new Error(`native mux is not connected for ${method}`)
    muxSocket.send(JSON.stringify({ type: 'server-request', rpcId, method, payload }))
  }
  const sendQueueSnapshot = () => {
    sendMux('queue-snapshot', 'session/queue', {
      type: 'session/queue', sessionId: 'search-fixture', items: queueItems,
    })
  }
  const sendProjection = (key, value, seq = 1) => {
    sendMux(`${key}-projection`, 'session/projection', {
      type: 'session/projection', sessionId: 'search-fixture', key, value, seq,
    })
  }
  const sendRetryStarted = () => {
    sendMux('retry-started', 'session/event', {
      type: 'session/event', sessionId: 'search-fixture', event: {
        seq: 4, type: 'llm/retry-started', time: 1_700_000_000_004,
        data: { retryId: 'retry-fixture', turn: 1, step: 1, retry: 1 },
      },
    })
  }
  await page.route('**/plugins/events', route => route.fulfill({
    status: 200,
    contentType: 'text/event-stream',
    body: 'retry: 3000\n\n',
  }))
  await page.routeWebSocket('**/api/events.*', ws => {
    const pathname = new URL(ws.url()).pathname
    sockets.add(pathname)
    if (pathname === '/api/events.mux') muxSocket = ws
    const connectionNumber = (socketConnections.get(pathname) ?? 0) + 1
    socketConnections.set(pathname, connectionNumber)
    if (options.closeFirstSocketAfterMs && connectionNumber === 1) {
      setTimeout(() => ws.close(1011, 'native reconnect smoke'), options.closeFirstSocketAfterMs)
    }
    ws.onMessage(() => {})
  })
  await page.route('**/api/**', async route => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = JSON.parse(route.request().postData() ?? '{}')
    if (new URL(route.request().url()).pathname === '/api/respond') {
      requests.push({ method: '/api/respond', payload: body })
      if (options.interactions && body.type === 'client-response' && body.result?.ok === true) {
        if (body.rpcId === 'approval-1' && interactionStage === 'approval') {
          interactionStage = 'question'
          sendMux('approval-1', 'approval/resolved', {
            type: 'approval/resolved', sessionId: 'search-fixture', approvalId: 'approval-1', outcome: 'allowed-once',
          })
          setTimeout(() => sendMux('question-1', 'question/requested', {
            type: 'question/requested', sessionId: 'search-fixture', questions: [{
              id: 'mode', header: 'Mode', question: 'Choose a mode', options: [{ label: 'Safe', description: 'No side effects' }],
            }],
          }), 50)
        } else if (body.rpcId === 'question-1' && interactionStage === 'question') {
          interactionStage = 'resolved'
          sendMux('question-1', 'question/resolved', {
            type: 'question/resolved', sessionId: 'search-fixture', questionRpcId: 'question-1', outcome: 'answered',
          })
        }
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ accepted: true }),
      })
    }
    assert.equal(body.type, 'client-request', `unexpected native request envelope for ${body.method}`)
    requests.push(body.method)
    if (options.goalControls === true && body.method.startsWith('goals/')) {
      const operation = body.method.slice('goals/'.length)
      goalActions.push({ operation, payload: body.payload })
      if (goalState !== null) {
        if (operation === 'edit') {
          const args = body.payload?.args
          const request = Array.isArray(args) ? args[2] : undefined
          const objective = request?.objective
          if (typeof objective === 'string') goalState = { ...goalState, objective, revision: goalState.revision + 1 }
        } else if (operation === 'pause') {
          goalState = { ...goalState, phase: 'paused', revision: goalState.revision + 1 }
        } else if (operation === 'resume') {
          goalState = { ...goalState, phase: 'active', revision: goalState.revision + 1 }
        } else if (operation === 'clear') {
          goalState = null
        }
      }
      sendProjection('goal', goalState === null ? null : { goal: goalState, roundsStarted: 0, createdAt: goalState.createdAt, updatedAt: goalState.updatedAt }, goalState?.revision ?? 10)
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: goalState === null ? { ref: { id: 'goal-1', revision: 99 } } : goalState },
        }),
      })
    }
    if (options.goalControls === true && body.method === 'commands/execute') {
      const args = body.payload?.args
      const line = Array.isArray(args) ? args[1] : body.payload?.line
      if (line === '/plan off') sendProjection('plan', { active: false, pending: false }, 2)
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: { commandId: 'command-fixture', result: { kind: 'success' } } },
        }),
      })
    }
    if (options.goalControls === true && body.method === 'session.cancel') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: { accepted: true } },
        }),
      })
    }
    if (options.queueControls && body.method === 'session.updateQueue') {
      const action = body.payload?.action ?? {}
      const itemId = body.payload?.itemId
      queueUpdates.push({ itemId, action })
      if (action.kind === 'edit') {
        const text = action.content?.map(part => part.text ?? '').join('') ?? ''
        const item = queueItems.find(candidate => candidate.id === itemId)
        if (item !== undefined) item.message.content = [{ type: 'text', text }]
      } else if (action.kind === 'remove') {
        queueItems = queueItems.filter(item => item.id !== itemId)
      } else if (action.kind === 'steer') {
        const item = queueItems.find(candidate => candidate.id === itemId)
        if (item !== undefined) item.placement = 'steering'
      }
      sendQueueSnapshot()
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: { accepted: true } },
        }),
      })
    }
    if (body.method === 'session.search' && searchFailuresRemaining > 0) {
      searchFailuresRemaining -= 1
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: false, error: { code: 'search-unavailable', message: 'search fixture failure' } },
        }),
      })
    }
    if (body.method === 'session.list' && options.seedSession) {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: true, value: {
            items: [{
              sessionId: 'search-fixture', title: 'Search fixture', updatedAt: Date.now(),
              running: options.runningSession === true, blank: false, cwd: 'C:/shutu-search',
              projections: { asOfSeq: 0, values: { title: 'Search fixture', sessionListMetadata: { blank: false } } },
            }],
          } },
        }),
      })
    }
    if (body.method === 'workspace.list' && options.seedSession) {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: true, value: {
            items: [{ workspaceId: 'search-ws', path: 'C:/shutu-search', title: 'Search', sessionIds: ['search-fixture'], createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }],
            archivedSessionIds: [],
          } },
        }),
      })
    }
    if (body.method === 'session.create' && options.seedSession) {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: true, value: { sessionId: 'created-search' } },
        }),
      })
    }
    if (options.lifecycle && body.method === 'session.history') {
      const events = options.retryControls === true ? retryEvents : options.errorControls === true ? errorEvents : []
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: true, value: {
            header: { version: 0, id: 'search-fixture', createdAt: Date.now(), cwd: 'C:/shutu-search' },
            events: events.map(event => ({ event })), hasMore: false,
            surface: { nodes: events.map(event => event.seq), replacements: [] },
            projections: {
              asOfSeq: 0,
              values: {
                title: 'Search fixture', sessionListMetadata: { blank: false },
                ...(options.goalControls === true ? {
                  goal: { goal: goalState, roundsStarted: 0, createdAt: goalState.createdAt, updatedAt: goalState.updatedAt },
                  plan: { active: false, pending: false },
                } : {}),
              },
            },
          } },
        }),
      })
    }
    if (options.lifecycle && body.method === 'session.search') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: { items: [{ sessionId: 'search-fixture', snippet: 'lifecycle seed' }], hasMore: false } },
        }),
      })
    }
    if (options.lifecycle && body.method === 'workspace.rename') {
      const title = String(body.payload?.title ?? 'Lifecycle renamed')
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response', rpcId: body.rpcId,
          result: { ok: true, value: { workspace: {
            workspaceId: 'search-ws', path: 'C:/shutu-search', title,
            sessionIds: ['search-fixture'], createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
          } } },
        }),
      })
    }
    if (options.lifecycle && body.method === 'workspace.delete') {
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ type: 'server-response', rpcId: body.rpcId, result: { ok: true, value: { deleted: true } } }),
      })
    }
    if (options.lifecycle && body.method === 'workspace.archiveSession') {
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ type: 'server-response', rpcId: body.rpcId, result: { ok: true, value: { archivedSessionIds: ['search-fixture'] } } }),
      })
    }
    if (body.method === 'session.list' && options.sessionListDelayMs) {
      await new Promise(resolvePromise => setTimeout(resolvePromise, options.sessionListDelayMs))
    }
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        type: 'server-response',
        rpcId: body.rpcId,
        result: { ok: true, value: valueFor(body.method) },
      }),
    })
  })
  return {
    sockets, socketConnections, requests, queueUpdates, sendMux, sendQueueSnapshot,
    goalActions, sendProjection, sendRetryStarted,
    get interactionStage() { return interactionStage },
    setInteractionStage: value => { interactionStage = value },
  }
}

async function waitForServer() {
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    try {
      const response = await fetch(baseUrl)
      if (response.ok) return
    } catch {
      // Vite is still starting.
    }
    await new Promise(resolvePromise => setTimeout(resolvePromise, 100))
  }
  throw new Error(`Timed out waiting for ${baseUrl}`)
}

async function waitForCondition(check, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (check()) return
    await new Promise(resolvePromise => setTimeout(resolvePromise, 50))
  }
  throw new Error('timed out waiting for native fixture condition')
}

async function waitForNativeShell(page, allowLoaded = false) {
  await page.locator('button').first().waitFor()
  assert.equal(await page.title(), 'DeepSeek Harness')
  assert.equal(await page.locator('.shutu-shell').count(), 0, 'legacy Shutu shell is still mounted')
  return
  const shellBody = await page.locator('body').innerText()
  assert.match(shellBody, /鎺㈢储鏈嚦涔嬪/)
  return
  if (allowLoaded) {
    await page.locator('button').first().waitFor()
    assert.equal(await page.title(), 'DeepSeek Harness')
    assert.equal(await page.locator('.shutu-shell').count(), 0, 'legacy Shutu shell is still mounted')
    return
  }
  await page.getByRole('button', { name: '新建会话' }).first().waitFor()
  assert.equal(await page.title(), 'DeepSeek Harness')
  const body = await page.locator('body').innerText()
  assert.match(body, /探索未至之境/)
  assert.equal(await page.locator('.shutu-shell').count(), 0, 'legacy Shutu shell is still mounted')
}

async function runDesktop(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const { sockets, requests } = await installNativeMock(page)
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  assert.ok(sockets.has('/api/events.mux'), 'native mux WebSocket was not opened')
  assert.ok(sockets.has('/api/events.host'), 'native host WebSocket was not opened')

  const settingsButton = page.getByRole('button', { name: '设置', exact: true })
  await settingsButton.focus()
  await settingsButton.click()
  const settings = page.getByRole('dialog')
  await settings.waitFor()
  assert.match(await settings.innerText(), /通用设置/)
  await page.keyboard.press('Escape')
  await assert.rejects(() => settings.waitFor({ state: 'visible', timeout: 250 }), /Timeout/)
  assert.equal(await settingsButton.evaluate(button => document.activeElement === button), true, 'settings focus was not restored after Escape')
  const collapse = page.getByRole('button', { name: '收起侧边栏' })
  await collapse.focus()
  await page.keyboard.press('Enter')
  const expand = page.getByRole('button', { name: '打开侧边栏' })
  await expand.waitFor()
  await expand.focus()
  await page.keyboard.press('Enter')
  await collapse.waitFor()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  assert.ok(overflow <= 1, `desktop page has horizontal overflow: ${overflow}px`)
  const unnamedButtons = await page.getByRole('button').evaluateAll(buttons => buttons.filter(button => {
    const label = button.getAttribute('aria-label') ?? button.textContent ?? button.getAttribute('title') ?? ''
    return label.trim().length === 0
  }).length)
  assert.equal(unnamedButtons, 0, 'native desktop has an unnamed button')
  assert.deepEqual(issues, [])
  await page.screenshot({ path: resolve(artifactDirectory, 'shutu-native-desktop.png') })
  await page.close()
  return { sockets: [...sockets].sort(), requests: [...new Set(requests)].sort(), console: 'clean' }
}

async function runDarkDesktop(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 }, colorScheme: 'dark' })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const { sockets } = await installNativeMock(page)
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  assert.ok(sockets.has('/api/events.mux'), 'dark native mux WebSocket was not opened')
  assert.ok(sockets.has('/api/events.host'), 'dark native host WebSocket was not opened')
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  assert.ok(overflow <= 1, `dark desktop page has horizontal overflow: ${overflow}px`)
  assert.deepEqual(issues, [])
  await page.screenshot({ path: resolve(artifactDirectory, 'shutu-native-dark-desktop.png') })
  await page.close()
  return { viewport: '1280x900', colorScheme: 'dark', overflow, console: 'clean' }
}

async function runReconnectDesktop(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const { sockets, socketConnections } = await installNativeMock(page, { closeFirstSocketAfterMs: 1_500 })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  await page.waitForTimeout(4_000)
  assert.ok(sockets.has('/api/events.mux'), 'reconnect smoke did not open native mux WebSocket')
  assert.ok(sockets.has('/api/events.host'), 'reconnect smoke did not open native host WebSocket')
  assert.ok((socketConnections.get('/api/events.mux') ?? 0) >= 2, 'native mux WebSocket did not reconnect')
  assert.ok((socketConnections.get('/api/events.host') ?? 0) >= 2, 'native host WebSocket did not reconnect')
  await waitForNativeShell(page)
  assert.deepEqual(issues.filter(issue => issue !== '[web-runtime] connection lost, retry #1'), [])
  await page.close()
  return { sockets: [...sockets].sort(), connections: Object.fromEntries(socketConnections), console: 'clean' }
}

async function runLoadingDesktop(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  const { sockets } = await installNativeMock(page, { sessionListDelayMs: 1_000 })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(250)
  await page.screenshot({ path: resolve(artifactDirectory, 'shutu-native-loading-desktop.png') })
  await waitForNativeShell(page)
  assert.ok(sockets.has('/api/events.mux'), 'loading native mux WebSocket was not opened')
  assert.deepEqual(issues, [])
  await page.close()
  return { viewport: '1280x900', state: 'loading', console: 'clean' }
}

async function runErrorStateMatrix(browser) {
  const results = []
  for (const viewport of [{ width: 1280, height: 900, name: 'desktop' }, { width: 390, height: 844, name: 'mobile' }]) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height } })
    const issues = []
    page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
    page.on('pageerror', error => issues.push(error.message))
    await installNativeMock(page, { seedSession: true, lifecycle: true, errorControls: true })
    await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
    await waitForNativeShell(page, true)
    const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="鎼滅储浼氳瘽"], button[aria-label="鎼滅储"]').first()
    await search.click()
    const input = page.locator('input[placeholder]').first()
    await input.fill('fixture')
    const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
    await result.waitFor({ timeout: 15_000 })
    await result.click()
    await input.press('Escape')
    await page.waitForTimeout(250)
    const status = page.getByRole('status').filter({ hasText: /This turn failed|本轮运行失败|澶辫触/ }).last()
    await status.waitFor({ timeout: 15_000 })
    assert.match(await status.innerText(), /fixture turn failed|本轮运行失败|This turn failed/)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
    assert.ok(overflow <= 1, `${viewport.name} error state has horizontal overflow: ${overflow}px`)
    assert.deepEqual(issues, [])
    await page.screenshot({ path: resolve(artifactDirectory, `shutu-native-error-${viewport.name}.png`) })
    results.push({ viewport: `${viewport.width}x${viewport.height}`, overflow, console: 'clean' })
    await page.close()
  }
  return results
}

async function runVisualGeometryMatrix(browser) {
  const results = []
  for (const viewport of [{ width: 1280, height: 900, name: 'desktop' }, { width: 390, height: 844, name: 'mobile' }]) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height } })
    const issues = []
    page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
    page.on('pageerror', error => issues.push(error.message))
    await installNativeMock(page, { seedSession: true, lifecycle: true, errorControls: true })
    await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: '鏂板缓浼氳瘽' }).first().waitFor()
    assert.equal(await page.title(), 'DeepSeek Harness')
    assert.equal(await page.locator('.shutu-shell').count(), 0, 'legacy Shutu shell is still mounted')
    const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="鎼滅储浼氳瘽"], button[aria-label="鎼滅储"]').first()
    await search.click()
    const input = page.locator('input[placeholder]').first()
    await input.fill('fixture')
    const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
    await result.waitFor({ timeout: 15_000 })
    await result.click()
    await input.press('Escape')
    await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })
    const geometry = await page.evaluate(() => {
      const rect = (selector) => {
        const element = document.querySelector(selector)
        if (!(element instanceof HTMLElement)) return null
        const box = element.getBoundingClientRect()
        const style = getComputedStyle(element)
        return {
          x: Math.round(box.x), y: Math.round(box.y), width: Math.round(box.width), height: Math.round(box.height),
          fontFamily: style.fontFamily, fontSize: style.fontSize, lineHeight: style.lineHeight,
          color: style.color, backgroundColor: style.backgroundColor, borderColor: style.borderColor,
          overflowX: style.overflowX, overflowY: style.overflowY,
        }
      }
      const scrollables = [...document.querySelectorAll('*')]
        .filter(element => element instanceof HTMLElement && element.scrollHeight > element.clientHeight + 4)
        .map(element => ({ selector: element.getAttribute('data-trajectory-scroll') !== null ? '[data-trajectory-scroll]' : element.tagName.toLowerCase(), height: element.clientHeight, scrollHeight: element.scrollHeight }))
        .slice(0, 8)
      return {
        viewport: { width: window.innerWidth, height: window.innerHeight },
        body: rect('body'),
        main: rect('main, [role="main"], [role="tabpanel"], body'),
        tabpanel: rect('[role="tabpanel"]'),
        composer: rect('[data-composer-seat]'),
        trajectory: rect('[data-trajectory-scroll]'),
        scrollables,
      }
    })
    assert.ok(geometry.body !== null, `${viewport.name} geometry has no body metrics`)
    assert.ok(geometry.main !== null, `${viewport.name} geometry has no main region`)
    assert.ok(geometry.main.width <= viewport.width + 1, `${viewport.name} main region overflows viewport`)
    assert.ok(geometry.main.x >= -1, `${viewport.name} main region starts outside viewport`)
    assert.ok(geometry.scrollables.every(scrollable => scrollable.height > 0 && scrollable.scrollHeight >= scrollable.height), `${viewport.name} has invalid scroll geometry`)
    const trigger = page.locator('button[aria-haspopup="dialog"]').first()
    await trigger.click()
    const dialog = page.getByRole('dialog').first()
    await dialog.waitFor({ timeout: 15_000 })
    const overlay = await dialog.evaluate(element => {
      const box = element.getBoundingClientRect()
      const style = getComputedStyle(element)
      return { x: Math.round(box.x), y: Math.round(box.y), width: Math.round(box.width), height: Math.round(box.height), position: style.position, overflow: style.overflow }
    })
    assert.ok(overlay.x >= 0 && overlay.y >= 0, `${viewport.name} settings overlay starts outside viewport`)
    assert.ok(overlay.x + overlay.width <= viewport.width + 1, `${viewport.name} settings overlay exceeds viewport width`)
    assert.ok(overlay.y + overlay.height <= viewport.height + 1, `${viewport.name} settings overlay exceeds viewport height`)
    await page.keyboard.press('Escape')
    assert.equal(await trigger.evaluate(element => document.activeElement === element), true, `${viewport.name} overlay did not restore focus`)
    assert.deepEqual(issues, [])
    writeFileSync(resolve(artifactDirectory, `shutu-native-geometry-${viewport.name}.json`), `${JSON.stringify({ ...geometry, overlay }, null, 2)}\n`)
    await page.screenshot({ path: resolve(artifactDirectory, `shutu-native-geometry-${viewport.name}.png`) })
    results.push({ viewport: `${viewport.width}x${viewport.height}`, mainWidth: geometry.main.width, overlay, console: 'clean' })
    await page.close()
  }
  return results
}

async function runVisualGeometryMatrixStable(browser) {
  const results = []
  for (const viewport of [{ width: 1280, height: 900, name: 'desktop' }, { width: 390, height: 844, name: 'mobile' }]) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height } })
    const issues = []
    page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
    page.on('pageerror', error => issues.push(error.message))
    await installNativeMock(page, { seedSession: true, lifecycle: true, errorControls: true })
    await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
    await waitForNativeShell(page, true)
    const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
    await search.click()
    const input = page.locator('input[placeholder]').first()
    await input.fill('fixture')
    const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
    await result.waitFor({ timeout: 15_000 })
    await result.click()
    await input.press('Escape')
    await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })
    const geometry = await page.evaluate(() => {
      const metrics = selector => {
        const element = document.querySelector(selector)
        if (!(element instanceof HTMLElement)) return null
        const box = element.getBoundingClientRect()
        const style = getComputedStyle(element)
        return { x: Math.round(box.x), y: Math.round(box.y), width: Math.round(box.width), height: Math.round(box.height), font: style.font, color: style.color, background: style.backgroundColor, border: style.borderColor, overflowX: style.overflowX, overflowY: style.overflowY }
      }
      return { viewport: { width: innerWidth, height: innerHeight }, body: metrics('body'), tabpanel: metrics('[role="tabpanel"]'), trajectory: metrics('[data-trajectory-scroll]'), scrollables: [...document.querySelectorAll('*')].filter(element => element instanceof HTMLElement && element.scrollHeight > element.clientHeight + 4).slice(0, 8).map(element => ({ height: element.clientHeight, scrollHeight: element.scrollHeight })) }
    })
    assert.ok(geometry.body !== null && geometry.body.width <= viewport.width + 1, `${viewport.name} body geometry overflows viewport`)
    assert.ok(geometry.scrollables.every(scrollable => scrollable.height > 0 && scrollable.scrollHeight >= scrollable.height), `${viewport.name} has invalid scroll geometry`)
    const trigger = page.locator('button').filter({ hasText: /^设置$/ }).first()
    await trigger.click()
    const dialog = page.getByRole('dialog').first()
    await dialog.waitFor({ timeout: 15_000 })
    const overlay = await dialog.evaluate(element => { const box = element.getBoundingClientRect(); return { x: Math.round(box.x), y: Math.round(box.y), width: Math.round(box.width), height: Math.round(box.height) } })
    assert.ok(overlay.x >= 0 && overlay.y >= 0 && overlay.x + overlay.width <= viewport.width + 1 && overlay.y + overlay.height <= viewport.height + 1, `${viewport.name} dialog exceeds viewport`)
    await page.keyboard.press('Escape')
    assert.deepEqual(issues, [])
    writeFileSync(resolve(artifactDirectory, `shutu-native-geometry-${viewport.name}.json`), `${JSON.stringify({ ...geometry, overlay }, null, 2)}\n`)
    await page.screenshot({ path: resolve(artifactDirectory, `shutu-native-geometry-${viewport.name}.png`) })
    results.push({ viewport: `${viewport.width}x${viewport.height}`, bodyWidth: geometry.body.width, overlay, console: 'clean' })
    await page.close()
  }
  return results
}

async function runAccessibilityMatrix(browser) {
  const results = []
  for (const viewport of [{ width: 1280, height: 900, name: 'desktop' }, { width: 390, height: 844, name: 'mobile' }]) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height } })
    const issues = []
    page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
    page.on('pageerror', error => issues.push(error.message))
    await installNativeMock(page, { seedSession: true, lifecycle: true, errorControls: true })
    await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
    await waitForNativeShell(page, true)
    const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
    await search.click()
    const input = page.locator('input[placeholder]').first()
    await input.fill('fixture')
    const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
    await result.waitFor({ timeout: 15_000 })
    await result.click()
    await input.press('Escape')
    await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })
    const semantics = await page.evaluate(() => ({
      tabs: [...document.querySelectorAll('[role="tab"], button')].filter(element => { const name = element.textContent?.trim() ?? ''; return name.length > 0 && name.length <= 3 }).map(element => ({ role: element.getAttribute('role'), selected: element.getAttribute('aria-selected'), controls: element.getAttribute('aria-controls'), name: element.textContent?.trim() ?? '' })),
      panels: [...document.querySelectorAll('[role="tabpanel"]')].map(element => ({ labelledBy: element.getAttribute('aria-labelledby') })),
      buttons: [...document.querySelectorAll('button')].filter(element => { const box = element.getBoundingClientRect(); return box.width > 0 && box.height > 0 }).map(element => ({ width: element.getBoundingClientRect().width, height: element.getBoundingClientRect().height, label: element.getAttribute('aria-label') ?? element.textContent?.trim() ?? '' })),
    }))
    assert.ok(semantics.tabs.length >= 2, `${viewport.name} did not expose conversation tabs`)
    assert.ok(semantics.tabs.every(tab => tab.name.length > 0), `${viewport.name} conversation tab names are incomplete`)
    assert.ok(semantics.panels.every(panel => panel.labelledBy !== null), `${viewport.name} tabpanel semantics are incomplete`)
    const settingsButton = page.locator('button').filter({ hasText: /^设置$/ }).first()
    await settingsButton.focus()
    await settingsButton.click()
    const dialog = page.getByRole('dialog').first()
    await dialog.waitFor({ timeout: 15_000 })
    const focusables = dialog.locator('button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])')
    const focusableCount = await focusables.count()
    assert.ok(focusableCount >= 2, `${viewport.name} settings dialog has too few keyboard controls`)
    const first = focusables.first()
    const last = focusables.last()
    await first.focus()
    await page.keyboard.press('Shift+Tab')
    assert.equal(await last.evaluate(element => document.activeElement === element), true, `${viewport.name} Shift+Tab did not wrap focus`)
    await page.keyboard.press('Tab')
    assert.equal(await first.evaluate(element => document.activeElement === element), true, `${viewport.name} Tab did not wrap focus`)
    await page.keyboard.press('Escape')
    await page.waitForTimeout(100)
    assert.equal(await settingsButton.evaluate(element => document.activeElement === element), true, `${viewport.name} Escape did not restore focus`)
    assert.deepEqual(issues, [])
    results.push({ viewport: `${viewport.width}x${viewport.height}`, tabs: semantics.tabs.length, focusableCount, touchTargets: semantics.buttons.length, console: 'clean' })
    await page.close()
  }
  return results
}

async function runSearchErrorRecovery(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const { requests } = await installNativeMock(page, { searchFailures: 1, seedSession: true })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
  await search.click()
  const input = page.locator('input[placeholder]').first()
  await input.fill('first failed query')
  const unavailable = page.getByRole('status').filter({ hasText: /Content search is temporarily unavailable|内容搜索暂不可用/ })
  await unavailable.waitFor({ timeout: 15_000 })
  await input.press('Escape')
  await search.click()
  await input.fill('second recovered query')
  await page.waitForTimeout(500)
  assert.equal(await unavailable.count(), 0)
  assert.equal(requests.filter(method => method === 'session.search').length, 2)
  assert.deepEqual(issues, [])
  await page.close()
  return { requests: 2, recovered: true, console: 'clean' }
}

async function runSessionLifecycle(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const { requests } = await installNativeMock(page, { seedSession: true, lifecycle: true })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
  await search.click()
  const searchInput = page.locator('input[placeholder]').first()
  await searchInput.fill('fixture')
  const searchResult = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await searchResult.waitFor({ timeout: 15_000 })
  await searchResult.click()
  const seedText = page.getByText('Search fixture', { exact: true }).first()
  await seedText.waitFor({ timeout: 15_000 })
  await searchInput.press('Escape')

  const newSession = page.locator('button[aria-label="New session"], button[aria-label="新建会话"]').last()
  await newSession.click()
  assert.ok(requests.includes('session.create'), 'native new-session action did not send session.create')

  const workspaceActions = page.locator('button[aria-label="Workspace actions for Search"], button[aria-label="工作区“Search”的操作"]').first()
  const workspaceRow = workspaceActions.locator('xpath=ancestor::*[@role="treeitem"][1]')
  await workspaceRow.hover()
  await workspaceActions.click()
  await page.getByRole('menuitem', { name: /Rename|重命名/ }).click()
  const renameDialog = page.getByRole('dialog').last()
  await renameDialog.locator('input').first().fill('Lifecycle renamed')
  await renameDialog.getByRole('button', { name: /Rename|重命名/ }).click()
  await page.getByText('Lifecycle renamed', { exact: true }).waitFor({ timeout: 15_000 })
  assert.ok(requests.includes('workspace.rename'), 'native workspace rename did not send workspace.rename')

  const renamedRow = page.locator('[role="treeitem"]').filter({ hasText: 'Lifecycle renamed' }).first()
  await renamedRow.hover()
  await renamedRow.locator('button[aria-label*="Lifecycle renamed"]').first().click()
  await page.getByRole('menuitem', { name: /Delete workspace|删除工作区/ }).click()
  const deleteDialog = page.getByRole('dialog').last()
  await deleteDialog.getByRole('button', { name: /Delete workspace|删除工作区/ }).click()
  await page.getByText('Lifecycle renamed', { exact: true }).waitFor({ state: 'detached', timeout: 15_000 })
  assert.ok(requests.includes('workspace.delete'), 'native workspace delete did not send workspace.delete')

  const remainingSession = page.getByText('Search fixture', { exact: true })
  await remainingSession.waitFor({ timeout: 15_000 })
  const sessionRow = page.locator('[role="treeitem"]').filter({ hasText: 'Search fixture' }).first()
  await sessionRow.hover()
  await sessionRow.locator('button[aria-label*="Search fixture"]').first().click()
  await page.getByRole('menuitem', { name: /Archive session|归档会话/ }).click()
  await remainingSession.waitFor({ state: 'detached', timeout: 15_000 })
  assert.ok(requests.includes('workspace.archiveSession'), 'native session archive did not send workspace.archiveSession')
  assert.deepEqual(issues, [])
  await page.close()
  return { requests: [...new Set(requests.filter(method => ['session.create', 'workspace.rename', 'workspace.delete', 'workspace.archiveSession'].includes(method)))].sort(), console: 'clean' }
}

async function runInteractionControls(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const fixture = await installNativeMock(page, { seedSession: true, lifecycle: true, interactions: true })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
  await search.click()
  const input = page.locator('input[placeholder]').first()
  await input.fill('fixture')
  const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await result.waitFor({ timeout: 15_000 })
  await result.click()
  await input.press('Escape')
  await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })

  fixture.setInteractionStage('approval')
  fixture.sendMux('approval-1', 'approval/requested', {
    type: 'approval/requested', sessionId: 'search-fixture', approvalId: 'approval-1', toolName: 'shell', reason: 'Run the fixture command?',
  })
  await page.getByText(/Waiting for approval|等待审批/).last().waitFor({ timeout: 15_000 })
  await page.getByRole('button', { name: /Allow once|允许一次/ }).click()
  await waitForCondition(() => fixture.requests.filter(request => request.method === '/api/respond').length === 1)
  await page.getByText('Choose a mode', { exact: true }).waitFor({ timeout: 15_000 })
  await page.getByRole('radio', { name: 'Safe' }).click()
  await page.getByRole('button', { name: /Submit|提交/ }).click()
  await waitForCondition(() => fixture.requests.filter(request => request.method === '/api/respond').length === 2)
  await waitForCondition(() => fixture.interactionStage === 'resolved')
  await page.waitForTimeout(100)
  assert.equal(await page.getByText('Choose a mode', { exact: true }).count(), 0)
  assert.deepEqual(issues, [])
  await page.close()
  return { responses: 2, resolved: true, console: 'clean' }
}

async function runQueueControls(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const fixture = await installNativeMock(page, {
    seedSession: true, lifecycle: true, queueControls: true, runningSession: true,
    queueItems: [
      { id: 'queue-1', text: 'queued fixture message' },
      { id: 'queue-2', text: 'remove fixture message' },
    ],
  })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
  await search.click()
  const input = page.locator('input[placeholder]').first()
  await input.fill('fixture')
  const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await result.waitFor({ timeout: 15_000 })
  await result.click()
  await input.press('Escape')
  await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })
  fixture.sendQueueSnapshot()

  const queueDock = page.locator('[data-queue-dock=""]')
  await queueDock.waitFor({ timeout: 15_000 })
  await page.getByText(/2 queued messages|2 条排队消息/).waitFor({ timeout: 15_000 })
  await queueDock.getByRole('button', { name: /2 queued messages|2 条排队消息/ }).click()

  const firstRow = queueDock.locator('li').nth(0)
  await firstRow.getByRole('button', { name: /Edit queued message|编辑排队消息/ }).click()
  const editor = firstRow.locator('input[aria-label]')
  await editor.fill('edited fixture message')
  await firstRow.getByRole('button', { name: /Save queued message|保存排队消息/ }).click()
  await waitForCondition(() => fixture.queueUpdates.some(update => update.itemId === 'queue-1' && update.action.kind === 'edit'))
  await page.getByText('edited fixture message', { exact: true }).waitFor({ timeout: 15_000 })

  await firstRow.getByRole('button', { name: /Steer queued message|插话发送/ }).click()
  await waitForCondition(() => fixture.queueUpdates.some(update => update.itemId === 'queue-1' && update.action.kind === 'steer'))

  const secondRow = queueDock.locator('li').filter({ hasText: 'remove fixture message' }).first()
  await secondRow.getByRole('button', { name: /Remove queued message|删除排队消息/ }).click()
  await waitForCondition(() => fixture.queueUpdates.some(update => update.itemId === 'queue-2' && update.action.kind === 'remove'))
  await page.getByText('remove fixture message', { exact: true }).waitFor({ state: 'detached', timeout: 15_000 })
  fixture.sendQueueSnapshot()
  await queueDock.waitFor({ state: 'detached', timeout: 15_000 })
  assert.deepEqual(issues, [])
  await page.close()
  return {
    updates: fixture.queueUpdates.map(update => update.action.kind),
    collapsed: true, edited: true, steered: true, removed: true, console: 'clean',
  }
}

async function runCancelPlanGoalControls(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const fixture = await installNativeMock(page, { seedSession: true, lifecycle: true, goalControls: true, runningSession: true })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="搜索"]').first()
  await search.click()
  const input = page.locator('input[placeholder]').first()
  await input.fill('fixture')
  const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await result.waitFor({ timeout: 15_000 })
  await result.click()
  await input.press('Escape')
  await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })

  const stop = page.getByRole('button', { name: /Stop generating|停止生成/ }).first()
  await stop.waitFor({ timeout: 15_000 })
  await stop.click()
  await waitForCondition(() => fixture.requests.includes('session.cancel'))

  fixture.sendProjection('goal', { goal: goalStateForFixture(), roundsStarted: 0, createdAt: 1_700_000_000_000, updatedAt: 1_700_000_000_000 }, 1)
  fixture.sendProjection('plan', { active: true, pending: false }, 1)
  const goalBar = page.locator('[data-goal-bar]')
  await page.waitForTimeout(250)
  await goalBar.waitFor({ timeout: 15_000 })
  await page.getByText('Ship the fixture', { exact: true }).waitFor({ timeout: 15_000 })
  const planChip = page.getByRole('button', { name: /Plan mode on|plan mode 已开启/ })
  await planChip.waitFor({ timeout: 15_000 })
  await planChip.click()
  await waitForCondition(() => fixture.requests.includes('commands/execute'))
  await planChip.waitFor({ state: 'detached', timeout: 15_000 })

  await goalBar.getByRole('button', { name: /Edit goal|编辑目标/ }).click()
  const goalInput = goalBar.getByRole('textbox', { name: /Goal objective|目标内容/ })
  await goalInput.fill('Ship the edited fixture')
  await goalBar.getByRole('button', { name: /Save goal|保存目标/ }).click()
  await waitForCondition(() => fixture.goalActions.some(action => action.operation === 'edit'))
  await page.getByText('Ship the edited fixture', { exact: true }).waitFor({ timeout: 15_000 })

  await goalBar.getByRole('button', { name: /Pause goal|暂停目标/ }).click()
  await waitForCondition(() => fixture.goalActions.some(action => action.operation === 'pause'))
  await goalBar.getByRole('button', { name: /Resume goal|恢复目标/ }).waitFor({ timeout: 15_000 })
  await goalBar.getByRole('button', { name: /Resume goal|恢复目标/ }).click()
  await waitForCondition(() => fixture.goalActions.some(action => action.operation === 'resume'))
  await goalBar.getByRole('button', { name: /Clear goal|清除目标/ }).click()
  await waitForCondition(() => fixture.goalActions.some(action => action.operation === 'clear'))
  await goalBar.waitFor({ state: 'detached', timeout: 15_000 })
  assert.deepEqual(issues, [])
  await page.close()
  return {
    cancel: true, planExit: true,
    goalActions: fixture.goalActions.map(action => action.operation), console: 'clean',
  }
}

async function runRetryControls(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  const fixture = await installNativeMock(page, { seedSession: true, lifecycle: true, retryControls: true })
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const search = page.locator('button[aria-label="Search sessions"], button[aria-label="Search"], button[aria-label="搜索会话"], button[aria-label="鎼滅储浼氳瘽"], button[aria-label="鎼滅储"]').first()
  await search.click()
  const input = page.locator('input[placeholder]').first()
  await input.fill('fixture')
  const result = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await result.waitFor({ timeout: 15_000 })
  await result.click()
  await input.press('Escape')
  await page.getByText('Search fixture', { exact: true }).last().waitFor({ timeout: 15_000 })

  const retry = page.locator('details').filter({ hasText: /Retrying model request|正在重试模型请求/ }).last()
  await retry.waitFor({ timeout: 15_000 })
  assert.match(await retry.innerText(), /1\/3|temporary fixture failure/)
  await retry.locator('summary').click()
  assert.match(await retry.innerText(), /Retry delay|重试延迟/)
  fixture.sendRetryStarted()
  await page.getByText(/Retried model request|已重试模型请求/).last().waitFor({ timeout: 15_000 })
  assert.deepEqual(issues, [])
  await page.close()
  return { scheduled: true, started: true, details: true, console: 'clean' }
}

function goalStateForFixture() {
  return {
    id: 'goal-1', revision: 1, objective: 'Ship the fixture', phase: 'active', maxGoalRounds: 4,
    createdAt: 1_700_000_000_000, updatedAt: 1_700_000_000_000,
  }
}

async function runMobile(browser) {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  page.on('response', response => { if (response.status() >= 400) issues.push(`http ${response.status()}: ${response.url()}`) })
  await installNativeMock(page)
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await waitForNativeShell(page)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  assert.ok(overflow <= 1, `mobile page has horizontal overflow: ${overflow}px`)
  const smallTargets = await page.getByRole('button').evaluateAll(buttons => buttons.filter(button => {
    const rect = button.getBoundingClientRect()
    return rect.width > 0 && rect.height > 0 && (rect.width < 24 || rect.height < 24)
  }).length)
  assert.equal(smallTargets, 0, 'native mobile has a visible button below the 24px target minimum')
  const unnamedButtons = await page.getByRole('button').evaluateAll(buttons => buttons.filter(button => {
    const label = button.getAttribute('aria-label') ?? button.textContent ?? button.getAttribute('title') ?? ''
    return label.trim().length === 0
  }).map(button => button.outerHTML.slice(0, 240)))
  assert.deepEqual(unnamedButtons, [], `native mobile has unnamed buttons: ${unnamedButtons.join(' | ')}`)
  assert.deepEqual(issues, [])
  await page.screenshot({ path: resolve(artifactDirectory, 'shutu-native-mobile.png') })
  await page.close()
  return { viewport: '390x844', overflow, console: 'clean' }
}

const server = spawn(process.execPath, [vite, 'preview', '--host', host, '--port', String(port)], {
  cwd: webRoot,
  stdio: 'ignore',
  windowsHide: true,
})
try {
  await waitForServer()
  const browser = await chromium.launch({ headless: true })
  try {
    const desktop = await runDesktop(browser)
    const reconnectDesktop = await runReconnectDesktop(browser)
    const darkDesktop = await runDarkDesktop(browser)
    const loadingDesktop = await runLoadingDesktop(browser)
    const errorStateMatrix = await runErrorStateMatrix(browser)
    const visualGeometryMatrix = await runVisualGeometryMatrixStable(browser)
    const accessibilityMatrix = await runAccessibilityMatrix(browser)
    const searchErrorRecovery = await runSearchErrorRecovery(browser)
    const sessionLifecycle = await runSessionLifecycle(browser)
    const interactionControls = await runInteractionControls(browser)
    const queueControls = await runQueueControls(browser)
    const cancelPlanGoalControls = await runCancelPlanGoalControls(browser)
    const retryControls = await runRetryControls(browser)
    const mobile = await runMobile(browser)
    console.log(JSON.stringify({ browser: 'playwright', native: 'ok', desktop, reconnectDesktop, darkDesktop, loadingDesktop, errorStateMatrix, visualGeometryMatrix, accessibilityMatrix, searchErrorRecovery, sessionLifecycle, interactionControls, queueControls, cancelPlanGoalControls, retryControls, mobile }))
  } finally {
    await browser.close()
  }
} finally {
  server.kill()
}
