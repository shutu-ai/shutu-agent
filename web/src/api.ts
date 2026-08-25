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

export interface WorkspaceView {
  id: string
  title: string
  path?: string
  session_ids: string[]
  created_at: number
}

export interface WorkspaceList {
  workspaces: WorkspaceView[]
  ungrouped_ids: string[]
}

export interface DirectoryEntry {
  name: string
  path: string
  hidden?: boolean
}

export interface DirectoryListing {
  path: string
  home: string
  crumbs: DirectoryEntry[]
  entries: DirectoryEntry[]
  read_error?: string
  truncated?: boolean
}

export interface SessionFileView {
  name: string
  path: string
  dir: boolean
  size?: number
  mod_time?: string
}

export interface SessionFilesView {
  root: string
  path: string
  entries: SessionFileView[]
}

export interface FilePreview {
  path: string
  content: string
  start_line: number
  end_line: number
  total_lines: number
}

export interface SessionSearchHit {
  id: string
  title?: string
  updated_at: string
  snippet: string
}

export interface EventDetails {
  [key: string]: unknown
}

export interface ImageView {
  id: string
  media_type: string
  bytes?: number
  width?: number
  height?: number
}

export interface AttachmentView extends ImageView {
  bytes: number
}

export interface FeedbackView {
  session_id: string
  seq: number
  rating: 'positive' | 'negative'
  note?: string
  created_at?: string
  updated_at?: string
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
  images?: ImageView[]
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
  listFeedback(sessionId: string, signal?: AbortSignal): Promise<FeedbackView[]>
  putFeedback(sessionId: string, seq: number, rating: 'positive' | 'negative', note?: string, signal?: AbortSignal): Promise<FeedbackView>
  deleteFeedback(sessionId: string, seq: number, signal?: AbortSignal): Promise<void>
  uploadAttachment(sessionId: string, file: File, signal?: AbortSignal): Promise<AttachmentView>
  loadAttachment(sessionId: string, attachmentId: string, signal?: AbortSignal): Promise<Blob>
  listWorkspaces(signal?: AbortSignal): Promise<WorkspaceList>
  createWorkspace(title: string, path?: string, signal?: AbortSignal): Promise<{ id: string; title: string; path: string }>
  pickWorkspaceDirectory(signal?: AbortSignal): Promise<{ path: string }>
  listWorkspaceDirectories(path?: string, signal?: AbortSignal): Promise<DirectoryListing>
  createWorkspaceDirectory(path: string, name: string, signal?: AbortSignal): Promise<{ path: string }>
  renameWorkspace(workspaceId: string, title: string, signal?: AbortSignal): Promise<void>
  deleteWorkspace(workspaceId: string, signal?: AbortSignal): Promise<void>
  reorderWorkspaces(ids: string[], signal?: AbortSignal): Promise<void>
  reorderSessions(workspaceId: string, sessionIds: string[], signal?: AbortSignal): Promise<void>
  searchSessions(query: string, signal?: AbortSignal): Promise<SessionSearchHit[]>
  listFiles(sessionId: string, path?: string, query?: string, signal?: AbortSignal): Promise<SessionFilesView>
  previewFile(sessionId: string, path: string, start?: number, end?: number, signal?: AbortSignal): Promise<FilePreview>
  listSessions(signal?: AbortSignal): Promise<SessionSummary[]>
  createSession(workspaceId?: string, signal?: AbortSignal): Promise<{ id: string; workspace_id?: string }>
  resumeSession(sessionId: string, signal?: AbortSignal): Promise<void>
  renameSession(sessionId: string, title: string, signal?: AbortSignal): Promise<{ title: string }>
  archiveSession(sessionId: string, signal?: AbortSignal): Promise<void>
  deleteSession(sessionId: string, signal?: AbortSignal): Promise<void>
  loadEvents(sessionId: string, cursor?: EventPageCursor, signal?: AbortSignal): Promise<EventPage>
  sendMessage(sessionId: string, text: string, images?: string[], signal?: AbortSignal): Promise<void>
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

  private async blob(path: string, init: RequestInit = {}): Promise<Blob> {
    const headers = this.headers()
    new Headers(init.headers).forEach((value, key) => headers.set(key, value))
    const response = await this.fetcher(this.url(path), { ...init, headers })
    if (!response.ok) throw new ShutuApiError(`Request failed: HTTP ${response.status}`, response.status)
    return response.blob()
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

  listFeedback(sessionId: string, signal?: AbortSignal): Promise<FeedbackView[]> {
    return this.json<FeedbackView[]>(`/api/sessions/${encodeURIComponent(sessionId)}/feedback`, { signal })
  }

  listWorkspaces(signal?: AbortSignal): Promise<WorkspaceList> {
    return this.json<WorkspaceList>('/api/workspaces', { signal })
  }

  async createWorkspace(title: string, path = '', signal?: AbortSignal): Promise<{ id: string; title: string; path: string }> {
    return this.json<{ id: string; title: string; path: string }>('/api/workspaces', {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title, path }),
    })
  }

  pickWorkspaceDirectory(signal?: AbortSignal): Promise<{ path: string }> {
    return this.json<{ path: string }>('/api/workspaces/pick-directory', { method: 'POST', signal })
  }

  listWorkspaceDirectories(path = '', signal?: AbortSignal): Promise<DirectoryListing> {
    const query = path === '' ? '' : `?${new URLSearchParams({ path })}`
    return this.json<DirectoryListing>(`/api/workspaces/directories${query}`, { signal })
  }

  async createWorkspaceDirectory(path: string, name: string, signal?: AbortSignal): Promise<{ path: string }> {
    return this.json<{ path: string }>('/api/workspaces/directories', {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path, name }),
    })
  }

  async renameWorkspace(workspaceId: string, title: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/workspaces/${encodeURIComponent(workspaceId)}`, {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title }),
    })
  }

  async deleteWorkspace(workspaceId: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/workspaces/${encodeURIComponent(workspaceId)}`, { method: 'DELETE', signal })
  }

  async reorderWorkspaces(ids: string[], signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>('/api/workspaces/order', {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids }),
    })
  }

  async reorderSessions(workspaceId: string, sessionIds: string[], signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>('/api/sessions/order', {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ workspace_id: workspaceId, session_ids: sessionIds }),
    })
  }

  searchSessions(query: string, signal?: AbortSignal): Promise<SessionSearchHit[]> {
    const params = new URLSearchParams({ q: query })
    return this.json<{ hits: SessionSearchHit[] }>(`/api/search?${params}`, { signal }).then(result => result.hits ?? [])
  }

  listFiles(sessionId: string, path = '', query = '', signal?: AbortSignal): Promise<SessionFilesView> {
    const params = new URLSearchParams()
    if (path !== '') params.set('path', path)
    if (query !== '') params.set('q', query)
    const suffix = params.size > 0 ? `?${params}` : ''
    return this.json<SessionFilesView>(`/api/sessions/${encodeURIComponent(sessionId)}/files${suffix}`, { signal })
  }

  previewFile(sessionId: string, path: string, start?: number, end?: number, signal?: AbortSignal): Promise<FilePreview> {
    const params = new URLSearchParams({ path })
    if (start !== undefined) params.set('start', String(start))
    if (end !== undefined) params.set('end', String(end))
    return this.json<FilePreview>(`/api/sessions/${encodeURIComponent(sessionId)}/file?${params}`, { signal })
  }

  putFeedback(sessionId: string, seq: number, rating: 'positive' | 'negative', note = '', signal?: AbortSignal): Promise<FeedbackView> {
    return this.json<FeedbackView>(`/api/sessions/${encodeURIComponent(sessionId)}/feedback/${seq}`, {
      method: 'PUT', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ rating, note }),
    })
  }

  async deleteFeedback(sessionId: string, seq: number, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/feedback/${seq}`, { method: 'DELETE', signal })
  }

  async uploadAttachment(sessionId: string, file: File, signal?: AbortSignal): Promise<AttachmentView> {
    const body = new FormData()
    body.append('file', file, file.name)
    return this.json<AttachmentView>(`/api/sessions/${encodeURIComponent(sessionId)}/attachments`, { method: 'POST', signal, body })
  }

  loadAttachment(sessionId: string, attachmentId: string, signal?: AbortSignal): Promise<Blob> {
    return this.blob(`/api/sessions/${encodeURIComponent(sessionId)}/attachments/${encodeURIComponent(attachmentId)}`, { signal })
  }

  async createSession(workspaceId = '', signal?: AbortSignal): Promise<{ id: string; workspace_id?: string }> {
    return this.json<{ id: string; workspace_id?: string }>('/api/sessions', {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(workspaceId === '' ? {} : { workspace_id: workspaceId }),
    })
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

  async sendMessage(sessionId: string, text: string, images: string[] = [], signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/message`, {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text, images }),
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
