import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const { chromium } = createRequire(import.meta.url)(resolve(dshRoot, 'apps/web/node_modules/playwright'))
const baseUrl = process.env.SHUTU_REAL_TASK_URL ?? 'http://127.0.0.1:18099'
const sessionId = process.argv[2]
const durationSeconds = Number(process.argv[3] ?? process.env.SHUTU_REAL_TASK_SECONDS ?? 900)
const skipSelection = process.env.SHUTU_REAL_TASK_SKIP_SELECTION === '1'

if (!existsSync(resolve(dshRoot, 'apps/web/node_modules/playwright'))) {
  throw new Error(`Playwright is unavailable under ${dshRoot}`)
}
if (!sessionId) throw new Error('usage: node scripts/real-task-performance.mjs <session-id> [duration-seconds]')
if (!Number.isFinite(durationSeconds) || durationSeconds <= 0) throw new Error('duration must be positive')

async function sessionSummary() {
  const response = await fetch(`${baseUrl}/api/sessions`)
  if (!response.ok) throw new Error(`session list returned HTTP ${response.status}`)
  const sessions = await response.json()
  const summary = sessions.find(entry => entry.id === sessionId)
  if (!summary) throw new Error(`session ${sessionId} is absent from the session list`)
  return summary
}

async function selectSession(page, summary) {
  const rows = page.locator('.session-row')
  const count = await rows.count()
  for (let index = 0; index < count; index += 1) {
    const row = rows.nth(index)
    const text = await row.innerText()
    if (summary.title && text.includes(summary.title)) {
      await row.locator('button.session').click()
      return
    }
  }

  // A collapsed/filtered sidebar may hide the row; title search is more useful
  // than searching by ID because the UI renders title and event count, not IDs.
  await page.getByRole('button', { name: 'Search sessions' }).click()
  const search = page.getByRole('textbox', { name: 'Search sessions' })
  await search.fill(summary.title ?? sessionId)
  const remoteResult = page.locator('.remote-search-hit').first()
  try {
    await remoteResult.waitFor({ state: 'visible', timeout: 10_000 })
    await remoteResult.click()
    return
  } catch {
    // Fall through to the clear diagnostic below.
  }
  throw new Error(`session ${sessionId} was not selectable from the session list`)
}

async function eventTail() {
  const response = await fetch(`${baseUrl}/api/sessions/${encodeURIComponent(sessionId)}/events?limit=1`)
  if (!response.ok) throw new Error(`event tail returned HTTP ${response.status}`)
  const page = await response.json()
  const last = page.events?.at(-1)
  return { count: last?.seq ?? 0, type: last?.type ?? null, time: last?.time ?? null }
}

async function installBrowserMetrics(page) {
  await page.evaluate(() => {
    window.__shutuRealMetrics = { frames: 0, longTasks: 0, longTaskMs: 0 }
    const metrics = window.__shutuRealMetrics
    const frame = () => {
      metrics.frames += 1
      requestAnimationFrame(frame)
    }
    requestAnimationFrame(frame)
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
        // Long-task entries are optional in some Chromium modes.
      }
    }
  })
}

async function sampleBrowser(page) {
  return page.evaluate(() => {
    const metrics = window.__shutuRealMetrics ?? { frames: 0, longTasks: 0, longTaskMs: 0 }
    const frames = metrics.frames
    metrics.frames = 0
    const heap = performance.memory?.usedJSHeapSize
    return {
      frames,
      heapMiB: typeof heap === 'number' ? Math.round(heap / 1024 / 1024) : null,
      longTasks: metrics.longTasks,
      longTaskMs: Math.round(metrics.longTaskMs),
      mountedRows: document.querySelectorAll('.virtual-row').length,
      domNodes: document.getElementsByTagName('*').length,
    }
  })
}

const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] })
const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
const consoleErrors = []
page.on('console', message => { if (message.type() === 'error') consoleErrors.push(message.text()) })
page.on('pageerror', error => consoleErrors.push(error.message))
try {
  await page.goto(`${baseUrl}/`, { waitUntil: 'domcontentloaded', timeout: 60_000 })
  await page.getByRole('tab', { name: /Trajectory/ }).waitFor({ timeout: 60_000 })
  if (!skipSelection) await selectSession(page, await sessionSummary())
  await page.getByRole('tab', { name: /Trajectory/ }).click()
  try {
    await page.locator('.event-scroll').waitFor({ state: 'visible', timeout: 60_000 })
  } catch (error) {
    const diagnostic = await page.locator('body').innerText().catch(() => '')
    const htmlLength = await page.locator('body').innerHTML().then(value => value.length).catch(() => -1)
    throw new Error(`event panel did not mount: url=${page.url()} html=${htmlLength} console=${consoleErrors.join(' | ')} body=${diagnostic.slice(0, 2_000)}`, { cause: error })
  }
  await installBrowserMetrics(page)
  process.stderr.write(`observer-ready session=${sessionId}\n`)

  const samples = []
  const startedAt = Date.now()
  while (Date.now() - startedAt < durationSeconds * 1000) {
    await new Promise(resolvePromise => setTimeout(resolvePromise, 1_000))
    const [browserSample, tail] = await Promise.all([sampleBrowser(page), eventTail()])
    const sample = { elapsedSeconds: Math.round((Date.now() - startedAt) / 100) / 10, ...tail, ...browserSample }
    samples.push(sample)
  }

  assert.ok(samples.length > 0)
  const fps = samples.map(sample => sample.frames)
  const heaps = samples.flatMap(sample => sample.heapMiB === null ? [] : [sample.heapMiB])
  const result = {
    url: baseUrl,
    sessionId,
    durationSeconds: samples.at(-1)?.elapsedSeconds ?? 0,
    samples: samples.length,
    firstEventCount: samples[0]?.count ?? 0,
    lastEventCount: samples.at(-1)?.count ?? 0,
    lastEventType: samples.at(-1)?.type ?? null,
    minFps: fps.length > 0 ? Math.min(...fps) : null,
    avgFps: fps.length > 0 ? Math.round(fps.reduce((sum, value) => sum + value, 0) / fps.length * 10) / 10 : null,
    minMountedRows: Math.min(...samples.map(sample => sample.mountedRows)),
    maxMountedRows: Math.max(...samples.map(sample => sample.mountedRows)),
    maxDomNodes: Math.max(...samples.map(sample => sample.domNodes)),
    heapStartMiB: heaps[0] ?? null,
    heapMaxMiB: heaps.length > 0 ? Math.max(...heaps) : null,
    longTasks: samples.at(-1)?.longTasks ?? 0,
    longTaskMs: samples.at(-1)?.longTaskMs ?? 0,
    consoleErrors,
    samplesDetail: samples,
  }
  console.log(JSON.stringify(result))
} finally {
  await page.close()
  await browser.close()
}
