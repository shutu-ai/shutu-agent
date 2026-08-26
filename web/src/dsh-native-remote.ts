/**
 * Native fallback for DSH's generated Remote projection.
 *
 * The source checkout normally supplies this projection during the Host
 * Typert build. shutu-agent's single-binary build cannot write generated files
 * into that checkout, so this adapter keeps the same callable namespace shape
 * and forwards invocations through the already-compatible Connection RPC.
 */

interface NativeConnection {
  readonly rpc: {
    call(channel: string, endpoint: string, payload: unknown, signal?: AbortSignal): Promise<unknown>
  }
}

interface NativeContext {
  get(name: string): unknown
  reflect: { provide(name: string, value: unknown): void }
}

type RemoteListener = (...args: unknown[]) => void

const remoteEvents = new Map<string, Set<RemoteListener>>()
const NATIVE_REMOTE_NAMESPACES = [
  'commands', 'goals', 'dynamicCordisRunner', 'fileReferences',
  'pluginInventory', 'messageFeedback', 'sessionReferenceResolver',
] as const

function namespace(connection: NativeConnection, name: string): object {
  return new Proxy({}, {
    get: (_target, method: string | symbol) => {
      if (typeof method !== 'string') return undefined
      return (...args: unknown[]) => connection.rpc.call('/api', `${name}/${method}`, { args })
    },
  })
}

function remoteFacade(connection: NativeConnection): object {
  const root = {
    async $mount(_contribution: unknown): Promise<() => Promise<void>> {
      return async () => undefined
    },
    $on(event: string, listener: RemoteListener): () => void {
      const listeners = remoteEvents.get(event) ?? new Set<RemoteListener>()
      listeners.add(listener)
      remoteEvents.set(event, listeners)
      return () => { listeners.delete(listener) }
    },
    $dispatch(event: string, args: readonly unknown[]): void {
      for (const listener of remoteEvents.get(event) ?? []) listener(...args)
    },
  }
  return new Proxy(root, {
    get: (target, property: string | symbol) => {
      if (typeof property === 'string' && property in target) return target[property as keyof typeof target]
      if (typeof property === 'string') return namespace(connection, property)
      return undefined
    },
  })
}

export const inject = ['typert', 'connection']

export function apply(ctx: NativeContext): void {
  const connection = ctx.get('connection') as NativeConnection
  ctx.reflect.provide('remote', remoteFacade(connection))
  for (const name of NATIVE_REMOTE_NAMESPACES) {
    ctx.reflect.provide(`remote.${name}`, namespace(connection, name))
  }
}
