import { describe, expect, it } from 'vitest'
import { ShutuApiError, type EventPage, type EventView, type SessionSummary, type WebApi } from './api'
import { WebStore } from './store'

const session = (id: string): SessionSummary => ({
  id, title: id, blank: false, event_count: 2, updated_at: '2026-08-25T00:00:00Z',
})

const event = (seq: number): EventView => ({
  seq, type: 'assistant/message', version: 1, time: '2026-08-25T00:00:00Z', summary: `event ${seq}`,
})

const page = (events: EventView[], hasMore = false): EventPage => ({
  events, has_more: hasMore, first_seq: events[0]?.seq, last_seq: events.at(-1)?.seq,
})

const waitForAbort = (signal: AbortSignal): Promise<void> => new Promise(resolve => {
  if (signal.aborted) { resolve(); return }
  signal.addEventListener('abort', () => resolve(), { once: true })
})

describe('WebStore', () => {
  it('loads a session and de-duplicates live events by sequence', async () => {
    let emitted = false
    const api: WebApi = {
      listSessions: async () => [session('one')],
      loadEvents: async () => page([event(1)]),
      sendMessage: async () => undefined,
      stop: async () => undefined,
      stream: async (_id, _lastSeq, signal, onEvent) => {
        if (!emitted) {
          emitted = true
          onEvent(event(2))
          onEvent(event(2))
        }
        await waitForAbort(signal)
      },
    }
    const store = new WebStore(api)

    await store.start()
    await Promise.resolve()
    expect(store.getSnapshot().selectedId).toBe('one')
    expect(store.getSnapshot().events.map(item => item.seq)).toEqual([1, 2])

    await store.open('other')
    expect(store.getSnapshot().events).toEqual([event(1)])
  })

  it('ignores an older page that resolves after switching sessions', async () => {
    let resolveOlder!: (value: EventPage) => void
    const olderPage = new Promise<EventPage>(resolve => { resolveOlder = resolve })
    const api: WebApi = {
      listSessions: async () => [],
      loadEvents: async (id, cursor) => {
        if (id === 'one' && cursor?.beforeSeq !== undefined) return olderPage
        return page([event(id === 'one' ? 10 : 20)], true)
      },
      sendMessage: async () => undefined,
      stop: async () => undefined,
      stream: async (_id, _lastSeq, signal) => waitForAbort(signal),
    }
    const store = new WebStore(api)

    await store.open('one')
    const loadingOlder = store.loadOlder()
    await Promise.resolve()
    await store.open('two')
    resolveOlder(page([event(1)]))
    await loadingOlder

    expect(store.getSnapshot().selectedId).toBe('two')
    expect(store.getSnapshot().events.map(item => item.seq)).toEqual([20])
    expect(store.getSnapshot().loadingOlder).toBe(false)
  })

  it('marks bearer authentication as required and retries after a token update', async () => {
    let authorized = false
    const api: WebApi = {
      listSessions: async () => {
        if (!authorized) throw new ShutuApiError('unauthorized', 401)
        return [session('secure')]
      },
      loadEvents: async () => page([]),
      sendMessage: async () => undefined,
      stop: async () => undefined,
      stream: async (_id, _lastSeq, signal) => waitForAbort(signal),
      setToken: () => { authorized = true },
    }
    const store = new WebStore(api)

    await store.start()
    expect(store.getSnapshot().authRequired).toBe(true)
    await store.authenticate('token')
    expect(store.getSnapshot().authRequired).toBe(false)
    expect(store.getSnapshot().selectedId).toBe('secure')
  })
})
