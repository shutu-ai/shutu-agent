import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync } from 'node:fs'
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
  const sendMux = (rpcId, method, payload) => {
    if (muxSocket === null) throw new Error(`native mux is not connected for ${method}`)
    muxSocket.send(JSON.stringify({ type: 'server-request', rpcId, method, payload }))
  }
  const sendQueueSnapshot = () => {
    sendMux('queue-snapshot', 'session/queue', {
      type: 'session/queue', sessionId: 'search-fixture', items: queueItems,
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
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          type: 'server-response',
          rpcId: body.rpcId,
          result: { ok: true, value: {
            header: { version: 0, id: 'search-fixture', createdAt: Date.now(), cwd: 'C:/shutu-search' },
            events: [], hasMore: false, surface: { nodes: [], replacements: [] },
            projections: { asOfSeq: 0, values: { title: 'Search fixture', sessionListMetadata: { blank: false } } },
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

async function waitForNativeShell(page) {
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
    const searchErrorRecovery = await runSearchErrorRecovery(browser)
    const sessionLifecycle = await runSessionLifecycle(browser)
    const interactionControls = await runInteractionControls(browser)
    const queueControls = await runQueueControls(browser)
    const mobile = await runMobile(browser)
    console.log(JSON.stringify({ browser: 'playwright', native: 'ok', desktop, reconnectDesktop, darkDesktop, loadingDesktop, searchErrorRecovery, sessionLifecycle, interactionControls, queueControls, mobile }))
  } finally {
    await browser.close()
  }
} finally {
  server.kill()
}
