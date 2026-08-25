import { describe, expect, it } from 'vitest'
import { projectDshConversation } from './dsh-conversation'
import type { EventView } from './api'

function event(seq: number, type: string, summary: string, extra: Partial<EventView> = {}): EventView {
  return { seq, type, version: 1, time: `2026-08-25T00:00:0${seq}Z`, summary, ...extra }
}

describe('DSH conversation carrier', () => {
  it('folds raw events into DSH chat nodes and pairs tool results', () => {
    const snapshot = projectDshConversation([
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
    const snapshot = projectDshConversation([
      event(1, 'tool/call', 'shell', { tool_name: 'shell', call_id: 'c2' }),
    ])
    expect(snapshot.running).toBe(true)
    expect(snapshot.runningCalls).toMatchObject([{ callId: 'c2', name: 'shell' }])
    expect(snapshot.nodes).toMatchObject([{ kind: 'tool-running', seq: 1 }])
  })
})
