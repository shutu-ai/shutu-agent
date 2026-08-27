import { AppWebEntry } from '@deepseek-ai/dsh-client-web'
import * as clientModules from '@deepseek-ai/dsh-client-modules/client'
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

const dshModulesID = '@deepseek-ai/dsh-client-modules'

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
    rev: 'shutu-native-p36-4',
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
    const dialogMounted = documentObject.querySelector('[role="dialog"]') !== null
    if (dialogMounted) {
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
  apply()
  const observer = new MutationObserver(apply)
  observer.observe(documentObject.body, { childList: true, subtree: true })
  documentObject.addEventListener('click', rememberDialogTrigger, true)
  return () => {
    observer.disconnect()
    documentObject.removeEventListener('click', rememberDialogTrigger, true)
  }
}

/** Report the build marker used by the native manifest and diagnostics. */
export function isDshNativeBuild(): boolean {
  return __SHUTU_DSH_NATIVE__
}
