import type { EventView, SessionSummary, ShutuApi } from './api'

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
}

const EMPTY: WebState = {
  sessions: [], selectedId: null, events: [], hasOlder: false,
  loading: false, loadingOlder: false, sending: false, connected: false, error: null,
}

export class WebStore {
  private state: WebState = EMPTY
  private readonly listeners = new Set<() => void>()
  private readonly knownSeqs = new Set<number>()
  private streamAbort: AbortController | null = null
  private generation = 0

  constructor(private readonly api: ShutuApi) {}

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
    try {
      const sessions = await this.api.listSessions()
      this.patch({ sessions, error: null })
      const first = sessions[0]
      if (first !== undefined) await this.open(first.id)
    } catch (error) {
      this.patch({ error: error instanceof Error ? error.message : String(error) })
    }
  }

  async open(sessionId: string): Promise<void> {
    const generation = ++this.generation
    this.streamAbort?.abort()
    const abort = new AbortController()
    this.streamAbort = abort
    this.knownSeqs.clear()
    this.patch({ selectedId: sessionId, events: [], hasOlder: false, loading: true, loadingOlder: false, sending: false, connected: false, error: null })
    try {
      const page = await this.api.loadEvents(sessionId, { limit: 100 }, abort.signal)
      if (generation !== this.generation) return
      for (const event of page.events) this.knownSeqs.add(event.seq)
      this.patch({ events: page.events, hasOlder: page.has_more, loading: false })
      void this.streamLoop(sessionId, page.last_seq ?? page.events.at(-1)?.seq ?? 0, abort, generation)
    } catch (error) {
      if (abort.signal.aborted) return
      this.patch({ loading: false, error: error instanceof Error ? error.message : String(error) })
    }
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
    try { await this.api.sendMessage(id, text.trim()) } finally { this.patch({ sending: false }) }
  }

  async stop(): Promise<void> {
    const id = this.state.selectedId
    if (id === null) return
    await this.api.stop(id)
    this.patch({ sending: false })
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
