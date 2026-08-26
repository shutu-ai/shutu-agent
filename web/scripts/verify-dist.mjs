import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { resolve, relative, sep } from 'node:path'
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

const nativeManifestPath = resolve(distRoot, 'dsh-native-manifest.json')
if (existsSync(nativeManifestPath)) {
  const nativeManifest = JSON.parse(readFileSync(nativeManifestPath, 'utf8'))
  if (nativeManifest.kind !== 'shutu-dsh-native' || nativeManifest.schemaVersion !== 1) {
    throw new Error('native frontend manifest has an unsupported schema')
  }
  if (!Array.isArray(nativeManifest.plugins) || nativeManifest.plugins.length === 0) {
    throw new Error('native frontend manifest has no linked plugins')
  }
}

console.log(JSON.stringify({
  dist: relative(webRoot, distRoot),
  assets: builtFiles.length,
  index: 'ok',
  nativeManifest: existsSync(nativeManifestPath) ? 'ok' : 'not-present',
}))
