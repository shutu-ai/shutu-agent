import { staticLinked } from '../tsdown.client.ts'

export default staticLinked(
  '@shutu-ai/client-web',
  ['lib/types/index.js', 'lib/types/invariant.js'],
)
