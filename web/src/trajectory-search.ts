import type { EventView } from './api'

export interface EventSearchMatch {
  readonly event: EventView
  readonly fields: readonly string[]
  readonly snippet: string
}

const SEARCH_FIELDS: readonly (readonly [string, (event: EventView) => unknown])[] = [
  ['type', event => event.type],
  ['summary', event => event.summary],
  ['command', event => event.command],
  ['reasoning', event => event.reasoning],
  ['tool_name', event => event.tool_name],
  ['tool_output', event => event.tool_output],
  ['tool_args', event => event.tool_args],
  ['call_id', event => event.call_id],
  ['compaction_summary', event => event.compaction_summary],
  ['compaction_error', event => event.compaction_error],
  ['details', event => event.details],
]

function normalized(value: unknown): string {
  if (value === undefined || value === null) return ''
  if (typeof value === 'string') return value
  try { return JSON.stringify(value) ?? '' } catch { return String(value) }
}

function searchableFields(event: EventView): readonly (readonly [string, string])[] {
  return SEARCH_FIELDS.map(([name, read]) => [name, normalized(read(event))] as const).filter(([, value]) => value !== '')
}

function snippet(value: string, query: string): string {
  const index = value.toLocaleLowerCase().indexOf(query)
  if (index < 0) return value.slice(0, 120)
  const start = Math.max(0, index - 44)
  const end = index + query.length + 76
  return `${start > 0 ? '…' : ''}${value.slice(start, end)}${end < value.length ? '…' : ''}`
}

export class TrajectorySearchIndex {
  private readonly entries = new Map<string, { event: EventView; fields: readonly (readonly [string, string])[] }>()
  private events: readonly EventView[] = []

  constructor(events: readonly EventView[]) {
    this.update(events)
  }

  /** Incrementally synchronize event entries while retaining unchanged fields. */
  update(events: readonly EventView[]): boolean {
    if (events === this.events || (events.length === this.events.length && events.every((event, index) => event === this.events[index]))) return false
    const seen = new Set<string>()
    for (const event of events) {
      const key = `${event.seq}:v${event.version}`
      const previous = this.entries.get(key)
      if (previous === undefined || previous.event !== event) {
        this.entries.set(key, { event, fields: searchableFields(event) })
      }
      seen.add(key)
    }
    for (const key of this.entries.keys()) {
      if (!seen.has(key)) this.entries.delete(key)
    }
    this.events = events
    return true
  }

  search(query: string): readonly EventSearchMatch[] {
    const needle = query.trim().toLocaleLowerCase()
    if (needle === '') return this.events.map(event => ({ event, fields: [], snippet: '' }))
    const terms = needle.split(/\s+/).filter(Boolean)
    return this.events.flatMap(event => {
      const entry = this.entries.get(`${event.seq}:v${event.version}`)
      if (entry === undefined) return []
      const matches = entry.fields.filter(([, value]) => terms.some(term => value.toLocaleLowerCase().includes(term)))
      const matchesEveryTerm = terms.every(term => entry.fields.some(([, value]) => value.toLocaleLowerCase().includes(term)))
      if (matches.length === 0 || !matchesEveryTerm) return []
      const match = matches[0]!
      const snippetTerm = terms.find(term => match[1].toLocaleLowerCase().includes(term)) ?? terms[0]!
      return [{ event, fields: matches.map(([name]) => name), snippet: snippet(match[1], snippetTerm) }]
    })
  }
}

/** Build the searchable surface used by both conversation and trajectory views. */
export function eventSearchText(event: EventView): string {
  return searchableFields(event).map(([, value]) => value).join('\n')
}

export function matchesEventSearch(event: EventView, query: string): boolean {
  const normalized = query.trim().toLocaleLowerCase()
  return normalized === '' || eventSearchText(event).toLocaleLowerCase().includes(normalized)
}

export function filterEventSearch(events: readonly EventView[], query: string): readonly EventView[] {
  if (query.trim() === '') return events
  return new TrajectorySearchIndex(events).search(query).map(match => match.event)
}
