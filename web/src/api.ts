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

export interface ProviderModelView {
  id: string
  name?: string
  context_window?: number
  max_tokens?: number
}

export interface ProviderView {
  id: string
  name?: string
  custom?: boolean
  available?: boolean
  configured?: boolean
  model?: string
  base_url?: string
  candidates?: string[]
  models?: ProviderModelView[]
  protocol?: string
  protocol_label?: string
  reasoning?: Record<string, unknown> | null
}

export interface MCPServerView {
  name?: string
  cmd?: string
  args?: string[]
  connected?: boolean
  tool_count?: number
  enabled?: boolean
  [key: string]: unknown
}

export interface SettingsView {
  agent_preset?: string
  permission_preset?: string
  terminal_shell?: string
  language?: string
  mode_current?: string
  terminal_current?: boolean
  mode_options?: string[]
  permission_options?: string[]
  terminal_options?: string[]
  restart_required?: boolean
}

export interface SessionConfigView {
  agent_preset?: string
  provider?: string
  model?: string
  reasoning_effort?: string
  permission?: string
}

export interface ContextView {
  used_tokens: number
  context_window: number
  percent: number
}

export interface GoalView {
  id?: string
  ID?: string
  title?: string
  Title?: string
  objective?: string
  Objective?: string
  status?: string
  Status?: string
  plans?: string[]
  Plans?: string[]
  blockedReason?: string
  BlockedReason?: string
  maxRounds?: number
  MaxRounds?: number
  roundsStarted?: number
  RoundsStarted?: number
  revision?: number
  Revision?: number
}

export interface SessionStateView {
  session_id?: string
  plan_mode?: boolean
  plan_enabled?: boolean
  goals?: GoalView[]
  plans?: unknown[]
  memory_enabled?: boolean
  memories?: unknown[]
}

export interface SkillView {
  name: string
  description?: string
  when_to_use?: string
  enabled?: boolean
  kind?: string
  source?: string
  scope?: string
  rel?: string
  model_invocable?: boolean
  user_invocable?: boolean
}

export interface SkillGroupView {
  id: string
  name?: string
  scopes?: Record<string, string[]>
}

export interface SkillScopeView {
  id: string
  label?: string
}

export interface SkillsView {
  skills: SkillView[]
  groups: SkillGroupView[]
  scopes: SkillScopeView[]
  enabled?: boolean
}

export interface SkillFileView {
  path: string
  base64: string
}

export interface SkillActionValues {
  name?: string
  scope?: string
  from?: string
  to?: string
  mode?: string
  kind?: string
  enabled?: boolean
  files?: SkillFileView[]
  group_id?: string
  group_name?: string
  names?: string[]
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
  command?: string
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
  providers?: ProviderView[]
  mcp_servers?: MCPServerView[]
  commands?: CommandView[]
  [key: string]: unknown
}

export interface CommandView {
  name: string
  hint?: string
  kind?: 'command' | 'skill' | string
}

export interface QueueItem {
  id: string
  text: string
  created_at?: string
  placement?: string
}

export interface InteractionQuestionOption {
  label: string
  description?: string
}

export interface InteractionQuestion {
  id?: string
  question: string
  options?: InteractionQuestionOption[]
}

export interface InteractionView {
  id: string
  prompt: string
  tool_name?: string
  args?: string
  status: string
  created_at?: string
  questions?: InteractionQuestion[]
  resolved_at?: string
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
  getSettings(signal?: AbortSignal): Promise<SettingsView>
  updateSettings(values: Partial<Pick<SettingsView, 'agent_preset' | 'permission_preset' | 'terminal_shell' | 'language'>>, signal?: AbortSignal): Promise<{ ok: true; restart_required?: boolean }>
  switchModel(provider?: string, model?: string, reasoningEffort?: string, signal?: AbortSignal): Promise<{ ok: true }>
  saveProvider(provider: { id: string; name?: string; base_url?: string; model?: string; api_key?: string; protocol?: string; models?: ProviderModelView[]; custom?: boolean }, signal?: AbortSignal): Promise<void>
  deleteProvider(id: string, signal?: AbortSignal): Promise<void>
  discoverProvider(values: { provider: string; base_url?: string; protocol?: string; api_key?: string }, signal?: AbortSignal): Promise<ProviderModelView[]>
  manageMcp(action: 'add' | 'update' | 'delete', values: { original_name?: string; name?: string; cmd?: string; args?: string[] }, signal?: AbortSignal): Promise<MCPServerView[]>
  refreshMcp(signal?: AbortSignal): Promise<MCPServerView[]>
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
  getSessionConfig(sessionId: string, signal?: AbortSignal): Promise<SessionConfigView>
  updateSessionConfig(sessionId: string, values: Partial<SessionConfigView>, signal?: AbortSignal): Promise<SessionConfigView>
  getContext(sessionId: string, signal?: AbortSignal): Promise<ContextView>
  getSessionState(sessionId: string, signal?: AbortSignal): Promise<SessionStateView>
  listSkills(signal?: AbortSignal): Promise<SkillsView>
  skillAction(action: string, values?: SkillActionValues, signal?: AbortSignal): Promise<Record<string, unknown>>
  exportSession(sessionId: string, signal?: AbortSignal): Promise<Blob>
  forkSession(sessionId: string, signal?: AbortSignal): Promise<{ id: string }>
  listQueue(sessionId: string, signal?: AbortSignal): Promise<QueueItem[]>
  enqueueQueue(sessionId: string, text: string, signal?: AbortSignal): Promise<QueueItem>
  updateQueue(sessionId: string, itemId: string, action: 'move_first' | 'delete' | 'steer', signal?: AbortSignal): Promise<void>
  listInteractions(sessionId: string, signal?: AbortSignal): Promise<InteractionView[]>
  resolveInteraction(sessionId: string, interactionId: string, status: 'approved' | 'rejected' | 'canceled', answer?: string, signal?: AbortSignal): Promise<void>
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

  getSettings(signal?: AbortSignal): Promise<SettingsView> {
    return this.json<SettingsView>('/api/settings', { signal })
  }

  updateSettings(values: Partial<Pick<SettingsView, 'agent_preset' | 'permission_preset' | 'terminal_shell' | 'language'>>, signal?: AbortSignal): Promise<{ ok: true; restart_required?: boolean }> {
    return this.json<{ ok: true; restart_required?: boolean }>('/api/settings', {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(values),
    })
  }

  switchModel(provider = '', model = '', reasoningEffort = '', signal?: AbortSignal): Promise<{ ok: true }> {
    return this.json<{ ok: true }>('/api/config/model', {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ provider, model, reasoning_effort: reasoningEffort }),
    })
  }

  async saveProvider(provider: { id: string; name?: string; base_url?: string; model?: string; api_key?: string; protocol?: string; models?: ProviderModelView[]; custom?: boolean }, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>('/api/config/provider', { method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(provider) })
  }

  async deleteProvider(id: string, signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>('/api/config/provider', { method: 'DELETE', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) })
  }

  discoverProvider(values: { provider: string; base_url?: string; protocol?: string; api_key?: string }, signal?: AbortSignal): Promise<ProviderModelView[]> {
    return this.json<{ models: ProviderModelView[] }>('/api/config/provider/discover', { method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(values) }).then(result => result.models ?? [])
  }

  manageMcp(action: 'add' | 'update' | 'delete', values: { original_name?: string; name?: string; cmd?: string; args?: string[] }, signal?: AbortSignal): Promise<MCPServerView[]> {
    return this.json<{ servers: MCPServerView[] }>('/api/config/mcp', { method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action, ...values }) }).then(result => result.servers ?? [])
  }

  refreshMcp(signal?: AbortSignal): Promise<MCPServerView[]> {
    return this.json<{ servers: MCPServerView[] }>('/api/config/mcp/refresh', { method: 'POST', signal }).then(result => result.servers ?? [])
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

  getSessionConfig(sessionId: string, signal?: AbortSignal): Promise<SessionConfigView> {
    return this.json<SessionConfigView>(`/api/sessions/${encodeURIComponent(sessionId)}/config`, { signal })
  }

  updateSessionConfig(sessionId: string, values: Partial<SessionConfigView>, signal?: AbortSignal): Promise<SessionConfigView> {
    return this.json<SessionConfigView>(`/api/sessions/${encodeURIComponent(sessionId)}/config`, { method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(values) })
  }

  getContext(sessionId: string, signal?: AbortSignal): Promise<ContextView> {
    return this.json<ContextView>(`/api/sessions/${encodeURIComponent(sessionId)}/context`, { signal })
  }

  getSessionState(sessionId: string, signal?: AbortSignal): Promise<SessionStateView> {
    return this.json<SessionStateView>(`/api/sessions/${encodeURIComponent(sessionId)}/state`, { signal })
  }

  listSkills(signal?: AbortSignal): Promise<SkillsView> {
    return this.json<SkillsView>('/api/config/skills', { signal }).then(result => ({ skills: result.skills ?? [], groups: result.groups ?? [], scopes: result.scopes ?? [], enabled: result.enabled }))
  }

  skillAction(action: string, values: SkillActionValues = {}, signal?: AbortSignal): Promise<Record<string, unknown>> {
    return this.json<Record<string, unknown>>('/api/config/skills', {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action, ...values }),
    })
  }

  exportSession(sessionId: string, signal?: AbortSignal): Promise<Blob> {
    return this.blob(`/api/session.export?${new URLSearchParams({ sessionId, includeDescendants: 'true' })}`, { signal })
  }

  forkSession(sessionId: string, signal?: AbortSignal): Promise<{ id: string }> {
    return this.json<{ id: string }>(`/api/sessions/${encodeURIComponent(sessionId)}/fork`, { method: 'POST', signal })
  }

  listQueue(sessionId: string, signal?: AbortSignal): Promise<QueueItem[]> {
    return this.json<QueueItem[]>(`/api/sessions/${encodeURIComponent(sessionId)}/queue`, { signal })
  }

  async enqueueQueue(sessionId: string, text: string, signal?: AbortSignal): Promise<QueueItem> {
    return this.json<QueueItem>(`/api/sessions/${encodeURIComponent(sessionId)}/queue`, {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }),
    })
  }

  async updateQueue(sessionId: string, itemId: string, action: 'move_first' | 'delete' | 'steer', signal?: AbortSignal): Promise<void> {
    await this.json<{ ok: true }>(`/api/sessions/${encodeURIComponent(sessionId)}/queue/${encodeURIComponent(itemId)}`, {
      method: 'PATCH', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action }),
    })
  }

  listInteractions(sessionId: string, signal?: AbortSignal): Promise<InteractionView[]> {
    const query = new URLSearchParams({ session_id: sessionId })
    return this.json<{ interactions: InteractionView[] }>(`/api/interactions?${query}`, { signal }).then(result => result.interactions ?? [])
  }

  async resolveInteraction(sessionId: string, interactionId: string, status: 'approved' | 'rejected' | 'canceled', answer = '', signal?: AbortSignal): Promise<void> {
    const query = new URLSearchParams({ session_id: sessionId })
    await this.json<{ ok: true }>(`/api/interactions/${encodeURIComponent(interactionId)}/resolve?${query}`, {
      method: 'POST', signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ status, answer }),
    })
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
