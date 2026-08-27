import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const playwrightRoot = resolve(dshRoot, 'apps/web/node_modules/playwright')
const { chromium } = createRequire(import.meta.url)(playwrightRoot)
const baseUrl = (process.env.SHUTU_REAL_TASK_URL ?? 'http://127.0.0.1:18099').replace(/\/$/, '')
const sessionId = process.argv[2]
const durationSeconds = Number(process.argv[3] ?? process.env.SHUTU_REAL_TASK_SECONDS ?? 900)
const skipSelection = process.env.SHUTU_REAL_TASK_SKIP_SELECTION === '1'
const triggerPrompt = process.env.SHUTU_REAL_TASK_PROMPT?.trim() ?? ''
const triggerMode = process.env.SHUTU_REAL_TASK_MODE === 'queue' ? 'queue' : 'steer'
const enforceThresholds = process.env.SHUTU_REAL_TASK_ENFORCE_THRESHOLDS === '1'
const performanceThresholds = {
  minFps: Number(process.env.SHUTU_REAL_TASK_MIN_FPS ?? 30),
  maxHeapGrowthMiB: Number(process.env.SHUTU_REAL_TASK_MAX_HEAP_GROWTH_MIB ?? 128),
  maxDomNodes: Number(process.env.SHUTU_REAL_TASK_MAX_DOM_NODES ?? 2_500),
  maxLongTaskMs: Number(process.env.SHUTU_REAL_TASK_MAX_LONG_TASK_MS ?? 2_000),
  maxEventToUiMs: Number(process.env.SHUTU_REAL_TASK_MAX_EVENT_TO_UI_MS ?? 500),
  maxReconnectMs: Number(process.env.SHUTU_REAL_TASK_MAX_RECONNECT_MS ?? 1_500),
}

if (!existsSync(playwrightRoot)) throw new Error(`Playwright is unavailable under ${dshRoot}`)
if (!sessionId) throw new Error('usage: node scripts/real-task-performance.mjs <session-id> [duration-seconds]')
if (!Number.isFinite(durationSeconds) || durationSeconds <= 0) throw new Error('duration must be positive')

let rpcSequence = 0
async function nativeRPC(method, payload = {}) {
  const rpcId = `real-perf-${++rpcSequence}`
  const response = await fetch(`${baseUrl}/api/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ type: 'client-request', rpcId, method, payload }),
  })
  if (!response.ok) throw new Error(`${method} returned HTTP ${response.status}`)
  const envelope = await response.json()
  assert.equal(envelope.type, 'server-response')
  assert.equal(envelope.rpcId, rpcId)
  if (!envelope.result?.ok) throw new Error(`${method} failed: ${envelope.result?.error?.message ?? 'unknown error'}`)
  return envelope.result.value
}

function triggerRealTask() {
  const startedAt = Date.now()
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), Math.max(30_000, (durationSeconds + 10) * 1_000))
  return fetch(`${baseUrl}/api/session.prompt`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    signal: controller.signal,
    body: JSON.stringify({
      type: 'client-request',
      rpcId: `real-perf-trigger-${Date.now()}`,
      method: 'session.prompt',
      payload: {
        sessionId,
        mode: triggerMode,
        content: [{ type: 'text', text: triggerPrompt }],
        clientTimeZone: 'UTC',
      },
    }),
  }).then(async response => {
    if (!response.ok) return { status: `http-${response.status}`, elapsedMs: Date.now() - startedAt }
    const envelope = await response.json()
    return {
      status: envelope.result?.ok ? 'accepted' : `rejected:${envelope.result?.error?.code ?? 'unknown'}`,
      elapsedMs: Date.now() - startedAt,
    }
  }).catch(error => ({ status: `pending:${error.name}`, elapsedMs: Date.now() - startedAt })).finally(() => clearTimeout(timeout))
}

async function sessionSummary() {
  const value = await nativeRPC('session.list')
  const summary = value.items?.find(entry => entry.sessionId === sessionId)
  if (!summary) throw new Error(`session ${sessionId} is absent from native session.list`)
  return summary
}

async function sessionSearchTerm(summary) {
  if (summary.title?.trim()) return summary.title.trim()
  // Avoid loading a multi-million-character history just to find a search
  // label. Real long-task sessions use this stable task keyword; callers may
  // override it when benchmarking another task.
  return process.env.SHUTU_REAL_TASK_SEARCH ?? '超级玛丽'
}

async function selectSession(page, summary) {
  const term = await sessionSearchTerm(summary)
  if (!term) throw new Error(`session ${sessionId} has no searchable title or user message`)
  await page.getByRole('button', { name: /Search sessions|搜索会话/ }).click({ force: true })
  const search = page.getByPlaceholder(/Search sessions\.\.\.|搜索会话…/)
  await search.fill(term)
  // Search result snippets can be localized or contain replacement glyphs;
  // the native command palette's treeitem is the stable DSH selection seam.
  const hit = page.locator('[role="tree"]').last().getByRole('treeitem').first()
  await hit.waitFor({ state: 'visible', timeout: 15_000 })
  // The DSH command palette keeps a pointer-blocking backdrop over the
  // result list while keyboard focus remains in the search field. Dispatching
  // the semantic treeitem click mirrors the keyboard/selection path without
  // depending on backdrop geometry in headless Chromium.
  await hit.dispatchEvent('click')
  await search.press('Escape')
}

async function installBrowserMetrics(page) {
  await page.evaluate(() => {
    window.__shutuRealMetrics = {
      frames: 0,
      longTasks: 0,
      longTaskMs: 0,
      mutationAt: 0,
      mutations: 0,
      mutationTimes: [],
      nativeEventAt: 0,
      nativeEventPending: false,
      nativeEventToUiMs: [],
    }
    const metrics = window.__shutuRealMetrics
    const frame = () => { metrics.frames += 1; requestAnimationFrame(frame) }
    requestAnimationFrame(frame)
    new MutationObserver(() => {
      const now = performance.now()
      metrics.mutationAt = Date.now()
      metrics.mutations += 1
      metrics.mutationTimes.push(metrics.mutationAt)
      if (metrics.mutationTimes.length > 500) metrics.mutationTimes.shift()
      // Several mux events can arrive before React commits one frame. Measure
      // the latest event in that batch to the commit, and do not attribute a
      // later unrelated mutation to every event that arrived in between.
      if (metrics.nativeEventPending && metrics.nativeEventAt > 0) {
        metrics.nativeEventToUiMs.push(Math.max(0, Math.round(now - metrics.nativeEventAt)))
        metrics.nativeEventPending = false
      }
    }).observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true })
    if ('PerformanceObserver' in window) {
      try {
        const observer = new PerformanceObserver(list => {
          for (const entry of list.getEntries()) {
            metrics.longTasks += 1
            metrics.longTaskMs += entry.duration
          }
        })
        observer.observe({ type: 'longtask', buffered: true })
      } catch {
        // Long-task entries are optional in headless Chromium.
      }
    }
  })
}

async function sampleBrowser(page) {
  return page.evaluate(() => {
    const metrics = window.__shutuRealMetrics ?? {
      frames: 0, longTasks: 0, longTaskMs: 0, mutationAt: 0, mutations: 0, mutationTimes: [],
      nativeEventToUiMs: [],
    }
    const frames = metrics.frames
    metrics.frames = 0
    const heap = performance.memory?.usedJSHeapSize
    const scrollables = [...document.querySelectorAll('*')]
      .filter(element => element.scrollHeight > element.clientHeight + 4 && element.clientHeight > 100)
    const largest = scrollables.sort((a, b) => b.scrollHeight - a.scrollHeight)[0]
    return {
      frames,
      heapMiB: typeof heap === 'number' ? Math.round(heap / 1024 / 1024) : null,
      longTasks: metrics.longTasks,
      longTaskMs: Math.round(metrics.longTaskMs),
      mutationAt: metrics.mutationAt,
      mutations: metrics.mutations,
      mutationTimes: metrics.mutationTimes.splice(0),
      eventToUiMs: metrics.nativeEventToUiMs.splice(0),
      scrollHeight: document.querySelector('[data-trajectory-scroll]')?.scrollHeight ?? largest?.scrollHeight ?? 0,
      domNodes: document.getElementsByTagName('*').length,
      trajectoryRows: document.querySelectorAll('[data-trajectory-scroll] tr[data-trajectory-row-key]').length,
      trajectoryRowCount: document.querySelector('[data-trajectory-scroll] table')?.getAttribute('aria-rowcount') ?? null,
    }
  })
}

const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] })
const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
await page.addInitScript(() => {
  const NativeWebSocket = window.WebSocket
  function ObservedWebSocket(...args) {
    const socket = new NativeWebSocket(...args)
    socket.addEventListener('message', message => {
      if (typeof message.data !== 'string') return
      try {
        const envelope = JSON.parse(message.data)
        if (envelope?.payload?.type !== 'session/event') return
        const metrics = window.__shutuRealMetrics ?? (window.__shutuRealMetrics = {
          nativeEventAt: 0, nativeEventPending: false, nativeEventToUiMs: [],
        })
        metrics.nativeEventAt = performance.now()
        metrics.nativeEventPending = true
      } catch {
        // Non-JSON WebSocket frames are outside this sampler's timing scope.
      }
    })
    return socket
  }
  ObservedWebSocket.prototype = NativeWebSocket.prototype
  Object.setPrototypeOf(ObservedWebSocket, NativeWebSocket)
  window.WebSocket = ObservedWebSocket
})
const consoleErrors = []
const stream = { eventFrames: 0, firstEventSeq: null, lastEventSeq: null, latencies: [], invalidTurnFrames: [] }
const reconnectAfterMs = Number(process.env.SHUTU_REAL_TASK_RECONNECT_AFTER_MS ?? 0)
const reconnect = { requested: Number.isFinite(reconnectAfterMs) && reconnectAfterMs > 0, connections: 0, closedAt: null, reconnectedAt: null }
if (reconnect.requested) {
  await page.routeWebSocket('**/api/events.*', async websocket => {
    const pathname = new URL(websocket.url()).pathname
    const upstream = await websocket.connectToServer()
    if (pathname !== '/api/events.mux') {
      websocket.onMessage(message => upstream.send(message))
      upstream.onMessage(message => websocket.send(message))
      return
    }
    reconnect.connections += 1
    if (reconnect.connections === 2 && reconnect.closedAt !== null) reconnect.reconnectedAt = Date.now()
    websocket.onMessage(message => upstream.send(message))
    upstream.onMessage(message => websocket.send(message))
    if (reconnect.connections === 1) {
      setTimeout(() => {
        reconnect.closedAt = Date.now()
        websocket.close(1011, 'real performance reconnect probe')
      }, reconnectAfterMs)
    }
  })
}
page.on('websocket', websocket => {
  if (new URL(websocket.url()).pathname !== '/api/events.mux') return
  websocket.on('framereceived', payload => {
    const encoded = typeof payload === 'string' ? payload : payload?.payload
    if (typeof encoded !== 'string') return
    let envelope
    try { envelope = JSON.parse(encoded) } catch { return }
    if (envelope?.payload?.sessionId !== sessionId) return
    const seq = Number(envelope?.payload?.event?.seq)
    if (!Number.isFinite(seq)) return
    const event = envelope?.payload?.event
    const eventTurn = Number(event?.data?.turn)
    if (event && (event.type === 'assistant/chunk' || event.type === 'assistant/message') && Number.isFinite(eventTurn) && eventTurn < 0) {
      stream.invalidTurnFrames.push({ seq, type: event.type, turn: eventTurn, step: Number(event.data?.step) })
    }
    stream.eventFrames += 1
    stream.firstEventSeq ??= seq
    stream.lastEventSeq = seq
  })
})
page.on('console', message => { if (message.type() === 'error') consoleErrors.push(message.text()) })
page.on('pageerror', error => consoleErrors.push(error.message))
try {
  await page.goto(`${baseUrl}/`, { waitUntil: 'domcontentloaded', timeout: 60_000 })
  const summary = await sessionSummary()
  const baselineSeq = summary.projections?.asOfSeq ?? 0
  const tabs = page.getByRole('tab', { name: /Trajectory|轨迹/ })
  if (!skipSelection) await selectSession(page, summary)
  await tabs.waitFor({ timeout: 60_000 })
  await page.getByRole('tab', { name: /Trajectory|轨迹/ }).click()
  await page.locator('[data-trajectory-scroll]').waitFor({ timeout: 60_000 })
  await page.locator('[data-trajectory-scroll] table[data-scroll-ready="true"]').waitFor({ timeout: 60_000 })
  await installBrowserMetrics(page)
  process.stderr.write(`observer-ready native session=${sessionId}\n`)
  let triggerStatus = null
  let promptAdmissionMs = null
  if (triggerPrompt) {
    triggerStatus = 'dispatched'
    void triggerRealTask().then(result => {
      triggerStatus = result.status
      promptAdmissionMs = result.elapsedMs
    })
    process.stderr.write(`dispatched native session prompt mode=${triggerMode}\n`)
  }

  const samples = []
  const startedAt = Date.now()
  while (Date.now() - startedAt < durationSeconds * 1000) {
    await new Promise(resolvePromise => setTimeout(resolvePromise, 1_000))
    // Do not poll session.list while a real model turn is running. The host
    // may serialize native RPC handlers behind the prompt; the mux event
    // sequence is already the authoritative live tail for this sampler.
    const [browserSample] = await Promise.all([sampleBrowser(page)])
    const tail = { count: stream.lastEventSeq ?? baselineSeq, type: null, time: null }
    stream.latencies.push(...(browserSample.eventToUiMs ?? []))
    samples.push({ elapsedSeconds: Math.round((Date.now() - startedAt) / 100) / 10, ...tail, ...browserSample })
  }
  assert.ok(samples.length > 0)
  const fps = samples.map(sample => sample.frames)
  const heaps = samples.flatMap(sample => sample.heapMiB === null ? [] : [sample.heapMiB])
  const eventToUi = stream.latencies.length > 0 ? {
    min: Math.min(...stream.latencies),
    avg: Math.round(stream.latencies.reduce((sum, value) => sum + value, 0) / stream.latencies.length * 10) / 10,
    max: Math.max(...stream.latencies),
    samples: stream.latencies.length,
  } : null
  const minFps = fps.length > 0 ? Math.min(...fps) : null
  const avgFps = fps.length > 0 ? Math.round(fps.reduce((sum, value) => sum + value, 0) / fps.length * 10) / 10 : null
  const heapStartMiB = heaps[0] ?? null
  const heapMaxMiB = heaps.length > 0 ? Math.max(...heaps) : null
  const maxDomNodes = Math.max(...samples.map(sample => sample.domNodes))
  const longTaskMs = samples.at(-1)?.longTaskMs ?? 0
  const reconnectRecoveryMs = reconnect.closedAt !== null && reconnect.reconnectedAt !== null
    ? reconnect.reconnectedAt - reconnect.closedAt
    : null
  const checks = {
    minFps: minFps === null || minFps >= performanceThresholds.minFps,
    heapGrowth: heapStartMiB === null || heapMaxMiB === null || heapMaxMiB - heapStartMiB <= performanceThresholds.maxHeapGrowthMiB,
    domNodes: maxDomNodes <= performanceThresholds.maxDomNodes,
    longTasks: longTaskMs <= performanceThresholds.maxLongTaskMs,
    eventToUi: eventToUi === null || eventToUi.max <= performanceThresholds.maxEventToUiMs,
    reconnect: !reconnect.requested || reconnectRecoveryMs === null || reconnectRecoveryMs <= performanceThresholds.maxReconnectMs,
  }
  const performanceGate = {
    enforced: enforceThresholds,
    passed: Object.values(checks).every(Boolean),
    thresholds: performanceThresholds,
    checks,
  }
  console.log(JSON.stringify({
    url: baseUrl,
    transport: 'native-rpc+downlink-websocket',
    sessionId,
    durationSeconds: samples.at(-1)?.elapsedSeconds ?? 0,
    samples: samples.length,
    firstEventSeq: samples[0]?.count ?? 0,
    lastEventSeq: samples.at(-1)?.count ?? 0,
    streamEventFrames: stream.eventFrames,
    streamFirstEventSeq: stream.firstEventSeq,
    streamLastEventSeq: stream.lastEventSeq,
    streamInvalidTurnFrames: stream.invalidTurnFrames,
    streamEventsPerSecond: Math.round(stream.eventFrames / Math.max(1, samples.at(-1)?.elapsedSeconds ?? 0) * 100) / 100,
    triggerStatus,
    promptAdmissionMs,
    eventToUiMs: eventToUi,
    minFps,
    avgFps,
    maxDomNodes,
    heapStartMiB,
    heapMaxMiB,
    longTasks: samples.at(-1)?.longTasks ?? 0,
    longTaskMs,
    performanceGate,
    reconnect: {
      ...reconnect,
      recoveryMs: reconnectRecoveryMs,
    },
    consoleErrors,
    samplesDetail: samples,
  }))
  if (enforceThresholds && !performanceGate.passed) process.exitCode = 2
} finally {
  await page.close()
  await browser.close()
}
