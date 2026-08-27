declare module '@shutu-ai/dsh-client-web' {
  export class AppWebEntry {
    constructor(container: HTMLElement)
    run(): Promise<void>
  }
}
