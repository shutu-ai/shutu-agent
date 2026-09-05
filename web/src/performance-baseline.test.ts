import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { projectShutuTrajectoryRecords } from './trajectory-model'
import { TrajectorySearchIndex } from './trajectory-search'
import { buildVirtualOffsets, virtualRange } from './virtual-list'

function makeEvents(count: number): EventView[] {
  return Array.from({ length: count }, (_, index) => ({
    seq: index + 1,
    type: index % 2 === 0 ? 'assistant/message' : 'tool/result',
    version: 1,
    time: `2026-08-26T10:00:${String(index % 60).padStart(2, '0')}Z`,
    summary: index === count - 1 ? 'needle at the end' : `event ${index + 1}`,
    ...(index % 2 === 1 ? { tool_name: 'read_file', tool_output: `output ${index}` } : {}),
  }))
}

describe('P34 synthetic performance baseline', () => {
  it('keeps projection and indexed search linear for a 10k-event history', () => {
    const events = makeEvents(10_000)
    const records = projectShutuTrajectoryRecords(events)
    const index = new TrajectorySearchIndex(events)

    expect(records).toHaveLength(10_000)
    expect(index.search('needle')).toMatchObject([{ event: { seq: 10_000 } }])
  })

  it('builds bounded virtual offsets for a 100k-row canvas', () => {
    const keys = Array.from({ length: 100_000 }, (_, index) => String(index))
    const offsets = buildVirtualOffsets(keys, new Map([['99999', 480]]), 132)

    expect(offsets).toHaveLength(100_001)
    expect(offsets.at(-1)).toBe(13_200_348)
    expect(virtualRange(offsets, 640_000, 640, 8).end - virtualRange(offsets, 640_000, 640, 8).start).toBeLessThan(30)
  })
})
