declare module '@shutu-ai/client-modules/client' {
  export function createClientModuleSystem(
    target: unknown,
    bootstrapModule: { id: string; exports: Record<string, unknown> },
    options: { boot: unknown; staticModules: Record<string, unknown> },
  ): unknown
}
