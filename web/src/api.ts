export interface SessionStatus {
  state?: string
  statuses?: readonly { state: string; label: string }[]
}

export interface SessionSummary {
  id: string
  title?: string
  blank: boolean
  event_count: number
  updated_at: string
  workspace_id?: string
  archived?: boolean
  sort?: number
  flat_sort?: number
  status?: SessionStatus
}

export interface EventDetails {
  [key: string]: unknown
}

export interface EventView {
  seq: number
  type: string
  version: number
  time: string
  summary: string
  details?: EventDetails
  reasoning?: string
  tool_name?: string
  tool_output?: string
  tool_args?: string
  call_id?: string
  context_message?: boolean
  compaction_summary?: string
  compaction_tokens?: number
  compaction_error?: string
}

export interface EventPage {
  events: EventView[]
  has_more: boolean
  next_before_seq?: number
  next_after_seq?: number
  first_seq?: number
  last_seq?: number
}

export interface EventPageCursor {
  beforeSeq?: number
  afterSeq?: number
  limit?: number
}

export interface ConfigView {
  model?: string
  base_url?: string
  llm_provider?: string
  reasoning_effort?: string
  mode?: string
  web_server_addr?: string
  tools_enabled?: string[]
  tools_enabled_count?: number
  providers?: unknown[]
  mcp_servers?: unknown[]
  [key: string]: unknown
}

export interface SubagentView {
  id: string
  label?: string
  running: boolean
}

export interface JobView {
  id: string
  kind?: string
  label?: string
  status?: string
  detail?: string
  started_at?: string
  finished_at?: string
}

export interface RunningSnapshot {
  subagents: SubagentView[]
  jobs: JobView[]
}

export type EventListener = (event: EventView) => void

export interface WebApi {
  getConfig(signal?: AbortSignal): Promise<ConfigView>
  listSubagents(sessionId: string, signal?: AbortSignal): Promise<SubagentView[]>
  listJobs(sessionId: string, signal?: AbortSignal): Promise<JobView[]>
  listSessions(signal?: AbortSignal): Promise<SessionSummary[]>
  createSession(signal?: AbortSignal): Promise<{ id: string }>
  resumeSession(sessionId: string, signal?: AbortSignal): Promise<void>
  renameSession(sessionId: string, title: string, signal?: AbortSignal): Promise<{ title: string }>
  archiveSession(sessionId: string, signal?: AbortSignal): Promise<void>
  deleteSession(sessionId: string, signal?: AbortSignal): Promise<void>
  loadEvents(sessionId: string, cursor?: EventPageCursor, signal?: AbortSignal): Promise<EventPage>
  sendMessage(sessionId: string, text: string, signal?: AbortSignal): Promise<void>
  stop(sessionId: string, signal?: AbortSignal): Promise<void>
  stream(sessionId: string, lastSeq: number, signal: AbortSignal, onEvent: EventListener): Promise<void>
  setToken?(token: string): void
  getToken?(): string
}

export class ShutuApiError extends Error {
  constructor(message: string, readonly status: number) { super(message) }
}

export class ShutuApi implements WebApi {
  private tokenValue: string

  constructor(
    private readonly baseUrl = '',
    token = '',
    private readonly fetcher: typeof fetch = globalThis.fetch.bind(globalThis),
  ) { this.tokenValue = token }

  setToken(token: string): void { this.tokenValue = token.trim() }

  getToken(): string { return this.tokenValue }

  private url(path: string): URL {
    return new URL(path, this.baseUrl || window.location.origin)
  }

  private headers(): Headers {
    const headers = new Headers({ Accept: 'application/json' })
    if (this.tokenValue !== '') headers.set('Authorization', `Bearer ${this.tokenValue}`)
    return headers
  }

  private async json<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = this.headers()
    new Headers(init.headers).forEach((value, key) => headers.set(key, value))
    const response = await this.fetcher(this.url(path), { ...init, headers })
    if (!response.ok) throw new ShutuApiError(`Request failed: HTTP ${response.status}`, response.status)
    return response.json() as Promise<T>
  }

  listSessions(signal?: AbortSignal): Promise<SessionSummary[]> {
    return this.json<SessionSummary[]>('/api/sessions', { signal })
  }

  getConfig(signal?: AbortSignal): Promise<ConfigView> {
    return this.json<ConfigView>('/api/config', { signal })
  }

  listSubagents(sessionId: string, signal?: AbortSignal): Promise<SubagentView[]> {
    const query = new URLSearchParams({ session_id: sessionId })
    return this.json<{ subagents: SubagentView[] }>(`/api/subagents?${query}`, { signal }).then(result => result.subagents ?? [])
  }

  listJobs(sessionId: string, signal?: AbortSignal): Promise<JobView[]> {
    const query = new URLSearchParams({ session_id: sessionId })
    return this.json<{ jobs: JobView[] }>(`/api/jobs?${query}`, { signal }).then(result => result.jobs ?? [])
  }

  async createSession(signal?: AbortSignal): Promise<{ id: string }> {
    return this.json<{ id: string }>('/api/sessions', { method: 'POST', signal, body: '{}' })
  }

  async resumeSession(sessionId: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ id: string }>(`/api/sessions/${encodeURIComponent(sessionId)}/resume`, { method: 'POST', signal })
  }

  async renameSession(sessionId: string, title: string, signal?: AbortSignal): Promise<{ title: string }> {
    return this.json<{ title: string }>(`/api/sessions/${encodeURIComponent(sessionId)}/title`, {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title }),
    })
  }

  async archiveSession(sessionId: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/archive`, { method: 'POST', signal })
  }

  async deleteSession(sessionId: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}`, { method: 'DELETE', signal })
  }

  loadEvents(sessionId: string, cursor: EventPageCursor = {}, signal?: AbortSignal): Promise<EventPage> {
    const query = new URLSearchParams({ limit: String(cursor.limit ?? 100) })
    if (cursor.beforeSeq !== undefined) query.set('before_seq', String(cursor.beforeSeq))
    if (cursor.afterSeq !== undefined) query.set('after_seq', String(cursor.afterSeq))
    return this.json<EventPage>(`/api/sessions/${encodeURIComponent(sessionId)}/events?${query}`, { signal })
  }

  async sendMessage(sessionId: string, text: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/message`, {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }),
    })
  }

  async stop(sessionId: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/stop`, {
      method: 'POST', signal,
    })
  }

  async stream(sessionId: string, lastSeq: number, signal: AbortSignal, onEvent: EventListener): Promise<void> {
    const headers = this.headers()
    headers.set('Accept', 'text/event-stream')
    if (lastSeq > 0) headers.set('Last-Event-ID', String(lastSeq))
    const response = await this.fetcher(this.url(`/api/sessions/${encodeURIComponent(sessionId)}/events/stream`), { signal, headers })
    if (!response.ok || response.body === null) throw new ShutuApiError(`Stream failed: HTTP ${response.status}`, response.status)

    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    try {
      while (true) {
        const next = await reader.read()
        if (next.done) return
        buffer += decoder.decode(next.value, { stream: true })
        let boundary = buffer.indexOf('\n\n')
        while (boundary >= 0) {
          const frame = buffer.slice(0, boundary)
          buffer = buffer.slice(boundary + 2)
          const data = frame.split('\n')
            .filter(line => line.startsWith('data: '))
            .map(line => line.slice(6))
            .join('')
          if (data !== '') onEvent(JSON.parse(data) as EventView)
          boundary = buffer.indexOf('\n\n')
        }
      }
    } finally {
      await reader.cancel().catch(() => undefined)
    }
  }
}
