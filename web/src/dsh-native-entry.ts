import { AppWebEntry } from '@shutu-ai/dsh-client-web'
import * as clientModules from '@shutu-ai/dsh-client-modules/client'
import type { DshNativePluginRegistration } from './dsh-native-plugins'

/** Build-time marker retained for diagnostics; the production entry is always
 * the DSH React/Cordis shell. */
declare const __SHUTU_DSH_NATIVE__: boolean

interface DshBootWindow extends Window {
  __ModuleLoader__?: DshModuleLoaderTarget
  __DSH_BOOT__?: unknown
}

interface DshModuleLoaderTarget {
  mode: 'queue' | 'live'
  pendingQueue: Array<{ id: string; factory: (require: (specifier: string) => unknown) => Record<string, unknown> }>
  load(registration: { id: string; factory: (require: (specifier: string) => unknown) => Record<string, unknown> }): void
  create(options: { boot: unknown; staticModules: Record<string, unknown> }): unknown
}

const dshModulesID = '@shutu-ai/dsh-client-modules'
const shutuLogoBlackPath = '/logo-b.png'
const shutuLogoWhitePath = '/logo-w.png'
const shutuHeroHeadline = '智行未至之境'
const dshHeroHeadline = '探索未至之境'
const shutuHeroHeadlineEnglish = 'Beyond the Known'
const dshHeroHeadlineEnglish = 'Into the Unknown'

const shutuLogoStyle = `
  [data-slot="sidebar.brand.mark"] img[data-shutu-dsh-logo],
  [data-slot="conversation.hero.brand.mark"] img[data-shutu-dsh-logo] {
    display: block;
    object-fit: contain;
    object-position: center;
  }
  [data-slot="sidebar.brand.mark"] img[data-shutu-dsh-logo] {
    width: 24px;
    height: 24px;
  }
  [data-slot="conversation.hero.brand.mark"] img[data-shutu-dsh-logo] {
    width: 42px;
    height: 42px;
  }
  [data-slot="sidebar.brand.name"] [data-shutu-brand-name] {
    align-items: center;
    color: currentColor;
    display: inline-flex;
    font-size: 15px;
    font-weight: 650;
    gap: 5px;
    letter-spacing: -0.02em;
    line-height: 1;
    white-space: nowrap;
  }
  [data-slot="sidebar.brand.name"] [data-shutu-brand-badge] {
    background: #181818;
    border-radius: 4px;
    color: #ffffff;
    font-size: 9px;
    font-weight: 750;
    letter-spacing: 0.04em;
    line-height: 1;
    padding: 3px 4px 2px;
  }
`

function getDshThemeLogoPath(documentObject: Document): string {
  const colorScheme = documentObject.documentElement.style.colorScheme
    || documentObject.defaultView?.getComputedStyle(documentObject.documentElement).colorScheme
    || ''
  return colorScheme.includes('dark') ? shutuLogoWhitePath : shutuLogoBlackPath
}

function createShutuLogo(documentObject: Document, src: string): HTMLImageElement {
  const image = documentObject.createElement('img')
  image.src = src
  image.alt = ''
  image.setAttribute('aria-hidden', 'true')
  image.setAttribute('data-shutu-dsh-logo', 'true')
  return image
}

/** Replace DSH's inline fish/wordmark with Shutu's theme-aware branding. */
export function installDshNativeLogoBridge(documentObject: Document = document): () => void {
  const style = documentObject.createElement('style')
  style.setAttribute('data-shutu-dsh-logo-style', 'true')
  style.textContent = shutuLogoStyle
  documentObject.head.append(style)

  const apply = (): void => {
    const logoPath = getDshThemeLogoPath(documentObject)
    for (const slot of documentObject.querySelectorAll<HTMLElement>(
      '[data-slot="sidebar.brand.mark"], [data-slot="conversation.hero.brand.mark"]',
    )) {
      let image = slot.querySelector<HTMLImageElement>('img[data-shutu-dsh-logo]')
      if (image === null) {
        image = createShutuLogo(documentObject, logoPath)
        slot.replaceChildren(image)
      } else if (image.src !== new URL(logoPath, documentObject.baseURI).href) {
        image.src = logoPath
      }

      if (!slot.matches('[data-slot="conversation.hero.brand.mark"]')) continue
      const headline = slot.parentElement?.parentElement
      const markParent = slot.parentElement
      if (headline === null || headline === undefined || markParent === null) continue
      if (markParent instanceof HTMLElement) {
        markParent.style.display = 'inline-flex'
        markParent.style.alignItems = 'center'
        markParent.style.justifyContent = 'center'
        markParent.style.width = '42px'
        markParent.style.height = '42px'
        markParent.style.flex = '0 0 42px'
      }
      const headlineText = [...headline.children].find(element => {
        const text = element.textContent?.trim()
        return element !== markParent && (text === dshHeroHeadline || text === dshHeroHeadlineEnglish)
      })
      if (headlineText instanceof HTMLElement) {
        headlineText.textContent = headlineText.textContent?.trim() === dshHeroHeadlineEnglish
          ? shutuHeroHeadlineEnglish
          : shutuHeroHeadline
        headlineText.setAttribute('data-shutu-hero-headline', 'true')
      }
    }

    for (const slot of documentObject.querySelectorAll<HTMLElement>('[data-slot="sidebar.brand.name"]')) {
      let label = slot.querySelector<HTMLElement>('[data-shutu-brand-name]')
      if (label === null) {
        label = documentObject.createElement('span')
        label.setAttribute('data-shutu-brand-name', 'true')
        slot.replaceChildren(label)
      }
      const existingBadge = label.querySelector<HTMLElement>('[data-shutu-brand-badge]')
      const isDarkTheme = logoPath === shutuLogoWhitePath
      if (existingBadge !== null) {
        existingBadge.style.backgroundColor = isDarkTheme ? '#ffffff' : '#181818'
        existingBadge.style.color = isDarkTheme ? '#181818' : '#ffffff'
        continue
      }
      const brandText = documentObject.createElement('span')
      brandText.textContent = 'SHUTU-AI'
      const badge = documentObject.createElement('span')
      badge.textContent = 'AGENT'
      badge.setAttribute('data-shutu-brand-badge', 'true')
      badge.style.backgroundColor = isDarkTheme ? '#ffffff' : '#181818'
      badge.style.color = isDarkTheme ? '#181818' : '#ffffff'
      label.replaceChildren(brandText, badge)
    }
  }

  apply()
  const observer = new MutationObserver(apply)
  observer.observe(documentObject.documentElement, {
    attributes: true,
    attributeFilter: ['class', 'data-theme', 'style'],
    childList: true,
    subtree: true,
  })
  return () => {
    observer.disconnect()
    style.remove()
  }
}

/** Install the inline bootstrap seam used by the native build. */
export function installDshNativeBoot(
  windowObject: Window = window,
  plugins: readonly DshNativePluginRegistration[] = [],
): void {
  const dshWindow = windowObject as DshBootWindow
  if (dshWindow.__ModuleLoader__ !== undefined && dshWindow.__DSH_BOOT__ !== undefined) return

  const nativeBootEntries = [
    { id: dshModulesID, url: '', rev: 'inline', external: [], inject: [], immediately: true },
    ...plugins.map(({ id, module }) => ({
      id,
      url: '',
      rev: 'inline',
      // The modules are already linked into the shell, so no network arrival
      // is needed. Keep the declared Cordis dependencies in the graph for
      // parity and diagnostics while Loader resolves the module objects
      // locally.
      external: [],
      inject: module.inject === undefined ? [] : [...module.inject],
      immediately: true,
    })),
  ]

  const target: DshModuleLoaderTarget = {
    mode: 'queue',
    pendingQueue: [],
    load(registration) {
      if (this.mode === 'queue') this.pendingQueue.push(registration)
      else throw new Error(`shutu web: unexpected late bootstrap registration for ${registration.id}`)
    },
    create(options) {
      return clientModules.createClientModuleSystem(
        target,
        { id: dshModulesID, exports: clientModules },
        options,
      )
    },
  }
  for (const { id, module } of plugins) {
    target.pendingQueue.push({
      id,
      factory: () => module as unknown as Record<string, unknown>,
    })
  }
  dshWindow.__ModuleLoader__ = target
  dshWindow.__DSH_BOOT__ = {
    rev: 'shutu-native-namespace-v1',
    entries: nativeBootEntries,
  }
}

/** Return whether the host has installed the two DSH boot contracts. */
export function hasDshNativeBoot(windowObject: Window = window): boolean {
  const dshWindow = windowObject as DshBootWindow
  return dshWindow.__ModuleLoader__ !== undefined && dshWindow.__DSH_BOOT__ !== undefined
}

/** Mount the DSH Cordis/plugin UI once the host boot contract is present. */
export async function mountDshNativeApp(container: HTMLElement): Promise<void> {
  const { DSH_NATIVE_PLUGINS } = await import('virtual:shutu-dsh-native-plugins')
  installDshNativeBoot(window, DSH_NATIVE_PLUGINS)
  if (!hasDshNativeBoot()) {
    throw new Error('shutu web: DSH native boot contract is unavailable; configure the DSH host bridge first')
  }
  installDshNativeAccessibilityBridge()
  installDshNativeLogoBridge()
  await new AppWebEntry(container).run()
}

/**
 * Keep the compact DSH settings trigger discoverable to assistive technology.
 * The upstream shell intentionally renders only the icon in the rail and does
 * not expose a label on that button. This host-owned bridge repairs the DOM
 * contract without changing the read-only DSH source tree.
 */
export function installDshNativeAccessibilityBridge(documentObject: Document = document): () => void {
  let lastDialogTrigger: HTMLButtonElement | null = null
  let dialogWasMounted = false
  const focusableSelector = [
    'button:not([disabled])', 'input:not([disabled])', 'select:not([disabled])',
    'textarea:not([disabled])', 'a[href]', '[tabindex]:not([tabindex="-1"])',
  ].join(',')
  const rememberDialogTrigger = (event: MouseEvent): void => {
    const target = event.target
    if (!(target instanceof Element)) return
    const trigger = target.closest<HTMLButtonElement>('button[aria-haspopup="dialog"]')
    if (trigger !== null) lastDialogTrigger = trigger
  }
  const apply = (): void => {
    for (const button of documentObject.querySelectorAll<HTMLButtonElement>('button[aria-haspopup="dialog"]')) {
      if (button.getAttribute('aria-label')?.trim() || button.textContent?.trim()) continue
      button.setAttribute('aria-label', 'Settings')
    }
    const dialog = documentObject.querySelector<HTMLElement>('[role="dialog"]')
    const dialogMounted = dialog !== null
    if (dialogMounted) {
      if (!dialogWasMounted && dialog !== null && !dialog.contains(documentObject.activeElement)) {
        dialog.querySelector<HTMLElement>(focusableSelector)?.focus()
      }
      dialogWasMounted = true
      return
    }
    if (!dialogWasMounted || lastDialogTrigger === null || !lastDialogTrigger.isConnected) return
    dialogWasMounted = false
    const trigger = lastDialogTrigger
    requestAnimationFrame(() => {
      if (trigger.isConnected) trigger.focus()
    })
  }
  const trapDialogFocus = (event: KeyboardEvent): void => {
    if (event.key !== 'Tab') return
    const active = documentObject.activeElement
    if (!(active instanceof HTMLElement)) return
    const dialog = active.closest<HTMLElement>('[role="dialog"]')
    if (dialog === null) return
    const focusable = [...dialog.querySelectorAll<HTMLElement>(focusableSelector)]
      .filter(element => element.isConnected)
    if (focusable.length === 0) {
      event.preventDefault()
      return
    }
    const index = focusable.indexOf(active)
    if (index < 0) return
    const next = event.shiftKey
      ? focusable[(index - 1 + focusable.length) % focusable.length]
      : focusable[(index + 1) % focusable.length]
    if (next === undefined) return
    event.preventDefault()
    next.focus()
  }
  apply()
  const observer = new MutationObserver(apply)
  observer.observe(documentObject.body, { childList: true, subtree: true })
  documentObject.addEventListener('click', rememberDialogTrigger, true)
  documentObject.addEventListener('keydown', trapDialogFocus, true)
  return () => {
    observer.disconnect()
    documentObject.removeEventListener('click', rememberDialogTrigger, true)
    documentObject.removeEventListener('keydown', trapDialogFocus, true)
  }
}

/** Report the build marker used by the native manifest and diagnostics. */
export function isDshNativeBuild(): boolean {
  return __SHUTU_DSH_NATIVE__
}
