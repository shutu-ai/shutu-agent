import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { eventSearchText, filterEventSearch, matchesEventSearch, TrajectorySearchIndex } from './trajectory-search'

const baseEvent = (seq: number, extra: Partial<EventView> = {}): EventView => ({
  seq, type: 'tool/result', version: 1, time: '2026-08-26T10:00:00Z', summary: 'completed', ...extra,
})

describe('trajectory search projection', () => {
  it('includes request, identity, output and structured details', () => {
    const event = baseEvent(4, {
      tool_name: 'bash', tool_args: '{"cwd":"/workspace"}', call_id: 'call-4',
      details: { provider: 'deepseek', usage: { outputTokens: 42 } },
    })
    const text = eventSearchText(event)
    expect(text).toContain('bash')
    expect(text).toContain('call-4')
    expect(text).toContain('outputTokens')
    expect(matchesEventSearch(event, 'workspace')).toBe(true)
  })

  it('filters only matching events and leaves an empty query untouched', () => {
    const events = [baseEvent(1, { summary: 'alpha' }), baseEvent(2, { summary: 'beta' })]
    expect(filterEventSearch(events, 'BETA').map(event => event.seq)).toEqual([2])
    expect(filterEventSearch(events, '')).toBe(events)
  })

  it('returns matching fields and a bounded result snippet', () => {
    const event = baseEvent(3, { summary: 'prefix '.repeat(20) + 'needle' })
    const result = new TrajectorySearchIndex([event]).search('needle')[0]
    expect(result?.event).toBe(event)
    expect(result?.fields).toContain('summary')
    expect(result?.snippet).toContain('needle')
    expect(result?.snippet.length).toBeLessThan(140)
  })

  it('updates incrementally and requires every query term', () => {
    const first = baseEvent(1, { summary: 'alpha output' })
    const second = baseEvent(2, { summary: 'beta output' })
    const index = new TrajectorySearchIndex([first])
    expect(index.search('alpha output').map(match => match.event.seq)).toEqual([1])
    expect(index.update([first, second])).toBe(true)
    expect(index.search('beta output').map(match => match.event.seq)).toEqual([2])
    expect(index.search('alpha missing')).toEqual([])
    expect(index.update([first, second])).toBe(false)
  })
})
