import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = resolve(fileURLToPath(new URL('..', import.meta.url)))
const distRoot = resolve(webRoot, process.argv[2] ?? 'dist')
const indexPath = resolve(distRoot, 'index.html')

if (!existsSync(indexPath)) throw new Error(`frontend dist is missing ${indexPath}`)

const index = readFileSync(indexPath, 'utf8')
if (index.includes('/src/') || index.includes('deepseek-harness')) {
  throw new Error('frontend dist still contains a source-only dependency reference')
}

const references = [...index.matchAll(/(?:src|href)="([^"]+)"/g)].map(match => match[1])
const assets = references.filter(reference => reference.startsWith('/assets/'))
if (assets.length === 0) throw new Error('frontend dist index has no built assets')

for (const asset of assets) {
  const assetPath = resolve(distRoot, `.${asset}`)
  const boundary = `${distRoot}${sep}`
  if (!assetPath.startsWith(boundary) || !existsSync(assetPath)) {
    throw new Error(`frontend dist asset is missing: ${asset}`)
  }
}

const builtFiles = readdirSync(resolve(distRoot, 'assets'))
if (!builtFiles.some(file => file.endsWith('.js'))) throw new Error('frontend dist has no JavaScript bundle')
if (!builtFiles.some(file => file.endsWith('.css'))) throw new Error('frontend dist has no CSS bundle')

const manifestPath = resolve(distRoot, 'shutu-native-manifest.json')
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
if (manifest.kind !== 'shutu-native-ui' || manifest.schemaVersion !== 1) {
  throw new Error('native frontend manifest has an unsupported schema')
}
if (!Array.isArray(manifest.plugins) || manifest.plugins.length < 39) {
  throw new Error('native frontend manifest has missing linked plugins')
}
const linked = new Set(manifest.plugins.map(plugin => plugin?.id))
if (linked.size !== manifest.plugins.length) throw new Error('native frontend manifest has duplicate plugin IDs')
for (const plugin of manifest.plugins) {
  if (typeof plugin?.id !== 'string' || typeof plugin?.source !== 'string') {
    throw new Error('native frontend manifest has an invalid plugin entry')
  }
  if (!plugin.id.startsWith('@shutu-ai/')) throw new Error(`native plugin ID is not namespaced: ${plugin.id}`)
}

console.log(JSON.stringify({
  dist: relative(webRoot, distRoot),
  assets: builtFiles.length,
  index: 'ok',
  nativeManifest: 'ok',
}))
