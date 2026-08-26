import { fileURLToPath } from 'node:url'
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import type { Plugin } from 'vite'

const local = (relative: string): string => fileURLToPath(new URL(relative, import.meta.url))
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? local('../../deepseek-harness'))
const dsh = (relative: string): string => resolve(dshRoot, relative)

const NATIVE_CLIENT_PACKAGE_DIRS = [
  'connection', 'hmr', 'locale', 'runtime', 'ui-agent-preset', 'ui-attachment',
  'ui-brand-official', 'ui-commands', 'ui-conversation', 'ui-deliverables',
  'ui-directory-picker-browse', 'ui-goal',
  'ui-input-trigger', 'ui-jobs', 'ui-layout', 'ui-message-feedback',
  'ui-model-selection', 'ui-permission-presets', 'ui-plan', 'ui-reference',
  'ui-renderer', 'ui-settings', 'ui-settings-general', 'ui-settings-models',
  'ui-settings-plugin-inventory', 'ui-settings-plugins', 'ui-sidebar', 'ui-skill',
  'ui-subagent', 'ui-theme', 'ui-tool', 'ui-trajectory', 'ui-user-questions',
  'ui-workflow-run', 'ui-workspace',
] as const

const NATIVE_CLIENT_PACKAGE_IDS = NATIVE_CLIENT_PACKAGE_DIRS.map(dir =>
  `@deepseek-ai/dsh-client-${dir}`)
const NATIVE_EXTRA_PLUGIN_SPECS = [
  ['typert/registry', '@deepseek-ai/dsh-typert-registry'],
  ['extensions/cordis-client-runner', '@deepseek-ai/dsh-cordis-client-runner'],
  ['extensions/ui-cordis', '@deepseek-ai/dsh-client-ui-cordis'],
  ['session-query/session-log-export', '@deepseek-ai/dsh-session-log-export'],
  ['__local-native-remote', '@deepseek-ai/dsh-api-remotes'],
] as const

const NATIVE_PLUGIN_SPECS = [
  ...NATIVE_CLIENT_PACKAGE_DIRS.map((dir, index) => [`client/${dir}`, NATIVE_CLIENT_PACKAGE_IDS[index]] as const),
  ...NATIVE_EXTRA_PLUGIN_SPECS,
]

/** Generate the statically linked DSH browser roster only for native builds. */
function nativeDshRoster(): Plugin {
  const virtualId = 'virtual:shutu-dsh-native-plugins'
  const resolvedId = `\0${virtualId}`
  return {
    name: 'shutu-native-dsh-roster',
    resolveId(id) {
      return id === virtualId ? resolvedId : undefined
    },
    load(id) {
      if (id !== resolvedId) return undefined
      if (process.env.SHUTU_DSH_NATIVE !== '1') return 'export const DSH_NATIVE_PLUGINS = []'
      const imports = NATIVE_PLUGIN_SPECS.map(([relative, _id], index) => {
        const source = (relative === '__local-native-remote'
          ? local('src/dsh-native-remote.ts')
          : dsh(`packages/${relative}/src/client/index.ts`)).replaceAll('\\', '/')
        return `import * as plugin${index} from ${JSON.stringify(source)}`
      }).join('\n')
      const registrations = NATIVE_PLUGIN_SPECS.map(([_relative, id], index) =>
        `{ id: ${JSON.stringify(id)}, module: plugin${index} }`).join(',\n  ')
      return `${imports}\nexport const DSH_NATIVE_PLUGINS = [\n  ${registrations}\n]`
    },
  }
}

interface SourcePackage {
  readonly root: string
  readonly sourceRoot: string
}

function sourceFile(base: string): string | undefined {
  for (const candidate of [base, `${base}.ts`, `${base}.tsx`, `${base}.js`, `${base}.jsx`, join(base, 'index.ts'), join(base, 'index.tsx')]) {
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate
  }
  return undefined
}

function discoverSourcePackages(root: string): ReadonlyMap<string, SourcePackage> {
  const packages = new Map<string, SourcePackage>()
  const walk = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      if (!entry.isDirectory() || entry.name === 'node_modules' || entry.name === '.git') continue
      const packageRoot = join(directory, entry.name)
      const manifestPath = join(packageRoot, 'package.json')
      if (existsSync(manifestPath)) {
        try {
          const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as { name?: unknown }
          if (typeof manifest.name === 'string') {
            const sourceRoot = join(packageRoot, 'src')
            if (existsSync(sourceRoot)) packages.set(manifest.name, { root: packageRoot, sourceRoot })
          }
        } catch {
          // An unrelated package manifest must not prevent the web build.
        }
      }
      walk(packageRoot)
    }
  }
  walk(root)
  return packages
}

/** Resolve DSH workspace package exports to their read-only TypeScript source. */
function nativeDshSourceResolver(): Plugin {
  const packages = new Map<string, SourcePackage>()
  for (const root of [dsh('packages'), dsh('vendor')]) {
    for (const [name, info] of discoverSourcePackages(root)) packages.set(name, info)
  }
  return {
    name: 'shutu-native-dsh-source-resolver',
    enforce: 'pre',
    resolveId(source) {
      if (!source.startsWith('@deepseek-ai/')) return undefined
      const parts = source.split('/')
      const packageName = parts.slice(0, 2).join('/')
      const packageInfo = packages.get(packageName)
      if (packageInfo === undefined) return undefined
      const subpath = parts.slice(2).join('/')
      const relatives = subpath === ''
        ? ['index']
        : subpath === 'client'
          ? ['client', 'fetch/client']
          : [subpath]
      for (const relative of relatives) {
        const resolved = sourceFile(join(packageInfo.sourceRoot, relative))
        if (resolved !== undefined) return resolved
      }
      return undefined
    },
  }
}

export default {
  plugins: [nativeDshSourceResolver(), nativeDshRoster()],
  base: '/',
  esbuild: {
    jsx: 'automatic',
    jsxImportSource: 'react',
  },
  resolve: {
    alias: [
      { find: /^node:module$/, replacement: dsh('apps/web/src/node-module-stub.ts') },
      { find: '@deepseek-ai/cordis', replacement: dsh('vendor/cordis/src/index.ts') },
      { find: '@deepseek-ai/cosmokit', replacement: dsh('vendor/cosmokit/src/index.ts') },
      { find: '@deepseek-ai/dsh-client-web', replacement: dsh('packages/client/web/src/index.ts') },
      { find: '@deepseek-ai/dsh-client-modules/client', replacement: dsh('packages/client/modules/src/client/index.ts') },
      { find: '@deepseek-ai/dsh-client-ui-renderer/client', replacement: dsh('packages/client/ui-renderer/src/client/index.ts') },
      { find: '@deepseek-ai/dsh-client-ui-slots', replacement: dsh('packages/client/ui-slots/src/index.ts') },
      { find: '@deepseek-ai/dsh-client-ui-primitives', replacement: dsh('packages/client/ui-primitives/src/index.ts') },
      { find: '@deepseek-ai/dsh-client-runtime/client', replacement: dsh('packages/client/runtime/src/client/index.ts') },
      { find: '@deepseek-ai/cordis-plugin-loader', replacement: dsh('vendor/loader/src/index.ts') },
      { find: '@shutu-dsh/trajectory', replacement: dsh('packages/client/ui-trajectory/src/client/timeline.ts') },
      { find: '@standard-schema/spec', replacement: dsh('apps/web/node_modules/@standard-schema/spec') },
      { find: 'react', replacement: dsh('apps/web/node_modules/react') },
      { find: 'react-dom', replacement: dsh('apps/web/node_modules/react-dom') },
    ],
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: true,
  },
  define: {
    __SHUTU_DSH_NATIVE__: JSON.stringify(process.env.SHUTU_DSH_NATIVE === '1'),
    // DSH client packages intentionally read a closed build-time environment;
    // never expose a runtime Node process object in the browser bundle.
    'process.env': JSON.stringify({
      NODE_ENV: 'production',
      DSH_CLIENT_BUILD_PROFILE: 'official',
      DSH_CLIENT_TITLE: 'DeepSeek Harness',
      DSH_CLIENT_COMMIT_HASH: 'shutu-native',
    }),
    'process.versions.node': '"0.0.0"',
    'process.execArgv': '[]',
  },
}
