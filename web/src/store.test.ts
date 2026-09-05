import { describe, expect, it } from 'vitest'
import { ShutuApiError, type EventPage, type EventView, type SessionSummary, type WebApi } from './api'
import { sortSessions, WebStore } from './store'

const session = (id: string): SessionSummary => ({
  id, title: id, blank: false, event_count: 2, updated_at: '2026-08-25T00:00:00Z',
})

const event = (seq: number): EventView => ({
  seq, type: 'assistant/message', version: 1, time: '2026-08-25T00:00:00Z', summary: `event ${seq}`,
})

const page = (events: EventView[], hasMore = false): EventPage => ({
  events, has_more: hasMore, first_seq: events[0]?.seq, last_seq: events.at(-1)?.seq,
  ...(hasMore && events[0] !== undefined ? { next_before_seq: events[0].seq } : {}),
})

const waitForAbort = (signal: AbortSignal): Promise<void> => new Promise(resolve => {
  if (signal.aborted) { resolve(); return }
  signal.addEventListener('abort', () => resolve(), { once: true })
})

describe('WebStore', () => {
  it('sorts sidebar sessions by updated time with stable id fallback', () => {
    const sorted = sortSessions([
      { ...session('older'), updated_at: '2026-08-25T00:00:00Z' },
      { ...session('newer'), updated_at: '2026-08-25T01:00:00Z' },
      { ...session('same-b'), updated_at: 'invalid' },
      { ...session('same-a'), updated_at: 'invalid' },
    ])
    expect(sorted.map(item => item.id)).toEqual(['newer', 'older', 'same-b', 'same-a'])
  })

  it('loads a session and de-duplicates live events by sequence', async () => {
    let emitted = false
    let workspaceCreated = false
    const createdWorkspaceIds: string[] = []
    const workspaceSession: SessionSummary = { ...session('new'), blank: true, event_count: 0, workspace_id: 'w1' }
    const api: WebApi = {
      getConfig: async () => ({}),
      getSettings: async () => ({}),
      updateSettings: async () => ({ ok: true }),
      switchModel: async () => ({ ok: true }),
      saveProvider: async () => undefined,
      deleteProvider: async () => undefined,
      discoverProvider: async () => [],
      manageMcp: async () => [],
      refreshMcp: async () => [],
      getSessionConfig: async () => ({}),
      updateSessionConfig: async () => ({}),
      getContext: async () => ({ used_tokens: 0, context_window: 1000, percent: 0 }),
      getSessionState: async () => ({}),
      listSkills: async () => ({ skills: [], groups: [], scopes: [] }),
      skillAction: async () => ({}),
      exportSession: async () => new Blob(),
      listSubagents: async () => [],
      listJobs: async () => [],
      listWorkspaces: async () => workspaceCreated
        ? { workspaces: [{ id: 'w1', title: 'Workspace', session_ids: ['new'], created_at: 1 }], ungrouped_ids: ['one'] }
        : { workspaces: [], ungrouped_ids: ['one'] },
      createWorkspace: async (_title, path = '') => {
        workspaceCreated = true
        return { id: 'w1', title: 'Workspace', path }
      },
      pickWorkspaceDirectory: async () => ({ path: '' }),
      listWorkspaceDirectories: async () => ({ path: '', home: '', crumbs: [], entries: [] }),
      createWorkspaceDirectory: async (_path, name) => ({ path: name }),
      renameWorkspace: async () => undefined,
      deleteWorkspace: async () => undefined,
      reorderWorkspaces: async () => undefined,
      reorderSessions: async () => undefined,
      searchSessions: async () => [],
      listFiles: async () => ({ root: '', path: '', entries: [] }),
      previewFile: async () => ({ path: '', content: '', start_line: 1, end_line: 1, total_lines: 1 }),
      forkSession: async () => ({ id: 'fork' }),
      listQueue: async () => [],
      enqueueQueue: async () => ({ id: 'q', text: '' }),
      updateQueue: async () => undefined,
      listInteractions: async () => [],
      resolveInteraction: async () => undefined,
      listFeedback: async () => [],
      putFeedback: async (_id, seq, rating) => ({ session_id: 'one', seq, rating }),
      deleteFeedback: async () => undefined,
      uploadAttachment: async () => ({ id: 'a', media_type: 'image/png', bytes: 1 }),
      loadAttachment: async () => new Blob(),
      listSessions: async () => workspaceCreated ? [workspaceSession, session('one')] : [session('one')],
      createSession: async workspaceId => {
        createdWorkspaceIds.push(workspaceId ?? '')
        return { id: 'new', workspace_id: workspaceId }
      },
      resumeSession: async () => undefined,
      renameSession: async (_id, title) => ({ title }),
      archiveSession: async () => undefined,
      deleteSession: async () => undefined,
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

    await store.createWorkspace('Workspace')
    expect(createdWorkspaceIds).toEqual(['w1'])
    expect(store.getSnapshot().selectedId).toBe('new')

    await store.createSession()
    expect(createdWorkspaceIds).toEqual(['w1', 'w1'])
    expect(store.getSnapshot().selectedId).toBe('new')
  })

  it('ignores an older page that resolves after switching sessions', async () => {
    let resolveOlder!: (value: EventPage) => void
    const olderPage = new Promise<EventPage>(resolve => { resolveOlder = resolve })
    const api: WebApi = {
      getConfig: async () => ({}),
      getSettings: async () => ({}),
      updateSettings: async () => ({ ok: true }),
      switchModel: async () => ({ ok: true }),
      saveProvider: async () => undefined,
      deleteProvider: async () => undefined,
      discoverProvider: async () => [],
      manageMcp: async () => [],
      refreshMcp: async () => [],
      getSessionConfig: async () => ({}),
      updateSessionConfig: async () => ({}),
      getContext: async () => ({ used_tokens: 0, context_window: 1000, percent: 0 }),
      getSessionState: async () => ({}),
      listSkills: async () => ({ skills: [], groups: [], scopes: [] }),
      skillAction: async () => ({}),
      exportSession: async () => new Blob(),
      listSubagents: async () => [],
      listJobs: async () => [],
      listWorkspaces: async () => ({ workspaces: [], ungrouped_ids: [] }),
      createWorkspace: async (_title, path = '') => ({ id: 'w1', title: 'Workspace', path }),
      pickWorkspaceDirectory: async () => ({ path: '' }),
      listWorkspaceDirectories: async () => ({ path: '', home: '', crumbs: [], entries: [] }),
      createWorkspaceDirectory: async (_path, name) => ({ path: name }),
      renameWorkspace: async () => undefined,
      deleteWorkspace: async () => undefined,
      reorderWorkspaces: async () => undefined,
      reorderSessions: async () => undefined,
      searchSessions: async () => [],
      listFiles: async () => ({ root: '', path: '', entries: [] }),
      previewFile: async () => ({ path: '', content: '', start_line: 1, end_line: 1, total_lines: 1 }),
      forkSession: async () => ({ id: 'fork' }),
      listQueue: async () => [],
      enqueueQueue: async () => ({ id: 'q', text: '' }),
      updateQueue: async () => undefined,
      listInteractions: async () => [],
      resolveInteraction: async () => undefined,
      listFeedback: async () => [],
      putFeedback: async (_id, seq, rating) => ({ session_id: 'one', seq, rating }),
      deleteFeedback: async () => undefined,
      uploadAttachment: async () => ({ id: 'a', media_type: 'image/png', bytes: 1 }),
      loadAttachment: async () => new Blob(),
      listSessions: async () => [],
      createSession: async () => ({ id: 'new' }),
      resumeSession: async () => undefined,
      renameSession: async (_id, title) => ({ title }),
      archiveSession: async () => undefined,
      deleteSession: async () => undefined,
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
    expect(store.getSnapshot().historyStartSeq).toBe(10)
    expect(store.getSnapshot().historyEndSeq).toBe(10)
    expect(store.getSnapshot().historyCursor).toEqual({ beforeSeq: 10 })
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
      getConfig: async () => ({}),
      getSettings: async () => ({}),
      updateSettings: async () => ({ ok: true }),
      switchModel: async () => ({ ok: true }),
      saveProvider: async () => undefined,
      deleteProvider: async () => undefined,
      discoverProvider: async () => [],
      manageMcp: async () => [],
      refreshMcp: async () => [],
      getSessionConfig: async () => ({}),
      updateSessionConfig: async () => ({}),
      getContext: async () => ({ used_tokens: 0, context_window: 1000, percent: 0 }),
      getSessionState: async () => ({}),
      listSkills: async () => ({ skills: [], groups: [], scopes: [] }),
      skillAction: async () => ({}),
      exportSession: async () => new Blob(),
      listSubagents: async () => [],
      listJobs: async () => [],
      listWorkspaces: async () => ({ workspaces: [], ungrouped_ids: ['secure'] }),
      createWorkspace: async (_title, path = '') => ({ id: 'w1', title: 'Workspace', path }),
      pickWorkspaceDirectory: async () => ({ path: '' }),
      listWorkspaceDirectories: async () => ({ path: '', home: '', crumbs: [], entries: [] }),
      createWorkspaceDirectory: async (_path, name) => ({ path: name }),
      renameWorkspace: async () => undefined,
      deleteWorkspace: async () => undefined,
      reorderWorkspaces: async () => undefined,
      reorderSessions: async () => undefined,
      searchSessions: async () => [],
      listFiles: async () => ({ root: '', path: '', entries: [] }),
      previewFile: async () => ({ path: '', content: '', start_line: 1, end_line: 1, total_lines: 1 }),
      forkSession: async () => ({ id: 'fork' }),
      listQueue: async () => [],
      enqueueQueue: async () => ({ id: 'q', text: '' }),
      updateQueue: async () => undefined,
      listInteractions: async () => [],
      resolveInteraction: async () => undefined,
      listFeedback: async () => [],
      putFeedback: async (_id, seq, rating) => ({ session_id: 'secure', seq, rating }),
      deleteFeedback: async () => undefined,
      uploadAttachment: async () => ({ id: 'a', media_type: 'image/png', bytes: 1 }),
      loadAttachment: async () => new Blob(),
      listSessions: async () => {
        if (!authorized) throw new ShutuApiError('unauthorized', 401)
        return [session('secure')]
      },
      createSession: async () => ({ id: 'new' }),
      resumeSession: async () => undefined,
      renameSession: async (_id, title) => ({ title }),
      archiveSession: async () => undefined,
      deleteSession: async () => undefined,
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
