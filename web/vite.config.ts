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
  },
}
