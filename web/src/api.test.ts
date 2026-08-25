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
})
