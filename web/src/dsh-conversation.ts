import type { EventView, ImageView } from './api'
import { dshEventDetails, dshEventString, normalizeDshEvent } from './dsh-event-model'

/** DSH ConversationSnapshot-compatible surface used by the shutu web client. */
export type DshConversationNode =
  | DshUserNode
  | DshAssistantNode
  | DshToolRunningNode
  | DshToolResultNode
  | DshContextNode
  | DshCompactionNode
  | DshUnknownNode

export interface DshUserNode {
  readonly id: string
  readonly kind: 'user'
  readonly seq: number
  readonly time: number
  readonly text: string
}

export interface DshAssistantNode {
  readonly id: string
  readonly kind: 'assistant'
  readonly seq: number
  readonly time: number
  readonly turn: number
  readonly step: number
  readonly blocks: readonly { kind: 'text' | 'reasoning'; text: string }[]
  readonly toolCalls?: readonly { id: string; name: string; argsRaw: string }[]
  readonly images?: readonly ImageView[]
  readonly requestId?: string
  readonly usage?: unknown
  readonly provenance?: { provider: string; model: string }
  readonly timing?: { stepStartTime: number | null; firstTokenTime: number | null; completedTime: number }
}

export interface DshToolResultNode {
  readonly id: string
  readonly kind: 'tool-result'
  readonly seq: number
  readonly time: number
  readonly callId: string
  readonly call: { name: string; argsRaw: string } | null
  readonly content: string
  readonly isError: boolean
  readonly requestId: string | null
  readonly assistantId?: string
}

export interface DshToolRunningNode {
  readonly id: string
  readonly kind: 'tool-running'
  readonly seq: number
  readonly time: number
  readonly callId: string
  readonly name: string
  readonly argsRaw: string
  readonly requestId: string | null
  readonly assistantId?: string
}

export interface DshContextNode {
  readonly id: string
  readonly kind: 'context'
  readonly seq: number
  readonly time: number
  readonly text: string
  readonly source: string
}

export interface DshCompactionNode {
  readonly id: string
  readonly kind: 'compaction'
  readonly seq: number
  readonly time: number
  readonly summary: string
  readonly shadowedTokenCount: number | null
}

export interface DshUnknownNode {
  readonly id: string
  readonly kind: 'unknown'
  readonly seq: number
  readonly time: number
  readonly type: string
  readonly text: string
}

export interface DshConversationTurn {
  readonly turn: number
  readonly startTime: number | null
  readonly endTime?: number
}

export interface DshConversationSnapshot {
  readonly sessionId: string
  readonly chat: {
    readonly order: readonly string[]
    readonly nodes: ReadonlyMap<string, DshConversationNode>
    readonly timeline: {
      readonly turnOrder: readonly number[]
      readonly turns: ReadonlyMap<number, DshConversationTurn>
    }
  }
  readonly nodes: readonly DshConversationNode[]
  readonly turnTimings: ReadonlyMap<number, DshConversationTurn>
  readonly turnEnds: ReadonlyMap<number, number>
  readonly runningCalls: readonly { callId: string; name: string; argsRaw: string; time: number }[]
  readonly running: boolean
  readonly openState: 'open'
}

interface CallState {
  readonly id: string
  readonly callId: string
  readonly seq: number
  readonly name: string
  readonly argsRaw: string
  readonly time: number
  readonly requestId: string | null
  readonly assistantId: string | null
}

function objectDetails(event: EventView): Record<string, unknown> {
  return dshEventDetails(event)
}

function nodeKey(node: DshConversationNode): string {
  return node.id
}

/**
 * Fold the shutu event view into the same outward shape used by DSH's
 * ConversationSnapshot. The fold intentionally owns pairing and turn
 * boundaries, so React consumers render nodes instead of re-interpreting raw
 * event vocabulary.
 */
export function projectDshConversation(events: readonly EventView[], sessionId = ''): DshConversationSnapshot {
  const nodes: DshConversationNode[] = []
  const nodeMap = new Map<string, DshConversationNode>()
  const calls = new Map<string, CallState>()
  const turns = new Map<number, DshConversationTurn>()
  const turnEnds = new Map<number, number>()
  let currentTurn = 0
  let currentRequestId: string | null = null
  let currentAssistantId: string | null = null
  const nodeIndexById = new Map<string, number>()

  const add = (node: DshConversationNode): void => {
    nodes.push(node)
    nodeMap.set(nodeKey(node), node)
    nodeIndexById.set(node.id, nodes.length - 1)
  }

  const attachToolCall = (assistantId: string | null, call: { id: string; name: string; argsRaw: string }): void => {
    if (assistantId === null) return
    const assistant = nodeMap.get(assistantId)
    const index = nodeIndexById.get(assistantId)
    if (assistant?.kind !== 'assistant' || index === undefined) return
    if (assistant.toolCalls?.some(existing => existing.id === call.id)) return
    const updated = { ...assistant, toolCalls: [...(assistant.toolCalls ?? []), call] }
    nodes[index] = updated
    nodeMap.set(assistantId, updated)
  }

  for (const event of events) {
    const facts = normalizeDshEvent(event, currentRequestId, currentTurn)
    const time = facts.time
    if (event.type === 'turn/start') {
      currentTurn = facts.turn ?? currentTurn + 1
      turns.set(currentTurn, { turn: currentTurn, startTime: time })
      continue
    }
    if (event.type === 'turn/end') {
      const turn = facts.turn ?? currentTurn
      const previous = turns.get(turn) ?? { turn, startTime: null }
      turns.set(turn, { ...previous, endTime: time })
      turnEnds.set(turn, event.seq)
      continue
    }
    if (event.type === 'llm/request_start' || event.type === 'request/header') {
      currentRequestId = facts.requestId ?? `request:${event.seq}`
      continue
    }
    if (event.type === 'llm/request_end') {
      currentRequestId = null
      continue
    }
    // Route capacity is a request fact consumed by the timeline/meter, not a
    // user-visible chat node. The shared model still classifies it as a
    // request so other projections can retain the durable record.
    if (event.type === 'request/context') continue
    if (event.type === 'user/message') {
      if (event.context_message) {
        add({ id: facts.id, kind: 'context', seq: event.seq, time, text: facts.text, source: event.context_source || dshEventString(event, 'source') || 'context' })
      } else {
        add({ id: facts.id, kind: 'user', seq: event.seq, time, text: facts.text })
      }
      continue
    }
    if (event.type === 'assistant/message') {
      const details = objectDetails(event)
      const blocks: { kind: 'text' | 'reasoning'; text: string }[] = []
      if (event.reasoning) blocks.push({ kind: 'reasoning', text: event.reasoning })
      if (event.summary) blocks.push({ kind: 'text', text: event.summary })
      const provider = typeof details.provider === 'string' ? details.provider : undefined
      const model = typeof details.model === 'string' ? details.model : undefined
      const assistantId = facts.id
      currentAssistantId = assistantId
      add({
        id: assistantId, kind: 'assistant', seq: event.seq, time, turn: facts.turn ?? currentTurn,
        step: facts.step ?? 0, blocks,
        ...(event.images && event.images.length > 0 ? { images: event.images } : {}),
        ...(facts.requestId !== null ? { requestId: facts.requestId } : {}),
        ...(details.usage === undefined ? {} : { usage: details.usage }),
        ...(provider !== undefined && model !== undefined ? { provenance: { provider, model } } : {}),
        timing: { stepStartTime: null, firstTokenTime: null, completedTime: time },
      })
      continue
    }
    if (event.type === 'tool/call' || event.type === 'tool/start') {
      const callId = facts.callId || `call:${event.seq}`
      const assistantId = currentAssistantId
      const call = { id: callId, name: facts.toolName, argsRaw: facts.toolArgs }
      attachToolCall(assistantId, call)
      calls.set(callId, { ...call, callId, seq: event.seq, time, requestId: facts.requestId, assistantId })
      continue
    }
    if (event.type === 'tool/result' || event.type === 'tool/error') {
      const callId = facts.callId || `call:${event.seq}`
      const call = calls.get(callId)
      calls.delete(callId)
      add({
        id: facts.id, kind: 'tool-result', seq: event.seq, time, callId,
        call: call === undefined ? null : { name: call.name, argsRaw: call.argsRaw },
        content: facts.text, isError: facts.isError, requestId: call?.requestId ?? facts.requestId,
        ...(call?.assistantId ? { assistantId: call.assistantId } : {}),
      })
      continue
    }
    if (event.type === 'compaction/summary') {
      add({
        id: facts.id, kind: 'compaction', seq: event.seq, time, summary: event.compaction_summary || event.summary,
        shadowedTokenCount: event.compaction_tokens ?? (typeof facts.details.shadowedTokens === 'number' ? facts.details.shadowedTokens : null),
      })
      continue
    }
    if (event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' || event.type.startsWith('llm/')) continue
    if (event.type === 'step/start' || event.type === 'step/end') continue
    add({ id: facts.id, kind: 'unknown', seq: event.seq, time, type: event.type, text: facts.text })
  }

  for (const call of calls.values()) {
    add({ id: call.id, kind: 'tool-running', seq: call.seq, time: call.time, callId: call.callId, name: call.name, argsRaw: call.argsRaw, requestId: call.requestId, ...(call.assistantId ? { assistantId: call.assistantId } : {}) })
  }
  nodes.sort((left, right) => left.seq - right.seq)

  const turnOrder = [...turns.keys()].sort((left, right) => left - right)
  const snapshotTurns = new Map(turns)
  return {
    sessionId,
    chat: { order: nodes.map(nodeKey), nodes: nodeMap, timeline: { turnOrder, turns: snapshotTurns } },
    nodes,
    turnTimings: snapshotTurns,
    turnEnds,
    runningCalls: [...calls.values()],
    running: calls.size > 0,
    openState: 'open',
  }
}
