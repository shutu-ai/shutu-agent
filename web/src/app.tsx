import { useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import type { EventDetails, EventView } from './api'
import { projectDshConversation, type DshConversationNode, type DshConversationSnapshot } from './dsh-conversation'
import { projectDshTrajectory, type DshTimelineMode } from './dsh-trajectory'
import { WebStore } from './store'
import './styles.css'

const ROW_HEIGHT = 132

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

function EventCard({ event }: { event: EventView }) {
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
        <div className="event-text">{text}</div>
        {event.tool_args && <div className="code-block"><span>Input</span><pre>{event.tool_args}</pre></div>}
        {tokenUsage !== undefined && <span className="token-chip">Tokens {detailText(tokenUsage)}</span>}
        {hasExtra && <button className="text-button" onClick={() => setExpanded(value => !value)}>{expanded ? 'Collapse details' : 'Expand details'}</button>}
        {expanded && entries.length > 0 && <div className="details-grid">
          {entries.map(([key, value]) => <div className="detail-item" key={key}><span>{key}</span><pre>{value}</pre></div>)}
        </div>}
      </div>
    </article>
  )
}

function DshTimeline({ events }: { events: readonly EventView[] }) {
  const [mode, setMode] = useState<DshTimelineMode>('sequence')
  const [selected, setSelected] = useState<number | null>(null)
  const { timeline } = useMemo(() => projectDshTrajectory(events, mode), [events, mode])
  if (timeline === null) return null
  const span = Math.max(1, timeline.end - timeline.start)
  return <section className="dsh-timeline" aria-label="Trajectory timeline">
    <div className="timeline-head"><div><strong>Timeline</strong><span>{timeline.spans.length} records</span></div><select aria-label="Timeline mode" value={mode} onChange={event => { setMode(event.target.value as DshTimelineMode); setSelected(null) }}><option value="sequence">Sequence</option><option value="duration">Duration</option><option value="time">Recorded time</option><option value="actual">Actual time</option></select></div>
    <div className="timeline-track">
      {timeline.spans.map(item => <button key={`${item.index}-${item.start}`} className={`timeline-span lane-${item.lane} ${item.isError ? 'error' : ''} ${selected === item.index ? 'selected' : ''}`} style={{ left: `${((item.start - timeline.start) / span) * 100}%`, width: `${Math.max(1.2, ((item.end - item.start) / span) * 100)}%` }} title={item.label || item.kind} aria-label={`${item.kind} ${item.label || item.index}`} onClick={() => setSelected(item.index)} />)}
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

function DshConversation({ events, sessionId, onReachTop, loadingOlder }: {
  events: readonly EventView[]
  sessionId: string
  onReachTop: () => void
  loadingOlder: boolean
}) {
  const snapshot = useMemo(() => projectDshConversation(events, sessionId), [events, sessionId])
  const eventBySeq = useMemo(() => new Map(events.map(event => [event.seq, event])), [events])
  return <div className="dsh-conversation-scroll" onScroll={event => {
    if (event.currentTarget.scrollTop < 100) onReachTop()
  }}>
    <DshConversationHeader snapshot={snapshot} />
    {loadingOlder && <div className="history-loading">Loading earlier events…</div>}
    <div className="dsh-conversation-list">
      {snapshot.nodes.map(node => {
        const raw = eventBySeq.get(node.seq)
        if (raw === undefined) return null
        return <section className={`conversation-node ${node.kind}`} key={`${node.kind}:${node.seq}`}>
          <div className="conversation-node-head"><span>{conversationNodeLabel(node)}</span><span>#{node.seq}</span></div>
          <EventCard event={raw} />
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

function VirtualEvents({ events, onReachTop, loadingOlder }: {
  events: readonly EventView[]
  onReachTop: () => void
  loadingOlder: boolean
}) {
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportHeight, setViewportHeight] = useState(640)
  const overscan = 8
  const start = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - overscan)
  const end = Math.min(events.length, Math.ceil((scrollTop + viewportHeight) / ROW_HEIGHT) + overscan)
  const visible = events.slice(start, end)

  useEffect(() => {
    const onResize = () => setViewportHeight(Math.max(320, window.innerHeight - 230))
    onResize()
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  return <div className="event-scroll" onScroll={event => {
    const top = event.currentTarget.scrollTop
    setScrollTop(top)
    if (top < 100) onReachTop()
  }}>
    {loadingOlder && <div className="history-loading">Loading earlier events…</div>}
    <div className="virtual-canvas" style={{ height: events.length * ROW_HEIGHT }}>
      {visible.map((event, index) => <div className="virtual-row" key={event.seq} style={{ transform: `translateY(${(start + index) * ROW_HEIGHT}px)` }}>
        <EventCard event={event} />
      </div>)}
    </div>
  </div>
}

function isConversationEvent(event: EventView): boolean {
  return event.type.startsWith('user/') || event.type.startsWith('assistant/') ||
    event.type.startsWith('tool/') || event.type.startsWith('interact/') ||
    event.type.startsWith('turn/') || event.type.startsWith('step/')
}

export function App({ store }: { store: WebStore }) {
  const state = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot)
  const [tab, setTab] = useState<'chat' | 'trajectory'>('chat')
  const [draft, setDraft] = useState('')
  const [search, setSearch] = useState('')
  const [sendError, setSendError] = useState<string | null>(null)
  const [token, setToken] = useState(() => store.getToken())
  const selected = state.sessions.find(session => session.id === state.selectedId)
  const filtered = useMemo(() => {
    const query = search.trim().toLocaleLowerCase()
    return state.events.filter(event => {
      if (tab === 'chat' && !isConversationEvent(event)) return false
      if (query === '') return true
      return `${event.type} ${event.summary} ${event.reasoning ?? ''} ${event.tool_output ?? ''} ${event.tool_name ?? ''}`
        .toLocaleLowerCase().includes(query)
    })
  }, [search, state.events, tab])

  useEffect(() => { void store.start() }, [store])

  const submit = async (): Promise<void> => {
    const value = draft.trim()
    if (!value) return
    setDraft('')
    setSendError(null)
    try { await store.send(value) } catch (error) {
      setDraft(value)
      setSendError(error instanceof Error ? error.message : String(error))
    }
  }

  const stopRun = async (): Promise<void> => {
    setSendError(null)
    try { await store.stop() } catch (error) {
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

  return <div className="shell">
    <aside className="sidebar">
      <div className="brand"><span className="brand-mark">S</span><span>Shutu</span><span className="brand-sub">DSH web</span></div>
      <div className="sidebar-title">Sessions <span>{state.sessions.length}</span></div>
      <div className="session-list">
        {state.sessions.map(session => <button className={session.id === state.selectedId ? 'session active' : 'session'} key={session.id} onClick={() => { void store.open(session.id) }}>
          <span className="session-title">{session.title || session.id}</span><small>{session.event_count} events</small>
        </button>)}
        {state.sessions.length === 0 && !state.loading && <div className="sidebar-empty">No sessions yet</div>}
      </div>
    </aside>
    <main className="main-panel">
      <header className="topbar">
        <div><h1>{selected?.title || (state.selectedId ? state.selectedId : 'Conversation')}</h1><div className="status-line"><span className={state.connected ? 'status-dot online' : 'status-dot'} />{state.connected ? 'Live' : 'Reconnecting'}</div></div>
        <label className="search-box"><span>⌕</span><input aria-label="Search trajectory" placeholder="Search events" value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" onClick={() => setSearch('')} aria-label="Clear search">×</button>}</label>
      </header>
      <nav className="tabs" role="tablist"><button role="tab" aria-selected={tab === 'chat'} className={tab === 'chat' ? 'tab selected' : 'tab'} onClick={() => setTab('chat')}>Conversation</button><button role="tab" aria-selected={tab === 'trajectory'} className={tab === 'trajectory' ? 'tab selected' : 'tab'} onClick={() => setTab('trajectory')}>Trajectory <span>{state.events.length}</span></button></nav>
      {(state.error || sendError) && <div className="error-banner"><span>{state.error || sendError}</span><button onClick={() => { setSendError(null); void store.start() }}>Retry</button></div>}
      <section className="content-panel">
        {state.authRequired ? <form className="auth-card" onSubmit={event => { event.preventDefault(); void authenticate() }}><strong>Authentication required</strong><span>Enter the bearer token configured for the Shutu web server.</span><input aria-label="Bearer token" type="password" autoComplete="current-password" value={token} onChange={event => setToken(event.target.value)} placeholder="Bearer token" /><button type="submit" disabled={token.trim() === ''}>Connect</button></form> : state.loading ? <div className="empty"><div className="spinner" />Loading session…</div> : state.selectedId === null ? <div className="empty"><strong>Start a new conversation</strong><span>Select a session or send a message from the agent.</span></div> : filtered.length === 0 ? <div className="empty"><strong>{search ? 'No matching events' : 'No events yet'}</strong><span>{search ? 'Try a different search term.' : 'Events will appear here as the session runs.'}</span></div> : tab === 'trajectory' ? <><DshTimeline events={filtered} /><VirtualEvents events={filtered} onReachTop={() => void store.loadOlder()} loadingOlder={state.loadingOlder} /></> : <DshConversation events={filtered} sessionId={state.selectedId} onReachTop={() => void store.loadOlder()} loadingOlder={state.loadingOlder} />}
      </section>
      <form className="composer" onSubmit={event => { event.preventDefault(); if (state.sending) void stopRun(); else void submit() }}><textarea value={draft} onChange={event => setDraft(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); if (!state.sending) void submit() } }} placeholder={state.sending ? 'Agent is running…' : 'Send a message…'} rows={2} /><button type="submit" disabled={state.selectedId === null || (!state.sending && draft.trim() === '')}>{state.sending ? 'Stop' : 'Send'} <span>{state.sending ? '■' : '↵'}</span></button></form>
    </main>
  </div>
}
