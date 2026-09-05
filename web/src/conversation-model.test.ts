import { describe, expect, it } from 'vitest'
import { projectShutuConversation } from './conversation-model'
import type { EventView } from './api'

function event(seq: number, type: string, summary: string, extra: Partial<EventView> = {}): EventView {
  return { seq, type, version: 1, time: `2026-08-25T00:00:0${seq}Z`, summary, ...extra }
}

describe('SHUTU conversation carrier', () => {
  it('folds raw events into SHUTU chat nodes and pairs tool results', () => {
    const snapshot = projectShutuConversation([
      event(1, 'turn/start', 'start', { details: { turn: 1 } }),
      event(2, 'user/message', 'inspect'),
      event(3, 'tool/call', 'read_file', { tool_name: 'read_file', tool_args: '{"path":"a"}', call_id: 'c1' }),
      event(4, 'tool/result', 'done', { tool_name: 'read_file', tool_output: 'ok', call_id: 'c1' }),
      event(5, 'assistant/message', 'answer', { details: { usage: { totalTokens: 9 } } }),
      event(6, 'turn/end', 'end', { details: { turn: 1 } }),
    ], 's1')

    expect(snapshot.sessionId).toBe('s1')
    expect(snapshot.nodes.map(node => node.kind)).toEqual(['user', 'tool-result', 'assistant'])
    expect(snapshot.chat.order).toHaveLength(3)
    expect(snapshot.nodes[1]).toMatchObject({ kind: 'tool-result', callId: 'c1', call: { name: 'read_file' } })
    expect(snapshot.turnEnds.get(1)).toBe(6)
    expect(snapshot.running).toBe(false)
  })

  it('keeps unmatched tool calls as running calls', () => {
    const snapshot = projectShutuConversation([
      event(1, 'tool/call', 'shell', { tool_name: 'shell', call_id: 'c2' }),
    ])
    expect(snapshot.running).toBe(true)
    expect(snapshot.runningCalls).toMatchObject([{ callId: 'c2', name: 'shell' }])
    expect(snapshot.nodes).toMatchObject([{ kind: 'tool-running', seq: 1 }])
  })

  it('carries request identity, context provenance and assistant images', () => {
    const snapshot = projectShutuConversation([
      event(1, 'llm/request_start', 'request', { details: { request_id: 'r-1' } }),
      event(2, 'user/message', 'context', { context_message: true, context_source: 'skill-catalog' }),
      event(3, 'assistant/message', 'answer', { images: [{ id: 'img-1', media_type: 'image/png' }] }),
      event(4, 'tool/call', 'read', { tool_name: 'read', call_id: 'c-1' }),
      event(5, 'tool/result', 'done', { tool_name: 'read', call_id: 'c-1' }),
      event(6, 'llm/request_end', 'complete', { details: { request_id: 'r-1' } }),
    ], 's-1')

    expect(snapshot.nodes[0]).toMatchObject({ kind: 'context', source: 'skill-catalog' })
    expect(snapshot.nodes[1]).toMatchObject({ kind: 'assistant', requestId: 'request:r-1', images: [{ id: 'img-1' }] })
    expect(snapshot.nodes[2]).toMatchObject({ kind: 'tool-result', requestId: 'request:r-1', callId: 'c-1' })
  })

  it('uses trajectory event ids and keeps assistant/tool relationships explicit', () => {
    const snapshot = projectShutuConversation([
      event(1, 'llm/request_start', 'request', { details: { request_id: 'r-2' } }),
      event(2, 'assistant/message', 'I will inspect the file', { reasoning: 'Selecting the safest tool.' }),
      event(3, 'tool/call', 'read_file', { tool_name: 'read_file', tool_args: '{"path":"a"}', call_id: 'c-2' }),
      event(4, 'tool/result', 'ok', { tool_name: 'read_file', tool_output: 'contents', call_id: 'c-2' }),
    ])

    expect(snapshot.chat.order).toEqual(['event:2:v1', 'event:4:v1'])
    expect(snapshot.nodes[0]).toMatchObject({
      id: 'event:2:v1',
      kind: 'assistant',
      toolCalls: [{ id: 'c-2', name: 'read_file', argsRaw: '{"path":"a"}' }],
    })
    expect(snapshot.nodes[1]).toMatchObject({ id: 'event:4:v1', assistantId: 'event:2:v1', call: { name: 'read_file' } })
    expect(snapshot.chat.nodes.get('event:2:v1')).toBe(snapshot.nodes[0])
  })

  it('keeps empty message nodes addressable', () => {
    const snapshot = projectShutuConversation([event(1, 'user/message', '')])
    expect(snapshot.nodes[0]).toMatchObject({ id: 'event:1:v1', kind: 'user', text: '' })
  })
})
