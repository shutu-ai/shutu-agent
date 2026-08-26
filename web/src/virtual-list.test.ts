import { describe, expect, it } from 'vitest'
import { buildVirtualOffsets, upperBound, virtualRange } from './virtual-list'

describe('dynamic virtual list layout', () => {
  it('uses measured heights and keeps the total canvas height exact', () => {
    const offsets = buildVirtualOffsets(['a', 'b', 'c'], new Map([['a', 80], ['c', 210]]), 100)
    expect(offsets).toEqual([0, 80, 180, 390])
  })

  it('finds a bounded range at both ends of a variable-height list', () => {
    const offsets = [0, 80, 180, 390, 450]
    expect(virtualRange(offsets, 0, 100, 0)).toEqual({ start: 0, end: 2 })
    expect(virtualRange(offsets, 390, 100, 0)).toEqual({ start: 3, end: 4 })
    expect(upperBound(offsets, 180)).toBe(2)
  })

  it('handles an empty list without producing an invalid range', () => {
    expect(buildVirtualOffsets([], new Map(), 100)).toEqual([0])
    expect(virtualRange([0], 0, 640, 8)).toEqual({ start: 0, end: 0 })
  })

  it('preserves an existing row anchor when rows are prepended', () => {
    const previous = buildVirtualOffsets(['b', 'c'], new Map([['b', 120], ['c', 160]]), 100)
    const next = buildVirtualOffsets(['a', 'b', 'c'], new Map([['a', 75], ['b', 120], ['c', 160]]), 100)
    const previousAnchorIndex = upperBound(previous, 120)
    const nextAnchorIndex = ['a', 'b', 'c'].indexOf('c')
    expect(next[nextAnchorIndex]! - previous[previousAnchorIndex]!).toBe(75)
  })
})
