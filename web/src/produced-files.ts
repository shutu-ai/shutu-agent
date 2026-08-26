import type { EventView } from './api'

export function deriveProducedFiles(events: readonly EventView[]): ReadonlyMap<number, readonly string[]> {
  const result = new Map<number, readonly string[]>()
  const pending: string[] = []
  const seen = new Set<string>()
  for (const event of events) {
    if (event.type === 'user/message') {
      pending.length = 0
      seen.clear()
    }
    if (event.type === 'fs/write') {
      const path = event.details?.path
      if (typeof path === 'string' && path !== '' && !seen.has(path)) {
        seen.add(path)
        pending.push(path)
      }
    }
    if (event.type === 'assistant/message' && pending.length > 0) {
      result.set(event.seq, [...pending])
      pending.length = 0
      seen.clear()
    }
  }
  return result
}
