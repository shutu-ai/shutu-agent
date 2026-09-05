import { AppWebEntry } from '@shutu-ai/client-web'
import * as clientModules from '@shutu-ai/client-modules/client'
import type { ShutuNativePluginRegistration } from './native-plugins'

/** Build-time marker retained for diagnostics; the production entry is always
 * the SHUTU React/Cordis shell. */
declare const __SHUTU_UI_NATIVE__: boolean

interface ShutuBootWindow extends Window {
  __ModuleLoader__?: ShutuModuleLoaderTarget
  __SHUTU_BOOT__?: unknown
}

interface ShutuModuleLoaderTarget {
  mode: 'queue' | 'live'
  pendingQueue: Array<{ id: string; factory: (require: (specifier: string) => unknown) => Record<string, unknown> }>
  load(registration: { id: string; factory: (require: (specifier: string) => unknown) => Record<string, unknown> }): void
  create(options: { boot: unknown; staticModules: Record<string, unknown> }): unknown
}

const shutuModulesID = '@shutu-ai/client-modules'
const shutuLogoPath = '/new-logo-b.png'
const productHeroHeadline = '智行未至之境'
const upstreamHeroHeadline = '探索未至之境'
const productHeroHeadlineEnglish = 'Beyond the Known'
const upstreamHeroHeadlineEnglish = 'Into the Unknown'

const shutuLogoStyle = `
  [data-slot="sidebar.brand.mark"] img[data-shutu-logo],
  [data-slot="conversation.hero.brand.mark"] img[data-shutu-logo] {
    display: block;
    object-fit: contain;
    object-position: center;
  }
  [data-slot="sidebar.brand.mark"] img[data-shutu-logo] {
    width: 24px;
    height: 24px;
  }
  [data-slot="conversation.hero.brand.mark"] img[data-shutu-logo] {
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

function getShutuLogoPath(): string {
  return shutuLogoPath
}

function createShutuLogo(documentObject: Document, src: string): HTMLImageElement {
  const image = documentObject.createElement('img')
  image.src = src
  image.alt = ''
  image.setAttribute('aria-hidden', 'true')
  image.setAttribute('data-shutu-logo', 'true')
  return image
}

/** Replace SHUTU's inline fish/wordmark with Shutu's theme-aware branding. */
export function installShutuNativeLogoBridge(documentObject: Document = document): () => void {
  const style = documentObject.createElement('style')
  // Replace the vendored shell's inline mark with Shutu's branding.
  style.textContent = shutuLogoStyle
  documentObject.head.append(style)

  const apply = (): void => {
    const logoPath = getShutuLogoPath()
    for (const slot of documentObject.querySelectorAll<HTMLElement>(
      '[data-slot="sidebar.brand.mark"], [data-slot="conversation.hero.brand.mark"]',
    )) {
      let image = slot.querySelector<HTMLImageElement>('img[data-shutu-logo]')
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
        return element !== markParent && (text === upstreamHeroHeadline || text === upstreamHeroHeadlineEnglish)
      })
      if (headlineText instanceof HTMLElement) {
        headlineText.textContent = headlineText.textContent?.trim() === upstreamHeroHeadlineEnglish
          ? productHeroHeadlineEnglish
          : productHeroHeadline
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
      const isDarkTheme = false
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
export function installShutuNativeBoot(
  windowObject: Window = window,
  plugins: readonly ShutuNativePluginRegistration[] = [],
): void {
  const shutuWindow = windowObject as ShutuBootWindow
  if (shutuWindow.__ModuleLoader__ !== undefined && shutuWindow.__SHUTU_BOOT__ !== undefined) return

  const nativeBootEntries = [
    { id: shutuModulesID, url: '', rev: 'inline', external: [], inject: [], immediately: true },
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

  const target: ShutuModuleLoaderTarget = {
    mode: 'queue',
    pendingQueue: [],
    load(registration) {
      if (this.mode === 'queue') this.pendingQueue.push(registration)
      else throw new Error(`shutu web: unexpected late bootstrap registration for ${registration.id}`)
    },
    create(options) {
      return clientModules.createClientModuleSystem(
        target,
        { id: shutuModulesID, exports: clientModules },
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
  shutuWindow.__ModuleLoader__ = target
  shutuWindow.__SHUTU_BOOT__ = {
    rev: 'shutu-native-namespace-v1',
    entries: nativeBootEntries,
  }
}

/** Return whether the host has installed the two SHUTU boot contracts. */
export function hasShutuNativeBoot(windowObject: Window = window): boolean {
  const shutuWindow = windowObject as ShutuBootWindow
  return shutuWindow.__ModuleLoader__ !== undefined && shutuWindow.__SHUTU_BOOT__ !== undefined
}

/** Mount the SHUTU Cordis/plugin UI once the host boot contract is present. */
export async function mountShutuNativeApp(container: HTMLElement): Promise<void> {
  const { SHUTU_NATIVE_PLUGINS } = await import('virtual:shutu-native-plugins')
  installShutuNativeBoot(window, SHUTU_NATIVE_PLUGINS)
  if (!hasShutuNativeBoot()) {
    throw new Error('shutu web: SHUTU native boot contract is unavailable; configure the SHUTU host bridge first')
  }
  installShutuNativeAccessibilityBridge()
  installShutuNativeLogoBridge()
  await new AppWebEntry(container).run()
}

/**
 * Keep the compact SHUTU settings trigger discoverable to assistive technology.
 * The upstream shell intentionally renders only the icon in the rail and does
 * not expose a label on that button. This host-owned bridge repairs the DOM
 * contract without changing the read-only SHUTU source tree.
 */
export function installShutuNativeAccessibilityBridge(documentObject: Document = document): () => void {
  let lastDialogTrigger: HTMLButtonElement | null = null
  let dialogWasMounted = false
  let restoreFrame: number | null = null
  let restoreAttempts = 0
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
    if (!dialogWasMounted || lastDialogTrigger === null) return
    dialogWasMounted = false
    const restoreTrigger = (): void => {
      restoreFrame = null
      if (documentObject.querySelector('[role="dialog"]') !== null) return
      const replacementTrigger = lastDialogTrigger === null ? undefined : [...documentObject.querySelectorAll<HTMLButtonElement>(
        'button[aria-haspopup="dialog"]',
      )].find(button => button.getAttribute('aria-label') === lastDialogTrigger?.getAttribute('aria-label')
        && (button.offsetWidth > 0 || button.offsetHeight > 0))
      const trigger = (lastDialogTrigger?.isConnected === true ? lastDialogTrigger : replacementTrigger)
        ?? lastDialogTrigger
      if (trigger?.isConnected === true) {
        trigger.focus()
        return
      }
      // React can replace the compact/mobile trigger while closing the dialog.
      // Retry through the next frames so focus lands on its connected
      // replacement instead of silently falling back to the removed node.
      if (restoreAttempts < 10) {
        restoreAttempts += 1
        restoreFrame = documentObject.defaultView?.requestAnimationFrame(restoreTrigger) ?? null
      }
    }
    restoreAttempts = 0
    restoreFrame = documentObject.defaultView?.requestAnimationFrame(restoreTrigger) ?? null
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
    if (restoreFrame !== null) documentObject.defaultView?.cancelAnimationFrame(restoreFrame)
    restoreFrame = null
    observer.disconnect()
    documentObject.removeEventListener('click', rememberDialogTrigger, true)
    documentObject.removeEventListener('keydown', trapDialogFocus, true)
  }
}

/** Report the build marker used by the native manifest and diagnostics. */
export function isShutuNativeBuild(): boolean {
  return __SHUTU_UI_NATIVE__
}
