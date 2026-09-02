import type { EventView } from './api'
import { deriveTrajectoryTimeline, trajectoryTimelineFocusIndexes, type DshTimelineMode, type DshTimelineModel, type DshTurn } from '@shutu-dsh/trajectory'
import { dshEventDetails, normalizeDshEvent } from './dsh-event-model'

export type { DshTimelineMode }

export interface DshTrajectoryProjection {
  records: readonly DshTrajectoryRecord[]
  turns: readonly DshTurn[]
  timeline: DshTimelineModel | null
}

export interface DshTimelineMetrics {
  readonly durationMs: number
  readonly idleMs: number
  readonly timestampedEvents: number
  readonly missingTimestamps: number
  readonly reversedTimestamps: boolean
}

export type DshTrajectoryRecordKind =
  | 'turn'
  | 'step'
  | 'request'
  | 'user'
  | 'assistant'
  | 'reasoning'
  | 'tool-call'
  | 'tool-result'
  | 'compaction'
  | 'system'
  | 'unknown'

export type DshTrajectoryRecordStatus = 'pending' | 'running' | 'completed' | 'failed'

export interface DshTrajectoryTokenUsage {
  inputTokens?: number
  outputTokens?: number
  totalTokens?: number
  reasoningTokens?: number
  cachedInputTokens?: number
}

/** One normalized record shared by trajectory, timeline, inspector and search. */
export interface DshTrajectoryRecord {
  readonly id: string
  readonly seq: number
  readonly version: number
  readonly eventType: string
  readonly kind: DshTrajectoryRecordKind
  readonly status: DshTrajectoryRecordStatus
  readonly time: string
  readonly startedAt: number | null
  readonly turn: number | null
  readonly requestId: string | null
  readonly parentId: string | null
  readonly childIds: readonly string[]
  readonly callId: string | null
  readonly label: string
  readonly text: string
  readonly inputDetail?: string
  readonly outputDetail?: string
  readonly isStructural: boolean
  readonly isError: boolean
  readonly usage?: DshTrajectoryTokenUsage
  readonly event: EventView
}

export interface DshTrajectoryEvent extends EventView {
  collapsedAssistantSeq?: number
  collapsedToolCount?: number
  collapsedToolNames?: readonly string[]
  streaming?: boolean
}

/** Summarize timing facts independently of display mode and input ordering. */
export function summarizeDshTimeline(events: readonly EventView[]): DshTimelineMetrics {
  let durationMs = 0
  let missingTimestamps = 0
  let previousTimestamp: number | null = null
  let reversedTimestamps = false
  const timestamps: number[] = []
  for (const event of events) {
    const duration = event.details?.duration_ms ?? event.details?.durationMs
    if (typeof duration === 'number' && Number.isFinite(duration)) durationMs += Math.max(0, duration)
    const timestamp = Date.parse(event.time)
    if (!Number.isFinite(timestamp)) {
      missingTimestamps += 1
      continue
    }
    if (previousTimestamp !== null && timestamp < previousTimestamp) reversedTimestamps = true
    previousTimestamp = timestamp
    timestamps.push(timestamp)
  }
  timestamps.sort((left, right) => left - right)
  let idleMs = 0
  for (let index = 1; index < timestamps.length; index += 1) {
    const gap = timestamps[index]! - timestamps[index - 1]!
    if (gap > 0) idleMs += gap
  }
  return { durationMs, idleMs, timestampedEvents: timestamps.length, missingTimestamps, reversedTimestamps }
}

function isStructuralEvent(event: EventView): boolean {
  return normalizeDshEvent(event, null, null).structural
}

function isAssistantStreamEvent(event: EventView): boolean {
  return event.type === 'assistant/chunk' || event.type === 'assistant/reasoning'
}

function mergeAssistantStream(events: readonly EventView[]): DshTrajectoryEvent | null {
  const first = events[0]
  if (first === undefined) return null
  const text = events.filter(event => event.type === 'assistant/chunk').map(event => event.summary).join('')
  const reasoning = events.filter(event => event.type === 'assistant/reasoning').map(event => event.reasoning || event.summary).join('')
  const last = events[events.length - 1]!
  return {
    ...first,
    type: 'assistant/stream',
    summary: text || reasoning || first.summary,
    ...(reasoning ? { reasoning } : {}),
    time: last.time,
    details: { ...first.details, status: 'running' },
    streaming: true,
  }
}

function compactTurn(turn: readonly EventView[]): readonly DshTrajectoryEvent[] {
  const streamEvents = turn.filter(isAssistantStreamEvent)
  const committed = turn.some(event => event.type === 'assistant/message')
  const merged = !committed ? mergeAssistantStream(streamEvents) : null
  const output: DshTrajectoryEvent[] = []
  let inserted = false
  for (const event of turn) {
    if (isAssistantStreamEvent(event)) {
      if (merged !== null && !inserted) {
        output.push(merged)
        inserted = true
      }
      continue
    }
    if (!isStructuralEvent(event)) output.push(event)
  }
  if (output.length > 0) return output
  if (merged !== null) return [merged]
  if (turn.length > 0) return [turn[turn.length - 1]!]
  return []
}

/** Return the compact DSH trajectory view for a session. */
export function collapseDshTrajectoryTurns(events: readonly EventView[]): readonly EventView[] {
  const compacted: DshTrajectoryEvent[] = []
  let turn: EventView[] = []
  const flush = (): void => {
    compacted.push(...compactTurn(turn))
    turn = []
  }
  for (const event of events) {
    if (event.type === 'user/message' && turn.some(item => item.type === 'user/message')) flush()
    turn.push(event)
  }
  flush()
  return compacted
}

function isToolEvent(event: EventView): boolean {
  return event.type.startsWith('tool/')
}

function detailsOf(event: EventView): Record<string, unknown> {
  return dshEventDetails(event)
}

function tokenUsage(event: EventView): DshTrajectoryTokenUsage | undefined {
  const raw = detailsOf(event).usage
  if (raw === null || typeof raw !== 'object') return undefined
  const source = raw as Record<string, unknown>
  const usage: DshTrajectoryTokenUsage = {
    ...(typeof source.input_tokens === 'number' ? { inputTokens: source.input_tokens } : typeof source.inputTokens === 'number' ? { inputTokens: source.inputTokens } : {}),
    ...(typeof source.output_tokens === 'number' ? { outputTokens: source.output_tokens } : typeof source.outputTokens === 'number' ? { outputTokens: source.outputTokens } : {}),
    ...(typeof source.total_tokens === 'number' ? { totalTokens: source.total_tokens } : typeof source.totalTokens === 'number' ? { totalTokens: source.totalTokens } : {}),
    ...(typeof source.reasoning_tokens === 'number' ? { reasoningTokens: source.reasoning_tokens } : typeof source.reasoningTokens === 'number' ? { reasoningTokens: source.reasoningTokens } : {}),
    ...(typeof source.cached_input_tokens === 'number' ? { cachedInputTokens: source.cached_input_tokens } : typeof source.cachedInputTokens === 'number' ? { cachedInputTokens: source.cachedInputTokens } : {}),
  }
  return Object.keys(usage).length === 0 ? undefined : usage
}

function recordLabel(event: EventView, kind: DshTrajectoryRecordKind): string {
  if (event.tool_name) return event.tool_name
  if (kind === 'user') return 'User'
  if (kind === 'assistant') return 'Assistant'
  if (kind === 'request') return 'LLM request'
  if (kind === 'tool-call' || kind === 'tool-result') return 'Tool'
  if (kind === 'reasoning') return 'Reasoning'
  if (kind === 'compaction') return 'Compaction'
  return event.type
}

/** Normalize raw events once so every DSH surface uses the same relationships. */
export function projectDshTrajectoryRecords(events: readonly EventView[]): readonly DshTrajectoryRecord[] {
  const records: Array<Omit<DshTrajectoryRecord, 'childIds'>> = []
  const recordById = new Map<string, Omit<DshTrajectoryRecord, 'childIds'>>()
  const callRecordById = new Map<string, string>()
  let currentTurn: number | null = null
  let currentRequestId: string | null = null
  let currentAssistantId: string | null = null

  for (const event of events) {
    const facts = normalizeDshEvent(event, currentRequestId, currentTurn)
    const kind = facts.kind as DshTrajectoryRecordKind
    const id = facts.id
    if (event.type === 'turn/start') currentTurn = facts.turn ?? (currentTurn ?? 0) + 1
    if (event.type === 'user/message' && !event.context_message && currentTurn === null) currentTurn = 0
    const requestId: string | null = kind === 'request' ? facts.requestId : facts.requestId
    if (kind === 'request' && requestId !== null) currentRequestId = requestId
    const callId = facts.callId
    const parentId = kind === 'tool-result' && callId !== null && callRecordById.has(callId)
      ? callRecordById.get(callId)!
      : kind === 'tool-call' && currentAssistantId !== null
        ? currentAssistantId
        : kind !== 'turn' && kind !== 'step' && kind !== 'user' && currentRequestId !== null
          ? recordById.get(currentRequestId)?.id ?? null
          : null
    const startedAt = Date.parse(event.time)
    const normalized: Omit<DshTrajectoryRecord, 'childIds'> = {
      id, seq: event.seq, version: event.version, eventType: event.type, kind,
      status: facts.status as DshTrajectoryRecordStatus, time: event.time, startedAt: Number.isFinite(startedAt) ? startedAt : null,
      turn: facts.turn ?? currentTurn, requestId,
      parentId, callId, label: recordLabel(event, kind), text: facts.text,
      ...(event.tool_args ? { inputDetail: event.tool_args } : {}),
      ...(event.tool_output ? { outputDetail: event.tool_output } : {}),
      isStructural: facts.structural, isError: facts.isError,
      ...(tokenUsage(event) ? { usage: tokenUsage(event) } : {}), event,
    }
    records.push(normalized)
    recordById.set(id, normalized)
	if (kind === 'request' && requestId !== null && (event.type.endsWith('/start') || event.type === 'llm/request_start' || event.type === 'request/header' || event.type === 'request/context')) {
      recordById.set(requestId, normalized)
    }
    if (kind === 'tool-call' && callId !== null) callRecordById.set(callId, id)
    if (kind === 'assistant') currentAssistantId = id
    if ((event.type.endsWith('/end') || event.type === 'llm/request_end') && kind === 'request') currentRequestId = null
    if (event.type === 'turn/end') currentAssistantId = null
  }

  const children = new Map<string, string[]>()
  for (const record of records) {
    if (record.parentId === null) continue
    const list = children.get(record.parentId) ?? []
    list.push(record.id)
    children.set(record.parentId, list)
  }
  return records.map(record => ({ ...record, childIds: children.get(record.id) ?? [] }))
}

function toolCallSummary(events: readonly EventView[]): string {
  const names = [...new Set(events.map(event => event.tool_name).filter((name): name is string => name !== undefined && name !== ''))]
  const count = events.length
  return `${count} tool call${count === 1 ? '' : 's'}${names.length === 0 ? '' : ` · ${names.join(', ')}`}`
}

/** Collapse the contiguous tool calls immediately following selected assistant messages. */
export function collapseDshAssistantToolCalls(events: readonly EventView[], collapsedAssistants: ReadonlySet<number>): readonly DshTrajectoryEvent[] {
  const output: DshTrajectoryEvent[] = []
  for (let index = 0; index < events.length; index += 1) {
    const event = events[index]
    if (event === undefined) continue
    output.push(event)
    if (event.type !== 'assistant/message' || !collapsedAssistants.has(event.seq)) continue
    const calls: EventView[] = []
    let next = index + 1
    while (next < events.length && isToolEvent(events[next]!)) {
      calls.push(events[next]!)
      next += 1
    }
    if (calls.length === 0) continue
    const last = calls[calls.length - 1]!
    output.push({
      seq: last.seq,
      type: 'tool/summary',
      version: last.version,
      time: last.time,
      summary: toolCallSummary(calls),
      collapsedAssistantSeq: event.seq,
      collapsedToolCount: calls.length,
      collapsedToolNames: [...new Set(calls.map(call => call.tool_name).filter((name): name is string => name !== undefined && name !== ''))],
    })
    index = next - 1
  }
  return output
}

function cellKind(event: EventView): string {
  if (event.type.startsWith('tool/')) return 'tool'
  if (event.type === 'user/message') return 'user'
  if (event.type === 'assistant/reasoning') return 'message'
  if (event.type.startsWith('llm/')) return 'system'
  if (event.type.startsWith('compaction/')) return 'compacted'
  return 'message'
}

function durationSeconds(event: EventView): number | null {
  const value = event.details?.duration_ms ?? event.details?.durationMs
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value / 1000) : null
}

function turnsFromRecords(records: readonly DshTrajectoryRecord[]): readonly DshTurn[] {
  const turns: DshTurn[] = []
  let turn = 0
  let index = 0
  let cells: DshTurn['groups'][number]['cells'] = []
  const flush = (): void => {
    if (cells.length === 0) return
    turns.push({ turn, groups: [{ title: `Turn ${turn}`, cells }] })
    cells = []
  }
  for (const record of records) {
    const event = record.event
    if (event.type === 'user/message' && cells.length > 0) {
      flush()
      turn += 1
    }
    const text = record.text
    cells = [...cells, {
      index: ++index,
      kind: record.kind === 'tool-call' || record.kind === 'tool-result' ? 'tool' : cellKind(event),
      text,
      timeSeconds: durationSeconds(event),
      ...(record.startedAt !== null ? { startedAt: record.startedAt } : {}),
      sourceSeq: event.seq,
      ...(event.tool_args ? { inputDetail: event.tool_args } : {}),
      ...(event.tool_output ? { outputDetail: event.tool_output } : {}),
      ...(record.callId !== null ? { callId: record.callId } : {}),
      ...(record.isError ? { isError: true } : {}),
      ...(record.usage !== undefined ? {
        input: record.usage.inputTokens,
        output: record.usage.outputTokens,
      } : {}),
    }]
  }
  flush()
  return turns
}

export function toDshTurns(events: readonly EventView[]): readonly DshTurn[] {
  return turnsFromRecords(projectDshTrajectoryRecords(events))
}

export function projectDshTrajectory(events: readonly EventView[], mode: DshTimelineMode = 'sequence'): DshTrajectoryProjection {
  const records = projectDshTrajectoryRecords(events)
  const turns = turnsFromRecords(records)
  return { records, turns, timeline: deriveTrajectoryTimeline(turns, mode) }
}

export function focusDshTrajectory(events: readonly EventView[], range: { start: number; end: number }, mode: DshTimelineMode = 'sequence'): ReadonlySet<number> {
  return trajectoryTimelineFocusIndexes(toDshTurns(events), range, mode)
}
