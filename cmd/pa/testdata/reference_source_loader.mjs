// Node ESM loader used only by the opt-in pinned-reference test. It lets the
// read-only reference checkout run from TypeScript without generating lib/
// artifacts or launching esbuild's helper process.
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'
import path from 'node:path'

const referenceRoot = path.resolve(process.env.DSH_REFERENCE_ROOT)
let workspacePackages
let typescript

function indexWorkspacePackages() {
  if (workspacePackages) return workspacePackages
  workspacePackages = new Map()
  const files = []
  const walk = (directory, depth) => {
    if (depth > 8) return
    let entries
    try {
      entries = readdirSync(directory, { withFileTypes: true })
    } catch {
      return
    }
    for (const entry of entries) {
      if (entry.name === 'node_modules' || entry.name === '.git') continue
      const child = path.join(directory, entry.name)
      if (entry.isFile() && entry.name === 'package.json') files.push(child)
      else if (entry.isDirectory()) walk(child, depth + 1)
    }
  }
  for (const base of ['packages', 'vendor']) walk(path.join(referenceRoot, base), 0)
  for (const file of files) {
    try {
      const manifest = JSON.parse(readFileSync(file, 'utf8'))
      if (typeof manifest.name === 'string' && !workspacePackages.has(manifest.name)) {
        workspacePackages.set(manifest.name, path.dirname(file))
      }
    } catch {
      // Reference fixtures and generated files may not be valid JSON packages.
    }
  }
  return workspacePackages
}

function mapWorkspaceSpecifier(specifier) {
  const segments = specifier.split('/')
  const [packageName, ...rest] = segments[0].startsWith('@')
    ? [segments.slice(0, 2).join('/'), ...segments.slice(2)]
    : [segments[0], ...segments.slice(1)]
  const packageRoot = indexWorkspacePackages().get(packageName)
  if (!packageRoot) return undefined
  const relativeParts = rest.length === 0 ? ['index.ts'] : rest.map(part => part.replace(/\.js$/, '.ts'))
  if (!relativeParts.at(-1).includes('.')) relativeParts[relativeParts.length - 1] += '.ts'
  const candidate = path.join(packageRoot, 'src', ...relativeParts)
  return existsSync(candidate) ? candidate : undefined
}

function mapReferenceSource(urlString) {
  if (!urlString.startsWith('file:')) return { url: urlString }
  let source
  try {
    source = fileURLToPath(urlString)
  } catch {
    return { url: urlString }
  }
  const normalized = path.resolve(source)
  const relative = path.relative(referenceRoot, normalized)
  if (!relative || relative.startsWith('..') || path.isAbsolute(relative)) return { url: urlString }
  const parts = relative.split(/[\\/]/)
  let candidate = normalized
  const libIndex = parts.indexOf('lib')
  if (libIndex > 0 && parts.at(-1).endsWith('.js')) {
    const packageRoot = path.join(referenceRoot, ...parts.slice(0, libIndex))
    const tail = parts.slice(libIndex + 1).map(part => part.replace(/\.js$/, '.ts'))
    candidate = path.join(packageRoot, 'src', ...tail)
  } else if (relative.includes(`${path.sep}src${path.sep}`) && normalized.endsWith('.js')) {
    candidate = normalized.replace(/\.js$/, '.ts')
  }
  if (candidate !== normalized && existsSync(candidate)) {
    return { url: pathToFileURL(candidate).href, shortCircuit: true }
  }
  return { url: urlString }
}

async function typescriptModule() {
  typescript ??= await import(pathToFileURL(path.join(referenceRoot, 'node_modules', 'typescript', 'lib', 'typescript.js')).href)
  return typescript
}

export async function resolve(specifier, context, nextResolve) {
  if (specifier === '@agentclientprotocol/sdk') {
    return {
      url: pathToFileURL(path.join(referenceRoot, 'node_modules', '@agentclientprotocol', 'sdk', 'dist', 'acp.js')).href,
      shortCircuit: true,
    }
  }
  if (specifier === 'zod' || specifier.startsWith('zod/')) {
    const pinnedZod = path.join(referenceRoot, 'node_modules', '.pnpm', 'zod@4.4.3', 'node_modules', 'zod', 'index.js')
    if (existsSync(pinnedZod)) return { url: pathToFileURL(pinnedZod).href, shortCircuit: true }
  }
  if (specifier.startsWith('@deepseek-ai/')) {
    const candidate = mapWorkspaceSpecifier(specifier)
    if (candidate) return { url: pathToFileURL(candidate).href, shortCircuit: true }
  }
  return mapReferenceSource((await nextResolve(specifier, context)).url)
}

export async function load(url, context, nextLoad) {
  const source = url.startsWith('file:') ? path.resolve(fileURLToPath(url)) : ''
  const inReference = source && !path.relative(referenceRoot, source).startsWith('..')
  if (inReference && source.endsWith('.ts')) {
    const ts = await typescriptModule()
    const text = await import('node:fs/promises').then(fs => fs.readFile(source, 'utf8'))
    const { outputText } = ts.transpileModule(text, {
      compilerOptions: {
        experimentalDecorators: false,
        module: 99,
        target: 7,
        useDefineForClassFields: true,
      },
      fileName: source,
    })
    return { format: 'module', source: outputText, shortCircuit: true }
  }
  return nextLoad(url, context)
}
