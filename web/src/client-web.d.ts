declare module '@shutu-ai/client-web' {
  export class AppWebEntry {
    constructor(container: HTMLElement)
    run(): Promise<void>
  }
}
