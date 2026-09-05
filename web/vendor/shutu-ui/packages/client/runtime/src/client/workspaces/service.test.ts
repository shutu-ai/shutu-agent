import { describe, expect, it, vi } from 'vitest'
import type { Context } from '@shutu-ai/cordis'
import type { IApiClient, WorkspaceView } from '@shutu-ai/api-remotes/client'
import { createSnapshotStore } from '../contract/store.ts'
import type { SessionsPort, SessionsPortList } from '../contract/sessions-port.ts'
import { WorkspaceRuntime } from './service.ts'

describe('WorkspaceRuntime', () => {
  it('reuses a just-created blank session before host membership reaches the snapshot', async () => {
    const now = new Date().toISOString()
    const workspace: WorkspaceView = {
      workspaceId: 'w-new', path: 'C:/project', title: 'project', sessionIds: [],
      createdAt: now, updatedAt: now,
    }
    const list = createSnapshotStore<SessionsPortList>({
      ids: [], byId: {}, current: undefined, phase: 'ready',
      subagentsByParent: {}, jobsBySession: {}, currentAddress: undefined,
    })
    let sessionCount = 0
    const sessions: SessionsPort = {
      list,
      create: vi.fn(async () => {
        const sessionId = `s-${++sessionCount}`
        list.update(draft => {
          draft.ids.push(sessionId)
          draft.byId[sessionId] = { id: sessionId, blank: true, updatedAt: Date.now() }
        })
        return sessionId
      }),
      open: vi.fn(),
      clear: vi.fn(),
    }
    const api = {
      workspace: {
        create: vi.fn(async () => ({ result: { ok: true, value: { workspace, created: true } } })),
      },
    } as unknown as IApiClient
    const runtime = new WorkspaceRuntime({ reflect: { provide: vi.fn() } } as unknown as Context, api, sessions)

    const created = await runtime.create({ path: workspace.path })
    const first = await runtime.connectWorkspace(created.workspaceId)
    const second = await runtime.connectWorkspace(created.workspaceId)

    expect(first).toBe('s-1')
    expect(second).toBe(first)
    expect(sessions.create).toHaveBeenCalledTimes(1)
  })

  it('creates an explicit Ungrouped session without inheriting recent workspace', async () => {
    const list = createSnapshotStore<SessionsPortList>({
      ids: ['s-named'],
      byId: { 's-named': { id: 's-named', blank: false, updatedAt: Date.now() } },
      current: 's-named', phase: 'ready',
      subagentsByParent: {}, jobsBySession: {}, currentAddress: undefined,
    })
    const sessions: SessionsPort = {
      list,
      create: vi.fn(async () => 's-ungrouped'),
      open: vi.fn(),
      clear: vi.fn(),
    }
    const runtime = new WorkspaceRuntime(
      { reflect: { provide: vi.fn() } } as unknown as Context,
      {} as unknown as IApiClient,
      sessions,
    )

    runtime.startSession(null)
    await vi.waitFor(() => expect(sessions.open).toHaveBeenCalledWith('s-ungrouped'))

    expect(sessions.create).toHaveBeenCalledWith({})
  })
})
