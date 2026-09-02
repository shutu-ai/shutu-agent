import type { EventView } from './api'

/**
 * The canonical event facts shared by the conversation and trajectory
 * projections.  UI-specific projections may add presentation fields, but
 * they must not independently reinterpret identity, time, or relationships.
 */
export interface DshEventFacts {
  readonly event: EventView
  readonly id: string
  readonly time: number
  readonly details: Record<string, unknown>
  readonly text: string
  readonly requestId: string | null
  readonly callId: string | null
  readonly turn: number | null
  readonly step: number | null
  readonly toolName: string
  readonly toolArgs: string
  readonly toolOutput: string
  readonly kind: DshEventKind
  readonly status: DshEventStatus
  readonly structural: boolean
  readonly isContext: boolean
  readonly isError: boolean
}

/** Canonical event lanes shared by every DSH projection. */
export type DshEventKind =
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

export type DshEventStatus = 'pending' | 'running' | 'completed' | 'failed'

export function dshEventDetails(event: EventView): Record<string, unknown> {
  return event.details && typeof event.details === 'object' ? event.details : {}
}

export function dshEventText(event: EventView): string {
  return event.tool_output || event.reasoning || event.summary || event.compaction_summary || ''
}

export function dshEventNumber(event: EventView, ...keys: string[]): number | undefined {
  const details = dshEventDetails(event)
  for (const key of keys) {
    const value = details[key]
    if (typeof value === 'number' && Number.isFinite(value)) return value
  }
  return undefined
}

export function dshEventString(event: EventView, ...keys: string[]): string | undefined {
  const details = dshEventDetails(event)
  for (const key of keys) {
    const value = details[key]
    if (typeof value === 'string' && value.trim() !== '') return value
  }
  return undefined
}

export function dshEventTime(event: EventView): number {
  const value = Date.parse(event.time)
  return Number.isFinite(value) ? value : 0
}

export function dshEventId(event: EventView): string {
  return `event:${event.seq}:v${event.version}`
}

export function dshEventKind(event: EventView): DshEventKind {
  if (event.type.startsWith('turn/')) return 'turn'
  if (event.type.startsWith('step/')) return 'step'
  if (event.type.startsWith('llm/') || event.type === 'request/header' || event.type === 'request/context') return 'request'
  if (event.type === 'user/message') return event.context_message ? 'system' : 'user'
  if (event.type === 'assistant/message') return 'assistant'
  if (event.type === 'assistant/reasoning' || event.type === 'assistant/chunk') return 'reasoning'
  if (event.type === 'tool/call' || event.type === 'tool/start') return 'tool-call'
  if (event.type === 'tool/result' || event.type === 'tool/error') return 'tool-result'
  if (event.type.startsWith('compaction/')) return 'compaction'
  if (event.type.startsWith('system/') || event.context_message) return 'system'
  return 'unknown'
}

export function dshEventStatus(event: EventView): DshEventStatus {
  if (event.type.includes('error') || event.type.includes('failed') || event.compaction_error) return 'failed'
  if (event.type.endsWith('/start') || event.type === 'llm/request_start' || event.type === 'request/header' || event.type === 'request/context' || event.type === 'tool/call' || event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' || event.type === 'llm/retry' || event.type === 'llm/retry-started') return 'running'
  if (event.type.endsWith('/end') || event.type === 'llm/request_end' || event.type.endsWith('/result') || event.type === 'tool/error' || event.type === 'assistant/message' || event.type === 'compaction/summary') return 'completed'
  return event.details?.status === 'running' ? 'running' : 'completed'
}

export function dshEventIsStructural(event: EventView): boolean {
  return event.type === 'turn/start' || event.type === 'turn/end' ||
    event.type === 'step/start' || event.type === 'step/end' ||
    event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' ||
    event.type === 'assistant/stream' || event.type.startsWith('llm/') ||
    event.type === 'request/header' || event.type === 'request/context'
}

/** Normalize the fields which must have identical meaning on every UI surface. */
export function normalizeDshEvent(event: EventView, currentRequestId: string | null, currentTurn: number | null): DshEventFacts {
  const explicitRequest = dshEventString(event, 'request_id', 'requestId', 'requestID', 'id')
  const requestId = explicitRequest === undefined ? currentRequestId : `request:${explicitRequest}`
  const kind = dshEventKind(event)
  const status = dshEventStatus(event)
  return {
    event,
    id: dshEventId(event),
    time: dshEventTime(event),
    details: dshEventDetails(event),
    text: dshEventText(event),
    requestId,
    callId: event.call_id || dshEventString(event, 'call_id', 'callId') || null,
    turn: dshEventNumber(event, 'turn') ?? currentTurn,
    step: dshEventNumber(event, 'step') ?? null,
    toolName: event.tool_name || event.summary || '',
    toolArgs: event.tool_args || '',
    toolOutput: event.tool_output || '',
    kind,
    status,
    structural: dshEventIsStructural(event),
    isContext: event.context_message === true,
    isError: status === 'failed',
  }
}
