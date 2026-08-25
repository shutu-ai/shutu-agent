import type { EventView } from './api'

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
  readonly kind: 'user'
  readonly seq: number
  readonly time: number
  readonly text: string
}

export interface DshAssistantNode {
  readonly kind: 'assistant'
  readonly seq: number
  readonly time: number
  readonly turn: number
  readonly step: number
  readonly blocks: readonly { kind: 'text' | 'reasoning'; text: string }[]
  readonly usage?: unknown
  readonly provenance?: { provider: string; model: string }
  readonly timing?: { stepStartTime: number | null; firstTokenTime: number | null; completedTime: number }
}

export interface DshToolResultNode {
  readonly kind: 'tool-result'
  readonly seq: number
  readonly time: number
  readonly callId: string
  readonly call: { name: string; argsRaw: string } | null
  readonly content: string
  readonly isError: boolean
}

export interface DshToolRunningNode {
  readonly kind: 'tool-running'
  readonly seq: number
  readonly time: number
  readonly callId: string
  readonly name: string
  readonly argsRaw: string
}

export interface DshContextNode {
  readonly kind: 'context'
  readonly seq: number
  readonly time: number
  readonly text: string
  readonly source: string
}

export interface DshCompactionNode {
  readonly kind: 'compaction'
  readonly seq: number
  readonly time: number
  readonly summary: string
  readonly shadowedTokenCount: number | null
}

export interface DshUnknownNode {
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
  readonly callId: string
  readonly seq: number
  readonly name: string
  readonly argsRaw: string
  readonly time: number
}

function objectDetails(event: EventView): Record<string, unknown> {
  return event.details && typeof event.details === 'object' ? event.details : {}
}

function textOf(event: EventView): string {
  return event.tool_output || event.reasoning || event.summary || event.compaction_summary || ''
}

function numberOf(event: EventView, key: string): number | undefined {
  const value = objectDetails(event)[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

function timeOf(event: EventView): number {
  const value = Date.parse(event.time)
  return Number.isFinite(value) ? value : 0
}

function nodeKey(node: DshConversationNode): string {
  return `${node.kind}:${node.seq}`
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

  const add = (node: DshConversationNode): void => {
    nodes.push(node)
    nodeMap.set(nodeKey(node), node)
  }

  for (const event of events) {
    const time = timeOf(event)
    if (event.type === 'turn/start') {
      currentTurn = numberOf(event, 'turn') ?? currentTurn + 1
      turns.set(currentTurn, { turn: currentTurn, startTime: time })
      continue
    }
    if (event.type === 'turn/end') {
      const turn = numberOf(event, 'turn') ?? currentTurn
      const previous = turns.get(turn) ?? { turn, startTime: null }
      turns.set(turn, { ...previous, endTime: time })
      turnEnds.set(turn, event.seq)
      continue
    }
    if (event.type === 'user/message') {
      if (event.context_message) {
        add({ kind: 'context', seq: event.seq, time, text: textOf(event), source: 'context' })
      } else {
        add({ kind: 'user', seq: event.seq, time, text: textOf(event) })
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
      add({
        kind: 'assistant', seq: event.seq, time, turn: numberOf(event, 'turn') ?? currentTurn,
        step: numberOf(event, 'step') ?? 0, blocks,
        ...(details.usage === undefined ? {} : { usage: details.usage }),
        ...(provider !== undefined && model !== undefined ? { provenance: { provider, model } } : {}),
        timing: { stepStartTime: null, firstTokenTime: null, completedTime: time },
      })
      continue
    }
    if (event.type === 'tool/call' || event.type === 'tool/start') {
      const callId = event.call_id || `call:${event.seq}`
      calls.set(callId, { callId, seq: event.seq, name: event.tool_name || event.summary, argsRaw: event.tool_args || '', time })
      continue
    }
    if (event.type === 'tool/result' || event.type === 'tool/error') {
      const callId = event.call_id || `call:${event.seq}`
      const call = calls.get(callId)
      calls.delete(callId)
      add({
        kind: 'tool-result', seq: event.seq, time, callId,
        call: call === undefined ? null : { name: call.name, argsRaw: call.argsRaw },
        content: textOf(event), isError: event.type === 'tool/error',
      })
      continue
    }
    if (event.type === 'compaction/summary') {
      add({
        kind: 'compaction', seq: event.seq, time, summary: event.compaction_summary || event.summary,
        shadowedTokenCount: event.compaction_tokens ?? numberOf(event, 'shadowedTokens') ?? null,
      })
      continue
    }
    if (event.type === 'assistant/chunk' || event.type === 'assistant/reasoning' || event.type.startsWith('llm/')) continue
    if (event.type === 'step/start' || event.type === 'step/end') continue
    add({ kind: 'unknown', seq: event.seq, time, type: event.type, text: textOf(event) })
  }

  for (const call of calls.values()) {
    add({ kind: 'tool-running', seq: call.seq, time: call.time, callId: call.callId, name: call.name, argsRaw: call.argsRaw })
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
