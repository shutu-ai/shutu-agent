import vm from 'node:vm'
import readline from 'node:readline'

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity })
let startResolve
let startReject
let started = false
const startPromise = new Promise((resolve, reject) => {
  startResolve = resolve
  startReject = reject
})
const pending = new Map()
let nextCallID = 0

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`)
}

rl.on('line', (line) => {
  if (!line.trim()) return
  let message
  try {
    message = JSON.parse(line)
  } catch (error) {
    if (!started) startReject(error)
    return
  }
  if (!started) {
    started = true
    startResolve(message)
    return
  }
  if (message.type !== 'agent_result') return
  const waiter = pending.get(message.id)
  if (!waiter) return
  pending.delete(message.id)
  if (message.error) waiter.reject(fatalError(message.error, 'AGENT_RESULT'))
  else waiter.resolve(message)
})

rl.on('close', () => {
  if (!started) startReject(new Error('workflow host closed before start'))
  for (const waiter of pending.values()) waiter.reject(fatalError('workflow host closed', 'HOST_CLOSED'))
  pending.clear()
})

function fatalError(message, code) {
  const error = new Error(String(message))
  error.fatal = true
  error.code = code
  return error
}

function asString(value, name) {
  if (typeof value !== 'string' || value.length === 0) {
    throw fatalError(`${name}() requires a non-empty string`, 'INVALID_ARGUMENT')
  }
  return value
}

const agentOptionKeys = new Set(['label', 'phase', 'provider', 'model', 'schema'])
const agentDeferredKeys = new Set(['effort', 'isolation', 'agentType'])
const schemaKeys = new Set(['type', 'properties', 'required', 'additionalProperties', 'items', 'enum', 'const', 'oneOf'])
const schemaTypes = new Set(['object', 'array', 'string', 'number', 'integer', 'boolean', 'null'])

function assertObjectSchema(value, path = 'schema') {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw fatalError(`${path} must be an object-rooted JSON Schema`, 'UNSUPPORTED_SCHEMA')
  }
  for (const key of Object.keys(value)) {
    if (!schemaKeys.has(key)) throw fatalError(`${path}.${key} is not supported`, 'UNSUPPORTED_SCHEMA')
  }
  if (value.type !== 'object') throw fatalError(`${path}.type must be "object"`, 'UNSUPPORTED_SCHEMA')
  if (value.properties !== undefined) {
    if (value.properties === null || typeof value.properties !== 'object' || Array.isArray(value.properties)) {
      throw fatalError(`${path}.properties must be an object of schemas`, 'UNSUPPORTED_SCHEMA')
    }
    for (const [name, child] of Object.entries(value.properties)) assertSchemaNode(child, `${path}.properties.${name}`)
  }
  if (value.required !== undefined && (!Array.isArray(value.required) || value.required.some((name) => typeof name !== 'string'))) {
    throw fatalError(`${path}.required must be an array of strings`, 'UNSUPPORTED_SCHEMA')
  }
  if (value.additionalProperties !== undefined && typeof value.additionalProperties !== 'boolean') {
    throw fatalError(`${path}.additionalProperties must be boolean`, 'UNSUPPORTED_SCHEMA')
  }
  if (value.oneOf !== undefined) {
    if (!Array.isArray(value.oneOf) || value.oneOf.length === 0) throw fatalError(`${path}.oneOf must be a non-empty array`, 'UNSUPPORTED_SCHEMA')
    for (const [index, child] of value.oneOf.entries()) assertSchemaNode(child, `${path}.oneOf.${index}`)
  }
}

function assertSchemaNode(value, path) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw fatalError(`${path} must be a schema object`, 'UNSUPPORTED_SCHEMA')
  }
  for (const key of Object.keys(value)) {
    if (!schemaKeys.has(key)) throw fatalError(`${path}.${key} is not supported`, 'UNSUPPORTED_SCHEMA')
  }
  if (typeof value.type !== 'string' || !schemaTypes.has(value.type)) {
    throw fatalError(`${path}.type must be one of ${[...schemaTypes].join('/')}`, 'UNSUPPORTED_SCHEMA')
  }
  if (value.type === 'object') {
    if (value.properties !== undefined) {
      if (value.properties === null || typeof value.properties !== 'object' || Array.isArray(value.properties)) {
        throw fatalError(`${path}.properties must be an object of schemas`, 'UNSUPPORTED_SCHEMA')
      }
      for (const [name, child] of Object.entries(value.properties)) assertSchemaNode(child, `${path}.properties.${name}`)
    }
    if (value.required !== undefined && (!Array.isArray(value.required) || value.required.some((name) => typeof name !== 'string'))) {
      throw fatalError(`${path}.required must be an array of strings`, 'UNSUPPORTED_SCHEMA')
    }
    if (value.additionalProperties !== undefined && typeof value.additionalProperties !== 'boolean') {
      throw fatalError(`${path}.additionalProperties must be boolean`, 'UNSUPPORTED_SCHEMA')
    }
  }
  if (value.type === 'array' && value.items !== undefined) assertSchemaNode(value.items, `${path}.items`)
  if (value.oneOf !== undefined) {
    if (!Array.isArray(value.oneOf) || value.oneOf.length === 0) throw fatalError(`${path}.oneOf must be a non-empty array`, 'UNSUPPORTED_SCHEMA')
    for (const [index, child] of value.oneOf.entries()) assertSchemaNode(child, `${path}.oneOf.${index}`)
  }
  if (value.enum !== undefined && (!Array.isArray(value.enum) || value.enum.length === 0)) {
    throw fatalError(`${path}.enum must be a non-empty array`, 'UNSUPPORTED_SCHEMA')
  }
}

function jsonValue(value) {
  if (value === undefined) return null
  return JSON.parse(JSON.stringify(value))
}

async function execute(request) {
  const limits = request.limits ?? {}
  const maxConcurrent = Number.isInteger(limits.maxConcurrentAgents) && limits.maxConcurrentAgents > 0
    ? limits.maxConcurrentAgents : 4
  const maxTotal = Number.isInteger(limits.maxTotalAgents) && limits.maxTotalAgents > 0
    ? limits.maxTotalAgents : 1000
  const maxItems = Number.isInteger(limits.maxItemsPerCall) && limits.maxItemsPerCall > 0
    ? limits.maxItemsPerCall : 4096
  const syncTimeout = Number.isInteger(limits.syncTimeoutMs) && limits.syncTimeoutMs > 0
    ? limits.syncTimeoutMs : 5000
  let currentPhase
  let agentsStarted = 0
  let activeAgents = 0
  const waiters = []

  const base = (type, data = {}) => send({ type: 'event', event: type, data: { run_id: request.run_id, meta: request.meta ?? {}, ...data } })

  async function acquireSlot() {
    if (activeAgents < maxConcurrent) {
      activeAgents += 1
      return
    }
    await new Promise((resolve, reject) => waiters.push({ resolve, reject }))
    activeAgents += 1
  }

  function releaseSlot() {
    activeAgents -= 1
    const next = waiters.shift()
    if (next) next.resolve()
  }

  async function agent(prompt, options = {}) {
    asString(prompt, 'agent')
    if (options === null || typeof options !== 'object' || Array.isArray(options)) {
      throw fatalError('agent() options must be an object', 'INVALID_ARGUMENT')
    }
    for (const key of Object.keys(options)) {
      if (!agentOptionKeys.has(key)) {
        if (agentDeferredKeys.has(key)) throw fatalError(`agent() option "${key}" is deferred and not supported`, 'UNSUPPORTED_OPTION')
        throw fatalError(`agent() option "${key}" is not recognized`, 'UNSUPPORTED_OPTION')
      }
    }
    for (const key of ['label', 'phase', 'provider', 'model']) {
      if (options[key] !== undefined && typeof options[key] !== 'string') {
        throw fatalError(`agent() option "${key}" must be a string`, 'INVALID_ARGUMENT')
      }
    }
    if (options.schema !== undefined) assertObjectSchema(options.schema)
    if (agentsStarted >= maxTotal) {
      throw fatalError(`workflow reached maxTotalAgents (${maxTotal})`, 'AGENT_CAP')
    }
    await acquireSlot()
    const seq = ++agentsStarted
    const id = ++nextCallID
    const label = typeof options.label === 'string' && options.label.length > 0
      ? options.label : prompt.slice(0, 80)
    const phase = typeof options.phase === 'string' && options.phase.length > 0 ? options.phase : currentPhase
    base('workflow/agent-start', { seq, label, ...(phase === undefined ? {} : { phase }) })
    try {
      const result = await new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject })
        send({
          type: 'agent',
          id,
          prompt,
          options: {
            ...(typeof options.label === 'string' ? { label: options.label } : {}),
            ...(typeof options.phase === 'string' ? { phase: options.phase } : {}),
            ...(typeof options.provider === 'string' ? { provider: options.provider } : {}),
            ...(typeof options.model === 'string' ? { model: options.model } : {}),
            ...(options.schema !== undefined ? { schema: options.schema } : {}),
          },
        })
      })
      const outcome = result.stop_reason === 'completed' ? 'completed' : 'failed'
      base('workflow/agent-end', { seq, label, ...(phase === undefined ? {} : { phase }), child_id: result.child_id ?? '', outcome })
      if (result.stop_reason !== 'completed') return null
      if (options.schema !== undefined) {
        return result.structured !== undefined ? jsonValue(result.structured) : null
      }
      return result.structured !== undefined ? jsonValue(result.structured) : String(result.output ?? '')
    } catch (error) {
      base('workflow/agent-end', { seq, label, ...(phase === undefined ? {} : { phase }), outcome: 'failed' })
      throw error
    } finally {
      releaseSlot()
    }
  }

  async function parallel(thunks) {
    if (!Array.isArray(thunks)) throw fatalError('parallel() requires an array of functions', 'INVALID_ARGUMENT')
    if (thunks.length > maxItems) throw fatalError(`parallel() received ${thunks.length} items; cap is ${maxItems}`, 'ITEM_CAP')
    return Promise.all(thunks.map(async (thunk, index) => {
      if (typeof thunk !== 'function') throw fatalError(`parallel() item ${index} is not a function`, 'INVALID_ARGUMENT')
      try { return await thunk() } catch (error) {
        if (error?.fatal) throw error
        return null
      }
    }))
  }

  async function pipeline(items, ...stages) {
    if (!Array.isArray(items)) throw fatalError('pipeline() requires an items array', 'INVALID_ARGUMENT')
    if (items.length > maxItems) throw fatalError(`pipeline() received ${items.length} items; cap is ${maxItems}`, 'ITEM_CAP')
    if (stages.length === 0 || stages.some((stage) => typeof stage !== 'function')) {
      throw fatalError('pipeline() requires one or more stage functions', 'INVALID_ARGUMENT')
    }
    return Promise.all(items.map(async (item, index) => {
      let value = item
      try {
        for (const stage of stages) value = await stage(value, item, index)
        return value
      } catch (error) {
        if (error?.fatal) throw error
        return null
      }
    }))
  }

  function phase(title) {
    currentPhase = asString(title, 'phase')
    base('workflow/phase', { title: currentPhase })
  }

  function log(message) {
    base('workflow/log', { message: asString(message, 'log') })
  }

  base('workflow/start')
  const sandbox = { args: request.args, agent, parallel, pipeline, phase, log }
  const context = vm.createContext(sandbox)
  const source = `(async () => {\n${request.script}\n})()`
  try {
    const value = await vm.runInContext(source, context, {
      filename: `workflow:${request.meta?.name ?? request.run_id}`,
      timeout: syncTimeout,
    })
    const result = { value: jsonValue(value), stop_reason: 'completed', agents_started: agentsStarted }
    base('workflow/end', { stop_reason: result.stop_reason, agents_started: agentsStarted })
    return result
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    const result = { value: null, stop_reason: 'error', error: message, agents_started: agentsStarted }
    base('workflow/end', { stop_reason: result.stop_reason, error: message, agents_started: agentsStarted })
    return result
  }
}

try {
  const request = await startPromise
  if (!request || request.type !== 'start' || typeof request.script !== 'string' || request.script.length === 0) {
    throw new Error('invalid workflow start request')
  }
  const result = await execute(request)
  send({ type: 'result', ...result })
} catch (error) {
  send({ type: 'result', value: null, stop_reason: 'error', error: error instanceof Error ? error.message : String(error), agents_started: 0 })
}
rl.close()
process.stdin.pause()
