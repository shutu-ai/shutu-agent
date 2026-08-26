import { describe, expect, it } from 'vitest'
import type { EventView } from './api'
import { deriveProducedFiles } from './produced-files'

function event(seq: number, type: string, details?: Record<string, unknown>): EventView {
  return { seq, type, version: 1, time: `2026-08-25T00:00:0${seq}Z`, summary: type, details }
}

describe('produced files', () => {
  it('deduplicates writes and attaches them to the following assistant message', () => {
    const produced = deriveProducedFiles([
      event(1, 'user/message'),
      event(2, 'fs/write', { path: 'src/app.tsx' }),
      event(3, 'fs/write', { path: 'src/app.tsx' }),
      event(4, 'fs/write', { path: 'src/styles.css' }),
      event(5, 'assistant/message'),
    ])

    expect(produced.get(5)).toEqual(['src/app.tsx', 'src/styles.css'])
  })

  it('does not carry files across a new user turn or unrelated assistant messages', () => {
    const produced = deriveProducedFiles([
      event(1, 'fs/write', { path: 'before.ts' }),
      event(2, 'user/message'),
      event(3, 'assistant/message'),
      event(4, 'fs/write', { path: 'after.ts' }),
      event(5, 'user/message'),
      event(6, 'fs/write', { path: 'final.ts' }),
      event(7, 'assistant/message'),
    ])

    expect(produced.has(3)).toBe(false)
    expect(produced.get(7)).toEqual(['final.ts'])
  })

  it('ignores malformed or empty paths', () => {
    const produced = deriveProducedFiles([
      event(1, 'fs/write', { path: '' }),
      event(2, 'fs/write', { path: 42 }),
      event(3, 'assistant/message'),
    ])

    expect(produced.size).toBe(0)
  })
})
