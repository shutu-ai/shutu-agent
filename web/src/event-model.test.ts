import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { shutuEventId, shutuEventText, normalizeShutuEvent } from './event-model'
import { projectShutuConversation } from './conversation-model'
import { projectShutuTrajectoryRecords } from './trajectory-model'

const event = (seq: number, type: string, summary: string, details: Record<string, unknown> = {}): EventView => ({
  seq,
  type,
  version: 2,
  time: `2026-08-28T00:00:${String(seq).padStart(2, '0')}Z`,
  summary,
  details,
})

describe('shared SHUTU event model', () => {
  it('uses one identity, text and request/call normalization contract', () => {
    const raw = {
      ...event(7, 'tool/result', 'fallback', { request_id: 'r-1', callId: 'c-1', turn: 3, step: 2 }),
      tool_output: 'canonical output',
      call_id: 'c-1',
    }
    const facts = normalizeShutuEvent(raw, null, null)

    expect(facts.id).toBe(shutuEventId(raw))
    expect(facts.text).toBe(shutuEventText(raw))
    expect(facts.requestId).toBe('request:r-1')
    expect(facts.callId).toBe('c-1')
    expect(facts.turn).toBe(3)
    expect(facts.step).toBe(2)
  })

  it('keeps conversation and trajectory anchored to the same event IDs and tool call', () => {
    const events: EventView[] = [
      event(1, 'llm/request_start', 'request', { request_id: 'r-2' }),
      event(2, 'user/message', 'inspect'),
      { ...event(3, 'assistant/message', 'answer'), details: { request_id: 'r-2' } },
      { ...event(4, 'tool/call', 'read'), tool_name: 'read', tool_args: '{"path":"a"}', call_id: 'c-2' },
      { ...event(5, 'tool/result', 'done'), tool_output: 'contents', call_id: 'c-2' },
    ]

    const conversation = projectShutuConversation(events)
    const trajectory = projectShutuTrajectoryRecords(events)
    const conversationIds = new Set(conversation.nodes.map(node => node.id))

    for (const record of trajectory) {
      if (conversationIds.has(record.id)) expect(record.id).toBe(shutuEventId(record.event))
    }
    expect(conversation.nodes.find(node => node.kind === 'tool-result')).toMatchObject({
      id: shutuEventId(events[4]!),
      callId: 'c-2',
      assistantId: shutuEventId(events[2]!),
    })
    expect(trajectory.find(record => record.event.type === 'tool/result')).toMatchObject({
      id: shutuEventId(events[4]!),
      callId: 'c-2',
      parentId: shutuEventId(events[3]!),
    })
  })

  it('uses the same classification facts for context, failed tools and extensions', () => {
    const events: EventView[] = [
      event(1, 'request/context', 'route', { provider: 'mock', model: 'm', contextWindow: 128000 }),
      { ...event(2, 'user/message', 'catalog'), context_message: true, context_source: 'skill-catalog' },
      { ...event(3, 'tool/error', 'failed', { request_id: 'r-3' }), call_id: 'c-3', tool_name: 'read', tool_output: 'denied' },
      event(4, 'extension/custom', 'opaque'),
    ]

    const facts = events.map((item, index) => normalizeShutuEvent(item, index === 2 ? 'request:r-3' : null, 1))
    expect(facts.map(item => item.kind)).toEqual(['request', 'system', 'tool-result', 'unknown'])
    expect(facts[2]).toMatchObject({ status: 'failed', isError: true, requestId: 'request:r-3', callId: 'c-3', structural: false })
    expect(facts[3]).toMatchObject({ status: 'completed', isError: false, text: 'opaque' })

    const conversation = projectShutuConversation(events)
    const trajectory = projectShutuTrajectoryRecords(events)
    const conversationIds = new Set(conversation.nodes.map(node => node.id))
    expect(trajectory.filter(record => conversationIds.has(record.id)).map(record => record.id)).toEqual(conversation.nodes.map(node => node.id))
    expect(trajectory.find(record => record.event.type === 'tool/error')).toMatchObject({ kind: 'tool-result', status: 'failed', isError: true, requestId: 'request:r-3' })
  })
})
