import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type KeyboardEvent } from 'react'
import type { AttachmentView, CommandView, ConfigView, ContextView, DirectoryListing, EventDetails, EventView, FeedbackView, GoalView, ImageView, InteractionView, JobView, MCPServerView, PlanView, ProviderModelView, ProviderView, QueueItem, RunningSnapshot, SessionSearchHit, SessionStateView, SessionSummary, SettingsView, SkillView, SkillsView, SubagentView, TodoView, WorkspaceView } from './api'
import { ShutuApiError } from './api'
import { projectDshConversation, type DshConversationNode, type DshConversationSnapshot } from './dsh-conversation'
import { collapseDshTrajectoryTurns, projectDshTrajectory, type DshTimelineMode } from './dsh-trajectory'
import { deriveProducedFiles } from './produced-files'
import { WebStore } from './store'
import { buildVirtualOffsets, virtualRange } from './virtual-list'
import './styles.css'

const ROW_HEIGHT_ESTIMATE = 132
const CONVERSATION_ROW_HEIGHT_ESTIMATE = 164

function useMeasuredVirtualRows(keys: readonly string[], estimate: number, scrollRef: { current: HTMLDivElement | null }) {
  const heights = useRef(new Map<string, number>())
  const elements = useRef(new Map<string, HTMLElement>())
  const observer = useRef<ResizeObserver | null>(null)
  const previousLayout = useRef<{ keys: readonly string[]; offsets: readonly number[] } | null>(null)
  const [revision, setRevision] = useState(0)

  useEffect(() => {
    if (typeof ResizeObserver === 'undefined') return
    const nextObserver = new ResizeObserver(entries => {
      let changed = false
      for (const entry of entries) {
        const key = entry.target.getAttribute('data-virtual-row-key')
        if (key === null) continue
        const height = Math.max(1, Math.ceil(entry.target.getBoundingClientRect().height))
        if (heights.current.get(key) !== height) {
          heights.current.set(key, height)
          changed = true
        }
      }
      if (changed) setRevision(value => value + 1)
    })
    observer.current = nextObserver
    elements.current.forEach(element => nextObserver.observe(element))
    return () => {
      nextObserver.disconnect()
      observer.current = null
    }
  }, [])

  const measureRow = useCallback((key: string, element: HTMLElement | null) => {
    const previous = elements.current.get(key)
    if (previous !== undefined && previous !== element) observer.current?.unobserve(previous)
    if (element === null) {
      elements.current.delete(key)
      return
    }
    elements.current.set(key, element)
    observer.current?.observe(element)
  }, [])

  const offsets = useMemo(() => buildVirtualOffsets(keys, heights.current, estimate), [estimate, keys, revision])

  useLayoutEffect(() => {
    const previous = previousLayout.current
    const host = scrollRef.current
    if (previous !== null && host !== null && previous.keys.length > 0) {
      const previousIndex = Math.min(previous.keys.length - 1, virtualRange(previous.offsets, host.scrollTop, 1, 0).start)
      const anchorKey = previous.keys[previousIndex]
      const nextIndex = anchorKey === undefined ? -1 : keys.indexOf(anchorKey)
      if (nextIndex >= 0) {
        const delta = (offsets[nextIndex] ?? 0) - (previous.offsets[previousIndex] ?? 0)
        if (delta !== 0) host.scrollTop = Math.max(0, host.scrollTop + delta)
      }
    }
    previousLayout.current = { keys, offsets }
  }, [keys, offsets, scrollRef])

  return { offsets, measureRow }
}

function eventLabel(event: EventView): string {
  if (event.tool_name) return event.tool_name
  if (event.type === 'user/message') return 'User'
  if (event.type === 'assistant/message') return 'Assistant'
  if (event.type === 'assistant/reasoning') return 'Reasoning'
  if (event.type.startsWith('llm/')) return 'LLM request'
  if (event.type.startsWith('tool/')) return 'Tool'
  return event.type
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

function relativeTime(value: string, now = Date.now()): string {
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return ''
  const seconds = Math.max(0, Math.floor((now - timestamp) / 1000))
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  if (seconds < 2592000) return `${Math.floor(seconds / 86400)}d`
  if (seconds < 31536000) return `${Math.floor(seconds / 2592000)}mo`
  return `${Math.floor(seconds / 31536000)}y`
}

function sessionTitle(session: SessionSummary): string {
  return session.blank ? 'New session' : session.title || session.id
}

function sessionStatus(session: SessionSummary): { tone: 'running' | 'done' | 'idle'; label: string } {
  const state = session.status?.state?.toLowerCase()
  if (state === 'ongoing' || state === 'running') return { tone: 'running', label: 'Running' }
  if (state === 'done' || state === 'completed') return { tone: 'done', label: 'Completed' }
  return { tone: 'idle', label: 'Idle' }
}

type SidebarDialog =
  | { kind: 'rename'; session: SessionSummary }
  | { kind: 'archive' | 'delete'; session: SessionSummary }

type WorkspaceDialog =
  | { kind: 'create' }
  | { kind: 'rename' | 'delete'; workspace: WorkspaceView }

function WorkspaceDialog({ store, dialog, onClose, onError }: {
  store: WebStore
  dialog: WorkspaceDialog
  onClose: () => void
  onError: (error: unknown) => void
}) {
  const initial = dialog.kind === 'create' ? undefined : dialog.workspace
  const [title, setTitle] = useState(initial?.title ?? '')
  const [path, setPath] = useState(initial?.path ?? '')
  const [browser, setBrowser] = useState<DirectoryListing | null>(null)
  const [browserLoading, setBrowserLoading] = useState(false)
  const [folderName, setFolderName] = useState('')
  const [working, setWorking] = useState(false)

  const loadDirectory = async (nextPath = ''): Promise<void> => {
    setBrowserLoading(true)
    try { setBrowser(await store.listWorkspaceDirectories(nextPath)) }
    catch (error) { onError(error) }
    finally { setBrowserLoading(false) }
  }

  const chooseNativeDirectory = async (): Promise<void> => {
    try {
      const result = await store.pickWorkspaceDirectory()
      if (result.path !== '') setPath(result.path)
    } catch (error) { onError(error) }
  }

  const createFolder = async (): Promise<void> => {
    if (browser === null || folderName.trim() === '') return
    setWorking(true)
    try {
      const result = await store.createWorkspaceDirectory(browser.path, folderName.trim())
      setFolderName('')
      await loadDirectory(result.path)
    } catch (error) { onError(error) }
    finally { setWorking(false) }
  }

  const submit = async (): Promise<void> => {
    setWorking(true)
    try {
      if (dialog.kind === 'create') await store.createWorkspace(title.trim(), path.trim())
      else if (dialog.kind === 'rename') await store.renameWorkspace(dialog.workspace.id, title.trim())
      else await store.deleteWorkspace(dialog.workspace.id)
      onClose()
    } catch (error) { onError(error) }
    finally { setWorking(false) }
  }

  const isDelete = dialog.kind === 'delete'
  return <div className="sidebar-dialog-backdrop" role="presentation" onMouseDown={() => !working && onClose()}>
    <form className="sidebar-dialog workspace-dialog" role="dialog" aria-modal="true" aria-labelledby="workspace-dialog-title" onSubmit={event => { event.preventDefault(); void submit() }} onMouseDown={event => event.stopPropagation()}>
      <h2 id="workspace-dialog-title">{dialog.kind === 'create' ? 'New workspace' : dialog.kind === 'rename' ? 'Rename workspace' : 'Delete workspace?'}</h2>
      {isDelete ? <p>{`Delete ${dialog.workspace.title}? Sessions will return to the ungrouped list.`}</p> : <>
        <label>Workspace name<input autoFocus value={title} onChange={event => setTitle(event.target.value)} maxLength={60} aria-label="Workspace name" /></label>
        {dialog.kind === 'create' && <label>Directory<input value={path} onChange={event => setPath(event.target.value)} placeholder="Optional absolute path" aria-label="Workspace directory" /><span className="workspace-dialog-actions"><button type="button" onClick={() => void chooseNativeDirectory()} disabled={working}>Choose directory</button><button type="button" onClick={() => { setBrowser(value => value === null ? { path: '', home: '', crumbs: [], entries: [] } : null); if (browser === null) void loadDirectory() }} disabled={working}>Browse</button></span></label>}
        {dialog.kind === 'create' && browser !== null && <div className="directory-browser" aria-label="Directory browser">
          <div className="directory-crumbs">{browser.crumbs.map(crumb => <button type="button" key={crumb.path} onClick={() => void loadDirectory(crumb.path)}>{crumb.name}</button>)}</div>
          {browserLoading ? <span className="muted">Loading directories…</span> : browser.read_error ? <span className="muted">{browser.read_error}</span> : <div className="directory-list">{browser.entries.filter(entry => !entry.hidden).map(entry => <button type="button" key={entry.path} onClick={() => void loadDirectory(entry.path)}>📁 {entry.name}</button>)}</div>}
          <div className="directory-new-folder"><input value={folderName} onChange={event => setFolderName(event.target.value)} placeholder="New folder name" aria-label="New folder name" /><button type="button" onClick={() => void createFolder()} disabled={working || folderName.trim() === ''}>Create</button></div>
        </div>}
      </>}
      <div className="dialog-actions"><button type="button" onClick={onClose} disabled={working}>Cancel</button><button type="submit" className={isDelete ? 'danger-button' : ''} disabled={working || (!isDelete && title.trim() === '')}>{isDelete ? 'Delete' : 'Save'}</button></div>
    </form>
  </div>
}

function SessionBrowser({
  sessions,
  workspaces,
  selectedId,
  store,
  onError,
  onSettings,
}: {
  sessions: readonly SessionSummary[]
  workspaces: { workspaces: WorkspaceView[]; ungrouped_ids: string[] }
  selectedId: string | null
  store: WebStore
  onError: (error: unknown) => void
  onSettings: () => void
}) {
  const [query, setQuery] = useState('')
  const [searchOpen, setSearchOpen] = useState(false)
  const [expanded, setExpanded] = useState(false)
  const [collapsed, setCollapsed] = useState(() => {
    if (typeof localStorage === 'undefined') return false
    try { return localStorage.getItem('shutu.web.sidebar-collapsed') === 'true' } catch { return false }
  })
  const [menuId, setMenuId] = useState<string | null>(null)
  const [dialog, setDialog] = useState<SidebarDialog | null>(null)
  const [draftTitle, setDraftTitle] = useState('')
  const [working, setWorking] = useState(false)
  const [workspaceDialog, setWorkspaceDialog] = useState<WorkspaceDialog | null>(null)
  const [workspaceMenuId, setWorkspaceMenuId] = useState<string | null>(null)
  const [remoteHits, setRemoteHits] = useState<SessionSearchHit[]>([])
  const [remoteLoading, setRemoteLoading] = useState(false)
  const [collapsedWorkspaces, setCollapsedWorkspaces] = useState<Record<string, boolean>>({})
  const [draggedWorkspaceId, setDraggedWorkspaceId] = useState<string | null>(null)
  const [draggedSessionId, setDraggedSessionId] = useState<string | null>(null)
  const filtered = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase()
    if (!needle) return sessions
    return sessions.filter(session => `${sessionTitle(session)} ${session.id}`.toLocaleLowerCase().includes(needle))
  }, [query, sessions])
  const visible = expanded || query.trim() !== '' ? filtered : filtered.slice(0, 5)
  const overflow = Math.max(0, filtered.length - 5)

  useEffect(() => {
    const needle = query.trim()
    if (needle === '') { setRemoteHits([]); return }
    const abort = new AbortController()
    setRemoteLoading(true)
    void store.searchSessions(needle, abort.signal).then(hits => {
      if (!abort.signal.aborted) setRemoteHits(hits)
    }).catch(() => {
      if (!abort.signal.aborted) setRemoteHits([])
    }).finally(() => {
      if (!abort.signal.aborted) setRemoteLoading(false)
    })
    return () => abort.abort()
  }, [query, store])

  useEffect(() => {
    if (typeof localStorage === 'undefined') return
    try { localStorage.setItem('shutu.web.sidebar-collapsed', String(collapsed)) } catch { /* optional preference */ }
  }, [collapsed])

  const run = async (operation: () => Promise<void>): Promise<void> => {
    setWorking(true)
    try { await operation(); setDialog(null); setMenuId(null); setWorkspaceMenuId(null) }
    catch (error) { onError(error) }
    finally { setWorking(false) }
  }

  const sessionById = useMemo(() => new Map(sessions.map(session => [session.id, session])), [sessions])
  const visibleIds = useMemo(() => new Set(visible.map(session => session.id)), [visible])
  const sessionIdsForWorkspace = (workspaceId: string): string[] => workspaceId === ''
    ? workspaces.ungrouped_ids
    : workspaces.workspaces.find(workspace => workspace.id === workspaceId)?.session_ids ?? []

  const dropWorkspace = async (targetId: string): Promise<void> => {
    if (draggedWorkspaceId === null || draggedWorkspaceId === targetId) return
    const ids = workspaces.workspaces.map(workspace => workspace.id).filter(id => id !== draggedWorkspaceId)
    const targetIndex = ids.indexOf(targetId)
    ids.splice(targetIndex < 0 ? ids.length : targetIndex, 0, draggedWorkspaceId)
    setDraggedWorkspaceId(null)
    await run(() => store.reorderWorkspaces(ids))
  }

  const dropSession = async (workspaceId: string, targetId?: string): Promise<void> => {
    if (draggedSessionId === null) return
    const ids = sessionIdsForWorkspace(workspaceId).filter(id => id !== draggedSessionId)
    const targetIndex = targetId === undefined ? ids.length : Math.max(0, ids.indexOf(targetId))
    ids.splice(targetIndex, 0, draggedSessionId)
    setDraggedSessionId(null)
    await run(() => store.reorderSessions(workspaceId, ids))
  }

  const renderSession = (session: SessionSummary, workspaceId: string) => {
    const status = sessionStatus(session)
    const menuOpen = menuId === session.id
    return <div className={`session-row ${session.id === selectedId ? 'active' : ''} ${draggedSessionId === session.id ? 'dragging' : ''}`} key={session.id} role="treeitem" aria-selected={session.id === selectedId} draggable onDragStart={event => { event.stopPropagation(); setDraggedSessionId(session.id); event.dataTransfer.effectAllowed = 'move'; event.dataTransfer.setData('text/plain', session.id) }} onDragEnd={() => setDraggedSessionId(null)} onDragOver={event => { event.preventDefault(); event.dataTransfer.dropEffect = 'move' }} onDrop={event => { event.preventDefault(); event.stopPropagation(); void dropSession(workspaceId, session.id) }}>
      <button className="session" type="button" onClick={() => { setMenuId(null); void store.open(session.id) }}>
        <span className={`session-state ${status.tone}`} aria-label={status.label} title={status.label} />
        <span className="session-copy"><span className="session-title">{sessionTitle(session)}</span><small>{relativeTime(session.updated_at)} · {session.event_count} events</small></span>
      </button>
      <button className="session-actions" type="button" onClick={event => { event.stopPropagation(); setMenuId(menuOpen ? null : session.id) }} aria-label={`Actions for ${sessionTitle(session)}`} aria-expanded={menuOpen}>⋯</button>
      {menuOpen && <div className="session-menu" role="menu">
        <button type="button" role="menuitem" onClick={() => { setDraftTitle(session.title || ''); setDialog({ kind: 'rename', session }); setMenuId(null) }}>Rename</button>
        <button type="button" role="menuitem" onClick={() => { setDialog({ kind: 'archive', session }); setMenuId(null) }}>Archive</button>
        <button type="button" role="menuitem" className="danger-action" onClick={() => { setDialog({ kind: 'delete', session }); setMenuId(null) }}>Delete</button>
      </div>}
    </div>
  }

  const renderWorkspace = (workspace: WorkspaceView) => {
    const ids = workspace.session_ids.filter(id => visibleIds.has(id))
    const isCollapsed = collapsedWorkspaces[workspace.id] === true
    const menuOpen = workspaceMenuId === workspace.id
    return <section className={`workspace-group ${draggedWorkspaceId === workspace.id ? 'dragging' : ''}`} key={workspace.id}>
      <div className="workspace-group-head" draggable onDragStart={event => { setDraggedWorkspaceId(workspace.id); event.dataTransfer.effectAllowed = 'move'; event.dataTransfer.setData('text/plain', workspace.id) }} onDragEnd={() => setDraggedWorkspaceId(null)} onDragOver={event => { event.preventDefault(); event.dataTransfer.dropEffect = 'move' }} onDrop={event => { event.preventDefault(); void (draggedWorkspaceId !== null ? dropWorkspace(workspace.id) : dropSession(workspace.id)) }}>
        <button type="button" className="workspace-toggle" onClick={() => setCollapsedWorkspaces(previous => ({ ...previous, [workspace.id]: !isCollapsed }))} aria-expanded={!isCollapsed}>
          <span className="workspace-chevron">{isCollapsed ? '›' : '⌄'}</span><span className="workspace-title">{workspace.title}</span><small>{ids.length}</small>
        </button>
        <button type="button" className="workspace-new-session" onClick={() => void run(() => store.createSession(workspace.id))} aria-label={`New session in ${workspace.title}`}>＋</button>
        <button type="button" className="workspace-actions" onClick={() => setWorkspaceMenuId(menuOpen ? null : workspace.id)} aria-label={`Actions for ${workspace.title}`} aria-expanded={menuOpen}>⋯</button>
        {menuOpen && <div className="workspace-menu" role="menu"><button type="button" role="menuitem" onClick={() => { setWorkspaceMenuId(null); setWorkspaceDialog({ kind: 'rename', workspace }) }}>Rename</button><button type="button" role="menuitem" className="danger-action" onClick={() => { setWorkspaceMenuId(null); setWorkspaceDialog({ kind: 'delete', workspace }) }}>Delete</button></div>}
      </div>
      {!isCollapsed && ids.map(id => { const session = sessionById.get(id); return session ? renderSession(session, workspace.id) : null })}
      {!isCollapsed && ids.length === 0 && <div className="workspace-empty">No sessions</div>}
    </section>
  }

  return <>
    <aside className={`sidebar ${collapsed ? 'collapsed' : ''}`} aria-label="Session sidebar">
      <div className="brand-row">
        <button className="brand" type="button" onClick={() => void run(() => store.createSession())} aria-label="New session">
          <span className="brand-mark">S</span><span>Shutu</span><span className="brand-sub">DSH web</span>
        </button>
        <button className="sidebar-toggle" type="button" onClick={() => setCollapsed(value => !value)} aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'} title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}>{collapsed ? '›' : '‹'}</button>
      </div>
      <button className="new-session" type="button" onClick={() => void run(() => store.createSession())}>
        <span aria-hidden="true">＋</span> New session
      </button>
      <div className="sidebar-section-head">
        <span>Sessions</span><span className="session-count">{filtered.length}</span>
        <button type="button" className="sidebar-icon-button" onClick={() => setWorkspaceDialog({ kind: 'create' })} aria-label="New workspace" title="New workspace">＋</button>
        <button type="button" className="sidebar-icon-button" onClick={() => setSearchOpen(value => !value)} aria-label="Search sessions" title="Search sessions">⌕</button>
      </div>
      {searchOpen && <div className="session-search-wrap"><input autoFocus value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => { if (event.key === 'Escape') { setQuery(''); setSearchOpen(false) } }} placeholder="Search sessions…" aria-label="Search sessions" /><button type="button" onClick={() => { setQuery(''); setSearchOpen(false) }} aria-label="Clear session search">×</button></div>}
      <div className="session-list" role="tree" aria-label="Sessions">
        {query.trim() !== '' && remoteHits.length > 0 && <div className="remote-search-results" role="group" aria-label="Search results"><span className="workspace-label">Matching session history</span>{remoteHits.map(hit => <button type="button" className="remote-search-hit" key={hit.id} onClick={() => { setQuery(''); void store.open(hit.id) }}><strong>{hit.title || hit.id}</strong><span>{hit.snippet}</span></button>)}</div>}
        {remoteLoading && query.trim() !== '' && <div className="workspace-empty">Searching history…</div>}
        {workspaces.workspaces.map(renderWorkspace)}
        <section className="workspace-group ungrouped-group">
          <div className="workspace-group-head" onDragOver={event => { event.preventDefault(); event.dataTransfer.dropEffect = 'move' }} onDrop={event => { event.preventDefault(); void dropSession('') }}><span className="workspace-label">Ungrouped</span><small>{workspaces.ungrouped_ids.filter(id => visibleIds.has(id)).length}</small></div>
          {workspaces.ungrouped_ids.filter(id => visibleIds.has(id)).map(id => { const session = sessionById.get(id); return session ? renderSession(session, '') : null })}
        </section>
        {overflow > 0 && !expanded && query.trim() === '' && <button className="session-overflow" type="button" onClick={() => setExpanded(true)}>Show {overflow} more sessions</button>}
        {expanded && query.trim() === '' && overflow > 0 && <button className="session-overflow" type="button" onClick={() => setExpanded(false)}>Show fewer sessions</button>}
        {visible.length === 0 && <div className="sidebar-empty">{query ? 'No matching sessions' : 'No sessions yet'}</div>}
      </div>
      <button className="settings-button" type="button" onClick={onSettings}><span aria-hidden="true">⚙</span> Settings</button>
    </aside>
    {dialog?.kind === 'rename' && <div className="sidebar-dialog-backdrop" role="presentation" onMouseDown={() => !working && setDialog(null)}><form className="sidebar-dialog" role="dialog" aria-modal="true" aria-labelledby="rename-session-title" onSubmit={event => { event.preventDefault(); void run(() => store.renameSession(dialog.session.id, draftTitle.trim())) }} onMouseDown={event => event.stopPropagation()}><h2 id="rename-session-title">Rename session</h2><input autoFocus value={draftTitle} onChange={event => setDraftTitle(event.target.value)} maxLength={120} aria-label="Session name" /><div className="dialog-actions"><button type="button" onClick={() => setDialog(null)} disabled={working}>Cancel</button><button type="submit" disabled={working || draftTitle.trim() === ''}>Save</button></div></form></div>}
    {(dialog?.kind === 'archive' || dialog?.kind === 'delete') && <div className="sidebar-dialog-backdrop" role="presentation" onMouseDown={() => !working && setDialog(null)}><div className="sidebar-dialog" role="dialog" aria-modal="true" aria-labelledby="session-action-title" onMouseDown={event => event.stopPropagation()}><h2 id="session-action-title">{dialog.kind === 'archive' ? 'Archive session?' : 'Delete session?'}</h2><p>{dialog.kind === 'archive' ? 'The session will leave the active list and remain in storage.' : 'This permanently removes the session and its events.'}</p><div className="dialog-actions"><button type="button" onClick={() => setDialog(null)} disabled={working}>Cancel</button><button type="button" className={dialog.kind === 'delete' ? 'danger-button' : ''} disabled={working} onClick={() => void run(() => dialog.kind === 'archive' ? store.archiveSession(dialog.session.id) : store.deleteSession(dialog.session.id))}>{dialog.kind === 'archive' ? 'Archive' : 'Delete'}</button></div></div></div>}
    {workspaceDialog !== null && <WorkspaceDialog store={store} dialog={workspaceDialog} onClose={() => setWorkspaceDialog(null)} onError={onError} />}
  </>
}

type SettingsSection = 'general' | 'model' | 'capabilities' | 'tools' | 'runtime' | 'skills'
type ThemePreference = 'dark' | 'light' | 'system'

const CAPABILITY_LABELS: Record<string, string> = {
  code_enabled: '代码执行', compaction_enabled: '上下文压缩', eval_enabled: '评估工具',
  fs_enabled: '文件系统', fs_search_enabled: '文件搜索', interact_enabled: '交互提问',
  jobs_enabled: '后台任务', mcp_enabled: 'MCP 工具', multimodal_enabled: '多模态',
  plan_enabled: '计划模式', ralph_enabled: 'Ralph 工作流', schedule_enabled: '定时任务',
  skill_enabled: '技能系统', spill_enabled: '上下文溢出存储', subagent_enabled: '子代理',
  terminal_enabled: '终端', web_enabled: '网页访问', workflow_enabled: '工作流',
}

function configText(config: ConfigView | null, key: string, fallback = '—'): string {
  const value = config?.[key]
  return typeof value === 'string' && value !== '' ? value : fallback
}

function SettingsControls({ store, config, settings, onSaved }: { store: WebStore; config: ConfigView; settings: SettingsView; onSaved: () => void }) {
  const providers = config.providers ?? []
  const [provider, setProvider] = useState(config.llm_provider ?? providers[0]?.id ?? '')
  const [model, setModel] = useState(config.model ?? providers[0]?.model ?? providers[0]?.candidates?.[0] ?? '')
  const [busy, setBusy] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [values, setValues] = useState({ agent_preset: settings.agent_preset ?? '', permission_preset: settings.permission_preset ?? '', terminal_shell: settings.terminal_shell ?? '' })
  const currentProvider = providers.find(item => item.id === provider) ?? providers[0]
  const modelOptions = useMemo(() => currentProvider?.models?.map(item => item.id) ?? currentProvider?.candidates ?? [], [currentProvider])

  useEffect(() => {
    setProvider(config.llm_provider ?? providers[0]?.id ?? '')
    setModel(config.model ?? providers[0]?.model ?? providers[0]?.candidates?.[0] ?? '')
    setValues({ agent_preset: settings.agent_preset ?? '', permission_preset: settings.permission_preset ?? '', terminal_shell: settings.terminal_shell ?? '' })
  }, [config.llm_provider, config.model, providers, settings.agent_preset, settings.permission_preset, settings.terminal_shell])

  useEffect(() => {
    if (modelOptions.length > 0 && !modelOptions.includes(model)) setModel(modelOptions[0])
  }, [model, modelOptions])

  const saveSetting = async (key: 'agent_preset' | 'permission_preset' | 'terminal_shell'): Promise<void> => {
    setBusy(key); setNotice(null)
    try { await store.updateSettings({ [key]: values[key] }); setNotice('已保存，重启服务后生效'); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(null) }
  }

  const saveModel = async (): Promise<void> => {
    if (provider === '' || model === '') return
    setBusy('model'); setNotice(null)
    try { await store.switchModel(provider, model, config.reasoning_effort); setNotice('模型已切换'); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(null) }
  }

  return <section className="settings-controls" aria-label="Editable settings">
    <div className="settings-control-head"><div><strong>快速配置</strong><span>可直接应用的运行时选择；持久化项会标记重启要求。</span></div>{notice && <span className="settings-control-notice" role="status">{notice}</span>}</div>
    <div className="settings-control-grid">
      <label><span>Agent preset</span><select value={values.agent_preset} onChange={event => setValues(previous => ({ ...previous, agent_preset: event.target.value }))}>{(settings.mode_options ?? ['minimal', 'standard', 'code']).map(item => <option key={item} value={item}>{item}</option>)}</select><button type="button" disabled={busy !== null || values.agent_preset === ''} onClick={() => void saveSetting('agent_preset')}>{busy === 'agent_preset' ? '保存中…' : '保存'}</button></label>
      <label><span>Permission</span><select value={values.permission_preset} onChange={event => setValues(previous => ({ ...previous, permission_preset: event.target.value }))}>{(settings.permission_options ?? ['readonly', 'standard', 'full']).map(item => <option key={item} value={item}>{item}</option>)}</select><button type="button" disabled={busy !== null || values.permission_preset === ''} onClick={() => void saveSetting('permission_preset')}>{busy === 'permission_preset' ? '保存中…' : '保存'}</button></label>
      <label><span>Terminal shell</span><select value={values.terminal_shell} onChange={event => setValues(previous => ({ ...previous, terminal_shell: event.target.value }))}>{(settings.terminal_options ?? ['off', 'powershell', 'gitbash', 'wsl']).map(item => <option key={item} value={item}>{item}</option>)}</select><button type="button" disabled={busy !== null || values.terminal_shell === ''} onClick={() => void saveSetting('terminal_shell')}>{busy === 'terminal_shell' ? '保存中…' : '保存'}</button></label>
      <label><span>Provider</span><select value={provider} onChange={event => { setProvider(event.target.value); const next = providers.find(item => item.id === event.target.value); setModel(next?.model ?? next?.candidates?.[0] ?? '') }}>{providers.length > 0 ? providers.map(item => <option key={item.id} value={item.id}>{item.name || item.id}</option>) : <option value="">服务端未返回 Provider</option>}</select><small>当前：{provider || '—'}</small></label>
      <label><span>Model</span><select value={model} onChange={event => setModel(event.target.value)}>{modelOptions.length > 0 ? modelOptions.map(item => <option key={item} value={item}>{item}</option>) : <option value={model}>{model || '服务端未返回模型'}</option>}</select><button type="button" disabled={busy !== null || provider === '' || model === ''} onClick={() => void saveModel()}>{busy === 'model' ? '切换中…' : '应用模型'}</button></label>
    </div>
  </section>
}

function ProviderManager({ store, providers, onSaved }: { store: WebStore; providers: readonly ProviderView[]; onSaved: () => void }) {
  const [selectedId, setSelectedId] = useState(providers[0]?.id ?? '')
  const selected = providers.find(item => item.id === selectedId)
  const [form, setForm] = useState({ id: selected?.id ?? '', name: selected?.name ?? '', base_url: selected?.base_url ?? '', model: selected?.model ?? selected?.candidates?.[0] ?? '', api_key: '', protocol: selected?.protocol ?? 'openai-completions', custom: selected?.custom ?? false })
  const [discovered, setDiscovered] = useState<ProviderModelView[]>([])
  const [busy, setBusy] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  useEffect(() => {
    const next = providers.find(item => item.id === selectedId) ?? providers[0]
    if (next === undefined) return
    setForm({ id: next.id, name: next.name ?? '', base_url: next.base_url ?? '', model: next.model ?? next.candidates?.[0] ?? '', api_key: '', protocol: next.protocol ?? 'openai-completions', custom: next.custom ?? false })
    setDiscovered([])
  }, [providers, selectedId])

  const save = async (): Promise<void> => {
    if (form.id.trim() === '') return
    setBusy('save'); setNotice(null)
    try { await store.saveProvider({ ...form, id: form.id.trim(), name: form.name.trim(), base_url: form.base_url.trim(), model: form.model.trim(), api_key: form.api_key, protocol: form.protocol.trim() }); setNotice('Provider 已保存并即时应用'); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(null) }
  }

  const remove = async (): Promise<void> => {
    if (!form.custom || form.id.trim() === '') return
    setBusy('delete'); setNotice(null)
    try { await store.deleteProvider(form.id); setNotice('Provider 已删除'); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(null) }
  }

  const discover = async (): Promise<void> => {
    if (form.id.trim() === '') return
    setBusy('discover'); setNotice(null)
    try { setDiscovered(await store.discoverProvider({ provider: form.id, base_url: form.base_url, protocol: form.protocol, api_key: form.api_key })); setNotice('已读取模型目录') }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(null) }
  }

  return <section className="runtime-manager"><div className="runtime-manager-head"><div><h2>Provider 管理</h2><p>保存 API Key、端点和模型目录；敏感字段不会回显。</p></div>{notice && <span className="settings-control-notice" role="status">{notice}</span>}</div>
    <div className="provider-layout"><div className="provider-list">{providers.length === 0 ? <span className="muted">暂无 Provider</span> : providers.map(item => <button type="button" className={item.id === selectedId ? 'selected' : ''} key={item.id} onClick={() => setSelectedId(item.id)}><strong>{item.name || item.id}</strong><small>{item.id} · {item.configured ? '已配置' : '未配置'}</small></button>)}</div>
      <div className="provider-editor"><div className="provider-editor-grid"><label>ID<input value={form.id} disabled={!form.custom} onChange={event => setForm(previous => ({ ...previous, id: event.target.value }))} /></label><label>名称<input value={form.name} onChange={event => setForm(previous => ({ ...previous, name: event.target.value }))} /></label><label>Base URL<input value={form.base_url} onChange={event => setForm(previous => ({ ...previous, base_url: event.target.value }))} placeholder="https://…" /></label><label>Model<input value={form.model} onChange={event => setForm(previous => ({ ...previous, model: event.target.value }))} /></label><label>Protocol<select value={form.protocol} onChange={event => setForm(previous => ({ ...previous, protocol: event.target.value }))}><option value="openai-completions">OpenAI Completions</option><option value="anthropic-messages">Anthropic Messages</option><option value="google-generative-ai">Google Generative AI</option><option value="openai-responses">OpenAI Responses</option></select></label><label>API Key<input type="password" value={form.api_key} onChange={event => setForm(previous => ({ ...previous, api_key: event.target.value }))} placeholder="留空则不修改" /></label></div><div className="runtime-actions"><button type="button" disabled={busy !== null} onClick={() => void save()}>{busy === 'save' ? '保存中…' : '保存 Provider'}</button><button type="button" disabled={busy !== null} onClick={() => void discover()}>{busy === 'discover' ? '读取中…' : '发现模型'}</button>{form.custom && <button type="button" disabled={busy !== null} onClick={() => void remove()}>删除</button>}</div>{discovered.length > 0 && <div className="discovered-models"><span>发现的模型</span>{discovered.map(item => <button type="button" key={item.id} onClick={() => setForm(previous => ({ ...previous, model: item.id }))}>{item.id}</button>)}</div>}</div>
    </div>
  </section>
}

function McpManager({ store, servers, onSaved }: { store: WebStore; servers: readonly MCPServerView[]; onSaved: () => void }) {
  const [items, setItems] = useState<MCPServerView[]>([...servers])
  const [editing, setEditing] = useState('')
  const [form, setForm] = useState({ name: '', cmd: '', args: '' })
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  useEffect(() => setItems([...servers]), [servers])
  const reset = () => { setEditing(''); setForm({ name: '', cmd: '', args: '' }) }
  const save = async (): Promise<void> => {
    if (form.name.trim() === '' || form.cmd.trim() === '') return
    setBusy(true); setNotice(null)
    try { const next = await store.manageMcp(editing === '' ? 'add' : 'update', { original_name: editing || undefined, name: form.name.trim(), cmd: form.cmd.trim(), args: form.args.trim() ? form.args.trim().split(/\s+/) : [] }); setItems(next); setNotice('MCP 配置已保存，重启后启动'); reset(); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(false) }
  }
  const remove = async (name: string): Promise<void> => {
    setBusy(true); setNotice(null)
    try { setItems(await store.manageMcp('delete', { original_name: name })); setNotice('MCP 服务已删除'); onSaved() }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(false) }
  }
  const refresh = async (): Promise<void> => {
    setBusy(true); setNotice(null)
    try { setItems(await store.refreshMcp()); setNotice('连接状态已刷新') }
    catch (error) { setNotice(error instanceof Error ? error.message : String(error)) }
    finally { setBusy(false) }
  }
  return <section className="runtime-manager"><div className="runtime-manager-head"><div><h2>MCP 管理</h2><p>维护 stdio MCP 服务并刷新连接诊断。</p></div><div className="runtime-actions"><button type="button" disabled={busy} onClick={() => void refresh()}>刷新连接</button>{notice && <span className="settings-control-notice" role="status">{notice}</span>}</div></div><div className="mcp-list">{items.length === 0 ? <span className="muted">暂无 MCP 服务</span> : items.map(item => <div className="mcp-row" key={item.name}><div><strong>{item.name}</strong><small>{item.cmd} {(item.args ?? []).join(' ')}</small></div><span className={`mcp-state ${item.connected ? 'on' : ''}`}>{item.connected ? '已连接' : '未连接'}</span><button type="button" disabled={busy} onClick={() => { setEditing(item.name ?? ''); setForm({ name: item.name ?? '', cmd: item.cmd ?? '', args: (item.args ?? []).join(' ') }) }}>编辑</button><button type="button" disabled={busy} onClick={() => void remove(item.name ?? '')}>删除</button></div>)}</div><div className="mcp-editor"><label>名称<input value={form.name} onChange={event => setForm(previous => ({ ...previous, name: event.target.value }))} /></label><label>命令<input value={form.cmd} onChange={event => setForm(previous => ({ ...previous, cmd: event.target.value }))} /></label><label>参数<input value={form.args} onChange={event => setForm(previous => ({ ...previous, args: event.target.value }))} placeholder="按空格分隔" /></label><div className="runtime-actions"><button type="button" disabled={busy} onClick={() => void save()}>{editing ? '保存修改' : '新增 MCP'}</button>{editing && <button type="button" disabled={busy} onClick={reset}>取消</button>}</div></div></section>
}

function encodeBase64(value: string): string {
  const bytes = new TextEncoder().encode(value)
  let binary = ''
  bytes.forEach(byte => { binary += String.fromCharCode(byte) })
  return btoa(binary)
}

function SkillsManager({ store }: { store: WebStore }) {
  const [data, setData] = useState<SkillsView | null>(null)
  const [selected, setSelected] = useState<SkillView | null>(null)
  const [content, setContent] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [form, setForm] = useState({ name: '', scope: 'global', kind: 'flat', content: '' })

  const load = useCallback(async (signal?: AbortSignal): Promise<void> => {
    setLoading(true)
    setError(null)
    try {
      const next = await store.listSkills(signal)
      if (signal?.aborted) return
      setData(next)
      setForm(previous => ({ ...previous, scope: previous.scope || next.scopes[0]?.id || 'global' }))
      setSelected(previous => previous === null ? null : next.skills.find(item => item.name === previous.name && item.scope === previous.scope) ?? null)
    } catch (reason) {
      if (!signal?.aborted) setError(reason instanceof Error ? reason.message : String(reason))
    } finally {
      if (!signal?.aborted) setLoading(false)
    }
  }, [store])

  useEffect(() => {
    const abort = new AbortController()
    void load(abort.signal)
    return () => abort.abort()
  }, [load])

  const open = async (skill: SkillView): Promise<void> => {
    setSelected(skill)
    setContent(null)
    try {
      const result = await store.skillAction('content', { name: skill.name, scope: skill.scope })
      setContent(typeof result.content === 'string' ? result.content : '')
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
  }

  const toggle = async (skill: SkillView): Promise<void> => {
    setBusy(true); setError(null)
    try { await store.skillAction('set_enabled', { name: skill.name, scope: skill.scope, enabled: !skill.enabled }); await load() }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
    finally { setBusy(false) }
  }

  const remove = async (skill: SkillView): Promise<void> => {
    setBusy(true); setError(null)
    try { await store.skillAction('delete', { name: skill.name, scope: skill.scope }); setSelected(null); setContent(null); await load() }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
    finally { setBusy(false) }
  }

  const add = async (): Promise<void> => {
    const name = form.name.trim()
    if (name === '' || form.content.trim() === '') return
    setBusy(true); setError(null)
    try {
      await store.skillAction('add', { kind: form.kind, scope: form.scope, files: [{ path: `${name}.md`, base64: encodeBase64(form.content) }] })
      setForm(previous => ({ ...previous, name: '', content: '' }))
      await load()
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
    finally { setBusy(false) }
  }

  return <div className="skills-manager">
    <div className="skills-manager-head"><div><h2>Skills</h2><p>查看、启用、停用和管理 Agent 技能文件。</p></div><span className="settings-control-notice" role="status">{data?.skills.length ?? 0} skills</span></div>
    {loading && <div className="settings-state"><div className="spinner" />Loading skills…</div>}
    {!loading && error && <div className="settings-state settings-error"><strong>Unable to load skills</strong><span>{error}</span><button type="button" onClick={() => void load()}>Retry</button></div>}
    {!loading && data !== null && <>
      <div className="skills-layout">
        <div className="skills-list" role="list" aria-label="Skills list">
          {data.skills.length === 0 && <span className="muted">No skills found.</span>}
          {data.skills.map(skill => <div className={`skill-row ${selected?.name === skill.name && selected?.scope === skill.scope ? 'selected' : ''}`} key={`${skill.scope ?? ''}:${skill.name}`} role="listitem">
            <button type="button" className="skill-select" onClick={() => void open(skill)}><strong>{skill.name}</strong><span>{skill.description || skill.when_to_use || skill.kind || 'Skill'}</span><small>{skill.scope || 'global'} · {skill.enabled === false ? 'disabled' : 'enabled'}</small></button>
            <div className="skill-actions"><button type="button" disabled={busy} onClick={() => void toggle(skill)}>{skill.enabled === false ? 'Enable' : 'Disable'}</button><button type="button" disabled={busy} onClick={() => void remove(skill)}>Delete</button></div>
          </div>)}
        </div>
        <article className="skill-detail">{selected === null ? <div className="settings-state">Select a skill to inspect its content.</div> : <><div className="skill-detail-head"><div><strong>{selected.name}</strong><span>{selected.source || selected.rel || selected.scope || 'global'}</span></div><span className={`capability-status ${selected.enabled === false ? 'off' : 'on'}`}>{selected.enabled === false ? 'Disabled' : 'Enabled'}</span></div><pre>{content === null ? 'Loading content…' : content || 'This skill has no readable content.'}</pre></>}</article>
      </div>
      <div className="skill-add"><div><h3>Add skill</h3><p>Paste a Markdown skill file. The name becomes the file name.</p></div><div className="skill-add-grid"><label>Name<input value={form.name} onChange={event => setForm(previous => ({ ...previous, name: event.target.value }))} placeholder="my-skill" /></label><label>Scope<select value={form.scope} onChange={event => setForm(previous => ({ ...previous, scope: event.target.value }))}>{data.scopes.length > 0 ? data.scopes.map(scope => <option key={scope.id} value={scope.id}>{scope.label || scope.id}</option>) : <option value="global">global</option>}</select></label><label>Kind<select value={form.kind} onChange={event => setForm(previous => ({ ...previous, kind: event.target.value }))}><option value="flat">flat</option><option value="project">project</option></select></label></div><textarea value={form.content} onChange={event => setForm(previous => ({ ...previous, content: event.target.value }))} placeholder="---\nname: my-skill\ndescription: ...\n---\n\nInstructions..." rows={7} /><button type="button" disabled={busy || form.name.trim() === '' || form.content.trim() === ''} onClick={() => void add()}>Add skill</button></div>
    </>}
  </div>
}

function SettingsPage({ store, theme, onThemeChange, onBack }: {
  store: WebStore
  theme: ThemePreference
  onThemeChange: (theme: ThemePreference) => void
  onBack: () => void
}) {
  const [section, setSection] = useState<SettingsSection>('general')
  const [config, setConfig] = useState<ConfigView | null>(null)
  const [settings, setSettings] = useState<SettingsView | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [reload, setReload] = useState(0)

  useEffect(() => {
    const abort = new AbortController()
    setLoading(true)
    setError(null)
    void Promise.all([store.getConfig(abort.signal), store.getSettings(abort.signal)]).then(([value, nextSettings]) => {
      if (abort.signal.aborted) return
      setConfig(value)
      setSettings(nextSettings)
    }).catch(reason => {
      if (!abort.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason))
    }).finally(() => {
      if (!abort.signal.aborted) setLoading(false)
    })
    return () => abort.abort()
  }, [reload, store])

  const capabilities = useMemo(() => Object.entries(config ?? {})
    .filter(([key, value]) => key.endsWith('_enabled') && typeof value === 'boolean')
    .sort(([left], [right]) => (CAPABILITY_LABELS[left] ?? left).localeCompare(CAPABILITY_LABELS[right] ?? right, 'zh-CN')),
  [config])
  const tools = useMemo(() => Array.isArray(config?.tools_enabled)
    ? config.tools_enabled.filter((tool): tool is string => typeof tool === 'string')
    : [], [config])
  const providers = useMemo(() => config?.providers ?? [], [config?.providers])
  const mcpServers = useMemo(() => config?.mcp_servers ?? [], [config?.mcp_servers])
  const toolCount = typeof config?.tools_enabled_count === 'number' ? config.tools_enabled_count : tools.length
  const sections: { id: SettingsSection; label: string; hint: string }[] = [
    { id: 'general', label: '通用设置', hint: '界面与运行模式' },
    { id: 'model', label: '模型', hint: '当前会话模型' },
    { id: 'capabilities', label: '能力开关', hint: '功能启用状态' },
    { id: 'tools', label: '工具', hint: `${toolCount} 个已注册工具` },
    { id: 'runtime', label: '运行时清单', hint: 'Provider 与 MCP' },
  ]

  sections.push({ id: 'skills', label: 'Skills', hint: 'Agent 技能文件' })

  return <div className="settings-page">
    <header className="settings-topbar">
      <button className="settings-back" type="button" onClick={onBack}>‹ 返回聊天</button>
      <div className="settings-theme-toggle" role="group" aria-label="主题"><button type="button" className={theme === 'light' ? 'selected' : ''} onClick={() => onThemeChange('light')}>浅色</button><button type="button" className={theme === 'dark' ? 'selected' : ''} onClick={() => onThemeChange('dark')}>深色</button><button type="button" className={theme === 'system' ? 'selected' : ''} onClick={() => onThemeChange('system')}>系统</button></div>
    </header>
    <main className="settings-panel">
      <div className="settings-heading"><div><span className="eyebrow">DSH WEB</span><h1>设置</h1><p>查看当前运行配置与已启用能力。</p></div><button className="settings-close" type="button" onClick={onBack} aria-label="关闭设置">×</button></div>
      <div className="settings-layout">
        <nav className="settings-nav" aria-label="设置分区">{sections.map(item => <button key={item.id} type="button" className={section === item.id ? 'selected' : ''} onClick={() => setSection(item.id)}><strong>{item.label}</strong><span>{item.hint}</span></button>)}</nav>
        <section className="settings-content" aria-live="polite">
          {!loading && !error && config !== null && settings !== null && <SettingsControls store={store} config={{ ...config, providers }} settings={settings} onSaved={() => setReload(value => value + 1)} />}
          {loading && <div className="settings-state"><div className="spinner" />正在加载配置…</div>}
          {!loading && error && <div className="settings-state settings-error"><strong>配置读取失败</strong><span>{error}</span><button type="button" onClick={() => setReload(value => value + 1)}>重试</button></div>}
          {!loading && !error && config !== null && section === 'general' && <div className="settings-section"><h2>通用设置</h2><p className="settings-description">这些选项描述当前 Web 工作区的运行方式。</p><div className="setting-group"><h3>外观</h3><div className="appearance-options"><button type="button" className={theme === 'light' ? 'selected' : ''} onClick={() => onThemeChange('light')}><span className="appearance-swatch light-swatch" />浅色</button><button type="button" className={theme === 'dark' ? 'selected' : ''} onClick={() => onThemeChange('dark')}><span className="appearance-swatch dark-swatch" />深色</button><button type="button" className={theme === 'system' ? 'selected' : ''} onClick={() => onThemeChange('system')}><span className="appearance-swatch system-swatch" />系统</button></div></div><div className="setting-row"><div><strong>运行模式</strong><span>当前 Agent 的默认权限与行为模式</span></div><code>{configText(config, 'mode')}</code></div><div className="setting-row"><div><strong>Web 地址</strong><span>当前服务监听地址</span></div><code>{configText(config, 'web_server_addr')}</code></div><div className="setting-row"><div><strong>配置文件</strong><span>配置由服务启动时加载，Web 端只读展示</span></div><span className="readonly-badge">只读</span></div><div className="settings-note">修改配置文件或能力开关后，重启 Shutu 服务才能生效。</div></div>}
          {!loading && !error && config !== null && section === 'model' && <div className="settings-section"><h2>模型</h2><p className="settings-description">当前连接使用的模型配置。</p><article className="model-card"><div className="model-card-head"><div className="model-avatar">D</div><div><strong>DeepSeek</strong><span>{configText(config, 'llm_provider')}</span></div><span className="readonly-badge">只读</span></div><div className="model-fields"><div><span>模型 ID</span><code>{configText(config, 'model')}</code></div><div><span>API 地址</span><code>{configText(config, 'base_url', '使用默认地址')}</code></div><div><span>会话模式</span><code>{configText(config, 'mode')}</code></div></div></article><div className="settings-note">模型切换与凭据管理由服务端配置负责，当前页面不直接修改提供方。</div></div>}
          {!loading && !error && config !== null && section === 'capabilities' && <div className="settings-section"><h2>能力开关</h2><p className="settings-description">根据服务端配置自动发现的功能开关。</p><div className="capability-list">{capabilities.map(([key, value]) => <div className="capability-row" key={key}><div><strong>{CAPABILITY_LABELS[key] ?? key.replace(/_enabled$/, '')}</strong><span>{key}</span></div><span className={`capability-status ${value ? 'on' : 'off'}`}>{value ? '已启用' : '已关闭'}</span></div>)}</div></div>}
          {!loading && !error && config !== null && section === 'tools' && <div className="settings-section"><h2>工具</h2><p className="settings-description">当前注册并可供 Agent 使用的工具。</p><div className="tool-summary"><strong>{toolCount}</strong><span>个工具已启用</span></div><div className="tool-list">{tools.length > 0 ? tools.map(tool => <span className="tool-chip" key={tool}>{tool}</span>) : <span className="muted">服务端未返回工具清单。</span>}</div><div className="settings-note">工具的实际可用性还会受到对应能力开关和当前会话权限影响。</div></div>}
          {!loading && !error && config !== null && section === 'runtime' && <div className="settings-section"><ProviderManager store={store} providers={providers} onSaved={() => setReload(value => value + 1)} /><McpManager store={store} servers={mcpServers} onSaved={() => setReload(value => value + 1)} /></div>}
          {section === 'skills' && <div className="settings-section"><SkillsManager store={store} /></div>}
        </section>
      </div>
    </main>
  </div>
}

function detailText(value: unknown): string {
  if (typeof value === 'string') return value
  try { return JSON.stringify(value, null, 2) } catch { return String(value) }
}

function detailEntries(details: EventDetails | undefined): [string, string][] {
  if (!details) return []
  return Object.entries(details)
    .filter(([, value]) => value !== undefined && value !== null && value !== '')
    .map(([key, value]) => [key, detailText(value)])
}

function parentFilePath(path: string): string {
  const parts = path.split('/').filter(Boolean)
  parts.pop()
  return parts.join('/')
}

function FilesPanel({ store, sessionId, onClose, onReference, openPath }: {
  store: WebStore
  sessionId: string
  onClose: () => void
  onReference: (path: string) => void
  openPath?: string | null
}) {
  // The openPath prop is intentionally read by the effect below rather than
  // changing the listing directory: a produced file can be outside the
  // currently browsed folder, while its preview remains a useful destination.
  const [path, setPath] = useState('')
  const [query, setQuery] = useState('')
  const [listing, setListing] = useState<Awaited<ReturnType<WebStore['listFiles']>> | null>(null)
  const [preview, setPreview] = useState<Awaited<ReturnType<WebStore['previewFile']>> | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const abort = new AbortController()
    setLoading(true)
    setError(null)
    void store.listFiles(sessionId, path, query, abort.signal).then(value => {
      if (!abort.signal.aborted) setListing(value)
    }).catch(reason => {
      if (!abort.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason))
    }).finally(() => {
      if (!abort.signal.aborted) setLoading(false)
    })
    return () => abort.abort()
  }, [path, query, sessionId, store])

  const openFile = async (filePath: string): Promise<void> => {
    setPreview(null)
    setError(null)
    try { setPreview(await store.previewFile(sessionId, filePath)) }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
  }

  useEffect(() => {
    if (!openPath) return
    const abort = new AbortController()
    setPreview(null)
    setError(null)
    void store.previewFile(sessionId, openPath, undefined, undefined, abort.signal).then(value => {
      if (!abort.signal.aborted) setPreview(value)
    }).catch(reason => {
      if (!abort.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason))
    })
    return () => abort.abort()
  }, [openPath, sessionId, store])

  return <section className="files-panel" aria-label="Workspace files">
    <div className="files-panel-head"><div><span className="eyebrow">WORKSPACE FILES</span><h2>Files</h2><p>{listing?.root || 'Session workspace'}</p></div><button type="button" className="settings-close" onClick={onClose} aria-label="Close files">×</button></div>
    <div className="files-toolbar"><button type="button" onClick={() => { setPath(parentFilePath(path)); setPreview(null) }} disabled={path === ''}>↑ Up</button><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Search files…" aria-label="Search workspace files" />{path !== '' && <code>{path}</code>}</div>
    {loading && <div className="files-state"><div className="spinner" />Loading files…</div>}
    {!loading && error && <div className="files-state files-error"><strong>Unable to load files</strong><span>{error}</span></div>}
    {!loading && !error && listing !== null && <div className="files-layout">
      <div className="files-list" role="list" aria-label="Workspace file list">{listing.entries.length === 0 ? <div className="files-state">No files found.</div> : listing.entries.map(entry => <div className="file-row" role="listitem" key={entry.path}><button type="button" className="file-open" onClick={() => entry.dir ? (setPath(entry.path), setPreview(null)) : void openFile(entry.path)}><span>{entry.dir ? '📁' : '📄'}</span><strong>{entry.name}</strong><small>{entry.dir ? 'Folder' : entry.size === undefined ? '' : `${Math.ceil(entry.size / 1024)} KB`}</small></button>{!entry.dir && <button type="button" className="file-reference" onClick={() => onReference(entry.path)}>引用</button>}</div>)}</div>
      {preview !== null && <article className="file-preview"><div className="file-preview-head"><strong>{preview.path}</strong><span>Lines {preview.start_line}–{preview.end_line} / {preview.total_lines}</span><button type="button" onClick={() => onReference(preview.path)}>引用文件</button></div><pre>{preview.content}</pre></article>}
    </div>}
  </section>
}

function EventImages({ store, sessionId, images }: { store: WebStore; sessionId: string; images: readonly ImageView[] }) {
  const imageKey = images.map(image => image.id).join(',')
  const [urls, setUrls] = useState<Record<string, string>>({})
  useEffect(() => {
    const abort = new AbortController()
    const objectUrls: string[] = []
    void Promise.all(images.map(async image => {
      try {
        const blob = await store.loadAttachment(sessionId, image.id, abort.signal)
        const url = URL.createObjectURL(blob)
        objectUrls.push(url)
        return [image.id, url] as const
      } catch {
        return null
      }
    })).then(entries => {
      if (abort.signal.aborted) return
      setUrls(Object.fromEntries(entries.filter((entry): entry is readonly [string, string] => entry !== null)))
    })
    return () => { abort.abort(); objectUrls.forEach(url => URL.revokeObjectURL(url)) }
  }, [imageKey, sessionId, store])
  return <div className="event-images">{images.map(image => urls[image.id] ? <img key={image.id} src={urls[image.id]} alt="消息附件" /> : <span className="event-image-loading" key={image.id}>图片加载中…</span>)}</div>
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/)
  return parts.at(-1) || path
}

function ProducedFiles({ paths, onOpenFile }: { paths: readonly string[]; onOpenFile?: (path: string) => void }) {
  if (paths.length === 0) return null
  const visible = paths.slice(0, 6)
  return <div className="produced-files" aria-label="Produced files"><span>Produced</span><div>{visible.map(path => <button type="button" key={path} title={path} onClick={() => onOpenFile?.(path)}>{basename(path)}</button>)}{paths.length > visible.length && <small>+{paths.length - visible.length} more</small>}</div></div>
}

function RichText({ text }: { text: string }) {
  const blocks = text.split(/(```[^\n]*\n[\s\S]*?```)/g).filter(Boolean)
  return <div className="rich-text">{blocks.map((block, index) => {
    if (block.startsWith('```')) {
      const firstLineEnd = block.indexOf('\n')
      const language = firstLineEnd > 0 ? block.slice(3, firstLineEnd).trim() : ''
      const content = block.slice(firstLineEnd + 1, block.endsWith('```') ? -3 : undefined)
      return <pre className="rich-code" data-language={language || undefined} key={`${index}:${language}`}><code>{content}</code></pre>
    }
    return block.split(/\n{2,}/).map((paragraph, paragraphIndex) => {
      const lines = paragraph.split('\n')
      return <p key={`${index}:${paragraphIndex}`}>{lines.map((line, lineIndex) => <span key={`${lineIndex}:${line}`}>{line}{lineIndex < lines.length - 1 && <br />}</span>)}</p>
    })
  })}</div>
}

function EventCard({ event, store, sessionId, feedback, producedPaths = [], onFeedback, onCopy, onRetry, onFork, onOpenFile }: { event: EventView; store: WebStore; sessionId: string; feedback?: FeedbackView; producedPaths?: readonly string[]; onFeedback: (seq: number, rating: 'positive' | 'negative') => void; onCopy?: (text: string) => void; onRetry?: (text: string) => void; onFork?: () => void; onOpenFile?: (path: string) => void }) {
  const [expanded, setExpanded] = useState(false)
  const text = event.tool_output || event.reasoning || event.summary || event.compaction_summary || 'No content'
  const entries = detailEntries(event.details)
  const tokenUsage = event.details?.usage
  const hasExtra = entries.length > 0 || Boolean(event.tool_args) || text.length > 320
  const tone = event.type.includes('error') || event.type.includes('failed') ? 'danger' :
    event.type.startsWith('user/') ? 'user' : event.type.startsWith('assistant/') ? 'assistant' : 'system'

  return (
    <article className={`event-card ${tone} ${expanded ? 'expanded' : ''}`}>
      <div className="event-line" aria-hidden="true" />
      <div className="event-head">
        <span className="seq">#{event.seq}</span>
        <span className="event-label">{eventLabel(event)}</span>
        <time>{formatTime(event.time)}</time>
        {event.version > 1 && <span className="version">v{event.version}</span>}
      </div>
      <div className="event-content">
        {event.call_id && <span className="call-id">call {event.call_id}</span>}
        <RichText text={text} />
        {event.type === 'assistant/message' && <div className="event-actions message-actions" aria-label="Message actions"><button type="button" onClick={() => onCopy ? onCopy(text) : void navigator.clipboard?.writeText(text)}>Copy</button><button type="button" onClick={() => onRetry ? onRetry(text) : void store.send(text)}>Retry</button><button type="button" onClick={() => onFork ? onFork() : void store.forkSession(sessionId)}>Fork</button></div>}
        {event.images && event.images.length > 0 && <EventImages store={store} sessionId={sessionId} images={event.images} />}
        {event.type === 'assistant/message' && <ProducedFiles paths={producedPaths} onOpenFile={onOpenFile} />}
        {event.tool_args && <div className="code-block"><span>Input</span><pre>{event.tool_args}</pre></div>}
        {tokenUsage !== undefined && <span className="token-chip">Tokens {detailText(tokenUsage)}</span>}
        {hasExtra && <button className="text-button" onClick={() => setExpanded(value => !value)}>{expanded ? 'Collapse details' : 'Expand details'}</button>}
        {expanded && entries.length > 0 && <div className="details-grid">
          {entries.map(([key, value]) => <div className="detail-item" key={key}><span>{key}</span><pre>{value}</pre></div>)}
        </div>}
        {event.type === 'assistant/message' && <div className="event-actions" aria-label="消息反馈"><button type="button" className={feedback?.rating === 'positive' ? 'selected' : ''} onClick={() => onFeedback(event.seq, 'positive')} aria-label="有帮助">👍</button><button type="button" className={feedback?.rating === 'negative' ? 'selected' : ''} onClick={() => onFeedback(event.seq, 'negative')} aria-label="没帮助">👎</button></div>}
      </div>
    </article>
  )
}

function DshTimeline({ events, onSelectSeq }: {
  events: readonly EventView[]
  onSelectSeq: (seq: number) => void
}) {
  const [mode, setMode] = useState<DshTimelineMode>('sequence')
  const [selected, setSelected] = useState<number | null>(null)
  const projection = useMemo(() => projectDshTrajectory(events, mode), [events, mode])
  const { timeline } = projection
  const sourceSeqByIndex = useMemo(() => new Map(
    projection.turns.flatMap(turn => turn.groups.flatMap(group =>
      group.cells.map(cell => [cell.index, cell.sourceSeq] as const))),
  ), [projection.turns])
  if (timeline === null) return null
  const span = Math.max(1, timeline.end - timeline.start)
  return <section className="dsh-timeline" aria-label="Trajectory timeline">
    <div className="timeline-head"><div><strong>Timeline</strong><span>{timeline.spans.length} records</span></div><select aria-label="Timeline mode" value={mode} onChange={event => { setMode(event.target.value as DshTimelineMode); setSelected(null) }}><option value="sequence">Sequence</option><option value="duration">Duration</option><option value="time">Recorded time</option><option value="actual">Actual time</option></select></div>
    <div className="timeline-track">
      {timeline.spans.map(item => <button key={`${item.index}-${item.start}`} className={`timeline-span lane-${item.lane} ${item.isError ? 'error' : ''} ${selected === item.index ? 'selected' : ''}`} style={{ left: `${((item.start - timeline.start) / span) * 100}%`, width: `${Math.max(1.2, ((item.end - item.start) / span) * 100)}%` }} title={item.label || item.kind} aria-label={`${item.kind} ${item.label || item.index}`} onClick={() => {
        setSelected(item.index)
        const seq = sourceSeqByIndex.get(item.index)
        if (seq !== undefined) onSelectSeq(seq)
      }} />)}
    </div>
    <div className="timeline-legend"><span><i className="lane-dot lane-0" />Model</span><span><i className="lane-dot lane-1" />Assistant</span><span><i className="lane-dot lane-2" />Tools</span>{selected !== null && <span className="timeline-selected">Record #{selected}</span>}</div>
  </section>
}

function conversationNodeLabel(node: DshConversationNode): string {
  switch (node.kind) {
    case 'user': return 'User message'
    case 'assistant': return 'Assistant message'
    case 'tool-running': return 'Tool running'
    case 'tool-result': return node.isError ? 'Tool error' : 'Tool result'
    case 'context': return `Context · ${node.source}`
    case 'compaction': return 'Compaction'
    case 'unknown': return node.type
  }
}

function DshConversation({ events, sessionId, store, feedbackBySeq, producedBySeq, onFeedback, onCopy, onRetry, onFork, onOpenFile, onReachTop, loadingOlder }: {
  events: readonly EventView[]
  sessionId: string
  store: WebStore
  feedbackBySeq: Readonly<Record<number, FeedbackView>>
  producedBySeq: ReadonlyMap<number, readonly string[]>
  onFeedback: (seq: number, rating: 'positive' | 'negative') => void
  onCopy?: (text: string) => void
  onRetry?: (text: string) => void
  onFork?: () => void
  onOpenFile?: (path: string) => void
  onReachTop: () => void
  loadingOlder: boolean
}) {
  const snapshot = useMemo(() => projectDshConversation(events, sessionId), [events, sessionId])
  const eventBySeq = useMemo(() => new Map(events.map(event => [event.seq, event])), [events])
  const scrollRef = useRef<HTMLDivElement>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportHeight, setViewportHeight] = useState(640)
  const overscan = 6
  const rowKeys = useMemo(() => snapshot.nodes.map(node => `${node.kind}:${node.seq}`), [snapshot.nodes])
  const { offsets, measureRow } = useMeasuredVirtualRows(rowKeys, CONVERSATION_ROW_HEIGHT_ESTIMATE, scrollRef)
  const { start, end } = virtualRange(offsets, scrollTop, viewportHeight, overscan)
  const visible = snapshot.nodes.slice(start, end)

  useEffect(() => {
    const onResize = () => setViewportHeight(Math.max(320, window.innerHeight - 230))
    onResize()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  return <div ref={scrollRef} className="dsh-conversation-scroll" onScroll={event => {
    const top = event.currentTarget.scrollTop
    setScrollTop(top)
    if (top < 100) onReachTop()
  }}>
    <DshConversationHeader snapshot={snapshot} />
    {loadingOlder && <div className="history-loading">Loading earlier events…</div>}
    <div className="dsh-conversation-canvas" style={{ height: offsets[offsets.length - 1] ?? 0 }}>
      {visible.map((node, index) => {
        const raw = eventBySeq.get(node.seq)
        if (raw === undefined) return null
        const key = `${node.kind}:${node.seq}`
        return <section className={`conversation-node conversation-virtual-row ${node.kind}`} data-virtual-row-key={key} key={key} ref={element => measureRow(key, element)} style={{ transform: `translateY(${offsets[start + index] ?? 0}px)` }}>
          <div className="conversation-node-head"><span>{conversationNodeLabel(node)}</span><span>#{node.seq}</span></div>
          <EventCard event={raw} store={store} sessionId={sessionId} feedback={feedbackBySeq[raw.seq]} producedPaths={producedBySeq.get(raw.seq)} onFeedback={onFeedback} onCopy={onCopy} onRetry={onRetry} onFork={onFork} onOpenFile={onOpenFile} />
        </section>
      })}
    </div>
  </div>
}

function DshConversationHeader({ snapshot }: { snapshot: DshConversationSnapshot }) {
  const activeTools = snapshot.runningCalls.length
  return <div className="dsh-conversation-header" aria-label="DSH conversation snapshot">
    <strong>Conversation</strong>
    <span>{snapshot.nodes.length} nodes</span>
    <span>{snapshot.chat.timeline.turnOrder.length} turns</span>
    {activeTools > 0 && <span className="conversation-running">{activeTools} tool{activeTools === 1 ? '' : 's'} running</span>}
  </div>
}

function VirtualEvents({ events, store, sessionId, feedbackBySeq, producedBySeq, onFeedback, onCopy, onRetry, onFork, onOpenFile, onReachTop, loadingOlder, focusSeq }: {
  events: readonly EventView[]
  store: WebStore
  sessionId: string
  feedbackBySeq: Readonly<Record<number, FeedbackView>>
  producedBySeq: ReadonlyMap<number, readonly string[]>
  onFeedback: (seq: number, rating: 'positive' | 'negative') => void
  onCopy?: (text: string) => void
  onRetry?: (text: string) => void
  onFork?: () => void
  onOpenFile?: (path: string) => void
  onReachTop: () => void
  loadingOlder: boolean
  focusSeq: number | null
}) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const [collapsed, setCollapsed] = useState(false)
  const displayEvents = useMemo(() => collapsed ? collapseDshTrajectoryTurns(events) : events, [collapsed, events])
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportHeight, setViewportHeight] = useState(640)
  const overscan = 8
  const rowKeys = useMemo(() => displayEvents.map(event => String(event.seq)), [displayEvents])
  const { offsets, measureRow } = useMeasuredVirtualRows(rowKeys, ROW_HEIGHT_ESTIMATE, scrollRef)
  const { start, end } = virtualRange(offsets, scrollTop, viewportHeight, overscan)
  const visible = displayEvents.slice(start, end)

  useEffect(() => {
    const onResize = () => setViewportHeight(Math.max(320, window.innerHeight - 230))
    onResize()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  useEffect(() => {
    if (focusSeq === null) return
    const exactIndex = displayEvents.findIndex(event => event.seq === focusSeq)
    const index = exactIndex >= 0 ? exactIndex : displayEvents.findIndex(event => event.seq > focusSeq)
    if (index < 0 || scrollRef.current === null) return
    const nextTop = Math.max(0, (offsets[index] ?? 0) - viewportHeight / 3)
    scrollRef.current.scrollTo({ top: nextTop, behavior: 'smooth' })
    setScrollTop(nextTop)
  }, [displayEvents, focusSeq, offsets, viewportHeight])

  return <div ref={scrollRef} className="event-scroll" onScroll={event => {
    const top = event.currentTarget.scrollTop
    setScrollTop(top)
    if (top < 100) onReachTop()
  }}>
    <div className="trajectory-toolbar"><button type="button" className="text-button" onClick={() => setCollapsed(value => !value)} aria-label={collapsed ? 'Expand turns' : 'Collapse turns'}>{collapsed ? 'Expand turns' : 'Collapse turns'}</button><span>{collapsed ? `${displayEvents.length} compact records` : `${displayEvents.length} records`}</span></div>
    {loadingOlder && <div className="history-loading">Loading earlier events…</div>}
    <div className="virtual-canvas" style={{ height: offsets[offsets.length - 1] ?? 0 }}>
      {visible.map((event, index) => {
        const key = String(event.seq)
        return <div className="virtual-row" data-virtual-row-key={key} key={key} ref={element => measureRow(key, element)} style={{ transform: `translateY(${offsets[start + index] ?? 0}px)` }}>
        <EventCard event={event} store={store} sessionId={sessionId} feedback={feedbackBySeq[event.seq]} producedPaths={producedBySeq.get(event.seq)} onFeedback={onFeedback} onCopy={onCopy} onRetry={onRetry} onFork={onFork} onOpenFile={onOpenFile} />
      </div>
      })}
    </div>
  </div>
}

function runningJobLabel(status: string | undefined): string {
  switch (status?.toLowerCase()) {
    case 'running': case 'ongoing': return '运行中'
    case 'queued': case 'pending': return '排队中'
    case 'completed': case 'done': case 'success': return '已完成'
    case 'failed': case 'error': return '失败'
    case 'cancelled': case 'canceled': return '已取消'
    default: return status || '未知状态'
  }
}

function runningJobTone(status: string | undefined): 'running' | 'done' | 'danger' | 'idle' {
  switch (status?.toLowerCase()) {
    case 'running': case 'ongoing': return 'running'
    case 'completed': case 'done': case 'success': return 'done'
    case 'failed': case 'error': return 'danger'
    default: return 'idle'
  }
}

function RunningSummary({ data }: { data: RunningSnapshot }) {
  const runningSubagents = data.subagents.filter(item => item.running).length
  const runningJobs = data.jobs.filter(item => ['running', 'ongoing'].includes(item.status?.toLowerCase() ?? '')).length
  return <div className="running-summary" aria-label="运行状态摘要">
    <div className="running-metric"><span>活动子代理</span><strong>{runningSubagents}</strong><small>/{data.subagents.length} 个</small></div>
    <div className="running-metric"><span>运行中任务</span><strong>{runningJobs}</strong><small>/{data.jobs.length} 个</small></div>
    <div className="running-metric"><span>总活动项</span><strong>{runningSubagents + runningJobs}</strong><small>实时同步</small></div>
  </div>
}

function SubagentList({ items }: { items: readonly SubagentView[] }) {
  return <section className="running-section"><div className="running-section-head"><div><h3>子代理</h3><span>当前会话的代理树</span></div><strong>{items.length}</strong></div>{items.length === 0 ? <div className="running-empty">当前没有活动子代理。</div> : <div className="running-list">{items.map(item => <article className="running-row" key={item.id}><span className={`running-state-dot ${item.running ? 'active' : ''}`} /><div className="running-row-copy"><strong>{item.label || item.id}</strong><span>{item.id}</span></div><span className={`running-pill ${item.running ? 'running' : 'idle'}`}>{item.running ? '运行中' : '已结束'}</span></article>)}</div>}</section>
}

function JobList({ items }: { items: readonly JobView[] }) {
  return <section className="running-section"><div className="running-section-head"><div><h3>后台任务</h3><span>工作流与异步执行</span></div><strong>{items.length}</strong></div>{items.length === 0 ? <div className="running-empty">当前没有后台任务。</div> : <div className="running-list">{items.map(item => { const tone = runningJobTone(item.status); return <article className="running-row" key={item.id}><span className={`running-state-dot ${tone}`} /><div className="running-row-copy"><strong>{item.label || item.kind || item.id}</strong><span>{item.kind || item.id}{item.detail ? ` · ${item.detail}` : ''}</span></div><span className={`running-pill ${tone}`}>{runningJobLabel(item.status)}</span>{item.started_at && <time>{relativeTime(item.started_at)}</time>}</article> })}</div>}</section>
}

function RunningPanel({ store, sessionId }: { store: WebStore; sessionId: string | null }) {
  const [data, setData] = useState<RunningSnapshot | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)

  useEffect(() => {
    if (sessionId === null) {
      setData(null)
      setLoading(false)
      setError(null)
      return
    }
    const abort = new AbortController()
    let firstLoad = true
    const refresh = async (): Promise<void> => {
      if (firstLoad) setLoading(true)
      try {
        const next = await store.loadRunning(sessionId, abort.signal)
        if (abort.signal.aborted) return
        setData(next)
        setError(null)
        setLastUpdated(new Date())
      } catch (reason) {
        if (!abort.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason))
      } finally {
        if (!abort.signal.aborted) {
          setLoading(false)
          firstLoad = false
        }
      }
    }
    void refresh()
    const timer = setInterval(() => { void refresh() }, 5000)
    return () => { abort.abort(); clearInterval(timer) }
  }, [refreshKey, sessionId, store])

  if (sessionId === null) return <div className="running-panel"><div className="running-empty-state"><strong>选择一个会话</strong><span>运行中面板会显示该会话的子代理和后台任务。</span></div></div>
  if (loading && data === null) return <div className="running-panel"><div className="running-empty-state"><div className="spinner" /><span>正在加载运行状态…</span></div></div>
  if (error && data === null) return <div className="running-panel"><div className="running-empty-state running-error"><strong>运行状态读取失败</strong><span>{error}</span><button type="button" onClick={() => setRefreshKey(value => value + 1)}>重试</button></div></div>
  const snapshot = data ?? { subagents: [], jobs: [] }
  return <div className="running-panel">
    <div className="running-panel-head"><div><span className="eyebrow">LIVE STATUS</span><h2>运行中</h2><p>自动刷新当前会话的子代理和后台任务。</p></div><div className="running-actions"><span className="running-refresh">{lastUpdated ? `刚刚更新 · 每 5 秒` : '等待更新'}</span><button type="button" onClick={() => setRefreshKey(value => value + 1)} aria-label="刷新运行状态">刷新</button></div></div>
    {error && <div className="running-inline-error" role="status">本次刷新失败：{error}</div>}
    <RunningSummary data={snapshot} />
    <div className="running-columns"><SubagentList items={snapshot.subagents} /><JobList items={snapshot.jobs} /></div>
  </div>
}

const DEFAULT_COMMANDS: CommandView[] = [
  { name: 'help', hint: 'Show available slash commands', kind: 'command' },
  { name: 'status', hint: 'Show provider, model and mode', kind: 'command' },
  { name: 'compact', hint: 'Compact context', kind: 'command' },
  { name: 'permission', hint: 'Show or set permission', kind: 'command' },
  { name: 'feedback', hint: 'Record feedback', kind: 'command' },
  { name: 'goal', hint: 'Manage the goal', kind: 'command' },
  { name: 'plan', hint: 'Plan mode', kind: 'command' },
  { name: 'export', hint: 'Download session log', kind: 'command' },
]

function CommandMenu({ commands, query, activeIndex, onSelect }: { commands: readonly CommandView[]; query: string; activeIndex: number; onSelect: (command: CommandView) => void }) {
  if (query === '' && activeIndex < 0) return null
  const normalized = query.toLocaleLowerCase()
  const items = commands.filter(command => command.name.toLocaleLowerCase().includes(normalized)).slice(0, 8)
  if (items.length === 0) return null
  return <div className="command-menu" role="listbox" aria-label="Slash commands">
    {items.map((command, index) => <button type="button" role="option" aria-selected={index === activeIndex} className={index === activeIndex ? 'active' : ''} key={`${command.kind ?? 'command'}:${command.name}`} onMouseDown={event => event.preventDefault()} onClick={() => onSelect(command)}><strong>/{command.name}</strong><span>{command.hint ?? command.kind ?? ''}</span></button>)}
  </div>
}

function QueuePanel({ store, sessionId, active, onError }: { store: WebStore; sessionId: string | null; active: boolean; onError: (error: unknown) => void }) {
  const [items, setItems] = useState<QueueItem[]>([])
  const [busy, setBusy] = useState<string | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  useEffect(() => {
    if (sessionId === null) { setItems([]); return }
    const abort = new AbortController()
    const refresh = async () => {
      try { const next = await store.listQueue(sessionId, abort.signal); if (!abort.signal.aborted) setItems(next) }
      catch (error) { if (!abort.signal.aborted) onError(error) }
    }
    void refresh()
    const timer = window.setInterval(() => { void refresh() }, active ? 1500 : 5000)
    return () => { abort.abort(); window.clearInterval(timer) }
  }, [active, onError, refreshKey, sessionId, store])
  const update = async (item: QueueItem, action: 'move_first' | 'delete' | 'steer') => {
    if (sessionId === null) return
    setBusy(`${item.id}:${action}`)
    try { await store.updateQueue(sessionId, item.id, action); setRefreshKey(value => value + 1) }
    catch (error) { onError(error) }
    finally { setBusy(null) }
  }
  if (sessionId === null || items.length === 0) return null
  return <section className="queue-panel" aria-label="Queued messages"><div className="queue-head"><strong>Queue</strong><span>{items.length} waiting</span></div><div className="queue-list">{items.map(item => <article className="queue-item" key={item.id}><div><span>{item.text}</span><small>{item.placement || 'queued'}{item.created_at ? ` · ${relativeTime(item.created_at)}` : ''}</small></div><div className="queue-actions"><button type="button" disabled={busy !== null} onClick={() => void update(item, 'move_first')}>First</button><button type="button" disabled={busy !== null} onClick={() => void update(item, 'steer')}>Steer</button><button type="button" disabled={busy !== null} onClick={() => void update(item, 'delete')}>Remove</button></div></article>)}</div></section>
}

function InteractionPanel({ store, sessionId, onError }: { store: WebStore; sessionId: string | null; onError: (error: unknown) => void }) {
  const [items, setItems] = useState<InteractionView[]>([])
  const [busy, setBusy] = useState<string | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  useEffect(() => {
    if (sessionId === null) { setItems([]); return }
    const abort = new AbortController()
    const refresh = async () => {
      try { const next = await store.listInteractions(sessionId, abort.signal); if (!abort.signal.aborted) setItems(next.filter(item => !['approved', 'rejected', 'canceled', 'cancelled'].includes(item.status.toLowerCase()))) }
      catch (error) { if (!abort.signal.aborted) onError(error) }
    }
    void refresh()
    const timer = window.setInterval(() => { void refresh() }, 1500)
    return () => { abort.abort(); window.clearInterval(timer) }
  }, [onError, refreshKey, sessionId, store])
  const resolve = async (item: InteractionView, status: 'approved' | 'rejected' | 'canceled', answer = '') => {
    if (sessionId === null) return
    setBusy(item.id)
    try { await store.resolveInteraction(sessionId, item.id, status, answer); setRefreshKey(value => value + 1) }
    catch (error) { onError(error) }
    finally { setBusy(null) }
  }
  if (sessionId === null || items.length === 0) return null
  return <section className="interaction-panel" aria-label="Approval requests"><div className="interaction-head"><strong>Approval required</strong><span>{items.length} pending</span></div>{items.map(item => <article className="interaction-card" key={item.id}><div className="interaction-copy"><strong>{item.tool_name || 'Agent request'}</strong><p>{item.prompt}</p>{item.args && <pre>{item.args}</pre>}</div>{item.questions?.map(question => <div className="interaction-question" key={question.id ?? question.question}><span>{question.question}</span><div>{question.options?.map(option => <button type="button" key={option.label} disabled={busy !== null} onClick={() => void resolve(item, 'approved', option.label)}>{option.label}</button>)}</div></div>)}<div className="interaction-actions"><button type="button" disabled={busy !== null} onClick={() => void resolve(item, 'approved')}>Approve</button><button type="button" disabled={busy !== null} onClick={() => void resolve(item, 'rejected')}>Reject</button><button type="button" disabled={busy !== null} onClick={() => void resolve(item, 'canceled')}>Cancel</button></div></article>)}</section>
}

function formatTokenCount(value: number): string {
  if (!Number.isFinite(value)) return '—'
  if (value >= 1000000) return `${(value / 1000000).toFixed(1)}M`
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k`
  return String(Math.round(value))
}

function goalField(goal: GoalView, lower: keyof GoalView, upper: keyof GoalView): unknown {
  return goal[lower] ?? goal[upper]
}

function planField(item: PlanView | TodoView, lower: string, upper: string): unknown {
  const value = item as Record<string, unknown>
  return value[lower] ?? value[upper]
}

function planStatus(status: unknown): string {
  return String(status ?? 'pending').toLowerCase()
}

function PlanDetails({ goal, plans }: { goal?: GoalView; plans: readonly PlanView[] }) {
  if (plans.length === 0) return null
  const rawGoalPlanIds = goal === undefined ? undefined : goalField(goal, 'plans', 'Plans')
  const goalPlanIds = new Set(Array.isArray(rawGoalPlanIds) ? rawGoalPlanIds.map(String) : [])
  const visible = goalPlanIds.size === 0 ? plans : plans.filter(plan => goalPlanIds.has(String(planField(plan, 'id', 'ID'))))
  if (visible.length === 0) return null
  return <details className="plan-details" open>
    <summary>Plan details <span>{visible.length}</span></summary>
    <div className="plan-tree">
      {visible.map((plan, planIndex) => {
        const planId = String(planField(plan, 'id', 'ID') ?? `plan-${planIndex + 1}`)
        const title = String(planField(plan, 'title', 'Title') ?? planId)
        const status = planStatus(planField(plan, 'status', 'Status'))
        const rawSteps = planField(plan, 'steps', 'Steps')
        const steps = Array.isArray(rawSteps) ? rawSteps as TodoView[] : []
        return <article className="plan-card" key={planId}>
          <div className="plan-card-head"><span className={`plan-status ${status}`}>{status}</span><strong title={title}>{title}</strong><small>{steps.length} step{steps.length === 1 ? '' : 's'}</small></div>
          {steps.length > 0 && <ol className="plan-step-list">{steps.map((step, stepIndex) => {
            const stepId = String(planField(step, 'id', 'ID') ?? `${planId}-${stepIndex + 1}`)
            const stepTitle = String(planField(step, 'title', 'Title') ?? stepId)
            const stepStatus = planStatus(planField(step, 'status', 'Status'))
            const details = planField(step, 'details', 'Details')
            return <li className={`plan-step ${stepStatus}`} key={stepId}><span className="plan-step-marker" aria-hidden="true">{stepStatus === 'done' ? '✓' : stepIndex + 1}</span><div><strong title={stepTitle}>{stepTitle}</strong>{typeof details === 'string' && details !== '' && <small>{details}</small>}</div><span className="plan-step-status">{stepStatus}</span></li>
          })}</ol>}
        </article>
      })}
    </div>
  </details>
}

function GoalBar({ store, sessionId, sessionState, onUpdated, onError }: { store: WebStore; sessionId: string | null; sessionState: SessionStateView | null; onUpdated: () => void; onError: (error: unknown) => void }) {
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [error, setError] = useState<string | null>(null)
  const goals = sessionState?.goals ?? []
  const goal = goals.find(item => !['complete', 'done'].includes(String(goalField(item, 'status', 'Status') ?? '').toLowerCase())) ?? goals[0]
  const objective = goal === undefined ? '' : String(goalField(goal, 'objective', 'Objective') ?? goalField(goal, 'title', 'Title') ?? '')
  const status = goal === undefined ? '' : String(goalField(goal, 'status', 'Status') ?? 'active').toLowerCase()
  const plans = goal === undefined ? [] : (goalField(goal, 'plans', 'Plans') as string[] | undefined) ?? []
  const planRecords = sessionState?.plans ?? []

  const run = async (command: string): Promise<void> => {
    if (sessionId === null || command.trim() === '') return
    setBusy(true); setError(null)
    try { await store.send(command); setEditing(false); onUpdated() }
    catch (reason) { const message = reason instanceof Error ? reason.message : String(reason); setError(message); onError(reason) }
    finally { setBusy(false) }
  }

  if (sessionId === null || sessionState === null || (!sessionState.plan_mode && goal === undefined)) return null
  return <div className="goal-bar" aria-label="Goal and plan controls">
    {sessionState.plan_mode && <button type="button" className="plan-chip" disabled={busy} onClick={() => void run('/plan off')} title="Turn off plan mode">Plan <span>×</span></button>}
    {goal !== undefined && <>
      <span className={`goal-phase ${status}`}>{status || 'active'}</span>
      {editing ? <><input aria-label="Goal objective" value={draft} onChange={event => setDraft(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') void run(`/goal edit ${draft.trim()}`); if (event.key === 'Escape') setEditing(false) }} autoFocus /><button type="button" disabled={busy || draft.trim() === ''} onClick={() => void run(`/goal edit ${draft.trim()}`)}>Save</button><button type="button" disabled={busy} onClick={() => setEditing(false)}>Cancel</button></> : <><span className="goal-objective" title={objective}>{objective}</span><span className="goal-count">{plans.length} plan{plans.length === 1 ? '' : 's'}</span><button type="button" disabled={busy} onClick={() => { setDraft(objective); setEditing(true) }}>Edit</button>{status === 'paused' ? <button type="button" disabled={busy} onClick={() => void run('/goal resume')}>Resume</button> : <button type="button" disabled={busy} onClick={() => void run('/goal pause')}>Pause</button>}<button type="button" disabled={busy} onClick={() => void run('/goal clear')}>Clear</button></>}
    </>}
    <PlanDetails goal={goal} plans={planRecords} />
    {error !== null && <span className="goal-error" role="status" title={error}>Action failed</span>}
  </div>
}

function SessionStatusBar({ store, sessionId, onError }: { store: WebStore; sessionId: string | null; onError: (error: unknown) => void }) {
  const [context, setContext] = useState<ContextView | null>(null)
  const [sessionState, setSessionState] = useState<SessionStateView | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)

  useEffect(() => {
    if (sessionId === null) { setContext(null); setSessionState(null); return }
    const abort = new AbortController()
    const refresh = async (): Promise<void> => {
      const [contextResult, stateResult] = await Promise.allSettled([store.getContext(sessionId, abort.signal), store.getSessionState(sessionId, abort.signal)])
      if (abort.signal.aborted) return
      if (contextResult.status === 'fulfilled') setContext(contextResult.value)
      else if (!(contextResult.reason instanceof ShutuApiError && contextResult.reason.status === 404)) onError(contextResult.reason)
      if (stateResult.status === 'fulfilled') setSessionState(stateResult.value)
      else if (!(stateResult.reason instanceof ShutuApiError && [404, 501].includes(stateResult.reason.status))) onError(stateResult.reason)
    }
    void refresh()
    const timer = window.setInterval(() => { void refresh() }, 5000)
    return () => { abort.abort(); window.clearInterval(timer) }
  }, [onError, refreshKey, sessionId, store])

  if (sessionId === null || (context === null && sessionState === null)) return null
  const percent = Math.max(0, Math.min(100, context?.percent ?? 0))
  const goals = (sessionState?.goals ?? []).length + (sessionState?.plans ?? []).length
  const memories = (sessionState?.memories ?? []).length
  return <div className="session-status-bar" aria-label="Session status">
    <GoalBar store={store} sessionId={sessionId} sessionState={sessionState} onUpdated={() => setRefreshKey(value => value + 1)} onError={onError} />
    {context !== null && <div className="context-meter" title={`${formatTokenCount(context.used_tokens)} / ${formatTokenCount(context.context_window)} tokens`}><span>Context</span><div className="context-meter-track"><div style={{ width: `${percent}%` }} /></div><small>{formatTokenCount(context.used_tokens)} / {formatTokenCount(context.context_window)} · {Math.round(percent)}%</small></div>}
    {sessionState !== null && <div className="session-state-badges"><span className={`session-badge ${sessionState.plan_mode ? 'active' : ''}`}>{sessionState.plan_mode ? 'Plan mode' : 'Normal mode'}</span>{sessionState.plan_enabled !== undefined && <span className="session-badge">Plan {sessionState.plan_enabled ? 'on' : 'off'}</span>}{goals > 0 && <span className="session-badge">{goals} plan item{goals === 1 ? '' : 's'}</span>}{sessionState.memory_enabled && <span className="session-badge">Memory {memories}</span>}</div>}
  </div>
}

function SessionControls({ store, sessionId, onError }: { store: WebStore; sessionId: string | null; onError: (error: unknown) => void }) {
  const [config, setConfig] = useState<ConfigView | null>(null)
  const [values, setValues] = useState({ provider: '', model: '', reasoning_effort: '', permission: '' })
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (sessionId === null) { setConfig(null); return }
    const abort = new AbortController()
    void Promise.all([store.getConfig(abort.signal), store.getSessionConfig(sessionId, abort.signal)]).then(([globalConfig, sessionConfig]) => {
      if (abort.signal.aborted) return
      setConfig(globalConfig)
      setValues({ provider: sessionConfig.provider ?? globalConfig.llm_provider ?? '', model: sessionConfig.model ?? globalConfig.model ?? '', reasoning_effort: sessionConfig.reasoning_effort ?? globalConfig.reasoning_effort ?? '', permission: sessionConfig.permission ?? '' })
    }).catch(error => { if (!abort.signal.aborted) onError(error) })
    return () => abort.abort()
  }, [onError, sessionId, store])
  const providers = config?.providers ?? []
  const provider = providers.find(item => item.id === values.provider)
  const models = provider?.models?.map(item => item.id) ?? provider?.candidates ?? []
  const update = async (next: Partial<typeof values>): Promise<void> => {
    if (sessionId === null) return
    const merged = { ...values, ...next }
    setValues(merged); setBusy(true)
    try { await store.updateSessionConfig(sessionId, merged) }
    catch (error) { onError(error) }
    finally { setBusy(false) }
  }
  if (sessionId === null || config === null) return null
  return <div className="session-controls" aria-label="Current session controls"><label><span>Model</span><select value={values.model} disabled={busy} onChange={event => void update({ model: event.target.value })}>{models.length > 0 ? models.map(item => <option key={item} value={item}>{item}</option>) : <option value={values.model}>{values.model || 'default'}</option>}</select></label><label><span>Reasoning</span><select value={values.reasoning_effort} disabled={busy} onChange={event => void update({ reasoning_effort: event.target.value })}><option value="">Default</option><option value="off">Off</option><option value="low">Low</option><option value="high">High</option><option value="max">Max</option></select></label><label><span>Permission</span><select value={values.permission} disabled={busy} onChange={event => void update({ permission: event.target.value })}><option value="">Default</option><option value="readonly">Read only</option><option value="standard">Standard</option><option value="full">Full</option></select></label></div>
}

function isConversationEvent(event: EventView): boolean {
  return event.type.startsWith('user/') || event.type.startsWith('assistant/') ||
    event.type.startsWith('tool/') || event.type.startsWith('interact/') ||
    event.type.startsWith('turn/') || event.type.startsWith('step/')
}

export function App({ store }: { store: WebStore }) {
  const state = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot)
  const [tab, setTab] = useState<'chat' | 'trajectory' | 'running'>('chat')
  const [draft, setDraft] = useState('')
  const [commands, setCommands] = useState<CommandView[]>(DEFAULT_COMMANDS)
  const [commandIndex, setCommandIndex] = useState(-1)
  const [search, setSearch] = useState('')
  const [sendError, setSendError] = useState<string | null>(null)
  const [focusedSeq, setFocusedSeq] = useState<number | null>(null)
  const [filesOpen, setFilesOpen] = useState(false)
  const [fileOpenPath, setFileOpenPath] = useState<string | null>(null)
  const [token, setToken] = useState(() => store.getToken())
  const [settingsOpen, setSettingsOpen] = useState(() => typeof window !== 'undefined' && window.location.hash === '#/settings')
  const [feedbackBySeq, setFeedbackBySeq] = useState<Record<number, FeedbackView>>({})
  const [pendingImages, setPendingImages] = useState<readonly { ref: AttachmentView; previewUrl: string }[]>([])
  const [uploading, setUploading] = useState(false)
  const [theme, setTheme] = useState<ThemePreference>(() => {
    if (typeof localStorage === 'undefined') return 'dark'
    try {
      const stored = localStorage.getItem('shutu.web.theme')
      return stored === 'light' || stored === 'system' ? stored : 'dark'
    } catch { return 'dark' }
  })
  const reportError = useCallback((error: unknown): void => { setSendError(error instanceof Error ? error.message : String(error)) }, [])
  const referenceFile = useCallback((path: string): void => {
    setDraft(previous => `${previous}${previous.trim() === '' ? '' : ' '}@${path}`)
    setFilesOpen(false)
    setFileOpenPath(null)
  }, [])
  const openFilePreview = useCallback((path: string): void => {
    setFileOpenPath(path)
    setFilesOpen(true)
  }, [])
  const selected = state.sessions.find(session => session.id === state.selectedId)
  const producedBySeq = useMemo(() => deriveProducedFiles(state.events), [state.events])
  const filtered = useMemo(() => {
    const query = search.trim().toLocaleLowerCase()
    return state.events.filter(event => {
      if (tab === 'chat' && !isConversationEvent(event)) return false
      if (query === '') return true
      return `${event.type} ${event.summary} ${event.reasoning ?? ''} ${event.tool_output ?? ''} ${event.tool_name ?? ''}`
        .toLocaleLowerCase().includes(query)
    })
  }, [search, state.events, tab])
  const commandQuery = draft.match(/^\/([^\s]*)$/)?.[1] ?? null
  const commandItems = useMemo(() => commandQuery === null ? [] : commands.filter(command => command.name.toLocaleLowerCase().includes(commandQuery.toLocaleLowerCase())).slice(0, 8), [commandQuery, commands])

  useEffect(() => { void store.start() }, [store])

  useEffect(() => {
    const abort = new AbortController()
    void store.getConfig(abort.signal).then(config => {
      if (!abort.signal.aborted && Array.isArray(config.commands) && config.commands.length > 0) setCommands(config.commands)
    }).catch(() => undefined)
    return () => abort.abort()
  }, [store])

  useEffect(() => {
    if (state.selectedId === null) {
      setFeedbackBySeq({})
      return
    }
    const abort = new AbortController()
    setFeedbackBySeq({})
    void store.listFeedback(state.selectedId, abort.signal).then(items => {
      if (abort.signal.aborted) return
      setFeedbackBySeq(Object.fromEntries(items.map(item => [item.seq, item])))
    }).catch(error => {
      if (!abort.signal.aborted) setSendError(error instanceof Error ? error.message : String(error))
    })
    return () => abort.abort()
  }, [state.selectedId, store])

  useEffect(() => { setFilesOpen(false) }, [state.selectedId])

  useEffect(() => {
    const syncRoute = () => setSettingsOpen(window.location.hash === '#/settings')
    window.addEventListener('hashchange', syncRoute)
    window.addEventListener('popstate', syncRoute)
    return () => { window.removeEventListener('hashchange', syncRoute); window.removeEventListener('popstate', syncRoute) }
  }, [])

  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: light)')
    const applyTheme = () => {
      const resolved = theme === 'system' ? (media.matches ? 'light' : 'dark') : theme
      document.body.dataset.theme = resolved
      document.documentElement.style.colorScheme = resolved
    }
    applyTheme()
    if (theme !== 'system') {
      try { localStorage.setItem('shutu.web.theme', theme) } catch { /* optional preference */ }
      return
    }
    media.addEventListener('change', applyTheme)
    try { localStorage.setItem('shutu.web.theme', theme) } catch { /* optional preference */ }
    return () => media.removeEventListener('change', applyTheme)
  }, [theme])

  const setSettingsRoute = (open: boolean): void => {
    setSettingsOpen(open)
    const hash = open ? '#/settings' : '#/'
    if (window.location.hash !== hash) window.location.hash = hash
  }

  const submit = async (): Promise<void> => {
    const value = draft.trim()
    if (!value && pendingImages.length === 0) return
    setDraft('')
    setSendError(null)
    try {
      if (state.sending) {
        if (pendingImages.length > 0) throw new Error('Queued messages currently support text only.')
        if (state.selectedId !== null) await store.enqueueQueue(state.selectedId, value)
      } else {
        await store.send(value, pendingImages.map(item => item.ref.id))
      }
      pendingImages.forEach(item => URL.revokeObjectURL(item.previewUrl))
      setPendingImages([])
    } catch (error) {
      setDraft(value)
      setSendError(error instanceof Error ? error.message : String(error))
    }
  }

  const copyMessage = async (text: string): Promise<void> => {
    try {
      await navigator.clipboard.writeText(text)
      setSendError(null)
    } catch (error) { setSendError(error instanceof Error ? error.message : String(error)) }
  }

  const retryMessage = async (text: string): Promise<void> => {
    setSendError(null)
    try { await store.send(text) }
    catch (error) { setSendError(error instanceof Error ? error.message : String(error)) }
  }

  const forkSession = async (): Promise<void> => {
    if (state.selectedId === null) return
    setSendError(null)
    try { await store.forkSession(state.selectedId) }
    catch (error) { setSendError(error instanceof Error ? error.message : String(error)) }
  }

  const downloadExport = useCallback(async (): Promise<void> => {
    if (state.selectedId === null) return
    setSendError(null)
    try {
      const blob = await store.exportSession(state.selectedId)
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `shutu-session-${state.selectedId.replace(/[^A-Za-z0-9_-]/g, '_')}.zip`
      document.body.appendChild(link)
      link.click()
      link.remove()
      URL.revokeObjectURL(url)
    } catch (error) { setSendError(error instanceof Error ? error.message : String(error)) }
  }, [state.selectedId, store])

  const exportSeenSession = useRef<string | null>(null)
  const exportSeenSeq = useRef(0)
  useEffect(() => {
    if (state.selectedId === null) {
      exportSeenSession.current = null
      exportSeenSeq.current = 0
      return
    }
    if (exportSeenSession.current !== state.selectedId) {
      exportSeenSession.current = state.selectedId
      exportSeenSeq.current = state.events.at(-1)?.seq ?? 0
      return
    }
    const latest = state.events.at(-1)
    if (latest === undefined || latest.seq <= exportSeenSeq.current) return
    exportSeenSeq.current = latest.seq
    if (latest.type === 'web/command-result' && latest.command === 'export') void downloadExport()
  }, [downloadExport, state.events, state.selectedId])

  const selectCommand = (command: CommandView): void => {
    setDraft(`/${command.name} `)
    setCommandIndex(-1)
  }

  const handleDraftChange = (value: string): void => {
    setDraft(value)
    setCommandIndex(/^\/[^\s]*$/.test(value) ? 0 : -1)
  }

  const handleComposerKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>): void => {
    if (commandQuery !== null && commandItems.length > 0) {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault()
        setCommandIndex(previous => (previous + (event.key === 'ArrowDown' ? 1 : commandItems.length - 1)) % commandItems.length)
        return
      }
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault()
        selectCommand(commandItems[commandIndex] ?? commandItems[0])
        return
      }
      if (event.key === 'Escape') {
        event.preventDefault()
        setDraft('')
        return
      }
    }
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      if (!state.sending) void submit()
    }
  }

  const attachFiles = async (files: FileList | null): Promise<void> => {
    if (state.selectedId === null || files === null) return
    const candidates = Array.from(files).slice(0, 4)
    const invalid = candidates.find(file => !file.type.startsWith('image/') || file.size > 10 * 1024 * 1024)
    if (invalid !== undefined) {
      setSendError('仅支持 10MB 以内的图片附件。')
      return
    }
    setUploading(true)
    setSendError(null)
    try {
      const uploaded = await Promise.all(candidates.map(async file => ({
        ref: await store.uploadAttachment(state.selectedId as string, file),
        previewUrl: URL.createObjectURL(file),
      })))
      setPendingImages(previous => [...previous, ...uploaded].slice(0, 10))
    } catch (error) {
      setSendError(error instanceof Error ? error.message : String(error))
    } finally { setUploading(false) }
  }

  const removePendingImage = (id: string): void => {
    const item = pendingImages.find(candidate => candidate.ref.id === id)
    if (item !== undefined) URL.revokeObjectURL(item.previewUrl)
    setPendingImages(previous => previous.filter(candidate => candidate.ref.id !== id))
  }

  const submitFeedback = async (seq: number, rating: 'positive' | 'negative'): Promise<void> => {
    if (state.selectedId === null) return
    const current = feedbackBySeq[seq]
    try {
      if (current?.rating === rating) {
        await store.deleteFeedback(state.selectedId, seq)
        setFeedbackBySeq(previous => { const next = { ...previous }; delete next[seq]; return next })
      } else {
        const item = await store.putFeedback(state.selectedId, seq, rating)
        setFeedbackBySeq(previous => ({ ...previous, [seq]: item }))
      }
    } catch (error) { setSendError(error instanceof Error ? error.message : String(error)) }
  }

  const stopRun = async (): Promise<void> => {
    setSendError(null)
    try {
      if (state.sending && state.selectedId !== null && draft.trim() !== '') {
        if (pendingImages.length > 0) throw new Error('Queued messages currently support text only.')
        await store.enqueueQueue(state.selectedId, draft.trim())
        setDraft('')
        return
      }
      await store.stop()
    } catch (error) {
      setSendError(error instanceof Error ? error.message : String(error))
    }
  }

  const authenticate = async (): Promise<void> => {
    const value = token.trim()
    if (!value) return
    localStorage.setItem('shutu.web.token', value)
    try { await store.authenticate(value) } catch (error) {
      setSendError(error instanceof Error ? error.message : String(error))
    }
  }

  if (settingsOpen) return <SettingsPage store={store} theme={theme} onThemeChange={setTheme} onBack={() => setSettingsRoute(false)} />

  return <div className="shell">
    <SessionBrowser sessions={state.sessions} workspaces={state.workspaces} selectedId={state.selectedId} store={store} onError={error => setSendError(error instanceof Error ? error.message : String(error))} onSettings={() => setSettingsRoute(true)} />
    <main className="main-panel">
      <header className="topbar">
        <button type="button" className="export-toggle" onClick={() => void downloadExport()} disabled={state.selectedId === null}>Export</button>
        <div><h1>{selected?.title || (state.selectedId ? state.selectedId : 'Conversation')}</h1><div className="status-line"><span className={state.connected ? 'status-dot online' : 'status-dot'} />{state.connected ? 'Live' : 'Reconnecting'}</div></div>
        <div className="topbar-actions"><button type="button" className="files-toggle" onClick={() => { setFileOpenPath(null); setFilesOpen(value => !value) }} disabled={state.selectedId === null} aria-pressed={filesOpen}>Files</button><label className="search-box"><span>⌕</span><input aria-label="Search trajectory" placeholder="Search events" value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" onClick={() => setSearch('')} aria-label="Clear search">×</button>}</label></div>
      </header>
      <nav className="tabs" role="tablist"><button role="tab" aria-selected={tab === 'chat'} className={tab === 'chat' ? 'tab selected' : 'tab'} onClick={() => setTab('chat')}>Conversation</button><button role="tab" aria-selected={tab === 'trajectory'} className={tab === 'trajectory' ? 'tab selected' : 'tab'} onClick={() => setTab('trajectory')}>Trajectory <span>{state.events.length}</span></button><button role="tab" aria-selected={tab === 'running'} className={tab === 'running' ? 'tab selected' : 'tab'} onClick={() => setTab('running')}>运行中</button></nav>
      <SessionControls store={store} sessionId={state.selectedId} onError={reportError} />
      <SessionStatusBar store={store} sessionId={state.selectedId} onError={reportError} />
      {search.trim() !== '' && <div className="search-status" role="status" aria-live="polite">{filtered.length} matching loaded events{state.hasOlder ? ' · scroll to load older history' : ''}</div>}
      {(state.error || sendError) && <div className="error-banner"><span>{state.error || sendError}</span><button onClick={() => { setSendError(null); void store.start() }}>Retry</button></div>}
      <InteractionPanel store={store} sessionId={state.selectedId} onError={reportError} />
      <QueuePanel store={store} sessionId={state.selectedId} active={state.sending} onError={reportError} />
      <section className="content-panel">
        {filesOpen && state.selectedId !== null ? <FilesPanel store={store} sessionId={state.selectedId} openPath={fileOpenPath} onClose={() => { setFilesOpen(false); setFileOpenPath(null) }} onReference={referenceFile} /> : state.authRequired ? <form className="auth-card" onSubmit={event => { event.preventDefault(); void authenticate() }}><strong>Authentication required</strong><span>Enter the bearer token configured for the Shutu web server.</span><input aria-label="Bearer token" type="password" autoComplete="current-password" value={token} onChange={event => setToken(event.target.value)} placeholder="Bearer token" /><button type="submit" disabled={token.trim() === ''}>Connect</button></form> : state.loading ? <div className="empty"><div className="spinner" />Loading session…</div> : tab === 'running' ? <RunningPanel store={store} sessionId={state.selectedId} /> : state.selectedId === null ? <div className="empty"><strong>Start a new conversation</strong><span>Select a session or send a message from the agent.</span></div> : filtered.length === 0 ? <div className="empty"><strong>{search ? 'No matching events' : 'No events yet'}</strong><span>{search ? 'Try a different search term.' : 'Events will appear here as the session runs.'}</span></div> : tab === 'trajectory' ? <><DshTimeline events={filtered} onSelectSeq={setFocusedSeq} /><VirtualEvents events={filtered} store={store} sessionId={state.selectedId} feedbackBySeq={feedbackBySeq} producedBySeq={producedBySeq} onFeedback={submitFeedback} onOpenFile={openFilePreview} focusSeq={focusedSeq} onReachTop={() => void store.loadOlder()} loadingOlder={state.loadingOlder} /></> : <DshConversation events={filtered} sessionId={state.selectedId} store={store} feedbackBySeq={feedbackBySeq} producedBySeq={producedBySeq} onFeedback={submitFeedback} onOpenFile={openFilePreview} onReachTop={() => void store.loadOlder()} loadingOlder={state.loadingOlder} />}
      </section>
      <form className="composer" onSubmit={event => { event.preventDefault(); if (state.sending && draft.trim() === '' && pendingImages.length === 0) void stopRun(); else void submit() }}><CommandMenu commands={commands} query={commandQuery ?? ''} activeIndex={commandIndex} onSelect={selectCommand} />{pendingImages.length > 0 && <div className="attachment-preview-list">{pendingImages.map(item => <div className="attachment-preview" key={item.ref.id}><img src={item.previewUrl} alt="待发送图片" /><button type="button" onClick={() => removePendingImage(item.ref.id)} aria-label="移除附件">×</button></div>)}</div>}<div className="composer-row"><label className="attach-button" aria-label="添加图片"><input type="file" accept="image/*" multiple disabled={state.selectedId === null || state.sending || uploading} onChange={event => { void attachFiles(event.currentTarget.files); event.currentTarget.value = '' }} />📎</label><textarea value={draft} onChange={event => handleDraftChange(event.target.value)} onKeyDown={handleComposerKeyDown} placeholder={uploading ? '正在上传图片…' : state.sending ? 'Agent is running…' : 'Send a message…'} rows={2} /><button type="submit" disabled={state.selectedId === null || uploading || (!state.sending && draft.trim() === '' && pendingImages.length === 0)}>{state.sending ? (draft.trim() ? 'Queue' : 'Stop') : 'Send'} <span>{state.sending ? '■' : '↵'}</span></button></div></form>
    </main>
  </div>
}
