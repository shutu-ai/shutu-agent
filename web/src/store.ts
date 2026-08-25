import type { ConfigView, EventView, SessionSummary, WebApi } from './api'
import { ShutuApiError } from './api'

export interface WebState {
  sessions: readonly SessionSummary[]
  selectedId: string | null
  events: readonly EventView[]
  hasOlder: boolean
  loading: boolean
  loadingOlder: boolean
  sending: boolean
  connected: boolean
  error: string | null
  authRequired: boolean
}

const CURRENT_SESSION_KEY = 'shutu.web.current-session'

export function sortSessions(sessions: readonly SessionSummary[]): SessionSummary[] {
  return [...sessions].sort((left, right) => {
    const rightTime = Date.parse(right.updated_at)
    const leftTime = Date.parse(left.updated_at)
    if (Number.isFinite(rightTime) !== Number.isFinite(leftTime)) return Number.isFinite(rightTime) ? 1 : -1
    const updated = rightTime - leftTime
    if (Number.isFinite(updated) && updated !== 0) return updated
    const flat = (right.flat_sort ?? 0) - (left.flat_sort ?? 0)
    return flat !== 0 ? flat : right.id.localeCompare(left.id)
  })
}

function savedSessionId(): string | null {
  if (typeof localStorage === 'undefined') return null
  try { return localStorage.getItem(CURRENT_SESSION_KEY) }
  catch { return null }
}

function saveSessionId(id: string | null): void {
  if (typeof localStorage === 'undefined') return
  try {
    if (id === null) localStorage.removeItem(CURRENT_SESSION_KEY)
    else localStorage.setItem(CURRENT_SESSION_KEY, id)
  } catch {
    // Browser storage is an optimization; session navigation still works.
  }
}

const EMPTY: WebState = {
  sessions: [], selectedId: null, events: [], hasOlder: false,
  loading: false, loadingOlder: false, sending: false, connected: false, error: null, authRequired: false,
}

export class WebStore {
  private state: WebState = EMPTY
  private readonly listeners = new Set<() => void>()
  private readonly knownSeqs = new Set<number>()
  private streamAbort: AbortController | null = null
  private generation = 0
  private pollTimer: ReturnType<typeof setInterval> | null = null

  constructor(private readonly api: WebApi) {}

  getSnapshot = (): WebState => this.state

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  private patch(next: Partial<WebState>): void {
    this.state = { ...this.state, ...next }
    for (const listener of this.listeners) listener()
  }

  async start(): Promise<void> {
    this.patch({ loading: true })
    try {
      const sessions = sortSessions(await this.api.listSessions())
      this.patch({ sessions, error: null, authRequired: false })
      const saved = savedSessionId()
      const current = sessions.find(session => session.id === saved) ?? sessions[0]
      if (current !== undefined) await this.open(current.id)
      else {
        saveSessionId(null)
        this.patch({ selectedId: null, events: [], loading: false })
      }
      if (this.pollTimer === null) {
        this.pollTimer = setInterval(() => { void this.refreshSessions() }, 30_000)
        const timer = this.pollTimer as unknown as { unref?: () => void }
        timer.unref?.()
      }
    } catch (error) {
      this.patch({ loading: false, error: error instanceof Error ? error.message : String(error), authRequired: isUnauthorized(error) })
    }
  }

  async refreshSessions(): Promise<readonly SessionSummary[]> {
    const sessions = sortSessions(await this.api.listSessions())
    this.patch({ sessions })
    return sessions
  }

  async createSession(): Promise<void> {
    const result = await this.api.createSession()
    await this.refreshSessions()
    await this.open(result.id)
  }

  async open(sessionId: string): Promise<void> {
    const generation = ++this.generation
    this.streamAbort?.abort()
    const abort = new AbortController()
    this.streamAbort = abort
    this.knownSeqs.clear()
    saveSessionId(sessionId)
    this.patch({ selectedId: sessionId, events: [], hasOlder: false, loading: true, loadingOlder: false, sending: false, connected: false, error: null, authRequired: false })
    try {
      await this.api.resumeSession(sessionId, abort.signal)
      const page = await this.api.loadEvents(sessionId, { limit: 100 }, abort.signal)
      if (generation !== this.generation) return
      for (const event of page.events) this.knownSeqs.add(event.seq)
      this.patch({ events: page.events, hasOlder: page.has_more, loading: false })
      void this.streamLoop(sessionId, page.last_seq ?? page.events.at(-1)?.seq ?? 0, abort, generation)
    } catch (error) {
      if (abort.signal.aborted) return
      this.patch({ loading: false, error: error instanceof Error ? error.message : String(error), authRequired: isUnauthorized(error) })
    }
  }

  async renameSession(sessionId: string, title: string): Promise<void> {
    await this.api.renameSession(sessionId, title)
    await this.refreshSessions()
  }

  async archiveSession(sessionId: string): Promise<void> {
    await this.api.archiveSession(sessionId)
    await this.afterSessionRemoval(sessionId)
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.api.deleteSession(sessionId)
    await this.afterSessionRemoval(sessionId)
  }

  private async afterSessionRemoval(sessionId: string): Promise<void> {
    const sessions = await this.refreshSessions()
    if (this.state.selectedId !== sessionId) return
    this.streamAbort?.abort()
    const next = sessions[0]
    if (next === undefined) {
      this.generation += 1
      saveSessionId(null)
      this.knownSeqs.clear()
      this.patch({ selectedId: null, events: [], hasOlder: false, loading: false, connected: false })
      return
    }
    await this.open(next.id)
  }

  async loadOlder(): Promise<void> {
    const { selectedId, events, hasOlder, loadingOlder } = this.state
    if (selectedId === null || !hasOlder || loadingOlder || events.length === 0) return
    const generation = this.generation
    const sessionId = selectedId
    this.patch({ loadingOlder: true })
    try {
      const page = await this.api.loadEvents(sessionId, { beforeSeq: events[0].seq, limit: 100 })
      if (generation !== this.generation || sessionId !== this.state.selectedId) return
      const known = new Set(this.state.events.map(event => event.seq))
      const older = page.events.filter(event => !known.has(event.seq))
      this.patch({ events: [...older, ...this.state.events], hasOlder: page.has_more, loadingOlder: false })
    } catch (error) {
      if (generation !== this.generation || sessionId !== this.state.selectedId) return
      this.patch({ loadingOlder: false, error: error instanceof Error ? error.message : String(error) })
    }
  }

  async send(text: string): Promise<void> {
    const id = this.state.selectedId
    if (id === null || text.trim() === '') return
    this.patch({ sending: true })
    try {
      await this.api.sendMessage(id, text.trim())
      await this.refreshSessions()
    } finally { this.patch({ sending: false }) }
  }

  async stop(): Promise<void> {
    const id = this.state.selectedId
    if (id === null) return
    await this.api.stop(id)
    this.patch({ sending: false })
  }

  getToken(): string { return this.api.getToken?.() ?? '' }

  getConfig(signal?: AbortSignal): Promise<ConfigView> {
    return this.api.getConfig(signal)
  }

  async authenticate(token: string): Promise<void> {
    this.api.setToken?.(token)
    this.patch({ authRequired: false, error: null })
    await this.start()
  }

  private async streamLoop(sessionId: string, initialSeq: number, abort: AbortController, generation: number): Promise<void> {
    let cursor = initialSeq
    while (!abort.signal.aborted && generation === this.generation) {
      try {
        this.patch({ connected: true })
        await this.api.stream(sessionId, cursor, abort.signal, event => {
          if (event.seq <= cursor || generation !== this.generation) return
          cursor = event.seq
          if (this.knownSeqs.has(event.seq)) return
          this.knownSeqs.add(event.seq)
          this.patch({ events: [...this.state.events, event], connected: true })
        })
      } catch (error) {
        if (abort.signal.aborted) return
        this.patch({ connected: false, error: error instanceof Error ? error.message : String(error) })
      }
      if (abort.signal.aborted || generation !== this.generation) return
      await new Promise(resolve => setTimeout(resolve, 500))
    }
  }
}

function isUnauthorized(error: unknown): boolean {
  return error instanceof ShutuApiError && error.status === 401
}
