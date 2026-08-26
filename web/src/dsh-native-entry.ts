import { AppWebEntry } from '@deepseek-ai/dsh-client-web'

/** Build-time switch for the DSH-native UI entry. The legacy Shutu shell stays
 * available until the host protocol and plugin manifest are ready. */
declare const __SHUTU_DSH_NATIVE__: boolean

interface DshBootWindow extends Window {
  __ModuleLoader__?: unknown
  __DSH_BOOT__?: unknown
}

/** Return whether the host has installed the two DSH boot contracts. */
export function hasDshNativeBoot(windowObject: Window = window): boolean {
  const dshWindow = windowObject as DshBootWindow
  return dshWindow.__ModuleLoader__ !== undefined && dshWindow.__DSH_BOOT__ !== undefined
}

/** Mount the DSH Cordis/plugin UI once the host boot contract is present. */
export async function mountDshNativeApp(container: HTMLElement): Promise<void> {
  if (!hasDshNativeBoot()) {
    throw new Error('shutu web: DSH native boot contract is unavailable; configure the DSH host bridge first')
  }
  await new AppWebEntry(container).run()
}

/** Select the native entry only in an explicitly enabled native build. */
export function isDshNativeBuild(): boolean {
  return __SHUTU_DSH_NATIVE__
}
