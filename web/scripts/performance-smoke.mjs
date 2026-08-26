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
  const text = '持续流式输出 '.repeat(24)
  let seq = 0
  for (let turn = 1; events.length < eventCount; turn += 1) {
    const time = 1787746887000 + turn * 1000
    events.push(
      { seq: ++seq, type: 'turn/start', time, data: { turn } },
      { seq: ++seq, type: 'user/message', time, data: { id: `user-${turn}`, role: 'user', content: [{ type: 'text', text: `task ${turn} ${text}` }], source: { kind: 'user' } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'step/start', time, data: { turn, step: 1 } },
      { seq: ++seq, type: 'assistant/chunk', time, data: { turn, step: 1, chunk: { type: 'text-delta', index: 0, text: `stream ${turn} ${text}` } } },
      { seq: ++seq, type: 'tool/call', time, data: { turn, step: 1, callId: `call-${turn}`, name: 'read', arguments: JSON.stringify({ path: `src/file-${turn}.tsx`, query: text }) } },
      { seq: ++seq, type: 'tool/result', time, data: { turn, step: 1, message: { id: `tool-${turn}`, role: 'user', content: [{ type: 'tool-result', toolCallId: `call-${turn}`, content: [{ type: 'text', text }], isError: false }], source: { kind: 'tool', callId: `call-${turn}` } } }, surfaceOp: 'append' },
      { seq: ++seq, type: 'assistant/message', time, data: { turn, step: 1, message: { id: `assistant-${turn}`, role: 'assistant', content: [{ type: 'text', text: `done ${turn} ${text}` }], source: { kind: 'model', provider: 'test', model: 'perf' } }, usage: { inputTokens: 1200, outputTokens: 180 } }, surfaceOp: 'append' },
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

function valueFor(method) {
  switch (method) {
    case 'host.describe': return { attachedSessions: 1, canOpenPath: false, cwd: 'C:/shutu-perf', home: '', model: 'perf', version: 'perf' }
    case 'session.list': return { items: [{ sessionId: 'perf', title: 'Native performance fixture', updatedAt: Date.now(), running: false, blank: false, cwd: 'C:/shutu-perf', projections }] }
    case 'workspace.list': return { items: [{ workspaceId: 'perf-ws', path: 'C:/shutu-perf', title: 'Performance', sessionIds: ['perf'], createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }], archivedSessionIds: [] }
    case 'session.history': return { header: { version: 0, id: 'perf', createdAt: 1787746887000, cwd: 'C:/shutu-perf' }, events: events.map(event => ({ event })), hasMore: false, surface: { nodes: events.filter(event => event.surfaceOp === 'append').map(event => event.seq), replacements: [] }, projections }
    case 'session.search': return { items: [{ sessionId: 'perf', snippet: 'tool result 777' }], hasMore: false }
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
  await page.route('**/plugins/events', route => route.fulfill({ status: 200, contentType: 'text/event-stream', body: 'retry: 3000\n\n' }))
  await page.routeWebSocket('**/api/events.*', ws => { sockets.add(new URL(ws.url()).pathname); ws.onMessage(() => {}) })
  await page.route('**/api/**', async route => {
    if (route.request().method() !== 'POST') return route.fallback()
    const body = JSON.parse(route.request().postData() ?? '{}')
    assert.equal(body.type, 'client-request')
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ type: 'server-response', rpcId: body.rpcId, result: { ok: true, value: valueFor(body.method) } }) })
  })
  return sockets
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
    const sockets = await installNativeMock(page)
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
    const tab = page.getByRole('tab', { name: /轨迹|Trajectory/ })
    await tab.waitFor({ timeout: 60_000 })
    await tab.click()
    const trajectory = page.locator('[data-trajectory-scroll]')
    await trajectory.waitFor({ timeout: 60_000 })
    await page.locator('[data-trajectory-scroll] table[data-scroll-ready="true"]').waitFor({ timeout: 60_000 })
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
