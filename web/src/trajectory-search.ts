import type { EventView } from './api'

function stringifyDetails(value: unknown): string {
  if (value === undefined || value === null) return ''
  if (typeof value === 'string') return value
  try { return JSON.stringify(value) ?? '' } catch { return String(value) }
}

/** Build the searchable surface used by both conversation and trajectory views. */
export function eventSearchText(event: EventView): string {
  return [
    event.type,
    event.summary,
    event.command,
    event.reasoning,
    event.tool_name,
    event.tool_output,
    event.tool_args,
    event.call_id,
    event.compaction_summary,
    event.compaction_error,
    stringifyDetails(event.details),
  ].filter((value): value is string => value !== undefined && value !== '').join('\n')
}

export function matchesEventSearch(event: EventView, query: string): boolean {
  const normalized = query.trim().toLocaleLowerCase()
  return normalized === '' || eventSearchText(event).toLocaleLowerCase().includes(normalized)
}

export function filterEventSearch(events: readonly EventView[], query: string): readonly EventView[] {
  if (query.trim() === '') return events
  return events.filter(event => matchesEventSearch(event, query))
}
