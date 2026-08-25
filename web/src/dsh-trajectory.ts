import type { EventView } from './api'
import { deriveTrajectoryTimeline, trajectoryTimelineFocusIndexes, type DshTimelineMode, type DshTimelineModel, type DshTurn } from '@shutu-dsh/trajectory'

export type { DshTimelineMode }

export interface DshTrajectoryProjection {
  turns: readonly DshTurn[]
  timeline: DshTimelineModel | null
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

export function toDshTurns(events: readonly EventView[]): readonly DshTurn[] {
  const turns: DshTurn[] = []
  let turn = 0
  let index = 0
  let cells: DshTurn['groups'][number]['cells'] = []
  const flush = (): void => {
    if (cells.length === 0) return
    turns.push({ turn, groups: [{ title: `Turn ${turn}`, cells }] })
    cells = []
  }
  for (const event of events) {
    if (event.type === 'user/message' && cells.length > 0) {
      flush()
      turn += 1
    }
    const startedAt = Date.parse(event.time)
    const text = event.tool_output || event.reasoning || event.summary || event.compaction_summary || ''
    cells = [...cells, {
      index: ++index,
      kind: cellKind(event),
      text,
      timeSeconds: durationSeconds(event),
      ...(Number.isFinite(startedAt) ? { startedAt } : {}),
      sourceSeq: event.seq,
      ...(event.tool_args ? { inputDetail: event.tool_args } : {}),
      ...(event.tool_output ? { outputDetail: event.tool_output } : {}),
      ...(event.tool_name ? { callId: event.call_id } : {}),
      ...(event.type.includes('error') ? { isError: true } : {}),
      ...(typeof event.details?.usage === 'object' && event.details.usage !== null ? {
        input: typeof (event.details.usage as Record<string, unknown>).inputTokens === 'number'
          ? (event.details.usage as Record<string, number>).inputTokens : undefined,
        output: typeof (event.details.usage as Record<string, unknown>).outputTokens === 'number'
          ? (event.details.usage as Record<string, number>).outputTokens : undefined,
      } : {}),
    }]
  }
  flush()
  return turns
}

export function projectDshTrajectory(events: readonly EventView[], mode: DshTimelineMode = 'sequence'): DshTrajectoryProjection {
  const turns = toDshTurns(events)
  return { turns, timeline: deriveTrajectoryTimeline(turns, mode) }
}

export function focusDshTrajectory(events: readonly EventView[], range: { start: number; end: number }, mode: DshTimelineMode = 'sequence'): ReadonlySet<number> {
  return trajectoryTimelineFocusIndexes(toDshTurns(events), range, mode)
}
