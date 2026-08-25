declare module '@shutu-dsh/trajectory' {
  export type DshTimelineMode = 'sequence' | 'duration' | 'time' | 'actual'

  export interface DshCell {
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

  export interface DshTurn {
    turn: number | null
    groups: readonly { title: string; cells: readonly DshCell[] }[]
  }

  export interface DshTimelineSpan {
    start: number
    end: number
    index: number
    isError: boolean
    kind: string
    label: string
    lane: number
  }

  export interface DshTimelineModel {
    start: number
    end: number
    spans: readonly DshTimelineSpan[]
    turnBoundaries: readonly { turn: number; time: number }[]
  }

  export function deriveTrajectoryTimeline(turns: readonly DshTurn[], mode?: DshTimelineMode): DshTimelineModel | null
  export function trajectoryTimelineFocusIndexes(turns: readonly DshTurn[], range: { start: number; end: number }, mode?: DshTimelineMode): ReadonlySet<number>
}
