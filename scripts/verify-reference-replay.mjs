#!/usr/bin/env node
/**
 * Replay the shared core-turn fixture through the reference project's real Session
 * implementation. Run with the reference repository's tsx loader, because
 * the checkout intentionally keeps packages in source form:
 *
 *   node --import <reference>/node_modules/tsx/dist/loader.mjs \
 *     scripts/verify-reference-replay.mjs <fixture.json> <reference-root>
 *
 * The output is deliberately small and stable: the ordered surface node
 * positions and the model-facing message projection. A Go contract test owns
 * the other half of this comparison.
 */

import fs from 'node:fs/promises'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

const [fixturePath, referenceRoot] = process.argv.slice(2)
if (!fixturePath || !referenceRoot) {
  throw new Error('usage: verify-reference-replay.mjs <fixture.json> <reference-root>')
}

const fixture = JSON.parse(await fs.readFile(fixturePath, 'utf8'))
const sessionModule = await import(pathToFileURL(path.join(referenceRoot, 'packages/core/session/src/index.ts')).href)
const { Session, SessionId } = sessionModule

function referenceEvent(source, index) {
  const data = structuredClone(source.data)
  // The Go adapter's durable envelope is 1-based and keeps turn/step routing
  // fields on user/message. Reference Session uses a 0-based log and stores
  // user messages as the UserMessage payload itself.
  if (source.type === 'user/message') {
    delete data.turn
    delete data.step
  }
  const event = {
    type: source.type,
    seq: index,
    time: source.time,
    data,
  }
  if (source.surfaceOp !== undefined) event.surfaceOp = source.surfaceOp
  if (Array.isArray(source.sourceEventSeqs)) {
    event.sourceEventSeqs = source.sourceEventSeqs.map(value => value - 1)
  }
  return event
}

const session = Session.create(
  SessionId('contract-fixture'),
  fixture.map(referenceEvent),
)
const history = session.deriveMessages().map(message => {
  const first = message.content[0]
  if (message.role === 'user' && first?.type === 'tool-result') {
    return {
      role: 'tool',
      content: first.content,
      toolCallId: first.toolCallId,
      isError: first.isError,
    }
  }
  return { role: message.role, content: message.content }
})
process.stdout.write(`${JSON.stringify({ surface: session.surface.nodes, history })}\n`)
