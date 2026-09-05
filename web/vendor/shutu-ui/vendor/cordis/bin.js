#!/usr/bin/env node

import { Context } from '@shutu-ai/cordis'
import { pathToFileURL } from 'node:url'
import Loader from '@shutu-ai/cordis-plugin-loader'

const ctx = new Context()
ctx.baseUrl = pathToFileURL(process.cwd()).href + '/'

await ctx.plugin(Loader)
await ctx.loader.create({
  name: '@shutu-ai/cordis-plugin-include',
  config: {
    path: './cordis.yml',
  },
})
