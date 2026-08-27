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
    window.__shutuRealMetrics = { frames: 0, longTasks: 0, longTaskMs: 0, mutationAt: 0, mutations: 0 }
    const metrics = window.__shutuRealMetrics
    const frame = () => { metrics.frames += 1; requestAnimationFrame(frame) }
    requestAnimationFrame(frame)
    new MutationObserver(() => {
      metrics.mutationAt = Date.now()
      metrics.mutations += 1
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
    const metrics = window.__shutuRealMetrics ?? { frames: 0, longTasks: 0, longTaskMs: 0, mutationAt: 0, mutations: 0 }
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
      scrollHeight: document.querySelector('[data-trajectory-scroll]')?.scrollHeight ?? largest?.scrollHeight ?? 0,
      domNodes: document.getElementsByTagName('*').length,
      trajectoryRows: document.querySelectorAll('[data-trajectory-scroll] tr[data-trajectory-row-key]').length,
      trajectoryRowCount: document.querySelector('[data-trajectory-scroll] table')?.getAttribute('aria-rowcount') ?? null,
    }
  })
}

const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] })
const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
const consoleErrors = []
const stream = { eventFrames: 0, firstEventSeq: null, lastEventSeq: null, pending: [], latencies: [] }
page.on('websocket', websocket => {
  if (new URL(websocket.url()).pathname !== '/api/events.mux') return
  websocket.on('framereceived', payload => {
    const encoded = typeof payload === 'string' ? payload : payload?.payload
    if (typeof encoded !== 'string') return
    let envelope
    try { envelope = JSON.parse(encoded) } catch { return }
    const seq = Number(envelope?.payload?.event?.seq)
    if (!Number.isFinite(seq)) return
    const receivedAt = Date.now()
    stream.eventFrames += 1
    stream.firstEventSeq ??= seq
    stream.lastEventSeq = seq
    stream.pending.push({ receivedAt, seq })
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
    const uiMutationAt = browserSample.mutationAt
    const completed = stream.pending.filter(event => uiMutationAt >= event.receivedAt)
    if (completed.length > 0) {
      stream.latencies.push(...completed.map(event => uiMutationAt - event.receivedAt))
      stream.pending = stream.pending.filter(event => uiMutationAt < event.receivedAt)
    }
    samples.push({ elapsedSeconds: Math.round((Date.now() - startedAt) / 100) / 10, ...tail, ...browserSample })
  }
  assert.ok(samples.length > 0)
  const fps = samples.map(sample => sample.frames)
  const heaps = samples.flatMap(sample => sample.heapMiB === null ? [] : [sample.heapMiB])
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
    streamEventsPerSecond: Math.round(stream.eventFrames / Math.max(1, samples.at(-1)?.elapsedSeconds ?? 0) * 100) / 100,
    triggerStatus,
    promptAdmissionMs,
    eventToUiMs: stream.latencies.length > 0 ? {
      min: Math.min(...stream.latencies),
      avg: Math.round(stream.latencies.reduce((sum, value) => sum + value, 0) / stream.latencies.length * 10) / 10,
      max: Math.max(...stream.latencies),
      samples: stream.latencies.length,
    } : null,
    minFps: fps.length > 0 ? Math.min(...fps) : null,
    avgFps: fps.length > 0 ? Math.round(fps.reduce((sum, value) => sum + value, 0) / fps.length * 10) / 10 : null,
    maxDomNodes: Math.max(...samples.map(sample => sample.domNodes)),
    heapStartMiB: heaps[0] ?? null,
    heapMaxMiB: heaps.length > 0 ? Math.max(...heaps) : null,
    longTasks: samples.at(-1)?.longTasks ?? 0,
    longTaskMs: samples.at(-1)?.longTaskMs ?? 0,
    consoleErrors,
    samplesDetail: samples,
  }))
} finally {
  await page.close()
  await browser.close()
}
