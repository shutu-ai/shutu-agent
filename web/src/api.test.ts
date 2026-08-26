import { describe, expect, it } from 'vitest'
import { ShutuApi } from './api'

describe('ShutuApi', () => {
  it('loads the sanitized read-only config endpoint with bearer authentication', async () => {
    let requestURL = ''
    let requestHeaders: Headers | undefined
    const api = new ShutuApi('https://shutu.test', 'secret', async (input, init) => {
      requestURL = String(input)
      requestHeaders = new Headers(init?.headers)
      return new Response(JSON.stringify({ model: 'deepseek-v4-flash', terminal_enabled: true }), { status: 200 })
    })

    await expect(api.getConfig()).resolves.toMatchObject({ model: 'deepseek-v4-flash', terminal_enabled: true })
    expect(requestURL).toBe('https://shutu.test/api/config')
    expect(requestHeaders?.get('Authorization')).toBe('Bearer secret')
  })

  it('loads session-scoped subagents and jobs', async () => {
    const requests: string[] = []
    const api = new ShutuApi('https://shutu.test', '', async input => {
      requests.push(String(input))
      const body = String(input).includes('/subagents')
        ? { subagents: [{ id: 'a-1', label: '分析', running: true }] }
        : { jobs: [{ id: 'j-1', kind: 'workflow', label: '构建', status: 'running' }] }
      return new Response(JSON.stringify(body), { status: 200 })
    })

    await expect(api.listSubagents('session/1')).resolves.toEqual([{ id: 'a-1', label: '分析', running: true }])
    await expect(api.listJobs('session/1')).resolves.toEqual([{ id: 'j-1', kind: 'workflow', label: '构建', status: 'running' }])
    expect(requests).toEqual([
      'https://shutu.test/api/subagents?session_id=session%2F1',
      'https://shutu.test/api/jobs?session_id=session%2F1',
    ])
  })

  it('maps feedback, image upload, protected image download and image messages', async () => {
    const requests: { path: string; method: string; body?: string; multipart?: boolean }[] = []
    const api = new ShutuApi('https://shutu.test', 'secret', async (input, init) => {
      const path = new URL(String(input)).pathname
      requests.push({ path, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined, multipart: init?.body instanceof FormData })
      if (path.endsWith('/attachments')) return new Response(JSON.stringify({ id: 'img-1', media_type: 'image/png', bytes: 4 }), { status: 201 })
      if (path.endsWith('/feedback')) return new Response(JSON.stringify([]), { status: 200 })
      if (path.includes('/feedback/')) return new Response(JSON.stringify({ session_id: 's1', seq: 2, rating: 'positive' }), { status: 200 })
      if (path.endsWith('/attachments/img-1')) return new Response(new Blob(['data'], { type: 'image/png' }), { status: 200 })
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    })

    await api.uploadAttachment('s1', new File(['data'], 'photo.png', { type: 'image/png' }))
    await api.sendMessage('s1', '带图', ['img-1'])
    await api.listFeedback('s1')
    await api.putFeedback('s1', 2, 'positive')
    await api.deleteFeedback('s1', 2)
    await expect(api.loadAttachment('s1', 'img-1')).resolves.toBeInstanceOf(Blob)

    expect(requests).toEqual([
      { path: '/api/sessions/s1/attachments', method: 'POST', body: undefined, multipart: true },
      { path: '/api/sessions/s1/message', method: 'POST', body: '{"text":"带图","images":["img-1"]}', multipart: false },
      { path: '/api/sessions/s1/feedback', method: 'GET', body: undefined, multipart: false },
      { path: '/api/sessions/s1/feedback/2', method: 'PUT', body: '{"rating":"positive","note":""}', multipart: false },
      { path: '/api/sessions/s1/feedback/2', method: 'DELETE', body: undefined, multipart: false },
      { path: '/api/sessions/s1/attachments/img-1', method: 'GET', body: undefined, multipart: false },
    ])
  })

  it('builds encoded cursor requests with bearer authentication', async () => {
    let requestURL = ''
    let requestHeaders: Headers | undefined
    const api = new ShutuApi('https://shutu.test', 'secret', async (input, init) => {
      requestURL = String(input)
      requestHeaders = new Headers(init?.headers)
      return new Response(JSON.stringify({ events: [], has_more: false }), { status: 200 })
    })

    await api.loadEvents('session/with space', { beforeSeq: 42, limit: 7 })

    expect(requestURL).toBe('https://shutu.test/api/sessions/session%2Fwith%20space/events?limit=7&before_seq=42')
    expect(requestHeaders?.get('Authorization')).toBe('Bearer secret')
    expect(requestHeaders?.get('Accept')).toBe('application/json')
  })

  it('maps workspace, directory, remote search and file APIs', async () => {
    const requests: { path: string; method: string; body?: string }[] = []
    const api = new ShutuApi('https://shutu.test', 'secret', async (input, init) => {
      const url = new URL(String(input))
      requests.push({ path: `${url.pathname}${url.search}`, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
      if (url.pathname === '/api/workspaces' && (init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify({ workspaces: [{ id: 'w1', title: 'Project', session_ids: ['s1'], created_at: 1 }], ungrouped_ids: [] }), { status: 200 })
      if (url.pathname === '/api/workspaces/directories') return new Response(JSON.stringify({ path: 'C:/project', home: 'C:/Users/test', crumbs: [], entries: [{ name: 'src', path: 'C:/project/src' }] }), { status: 200 })
      if (url.pathname === '/api/search') return new Response(JSON.stringify({ hits: [{ id: 's1', snippet: 'match', updated_at: '2026-08-25T00:00:00Z' }] }), { status: 200 })
      if (url.pathname.endsWith('/files')) return new Response(JSON.stringify({ root: 'C:/project', path: '', entries: [{ name: 'README.md', path: 'README.md', dir: false, size: 4 }] }), { status: 200 })
      if (url.pathname.endsWith('/file')) return new Response(JSON.stringify({ path: 'README.md', content: 'docs', start_line: 1, end_line: 1, total_lines: 1 }), { status: 200 })
      return new Response(JSON.stringify({ ok: true, id: 'w1', title: 'Project', path: 'C:/project' }), { status: 200 })
    })

    await expect(api.listWorkspaces()).resolves.toMatchObject({ workspaces: [{ id: 'w1' }] })
    await expect(api.createWorkspace('Project', 'C:/project')).resolves.toMatchObject({ id: 'w1' })
    await expect(api.listWorkspaceDirectories('C:/project')).resolves.toMatchObject({ path: 'C:/project' })
    await expect(api.searchSessions('match')).resolves.toHaveLength(1)
    await expect(api.listFiles('s/1', '', 'readme')).resolves.toMatchObject({ entries: [{ path: 'README.md' }] })
    await expect(api.previewFile('s/1', 'README.md', 1, 3)).resolves.toMatchObject({ content: 'docs' })
    expect(requests).toEqual([
      { path: '/api/workspaces', method: 'GET' },
      { path: '/api/workspaces', method: 'POST', body: '{"title":"Project","path":"C:/project"}' },
      { path: '/api/workspaces/directories?path=C%3A%2Fproject', method: 'GET' },
      { path: '/api/search?q=match', method: 'GET' },
      { path: '/api/sessions/s%2F1/files?q=readme', method: 'GET' },
      { path: '/api/sessions/s%2F1/file?path=README.md&start=1&end=3', method: 'GET' },
    ])
  })

  it('resumes SSE with Last-Event-ID and parses event frames', async () => {
    let requestHeaders: Headers | undefined
    const first = { seq: 8, type: 'tool/start', version: 1, time: '2026-08-25T00:00:00Z', summary: 'start' }
    const second = { seq: 9, type: 'tool/result', version: 1, time: '2026-08-25T00:00:01Z', summary: 'done' }
    const payload = `id: 8\ndata: ${JSON.stringify(first)}\n\nid: 9\ndata: ${JSON.stringify(second)}\n\n`
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(payload))
        controller.close()
      },
    })
    const api = new ShutuApi('https://shutu.test', '', async (_input, init) => {
      requestHeaders = new Headers(init?.headers)
      return new Response(stream, { status: 200 })
    })
    const events: number[] = []

    await api.stream('session-1', 7, new AbortController().signal, event => events.push(event.seq))

    expect(requestHeaders?.get('Last-Event-ID')).toBe('7')
    expect(requestHeaders?.get('Accept')).toBe('text/event-stream')
    expect(events).toEqual([8, 9])
  })

  it('ignores malformed or incomplete SSE frames without dropping valid events', async () => {
    const payload = 'data: {not-json}\n\ndata: {"seq": 10, "type": "tool/result", "version": 1, "time": "2026-08-25T00:00:00Z", "summary": "ok"}\n\ndata: {"type": "missing-seq"}\n\n'
    const stream = new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(new TextEncoder().encode(payload)); controller.close() },
    })
    const api = new ShutuApi('https://shutu.test', '', async () => new Response(stream, { status: 200 }))
    const events: number[] = []

    await api.stream('session-1', 0, new AbortController().signal, event => events.push(event.seq))

    expect(events).toEqual([10])
  })

  it('maps sidebar session actions to the new session endpoints', async () => {
    const requests: { path: string; method: string; body?: string }[] = []
    const api = new ShutuApi('https://shutu.test', '', async (input, init) => {
      requests.push({ path: new URL(String(input)).pathname, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
      return new Response(JSON.stringify({ id: 'new-session', title: 'Renamed' }), { status: 200 })
    })

    await api.createSession()
    await api.resumeSession('session-1')
    await api.renameSession('session-1', 'Renamed')
    await api.archiveSession('session-1')
    await api.deleteSession('session-1')

    expect(requests).toEqual([
      { path: '/api/sessions', method: 'POST', body: '{}' },
      { path: '/api/sessions/session-1/resume', method: 'POST' },
      { path: '/api/sessions/session-1/title', method: 'PATCH', body: '{"title":"Renamed"}' },
      { path: '/api/sessions/session-1/archive', method: 'POST' },
      { path: '/api/sessions/session-1', method: 'DELETE' },
    ])
  })

  it('maps fork, queue and interaction controls to session-scoped endpoints', async () => {
    const requests: { path: string; method: string; body?: string }[] = []
    const api = new ShutuApi('https://shutu.test', '', async (input, init) => {
      const url = new URL(String(input))
      requests.push({ path: `${url.pathname}${url.search}`, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
      if (url.pathname.endsWith('/queue') && (init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify([{ id: 'q1', text: 'next', placement: 'queued' }]), { status: 200 })
      if (url.pathname.endsWith('/queue') && init?.method === 'POST') return new Response(JSON.stringify({ id: 'q2', text: 'next', placement: 'queued' }), { status: 202 })
      if (url.pathname === '/api/interactions') return new Response(JSON.stringify({ interactions: [{ id: 'i1', prompt: 'Allow?', status: 'pending' }] }), { status: 200 })
      return new Response(JSON.stringify({ id: 'fork-1', ok: true }), { status: 200 })
    })

    await expect(api.forkSession('s/1')).resolves.toMatchObject({ id: 'fork-1' })
    await expect(api.listQueue('s/1')).resolves.toEqual([{ id: 'q1', text: 'next', placement: 'queued' }])
    await expect(api.enqueueQueue('s/1', 'next')).resolves.toMatchObject({ id: 'q2' })
    await api.updateQueue('s/1', 'q1', 'steer')
    await expect(api.listInteractions('s/1')).resolves.toMatchObject([{ id: 'i1', status: 'pending' }])
    await api.resolveInteraction('s/1', 'i1', 'approved', 'yes')

    expect(requests).toEqual([
      { path: '/api/sessions/s%2F1/fork', method: 'POST' },
      { path: '/api/sessions/s%2F1/queue', method: 'GET' },
      { path: '/api/sessions/s%2F1/queue', method: 'POST', body: '{"text":"next"}' },
      { path: '/api/sessions/s%2F1/queue/q1', method: 'PATCH', body: '{"action":"steer"}' },
      { path: '/api/interactions?session_id=s%2F1', method: 'GET' },
      { path: '/api/interactions/i1/resolve?session_id=s%2F1', method: 'POST', body: '{"status":"approved","answer":"yes"}' },
    ])
  })

  it('maps P8 settings, model, session-config and export APIs', async () => {
    const requests: { path: string; method: string; body?: string }[] = []
    const api = new ShutuApi('https://shutu.test', '', async (input, init) => {
      const url = new URL(String(input))
      requests.push({ path: `${url.pathname}${url.search}`, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
      if (url.pathname === '/api/session.export') return new Response(new Blob(['zip']), { status: 200 })
      if (url.pathname === '/api/settings' && (init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify({ agent_preset: 'standard' }), { status: 200 })
      if (url.pathname === '/api/config/provider/discover') return new Response(JSON.stringify({ models: [{ id: 'model-a' }] }), { status: 200 })
      if (url.pathname === '/api/config/mcp/refresh' || url.pathname === '/api/config/mcp') return new Response(JSON.stringify({ servers: [{ name: 'fs' }] }), { status: 200 })
      if (url.pathname.endsWith('/config') && (init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify({ provider: 'deepseek-official' }), { status: 200 })
      if (url.pathname.endsWith('/config')) return new Response(JSON.stringify({ provider: 'deepseek-official', model: 'model-a' }), { status: 200 })
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    })

    await expect(api.getSettings()).resolves.toMatchObject({ agent_preset: 'standard' })
    await api.updateSettings({ permission_preset: 'full' })
    await api.switchModel('deepseek-official', 'model-a', 'high')
    await api.saveProvider({ id: 'custom', custom: true, base_url: 'https://provider.test', model: 'model-a' })
    await api.deleteProvider('custom')
    await expect(api.discoverProvider({ provider: 'custom', base_url: 'https://provider.test' })).resolves.toEqual([{ id: 'model-a' }])
    await expect(api.manageMcp('add', { name: 'fs', cmd: 'npx', args: ['server'] })).resolves.toEqual([{ name: 'fs' }])
    await expect(api.refreshMcp()).resolves.toEqual([{ name: 'fs' }])
    await expect(api.getSessionConfig('s1')).resolves.toMatchObject({ provider: 'deepseek-official' })
    await expect(api.updateSessionConfig('s1', { model: 'model-a' })).resolves.toMatchObject({ model: 'model-a' })
    await expect(api.exportSession('s1')).resolves.toBeInstanceOf(Blob)

    expect(requests.map(item => `${item.method} ${item.path}`)).toEqual([
      'GET /api/settings', 'PATCH /api/settings', 'POST /api/config/model', 'POST /api/config/provider', 'DELETE /api/config/provider',
      'POST /api/config/provider/discover', 'POST /api/config/mcp', 'POST /api/config/mcp/refresh', 'GET /api/sessions/s1/config',
      'PATCH /api/sessions/s1/config', 'GET /api/session.export?sessionId=s1&includeDescendants=true',
    ])
  })

  it('maps P10 context, session state and skills APIs', async () => {
    const requests: { path: string; method: string; body?: string }[] = []
    const api = new ShutuApi('https://shutu.test', '', async (input, init) => {
      const url = new URL(String(input))
      requests.push({ path: `${url.pathname}${url.search}`, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
      if (url.pathname.endsWith('/context')) return new Response(JSON.stringify({ used_tokens: 20, context_window: 100, percent: 20 }), { status: 200 })
      if (url.pathname.endsWith('/state')) return new Response(JSON.stringify({ session_id: 's1', plan_mode: true, goals: [{ id: 'g1' }] }), { status: 200 })
      if (url.pathname === '/api/config/skills' && (init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify({ skills: [{ name: 'demo', enabled: true }], groups: [], scopes: [{ id: 'global' }] }), { status: 200 })
      return new Response(JSON.stringify({ name: 'demo', content: '# Demo' }), { status: 200 })
    })

    await expect(api.getContext('s/1')).resolves.toMatchObject({ used_tokens: 20, percent: 20 })
    await expect(api.getSessionState('s/1')).resolves.toMatchObject({ plan_mode: true })
    await expect(api.listSkills()).resolves.toMatchObject({ skills: [{ name: 'demo' }] })
    await expect(api.skillAction('set_enabled', { name: 'demo', scope: 'global', enabled: false })).resolves.toMatchObject({ name: 'demo' })

    expect(requests).toEqual([
      { path: '/api/sessions/s%2F1/context', method: 'GET' },
      { path: '/api/sessions/s%2F1/state', method: 'GET' },
      { path: '/api/config/skills', method: 'GET' },
      { path: '/api/config/skills', method: 'POST', body: '{"action":"set_enabled","name":"demo","scope":"global","enabled":false}' },
    ])
  })
})
