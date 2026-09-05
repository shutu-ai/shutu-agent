/**
 * Package-owned invariant companion for `@shutu-ai/file-reference`.
 * @module @shutu-ai/file-reference/invariant
 */

/* jscpd:ignore-start */
import type { Context } from '@shutu-ai/cordis'
import type { InvariantInstaller } from '@shutu-ai/invariants'

const PACKAGE_NAME = '@shutu-ai/file-reference'

/** Cordis companion plugin name. */
export const name = 'file-reference-invariant'
/** Service required before the companion can reserve package ownership. */
export const inject = ['invariants']

/**
 * No runtime invariant: the interface retains no candidate or lifecycle
 * state; concrete providers own their cache and invalidation relationships.
 */
const install: InvariantInstaller = () => {}

/**
 * Register this package's invariant companion.
 * @param ctx - Cordis context carrying the invariant service.
 * @returns the installed registration's disposer after setup succeeds.
 */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
/* jscpd:ignore-end */
