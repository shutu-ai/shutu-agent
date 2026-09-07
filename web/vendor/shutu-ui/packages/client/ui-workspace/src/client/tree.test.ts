import { describe, expect, it } from 'vitest'
import type { SessionListState, WorkspaceView } from '@shutu-ai/client-runtime/client'
import { deriveGroups, UNGROUPED_KEY, UNGROUPED_LABEL } from './tree.ts'

function emptySessionList(): SessionListState {
  return {
    ids: [],
    byId: {},
    current: undefined,
    phase: 'ready',
    state: 'idle',
    error: null,
    subagentsByParent: {},
    jobsBySession: {},
    currentAddress: undefined,
  }
}

describe('deriveGroups', () => {
  it('keeps the default Ungrouped group visible before the first session exists', () => {
    expect(deriveGroups(emptySessionList(), [], [], { expandedGroups: [] })).toEqual([
      expect.objectContaining({
        key: UNGROUPED_KEY,
        workspaceId: undefined,
        label: UNGROUPED_LABEL,
        sessionCount: 0,
        sessions: [],
      }),
    ])
  })

  it('places Ungrouped after real workspaces', () => {
    const workspace: WorkspaceView = {
      workspaceId: 'w-project',
      path: 'C:/project',
      title: 'project',
      sessionIds: [],
      createdAt: new Date(0).toISOString(),
      updatedAt: new Date(0).toISOString(),
    }

    expect(deriveGroups(emptySessionList(), [workspace], [], { expandedGroups: [] }).map(group => group.key))
      .toEqual(['w-project', UNGROUPED_KEY])
  })
})
