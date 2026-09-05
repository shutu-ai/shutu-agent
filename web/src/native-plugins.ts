/** Type contract for the Vite-generated native SHUTU plugin roster. */

export interface ShutuNativePluginModule {
  readonly apply?: (context: unknown) => void
  readonly inject?: readonly string[]
}

export interface ShutuNativePluginRegistration {
  readonly id: string
  readonly module: ShutuNativePluginModule
}
