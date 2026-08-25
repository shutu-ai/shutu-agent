declare module '*.css'

declare module '@deepseek-ai/cordis' {
  export class Context {
    readonly reflect: { provide(name: string, value: unknown): void }
    get(name: string): unknown
  }
}
