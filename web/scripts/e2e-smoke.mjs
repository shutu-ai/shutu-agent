import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? resolve(webRoot, '../../deepseek-harness'))
const vite = resolve(dshRoot, 'apps/web/node_modules/vite/bin/vite.js')
const { chromium } = createRequire(import.meta.url)(resolve(dshRoot, 'apps/web/node_modules/playwright'))
const host = '127.0.0.1'
const port = Number(process.env.SHUTU_E2E_PORT ?? 18117)
const baseUrl = `http://${host}:${port}/`

if (!existsSync(vite)) {
  throw new Error(`Vite is unavailable at ${vite}; set SHUTU_DSH_ROOT to a DSH checkout.`)
}

const events = [
  { seq: 1, type: 'turn/start', version: 1, time: '2026-08-26T10:00:00Z', summary: 'turn start', details: { turn: 1 } },
  { seq: 2, type: 'user/message', version: 1, time: '2026-08-26T10:00:01Z', summary: 'inspect repository' },
  { seq: 3, type: 'step/start', version: 1, time: '2026-08-26T10:00:02Z', summary: 'step start' },
  { seq: 4, type: 'assistant/chunk', version: 1, time: '2026-08-26T10:00:03Z', summary: 'stream delta' },
  { seq: 5, type: 'tool/result', version: 1, time: '2026-08-26T10:00:04Z', summary: 'files listed', tool_name: 'glob', tool_output: 'src/app.tsx', details: { output: Array.from({ length: 20 }, (_, index) => `matched file ${index + 1}: src/components/example-${index + 1}.tsx`).join('\n') } },
  { seq: 6, type: 'assistant/reasoning', version: 1, time: '2026-08-26T10:00:05Z', summary: 'reasoning' },
  { seq: 7, type: 'assistant/message', version: 1, time: '2026-08-26T10:00:06Z', summary: 'Repository inspected' },
  { seq: 8, type: 'step/end', version: 1, time: '2026-08-26T10:00:07Z', summary: 'step end' },
  { seq: 9, type: 'turn/end', version: 1, time: '2026-08-26T10:00:08Z', summary: 'turn end' },
  { seq: 10, type: 'turn/start', version: 1, time: '2026-08-26T10:01:00Z', summary: 'turn start', details: { turn: 2 } },
  { seq: 11, type: 'user/message', version: 1, time: '2026-08-26T10:01:01Z', summary: 'continue' },
  { seq: 12, type: 'assistant/message', version: 1, time: '2026-08-26T10:01:02Z', summary: 'Done' },
]

function json(route, value) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) })
}

async function assertVisibleRowsDoNotOverlap(page) {
  let minimumGap = Number.NEGATIVE_INFINITY
  for (let attempt = 0; attempt < 10; attempt += 1) {
    minimumGap = await page.locator('.virtual-row').evaluateAll(elements => {
      const boxes = elements.map(element => {
        const rect = element.getBoundingClientRect()
        return { top: rect.top, bottom: rect.bottom }
      }).sort((left, right) => left.top - right.top)
      return boxes.slice(1).reduce((gap, box, index) => Math.min(gap, box.top - boxes[index].bottom), Number.POSITIVE_INFINITY)
    })
    if (minimumGap >= -1) return
    await page.waitForTimeout(50)
  }
  assert.ok(minimumGap >= -1, `dynamic virtual rows overlap by ${Math.abs(minimumGap)}px`)
}

async function installApiMock(page) {
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/sessions') return json(route, [{ id: 'p18', title: 'P18 smoke fixture', blank: false, event_count: events.length, updated_at: '2026-08-26T10:01:02Z' }])
    if (url.pathname === '/api/workspaces') return json(route, { workspaces: [], ungrouped_ids: ['p18'] })
    if (url.pathname === '/api/config') return json(route, { model: 'test-model', llm_provider: 'test', commands: [] })
    if (url.pathname === '/api/sessions/p18/resume') return json(route, { ok: true })
    if (url.pathname === '/api/sessions/p18/events/stream') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: 'retry: 3000\n\n' })
    if (url.pathname === '/api/sessions/p18/events') return json(route, { events, has_more: false, first_seq: 1, last_seq: events.length })
    if (url.pathname === '/api/sessions/p18/feedback') return json(route, [])
    if (url.pathname === '/api/sessions/p18/queue') return json(route, [])
    if (url.pathname === '/api/interactions') return json(route, { interactions: [] })
    if (url.pathname === '/api/sessions/p18/config') return json(route, { provider: 'test', model: 'test-model', reasoning_effort: '' })
    if (url.pathname === '/api/sessions/p18/context') return json(route, { used_tokens: 0, context_window: 1000, percent: 0 })
    if (url.pathname === '/api/sessions/p18/state') return json(route, { plan_mode: false, goals: [], plans: [] })
    return json(route, {})
  })
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

async function runDesktop(browser) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  await installApiMock(page)
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await page.getByRole('tab', { name: /Trajectory/ }).waitFor()
  assert.equal(await page.title(), 'Shutu DSH Web')
  assert.match(await page.locator('body').innerText(), /P18 smoke fixture/)
  await page.getByRole('tab', { name: /Trajectory/ }).click()
  const collapse = page.getByRole('button', { name: 'Collapse turns' })
  await collapse.waitFor()
  const before = await page.locator('.virtual-row').count()
  const toolRow = page.locator('.virtual-row').filter({ hasText: 'src/app.tsx' })
  await toolRow.getByRole('button', { name: 'Expand details' }).click()
  await assertVisibleRowsDoNotOverlap(page)
  await collapse.click()
  await page.getByRole('button', { name: 'Expand turns' }).waitFor()
  const after = await page.locator('.virtual-row').count()
  assert.ok(after < before, `turn collapse did not reduce mounted rows: ${before} -> ${after}`)
  const search = page.getByRole('textbox', { name: 'Search trajectory' })
  await search.fill('glob')
  assert.ok(await page.getByText('src/app.tsx', { exact: true }).count() > 0, 'trajectory search result is not visible')
  assert.deepEqual(issues, [])
  await page.screenshot({ path: resolve(process.env.TEMP ?? process.env.TMP ?? '.', 'shutu-p18-desktop.png') })
  await page.close()
  return { before, after }
}

async function runMobile(browser) {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } })
  const issues = []
  page.on('console', message => { if (message.type() === 'error' || message.type() === 'warning') issues.push(message.text()) })
  page.on('pageerror', error => issues.push(error.message))
  await installApiMock(page)
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' })
  await page.getByRole('tab', { name: /Trajectory/ }).waitFor()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  assert.ok(overflow <= 1, `mobile page has horizontal overflow: ${overflow}px`)
  assert.deepEqual(issues, [])
  await page.screenshot({ path: resolve(process.env.TEMP ?? process.env.TMP ?? '.', 'shutu-p18-mobile.png') })
  await page.close()
}

const server = spawn(process.execPath, [vite, '--host', host, '--port', String(port)], {
  cwd: webRoot,
  stdio: 'ignore',
  windowsHide: true,
})
try {
  await waitForServer()
  const browser = await chromium.launch({ headless: true })
  try {
    const desktop = await runDesktop(browser)
    await runMobile(browser)
    console.log(JSON.stringify({ browser: 'playwright', desktop, mobile: 'ok', console: 'clean' }))
  } finally {
    await browser.close()
  }
} finally {
  server.kill()
}
