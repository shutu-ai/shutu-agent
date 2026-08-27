import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const vite = resolve(dshRoot, 'apps/web/node_modules/vite/bin/vite.js')
const distIndex = resolve(webRoot, 'dist/index.html')
const { chromium } = createRequire(import.meta.url)(resolve(dshRoot, 'apps/web/node_modules/playwright'))
const host = '127.0.0.1'
const port = Number(process.env.SHUTU_PERF_PORT ?? 18118)
const baseUrl = `http://${host}:${port}/`
const eventCount = Number(process.env.SHUTU_PERF_EVENTS ?? 10_000)

if (!existsSync(vite)) throw new Error(`Vite is unavailable at ${vite}; set SHUTU_DSH_ROOT to a DSH checkout.`)
if (!existsSync(distIndex)) throw new Error(`Native dist is unavailable at ${distIndex}; run npm.cmd run build first.`)
if (!Number.isInteger(eventCount) || eventCount < 100) throw new Error('SHUTU_PERF_EVENTS must be an integer >= 100')

function makeEvents() {
  const events = []
  const longText = '持续流式输出 '.repeat(24)
  const streamText = 'stream delta '.repeat(8)
  const toolText = 'tool result line '.repeat(8)
  let seq = 0
  for (let turn = 1; events.length < eventCount; turn += 1) {
    const time = 1787746887000 + turn * 1000
    const readCallId = `read-${turn}`
    const searchCallId = `search-${turn}`
    const readArguments = JSON.stringify({ path: `src/file-${turn}.tsx`, query: `level ${turn}` })
    const searchArguments = JSON.stringify({ query: `Super Mario level ${turn}`, limit: 20 })
    const code = '\n\n```tsx\nconst level_' + turn + ' = createLevel({ width: 320, height: 180 })\nrender(level_' + turn + ')\n```'
    events.push(
      { seq: ++seq, type: 'turn/start', time, data: { turn } },
      { seq: ++seq, type: 'user/message', time, data: { id: `user-${turn}`, role: 'user', content: [{ type: 'text', text: `task ${turn}: build the next platform section` }], source: { kind: 'user' } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'step/start', time, data: { turn, step: 1 } },
      { seq: ++seq, type: 'assistant/chunk', time, data: { turn, step: 1, chunk: { type: 'reasoning-delta', index: 0, text: `reasoning ${turn} ` } } },
      { seq: ++seq, type: 'assistant/chunk', time, data: { turn, step: 1, chunk: { type: 'text-delta', index: 0, text: `stream ${turn} ${streamText}` } } },
      { seq: ++seq, type: 'assistant/message', time, data: { turn, step: 1, message: { id: `assistant-tools-${turn}`, role: 'assistant', content: [{ type: 'reasoning', text: `reasoning ${turn}` }, { type: 'text', text: `planning tool work for level ${turn}` }, { type: 'tool-call', id: readCallId, name: 'read', arguments: readArguments }, { type: 'tool-call', id: searchCallId, name: 'search', arguments: searchArguments }], source: { kind: 'model', provider: 'test', model: 'perf' } }, usage: { inputTokens: 1200, outputTokens: 260, reasoningTokens: 40 } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'tool/call', time, data: { turn, step: 1, callId: readCallId, name: 'read', arguments: readArguments } },
      { seq: ++seq, type: 'tool/result', time, data: { turn, step: 1, message: { id: `tool-read-${turn}`, role: 'user', content: [{ type: 'tool-result', toolCallId: readCallId, content: [{ type: 'text', text: toolText }], isError: false }], source: { kind: 'tool', callId: readCallId } } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'tool/call', time, data: { turn, step: 1, callId: searchCallId, name: 'search', arguments: searchArguments } },
      { seq: ++seq, type: 'tool/result', time, data: { turn, step: 1, message: { id: `tool-search-${turn}`, role: 'user', content: [{ type: 'tool-result', toolCallId: searchCallId, content: [{ type: 'text', text: `result ${turn} ${toolText}` }], isError: false }], source: { kind: 'tool', callId: searchCallId } } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'assistant/message', time, data: { turn, step: 1, message: { id: `assistant-${turn}`, role: 'assistant', content: [{ type: 'text', text: `done ${turn} ${longText}${code}` }], source: { kind: 'model', provider: 'test', model: 'perf' } }, usage: { inputTokens: 1200, outputTokens: 260, reasoningTokens: 40 } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'step/end', time, data: { turn, step: 1 } },
      { seq: ++seq, type: 'turn/end', time, data: { turn, reason: { kind: 'completed' } } },
    )
  }
  return events.slice(0, eventCount)
}

const events = makeEvents()
const projections = { asOfSeq: events.length, values: {
  contextBreakdown: { messageTokens: events.length * 3, systemTokens: 0, toolsTokens: events.length },
  contextPressure: { pressureTokens: events.length * 4 }, goal: null, imageLimits: undefined,
  permissions: { currentValue: 'standard', options: [{ name: 'Standard', value: 'standard' }] },
  plan: { active: false, pending: false }, sessionListMetadata: { blank: false, lastPromptAt: 1787746887000 },
  sessionStats: { decodeMs: 1000, decodeTokens: events.length, llmMs: 1200, steps: Math.floor(events.length / 9), toolMs: 100, ttftMs: 30, ttftSteps: Math.floor(events.length / 9), turns: Math.floor(events.length / 9) },
  subagent: null, subagentTiming: { settledMs: 0 }, title: 'Native performance fixture', todos: null,
  tokenUsage: { cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: events.length, uncachedInputTokens: 0 },
} }

function valueFor(method, request = null) {
  switch (method) {
    case 'host.describe': return { attachedSessions: 1, canOpenPath: false, cwd: 'C:/shutu-perf', home: '', model: 'perf', version: 'perf' }
    case 'session.list': return { items: [{ sessionId: 'perf', title: 'Native performance fixture', updatedAt: Date.now(), running: false, blank: false, cwd: 'C:/shutu-perf', projections }] }
    case 'workspace.list': return { items: [{ workspaceId: 'perf-ws', path: 'C:/shutu-perf', title: 'Performance', sessionIds: ['perf'], createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }], archivedSessionIds: [] }
    case 'session.history': return historyPage()
    case 'session.search': return { items: [{ sessionId: 'perf', snippet: 'tool result 777' }], hasMore: false }
    case 'session.prompt': return { accepted: true }
    case 'messageFeedback/list': return { ok: true, value: { items: [] } }
    case 'messageFeedback/put': {
      const args = Array.isArray(request?.payload?.args) ? request.payload.args : []
      const input = args[0] && typeof args[0] === 'object' ? args[0] : {}
      return { ok: true, value: {
        messageId: String(input.messageId ?? 'perf-message'), rating: String(input.rating ?? 'positive'),
        note: typeof input.note === 'string' ? input.note : undefined, version: 'perf-feedback:1',
        createdAt: Date.now(), updatedAt: Date.now(),
      } }
    }
    case 'messageFeedback/delete': return { ok: true, value: { absent: true } }
    case 'settings.describe': return { hasDocument: false, namespaces: [] }
    case 'credentials.describe': return { credentials: {} }
    case 'agentPreset.list': return { authorable: false, hasDocument: false, presets: [] }
    case 'llm.providers': return { providers: [] }
    case 'commands/list': return []
    case 'dynamicCordisRunner/inventory': return []
    case 'dynamicCordisRunner/syncInspectManifest': return { ok: true }
    default: return {}
  }
}

/** Serve the same tail-paged history shape as the native host contract. */
function historyPage(payload = {}) {
  const requestedBefore = Number.isFinite(Number(payload.beforeSeq)) ? Number(payload.beforeSeq) : null
  const boundary = requestedBefore === null
    ? -1
    : events.findIndex(event => event.seq >= requestedBefore)
  const end = requestedBefore === null || boundary === -1 ? events.length : boundary
  const pageSize = Math.max(24, Math.min(180, Number(payload.maxMessages) || 50))
  const start = Math.max(0, end - pageSize)
  const page = events.slice(start, end)
  return {
    header: { version: 0, id: 'perf', createdAt: 1787746887000, cwd: 'C:/shutu-perf' },
    events: page.map(event => ({ event })),
    hasMore: start > 0,
    surface: { nodes: events.filter(event => event.surfaceOp === 'append').map(event => event.seq), replacements: [] },
    ...(requestedBefore === null ? { projections } : {}),
  }
}

async function waitForServer() {
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    try { if ((await fetch(baseUrl)).ok) return } catch { /* Vite is still starting. */ }
    await new Promise(resolvePromise => setTimeout(resolvePromise, 100))
  }
  throw new Error(`timed out waiting for ${baseUrl}`)
}

async function installNativeMock(page) {
  const sockets = new Set()
  const requests = []
  await page.route('**/plugins/events', route => route.fulfill({ status: 200, contentType: 'text/event-stream', body: 'retry: 3000\n\n' }))
  await page.routeWebSocket('**/api/events.*', ws => { sockets.add(new URL(ws.url()).pathname); ws.onMessage(() => {}) })
  await page.route('**/api/**', async route => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = JSON.parse(route.request().postData() ?? '{}')
    assert.equal(body.type, 'client-request')
    requests.push({ method: body.method, payload: body.payload })
    const value = body.method === 'session.history' ? historyPage(body.payload) : valueFor(body.method, body)
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ type: 'server-response', rpcId: body.rpcId, result: { ok: true, value } }) })
  })
  return { sockets, requests }
}

async function sample(page) {
  return page.evaluate(() => {
    const state = window.__perf ?? { frames: 0, longTasks: 0, longTaskMs: 0 }
    const frames = state.frames
    state.frames = 0
    const heap = performance.memory?.usedJSHeapSize
    const scrollables = [...document.querySelectorAll('*')].filter(e => e.scrollHeight > e.clientHeight + 4 && e.clientHeight > 100)
    const scroll = scrollables.sort((a, b) => b.scrollHeight - a.scrollHeight)[0]
    return {
      frames,
      heapMiB: typeof heap === 'number' ? Math.round(heap / 1024 / 1024) : null,
      longTasks: state.longTasks,
      longTaskMs: Math.round(state.longTaskMs),
      scrollHeight: scroll?.scrollHeight ?? 0,
      domNodes: document.getElementsByTagName('*').length,
      trajectoryRows: document.querySelectorAll('[data-trajectory-scroll] tr[data-trajectory-row-key]').length,
      trajectoryRowCount: document.querySelector('[data-trajectory-scroll] table')?.getAttribute('aria-rowcount') ?? null,
      trajectoryText: document.querySelector('[data-trajectory-scroll]')?.textContent?.slice(0, 240) ?? '',
    }
  })
}

const server = spawn(process.execPath, [vite, 'preview', '--host', host, '--port', String(port)], { cwd: webRoot, stdio: 'ignore', windowsHide: true })
try {
  await waitForServer()
  const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] })
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
    const { sockets, requests } = await installNativeMock(page)
    const errors = []
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()) })
    page.on('pageerror', error => errors.push(error.message))
    const started = performance.now()
    await page.goto(baseUrl, { waitUntil: 'domcontentloaded', timeout: 60_000 })
    try {
      await page.getByRole('button', { name: /Search sessions|搜索会话/ }).waitFor({ timeout: 15_000 })
    } catch (error) {
      console.error(JSON.stringify({ title: await page.title(), body: (await page.locator('body').innerText()).slice(0, 2000), buttons: await page.getByRole('button').allTextContents(), errors }))
      throw error
    }
    await page.getByRole('button', { name: /Search sessions|搜索会话/ }).click()
    await page.getByPlaceholder(/Search sessions\.\.\.|搜索会话…/).fill('tool result 777')
    const results = page.getByRole('tree', { name: /Search results|搜索结果/ }).getByRole('treeitem')
    await results.first().waitFor({ timeout: 60_000 })
    await results.first().dispatchEvent('click')
    // Exercise the loaded conversation's native message actions before
    // switching to Trajectory. The feedback controller intentionally loads
    // lazily on focus, so this proves the browser reaches the DSH Remote
    // namespace rather than only rendering the buttons.
    const like = page.getByRole('button', { name: /Good response|有帮助|好的回答/ }).first()
    await like.focus()
    await like.click()
    const rated = page.getByRole('button', { name: /Remove rating|取消评价|取消标记/ }).first()
    await rated.waitFor({ timeout: 15_000 })
    await page.getByRole('button', { name: /Add a note|添加备注|添加说明|补充说明/ }).first().click()
    const feedbackNote = page.getByRole('textbox', { name: /Feedback note|反馈说明/ })
    await feedbackNote.fill('native fixture note')
    await page.getByRole('button', { name: /^(?:Save|保存)$/ }).click()
    await page.getByText('native fixture note', { exact: true }).waitFor({ timeout: 15_000 })
    await rated.click()
    await page.getByRole('button', { name: /Good response|有帮助|好的回答/ }).first().waitFor({ timeout: 15_000 })
    assert.ok(requests.some(request => request.method === 'messageFeedback/list'), 'native feedback did not load its list')
    assert.ok(requests.some(request => request.method === 'messageFeedback/put'), 'native feedback did not persist its rating/note')
    assert.ok(requests.some(request => request.method === 'messageFeedback/delete'), 'native feedback did not retract its rating')

    const composer = page.locator('textarea').first()
    await composer.fill('native fixture prompt')
    await composer.press('Enter')
    await page.waitForFunction(() => document.querySelector('textarea')?.value === '')
    assert.ok(requests.some(request => request.method === 'session.prompt'), 'native composer did not send session.prompt')
    const tab = page.getByRole('tab', { name: /轨迹|Trajectory/ })
    await tab.waitFor({ timeout: 60_000 })
    await tab.click()
    const trajectory = page.locator('[data-trajectory-scroll]')
    await trajectory.waitFor({ timeout: 60_000 })
    await page.locator('[data-trajectory-scroll] table[data-scroll-ready="true"]').waitFor({ timeout: 60_000 })
    const initialRowCount = Number(await page.locator('[data-trajectory-scroll] table').getAttribute('aria-rowcount'))
    const requestBoundary = page.locator('[data-trajectory-scroll] button[data-request-run-index]').first()
    await requestBoundary.waitFor({ timeout: 15_000 })
    await requestBoundary.click()
    const details = page.getByRole('complementary', { name: /Event details/ })
    await details.waitFor({ timeout: 15_000 })
    assert.match(await details.innerText(), /Request #|Summary/)
    const turnsControl = page.getByRole('button', { name: /turns/i }).first()
    await turnsControl.click()
    await page.locator('[data-trajectory-scroll] tr[data-collapsed-summary="turn"]').first().waitFor({ timeout: 15_000 })
    assert.ok(await page.locator('[data-trajectory-scroll] tr[data-collapsed-summary="turn"]').count() > 0, 'native trajectory turns did not collapse')
    await turnsControl.click()
    await page.locator('[data-trajectory-scroll] tr[data-collapsed-summary="turn"]').first().waitFor({ state: 'detached', timeout: 15_000 })
    const loadEarlier = page.locator('[data-history-load] button')
    await loadEarlier.waitFor({ timeout: 15_000 })
    await loadEarlier.evaluate(button => button.click())
    await page.waitForFunction((minimum) => {
      const table = document.querySelector('[data-trajectory-scroll] table')
      return table !== null && Number(table.getAttribute('aria-rowcount') ?? 0) > minimum
    }, initialRowCount, { timeout: 15_000 })
    assert.ok(requests.some(request => request.method === 'session.history' && request.payload?.beforeSeq !== undefined), 'native history pagination did not send beforeSeq')
    await page.evaluate(() => {
      window.__perf = { frames: 0, longTasks: 0, longTaskMs: 0 }
      const state = window.__perf
      const frame = () => { state.frames += 1; requestAnimationFrame(frame) }
      requestAnimationFrame(frame)
      try {
        const observer = new PerformanceObserver(list => { for (const entry of list.getEntries()) { state.longTasks += 1; state.longTaskMs += entry.duration } })
        observer.observe({ type: 'longtask', buffered: true })
      } catch { /* optional metric */ }
    })
    const loadMs = performance.now() - started
    const first = await sample(page)
    assert.ok(first.trajectoryRows <= 160, `native trajectory mounted too many rows: ${first.trajectoryRows}`)
    assert.ok(first.trajectoryRowCount !== null, 'native trajectory did not expose a logical row count')
    assert.ok(first.scrollHeight > 0, 'native trajectory did not expose a scroll range')
    await page.waitForTimeout(1_000)
    const second = await sample(page)
    assert.deepEqual(errors, [], `native performance fixture emitted browser errors: ${errors.join('; ')}`)
    console.log(JSON.stringify({ browser: 'playwright-chromium', transport: 'native-rpc+downlink-websocket', eventCount: events.length, loadMs: Math.round(loadMs), first, second, sockets: [...sockets].sort(), consoleErrors: errors }))
    await page.close()
  } finally { await browser.close() }
} finally { server.kill() }
