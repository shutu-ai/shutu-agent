import type { ExtensionInventory } from './api'

export interface ExtensionNavigationItem {
  id: string
  title: string
  route: string
  icon: string
  group: string
  order: number
  ready: boolean
}

const DEFAULT_EXTENSION_ICON = '🧩'
const DEFAULT_EXTENSION_GROUP = 'Extensions'

// Keep inventory normalization out of the React tree so unavailable, disabled,
// malformed or unordered extensions have one tested behavior.
export function extensionNavigationItems(inventory: readonly ExtensionInventory[]): readonly ExtensionNavigationItem[] {
  const items: ExtensionNavigationItem[] = []
  for (const extension of inventory) {
    if (!extension.navigationEnabled) continue
    if (!extension.route.startsWith(`/extensions/${encodeURIComponent(extension.extensionId)}/`)) continue
    items.push({
      id: extension.extensionId,
      title: extension.title || extension.extensionId,
      route: extension.route,
      icon: extension.icon || DEFAULT_EXTENSION_ICON,
      group: extension.navigationGroup || DEFAULT_EXTENSION_GROUP,
      order: typeof extension.order === 'number' && Number.isFinite(extension.order) ? extension.order : 1000,
      ready: extension.ready === true,
    })
  }
  return items.sort((left, right) =>
    left.group.localeCompare(right.group) || left.order - right.order ||
    left.title.localeCompare(right.title) || left.id.localeCompare(right.id),
  )
}
