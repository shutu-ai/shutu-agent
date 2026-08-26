import type { AttachmentView, ConfigView, ContextView, EventView, FeedbackView, FilePreview, InteractionView, MCPServerView, ProviderModelView, QueueItem, RunningSnapshot, SessionConfigView, SessionFilesView, SessionSearchHit, SessionStateView, SessionSummary, SettingsView, SkillActionValues, SkillsView, WebApi, WorkspaceList } from './api'
import { ShutuApiError } from './api'

export interface WebState {
  sessions: readonly SessionSummary[]
  workspaces: WorkspaceList
  selectedId: string | null
  events: readonly EventView[]
  hasOlder: boolean
  historyStartSeq: number | null
  historyEndSeq: number | null
  historyCursor: { beforeSeq?: number; afterSeq?: number } | null
  historyError: string | null
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
  sessions: [], workspaces: { workspaces: [], ungrouped_ids: [] }, selectedId: null, events: [], hasOlder: false, historyStartSeq: null, historyEndSeq: null, historyCursor: null, historyError: null,
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
      const [rawSessions, workspaces] = await Promise.all([this.api.listSessions(), this.api.listWorkspaces()])
      const sessions = sortSessions(rawSessions)
      this.patch({ sessions, workspaces, error: null, authRequired: false })
      const saved = savedSessionId()
      const current = sessions.find(session => session.id === saved) ?? sessions[0]
      if (current !== undefined) await this.open(current.id)
      else {
        saveSessionId(null)
        this.patch({ selectedId: null, events: [], hasOlder: false, historyStartSeq: null, historyEndSeq: null, historyCursor: null, historyError: null, loading: false })
      }
      if (this.pollTimer === null) {
        this.pollTimer = setInterval(() => { void this.refreshSessions().catch(error => this.reportBackgroundError(error)) }, 30_000)
        const timer = this.pollTimer as unknown as { unref?: () => void }
        timer.unref?.()
      }
    } catch (error) {
      this.patch({ loading: false, error: error instanceof Error ? error.message : String(error), authRequired: isUnauthorized(error) })
    }
  }

  async refreshSessions(): Promise<readonly SessionSummary[]> {
    const [rawSessions, workspaces] = await Promise.all([this.api.listSessions(), this.api.listWorkspaces()])
    const sessions = sortSessions(rawSessions)
    this.patch({ sessions, workspaces })
    return sessions
  }

  private reportBackgroundError(error: unknown): void {
    if (this.state.selectedId === null) return
    this.patch({ error: error instanceof Error ? error.message : String(error), authRequired: isUnauthorized(error) })
  }

  async createSession(workspaceId = ''): Promise<void> {
    const result = await this.api.createSession(workspaceId)
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
    this.patch({ selectedId: sessionId, events: [], hasOlder: false, historyStartSeq: null, historyEndSeq: null, historyCursor: null, historyError: null, loading: true, loadingOlder: false, sending: false, connected: false, error: null, authRequired: false })
    try {
      await this.api.resumeSession(sessionId, abort.signal)
      const page = await this.api.loadEvents(sessionId, { limit: 100 }, abort.signal)
      if (generation !== this.generation) return
      for (const event of page.events) this.knownSeqs.add(event.seq)
      this.patch({
        events: page.events,
        hasOlder: page.has_more,
        historyStartSeq: page.first_seq ?? page.events[0]?.seq ?? null,
        historyEndSeq: page.last_seq ?? page.events.at(-1)?.seq ?? null,
        historyCursor: page.next_before_seq === undefined && page.next_after_seq === undefined
          ? null
          : { ...(page.next_before_seq === undefined ? {} : { beforeSeq: page.next_before_seq }), ...(page.next_after_seq === undefined ? {} : { afterSeq: page.next_after_seq }) },
        loading: false,
      })
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
      this.patch({ selectedId: null, events: [], hasOlder: false, historyStartSeq: null, historyEndSeq: null, historyCursor: null, historyError: null, loading: false, connected: false })
      return
    }
    await this.open(next.id)
  }

  async loadOlder(): Promise<void> {
    const { selectedId, events, hasOlder, loadingOlder } = this.state
    if (selectedId === null || !hasOlder || loadingOlder || events.length === 0) return
    const generation = this.generation
    const sessionId = selectedId
    this.patch({ loadingOlder: true, historyError: null })
    try {
      const page = await this.api.loadEvents(sessionId, { beforeSeq: events[0].seq, limit: 100 })
      if (generation !== this.generation || sessionId !== this.state.selectedId) return
      const known = new Set(this.state.events.map(event => event.seq))
      const older = page.events.filter(event => !known.has(event.seq))
      this.patch({
        events: [...older, ...this.state.events],
        hasOlder: page.has_more,
        historyStartSeq: page.first_seq ?? older[0]?.seq ?? this.state.historyStartSeq,
        historyEndSeq: page.last_seq ?? this.state.historyEndSeq,
        historyCursor: page.next_before_seq === undefined && page.next_after_seq === undefined
          ? null
          : { ...(page.next_before_seq === undefined ? {} : { beforeSeq: page.next_before_seq }), ...(page.next_after_seq === undefined ? {} : { afterSeq: page.next_after_seq }) },
        historyError: null,
        loadingOlder: false,
      })
    } catch (error) {
      if (generation !== this.generation || sessionId !== this.state.selectedId) return
      this.patch({ loadingOlder: false, historyError: error instanceof Error ? error.message : String(error) })
    }
  }

  async send(text: string, images: string[] = []): Promise<void> {
    const id = this.state.selectedId
    if (id === null || (text.trim() === '' && images.length === 0)) return
    this.patch({ sending: true })
    try {
      await this.api.sendMessage(id, text.trim(), images)
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

  getSettings(signal?: AbortSignal): Promise<SettingsView> {
    return this.api.getSettings(signal)
  }

  updateSettings(values: Partial<Pick<SettingsView, 'agent_preset' | 'permission_preset' | 'terminal_shell' | 'language'>>): Promise<{ ok: true; restart_required?: boolean }> {
    return this.api.updateSettings(values)
  }

  async switchModel(provider = '', model = '', reasoningEffort = ''): Promise<void> {
    await this.api.switchModel(provider, model, reasoningEffort)
  }

  async saveProvider(provider: { id: string; name?: string; base_url?: string; model?: string; api_key?: string; protocol?: string; models?: ProviderModelView[]; custom?: boolean }): Promise<void> {
    await this.api.saveProvider(provider)
  }

  async deleteProvider(id: string): Promise<void> {
    await this.api.deleteProvider(id)
  }

  discoverProvider(values: { provider: string; base_url?: string; protocol?: string; api_key?: string }, signal?: AbortSignal): Promise<ProviderModelView[]> {
    return this.api.discoverProvider(values, signal)
  }

  manageMcp(action: 'add' | 'update' | 'delete', values: { original_name?: string; name?: string; cmd?: string; args?: string[] }): Promise<MCPServerView[]> {
    return this.api.manageMcp(action, values)
  }

  refreshMcp(signal?: AbortSignal): Promise<MCPServerView[]> {
    return this.api.refreshMcp(signal)
  }

  listFeedback(sessionId: string, signal?: AbortSignal): Promise<FeedbackView[]> {
    return this.api.listFeedback(sessionId, signal)
  }

  putFeedback(sessionId: string, seq: number, rating: 'positive' | 'negative', note = ''): Promise<FeedbackView> {
    return this.api.putFeedback(sessionId, seq, rating, note)
  }

  async deleteFeedback(sessionId: string, seq: number): Promise<void> {
    await this.api.deleteFeedback(sessionId, seq)
  }

  uploadAttachment(sessionId: string, file: File): Promise<AttachmentView> {
    return this.api.uploadAttachment(sessionId, file)
  }

  loadAttachment(sessionId: string, attachmentId: string, signal?: AbortSignal): Promise<Blob> {
    return this.api.loadAttachment(sessionId, attachmentId, signal)
  }

  getWorkspaces(signal?: AbortSignal): Promise<WorkspaceList> {
    return this.api.listWorkspaces(signal)
  }

  async createWorkspace(title: string, path = ''): Promise<void> {
    await this.api.createWorkspace(title, path)
    await this.refreshSessions()
  }

  async renameWorkspace(workspaceId: string, title: string): Promise<void> {
    await this.api.renameWorkspace(workspaceId, title)
    await this.refreshSessions()
  }

  async deleteWorkspace(workspaceId: string): Promise<void> {
    await this.api.deleteWorkspace(workspaceId)
    await this.refreshSessions()
  }

  pickWorkspaceDirectory(signal?: AbortSignal): Promise<{ path: string }> {
    return this.api.pickWorkspaceDirectory(signal)
  }

  listWorkspaceDirectories(path = '', signal?: AbortSignal) {
    return this.api.listWorkspaceDirectories(path, signal)
  }

  createWorkspaceDirectory(path: string, name: string): Promise<{ path: string }> {
    return this.api.createWorkspaceDirectory(path, name)
  }

  async reorderWorkspaces(ids: string[]): Promise<void> {
    await this.api.reorderWorkspaces(ids)
    await this.refreshSessions()
  }

  async reorderSessions(workspaceId: string, sessionIds: string[]): Promise<void> {
    await this.api.reorderSessions(workspaceId, sessionIds)
    await this.refreshSessions()
  }

  searchSessions(query: string, signal?: AbortSignal): Promise<SessionSearchHit[]> {
    return this.api.searchSessions(query, signal)
  }

  listFiles(sessionId: string, path = '', query = '', signal?: AbortSignal): Promise<SessionFilesView> {
    return this.api.listFiles(sessionId, path, query, signal)
  }

  previewFile(sessionId: string, path: string, start?: number, end?: number, signal?: AbortSignal): Promise<FilePreview> {
    return this.api.previewFile(sessionId, path, start, end, signal)
  }

  getSessionConfig(sessionId: string, signal?: AbortSignal): Promise<SessionConfigView> {
    return this.api.getSessionConfig(sessionId, signal)
  }

  updateSessionConfig(sessionId: string, values: Partial<SessionConfigView>): Promise<SessionConfigView> {
    return this.api.updateSessionConfig(sessionId, values)
  }

  getContext(sessionId: string, signal?: AbortSignal): Promise<ContextView> {
    return this.api.getContext(sessionId, signal)
  }

  getSessionState(sessionId: string, signal?: AbortSignal): Promise<SessionStateView> {
    return this.api.getSessionState(sessionId, signal)
  }

  listSkills(signal?: AbortSignal): Promise<SkillsView> {
    return this.api.listSkills(signal)
  }

  skillAction(action: string, values?: SkillActionValues, signal?: AbortSignal): Promise<Record<string, unknown>> {
    return this.api.skillAction(action, values, signal)
  }

  exportSession(sessionId: string, signal?: AbortSignal): Promise<Blob> {
    return this.api.exportSession(sessionId, signal)
  }

  async forkSession(sessionId: string): Promise<void> {
    const result = await this.api.forkSession(sessionId)
    await this.refreshSessions()
    await this.open(result.id)
  }

  listQueue(sessionId: string, signal?: AbortSignal): Promise<QueueItem[]> {
    return this.api.listQueue(sessionId, signal)
  }

  enqueueQueue(sessionId: string, text: string): Promise<QueueItem> {
    return this.api.enqueueQueue(sessionId, text)
  }

  updateQueue(sessionId: string, itemId: string, action: 'move_first' | 'delete' | 'steer'): Promise<void> {
    return this.api.updateQueue(sessionId, itemId, action)
  }

  listInteractions(sessionId: string, signal?: AbortSignal): Promise<InteractionView[]> {
    return this.api.listInteractions(sessionId, signal)
  }

  resolveInteraction(sessionId: string, interactionId: string, status: 'approved' | 'rejected' | 'canceled', answer = ''): Promise<void> {
    return this.api.resolveInteraction(sessionId, interactionId, status, answer)
  }

  async loadRunning(sessionId: string, signal?: AbortSignal): Promise<RunningSnapshot> {
    const [subagents, jobs] = await Promise.all([
      this.api.listSubagents(sessionId, signal),
      this.api.listJobs(sessionId, signal),
    ])
    return { subagents, jobs }
  }

  async authenticate(token: string): Promise<void> {
    this.api.setToken?.(token)
    this.patch({ authRequired: false, error: null })
    await this.start()
  }

  private async streamLoop(sessionId: string, initialSeq: number, abort: AbortController, generation: number): Promise<void> {
    let cursor = initialSeq
    let retryDelay = 500
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
        retryDelay = 500
      } catch (error) {
        if (abort.signal.aborted) return
        this.patch({ connected: false, error: error instanceof Error ? error.message : String(error), authRequired: isUnauthorized(error) })
      }
      if (abort.signal.aborted || generation !== this.generation) return
      await new Promise(resolve => setTimeout(resolve, retryDelay))
      retryDelay = Math.min(8_000, retryDelay * 2)
    }
  }
}

function isUnauthorized(error: unknown): boolean {
  return error instanceof ShutuApiError && (error.status === 401 || error.status === 403)
}
