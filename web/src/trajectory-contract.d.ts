declare module '@shutu-ai/trajectory' {
  export type ShutuTimelineMode = 'sequence' | 'duration' | 'time' | 'actual'

  export interface ShutuCell {
    index: number
    kind: string
    text: string
    timeSeconds: number | null
    startedAt?: number | null
    sourceSeq?: number
    inputDetail?: string
    outputDetail?: string
    result?: string
    isError?: boolean
    callId?: string
    input?: number
    output?: number
    think?: number
  }

  export interface ShutuTurn {
    turn: number | null
    groups: readonly { title: string; cells: readonly ShutuCell[] }[]
  }

  export interface ShutuTimelineSpan {
    start: number
    end: number
    index: number
    isError: boolean
    kind: string
    label: string
    lane: number
  }

  export interface ShutuTimelineModel {
    start: number
    end: number
    spans: readonly ShutuTimelineSpan[]
    turnBoundaries: readonly { turn: number; time: number }[]
  }

  export function deriveTrajectoryTimeline(turns: readonly ShutuTurn[], mode?: ShutuTimelineMode): ShutuTimelineModel | null
  export function trajectoryTimelineFocusIndexes(turns: readonly ShutuTurn[], range: { start: number; end: number }, mode?: ShutuTimelineMode): ReadonlySet<number>
}
