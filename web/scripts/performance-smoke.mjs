import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const vite = resolve(dshRoot, 'apps/web/node_modules/vite/bin/vite.js')
const { chromium } = createRequire(import.meta.url)(resolve(dshRoot, 'apps/web/node_modules/playwright'))
const host = '127.0.0.1'
const port = Number(process.env.SHUTU_PERF_PORT ?? 18118)
const baseUrl = `http://${host}:${port}/`
const eventCount = 10_000

if (!existsSync(vite)) {
  throw new Error(`Vite is unavailable at ${vite}; set SHUTU_DSH_ROOT to a DSH checkout.`)
}

function makeEvents() {
  const events = []
  const longText = 'long streaming output '.repeat(64)
  for (let turn = 0; events.length < eventCount; turn += 1) {
    const base = turn * 10
    const time = `2026-08-26T10:${String(Math.floor(turn / 60) % 60).padStart(2, '0')}:${String(turn % 60).padStart(2, '0')}Z`
    events.push(
      { seq: base + 1, type: 'turn/start', version: 1, time, summary: `turn ${turn} start`, details: { turn } },
      { seq: base + 2, type: 'user/message', version: 1, time, summary: `user ${turn} ${longText}` },
      { seq: base + 3, type: 'step/start', version: 1, time, summary: `step ${turn}` },
      { seq: base + 4, type: 'assistant/chunk', version: 1, time, summary: `chunk ${turn} ${longText}` },
      { seq: base + 5, type: 'tool/call', version: 1, time, summary: `tool call ${turn}`, tool_name: 'read', details: { input: { path: `src/file-${turn}.tsx`, query: longText } } },
      { seq: base + 6, type: 'tool/result', version: 1, time, summary: `tool result ${turn}`, tool_name: 'read', tool_output: longText, details: { output: longText } },
      { seq: base + 7, type: 'assistant/reasoning', version: 1, time, summary: `reasoning ${turn} ${longText}` },
      { seq: base + 8, type: 'assistant/message', version: 1, time, summary: `assistant ${turn} ${longText}` },
      { seq: base + 9, type: 'step/end', version: 1, time, summary: `step ${turn} end` },
      { seq: base + 10, type: 'turn/end', version: 1, time, summary: `turn ${turn} end` },
    )
  }
  return events.slice(0, eventCount)
}

const events = makeEvents()

function json(route, value) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) })
}

async function installApiMock(page) {
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/sessions') return json(route, [{ id: 'perf', title: 'Performance fixture', blank: false, event_count: events.length, updated_at: '2026-08-26T10:01:02Z' }])
    if (url.pathname === '/api/workspaces') return json(route, { workspaces: [], ungrouped_ids: ['perf'] })
    if (url.pathname === '/api/config') return json(route, { model: 'test-model', llm_provider: 'test', commands: [] })
    if (url.pathname === '/api/sessions/perf/events/stream') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: 'retry: 3000\n\n' })
    if (url.pathname === '/api/sessions/perf/events') return json(route, { events, has_more: false, first_seq: 1, last_seq: events.length })
    if (url.pathname === '/api/sessions/perf/feedback') return json(route, [])
    if (url.pathname === '/api/sessions/perf/queue') return json(route, [])
    if (url.pathname === '/api/interactions') return json(route, { interactions: [] })
    if (url.pathname === '/api/sessions/perf/config') return json(route, { provider: 'test', model: 'test-model', reasoning_effort: '' })
    if (url.pathname === '/api/sessions/perf/context') return json(route, { used_tokens: 0, context_window: 1000, percent: 0 })
    if (url.pathname === '/api/sessions/perf/state') return json(route, { plan_mode: false, goals: [], plans: [] })
    return json(route, {})
  })
}

async function waitForServer() {
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    try {
      if ((await fetch(baseUrl)).ok) return
    } catch {
      // Vite is still starting.
    }
    await new Promise(resolvePromise => setTimeout(resolvePromise, 100))
  }
  throw new Error(`timed out waiting for ${baseUrl}`)
}

async function measureBrowser(page) {
  const started = performance.now()
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded', timeout: 60_000 })
  const trajectoryTab = page.getByRole('tab', { name: /Trajectory/ })
  await trajectoryTab.waitFor({ timeout: 60_000 })
  await trajectoryTab.click()
  const loadMs = performance.now() - started
  const mountedRows = await page.locator('.virtual-row').count()
  assert.ok(mountedRows < 200, `virtualized history mounted too many rows: ${mountedRows}`)

  const searchStarted = performance.now()
  const search = page.getByRole('textbox', { name: 'Search trajectory records' })
  await search.fill('tool result 777')
  await page.waitForTimeout(250)
  const searchMs = performance.now() - searchStarted
  assert.equal(await page.locator('.virtual-row').count(), 1)

  const scroll = await page.locator('.event-scroll').evaluate(async element => {
    const hostElement = element
    let frames = 0
    let running = true
    const tick = () => {
      frames += 1
      if (running) requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
    const startedAt = performance.now()
    for (let index = 0; index < 40; index += 1) {
      hostElement.scrollTop = (hostElement.scrollHeight * index) / 40
      await new Promise(resolvePromise => requestAnimationFrame(resolvePromise))
    }
    await new Promise(resolvePromise => setTimeout(resolvePromise, 500))
    running = false
    const elapsed = performance.now() - startedAt
    return { elapsedMs: elapsed, fps: frames / (elapsed / 1000) }
  })
  assert.ok(scroll.fps >= 30, `scroll baseline below 30 FPS: ${scroll.fps.toFixed(1)}`)

  const memory = await page.evaluate(() => {
    const value = performance.memory?.usedJSHeapSize
    return typeof value === 'number' ? Math.round(value / 1024 / 1024) : null
  })
  return { eventCount: events.length, mountedRows, loadMs: Math.round(loadMs), searchMs: Math.round(searchMs), scroll, heapMiB: memory }
}

const server = spawn(process.execPath, [vite, '--host', host, '--port', String(port)], {
  cwd: webRoot,
  stdio: 'ignore',
  windowsHide: true,
})
try {
  await waitForServer()
  const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] })
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
    await installApiMock(page)
    const result = await measureBrowser(page)
    await page.close()
    console.log(JSON.stringify({ browser: 'playwright-chromium', ...result }))
  } finally {
    await browser.close()
  }
} finally {
  server.kill()
}
