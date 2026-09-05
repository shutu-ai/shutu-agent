import type { EventView } from './api'

/**
 * The canonical event facts shared by the conversation and trajectory
 * projections.  UI-specific projections may add presentation fields, but
 * they must not independently reinterpret identity, time, or relationships.
 */
export interface ShutuEventFacts {
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
  readonly kind: ShutuEventKind
  readonly status: ShutuEventStatus
  readonly structural: boolean
  readonly isContext: boolean
  readonly isError: boolean
}

/** Canonical event lanes shared by every SHUTU projection. */
export type ShutuEventKind =
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

export type ShutuEventStatus = 'pending' | 'running' | 'completed' | 'failed'

export function shutuEventDetails(event: EventView): Record<string, unknown> {
  return event.details && typeof event.details === 'object' ? event.details : {}
}

export function shutuEventText(event: EventView): string {
  return event.tool_output || event.reasoning || event.summary || event.compaction_summary || ''
}

export function shutuEventNumber(event: EventView, ...keys: string[]): number | undefined {
  const details = shutuEventDetails(event)
  for (const key of keys) {
    const value = details[key]
    if (typeof value === 'number' && Number.isFinite(value)) return value
  }
  return undefined
}

export function shutuEventString(event: EventView, ...keys: string[]): string | undefined {
  const details = shutuEventDetails(event)
  for (const key of keys) {
    const value = details[key]
    if (typeof value === 'string' && value.trim() !== '') return value
  }
  return undefined
}

export function shutuEventTime(event: EventView): number {
  const value = Date.parse(event.time)
  return Number.isFinite(value) ? value : 0
}

export function shutuEventId(event: EventView): string {
  return `event:${event.seq}:v${event.version}`
}

export function shutuEventKind(event: EventView): ShutuEventKind {
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

export function shutuEventStatus(event: EventView): ShutuEventStatus {
  if (event.type.includes('error') || event.type.includes('failed') || event.compaction_error) return 'failed'
  if (event.type.endsWith('/start') || event.type === 'llm/request_start' || event.type === 'request/header' || event.type === 'request/context' || event.type === 'tool/call' || event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' || event.type === 'llm/retry' || event.type === 'llm/retry-started') return 'running'
  if (event.type.endsWith('/end') || event.type === 'llm/request_end' || event.type.endsWith('/result') || event.type === 'tool/error' || event.type === 'assistant/message' || event.type === 'compaction/summary') return 'completed'
  return event.details?.status === 'running' ? 'running' : 'completed'
}

export function shutuEventIsStructural(event: EventView): boolean {
  return event.type === 'turn/start' || event.type === 'turn/end' ||
    event.type === 'step/start' || event.type === 'step/end' ||
    event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' ||
    event.type === 'assistant/stream' || event.type.startsWith('llm/') ||
    event.type === 'request/header' || event.type === 'request/context'
}

/** Normalize the fields which must have identical meaning on every UI surface. */
export function normalizeShutuEvent(event: EventView, currentRequestId: string | null, currentTurn: number | null): ShutuEventFacts {
  const explicitRequest = shutuEventString(event, 'request_id', 'requestId', 'requestID', 'id')
  const requestId = explicitRequest === undefined ? currentRequestId : `request:${explicitRequest}`
  const kind = shutuEventKind(event)
  const status = shutuEventStatus(event)
  return {
    event,
    id: shutuEventId(event),
    time: shutuEventTime(event),
    details: shutuEventDetails(event),
    text: shutuEventText(event),
    requestId,
    callId: event.call_id || shutuEventString(event, 'call_id', 'callId') || null,
    turn: shutuEventNumber(event, 'turn') ?? currentTurn,
    step: shutuEventNumber(event, 'step') ?? null,
    toolName: event.tool_name || event.summary || '',
    toolArgs: event.tool_args || '',
    toolOutput: event.tool_output || '',
    kind,
    status,
    structural: shutuEventIsStructural(event),
    isContext: event.context_message === true,
    isError: status === 'failed',
  }
}
