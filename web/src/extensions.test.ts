import { describe, expect, it } from 'vitest'
import type { ExtensionInventory } from './api'
import { extensionNavigationItems } from './extensions'

const extension = (id: string, values: Partial<ExtensionInventory> = {}): ExtensionInventory => ({
  extensionId: id,
  title: id,
  route: `/extensions/${id}/`,
  navigationEnabled: true,
  navigationGroup: 'Extensions',
  order: 1000,
  ready: true,
  ...values,
})

describe('extension navigation', () => {
  it('sorts web contributions and supplies fallback presentation metadata', () => {
    const items = extensionNavigationItems([
      extension('zeta', { title: '', order: 20 }),
      extension('alpha', { icon: '📊', navigationGroup: 'Data', order: 20 }),
      extension('middle', { order: 10 }),
    ])
    expect(items.map(item => item.id)).toEqual(['alpha', 'middle', 'zeta'])
    expect(items[1]).toMatchObject({ icon: '🧩', group: 'Extensions', ready: true })
    expect(items[0]).toMatchObject({ icon: '📊', group: 'Data' })
  })

  it('hides disabled and malformed routes, and marks unhealthy contributions unavailable', () => {
    const items = extensionNavigationItems([
      extension('visible'),
      extension('disabled', { navigationEnabled: false }),
      extension('bad-route', { route: '/elsewhere/' }),
      extension('unhealthy', { ready: false }),
    ])
    expect(items.map(item => item.id)).toEqual(['unhealthy', 'visible'])
    expect(items[0].ready).toBe(false)
  })
})
