import { fileURLToPath } from 'node:url'

const local = (relative: string): string => fileURLToPath(new URL(relative, import.meta.url))

export default {
  base: '/',
  esbuild: {
    jsx: 'automatic',
    jsxImportSource: 'react',
  },
  resolve: {
    alias: [
      { find: '@deepseek-ai/cordis', replacement: local('../../deepseek-harness/vendor/cordis/src/index.ts') },
      { find: '@deepseek-ai/cosmokit', replacement: local('../../deepseek-harness/vendor/cosmokit/src/index.ts') },
      { find: '@shutu-dsh/trajectory', replacement: local('../../deepseek-harness/packages/client/ui-trajectory/src/client/timeline.ts') },
      { find: '@standard-schema/spec', replacement: local('../../deepseek-harness/apps/web/node_modules/@standard-schema/spec') },
      { find: 'react', replacement: local('../../deepseek-harness/apps/web/node_modules/react') },
      { find: 'react-dom', replacement: local('../../deepseek-harness/apps/web/node_modules/react-dom') },
    ],
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: true,
  },
}
