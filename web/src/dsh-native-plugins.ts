/** Type contract for the Vite-generated native DSH plugin roster. */

export interface DshNativePluginModule {
  readonly apply?: (context: unknown) => void
  readonly inject?: readonly string[]
}

export interface DshNativePluginRegistration {
  readonly id: string
  readonly module: DshNativePluginModule
}
