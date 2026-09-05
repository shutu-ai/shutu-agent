declare module '*.css'

declare module 'virtual:shutu-native-plugins' {
  export const SHUTU_NATIVE_PLUGINS: readonly {
    readonly id: string
    readonly module: {
      readonly apply?: (context: unknown) => void
      readonly inject?: readonly string[]
    }
  }[]
}

declare module '@shutu-ai/cordis' {
  export class Context {
    readonly reflect: { provide(name: string, value: unknown): void }
    get(name: string): unknown
  }
}
