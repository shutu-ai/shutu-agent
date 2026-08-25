import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { projectDshTrajectory } from './dsh-trajectory'

const event = (seq: number, type: string, summary: string, details?: Record<string, unknown>): EventView => ({
  seq, type, version: 1, time: `2026-08-25T00:00:0${seq}Z`, summary, ...(details ? { details } : {}),
})

describe('DSH trajectory adapter', () => {
  it('projects Shutu events into DSH timeline lanes', () => {
    const projection = projectDshTrajectory([
      event(1, 'user/message', 'question'),
      event(2, 'assistant/message', 'answer', { duration_ms: 120 }),
      event(3, 'tool/result', 'output', { duration_ms: 80 }),
    ])

    expect(projection.turns).toHaveLength(1)
    expect(projection.timeline?.spans).toHaveLength(3)
    expect(projection.timeline?.spans.map(span => span.lane)).toEqual([0, 1, 2])
    expect(projection.timeline?.spans.every(span => span.end >= span.start)).toBe(true)
  })
})
