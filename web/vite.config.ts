import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'

const local = (relative: string): string => fileURLToPath(new URL(relative, import.meta.url))
const dshRoot = resolve(process.env.SHUTU_DSH_ROOT ?? local('../../deepseek-harness'))
const dsh = (relative: string): string => resolve(dshRoot, relative)

export default {
  base: '/',
  esbuild: {
    jsx: 'automatic',
    jsxImportSource: 'react',
  },
  resolve: {
    alias: [
      { find: '@deepseek-ai/cordis', replacement: dsh('vendor/cordis/src/index.ts') },
      { find: '@deepseek-ai/cosmokit', replacement: dsh('vendor/cosmokit/src/index.ts') },
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
}
