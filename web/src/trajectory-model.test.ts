import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { collapseShutuAssistantToolCalls, collapseShutuTrajectoryTurns, projectShutuTrajectory, projectShutuTrajectoryRecords, summarizeShutuTimeline, type ShutuTrajectoryEvent } from './trajectory-model'

const event = (seq: number, type: string, summary: string, details?: Record<string, unknown>): EventView => ({
  seq, type, version: 1, time: `2026-08-25T00:00:0${seq}Z`, summary, ...(details ? { details } : {}),
})

describe('SHUTU trajectory adapter', () => {
  it('projects Shutu events into SHUTU timeline lanes', () => {
    const projection = projectShutuTrajectory([
      event(1, 'user/message', 'question'),
      event(2, 'assistant/message', 'answer', { duration_ms: 120 }),
      event(3, 'tool/result', 'output', { duration_ms: 80 }),
    ])

    expect(projection.turns).toHaveLength(1)
    expect(projection.timeline?.spans).toHaveLength(3)
    expect(projection.timeline?.spans.map(span => span.lane)).toEqual([0, 1, 2])
    expect(projection.timeline?.spans.every(span => span.end >= span.start)).toBe(true)
  })

  it('collapses turn and streaming bookkeeping without hiding surface outcomes', () => {
    const compacted = collapseShutuTrajectoryTurns([
      event(1, 'turn/start', 'start'),
      event(2, 'user/message', 'question'),
      event(3, 'step/start', 'step'),
      event(4, 'assistant/chunk', 'delta'),
      event(5, 'tool/result', 'tool output'),
      event(6, 'assistant/reasoning', 'reasoning'),
      event(7, 'assistant/message', 'answer'),
      event(8, 'step/end', 'step done'),
      event(9, 'turn/end', 'done'),
      event(10, 'turn/start', 'start'),
      event(11, 'user/message', 'second question'),
    ])

    expect(compacted.map(item => item.seq)).toEqual([2, 5, 7, 11])
  })

  it('collapses contiguous tool calls after one assistant without hiding later records', () => {
    const compacted = collapseShutuAssistantToolCalls([
      event(1, 'assistant/message', 'plan'),
      { ...event(2, 'tool/result', 'first'), tool_name: 'glob' },
      { ...event(3, 'tool/result', 'second'), tool_name: 'read' },
      event(4, 'assistant/message', 'final'),
    ], new Set([1]))

    expect(compacted.map(item => item.seq)).toEqual([1, 3, 4])
    expect(compacted[1]?.type).toBe('tool/summary')
    expect(compacted[1]?.summary).toBe('2 tool calls · glob, read')
    expect(compacted[1]?.collapsedAssistantSeq).toBe(1)
  })

  it('normalizes request and tool relationships with stable record ids', () => {
    const records = projectShutuTrajectoryRecords([
      event(1, 'turn/start', 'start', { turn: 3 }),
      event(2, 'llm/request_start', 'request', { request_id: 'req-1', usage: { input_tokens: 4 } }),
      { ...event(3, 'assistant/message', 'call'), tool_name: 'read', call_id: 'call-1' },
      { ...event(4, 'tool/call', 'read'), tool_name: 'read', tool_args: '{"path":"a"}', call_id: 'call-1' },
      { ...event(5, 'tool/result', 'done'), tool_name: 'read', tool_output: 'ok', call_id: 'call-1' },
      event(6, 'llm/request_end', 'complete', { request_id: 'req-1', usage: { input_tokens: 4, output_tokens: 2, total_tokens: 6 } }),
    ])

    expect(records.map(record => record.id)).toEqual([
      'event:1:v1', 'event:2:v1', 'event:3:v1', 'event:4:v1', 'event:5:v1', 'event:6:v1',
    ])
    expect(records[1]?.kind).toBe('request')
    expect(records[2]?.requestId).toBe('request:req-1')
    expect(records[3]?.parentId).toBe(records[2]?.id)
    expect(records[4]?.parentId).toBe(records[3]?.id)
    expect(records[4]?.childIds).toEqual([])
    expect(records[5]?.requestId).toBe('request:req-1')
    expect(records[5]?.usage?.totalTokens).toBe(6)
    expect(records[1]?.childIds).toEqual([records[2]?.id, records[5]?.id])
    expect(records[2]?.childIds).toEqual([records[3]?.id])
  })

  it('keeps malformed timestamps and unknown events inspectable', () => {
    const records = projectShutuTrajectoryRecords([event(1, 'custom/event', 'opaque')].map(item => ({ ...item, time: 'not-a-date' })))
    expect(records[0]?.kind).toBe('unknown')
    expect(records[0]?.startedAt).toBeNull()
    expect(records[0]?.text).toBe('opaque')
  })

  it('summarizes duration, idle gaps, missing and reversed timestamps', () => {
    const metrics = summarizeShutuTimeline([
      { ...event(1, 'assistant/message', 'one', { duration_ms: 25 }), time: '2026-08-25T00:00:02Z' },
      { ...event(2, 'tool/result', 'two'), time: '2026-08-25T00:00:01Z' },
      { ...event(3, 'assistant/message', 'three'), time: 'bad-time' },
      { ...event(4, 'assistant/message', 'four'), time: '2026-08-25T00:00:10Z' },
    ])

    expect(metrics).toMatchObject({ durationMs: 25, idleMs: 9000, timestampedEvents: 3, missingTimestamps: 1, reversedTimestamps: true })
  })

  it('merges uncommitted assistant chunks into one stable live record', () => {
    const compacted = collapseShutuTrajectoryTurns([
      event(1, 'turn/start', 'start'),
      event(2, 'user/message', 'question'),
      event(3, 'assistant/chunk', 'Hel'),
      event(4, 'assistant/reasoning', 'thinking'),
      event(5, 'assistant/chunk', 'lo'),
    ])

    expect(compacted.map(item => item.seq)).toEqual([2, 3])
    expect(compacted[1]?.type).toBe('assistant/stream')
    expect(compacted[1]?.summary).toBe('Hello')
    expect(compacted[1]?.reasoning).toBe('thinking')
    expect((compacted[1] as ShutuTrajectoryEvent)?.streaming).toBe(true)
  })

  it('drops uncommitted chunks when the final assistant message arrives', () => {
    const compacted = collapseShutuTrajectoryTurns([
      event(1, 'user/message', 'question'),
      event(2, 'assistant/chunk', 'partial'),
      event(3, 'assistant/message', 'complete'),
    ])

    expect(compacted.map(item => item.seq)).toEqual([1, 3])
    expect(compacted.some(item => item.type === 'assistant/stream')).toBe(false)
  })
})
