export interface VirtualRange {
  start: number
  end: number
}

export function buildVirtualOffsets(keys: readonly string[], measured: ReadonlyMap<string, number>, estimate: number): readonly number[] {
  const offsets = [0]
  for (const key of keys) {
    offsets.push((offsets[offsets.length - 1] ?? 0) + Math.max(1, measured.get(key) ?? estimate))
  }
  return offsets
}

export function upperBound(offsets: readonly number[], value: number): number {
  let low = 0
  let high = offsets.length
  while (low < high) {
    const middle = Math.floor((low + high) / 2)
    if ((offsets[middle] ?? 0) <= value) low = middle + 1
    else high = middle
  }
  return Math.max(0, low - 1)
}

export function virtualRange(offsets: readonly number[], scrollTop: number, viewportHeight: number, overscan: number): VirtualRange {
  const count = Math.max(0, offsets.length - 1)
  if (count === 0) return { start: 0, end: 0 }
  const start = Math.max(0, upperBound(offsets, Math.max(0, scrollTop)) - overscan)
  const end = Math.min(count, upperBound(offsets, Math.max(0, scrollTop) + Math.max(0, viewportHeight)) + overscan + 1)
  return { start, end: Math.max(start, end) }
}
