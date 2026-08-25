import { describe, expect, it } from 'vitest'
import { ShutuApi } from './api'

describe('ShutuApi', () => {
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
})
