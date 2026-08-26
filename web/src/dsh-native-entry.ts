import { AppWebEntry } from '@deepseek-ai/dsh-client-web'
import * as clientModules from '@deepseek-ai/dsh-client-modules/client'

/** Build-time switch for the DSH-native UI entry. The legacy Shutu shell stays
 * available until the host protocol and plugin manifest are ready. */
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
export function installDshNativeBoot(windowObject: Window = window): void {
  const dshWindow = windowObject as DshBootWindow
  if (dshWindow.__ModuleLoader__ !== undefined && dshWindow.__DSH_BOOT__ !== undefined) return

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
  dshWindow.__ModuleLoader__ = target
  dshWindow.__DSH_BOOT__ = {
    rev: 'shutu-native-p36-2',
    entries: [{ id: dshModulesID, url: '', rev: 'inline', immediately: true }],
  }
}

/** Return whether the host has installed the two DSH boot contracts. */
export function hasDshNativeBoot(windowObject: Window = window): boolean {
  const dshWindow = windowObject as DshBootWindow
  return dshWindow.__ModuleLoader__ !== undefined && dshWindow.__DSH_BOOT__ !== undefined
}

/** Mount the DSH Cordis/plugin UI once the host boot contract is present. */
export async function mountDshNativeApp(container: HTMLElement): Promise<void> {
  installDshNativeBoot()
  if (!hasDshNativeBoot()) {
    throw new Error('shutu web: DSH native boot contract is unavailable; configure the DSH host bridge first')
  }
  await new AppWebEntry(container).run()
}

/** Select the native entry only in an explicitly enabled native build. */
export function isDshNativeBuild(): boolean {
  return __SHUTU_DSH_NATIVE__
}
